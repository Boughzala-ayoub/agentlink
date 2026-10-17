// Package store is agentlink's mailbox: one SQLite file in WAL mode that every
// agentlink process opens directly. There is no daemon; every write runs in a
// BEGIN IMMEDIATE transaction, so concurrent processes serialize on SQLite's lock.
package store

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

const (
	MaxBody   = 32 << 10
	TTL       = 7 * 24 * time.Hour
	DedupSpan = 2 * time.Minute
)

// Now is the clock in unix milliseconds. Tests move it.
var Now = func() int64 { return time.Now().UnixMilli() }

type Endpoint struct {
	Handle   string
	App      string
	Project  string
	Session  string // the app's own session or thread id
	Tier     string // push | turn | pull
	Wake     string // adapter data, e.g. a Claude Code socket and token as JSON
	LastSeen int64
	Unread   int
}

type Message struct {
	Seq       int64
	ID        string
	Thread    string
	ReplyTo   string
	From      string
	FromApp   string
	To        []string
	Body      string
	CreatedAt int64
	ExpiresAt int64
	Hops      int
}

type Store struct{ db *sql.DB }

// Home is the agentlink folder: $AGENTLINK_HOME, else ~/.agentlink.
func Home() string {
	if h := os.Getenv("AGENTLINK_HOME"); h != "" {
		return h
	}
	h, _ := os.UserHomeDir()
	return filepath.Join(h, ".agentlink")
}

const schema = `
create table if not exists endpoints (
  handle text primary key, app text not null, project text not null default '',
  session text not null default '', tier text not null, wake text not null default '',
  created_at int not null, last_seen int not null, ended_at int);
create table if not exists messages (
  seq integer primary key, id text unique not null, thread text not null,
  reply_to text not null default '', sender text not null, sender_app text not null,
  body text not null, created_at int not null, expires_at int not null, hops int not null);
create table if not exists deliveries (
  seq int not null references messages(seq), recipient text not null, state text not null,
  woken_at int, delivered_at int, attempts int not null default 0,
  last_error text not null default '', notified int not null default 0,
  primary key (seq, recipient));
create index if not exists deliveries_open on deliveries(recipient, state);
create index if not exists messages_sender on messages(sender, created_at);`

// columns added after the first schema; adding one twice fails harmlessly
var later = []string{`alter table endpoints add column waiting_until int not null default 0`}

// Open opens (and creates) the database at path, or at Home()/agentlink.db if path is empty.
func Open(path string) (*Store, error) {
	if path == "" {
		path = filepath.Join(Home(), "agentlink.db")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	dsn := "file:" + (&url.URL{Path: filepath.ToSlash(path)}).EscapedPath() +
		"?_pragma=busy_timeout(10000)&_pragma=journal_mode(wal)&_pragma=synchronous(full)&_pragma=foreign_keys(on)&_txlock=immediate"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// Switching a new file to WAL fails with SQLITE_BUSY, without waiting, while
	// another process creates the same file, so the first open retries.
	for start := time.Now(); ; {
		_, err = db.Exec(schema)
		if err == nil {
			break
		}
		if !strings.Contains(err.Error(), "SQLITE_BUSY") || time.Since(start) > 10*time.Second {
			db.Close()
			return nil, fmt.Errorf("open %s: %w", path, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	for _, q := range later {
		if _, err := db.Exec(q); err != nil && !strings.Contains(err.Error(), "duplicate column") {
			db.Close()
			return nil, fmt.Errorf("open %s: %w", path, err)
		}
	}
	os.Chmod(path, 0o600)
	return &Store{db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

// Writable takes and releases the write lock, to prove the file can be written.
func (s *Store) Writable() error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	return tx.Rollback()
}

func randHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func uuid() string {
	b := make([]byte, 16)
	rand.Read(b)
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	h := hex.EncodeToString(b)
	return h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}

var nonSlug = regexp.MustCompile(`[^a-z0-9]+`)

// Handle names a conversation: app, project folder and four characters of the
// app's own session id, e.g. codex-wisp-3f2a. Without a session it is the app.
func Handle(app, project, session string) string {
	if session == "" {
		return app
	}
	base := strings.TrimRight(project, `/\`)
	base = base[strings.LastIndexAny(base, `/\`)+1:] // either separator, so CI on any OS agrees
	p := strings.Trim(nonSlug.ReplaceAllString(strings.ToLower(base), "-"), "-")
	sid := strings.ToLower(nonSlug.ReplaceAllString(strings.ToLower(session), ""))
	if len(sid) > 4 {
		sid = sid[len(sid)-4:]
	}
	if p == "" {
		return app + "-" + sid
	}
	return app + "-" + p + "-" + sid
}

// Register creates or refreshes an endpoint. It refuses to move a handle to another app.
func (s *Store) Register(e Endpoint) error {
	now := Now()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var app string
	switch err := tx.QueryRow(`select app from endpoints where handle=?`, e.Handle).Scan(&app); {
	case err == nil && app != e.App:
		return fmt.Errorf("handle %s belongs to %s, not %s", e.Handle, app, e.App)
	case err != nil && !errors.Is(err, sql.ErrNoRows):
		return err
	}
	seen := e.LastSeen // set when the endpoint was found in the app's own records rather than seen live
	if seen == 0 {
		seen = now
	}
	_, err = tx.Exec(`insert into endpoints(handle,app,project,session,tier,wake,created_at,last_seen) values(?,?,?,?,?,?,?,?)
		on conflict(handle) do update set project=excluded.project, session=excluded.session, tier=excluded.tier,
		wake=case when excluded.wake='' then endpoints.wake else excluded.wake end,
		last_seen=max(endpoints.last_seen, excluded.last_seen), ended_at=null`,
		e.Handle, e.App, e.Project, e.Session, e.Tier, e.Wake, now, seen)
	if err != nil {
		return err
	}
	return tx.Commit()
}

// Get returns one endpoint, or sql.ErrNoRows.
func (s *Store) Get(handle string) (Endpoint, error) {
	var e Endpoint
	err := s.db.QueryRow(`select handle,app,project,session,tier,wake,last_seen from endpoints where handle=?`, handle).
		Scan(&e.Handle, &e.App, &e.Project, &e.Session, &e.Tier, &e.Wake, &e.LastSeen)
	return e, err
}

// Peers lists live endpoints, most recently seen first, with each one's unread count.
func (s *Store) Peers() ([]Endpoint, error) {
	rows, err := s.db.Query(`select e.handle,e.app,e.project,e.session,e.tier,e.wake,e.last_seen,
		(select count(*) from deliveries d join messages m on m.seq=d.seq
		 where d.recipient=e.handle and d.state in ('queued','woken') and m.expires_at>?)
		from endpoints e where e.ended_at is null order by e.last_seen desc, e.handle`, Now())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Endpoint
	for rows.Next() {
		var e Endpoint
		if err := rows.Scan(&e.Handle, &e.App, &e.Project, &e.Session, &e.Tier, &e.Wake, &e.LastSeen, &e.Unread); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// NotFound is returned by Resolve for a name that matches no endpoint.
type NotFound struct {
	Name  string
	Known []Endpoint
}

func (e *NotFound) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "No agent called %s.", e.Name)
	if len(e.Known) == 0 {
		b.WriteString(" No other agents have connected to agentlink yet.")
		return b.String()
	}
	b.WriteString(" Known:")
	for i, k := range e.Known {
		if i > 0 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, " %s (%s, %s, seen %s ago)", k.Handle, k.App, k.Tier, Ago(k.LastSeen))
	}
	return b.String()
}

// Ago renders the time since a unix-ms timestamp the way a person would say it.
func Ago(ms int64) string {
	d := time.Duration(Now()-ms) * time.Millisecond
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%d s", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%d min", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%d h", int(d.Hours()))
	}
	return fmt.Sprintf("%d days", int(d.Hours()/24))
}

// Resolve turns `to` (a handle, an app name such as "codex" or "claude", or a
// comma-separated list) into endpoints. An app name picks that app's most
// recently seen endpoint, preferring one in the sender's project.
func (s *Store) Resolve(sender Endpoint, to string) ([]Endpoint, error) {
	peers, err := s.Peers()
	if err != nil {
		return nil, err
	}
	var out []Endpoint
	seen := map[string]bool{}
	for _, name := range strings.Split(to, ",") {
		name = strings.ToLower(strings.TrimSpace(name))
		if name == "" {
			continue
		}
		var pick *Endpoint
		for i, p := range peers {
			if p.Handle == name {
				pick = &peers[i]
				break
			}
		}
		if pick == nil {
			for i, p := range peers { // peers are sorted by last_seen desc
				if p.Handle == sender.Handle || (p.App != name && !strings.HasPrefix(p.App, name+"-")) {
					continue
				}
				if pick == nil || (p.Project == sender.Project && sender.Project != "" && pick.Project != sender.Project) {
					pick = &peers[i]
				}
			}
		}
		if pick == nil {
			var known []Endpoint
			for _, p := range peers {
				if p.Handle != sender.Handle {
					known = append(known, p)
				}
			}
			return nil, &NotFound{name, known}
		}
		if !seen[pick.Handle] {
			seen[pick.Handle] = true
			out = append(out, *pick)
		}
	}
	if len(out) == 0 {
		return nil, errors.New("no recipient given")
	}
	return out, nil
}

// Sent is the result of Send. Duplicate is set when the same sender sent the
// same body to the same recipients within DedupSpan; nothing new was stored.
type Sent struct {
	ID        string
	Seq       int64
	Thread    string
	Duplicate bool
}

// Send stores one message for each recipient and returns once it is on disk.
func (s *Store) Send(from Endpoint, to []string, body, thread, replyTo string) (Sent, error) {
	if body == "" {
		return Sent{}, errors.New("empty message")
	}
	if len(body) > MaxBody {
		return Sent{}, fmt.Errorf("message is %d bytes, the limit is %d", len(body), MaxBody)
	}
	if len(to) == 0 {
		return Sent{}, errors.New("no recipient given")
	}
	rcpt := append([]string(nil), to...)
	sort.Strings(rcpt)
	now := Now()
	tx, err := s.db.Begin()
	if err != nil {
		return Sent{}, err
	}
	defer tx.Rollback()

	// Retried tool calls: same sender, body and recipients within DedupSpan.
	rows, err := tx.Query(`select m.id, m.seq, m.thread, group_concat(d.recipient, char(10)) from messages m
		join deliveries d on d.seq=m.seq where m.sender=? and m.body=? and m.created_at>? group by m.seq`,
		from.Handle, body, now-DedupSpan.Milliseconds())
	if err != nil {
		return Sent{}, err
	}
	for rows.Next() {
		var d Sent
		var rs string
		if err := rows.Scan(&d.ID, &d.Seq, &d.Thread, &rs); err != nil {
			rows.Close()
			return Sent{}, err
		}
		got := strings.Split(rs, "\n")
		sort.Strings(got)
		if strings.Join(got, "\n") == strings.Join(rcpt, "\n") {
			rows.Close()
			d.Duplicate = true
			return d, nil
		}
	}
	rows.Close()

	if thread == "" && replyTo != "" {
		tx.QueryRow(`select thread from messages where id=?`, replyTo).Scan(&thread)
	}
	if thread == "" {
		thread = "t-" + randHex(3)
	}
	var hops int
	tx.QueryRow(`select coalesce(max(hops),0) from messages where thread=?`, thread).Scan(&hops)
	id := uuid()
	res, err := tx.Exec(`insert into messages(id,thread,reply_to,sender,sender_app,body,created_at,expires_at,hops) values(?,?,?,?,?,?,?,?,?)`,
		id, thread, replyTo, from.Handle, from.App, body, now, now+TTL.Milliseconds(), hops+1)
	if err != nil {
		return Sent{}, err
	}
	seq, _ := res.LastInsertId()
	for _, r := range rcpt {
		if _, err := tx.Exec(`insert into deliveries(seq,recipient,state) values(?,?,'queued')`, seq, r); err != nil {
			return Sent{}, err
		}
	}
	tx.Exec(`update endpoints set last_seen=? where handle=?`, now, from.Handle)
	if err := tx.Commit(); err != nil {
		return Sent{}, err
	}
	return Sent{ID: id, Seq: seq, Thread: thread}, nil
}

const msgCols = `m.seq,m.id,m.thread,m.reply_to,m.sender,m.sender_app,m.body,m.created_at,m.expires_at,m.hops,
	(select group_concat(recipient, char(10)) from deliveries where seq=m.seq)`

func scanMessages(rows *sql.Rows) ([]Message, error) {
	defer rows.Close()
	var out []Message
	for rows.Next() {
		var m Message
		var to sql.NullString
		if err := rows.Scan(&m.Seq, &m.ID, &m.Thread, &m.ReplyTo, &m.From, &m.FromApp, &m.Body, &m.CreatedAt, &m.ExpiresAt, &m.Hops, &to); err != nil {
			return nil, err
		}
		if to.String != "" {
			m.To = strings.Split(to.String, "\n")
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// Inbox claims every unread, unexpired message for me (only thread's, if given)
// and marks it delivered in one transaction, so two concurrent callers never
// receive the same message. Messages come back in send order.
func (s *Store) Inbox(me, thread string) ([]Message, error) {
	now := Now()
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	rows, err := tx.Query(`update deliveries set state='delivered', delivered_at=?
		where recipient=? and state in ('queued','woken')
		and seq in (select seq from messages where expires_at>? and (?='' or thread=?)) returning seq`,
		now, me, now, thread, thread)
	if err != nil {
		return nil, err
	}
	var seqs []any
	for rows.Next() {
		var q int64
		rows.Scan(&q)
		seqs = append(seqs, q)
	}
	rows.Close()
	if len(seqs) == 0 {
		return nil, tx.Commit()
	}
	q := `select ` + msgCols + ` from messages m where m.seq in (?` + strings.Repeat(",?", len(seqs)-1) + `) order by m.seq`
	r2, err := tx.Query(q, seqs...)
	if err != nil {
		return nil, err
	}
	msgs, err := scanMessages(r2)
	if err != nil {
		return nil, err
	}
	tx.Exec(`update endpoints set last_seen=? where handle=?`, now, me)
	return msgs, tx.Commit()
}

// WakeFresh is how long a wake covers later messages: one wake line says
// "N new messages", so a recipient that was woken and hasn't read yet isn't
// woken again. After this long agentlink assumes the wake got lost.
const WakeFresh = 10 * time.Minute

// NeedsWake reports whether handle has queued messages and no recent wake still unread.
func (s *Store) NeedsWake(handle string) (bool, error) {
	var queued, fresh int
	err := s.db.QueryRow(`select
		coalesce(sum(d.state='queued'),0), coalesce(sum(d.state='woken' and d.woken_at>?),0)
		from deliveries d join messages m on m.seq=d.seq where d.recipient=? and m.expires_at>?`,
		Now()-WakeFresh.Milliseconds(), handle, Now()).Scan(&queued, &fresh)
	return queued > 0 && fresh == 0, err
}

// MarkWoken records that handle was just woken for everything queued for it.
func (s *Store) MarkWoken(handle string) error {
	_, err := s.db.Exec(`update deliveries set state='woken', woken_at=?, attempts=attempts+1, last_error=''
		where recipient=? and state in ('queued','woken')`, Now(), handle)
	return err
}

// WakeFailed records why waking handle failed; its messages stay queued.
func (s *Store) WakeFailed(handle, why string) error {
	_, err := s.db.Exec(`update deliveries set attempts=attempts+1, last_error=? where recipient=? and state='queued'`, why, handle)
	return err
}

// SetWaiting records that handle sits in an agentlink_inbox wait until the
// given unix-ms time (0: not waiting). A sender then needn't wake it.
func (s *Store) SetWaiting(handle string, until int64) error {
	_, err := s.db.Exec(`update endpoints set waiting_until=? where handle=?`, until, handle)
	return err
}

// Waiting reports whether handle is inside an agentlink_inbox wait right now.
func (s *Store) Waiting(handle string) bool {
	var until int64
	s.db.QueryRow(`select waiting_until from endpoints where handle=?`, handle).Scan(&until)
	return until > Now()
}

// Delivered reports whether message seq has reached handle's inbox call.
func (s *Store) Delivered(seq int64, handle string) bool {
	var state string
	s.db.QueryRow(`select state from deliveries where seq=? and recipient=?`, seq, handle).Scan(&state)
	return state == "delivered"
}

// Unclaim puts messages a cancelled inbox call had claimed back in the queue.
func (s *Store) Unclaim(me string, msgs []Message) error {
	for _, m := range msgs {
		if _, err := s.db.Exec(`update deliveries set state='queued', delivered_at=null
			where recipient=? and seq=? and state='delivered'`, me, m.Seq); err != nil {
			return err
		}
	}
	return nil
}

// History returns the last limit messages to or from me, oldest first, without claiming anything.
func (s *Store) History(me, thread string, limit int) ([]Message, error) {
	rows, err := s.db.Query(`select * from (select `+msgCols+` from messages m
		where (m.sender=? or exists(select 1 from deliveries d where d.seq=m.seq and d.recipient=?))
		and (?='' or m.thread=?) order by m.seq desc limit ?) order by seq`, me, me, thread, thread, limit)
	if err != nil {
		return nil, err
	}
	return scanMessages(rows)
}

// After returns messages with seq > after, for `agentlink watch`.
func (s *Store) After(after int64) ([]Message, error) {
	rows, err := s.db.Query(`select `+msgCols+` from messages m where m.seq>? order by m.seq limit 500`, after)
	if err != nil {
		return nil, err
	}
	return scanMessages(rows)
}

// Unread counts my waiting messages and names their senders.
func (s *Store) Unread(me string) (int, []string, error) {
	rows, err := s.db.Query(`select m.sender, count(*) from deliveries d join messages m on m.seq=d.seq
		where d.recipient=? and d.state in ('queued','woken') and m.expires_at>? group by m.sender order by min(m.seq)`, me, Now())
	if err != nil {
		return 0, nil, err
	}
	defer rows.Close()
	total, from := 0, []string(nil)
	for rows.Next() {
		var f string
		var n int
		rows.Scan(&f, &n)
		total += n
		from = append(from, f)
	}
	return total, from, rows.Err()
}

// Expired is one delivery that ran out of time before its recipient read it.
type Expired struct {
	ID, Thread, Recipient string
}

// TakeExpired marks deliveries past their expiry and returns, once, those that
// sender hasn't been told about yet.
func (s *Store) TakeExpired(sender string) ([]Expired, error) {
	now := Now()
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`update deliveries set state='expired' where state in ('queued','woken','held')
		and seq in (select seq from messages where expires_at<=?)`, now); err != nil {
		return nil, err
	}
	rows, err := tx.Query(`update deliveries set notified=1 where state='expired' and notified=0
		and seq in (select seq from messages where sender=?) returning seq, recipient`, sender)
	if err != nil {
		return nil, err
	}
	type sr struct {
		seq int64
		r   string
	}
	var got []sr
	for rows.Next() {
		var x sr
		rows.Scan(&x.seq, &x.r)
		got = append(got, x)
	}
	rows.Close()
	var out []Expired
	for _, x := range got {
		e := Expired{Recipient: x.r}
		tx.QueryRow(`select id, thread from messages where seq=?`, x.seq).Scan(&e.ID, &e.Thread)
		out = append(out, e)
	}
	return out, tx.Commit()
}
