package clients

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const testToken = "0123456789abcdef-secret"

// setupHome is a temp HOME with Claude Code, opencode, Cursor, VS Code and
// Codex installed and configs present; run records every command.
func setupHome(t *testing.T, goos string) (Env, *[]string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("CODEX_HOME", "")
	t.Setenv("APPDATA", filepath.Join(home, "AppData", "Roaming"))
	files := map[string]string{
		filepath.Join(home, ".config", "opencode", "opencode.json"): `{"mcp":{"context7":{"type":"remote","url":"https://x"}}}`,
		filepath.Join(home, ".cursor", "mcp.json"):                  `{"mcpServers":{}}`,
		filepath.Join(home, ".codex", "config.toml"):                "model = \"o3\"\n",
	}
	for f, body := range files {
		if err := os.MkdirAll(filepath.Dir(f), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(f, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	var calls []string
	env := Env{Home: home, GOOS: goos, App: func(string) bool { return false },
		Look: func(n string) (string, error) { return "/usr/bin/" + n, nil },
		Run: func(name string, args ...string) (string, error) {
			calls = append(calls, name+" "+strings.Join(args, " "))
			if name == "claude" && len(args) > 1 && args[1] == "get" {
				return "", os.ErrNotExist
			}
			return "", nil
		}}
	return env, &calls
}

func setupOpts(env Env) SetupOptions {
	return SetupOptions{
		Exe: "/opt/bin/sonarless-mcp",
		Direct: Direct{Exe: "/opt/bin/sonarless-mcp", URL: "http://127.0.0.1:9233/sonarless-mcp",
			TokenFile: filepath.Join(env.Home, ".config", "sonarless-mcp", "mcp-gateway-token"), TokenEnv: "SONARLESS_MCP_TOKEN"},
		EnvToken: true,
		LogFile:  filepath.Join(env.Home, "gateway.log"),
	}
}

func read(t *testing.T, f string) string {
	t.Helper()
	b, err := os.ReadFile(f)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestSetupRegistersDirectEntries(t *testing.T) {
	env, calls := setupHome(t, "linux")
	results, autostart := Setup(env, All(), setupOpts(env))

	got := map[string]Result{}
	for _, r := range results {
		if r.Err != nil {
			t.Fatalf("%s: %v", r.Client.ID, r.Err)
		}
		got[r.Client.ID] = r
	}
	for id, direct := range map[string]bool{"claude": true, "opencode": true, "cursor": true, "codex": true, "vscode": true} {
		if got[id].Direct != direct {
			t.Errorf("%s direct = %v, want %v (%s)", id, got[id].Direct, direct, got[id].Why)
		}
	}

	// Claude Code: user-scope HTTP entry; the token comes from the helper.
	var claudeJSON string
	for _, c := range *calls {
		if strings.HasPrefix(c, "claude mcp add-json -s user sonarqube ") {
			claudeJSON = strings.TrimPrefix(c, "claude mcp add-json -s user sonarqube ")
		}
	}
	var ce map[string]any
	if err := json.Unmarshal([]byte(claudeJSON), &ce); err != nil {
		t.Fatalf("claude add-json %q: %v (calls %q)", claudeJSON, err, *calls)
	}
	if _, ok := ce["headers"]; ok {
		t.Errorf("claude entry has static headers (a stale ${PWD}): %v", ce)
	}
	if ce["type"] != "http" || ce["url"] != "http://127.0.0.1:9233/sonarless-mcp" || ce["headersHelper"] != "/opt/bin/sonarless-mcp ensure --headers" {
		t.Errorf("claude entry %v", ce)
	}

	opencode := read(t, filepath.Join(env.Home, ".config", "opencode", "opencode.json"))
	cursor := read(t, filepath.Join(env.Home, ".cursor", "mcp.json"))
	codex := read(t, filepath.Join(env.Home, ".codex", "config.toml"))
	vscode := read(t, filepath.Join(env.Home, ".config", "Code", "User", "mcp.json"))
	if !strings.Contains(vscode, `"type": "http"`) || !strings.Contains(vscode, `"Bearer ${env:SONARLESS_MCP_TOKEN}"`) {
		t.Errorf("vscode entry:\n%s", vscode)
	}
	for _, want := range []string{`"type": "remote"`, `"oauth": false`, `"Bearer {file:` + filepath.ToSlash(setupOpts(env).Direct.TokenFile) + `}"`, `"context7"`} {
		if !strings.Contains(opencode, want) {
			t.Errorf("opencode lacks %s:\n%s", want, opencode)
		}
	}
	if !strings.Contains(cursor, `"Bearer ${env:SONARLESS_MCP_TOKEN}"`) || !strings.Contains(cursor, `"url": "http://127.0.0.1:9233/sonarless-mcp"`) {
		t.Errorf("cursor entry:\n%s", cursor)
	}
	if !strings.Contains(codex, `bearer_token_env_var = "SONARLESS_MCP_TOKEN"`) || !strings.Contains(codex, `model = "o3"`) {
		t.Errorf("codex entry:\n%s", codex)
	}
	for name, body := range map[string]string{"opencode": opencode, "cursor": cursor, "codex": codex, "claude": claudeJSON, "vscode": vscode} {
		if strings.Contains(body, testToken) || strings.Contains(body, " mcp\"") {
			t.Errorf("%s: token inlined or still the stdio shim:\n%s", name, body)
		}
	}
	// The ownership check still recognises the direct entries as ours.
	for _, id := range []string{"opencode", "cursor", "codex", "vscode"} {
		if !clientByID(id).Configured(env) {
			t.Errorf("%s direct entry not recognised as sonarless-mcp's", id)
		}
	}

	// The gateway starts at login so opencode/Cursor/Codex find it up.
	if autostart == "" {
		t.Fatal("no autostart installed")
	}
	unit := read(t, autostart)
	if !strings.Contains(unit, `ExecStart="/opt/bin/sonarless-mcp" serve`) || !strings.Contains(unit, "Restart=always") {
		t.Errorf("unit:\n%s", unit)
	}
	if all := strings.Join(*calls, "\n"); !strings.Contains(all, "systemctl --user enable "+unitName) || !strings.Contains(all, "systemctl --user restart "+unitName) {
		t.Errorf("service not enabled/started:\n%s", all)
	}
}

func TestSetupKeepsShimWhereDirectCantWork(t *testing.T) {
	t.Run("token variable missing", func(t *testing.T) {
		env, _ := setupHome(t, "linux")
		o := setupOpts(env)
		o.EnvToken = false
		results, _ := Setup(env, All(), o)
		for _, r := range results {
			want := r.Client.ID == "claude" || r.Client.ID == "opencode"
			if r.Direct != want {
				t.Errorf("%s direct = %v, want %v", r.Client.ID, r.Direct, want)
			}
			if (r.Client.ID == "cursor" || r.Client.ID == "codex" || r.Client.ID == "vscode") && !r.NeedsEnv {
				t.Errorf("%s: NeedsEnv not set (%s)", r.Client.ID, r.Why)
			}
		}
		if !strings.Contains(read(t, filepath.Join(env.Home, ".codex", "config.toml")), `args = ["mcp"]`) {
			t.Error("codex did not get the stdio shim")
		}
	})
	t.Run("no autostart (wscript missing)", func(t *testing.T) {
		env, _ := setupHome(t, "windows")
		env.Look = func(n string) (string, error) {
			if n == "wscript" {
				return "", os.ErrNotExist
			}
			return "/usr/bin/" + n, nil
		}
		results, autostart := Setup(env, All(), setupOpts(env))
		if autostart != "" {
			t.Errorf("autostart %q without wscript", autostart)
		}
		for _, r := range results {
			if r.Direct != (r.Client.ID == "claude") {
				t.Errorf("%s direct = %v: only a client with a pre-connect hook can go direct", r.Client.ID, r.Direct)
			}
		}
	})
	t.Run("autostart fails", func(t *testing.T) {
		env, _ := setupHome(t, "linux")
		run := env.Run
		env.Run = func(name string, args ...string) (string, error) {
			if name == "systemctl" {
				return "", os.ErrPermission // e.g. no user session bus
			}
			return run(name, args...)
		}
		results, autostart := Setup(env, All(), setupOpts(env))
		if autostart != "" || AutostartInstalled(env) {
			t.Error("a failed autostart left its unit behind")
		}
		for _, r := range results {
			if r.Client.ID == "opencode" && (r.Direct || !strings.Contains(r.Why, "autostart")) {
				t.Errorf("opencode: direct=%v why=%q", r.Direct, r.Why)
			}
		}
	})
	t.Run("--stdio", func(t *testing.T) {
		env, _ := setupHome(t, "linux")
		o := setupOpts(env)
		o.Stdio = true
		results, autostart := Setup(env, All(), o)
		for _, r := range results {
			if r.Direct {
				t.Errorf("%s went direct with --stdio", r.Client.ID)
			}
		}
		if autostart != "" {
			t.Error("autostart installed with --stdio")
		}
	})
}

func TestRemoveAutostart(t *testing.T) {
	env, calls := setupHome(t, "linux")
	if _, err := InstallAutostart(env, "/opt/bin/sonarless-mcp", "", map[string]string{"SONARLESS_HOME": "/x y"}); err != nil {
		t.Fatal(err)
	}
	if unit := read(t, AutostartFile(env)); !strings.Contains(unit, `Environment="SONARLESS_HOME=/x y"`) {
		t.Errorf("env not quoted:\n%s", unit)
	}
	if err := RemoveAutostart(env); err != nil || AutostartInstalled(env) {
		t.Fatalf("remove: %v", err)
	}
	if !strings.Contains(strings.Join(*calls, "\n"), "systemctl --user disable --now "+unitName) {
		t.Errorf("service not stopped: %q", *calls)
	}
}

func TestSetupWindowsAutostart(t *testing.T) {
	env, calls := setupHome(t, "windows")
	o := setupOpts(env)
	o.Exe, o.GatewayEnv = `C:\Program Files\sonarless-mcp\sonarless-mcp.exe`, map[string]string{"SONARLESS_HOME": `C:\s "x"`}
	results, autostart := Setup(env, All(), o)
	for _, r := range results {
		if !r.Direct {
			t.Errorf("%s kept the shim on Windows: %s", r.Client.ID, r.Why)
		}
	}
	if !strings.HasSuffix(autostart, ".vbs") {
		t.Fatalf("autostart = %q", autostart)
	}
	vbs := read(t, autostart)
	for _, want := range []string{
		`sh.Run """C:\Program Files\sonarless-mcp\sonarless-mcp.exe"" serve", 0, False`, // hidden window, no wait
		`env("SONARLESS_HOME") = "C:\s ""x"""`,
	} {
		if !strings.Contains(vbs, want) {
			t.Errorf("launcher lacks %s:\n%s", want, vbs)
		}
	}
	if strings.Contains(vbs, SupervisedEnv) {
		t.Error("no service manager restarts it on Windows: it must respawn itself after an update")
	}
	all := strings.Join(*calls, "\n")
	if !strings.Contains(all, `reg add `+runKey+` /v `+runValue+` /t REG_SZ /d wscript.exe "`+autostart+`" /f`) || !strings.Contains(all, "wscript "+autostart) {
		t.Errorf("Run key not written or gateway not started:\n%s", all)
	}
	if err := RemoveAutostart(env); err != nil || AutostartInstalled(env) {
		t.Fatalf("remove: %v", err)
	}
	if !strings.Contains(strings.Join(*calls, "\n"), "reg delete "+runKey+" /v "+runValue+" /f") {
		t.Error("Run key not deleted")
	}
}

func TestOpencodeLayouts(t *testing.T) {
	cases := []struct {
		name, file, version, section string
		v2                           bool
	}{
		{"v2 file", `{"mcp":{"servers":{"context7":{"type":"remote","url":"https://x"}}}}`, "", "mcp.servers", true},
		{"v1 file", `{"mcp":{"context7":{"type":"remote","url":"https://x"}}}`, "opencode 2.3.0", "mcp", false},
		{"no mcp yet, opencode 2", `{"model":"x"}`, "2.3.0", "mcp.servers", true},
		{"no mcp yet, opencode 1", `{"model":"x"}`, "1.0.4", "mcp", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env, _ := setupHome(t, "linux")
			f := filepath.Join(env.Home, ".config", "opencode", "opencode.json")
			if err := os.WriteFile(f, []byte(tc.file), 0o600); err != nil {
				t.Fatal(err)
			}
			run := env.Run
			env.Run = func(name string, args ...string) (string, error) {
				if name == "opencode" {
					return tc.version + "\n", nil
				}
				return run(name, args...)
			}
			c := clientByID("opencode")
			if _, err := c.RegisterDirect(env, setupOpts(env).Direct); err != nil {
				t.Fatal(err)
			}
			v, ok := jsonEntry(f, tc.section, ServerName)
			if !ok {
				t.Fatalf("no entry under %s:\n%s", tc.section, read(t, f))
			}
			if strings.Contains(v, `"enabled"`) == tc.v2 {
				t.Errorf("v2=%v but entry %s", tc.v2, v)
			}
			if !c.Configured(env) {
				t.Error("not recognised as configured")
			}
			if err := c.Remove(env); err != nil || c.Configured(env) {
				t.Errorf("remove: %v", err)
			}
		})
	}
}

func TestShimOnlyClientKeepsShim(t *testing.T) {
	env, _ := setupHome(t, "linux")
	c := clientByID("cursor")
	c.Direct, c.RegisterDirect = ShimOnly, nil // a client without HTTP support
	results, _ := Setup(env, []Client{c}, setupOpts(env))
	if results[0].Direct || results[0].Err != nil {
		t.Fatalf("%+v", results[0])
	}
	if !strings.Contains(read(t, filepath.Join(env.Home, ".cursor", "mcp.json")), `"command": "/opt/bin/sonarless-mcp"`) {
		t.Error("no stdio shim entry")
	}
}
