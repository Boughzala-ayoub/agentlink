package wake

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"agentlink/store"
)

// OpencodeFeed runs as a child of agentlink's opencode plugin. The plugin
// writes {"session","directory"} lines for the sessions it sees; the feed
// registers them and writes {"session","text"} back when one needs waking, and
// the plugin turns that into ctx.session.prompt (spike 5). It ends with stdin.
func OpencodeFeed(st *store.Store, in io.Reader, out io.Writer) error {
	pid := os.Getppid() // the opencode server: agentlink's MCP server is its child too
	var mu sync.Mutex
	sessions := map[string]store.Endpoint{} // handle -> endpoint
	done := make(chan error, 1)
	go func() {
		sc := bufio.NewScanner(in)
		for sc.Scan() {
			var m struct{ Session, Directory string }
			if json.Unmarshal(sc.Bytes(), &m) != nil || m.Session == "" {
				continue
			}
			mu.Lock()
			e, ok := sessionByID(sessions, m.Session)
			if !ok {
				e = store.Endpoint{Handle: store.Handle("opencode", m.Directory, m.Session), App: "opencode",
					Project: base(m.Directory), Session: m.Session, Tier: "push", Wake: fmt.Sprintf(`{"pid":%d}`, pid)}
				sessions[e.Handle] = e
			}
			mu.Unlock()
			st.Register(e) // also marks it as the most recently active opencode session
		}
		done <- sc.Err()
	}()
	enc := json.NewEncoder(out)
	for {
		select {
		case err := <-done:
			return err
		case <-time.After(500 * time.Millisecond):
		}
		mu.Lock()
		list := make([]store.Endpoint, 0, len(sessions))
		for _, e := range sessions {
			list = append(list, e)
		}
		mu.Unlock()
		for _, e := range list {
			if need, _ := st.NeedsWake(e.Handle); need {
				if err := enc.Encode(map[string]string{"session": e.Session, "text": Line(st, e, "")}); err != nil {
					return err
				}
				st.MarkWoken(e.Handle)
			}
		}
	}
}

func sessionByID(m map[string]store.Endpoint, id string) (store.Endpoint, bool) {
	for _, e := range m {
		if e.Session == id {
			return e, true
		}
	}
	return store.Endpoint{}, false
}

func base(dir string) string {
	dir = strings.TrimRight(dir, `/\`)
	return dir[strings.LastIndexAny(dir, `/\`)+1:]
}
