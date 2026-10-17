package wake

import (
	"bytes"
	"encoding/json"
	"io"

	"agentlink/store"
)

// Hook runs one turn-end hook. The turn tier keeps one mailbox per app, the
// way Claude desktop does: when it has unread messages, the hook answers with
// that app's way of starting another turn, carrying only the wake line.
// It must finish fast and print nothing otherwise, since apps wait on it.
func Hook(st *store.Store, host, event string, in io.Reader, out io.Writer) error {
	raw, _ := io.ReadAll(in)
	raw = bytes.TrimPrefix(raw, []byte("\xef\xbb\xbf")) // Cursor writes a UTF-8 BOM (spike 7)
	var input struct {
		Status    string `json:"status"`
		LoopCount int    `json:"loop_count"`
	}
	json.Unmarshal(raw, &input)
	me := store.Endpoint{Handle: host, App: host, Tier: "turn"}
	if err := st.Register(me); err != nil {
		return err
	}
	var reply func(line string) any
	switch host + " " + event {
	case "cursor stop":
		if input.Status != "" && input.Status != "completed" || input.LoopCount >= 5 { // the user stopped it, or Cursor's loop_limit
			return nil
		}
		reply = func(line string) any { return map[string]string{"followup_message": line} }
	case "vscode stop": // VS Code's Stop hook, which Copilot treats as "keep going with this"
		reply = func(line string) any {
			return map[string]any{"hookSpecificOutput": map[string]string{"hookEventName": "Stop", "decision": "block", "reason": line}}
		}
	case "gemini after-agent": // Gemini sends a denied response's reason back as a new prompt
		reply = func(line string) any { return map[string]string{"decision": "deny", "reason": line} }
	default:
		return nil
	}
	need, err := st.NeedsWake(me.Handle)
	if err != nil || !need {
		return err
	}
	if err := json.NewEncoder(out).Encode(reply(Line(st, me, ""))); err != nil {
		return err
	}
	return st.MarkWoken(me.Handle)
}
