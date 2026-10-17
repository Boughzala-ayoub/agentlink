package store

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// The multi-process test re-runs this test binary as child processes.
func TestMain(m *testing.M) {
	switch os.Getenv("AGENTLINK_TEST_CHILD") {
	case "writer":
		os.Exit(child(func(s *Store, id string) error {
			from := Endpoint{Handle: "w" + id, App: "test"}
			for i := 0; i < 50; i++ {
				if _, err := s.Send(from, []string{"sink"}, fmt.Sprintf("w%s-%d", id, i), "t-load", ""); err != nil {
					return err
				}
			}
			return nil
		}))
	case "claimer":
		os.Exit(child(func(s *Store, id string) error {
			for empty := 0; empty < 20; {
				msgs, err := s.Inbox("sink", "")
				if err != nil {
					return err
				}
				if len(msgs) == 0 {
					empty++
					time.Sleep(5 * time.Millisecond)
				}
				for _, m := range msgs {
					fmt.Println(m.Seq)
				}
			}
			return nil
		}))
	}
	os.Exit(m.Run())
}

func child(f func(*Store, string) error) int {
	s, err := Open(os.Getenv("AGENTLINK_TEST_DB"))
	if err == nil {
		err = f(s, os.Getenv("AGENTLINK_TEST_ID"))
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}

func open(t *testing.T) (*Store, string) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "agentlink.db")
	s, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s, p
}

func runChildren(t *testing.T, kind, db string, n int) []string {
	t.Helper()
	var wg sync.WaitGroup
	outs := make([]string, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			cmd := exec.Command(os.Args[0], "-test.run=^$")
			cmd.Env = append(os.Environ(), "AGENTLINK_TEST_CHILD="+kind, "AGENTLINK_TEST_DB="+db, "AGENTLINK_TEST_ID="+strconv.Itoa(i))
			out, err := cmd.Output()
			if err != nil {
				var ee *exec.ExitError
				if errors.As(err, &ee) {
					t.Errorf("%s %d: %v: %s", kind, i, err, ee.Stderr)
				} else {
					t.Errorf("%s %d: %v", kind, i, err)
				}
			}
			outs[i] = string(out)
		}(i)
	}
	wg.Wait()
	return outs
}

func TestEightProcesses(t *testing.T) {
	db := filepath.Join(t.TempDir(), "agentlink.db")
	runChildren(t, "writer", db, 8) // all eight create the file at once
	s, err := Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var n, distinct int
	s.db.QueryRow(`select count(*), count(distinct seq) from deliveries where recipient='sink' and state='queued'`).Scan(&n, &distinct)
	if n != 400 || distinct != 400 {
		t.Fatalf("after 8 writers x 50: %d deliveries, %d distinct seqs, want 400", n, distinct)
	}
	// Each writer's own messages must be in its send order.
	for w := 0; w < 8; w++ {
		hist, _ := s.History(fmt.Sprintf("w%d", w), "", 100)
		for i, m := range hist {
			if m.Body != fmt.Sprintf("w%d-%d", w, i) {
				t.Fatalf("writer %d message %d is %q", w, i, m.Body)
			}
		}
	}

	seen := map[string]int{}
	for i, out := range runChildren(t, "claimer", db, 8) {
		for _, l := range strings.Fields(out) {
			if prev, dup := seen[l]; dup {
				t.Fatalf("seq %s delivered to claimer %d and %d", l, prev, i)
			}
			seen[l] = i
		}
	}
	if len(seen) != 400 {
		t.Fatalf("8 concurrent claimers got %d messages, want 400", len(seen))
	}
}

func TestDedup(t *testing.T) {
	s, _ := open(t)
	a := Endpoint{Handle: "a", App: "test"}
	t0 := Now()
	defer func() { Now = func() int64 { return time.Now().UnixMilli() } }()
	Now = func() int64 { return t0 }
	first, _ := s.Send(a, []string{"b", "c"}, "hello", "", "")
	again, _ := s.Send(a, []string{"c", "b"}, "hello", "", "")
	if !again.Duplicate || again.ID != first.ID {
		t.Fatalf("retry within 2 min: %+v, want duplicate of %s", again, first.ID)
	}
	other, _ := s.Send(a, []string{"b"}, "hello", "", "")
	if other.Duplicate {
		t.Fatal("same body to a different recipient set was dropped")
	}
	Now = func() int64 { return t0 + DedupSpan.Milliseconds() + 1 }
	later, _ := s.Send(a, []string{"b", "c"}, "hello", "", "")
	if later.Duplicate {
		t.Fatal("same message after 2 min was dropped")
	}
}

func TestOrderAndThreads(t *testing.T) {
	s, _ := open(t)
	a := Endpoint{Handle: "a", App: "test"}
	t1, _ := s.Send(a, []string{"b"}, "one", "", "")
	s.Send(a, []string{"b"}, "other thread", "", "")
	s.Send(a, []string{"b"}, "two", t1.Thread, "")
	r, _ := s.Send(a, []string{"b"}, "three", "", t1.ID)
	if r.Thread != t1.Thread {
		t.Fatalf("reply_to didn't keep the thread: %s vs %s", r.Thread, t1.Thread)
	}
	got, _ := s.Inbox("b", t1.Thread)
	var bodies []string
	for _, m := range got {
		bodies = append(bodies, m.Body)
	}
	if strings.Join(bodies, ",") != "one,two,three" {
		t.Fatalf("thread inbox = %v", bodies)
	}
	if got[2].Hops != 3 {
		t.Fatalf("third message in thread has hops %d", got[2].Hops)
	}
	rest, _ := s.Inbox("b", "")
	if len(rest) != 1 || rest[0].Body != "other thread" {
		t.Fatalf("thread filter took other messages: %v", rest)
	}
	if again, _ := s.Inbox("b", ""); len(again) != 0 {
		t.Fatal("a message was delivered twice")
	}
}

func TestCancelRollback(t *testing.T) {
	s, _ := open(t)
	a := Endpoint{Handle: "a", App: "test"}
	s.Send(a, []string{"b"}, "x", "", "")
	got, _ := s.Inbox("b", "")
	if n, _, _ := s.Unread("b"); n != 0 {
		t.Fatalf("unread after claim = %d", n)
	}
	s.Unclaim("b", got)
	if n, from, _ := s.Unread("b"); n != 1 || from[0] != "a" {
		t.Fatalf("unread after unclaim = %d %v", n, from)
	}
	if again, _ := s.Inbox("b", ""); len(again) != 1 || again[0].ID != got[0].ID {
		t.Fatalf("unclaimed message not delivered again: %v", again)
	}
}

func TestExpiry(t *testing.T) {
	s, _ := open(t)
	a := Endpoint{Handle: "a", App: "test"}
	defer func() { Now = func() int64 { return time.Now().UnixMilli() } }()
	m, _ := s.Send(a, []string{"b"}, "stale", "", "")
	if ex, _ := s.TakeExpired("a"); len(ex) != 0 {
		t.Fatalf("fresh message reported expired: %v", ex)
	}
	t0 := Now()
	Now = func() int64 { return t0 + TTL.Milliseconds() + 1 }
	if got, _ := s.Inbox("b", ""); len(got) != 0 {
		t.Fatal("expired message was delivered")
	}
	ex, _ := s.TakeExpired("a")
	if len(ex) != 1 || ex[0].ID != m.ID || ex[0].Recipient != "b" {
		t.Fatalf("expiry notice = %v", ex)
	}
	if ex, _ := s.TakeExpired("a"); len(ex) != 0 {
		t.Fatal("expiry reported twice")
	}
}

func TestResolve(t *testing.T) {
	s, _ := open(t)
	defer func() { Now = func() int64 { return time.Now().UnixMilli() } }()
	t0 := Now()
	reg := func(dt int64, e Endpoint) {
		Now = func() int64 { return t0 + dt }
		if err := s.Register(e); err != nil {
			t.Fatal(err)
		}
	}
	reg(0, Endpoint{Handle: "codex-wisp-0001", App: "codex", Project: "wisp", Tier: "push"})
	reg(1, Endpoint{Handle: "codex-loom-0002", App: "codex", Project: "loom", Tier: "push"})
	reg(2, Endpoint{Handle: "claude-code-wisp-0003", App: "claude-code", Project: "wisp", Tier: "push"})
	reg(3, Endpoint{Handle: "claude-desktop", App: "claude-desktop", Tier: "pull"})
	me := Endpoint{Handle: "claude-code-wisp-0003", App: "claude-code", Project: "wisp"}
	pick := func(to string) string {
		got, err := s.Resolve(me, to)
		if err != nil {
			return "error: " + err.Error()
		}
		var h []string
		for _, e := range got {
			h = append(h, e.Handle)
		}
		sort.Strings(h)
		return strings.Join(h, ",")
	}
	for to, want := range map[string]string{
		"codex":                 "codex-wisp-0001", // same project beats more recent
		"Codex":                 "codex-wisp-0001",
		"codex-loom-0002":       "codex-loom-0002",
		"claude":                "claude-desktop", // never resolves to the sender itself
		"claude-desktop, codex": "claude-desktop,codex-wisp-0001",
		"codex,codex-wisp-0001": "codex-wisp-0001",
	} {
		if got := pick(to); got != want {
			t.Errorf("resolve %q = %s, want %s", to, got, want)
		}
	}
	if got := pick("cursor"); !strings.Contains(got, "No agent called cursor. Known: claude-desktop") {
		t.Errorf("unknown name: %s", got)
	}
	if err := s.Register(Endpoint{Handle: "codex-wisp-0001", App: "cursor"}); err == nil {
		t.Error("a handle moved to another app")
	}
}

func TestHandle(t *testing.T) {
	for _, c := range [][4]string{
		{"codex", `C:\Users\a\Videos\Wisp App`, "01a14b9b-df00-7a81-b9d5-899b23f8f923", "codex-wisp-app-f923"},
		{"claude-code", "/home/a/agentlink", "15026825-39b0-4bbb-a736-23574b916efa", "claude-code-agentlink-6efa"},
		{"claude-desktop", "", "", "claude-desktop"},
	} {
		if got := Handle(c[0], c[1], c[2]); got != c[3] {
			t.Errorf("Handle%v = %s", c[:3], got)
		}
	}
}
