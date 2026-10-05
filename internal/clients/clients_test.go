package clients

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestJSONSetKeepsOrderAndOtherEntries(t *testing.T) {
	f := filepath.Join(t.TempDir(), "opencode.json")
	orig := `{
  "$schema": "https://opencode.ai/config.json",
  "mcp": {
    "context7": { "type": "http", "url": "https://mcp.context7.com/mcp" }
  },
  "model": "x"
}`
	if err := os.WriteFile(f, []byte(orig), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := jsonSet(f, "mcp", ServerName, map[string]any{"type": "local", "command": []string{"/bin/s", "mcp"}}); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(f)
	s := string(b)
	if i, j, k := strings.Index(s, `"$schema"`), strings.Index(s, `"mcp"`), strings.Index(s, `"model"`); !(i < j && j < k) {
		t.Errorf("top-level order changed:\n%s", s)
	}
	if !strings.Contains(s, `"context7"`) || strings.Index(s, `"context7"`) > strings.Index(s, `"sonarqube"`) {
		t.Errorf("existing server lost or reordered:\n%s", s)
	}
	if fi, _ := os.Stat(f); runtime.GOOS != "windows" && fi.Mode().Perm() != 0o600 { // no unix modes on Windows
		t.Errorf("mode changed to %v", fi.Mode().Perm())
	}
	if !jsonHas(f, "mcp", ServerName) {
		t.Error("jsonHas false after set")
	}

	// Re-register replaces in place, not appends.
	_ = jsonSet(f, "mcp", ServerName, map[string]any{"type": "local"})
	b, _ = os.ReadFile(f)
	if strings.Count(string(b), `"sonarqube"`) != 1 {
		t.Errorf("duplicate entry:\n%s", b)
	}

	if err := jsonDelete(f, "mcp", ServerName); err != nil {
		t.Fatal(err)
	}
	if jsonHas(f, "mcp", ServerName) || !jsonHas(f, "mcp", "context7") {
		t.Error("delete removed the wrong entries")
	}
}

func TestJSONSetCreatesFile(t *testing.T) {
	f := filepath.Join(t.TempDir(), "nested", "mcp.json")
	if err := jsonSet(f, "mcpServers", ServerName, map[string]any{"command": "x"}); err != nil {
		t.Fatal(err)
	}
	if !jsonHas(f, "mcpServers", ServerName) {
		t.Error("not created")
	}
}

func TestJSONCRefused(t *testing.T) {
	f := filepath.Join(t.TempDir(), "mcp.json")
	_ = os.WriteFile(f, []byte("// comment\n{}"), 0o644)
	if err := jsonSet(f, "servers", ServerName, map[string]any{}); err == nil {
		t.Error("edited a file with comments")
	}
}

func TestTOML(t *testing.T) {
	f := filepath.Join(t.TempDir(), "config.toml")
	orig := "model = \"o3\"\n\n[mcp_servers.other]\ncommand = \"x\"\n"
	_ = os.WriteFile(f, []byte(orig), 0o644)
	h := "[mcp_servers.sonarqube]"

	if err := tomlReplace(f, h, h+"\ncommand = \"/a\"\nargs = [\"mcp\"]\n"); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(f)
	if !strings.HasPrefix(string(b), orig) || !strings.Contains(string(b), `command = "/a"`) {
		t.Errorf("append:\n%s", b)
	}

	// Replace in the middle keeps the following table.
	_ = os.WriteFile(f, []byte("[mcp_servers.sonarqube]\ncommand = \"/old\"\n\n[mcp_servers.other]\ncommand = \"x\"\n"), 0o644)
	_ = tomlReplace(f, h, h+"\ncommand = \"/new\"\n")
	b, _ = os.ReadFile(f)
	s := string(b)
	if strings.Contains(s, "/old") || !strings.Contains(s, "/new") || !strings.Contains(s, "[mcp_servers.other]") {
		t.Errorf("replace:\n%s", s)
	}

	_ = tomlReplace(f, h, "")
	b, _ = os.ReadFile(f)
	if strings.Contains(string(b), "sonarqube") || !strings.Contains(string(b), "[mcp_servers.other]") {
		t.Errorf("remove:\n%s", b)
	}
}

func TestDetectionNeedsExecutable(t *testing.T) {
	home := t.TempDir()
	_ = os.MkdirAll(filepath.Join(home, ".cursor"), 0o755) // config dir alone
	env := Env{Home: home, GOOS: "linux", Look: func(string) (string, error) { return "", os.ErrNotExist }}
	for _, c := range All() {
		if c.ID != "cursor" {
			continue
		}
		if c.Detected(env) {
			t.Error("cursor detected from a config dir alone")
		}
	}
	env.Look = func(n string) (string, error) {
		if n == "cursor" {
			return "/usr/bin/cursor", nil
		}
		return "", os.ErrNotExist
	}
	for _, c := range All() {
		if got := c.Detected(env); got != (c.ID == "cursor") {
			t.Errorf("%s detected = %v", c.ID, got)
		}
	}
}

func clientByID(id string) Client {
	for _, c := range All() {
		if c.ID == id {
			return c
		}
	}
	panic(id)
}

func TestForeignEntryIsKept(t *testing.T) {
	home := t.TempDir()
	env := Env{Home: home, GOOS: "linux"}
	f := filepath.Join(home, ".cursor", "mcp.json")
	_ = os.MkdirAll(filepath.Dir(f), 0o755)
	foreign := `{"mcpServers":{"sonarqube":{"command":"docker","args":["run","mcp/sonarqube"],"env":{"SONARQUBE_URL":"https://sonar.example.com"}}}}`
	_ = os.WriteFile(f, []byte(foreign), 0o644)
	c := clientByID("cursor")

	if c.Configured(env) {
		t.Fatal("someone else's sonarqube entry counted as ours")
	}
	name, err := c.Register(env, "/opt/bin/sonarless-mcp")
	if err != nil || name != AltServerName {
		t.Fatalf("Register = %q, %v; want %q", name, err, AltServerName)
	}
	if v, _ := jsonEntry(f, "mcpServers", ServerName); !strings.Contains(v, "sonar.example.com") {
		t.Errorf("foreign entry changed: %s", v)
	}
	if !c.Configured(env) {
		t.Error("not configured after register")
	}
	if name, _ := c.Register(env, "/opt/bin/sonarless-mcp"); name != AltServerName {
		t.Errorf("re-register moved to %q", name)
	}
	if err := c.Remove(env); err != nil {
		t.Fatal(err)
	}
	if _, ok := jsonEntry(f, "mcpServers", ServerName); !ok {
		t.Error("Remove deleted the foreign entry")
	}
	if _, ok := jsonEntry(f, "mcpServers", AltServerName); ok {
		t.Error("Remove left our entry")
	}
}

func TestCodexForeignEntryIsKept(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CODEX_HOME", "")
	env := Env{Home: home, GOOS: "linux"}
	f := filepath.Join(home, ".codex", "config.toml")
	_ = os.MkdirAll(filepath.Dir(f), 0o755)
	_ = os.WriteFile(f, []byte("[mcp_servers.sonarqube]\ncommand = \"docker\"\n"), 0o644)
	c := clientByID("codex")
	if c.Configured(env) {
		t.Fatal("foreign entry counted as ours")
	}
	if name, err := c.Register(env, "/opt/bin/sonarless-mcp"); err != nil || name != AltServerName {
		t.Fatalf("Register = %q, %v", name, err)
	}
	_ = c.Remove(env)
	b, _ := os.ReadFile(f)
	if !strings.Contains(string(b), "[mcp_servers.sonarqube]") || strings.Contains(string(b), "sonarqube-local") {
		t.Errorf("after remove:\n%s", b)
	}
}

func TestFindSonarQubeCredentials(t *testing.T) {
	home := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", "")
	env := Env{Home: home, GOOS: "linux"}
	f := filepath.Join(home, ".config", "Code", "User", "mcp.json")
	_ = os.MkdirAll(filepath.Dir(f), 0o755)
	_ = os.WriteFile(f, []byte(`{"servers":{
	  "sonarqube":{"command":"docker","env":{"SONARQUBE_URL":"https://sonar.example.com","SONARQUBE_TOKEN":"squ_x"}},
	  "templated":{"command":"docker","env":{"SONARQUBE_URL":"https://y","SONARQUBE_TOKEN":"${input:token}"}},
	  "sonarqube-local":{"command":"/bin/sonarless-mcp","env":{"SONARQUBE_URL":"https://z","SONARQUBE_TOKEN":"t"}}}}`), 0o644)
	got := FindSonarQubeCredentials(env)
	if len(got) != 1 || got[0].URL != "https://sonar.example.com" || got[0].Token != "squ_x" || got[0].Client != "VS Code" {
		t.Errorf("got %+v", got)
	}
}

func TestSkillInstallRespectsUserSkill(t *testing.T) {
	home := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("CODEX_HOME", "")
	env := Env{Home: home, GOOS: "linux"}
	claude := clientByID("claude")

	f, err := claude.InstallSkill(env)
	if err != nil || f == "" {
		t.Fatalf("install: %q %v", f, err)
	}
	b, _ := os.ReadFile(f)
	if !strings.Contains(string(b), "name: sonarless-mcp") || !strings.Contains(string(b), skillMarker) {
		t.Errorf("skill content: %.80s", b)
	}
	// Outdated copy gets refreshed after an update.
	_ = os.WriteFile(f, []byte("old "+skillMarker), 0o644)
	if n := RefreshSkills(env); n != 1 {
		t.Errorf("refreshed %d", n)
	}
	if err := claude.RemoveSkill(env); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Dir(f)); !os.IsNotExist(err) {
		t.Error("skill dir left behind")
	}

	// A user's own skill with the same name is never touched.
	_ = os.MkdirAll(filepath.Dir(f), 0o755)
	_ = os.WriteFile(f, []byte("my own notes"), 0o644)
	if got, _ := claude.InstallSkill(env); got != "" {
		t.Error("overwrote the user's skill")
	}
	_ = claude.RemoveSkill(env)
	if b, _ := os.ReadFile(f); string(b) != "my own notes" {
		t.Error("removed the user's skill")
	}
	if clientByID("vscode").SkillDir(env) != "" {
		t.Error("vscode has no skills dir")
	}
}
