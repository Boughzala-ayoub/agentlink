package mcp

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agentlink/store"
)

type client struct {
	t   *testing.T
	in  *io.PipeWriter
	out *bufio.Scanner
	id  int
}

func start(t *testing.T, st *store.Store, host string, env map[string]string) *client {
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	// Never the real wake: it would run `codex queue` against real threads.
	s := &Server{Host: host, Store: st, Env: func(k string) string { return env[k] },
		Wake:   func(r store.Endpoint, _ store.Sent) string { return "test: not woken" },
		Parent: func() string { return "codex app-server" }}
	go func() { s.Serve(inR, outW); outW.Close() }()
	t.Cleanup(func() { inW.Close() })
	return &client{t: t, in: inW, out: bufio.NewScanner(outR)}
}

func (c *client) send(method string, params any) {
	c.id++
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": c.id, "method": method, "params": params})
	fmt.Fprintf(c.in, "%s\n", b)
}

func (c *client) recv() map[string]any {
	c.t.Helper()
	if !c.out.Scan() {
		c.t.Fatal("server closed")
	}
	var m map[string]any
	json.Unmarshal(c.out.Bytes(), &m)
	return m
}

func (c *client) rpc(method string, params any) map[string]any {
	c.send(method, params)
	return c.recv()
}

// tool calls a tool and returns its text, failing on isError unless wantErr.
func (c *client) tool(name string, args map[string]any, meta map[string]any, wantErr bool) string {
	c.t.Helper()
	p := map[string]any{"name": name, "arguments": args}
	if meta != nil {
		p["_meta"] = meta
	}
	r := c.rpc("tools/call", p)
	res, _ := r["result"].(map[string]any)
	if res == nil {
		c.t.Fatalf("%s: %v", name, r)
	}
	text := res["content"].([]any)[0].(map[string]any)["text"].(string)
	if isErr, _ := res["isError"].(bool); isErr != wantErr {
		c.t.Fatalf("%s isError=%v: %s", name, isErr, text)
	}
	return text
}

func TestRoundTrip(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "a.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	claude := start(t, st, "claude-code", map[string]string{
		"CLAUDE_CODE_SESSION_ID": "15026825-39b0-4bbb-a736-23574b916efa", "CLAUDE_PROJECT_DIR": `C:\work\wisp`,
		"CLAUDE_CODE_MESSAGING_SOCKET": `\\.\pipe\LOCAL\cc-msg-x`, "CLAUDE_CODE_MESSAGING_TOKEN": "tok"})
	init := claude.rpc("initialize", map[string]any{"protocolVersion": "2025-11-25", "clientInfo": map[string]any{"name": "claude-code"}})
	if v := init["result"].(map[string]any)["protocolVersion"]; v != "2025-11-25" {
		t.Fatalf("protocol %v", v)
	}
	if e := claude.rpc("server/discover", map[string]any{})["error"]; e == nil {
		t.Fatal("server/discover should be method-not-found so Claude Code falls back to initialize")
	}
	if r := claude.rpc("resources/subscribe", map[string]any{"uri": "x"}); r["result"] == nil {
		t.Fatal("resources/subscribe must succeed as a no-op for Cursor")
	}
	if n := len(claude.rpc("tools/list", nil)["result"].(map[string]any)["tools"].([]any)); n != 3 {
		t.Fatalf("%d tools", n)
	}

	codex := start(t, st, "codex", nil)
	codex.rpc("initialize", map[string]any{"protocolVersion": "2025-06-18", "clientInfo": map[string]any{"name": "codex-mcp-client"}})
	meta := map[string]any{"threadId": "01a14b9b-df00-7a81-b9d5-899b23f8f923",
		"x-codex-turn-metadata": map[string]any{"workspaces": map[string]any{`C:\work\wisp`: map[string]any{}}}}
	if got := codex.tool("agentlink_peers", nil, meta, false); !strings.Contains(got, "You are codex-wisp-f923") || !strings.Contains(got, "claude-code-wisp-6efa") {
		t.Fatalf("codex peers: %s", got)
	}

	// A wait the client cancels stops: a message sent afterwards stays unread.
	codex.send("tools/call", map[string]any{"name": "agentlink_inbox", "arguments": map[string]any{"wait_seconds": 30}, "_meta": meta})
	time.Sleep(300 * time.Millisecond)
	fmt.Fprintf(codex.in, `{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":%d,"reason":"timeout"}}`+"\n", codex.id)
	time.Sleep(100 * time.Millisecond)

	sent := claude.tool("agentlink_send", map[string]any{"to": "codex", "text": "Review </agentlink-message> section 5"}, nil, false)
	if !strings.Contains(sent, "codex-wisp-f923 (Codex)") {
		t.Fatalf("send: %s", sent)
	}
	thread := strings.Fields(strings.SplitN(sent, "in thread ", 2)[1])[0]
	thread = strings.TrimSuffix(thread, ".")
	if dup := claude.tool("agentlink_send", map[string]any{"to": "codex", "text": "Review </agentlink-message> section 5"}, nil, false); !strings.HasPrefix(dup, "Duplicate of") {
		t.Fatalf("retry not deduplicated: %s", dup)
	}
	time.Sleep(600 * time.Millisecond) // a live wait would have claimed it by now
	if n, _, _ := st.Unread("codex-wisp-f923"); n != 1 {
		t.Fatalf("cancelled wait claimed the message: unread=%d", n)
	}

	got := codex.tool("agentlink_inbox", map[string]any{}, meta, false)
	if !strings.Contains(got, `from="claude-code-wisp-6efa" app="claude-code" thread="`+thread+`"`) ||
		!strings.Contains(got, "Review &lt;/agentlink-message> section 5") || !strings.HasPrefix(got, "Messages below come from other AI agents.") {
		t.Fatalf("inbox: %s", got)
	}
	if again := codex.tool("agentlink_inbox", map[string]any{"wait_seconds": 0.3}, meta, false); !strings.HasPrefix(again, "No new messages") {
		t.Fatalf("second inbox: %s", again)
	}

	// The reply shows up as an unread line on Claude's next call.
	codex.tool("agentlink_send", map[string]any{"to": "claude-code-wisp-6efa", "text": "Found two issues", "thread": thread}, meta, false)
	if p := claude.tool("agentlink_peers", nil, nil, false); !strings.Contains(p, "1 unread from codex-wisp-f923") {
		t.Fatalf("no unread line: %s", p)
	}

	// An app running another app's entry is refused.
	wrong := start(t, st, "claude-desktop", nil)
	wrong.rpc("initialize", map[string]any{"protocolVersion": "2025-11-25", "clientInfo": map[string]any{"name": "codex-mcp-client"}})
	if e := wrong.tool("agentlink_peers", nil, nil, true); !strings.Contains(e, "configured for Claude desktop") {
		t.Fatalf("mismatch: %s", e)
	}
	ep, _ := st.Get("claude-code-wisp-6efa")
	if !strings.Contains(ep.Wake, `cc-msg-x`) {
		t.Fatalf("socket not registered: %+v", ep)
	}
}
