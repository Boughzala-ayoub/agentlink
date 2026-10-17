// waiter is a throwaway asyncRewake hook for spike 2. It logs its hook input,
// saves the session's messaging socket and token to sock-<session>.json, then
// polls for trigger-<session> (or trigger-any) in its own folder. When one
// appears it deletes it, writes a wake line to stderr and exits 2.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

func main() {
	exe, _ := os.Executable()
	dir := filepath.Dir(exe)
	in, _ := io.ReadAll(os.Stdin)
	var h struct {
		SessionID string `json:"session_id"`
		Event     string `json:"hook_event_name"`
		Source    string `json:"source"`
	}
	json.Unmarshal(in, &h)
	logF, _ := os.OpenFile(filepath.Join(dir, "waiter.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	start := time.Now()
	log := func(f string, a ...any) {
		fmt.Fprintf(logF, "%s pid=%d %s/%s "+f+"\n", append([]any{time.Now().Format("15:04:05"), os.Getpid(), h.Event, h.SessionID[:min(8, len(h.SessionID))]}, a...)...)
	}
	log("START input=%s", in)
	sock, _ := json.Marshal(map[string]string{"socket": os.Getenv("CLAUDE_CODE_MESSAGING_SOCKET"), "token": os.Getenv("CLAUDE_CODE_MESSAGING_TOKEN")})
	os.WriteFile(filepath.Join(dir, "sock-"+h.SessionID+".json"), sock, 0o600)
	last := start
	for {
		for _, name := range []string{"trigger-" + h.SessionID, "trigger-any"} {
			p := filepath.Join(dir, name)
			if b, err := os.ReadFile(p); err == nil {
				os.Remove(p)
				log("FIRE after %s via %s", time.Since(start).Round(time.Second), name)
				fmt.Fprintf(os.Stderr, "agentlink spike: wake via asyncRewake (%s). %s", h.Event, b)
				os.Exit(2)
			}
		}
		if time.Since(last) > time.Minute {
			last = time.Now()
			log("alive %s", time.Since(start).Round(time.Second))
		}
		time.Sleep(250 * time.Millisecond)
	}
}
