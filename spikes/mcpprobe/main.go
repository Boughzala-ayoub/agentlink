// mcpprobe is a throwaway stdio MCP server for the M0 spikes. It logs every
// line it reads and writes, plus its environment, to $PROBE_LOG (default
// probe.log next to the binary), and offers probe_sleep and probe_env tools.
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

var (
	logMu sync.Mutex
	logF  *os.File
	outMu sync.Mutex
	wg    sync.WaitGroup
)

func logf(format string, a ...any) {
	logMu.Lock()
	defer logMu.Unlock()
	fmt.Fprintf(logF, "%s %d "+format+"\n", append([]any{time.Now().Format("15:04:05.000"), os.Getpid()}, a...)...)
}

func write(v any) {
	b, _ := json.Marshal(v)
	outMu.Lock()
	os.Stdout.Write(append(b, '\n'))
	outMu.Unlock()
	logf("OUT %s", b)
}

type req struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
}

func main() {
	p := os.Getenv("PROBE_LOG")
	if p == "" {
		exe, _ := os.Executable()
		p = filepath.Join(filepath.Dir(exe), "probe.log")
	}
	logF, _ = os.OpenFile(p, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	env := os.Environ()
	sort.Strings(env)
	logf("START args=%q ppid=%d", os.Args, os.Getppid())
	for _, e := range env {
		if strings.HasPrefix(e, "CLAUDE") || strings.HasPrefix(e, "CODEX") || strings.HasPrefix(e, "OPENCODE") || strings.HasPrefix(e, "CURSOR") || strings.HasPrefix(e, "MCP") || strings.HasPrefix(e, "VSCODE") || strings.HasPrefix(e, "TERM_PROGRAM") {
			if k, v, _ := strings.Cut(e, "="); strings.Contains(k, "TOKEN") {
				e = fmt.Sprintf("%s=<%d chars>", k, len(v))
			}
			logf("ENV %s", e)
		}
	}
	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 1<<20), 1<<24)
	for sc.Scan() {
		line := sc.Text()
		logf("IN  %s", line)
		var r req
		if json.Unmarshal([]byte(line), &r) != nil || r.ID == nil {
			continue
		}
		wg.Add(1)
		go func() { defer wg.Done(); handle(r) }()
	}
	wg.Wait()
	logf("EOF err=%v", sc.Err())
}

func handle(r req) {
	reply := func(result any) { write(map[string]any{"jsonrpc": "2.0", "id": r.ID, "result": result}) }
	switch r.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		json.Unmarshal(r.Params, &p)
		reply(map[string]any{
			"protocolVersion": p.ProtocolVersion,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "mcpprobe", "version": "0.0.1"},
		})
	case "tools/list":
		num := map[string]any{"type": "object", "properties": map[string]any{"seconds": map[string]any{"type": "number"}}, "required": []string{"seconds"}}
		reply(map[string]any{"tools": []any{
			map[string]any{"name": "probe_sleep", "description": "Sleeps for the given seconds, then returns how long it slept.", "inputSchema": num,
				"annotations": map[string]any{"readOnlyHint": true}},
			map[string]any{"name": "probe_env", "description": "Returns agent-related environment variables of this MCP server process.",
				"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}}, "annotations": map[string]any{"readOnlyHint": true}},
			map[string]any{"name": "probe_send", "description": "Pretend to send a message to another agent. Returns ok.",
				"inputSchema": map[string]any{"type": "object", "properties": map[string]any{"text": map[string]any{"type": "string"}}},
				"annotations": map[string]any{"destructiveHint": false, "openWorldHint": false}},
		}})
	case "tools/call":
		var p struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		}
		json.Unmarshal(r.Params, &p)
		text := "ok"
		switch p.Name {
		case "probe_sleep":
			s, _ := p.Arguments["seconds"].(float64)
			start := time.Now()
			time.Sleep(time.Duration(s * float64(time.Second)))
			text = fmt.Sprintf("slept %.1fs", time.Since(start).Seconds())
		case "probe_env":
			var b strings.Builder
			for _, e := range os.Environ() {
				if strings.HasPrefix(e, "CLAUDE") || strings.HasPrefix(e, "CODEX") || strings.HasPrefix(e, "OPENCODE") || strings.HasPrefix(e, "CURSOR") {
					k, v, _ := strings.Cut(e, "=")
					if strings.Contains(k, "TOKEN") {
						v = fmt.Sprintf("<%d chars>", len(v))
					}
					b.WriteString(k + "=" + v + "\n")
				}
			}
			text = b.String()
		}
		reply(map[string]any{"content": []any{map[string]any{"type": "text", "text": text}}})
	case "ping":
		reply(map[string]any{})
	default:
		write(map[string]any{"jsonrpc": "2.0", "id": r.ID, "error": map[string]any{"code": -32601, "message": "method not found: " + r.Method}})
	}
}
