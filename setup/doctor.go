package setup

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"agentlink/store"
)

// ExeName is agentlink's file name on this OS.
func ExeName() string {
	if runtime.GOOS == "windows" {
		return "agentlink.exe"
	}
	return "agentlink"
}

// InstallSelf copies the running executable to ~/.agentlink/bin and returns
// that path, which is what setup writes into every app. On Windows a running
// executable can't be replaced, but it can be renamed out of the way.
func InstallSelf(e Env) (string, error) {
	self, err := os.Executable()
	if err != nil {
		return "", err
	}
	target := filepath.Join(e.AgentHome, "bin", ExeName())
	if a, b := filepath.Clean(self), filepath.Clean(target); strings.EqualFold(a, b) {
		return target, nil
	}
	data, err := os.ReadFile(self)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return "", err
	}
	if exists(target) {
		old := strings.TrimSuffix(target, ".exe") + ".old" + filepath.Ext(target)
		os.Remove(old)
		if err := os.Rename(target, old); err != nil {
			return "", fmt.Errorf("move the old %s aside: %w", target, err)
		}
	}
	if err := os.WriteFile(target, data, 0o755); err != nil {
		return "", err
	}
	return target, nil
}

// ClaudeDesktopRunning reports whether Claude desktop is open. It writes its
// in-memory claude_desktop_config.json back when it quits, so an edit made
// while it runs is lost (spike 6). The CLI is claude.exe too, hence the path test.
func ClaudeDesktopRunning() bool {
	switch runtime.GOOS {
	case "windows":
		out, _ := exec.Command("powershell", "-NoProfile", "-NonInteractive", "-Command",
			"Get-Process -Name claude -ErrorAction SilentlyContinue | ForEach-Object { $_.Path }").Output()
		for _, p := range strings.Split(strings.ToLower(string(out)), "\n") {
			if strings.Contains(p, `\windowsapps\claude_`) || strings.Contains(p, `\anthropicclaude\`) {
				return true
			}
		}
	case "darwin":
		return exec.Command("pgrep", "-x", "Claude").Run() == nil
	}
	return false // ponytail: Linux desktop beta's process name is undocumented
}

// Doctor checks every installed app and agentlink itself, printing one line
// each. It returns false if anything needs fixing.
func Doctor(e Env, exe string, w io.Writer) bool {
	ok := true
	line := func(status, name, text string) {
		fmt.Fprintf(w, "%-4s  %-15s %s\n", status, name, text)
		if status == "FIX" {
			ok = false
		}
	}
	if !exists(exe) {
		line("FIX", "agentlink", exe+" is missing. Run the install line again.")
	}
	changes, found, err := Plan(e, exe, nil)
	if err != nil {
		line("FIX", "config", err.Error())
	}
	pending := map[string][]string{}
	for _, c := range changes {
		pending[c.App] = append(pending[c.App], fmt.Sprintf("%s needs: %s", Tilde(e, c.Path), c.What))
	}
	installed := map[string]bool{}
	for _, a := range found {
		installed[a.Host] = true
		if p := pending[a.Host]; len(p) > 0 {
			line("FIX", a.Name, strings.Join(p, "; ")+". Run `agentlink setup`.")
			continue
		}
		var files []string
		seen := map[string]bool{}
		for _, ed := range a.Edits(e, exe) {
			if !seen[ed.Path] {
				seen[ed.Path] = true
				files = append(files, Tilde(e, ed.Path))
			}
		}
		line("ok", a.Name, "wired up in "+strings.Join(files, ", "))
		if a.Host == "codex" {
			codexNote(line)
		}
	}
	for _, a := range Apps {
		if !installed[a.Host] {
			line("--", a.Name, "not installed")
		}
	}

	if st, err := store.Open(filepath.Join(e.AgentHome, "agentlink.db")); err != nil {
		line("FIX", "database", err.Error())
	} else {
		if err := st.Writable(); err != nil {
			line("FIX", "database", err.Error())
		} else {
			line("ok", "database", Tilde(e, filepath.Join(e.AgentHome, "agentlink.db"))+" is writable")
		}
		st.Close()
	}
	if exists(exe) {
		if err := roundTrip(exe); err != nil {
			line("FIX", "round trip", err.Error())
		} else {
			line("ok", "round trip", "a test message went from one agentlink MCP server to another")
		}
	}
	return ok
}

// cleanEnv drops the variables that would tie a test server to a real session.
func cleanEnv() []string {
	var out []string
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		if !strings.HasPrefix(strings.ToUpper(k), "CLAUDE_") && !strings.HasPrefix(strings.ToUpper(k), "CODEX_") && !strings.EqualFold(k, "AGENTLINK_HOME") {
			out = append(out, kv)
		}
	}
	return out
}

// Tilde shortens a path under the home folder to ~/...
func Tilde(e Env, p string) string {
	if rel, err := filepath.Rel(e.Home, p); err == nil && !strings.HasPrefix(rel, "..") {
		return "~" + string(filepath.Separator) + rel
	}
	return p
}

func codexNote(line func(string, string, string)) {
	path, err := exec.LookPath("codex")
	if err != nil {
		line("--", "Codex CLI", "not on PATH. Codex desktop threads still wake; other agents can't use `codex queue` until the CLI is installed.")
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if exec.CommandContext(ctx, path, "app-server", "daemon", "version").Run() == nil {
		line("ok", "Codex CLI", "the shared daemon is running, so CLI threads wake too")
	} else {
		line("--", "Codex CLI", "Codex's shared app-server daemon isn't running. Desktop threads wake anyway; a CLI thread wakes only while it runs on that daemon.")
	}
}

// roundTrip runs two MCP servers against a throwaway database: one sends, the other reads.
func roundTrip(exe string) error {
	home, err := os.MkdirTemp("", "agentlink-doctor-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(home)
	type proc struct {
		in     io.WriteCloser
		out    *bufio.Scanner
		cmd    *exec.Cmd
		stderr *strings.Builder
	}
	died := func(p *proc, method string) error {
		p.cmd.Wait()
		return fmt.Errorf("agentlink mcp --host %s exited during %s: %s", p.cmd.Args[3], method, strings.TrimSpace(p.stderr.String()))
	}
	start := func(host string, env ...string) (*proc, error) {
		cmd := exec.Command(exe, "mcp", "--host", host)
		cmd.Env = append(cleanEnv(), append(env, "AGENTLINK_HOME="+home)...)
		in, _ := cmd.StdinPipe()
		out, _ := cmd.StdoutPipe()
		stderr := &strings.Builder{}
		cmd.Stderr = stderr
		if err := cmd.Start(); err != nil {
			return nil, err
		}
		p := &proc{in, bufio.NewScanner(out), cmd, stderr}
		p.out.Buffer(make([]byte, 1<<20), 1<<20)
		return p, nil
	}
	call := func(p *proc, id int, method string, params any) (map[string]any, error) {
		b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
		if _, err := p.in.Write(append(b, '\n')); err != nil {
			return nil, died(p, method)
		}
		done := make(chan bool, 1)
		go func() { done <- p.out.Scan() }()
		select {
		case got := <-done:
			if !got {
				return nil, died(p, method)
			}
		case <-time.After(10 * time.Second):
			return nil, fmt.Errorf("agentlink mcp didn't answer %s within 10 s", method)
		}
		var r map[string]any
		if err := json.Unmarshal(p.out.Bytes(), &r); err != nil {
			return nil, err
		}
		if r["error"] != nil {
			return nil, fmt.Errorf("%s: %v", method, r["error"])
		}
		return r, nil
	}
	text := func(r map[string]any) string {
		res, _ := r["result"].(map[string]any)
		c, _ := res["content"].([]any)
		if len(c) == 0 {
			return ""
		}
		t, _ := c[0].(map[string]any)["text"].(string)
		return t
	}
	a, err := start("claude-code", "CLAUDE_CODE_SESSION_ID=doctor-0000-a", "CLAUDE_PROJECT_DIR="+home)
	if err != nil {
		return err
	}
	defer func() { a.in.Close(); a.cmd.Wait() }()
	b, err := start("claude-desktop")
	if err != nil {
		return err
	}
	defer func() { b.in.Close(); b.cmd.Wait() }()
	for i, p := range []*proc{a, b} {
		r, err := call(p, 1, "initialize", map[string]any{"protocolVersion": "2025-11-25", "clientInfo": map[string]any{"name": "agentlink-doctor"}})
		if err != nil {
			return err
		}
		if res, _ := r["result"].(map[string]any); res["protocolVersion"] != "2025-11-25" {
			return fmt.Errorf("server %d answered initialize with %v", i, r)
		}
		r, err = call(p, 2, "tools/list", nil)
		if err != nil {
			return err
		}
		if res, _ := r["result"].(map[string]any); len(res["tools"].([]any)) != 3 {
			return fmt.Errorf("tools/list returned %v", res)
		}
	}
	// b waits in agentlink_inbox before a sends, the way a Claude desktop chat
	// waits for a reply, so the send needs no notification.
	inbox := make(chan error, 1)
	go func() {
		r, err := call(b, 3, "tools/call", map[string]any{"name": "agentlink_inbox", "arguments": map[string]any{"wait_seconds": 8}})
		if err == nil && !strings.Contains(text(r), "agentlink doctor ping") {
			err = fmt.Errorf("the message didn't arrive: %s", text(r))
		}
		inbox <- err
	}()
	time.Sleep(500 * time.Millisecond)
	r, err := call(a, 3, "tools/call", map[string]any{"name": "agentlink_send", "arguments": map[string]any{"to": "claude-desktop", "text": "agentlink doctor ping"}})
	if err != nil {
		return err
	}
	if t := text(r); !strings.HasPrefix(t, "Sent ") || !strings.Contains(t, "was waiting and read it at once") {
		return fmt.Errorf("send failed: %s", t)
	}
	return <-inbox
}
