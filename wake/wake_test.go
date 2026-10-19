package wake

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"agentlink/store"
)

// As a fake `codex`, the test binary appends its arguments to $FAKE_CODEX_LOG.
func TestMain(m *testing.M) {
	if log := os.Getenv("FAKE_CODEX_LOG"); log != "" {
		f, _ := os.OpenFile(log, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		b, _ := json.Marshal(os.Args[1:])
		fmt.Fprintln(f, string(b))
		f.Close()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func openStore(t *testing.T) *store.Store {
	st, err := store.Open(filepath.Join(t.TempDir(), "a.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func TestCodexWakeCoalesces(t *testing.T) {
	st := openStore(t)
	log := filepath.Join(t.TempDir(), "codex.log")
	t.Setenv("FAKE_CODEX_LOG", log)
	CodexCommand = func() string { return os.Args[0] }
	defer func() { CodexCommand = func() string { return "" } }()

	cx := store.Endpoint{Handle: "codex-wisp-f923", App: "codex", Session: "01a14b9b-df00-7a81-b9d5-899b23f8f923", Tier: "push", Wake: `{"daemonless":false}`}
	st.Register(cx)
	from := store.Endpoint{Handle: "claude-desktop", App: "claude-desktop"}
	calls := func() []string {
		b, _ := os.ReadFile(log)
		return strings.Split(strings.TrimSpace(string(b)), "\n")
	}

	s1, _ := st.Send(from, []string{cx.Handle}, "first", "", "")
	if got := Deliver(st, cx, s1); !strings.HasPrefix(got, "queued in Codex") {
		t.Fatalf("first send: %s", got)
	}
	var args []string
	json.Unmarshal([]byte(calls()[0]), &args)
	if strings.Join(args[:3], " ") != "queue --thread "+cx.Session || args[3] != "--message" ||
		!strings.Contains(args[4], "1 new message for codex-wisp-f923 from claude-desktop in thread "+s1.Thread) ||
		strings.Contains(args[4], "first") {
		t.Fatalf("codex queue args: %q", args)
	}

	s2, _ := st.Send(from, []string{cx.Handle}, "second", s1.Thread, "")
	if got := Deliver(st, cx, s2); !strings.Contains(got, "already woken") || len(calls()) != 1 {
		t.Fatalf("second send woke again: %s, %d calls", got, len(calls()))
	}
	if msgs, _ := st.Inbox(cx.Handle, ""); len(msgs) != 2 {
		t.Fatalf("inbox after one wake: %d messages", len(msgs))
	}
	s3, _ := st.Send(from, []string{cx.Handle}, "third", s1.Thread, "")
	Deliver(st, cx, s3)
	if len(calls()) != 2 {
		t.Fatalf("a send after the inbox was read didn't wake: %d calls", len(calls()))
	}

	dl := cx
	dl.Handle, dl.Wake = "codex-loom-0001", `{"daemonless":true}`
	st.Register(dl)
	s4, _ := st.Send(from, []string{dl.Handle}, "x", "", "")
	if got := Deliver(st, dl, s4); !strings.Contains(got, "--no-daemon") || len(calls()) != 2 {
		t.Fatalf("daemonless thread: %s, %d calls", got, len(calls()))
	}
}

func TestClaudeSocket(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the Windows named pipe is covered by the live M3 round trip")
	}
	st := openStore(t)
	sock := filepath.Join(t.TempDir(), "s")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	got := make(chan []string, 1)
	go func() {
		c, err := l.Accept()
		if err != nil {
			return
		}
		var lines []string
		sc := bufio.NewScanner(c)
		for sc.Scan() {
			lines = append(lines, sc.Text())
		}
		got <- lines
	}()
	w, _ := json.Marshal(map[string]string{"socket": sock, "token": "tok"})
	cc := store.Endpoint{Handle: "claude-code-wisp-6efa", App: "claude-code", Tier: "push", Wake: string(w)}
	st.Register(cc)
	sent, _ := st.Send(store.Endpoint{Handle: "codex-x", App: "codex"}, []string{cc.Handle}, "hi", "", "")
	if s := Deliver(st, cc, sent); !strings.HasPrefix(s, "woken") {
		t.Fatal(s)
	}
	lines := <-got
	if len(lines) != 2 || lines[0] != `{"token":"tok","type":"auth"}` || !strings.Contains(lines[1], `"type":"user"`) || !strings.Contains(lines[1], "agentlink: 1 new message for claude-code-wisp-6efa") {
		t.Fatalf("socket got %q", lines)
	}
}

func TestDeadSocketKeepsMessageQueued(t *testing.T) {
	st := openStore(t)
	cc := store.Endpoint{Handle: "claude-code-gone", App: "claude-code", Tier: "push", Wake: `{"socket":"` + strings.ReplaceAll(filepath.Join(t.TempDir(), "nope"), `\`, `\\`) + `","token":"x"}`}
	st.Register(cc)
	sent, _ := st.Send(store.Endpoint{Handle: "a", App: "codex"}, []string{cc.Handle}, "hi", "", "")
	if s := Deliver(st, cc, sent); !strings.HasPrefix(s, "stored, not woken: posting to its Claude Code session failed") {
		t.Fatal(s)
	}
	if need, _, _ := st.NeedsWake(cc.Handle); !need {
		t.Fatal("a failed wake must leave the message waiting for the next wake")
	}
}

// Read-only against the real Codex: AGENTLINK_LIVE=1 go test -run TestDiscoverLive -v ./wake/
func TestDiscoverLive(t *testing.T) {
	if os.Getenv("AGENTLINK_LIVE") == "" {
		t.Skip("set AGENTLINK_LIVE=1 to list this machine's Codex threads")
	}
	CodexCommand = defaultCodex
	st := openStore(t)
	Discover(st, "codex")
	ps, _ := st.Peers()
	for _, p := range ps {
		t.Logf("%s project=%s session=%s seen %s ago", p.Handle, p.Project, p.Session, store.Ago(p.LastSeen))
	}
	if len(ps) == 0 {
		t.Fatal("no Codex threads found")
	}
}

// A recipient inside an agentlink_inbox wait reads the message itself; waking it would cost a turn.
func TestWaitingRecipientIsNotWoken(t *testing.T) {
	st := openStore(t)
	log := filepath.Join(t.TempDir(), "codex.log")
	t.Setenv("FAKE_CODEX_LOG", log)
	CodexCommand = func() string { return os.Args[0] }
	defer func() { CodexCommand = defaultCodex }()
	cx := store.Endpoint{Handle: "codex-w", App: "codex", Session: "t1", Tier: "push"}
	st.Register(cx)
	st.SetWaiting(cx.Handle, store.Now()+30_000)
	sent, _ := st.Send(store.Endpoint{Handle: "a", App: "claude-code"}, []string{cx.Handle}, "q", "", "")
	go func() { time.Sleep(300 * time.Millisecond); st.Inbox(cx.Handle, "") }()
	if got := Deliver(st, cx, sent); !strings.HasPrefix(got, "delivered: its agentlink_inbox call was waiting") {
		t.Fatal(got)
	}
	if _, err := os.Stat(log); err == nil {
		t.Fatal("codex queue ran for a recipient that was waiting")
	}
}

func TestCursorStopHook(t *testing.T) {
	st := openStore(t)
	run := func(input string) string {
		var out strings.Builder
		if err := Hook(st, "cursor", "stop", strings.NewReader(input), &out); err != nil {
			t.Fatal(err)
		}
		return out.String()
	}
	in := "\xef\xbb\xbf" + `{"conversation_id":"c1","status":"completed","loop_count":0,"hook_event_name":"stop"}`
	if got := run(in); got != "" {
		t.Fatalf("no mail, but the hook said %q", got)
	}
	sent, _ := st.Send(store.Endpoint{Handle: "codex-x", App: "codex"}, []string{"cursor"}, "review this", "", "")
	if got := run(`{"status":"aborted","loop_count":0}`); got != "" {
		t.Fatalf("a turn the user stopped got a follow-up: %q", got)
	}
	if got := run(`{"status":"completed","loop_count":5}`); got != "" {
		t.Fatalf("past Cursor's loop limit: %q", got)
	}
	got := run(in)
	var m map[string]string
	if json.Unmarshal([]byte(got), &m) != nil || !strings.HasPrefix(m["followup_message"], "agentlink: 1 new message for cursor from codex-x") || strings.Contains(got, "review this") {
		t.Fatalf("follow-up = %q", got)
	}
	if again := run(in); again != "" {
		t.Fatalf("woke twice for one message: %q", again)
	}
	st.Inbox("cursor", sent.Thread)
	st.Send(store.Endpoint{Handle: "codex-x", App: "codex"}, []string{"cursor"}, "next", "", "")
	if got := run(in); !strings.Contains(got, "followup_message") {
		t.Fatalf("new mail after reading didn't wake: %q", got)
	}
}

func TestClaudeDesktopNotifiesOnce(t *testing.T) {
	st := openStore(t)
	var shown []string
	Notify = func(title, body, url string) error { shown = append(shown, body+" "+url); return nil }
	cd := store.Endpoint{Handle: "claude-desktop", App: "claude-desktop", Tier: "pull"}
	st.Register(cd)
	from := store.Endpoint{Handle: "codex-x", App: "codex"}
	s1, _ := st.Send(from, []string{cd.Handle}, "a", "", "")
	if got := Deliver(st, cd, s1); !strings.Contains(got, "showed its user a notification") {
		t.Fatal(got)
	}
	s2, _ := st.Send(from, []string{cd.Handle}, "b", "", "")
	if got := Deliver(st, cd, s2); !strings.Contains(got, "already told") {
		t.Fatal(got)
	}
	if len(shown) != 1 || !strings.Contains(shown[0], `codex-x messaged your Claude desktop chat. Type "inbox" there`) || !strings.HasSuffix(shown[0], "claude://") {
		t.Fatalf("notifications: %q", shown)
	}
}

func TestVSCodeAndGeminiHooks(t *testing.T) {
	st := openStore(t)
	for _, c := range []struct{ host, event, want string }{
		{"vscode", "stop", `{"hookSpecificOutput":{"decision":"block","hookEventName":"Stop","reason":"agentlink: 1 new message for vscode`},
		{"gemini", "after-agent", `{"decision":"deny","reason":"agentlink: 1 new message for gemini`},
	} {
		st.Register(store.Endpoint{Handle: c.host, App: c.host, Tier: "turn"})
		st.Send(store.Endpoint{Handle: "codex-x", App: "codex"}, []string{c.host}, "hi", "", "")
		var out strings.Builder
		if err := Hook(st, c.host, c.event, strings.NewReader(`{"session_id":"s","stop_hook_active":false}`), &out); err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(out.String(), c.want) {
			t.Errorf("%s %s: %s", c.host, c.event, out.String())
		}
	}
}

// Claude Code's UserPromptSubmit hook lifts a loop-guard pause when a person
// types, but not for agentlink's own wake line arriving as a prompt.
func TestClaudeCodePromptHook(t *testing.T) {
	st := openStore(t)
	defer func() { store.Now = func() int64 { return time.Now().UnixMilli() } }()
	clock := time.Now().UnixMilli()
	store.Now = func() int64 { return clock }
	t.Setenv("CLAUDE_PROJECT_DIR", "/work/proj")
	cc := store.Endpoint{Handle: store.Handle("claude-code", "/work/proj", "sess-abcd"), App: "claude-code", Tier: "push"}
	st.Register(cc)
	from := store.Endpoint{Handle: "codex-x", App: "codex"}
	sent, _ := st.Send(from, []string{cc.Handle}, "0", "", "")
	for i := 1; i <= store.MaxAgentWakes; i++ {
		clock += 20_000
		st.MarkWoken(cc.Handle)
		st.Inbox(cc.Handle, "")
		st.Send(from, []string{cc.Handle}, fmt.Sprint(i), sent.Thread, "")
	}
	prompt := func(text string) {
		clock++
		in, _ := json.Marshal(map[string]string{"session_id": "sess-abcd", "cwd": "/elsewhere", "prompt": text, "hook_event_name": "UserPromptSubmit"})
		var out strings.Builder
		if err := Hook(st, "claude-code", "prompt", strings.NewReader(string(in)), &out); err != nil || out.Len() > 0 {
			t.Fatalf("hook: %v, printed %q", err, out.String())
		}
	}
	prompt(Line(st, cc, sent.Thread))
	if _, held, _ := st.NeedsWake(cc.Handle); held != store.HeldPaused {
		t.Fatalf("the wake line counted as a person: held %q", held)
	}
	prompt("keep going")
	if need, _, _ := st.NeedsWake(cc.Handle); !need {
		t.Fatal("still paused after the user typed")
	}
}
