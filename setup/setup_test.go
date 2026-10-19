package setup

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tailscale/hujson"
)

// seeds is a fake home holding every config format agentlink edits, each with
// content of the user's own that must survive.
var seeds = map[string]string{
	".claude.json":          "{\n  \"numStartups\": 12,\n  \"mcpServers\": {\n    \"github\": {\n      \"command\": \"gh-mcp\"\n    }\n  },\n  \"projects\": {}\n}\n",
	".claude/settings.json": "{\n  \"permissions\": {\n    \"allow\": [\"Bash(git status)\"]\n  },\n  \"hooks\": {\n    \"Stop\": [{\"hooks\": [{\"type\": \"command\", \"command\": \"loom.exe --hook stop\"}]}]\n  }\n}\n",
	"AppData/Roaming/Claude/claude_desktop_config.json":                      "{\n  \"coworkUserFilesPath\": \"C:\\\\Users\\\\me\\\\Claude\",\n  \"preferences\": {\n    \"sidebarMode\": \"chat\"\n  }\n}\n",
	"AppData/Local/Packages/Claude_pzs8sxrjxfjjc/LocalCache/Roaming/Claude/": "",
	".codex/config.toml":                    "model = \"gpt-6.1-sol\"\r\n\r\n[projects.'c:\\users\\me']\r\ntrust_level = \"trusted\"\r\n\r\n[mcp_servers.node_repl]\r\ncommand = 'C:\\node_repl.exe'\r\n\r\n[mcp_servers.node_repl.env]\r\nX = \"1\"\r\n\r\n[desktop]\r\ntheme = \"light\"\r\n",
	".cursor/hooks.json":                    "{\n  \"version\": 1,\n  \"hooks\": {\n    \"beforeSubmitPrompt\": [{\"command\": \"./track.sh\"}],\n    \"stop\": [{\"command\": \"./user-stop.sh\"}]\n  }\n}\n",
	".cursor/permissions.json":              "{\n  // the user's own list\n  \"mcpAllowlist\": [\"github:*\"]\n}\n",
	"AppData/Roaming/Code/User/mcp.json":    "// VS Code MCP servers\n{\n  \"servers\": {\n    \"playwright\": {\n      \"type\": \"stdio\", // keep me\n      \"command\": \"npx\",\n    },\n  },\n}\n",
	"AppData/Roaming/devin/mcp_config.json": "{}\n",
	".config/opencode/opencode.jsonc":       "{\n  // opencode settings\n  \"$schema\": \"https://opencode.ai/config.json\",\n  \"permission\": {\"edit\": \"ask\"}, /* trailing */\n}\n",
	".gemini/settings.json":                 "{\"theme\": \"GitHub\"}",
}

func fakeHome(t *testing.T) (Env, string) {
	home := t.TempDir()
	for rel, content := range seeds {
		p := filepath.Join(home, filepath.FromSlash(rel))
		if strings.HasSuffix(rel, "/") {
			os.MkdirAll(p, 0o755)
			continue
		}
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	e := Env{OS: "windows", Home: home, AppData: filepath.Join(home, "AppData", "Roaming"), LocalAppData: filepath.Join(home, "AppData", "Local"),
		CodexHome: filepath.Join(home, ".codex"), AgentHome: filepath.Join(home, ".agentlink")}
	return e, home
}

func read(t *testing.T, home, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(home, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func decode(t *testing.T, s string) map[string]any {
	t.Helper()
	v, err := hujson.Parse([]byte(s))
	if err != nil {
		t.Fatalf("%v in:\n%s", err, s)
	}
	v.Standardize()
	var m map[string]any
	json.Unmarshal(v.Pack(), &m)
	return m
}

func get(m any, path ...string) any {
	for _, p := range path {
		mm, _ := m.(map[string]any)
		m = mm[p]
	}
	return m
}

func backups(t *testing.T, home string) int {
	n := 0
	filepath.Walk(home, func(p string, _ os.FileInfo, _ error) error {
		if strings.Contains(p, ".agentlink-backup-") {
			n++
		}
		return nil
	})
	return n
}

func setup(t *testing.T, e Env, exe string) []Change {
	t.Helper()
	ch, _, err := Plan(e, exe, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := Apply(e, ch, time.Now()); err != nil {
		t.Fatal(err)
	}
	return ch
}

func TestSetupAndUninstall(t *testing.T) {
	e, home := fakeHome(t)
	exe := `C:\Users\me\.agentlink\bin\agentlink.exe`

	ch, found, err := Plan(e, exe, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != len(Apps) {
		t.Fatalf("detected %d apps, want all %d", len(found), len(Apps))
	}
	if err := Apply(e, ch, time.Now()); err != nil {
		t.Fatal(err)
	}

	cc := decode(t, read(t, home, ".claude.json"))
	if get(cc, "mcpServers", "agentlink", "command") != exe || get(cc, "mcpServers", "github", "command") != "gh-mcp" || get(cc, "numStartups") != 12.0 {
		t.Errorf(".claude.json: %v", cc)
	}
	if s := read(t, home, ".claude/settings.json"); !strings.Contains(s, `"mcp__agentlink"`) || !strings.Contains(s, `"Bash(git status)"`) || !strings.Contains(s, "loom.exe --hook stop") ||
		!strings.HasSuffix(get(get(decode(t, s), "hooks", "UserPromptSubmit").([]any)[0], "hooks").([]any)[0].(map[string]any)["command"].(string), " hook claude-code prompt") {
		t.Errorf("settings.json:\n%s", s)
	}
	for _, rel := range []string{"AppData/Roaming/Claude/claude_desktop_config.json", "AppData/Local/Packages/Claude_pzs8sxrjxfjjc/LocalCache/Roaming/Claude/claude_desktop_config.json"} {
		if got := get(decode(t, read(t, home, rel)), "mcpServers", "agentlink", "args"); len(got.([]any)) != 3 {
			t.Errorf("%s args = %v", rel, got)
		}
	}
	toml := read(t, home, ".codex/config.toml")
	if !strings.HasPrefix(toml, seeds[".codex/config.toml"]) || !strings.Contains(toml, "\r\n[mcp_servers.agentlink]\r\ncommand = '"+exe+"'\r\nargs = [\"mcp\", \"--host\", \"codex\"]\r\n") {
		t.Errorf("config.toml:\n%q", toml)
	}
	hooks := decode(t, read(t, home, ".cursor/hooks.json"))
	stop := get(hooks, "hooks", "stop").([]any)
	if len(stop) != 2 || get(stop[0], "command") != "./user-stop.sh" || !strings.HasSuffix(get(stop[1], "command").(string), "agentlink.exe hook cursor stop") {
		t.Errorf("cursor hooks: %v", stop)
	}
	if s := read(t, home, ".cursor/permissions.json"); !strings.Contains(s, "// the user's own list") || !strings.Contains(s, `"agentlink:*"`) {
		t.Errorf("permissions.json:\n%s", s)
	}
	if s := read(t, home, "AppData/Roaming/Code/User/mcp.json"); !strings.Contains(s, "// keep me") || !strings.Contains(s, "// VS Code MCP servers") ||
		get(decode(t, s), "servers", "agentlink", "type") != "stdio" || get(decode(t, s), "servers", "playwright", "command") != "npx" {
		t.Errorf("vscode mcp.json:\n%s", s)
	}
	if s := read(t, home, ".config/opencode/opencode.jsonc"); !strings.Contains(s, "// opencode settings") || !strings.Contains(s, "/* trailing */") ||
		get(decode(t, s), "mcp", "agentlink", "type") != "local" {
		t.Errorf("opencode.jsonc:\n%s", s)
	}
	if _, err := os.Stat(filepath.Join(home, ".config/opencode/opencode.json")); err == nil {
		t.Error("created opencode.json next to the existing opencode.jsonc")
	}
	if js := read(t, home, ".config/opencode/plugins/agentlink.js"); !strings.Contains(js, `const EXE = "C:\\Users\\me\\.agentlink\\bin\\agentlink.exe";`) {
		t.Errorf("opencode plugin: %s", js[:400])
	}
	if g := decode(t, read(t, home, ".gemini/settings.json")); !strings.HasSuffix(get(get(g, "hooks", "AfterAgent").([]any)[0], "hooks").([]any)[0].(map[string]any)["command"].(string), " hook gemini after-agent") {
		t.Errorf("gemini hook: %v", g)
	}
	if s := read(t, home, ".copilot/hooks/agentlink.json"); !strings.Contains(s, `hook vscode stop`) || get(decode(t, s), "hooks", "Stop") == nil {
		t.Errorf("vscode hook file: %s", s)
	}
	if get(decode(t, read(t, home, ".gemini/settings.json")), "mcpServers", "agentlink", "trust") != true {
		t.Error("gemini entry lacks trust")
	}
	if get(decode(t, read(t, home, "AppData/Roaming/devin/config.json")), "permissions", "allow").([]any)[0] != "mcp__agentlink__*" {
		t.Error("devin permissions")
	}

	// Re-running changes nothing and makes no new backups.
	nb := backups(t, home)
	if again, _, _ := Plan(e, exe, nil); len(again) != 0 {
		for _, c := range again {
			t.Errorf("second run would change %s: %s", c.Path, c.What)
		}
	}
	setup(t, e, exe)
	if backups(t, home) != nb {
		t.Error("second run made backups")
	}

	// A moved executable updates the entries in place instead of adding new ones.
	moved := `D:\tools\agentlink.exe`
	setup(t, e, moved)
	stop = get(decode(t, read(t, home, ".cursor/hooks.json")), "hooks", "stop").([]any)
	if len(stop) != 2 || get(stop[1], "command") != "D:/tools/agentlink.exe hook cursor stop" { // forward slashes for Git Bash
		t.Errorf("hooks after move: %v", stop)
	}
	if toml := read(t, home, ".codex/config.toml"); strings.Count(toml, "[mcp_servers.agentlink]") != 1 || !strings.Contains(toml, moved) {
		t.Errorf("toml after move:\n%s", toml)
	}

	// Uninstall gives back every original byte for byte and deletes the files it created.
	done, err := Uninstall(e)
	if err != nil {
		t.Fatal(err)
	}
	for rel, want := range seeds {
		if strings.HasSuffix(rel, "/") {
			continue
		}
		if got := read(t, home, rel); got != want {
			t.Errorf("%s not restored:\n got %q\nwant %q", rel, got, want)
		}
	}
	for _, rel := range []string{"AppData/Local/Packages/Claude_pzs8sxrjxfjjc/LocalCache/Roaming/Claude/claude_desktop_config.json", ".cursor/mcp.json", "AppData/Roaming/devin/config.json", ".config/opencode/plugins/agentlink.js", ".copilot/hooks/agentlink.json"} {
		if _, err := os.Stat(filepath.Join(home, filepath.FromSlash(rel))); err == nil {
			t.Errorf("uninstall left %s, which agentlink created", rel)
		}
	}
	if m, _ := readManifest(e); len(m) != 0 {
		t.Errorf("manifest still lists %d files", len(m))
	}
	if len(done) == 0 {
		t.Error("uninstall reported nothing")
	}
}

// When an app rewrites its config after setup, uninstall takes out only agentlink's entry.
func TestUninstallAfterAppRewrote(t *testing.T) {
	e, home := fakeHome(t)
	setup(t, e, `C:\a\agentlink.exe`)
	p := filepath.Join(home, "AppData/Roaming/Claude/claude_desktop_config.json")
	m := decode(t, read(t, home, "AppData/Roaming/Claude/claude_desktop_config.json"))
	m["preferences"].(map[string]any)["sideChats"] = false // what Claude desktop did in spike 6
	b, _ := json.MarshalIndent(m, "", "  ")
	os.WriteFile(p, b, 0o644)
	toml := filepath.Join(home, ".codex/config.toml")
	cur, _ := os.ReadFile(toml)
	os.WriteFile(toml, append(cur, []byte("\r\n[projects.'d:\\new']\r\ntrust_level = \"trusted\"\r\n")...), 0o644)

	if _, err := Uninstall(e); err != nil {
		t.Fatal(err)
	}
	after := decode(t, read(t, home, "AppData/Roaming/Claude/claude_desktop_config.json"))
	if get(after, "mcpServers", "agentlink") != nil || get(after, "preferences", "sideChats") != false {
		t.Errorf("claude desktop config after uninstall: %v", after)
	}
	if s := read(t, home, ".codex/config.toml"); strings.Contains(s, "agentlink") || !strings.Contains(s, `[projects.'d:\new']`) || !strings.Contains(s, "[mcp_servers.node_repl.env]") {
		t.Errorf("config.toml after uninstall:\n%s", s)
	}
}

func TestOnlyAndUndetected(t *testing.T) {
	e, home := fakeHome(t)
	os.RemoveAll(filepath.Join(home, ".gemini"))
	ch, found, _ := Plan(e, `C:\a\agentlink.exe`, []string{"codex", "gemini"})
	if len(found) != 1 || found[0].Host != "codex" || len(ch) != 1 {
		t.Fatalf("--only codex,gemini without gemini: found %v, %d changes", found, len(ch))
	}
}

func TestDoctor(t *testing.T) {
	e, home := fakeHome(t)
	exe := filepath.Join(t.TempDir(), ExeName())
	if out, err := exec.Command("go", "build", "-o", exe, "agentlink/cmd/agentlink").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	var buf bytes.Buffer
	if Doctor(e, exe, &buf) {
		t.Fatalf("doctor passed before setup:\n%s", buf.String())
	}
	if !strings.Contains(buf.String(), "FIX   Cursor") {
		t.Errorf("doctor before setup:\n%s", buf.String())
	}
	setup(t, e, exe)
	buf.Reset()
	if !Doctor(e, exe, &buf) {
		t.Fatalf("doctor failed after setup:\n%s", buf.String())
	}
	if !strings.Contains(buf.String(), "ok    round trip") || !strings.Contains(buf.String(), "ok    Claude desktop") {
		t.Errorf("doctor after setup:\n%s", buf.String())
	}
	t.Logf("doctor after setup:\n%s", strings.ReplaceAll(buf.String(), home, "~"))
}
