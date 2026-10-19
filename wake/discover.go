package wake

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"agentlink/store"
)

// Discover registers Codex threads that never called agentlink, so "ask Codex"
// works before Codex has used agentlink once. It runs only when `to` names the
// codex app and no Codex endpoint was active in the last ten minutes.
func Discover(st *store.Store, to string) {
	if os.Getenv("AGENTLINK_NO_DISCOVER") != "" { // tests on a machine with real Codex threads
		return
	}
	asked := false
	for _, n := range strings.Split(to, ",") {
		asked = asked || strings.EqualFold(strings.TrimSpace(n), "codex")
	}
	if !asked {
		return
	}
	if ps, err := st.Peers(); err == nil {
		for _, p := range ps {
			if p.App == "codex" && p.Session != "" && store.Now()-p.LastSeen < (10*time.Minute).Milliseconds() {
				return
			}
		}
	}
	threads, err := codexThreads()
	if err != nil {
		return // Resolve then reports "no agent called codex", which is the truth agentlink can see
	}
	kept := 0
	for _, t := range threads { // newest first
		if kept == 5 {
			break
		}
		if store.Now()-t.RecencyAt*1000 > store.TTL.Milliseconds() {
			continue // older than a message would live
		}
		if t.RecencyAt < t.CreatedAt {
			continue // a Claude Code session Codex desktop imported and nobody continued in Codex
		}
		st.Register(store.Endpoint{Handle: store.Handle("codex", t.Cwd, t.ID), App: "codex", Project: base(t.Cwd),
			Session: t.ID, Tier: "push", LastSeen: t.RecencyAt * 1000})
		kept++
	}
}

type codexThread struct {
	ID         string `json:"id"`
	Cwd        string `json:"cwd"`
	CreatedAt  int64  `json:"createdAt"`
	RecencyAt  int64  `json:"recencyAt"`
	Originator string `json:"originator"`
}

// codexThreads asks Codex's own app-server for its most recent desktop threads
// (spike 4; their source is "vscode"). CLI threads are left out: one started
// with --no-daemon, as Loom starts them, never drains `codex queue`, and only
// its own first agentlink call tells agentlink which kind it is.
func codexThreads() ([]codexThread, error) {
	exe := CodexCommand()
	if exe == "" {
		return nil, fmt.Errorf("codex isn't installed")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, exe, "app-server")
	in, _ := cmd.StdinPipe()
	out, _ := cmd.StdoutPipe()
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	defer func() { in.Close(); cmd.Process.Kill(); cmd.Wait() }()
	for _, m := range []any{
		map[string]any{"id": 1, "method": "initialize", "params": map[string]any{"clientInfo": map[string]any{"name": "agentlink", "version": "1"}}},
		map[string]any{"method": "initialized"},
		map[string]any{"id": 2, "method": "thread/list", "params": map[string]any{"sortKey": "recency_at", "limit": 100, "sourceKinds": []string{"vscode"}}},
	} {
		b, _ := json.Marshal(m)
		in.Write(append(b, '\n'))
	}
	sc := bufio.NewScanner(out)
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	for sc.Scan() {
		var r struct {
			ID     int `json:"id"`
			Result struct {
				Data []codexThread `json:"data"`
			} `json:"result"`
			Error any `json:"error"`
		}
		if json.Unmarshal(sc.Bytes(), &r) != nil || r.ID != 2 {
			continue
		}
		if r.Error != nil {
			return nil, fmt.Errorf("thread/list: %v", r.Error)
		}
		return r.Result.Data, nil
	}
	return nil, fmt.Errorf("codex app-server ended without answering: %v", sc.Err())
}
