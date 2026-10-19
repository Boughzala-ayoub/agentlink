// Package setup wires agentlink into each installed app's config files and
// takes it out again. It edits files directly, keeping comments and formatting,
// backs each one up first, and never restarts an app.
package setup

import (
	"bytes"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/tailscale/hujson"
)

// Env holds every location setup reads, so tests can point it at a fake home.
type Env struct {
	OS           string // runtime.GOOS
	Home         string
	AppData      string // Windows %APPDATA%
	LocalAppData string // Windows %LOCALAPPDATA%
	CodexHome    string
	AgentHome    string // ~/.agentlink
}

func RealEnv() Env {
	home, _ := os.UserHomeDir()
	e := Env{OS: runtime.GOOS, Home: home, AppData: os.Getenv("APPDATA"), LocalAppData: os.Getenv("LOCALAPPDATA"), CodexHome: os.Getenv("CODEX_HOME")}
	if e.CodexHome == "" {
		e.CodexHome = filepath.Join(home, ".codex")
	}
	if e.AgentHome = os.Getenv("AGENTLINK_HOME"); e.AgentHome == "" {
		e.AgentHome = filepath.Join(home, ".agentlink")
	}
	return e
}

// configDir is where desktop apps keep their settings on each OS.
func (e Env) configDir(name string) string {
	switch e.OS {
	case "windows":
		return filepath.Join(e.AppData, name)
	case "darwin":
		return filepath.Join(e.Home, "Library", "Application Support", name)
	}
	return filepath.Join(e.Home, ".config", name)
}

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

// edit is one reversible change to one file.
type edit struct {
	Path   string
	What   string
	Apply  func(src []byte) ([]byte, error) // src is nil when the file is missing
	Remove func(src []byte) ([]byte, error)
}

// App is one row of the installer table.
type App struct {
	Host    string
	Name    string
	Restart string
	Detect  func(Env) bool
	Edits   func(e Env, exe string) []edit
}

func mcpArgs(host string) []any { return []any{"mcp", "--host", host} }

func stdioEntry(exe, host string) map[string]any {
	return map[string]any{"command": exe, "args": mcpArgs(host)}
}

// Apps is the installer table. Paths come from PLAN.md section 3.2.
var Apps = []App{
	{
		Host: "claude-code", Name: "Claude Code",
		Restart: "Claude Code: new sessions pick agentlink up. Restart running ones to use it there.",
		Detect: func(e Env) bool {
			return exists(filepath.Join(e.Home, ".claude.json")) || exists(filepath.Join(e.Home, ".claude"))
		},
		Edits: func(e Env, exe string) []edit {
			entry := stdioEntry(exe, "claude-code")
			entry["type"] = "stdio"
			settings := filepath.Join(e.Home, ".claude", "settings.json")
			// It only tells the loop guard that a person typed, and prints nothing (PLAN.md 5.8).
			group := map[string]any{"hooks": []any{map[string]any{"type": "command", "command": hookCommand(exe, "claude-code", "prompt"), "timeout": 10}}}
			return []edit{
				jsonKey(filepath.Join(e.Home, ".claude.json"), "add the agentlink MCP server", "/mcpServers/agentlink", entry),
				jsonListItem(settings, "allow agentlink's own tools", "/permissions/allow", "mcp__agentlink", nil),
				jsonListItem(settings, "add agentlink's UserPromptSubmit hook", "/hooks/UserPromptSubmit", group, isHookGroup("claude-code", "prompt")),
			}
		},
	},
	{
		Host: "claude-desktop", Name: "Claude desktop",
		Restart: "Claude desktop: quit it completely (right-click its tray icon, Quit), then start it again.",
		Detect: func(e Env) bool {
			return len(claudeDesktopDirs(e)) > 0
		},
		Edits: func(e Env, exe string) []edit {
			var out []edit
			for _, d := range claudeDesktopDirs(e) {
				out = append(out, jsonKey(filepath.Join(d, "claude_desktop_config.json"), "add the agentlink MCP server", "/mcpServers/agentlink", stdioEntry(exe, "claude-desktop")))
			}
			return out
		},
	},
	{
		Host: "codex", Name: "Codex",
		Restart: "Codex desktop (in the ChatGPT app): restart it. New Codex CLI sessions pick agentlink up.",
		Detect:  func(e Env) bool { return exists(e.CodexHome) },
		Edits: func(e Env, exe string) []edit {
			return []edit{tomlTable(filepath.Join(e.CodexHome, "config.toml"), "add the agentlink MCP server", "mcp_servers.agentlink",
				"command = "+tomlString(exe)+"\nargs = [\"mcp\", \"--host\", \"codex\"]\n")}
		},
	},
	{
		Host: "cursor", Name: "Cursor",
		Restart: "Cursor: restart it.",
		Detect:  func(e Env) bool { return exists(filepath.Join(e.Home, ".cursor")) },
		Edits: func(e Env, exe string) []edit {
			hooks := filepath.Join(e.Home, ".cursor", "hooks.json")
			out := []edit{
				jsonKey(filepath.Join(e.Home, ".cursor", "mcp.json"), "add the agentlink MCP server", "/mcpServers/agentlink", stdioEntry(exe, "cursor")),
				jsonDefault(hooks, "/version", 1), // Cursor requires it in a new hooks.json
				jsonListItem(hooks, "add agentlink's stop hook", "/hooks/stop",
					map[string]any{"command": hookCommand(exe, "cursor", "stop")}, isHook("cursor", "stop")),
			}
			// Creating permissions.json would replace Cursor's own allowlist, so only touch an existing one.
			if p := filepath.Join(e.Home, ".cursor", "permissions.json"); exists(p) {
				out = append(out, jsonListItem(p, "allow agentlink's own tools", "/mcpAllowlist", "agentlink:*", nil))
			}
			return out
		},
	},
	{
		Host: "vscode", Name: "VS Code",
		Restart: "VS Code: restart it, then allow agentlink the first time it sends.",
		Detect:  func(e Env) bool { return exists(filepath.Join(e.configDir("Code"), "User")) },
		Edits: func(e Env, exe string) []edit {
			entry := stdioEntry(exe, "vscode")
			entry["type"] = "stdio"
			hook, _ := json.MarshalIndent(map[string]any{"hooks": map[string]any{"Stop": []any{
				map[string]any{"type": "command", "command": hookCommand(exe, "vscode", "stop"), "timeout": 10}}}}, "", "  ")
			return []edit{
				jsonKey(filepath.Join(e.configDir("Code"), "User", "mcp.json"), "add the agentlink MCP server", "/servers/agentlink", entry),
				// ~/.copilot/hooks/*.json are VS Code's user hook files, so agentlink owns one outright.
				wholeFile(filepath.Join(e.Home, ".copilot", "hooks", "agentlink.json"), "add agentlink's stop hook", string(hook)+"\n"),
			}
		},
	},
	{
		Host: "devin", Name: "Devin Desktop",
		Restart: "Devin Desktop: quit it completely, then start it again.",
		Detect:  func(e Env) bool { return exists(devinDir(e)) },
		Edits: func(e Env, exe string) []edit {
			return []edit{
				jsonKey(filepath.Join(devinDir(e), "mcp_config.json"), "add the agentlink MCP server", "/mcpServers/agentlink", stdioEntry(exe, "devin")),
				jsonListItem(filepath.Join(devinDir(e), "config.json"), "allow agentlink's own tools", "/permissions/allow", "mcp__agentlink__*", nil),
			}
		},
	},
	{
		Host: "opencode", Name: "opencode",
		Restart: "opencode: new sessions pick agentlink up. Restart running ones to use it there.",
		Detect:  func(e Env) bool { return exists(filepath.Join(e.Home, ".config", "opencode")) },
		Edits: func(e Env, exe string) []edit {
			js, _ := json.Marshal(exe)
			return []edit{
				jsonKey(opencodeConfig(e), "add the agentlink MCP server", "/mcp/agentlink",
					map[string]any{"type": "local", "command": append([]any{exe}, mcpArgs("opencode")...), "enabled": true}),
				wholeFile(filepath.Join(e.Home, ".config", "opencode", "plugins", "agentlink.js"), "add agentlink's wake plugin",
					strings.Replace(opencodePlugin, `"__AGENTLINK_EXE__"`, string(js), 1)),
			}
		},
	},
	{
		Host: "gemini", Name: "Gemini CLI",
		Restart: "Gemini CLI: new sessions pick agentlink up, or run /mcp reload in a running one.",
		Detect:  func(e Env) bool { return exists(filepath.Join(e.Home, ".gemini")) },
		Edits: func(e Env, exe string) []edit {
			entry := stdioEntry(exe, "gemini")
			entry["trust"] = true
			settings := filepath.Join(e.Home, ".gemini", "settings.json")
			group := map[string]any{"matcher": "", "hooks": []any{map[string]any{"type": "command", "name": "agentlink",
				"command": hookCommand(exe, "gemini", "after-agent"), "timeout": 10000}}}
			return []edit{
				jsonKey(settings, "add the agentlink MCP server", "/mcpServers/agentlink", entry),
				jsonListItem(settings, "add agentlink's AfterAgent hook", "/hooks/AfterAgent", group, isHookGroup("gemini", "after-agent")),
			}
		},
	},
}

// hookCommand is the command line a hook entry runs. Forward slashes, because
// Claude Code runs hooks through Git Bash on Windows, which eats backslashes;
// cmd and PowerShell take either.
func hookCommand(exe, host, event string) string {
	q := strings.ReplaceAll(exe, `\`, "/")
	if strings.ContainsAny(q, " \t") {
		q = `"` + q + `"`
	}
	return q + " hook " + host + " " + event
}

// isHook matches agentlink's hook entry whatever executable path it names.
func isHook(host, event string) func(any) bool {
	return func(x any) bool {
		m, _ := x.(map[string]any)
		c, _ := m["command"].(string)
		return strings.Contains(strings.ToLower(c), "agentlink") && strings.HasSuffix(c, " hook "+host+" "+event)
	}
}

// isHookGroup matches a {"hooks": [agentlink's hook]} group, the shape Claude Code and Gemini use.
func isHookGroup(host, event string) func(any) bool {
	return func(x any) bool {
		m, _ := x.(map[string]any)
		hs, _ := m["hooks"].([]any)
		return len(hs) == 1 && isHook(host, event)(hs[0])
	}
}

func claudeDesktopDirs(e Env) []string {
	var out []string
	if d := e.configDir("Claude"); exists(d) {
		out = append(out, d)
	}
	if e.OS == "windows" { // some MSIX installs read the virtualized copy (anthropics/claude-code#26073)
		if d := filepath.Join(e.LocalAppData, "Packages", "Claude_pzs8sxrjxfjjc", "LocalCache", "Roaming", "Claude"); exists(d) {
			out = append(out, d)
		}
	}
	return out
}

func devinDir(e Env) string {
	if e.OS == "windows" {
		return filepath.Join(e.AppData, "devin")
	}
	return filepath.Join(e.Home, ".config", "devin")
}

func opencodeConfig(e Env) string {
	dir := filepath.Join(e.Home, ".config", "opencode")
	for _, n := range []string{"opencode.json", "opencode.jsonc"} {
		if exists(filepath.Join(dir, n)) {
			return filepath.Join(dir, n)
		}
	}
	return filepath.Join(dir, "opencode.json")
}

// --- JSON and JSONC edits through hujson, which keeps comments and layout.

func parse(src []byte) (hujson.Value, error) {
	if len(bytes.TrimSpace(src)) == 0 {
		src = []byte("{}")
	}
	return hujson.Parse(src)
}

func ptr(parts ...string) string {
	var b strings.Builder
	for _, p := range parts {
		b.WriteString("/" + strings.NewReplacer("~", "~0", "/", "~1").Replace(p))
	}
	return b.String()
}

func split(pointer string) []string { return strings.Split(strings.TrimPrefix(pointer, "/"), "/") }

// value decodes the JSON at pointer, or returns ok=false if there is none.
func value(v *hujson.Value, pointer string) (any, bool) {
	f := v.Find(pointer)
	if f == nil {
		return nil, false
	}
	c := f.Clone()
	c.Standardize()
	var out any
	if json.Unmarshal(c.Pack(), &out) != nil {
		return nil, false
	}
	return out, true
}

func patch(v *hujson.Value, op, pointer string, val any) error {
	p := map[string]any{"op": op, "path": pointer}
	if op != "remove" {
		p["value"] = val
	}
	b, _ := json.Marshal([]any{p})
	return v.Patch(b)
}

// ensure creates missing parent objects (or the final array) along pointer.
func ensure(v *hujson.Value, pointer string, leaf any) error {
	parts := split(pointer)
	for i := range parts {
		p := ptr(parts[:i+1]...)
		if v.Find(p) != nil {
			continue
		}
		var empty any = map[string]any{}
		if i == len(parts)-1 {
			empty = leaf
		}
		if err := patch(v, "add", p, empty); err != nil {
			return err
		}
	}
	return nil
}

func jsonKey(path, what, pointer string, want any) edit {
	return edit{Path: path, What: what,
		Apply: func(src []byte) ([]byte, error) {
			v, err := parse(src)
			if err != nil {
				return nil, err
			}
			if got, ok := value(&v, pointer); ok && reflect.DeepEqual(got, norm(want)) {
				return src, nil
			}
			parts := split(pointer)
			if err := ensure(&v, ptr(parts[:len(parts)-1]...), map[string]any{}); err != nil {
				return nil, err
			}
			op := "add"
			if v.Find(pointer) != nil {
				op = "replace"
			}
			if err := patch(&v, op, pointer, want); err != nil {
				return nil, err
			}
			return v.Pack(), nil
		},
		Remove: func(src []byte) ([]byte, error) {
			v, err := parse(src)
			if err != nil {
				return nil, err
			}
			if v.Find(pointer) == nil {
				return src, nil
			}
			if err := patch(&v, "remove", pointer, nil); err != nil {
				return nil, err
			}
			return v.Pack(), nil
		},
	}
}

// jsonListItem keeps item in the array at pointer, creating the array if needed.
// same recognises agentlink's item even when it differs (a moved executable);
// nil means an exact match.
func jsonListItem(path, what, pointer string, item any, same func(any) bool) edit {
	want := norm(item)
	if same == nil {
		same = func(x any) bool { return reflect.DeepEqual(x, want) }
	}
	find := func(v *hujson.Value) (int, bool) { // index of agentlink's item, and whether it is already right
		arr, _ := value(v, pointer)
		list, _ := arr.([]any)
		for i, x := range list {
			if same(x) {
				return i, reflect.DeepEqual(x, want)
			}
		}
		return -1, false
	}
	return edit{Path: path, What: what,
		Apply: func(src []byte) ([]byte, error) {
			v, err := parse(src)
			if err != nil {
				return nil, err
			}
			i, right := find(&v)
			if right {
				return src, nil
			}
			if i >= 0 {
				err = patch(&v, "replace", fmt.Sprintf("%s/%d", pointer, i), item)
			} else if err = ensure(&v, pointer, []any{}); err == nil {
				err = patch(&v, "add", pointer+"/-", item)
			}
			if err != nil {
				return nil, err
			}
			return v.Pack(), nil
		},
		Remove: func(src []byte) ([]byte, error) {
			v, err := parse(src)
			if err != nil {
				return nil, err
			}
			i, _ := find(&v)
			if i < 0 {
				return src, nil
			}
			if err := patch(&v, "remove", fmt.Sprintf("%s/%d", pointer, i), nil); err != nil {
				return nil, err
			}
			return v.Pack(), nil
		},
	}
}

// jsonDefault sets pointer only when it is missing, and never removes it.
func jsonDefault(path, pointer string, val any) edit {
	return edit{Path: path,
		Apply: func(src []byte) ([]byte, error) {
			v, err := parse(src)
			if err != nil || v.Find(pointer) != nil {
				return src, err
			}
			if err := patch(&v, "add", pointer, val); err != nil {
				return nil, err
			}
			return v.Pack(), nil
		},
		Remove: func(src []byte) ([]byte, error) { return src, nil },
	}
}

func norm(x any) any {
	b, _ := json.Marshal(x)
	var o any
	json.Unmarshal(b, &o)
	return o
}

//go:embed opencode-plugin.js
var opencodePlugin string

// wholeFile owns a file outright, such as the opencode plugin. Removing it deletes the file.
func wholeFile(path, what, content string) edit {
	return edit{Path: path, What: what,
		Apply:  func([]byte) ([]byte, error) { return []byte(content), nil },
		Remove: func([]byte) ([]byte, error) { return nil, nil }, // nil: delete
	}
}

// --- TOML: Codex's config gets one table appended, and loses exactly that table again.

func tomlString(s string) string {
	if !strings.ContainsAny(s, "'\n") {
		return "'" + s + "'"
	}
	b, _ := json.Marshal(s) // a JSON string is a valid TOML basic string
	return string(b)
}

func tomlTable(path, what, name, body string) edit {
	header := regexp.MustCompile(`(?m)^[ \t]*\[[ \t]*` + regexp.QuoteMeta(name) + `[ \t]*\][ \t]*\r?$`)
	anyHeader := regexp.MustCompile(`(?m)^[ \t]*\[`)
	// span finds the table: its header line up to the next header that isn't one of its sub-tables.
	span := func(s string) (int, int) {
		loc := header.FindStringIndex(s)
		if loc == nil {
			return -1, -1
		}
		end := len(s)
		for _, h := range anyHeader.FindAllStringIndex(s[loc[1]:], -1) {
			rest := s[loc[1]+h[0]:]
			if !strings.HasPrefix(strings.TrimLeft(rest, " \t["), name+".") {
				end = loc[1] + h[0]
				break
			}
		}
		return loc[0], end
	}
	return edit{Path: path, What: what,
		Apply: func(src []byte) ([]byte, error) {
			s := string(src)
			nl := "\n"
			if strings.Contains(s, "\r\n") {
				nl = "\r\n"
			}
			block := "[" + name + "]" + nl + strings.ReplaceAll(body, "\n", nl)
			if a, b := span(s); a >= 0 {
				if strings.TrimRight(s[a:b], "\r\n") == strings.TrimRight(block, "\r\n") {
					return src, nil
				}
				return []byte(s[:a] + block + nl + s[b:]), nil
			}
			if s != "" && !strings.HasSuffix(s, "\n") {
				s += nl
			}
			if s != "" {
				s += nl
			}
			return []byte(s + block), nil
		},
		Remove: func(src []byte) ([]byte, error) {
			s := string(src)
			a, b := span(s)
			if a < 0 {
				return src, nil
			}
			pre := strings.TrimRight(s[:a], "\r\n")
			if pre != "" && b < len(s) {
				pre += "\n\n"
			} else if pre != "" {
				pre += "\n"
			}
			return []byte(pre + s[b:]), nil
		},
	}
}

// --- planning, writing, the manifest.

// Change is one file setup would write.
type Change struct {
	App, Name, Path, What string
	Old, New              []byte
	Existed               bool
	edit                  edit
}

// Plan lists the writes setup would make for the installed apps (or only those in only).
func Plan(e Env, exe string, only []string) ([]Change, []App, error) {
	var out []Change
	var found []App
	for _, a := range Apps {
		if len(only) > 0 && !contains(only, a.Host) {
			continue
		}
		if !a.Detect(e) {
			continue
		}
		found = append(found, a)
		// Each edit sees the result of the previous edits to the same file.
		pending := map[string]*Change{}
		for _, ed := range a.Edits(e, exe) {
			c := pending[ed.Path]
			if c == nil {
				old, err := os.ReadFile(ed.Path)
				if err != nil && !errors.Is(err, fs.ErrNotExist) {
					return nil, nil, err
				}
				c = &Change{App: a.Host, Name: a.Name, Path: ed.Path, Old: old, New: old, Existed: err == nil, edit: ed}
				pending[ed.Path] = c
			}
			nw, err := ed.Apply(c.New)
			if err != nil {
				return nil, nil, fmt.Errorf("%s: %s: %w", a.Name, ed.Path, err)
			}
			if !bytes.Equal(nw, c.New) {
				if c.What != "" && ed.What != "" {
					c.What += ", "
				}
				c.What += ed.What
				c.New = nw
			}
		}
		var paths []string
		for p := range pending {
			paths = append(paths, p)
		}
		sort.Strings(paths)
		for _, p := range paths {
			if c := pending[p]; !bytes.Equal(c.Old, c.New) {
				out = append(out, *c)
			}
		}
	}
	return out, found, nil
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

type record struct {
	App     string `json:"app"`
	Path    string `json:"path"`
	Created bool   `json:"created"`          // the file didn't exist before agentlink
	Backup  string `json:"backup,omitempty"` // a copy from before agentlink touched the file
	SHA     string `json:"sha256"`           // the content agentlink last wrote
}

func manifestPath(e Env) string { return filepath.Join(e.AgentHome, "installed.json") }

func readManifest(e Env) (map[string]*record, error) {
	m := map[string]*record{}
	b, err := os.ReadFile(manifestPath(e))
	if errors.Is(err, fs.ErrNotExist) {
		return m, nil
	}
	if err != nil {
		return nil, err
	}
	var list []*record
	if err := json.Unmarshal(b, &list); err != nil {
		return nil, fmt.Errorf("%s: %w", manifestPath(e), err)
	}
	for _, r := range list {
		m[r.Path] = r
	}
	return m, nil
}

func writeManifest(e Env, m map[string]*record) error {
	var list []*record
	for _, r := range m {
		list = append(list, r)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Path < list[j].Path })
	b, _ := json.MarshalIndent(list, "", "  ")
	if err := os.MkdirAll(e.AgentHome, 0o700); err != nil {
		return err
	}
	return writeFile(manifestPath(e), append(b, '\n'))
}

func sum(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }

// writeFile replaces path atomically, keeping an existing file's mode.
func writeFile(path string, b []byte) error {
	mode := fs.FileMode(0o644)
	if st, err := os.Stat(path); err == nil {
		mode = st.Mode().Perm()
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".agentlink-tmp"
	if err := os.WriteFile(tmp, b, mode); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// writeBackup saves b next to path under a new name; it never overwrites an older backup.
func writeBackup(path string, b []byte, now time.Time) (string, error) {
	base := path + ".agentlink-backup-" + now.Format("20060102-150405")
	for i := 1; ; i++ {
		name := base
		if i > 1 {
			name = fmt.Sprintf("%s-%d", base, i)
		}
		f, err := os.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		if err != nil {
			return "", err
		}
		_, err = f.Write(b)
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		return name, err
	}
}

// Apply writes the planned changes, backing up each existing file first.
func Apply(e Env, changes []Change, now time.Time) error {
	m, err := readManifest(e)
	if err != nil {
		return err
	}
	for _, c := range changes {
		backup := ""
		if c.Existed {
			var err error
			if backup, err = writeBackup(c.Path, c.Old, now); err != nil {
				return fmt.Errorf("back up %s: %w", c.Path, err)
			}
		}
		r := m[c.Path]
		switch {
		case r == nil: // first touch: this backup is the file as it was before agentlink
			r = &record{App: c.App, Path: c.Path, Created: !c.Existed, Backup: backup}
		case r.SHA != sum(c.Old): // someone changed it since our last write: no exact restore any more
			r.Created, r.Backup = false, ""
		}
		if err := writeFile(c.Path, c.New); err != nil {
			return fmt.Errorf("write %s: %w", c.Path, err)
		}
		r.SHA = sum(c.New)
		m[c.Path] = r
		if err := writeManifest(e, m); err != nil {
			return err
		}
	}
	return nil
}

// Uninstall takes agentlink out of every file it changed. A file nobody touched
// since setup goes back to its backup byte for byte; otherwise only agentlink's
// entries come out. Backups stay where they are.
func Uninstall(e Env) ([]string, error) {
	m, err := readManifest(e)
	if err != nil {
		return nil, err
	}
	removers := map[string][]edit{}
	for _, a := range Apps {
		for _, ed := range a.Edits(e, "") {
			removers[ed.Path] = append(removers[ed.Path], ed)
		}
	}
	paths := map[string]bool{}
	for p := range m {
		paths[p] = true
	}
	for p := range removers { // files agentlink may have touched without a manifest entry
		if exists(p) {
			paths[p] = true
		}
	}
	var done []string
	var sorted []string
	for p := range paths {
		sorted = append(sorted, p)
	}
	sort.Strings(sorted)
	for _, p := range sorted {
		cur, err := os.ReadFile(p)
		if errors.Is(err, fs.ErrNotExist) {
			delete(m, p)
			continue
		}
		if err != nil {
			return done, err
		}
		r := m[p]
		var out []byte
		switch {
		case r != nil && sum(cur) == r.SHA && r.Created:
			if err := os.Remove(p); err != nil {
				return done, err
			}
			done = append(done, p+" (removed, agentlink created it)")
			delete(m, p)
			writeManifest(e, m)
			continue
		case r != nil && sum(cur) == r.SHA && r.Backup != "":
			if out, err = os.ReadFile(r.Backup); err != nil {
				return done, fmt.Errorf("restore %s from %s: %w", p, r.Backup, err)
			}
		default:
			out = cur
			for _, ed := range removers[p] {
				if out, err = ed.Remove(out); err != nil {
					return done, fmt.Errorf("%s: %w", p, err)
				}
			}
			if out == nil { // a file agentlink owns outright
				if err := os.Remove(p); err != nil {
					return done, err
				}
				done = append(done, p+" (removed)")
				delete(m, p)
				writeManifest(e, m)
				continue
			}
		}
		if !bytes.Equal(out, cur) {
			if err := writeFile(p, out); err != nil {
				return done, err
			}
			done = append(done, p)
		}
		delete(m, p)
		if err := writeManifest(e, m); err != nil {
			return done, err
		}
	}
	return done, nil
}
