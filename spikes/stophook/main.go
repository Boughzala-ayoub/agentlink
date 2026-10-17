// stophook is a throwaway hook for spike 7. It logs its argv and stdin to
// stophook.log next to itself. If followup.txt exists there, it prints
// {"followup_message": <contents>} and deletes the file, so it fires once.
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
	f, _ := os.OpenFile(filepath.Join(dir, "stophook.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	fmt.Fprintf(f, "%s args=%q stdin=%s\n", time.Now().Format("15:04:05"), os.Args[1:], in)
	p := filepath.Join(dir, "followup.txt")
	if b, err := os.ReadFile(p); err == nil {
		os.Remove(p)
		out, _ := json.Marshal(map[string]string{"followup_message": string(b)})
		os.Stdout.Write(out)
		fmt.Fprintf(f, "  -> %s\n", out)
	}
}
