package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/BrainerVirus/sonarless-mcp/internal/clients"
	"github.com/BrainerVirus/sonarless-mcp/internal/idle"
	"github.com/BrainerVirus/sonarless-mcp/internal/remote"
)

const fakeExe = "/opt/bin/sonarless-mcp"

// testInstance isolates a test from a real install: its own home, instance
// name, ports and gateway, and a fake `docker` on PATH that logs every call.
// Containers named in running are running (with memory in use), the rest
// absent; orphan is listed by `docker ps` as a managed container.
func testInstance(t *testing.T, gatewayURL string) (e *env, calls string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell-script fake docker")
	}
	home := t.TempDir()
	t.Setenv("SONARLESS_HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "xdg"))
	t.Setenv("XDG_CACHE_HOME", "")
	t.Setenv("SONARLESS_INSTANCE", "sltest")
	t.Setenv("SONARLESS_SERVER_PORT", "19000")
	t.Setenv("SONARLESS_MCP_PORT", "19100")
	u, _ := url.Parse(gatewayURL)
	t.Setenv("SONARLESS_GATEWAY_PORT", u.Port())

	e, err := load("", false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := remote.Add(e.cfg, remote.Remote{Name: "work", URL: "http://127.0.0.1:1"}, "tok"); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	calls = filepath.Join(dir, "calls.log")
	inspect := func(name, status string) string {
		return fmt.Sprintf(`[{"Name":"/%s","Image":"sha256:1","Config":{"Image":"img:%s"},"State":{"Status":%q}}]`, name, name, status)
	}
	script := "#!/bin/sh\necho \"$*\" >> '" + calls + "'\ncase \"$1 $2\" in\n" +
		"  version*) echo 27.0 ;;\n" +
		"  'container inspect')\n    case \"$3\" in\n" +
		"      sltest-server) echo '" + inspect("sltest-server", "running") + "' ;;\n" +
		"      sltest-mcp) echo '" + inspect("sltest-mcp", "running") + "' ;;\n" +
		"      sltest-mcp-work) echo '" + inspect("sltest-mcp-work", "running") + "' ;;\n" +
		"      sltest-mcp-old) echo '" + inspect("sltest-mcp-old", "exited") + "' ;;\n" +
		"      sltest-mcp-mcp) echo '" + inspect("sltest-mcp-mcp", "running") + "' ;;\n" +
		"      sltest-mcp-server) echo '" + inspect("sltest-mcp-server", "running") + "' ;;\n" +
		"      *) echo \"Error: No such container: $3\" >&2; exit 1 ;;\n    esac ;;\n" +
		// Labels: this instance's MCP containers carry instance=sltest; an
		// instance literally named "sltest-mcp" (containers sltest-mcp-mcp,
		// sltest-mcp-server) shares their name prefix but not the label.
		"  'ps -a')\n    case \"$4\" in\n" +
		"      label=sonarless-mcp.instance=sltest) printf 'sltest-mcp\\nsltest-mcp-work\\nsltest-mcp-old\\n' ;;\n" +
		"      label=sonarless-mcp.instance=sltest-mcp) printf 'sltest-mcp-mcp\\n' ;;\n" +
		"      label=sonarless-mcp.managed=true) printf 'sltest-server\\nsltest-mcp\\nsltest-mcp-work\\nsltest-mcp-old\\nsltest-mcp-mcp\\nsltest-mcp-server\\n' ;;\n" +
		"    esac ;;\n" +
		"  stats*) for n in \"$@\"; do case \"$n\" in sltest-*) printf '%s\\t512MiB / 15.5GiB\\n' \"$n\" ;; esac; done ;;\n" +
		"  stop*) echo \"$2\" ;;\n" +
		"esac\n"
	if err := os.WriteFile(filepath.Join(dir, "docker"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return e, calls
}

// fakeProcs replaces the process table: pid -> command line.
func fakeProcs(t *testing.T, table map[int]string) {
	t.Helper()
	orig := idle.CommandLines
	idle.CommandLines = func(pids []int) map[int]string {
		out := map[int]string{}
		for _, p := range pids {
			if c, ok := table[p]; ok {
				out[p] = c
			}
		}
		return out
	}
	t.Cleanup(func() { idle.CommandLines = orig })
}

// fakeGateway answers /health and an authenticated /status like a gateway
// with two sessions.
func fakeGateway(t *testing.T, token string, pid int, started time.Time) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
			_, _ = w.Write([]byte(`{"status":"ok"}`))
		case "/status":
			if r.Header.Get("Authorization") != "Bearer "+token {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"pid": pid, "version": "1.2.3", "started": started,
				"sessions": []map[string]any{
					{"workspace": "/ws/app", "lastRequest": time.Now().Add(-3 * time.Minute), "streams": 1},
					{"workspace": "", "lastRequest": time.Now().Add(-time.Hour), "streams": 0},
				}})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// statusFixture is a machine with running containers (one left from a
// removed remote), a gateway with two sessions, a live and a stale stdio
// bridge, and a scan.
func statusFixture(t *testing.T) *env {
	t.Helper()
	started := time.Now().Add(-2 * time.Hour)
	gw := fakeGateway(t, "gw-token", 4100, started)
	e, _ := testInstance(t, gw.URL)
	if err := os.MkdirAll(filepath.Dir(e.cfg.GatewayTokenFile()), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(e.cfg.GatewayTokenFile(), []byte("gw-token"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(e.cfg.CacheDir, 0o755); err != nil {
		t.Fatal(err)
	}
	pidFile, _ := json.Marshal(map[string]any{"pid": 4100, "exe": fakeExe, "started": started})
	if err := os.WriteFile(filepath.Join(e.cfg.CacheDir, "gateway.pid"), pidFile, 0o644); err != nil {
		t.Fatal(err)
	}
	fakeProcs(t, map[int]string{
		4100: fakeExe + " serve",
		4200: fakeExe + " mcp /ws/app",
		4201: "/usr/bin/vim notes.txt", // a bridge's pid, reused
		4300: fakeExe + " mcp /ws/app", // a bridge running a scan through sonarless_scan
	})
	for _, p := range []idle.Proc{
		{Kind: idle.KindShim, PID: 4200, Exe: fakeExe, Verb: "mcp", Workspace: "/ws/app"},
		{Kind: idle.KindShim, PID: 4201, Exe: fakeExe, Verb: "mcp", Workspace: "/ws/gone"},
		{Kind: idle.KindScan, PID: 4300, Exe: fakeExe, Verb: "mcp", Workspace: "/ws/app", Project: "app", Scanner: "maven", State: idle.ScanWaiting},
	} {
		if _, err := idle.Register(e.cfg, p); err != nil {
			t.Fatal(err)
		}
	}
	return e
}

func TestStatusJSON(t *testing.T) {
	statusFixture(t)
	cmd := statusCommand()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"--json"})
	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatal(err)
	}
	var r statusReport
	if err := json.Unmarshal(out.Bytes(), &r); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, out.String())
	}

	mem := map[string]string{}
	for _, c := range r.Containers {
		mem[c.Role+" "+c.Name+" "+c.State] = c.Memory
	}
	want := map[string]string{
		"server sltest-server running":   "512MiB",
		"mcp sltest-mcp running":         "512MiB",
		"remote sltest-mcp-work running": "512MiB",
		"remote sltest-mcp-old exited":   "", // left from a removed remote: listed, no memory
	}
	if fmt.Sprint(mem) != fmt.Sprint(want) {
		t.Errorf("containers (role name state: memory):\n got %v\nwant %v", mem, want)
	}
	g := r.Gateway
	if !g.Running || g.PID != 4100 || g.Version != "1.2.3" || !strings.HasPrefix(g.Uptime, "2h") || len(g.Sessions) != 2 ||
		g.Sessions[0].Workspace != "/ws/app" || g.Sessions[0].Streams != 1 {
		t.Errorf("gateway: %+v", g)
	}
	if len(r.Shims) != 1 || r.Shims[0].PID != 4200 || r.Shims[0].Workspace != "/ws/app" {
		t.Errorf("shims: %+v", r.Shims)
	}
	if len(r.Scans) != 1 || r.Scans[0].Project != "app" || r.Scans[0].Scanner != "maven" || r.Scans[0].State != idle.ScanWaiting {
		t.Errorf("scans: %+v", r.Scans)
	}

	// The human view shows the same facts.
	var human bytes.Buffer
	printStatus(&human, r)
	for _, s := range []string{"512MiB", "pid 4100", "2 session(s)", "session /ws/app", "bridge  pid 4200", "scan    pid 4300, app (maven, waiting "} {
		if !strings.Contains(human.String(), s) {
			t.Errorf("human status lacks %q:\n%s", s, human.String())
		}
	}
}

func TestAgentStatusText(t *testing.T) {
	e := statusFixture(t)
	text := statusText(context.Background(), e)
	for _, s := range []string{
		"local server: running, 512MiB memory (http://localhost:19000)",
		"local mcp: running, 512MiB memory",
		"remote mcp sltest-mcp-work: running, 512MiB memory",
		"remote mcp sltest-mcp-old: exited",
		"gateway: running on port ", ", pid 4100, up 2h",
		"2 session(s): /ws/app (last request 3m",
		"(no workspace) (last request 1h",
		"stdio bridges: 1 (pid 4200 /ws/app)",
		"running scans: 1 (app (maven, waiting ",
		"work: http://127.0.0.1:1, UNREACHABLE",
	} {
		if !strings.Contains(text, s) {
			t.Errorf("agent status lacks %q:\n%s", s, text)
		}
	}
	if lines := strings.Count(text, "\n"); lines > 14 {
		t.Errorf("agent status is %d lines; keep it compact:\n%s", lines, text)
	}
}

func TestAgentStatusTextWithoutDocker(t *testing.T) {
	e := statusFixture(t)
	t.Setenv("PATH", t.TempDir()) // no docker at all
	text := statusText(context.Background(), e)
	for _, s := range []string{"docker: docker CLI not found", "gateway: running", "stdio bridges: 1", "running scans: 1"} {
		if !strings.Contains(text, s) {
			t.Errorf("agent status without docker lacks %q:\n%s", s, text)
		}
	}
	if strings.Contains(text, "local server:") {
		t.Errorf("container states reported without docker:\n%s", text)
	}
}

func TestStopAllStopsEveryContainerAndTheGateway(t *testing.T) {
	gw := fakeGateway(t, "gw-token", 0, time.Now())
	e, calls := testInstance(t, gw.URL)

	// A real child stands in for the gateway process, so the stop is observable.
	child := exec.Command("sleep", "60")
	if err := child.Start(); err != nil {
		t.Skip("no sleep binary:", err)
	}
	exited := make(chan struct{})
	go func() { _ = child.Wait(); close(exited) }()
	t.Cleanup(func() { _ = child.Process.Kill() })
	fakeProcs(t, map[int]string{child.Process.Pid: fakeExe + " serve"})
	if err := os.MkdirAll(e.cfg.CacheDir, 0o755); err != nil {
		t.Fatal(err)
	}
	pidFile, _ := json.Marshal(map[string]any{"pid": child.Process.Pid, "exe": fakeExe, "started": time.Now()})
	if err := os.WriteFile(filepath.Join(e.cfg.CacheDir, "gateway.pid"), pidFile, 0o644); err != nil {
		t.Fatal(err)
	}

	// A systemd login service runs the gateway: it must be stopped through
	// systemctl, or it would restart the gateway at once.
	var ran []string
	ce := clients.Env{Home: t.TempDir(), GOOS: "linux", Run: func(name string, args ...string) (string, error) {
		ran = append(ran, name+" "+strings.Join(args, " "))
		return "", nil
	}}
	writeUnit(t, ce, map[string]string{
		"SONARLESS_HOME": os.Getenv("SONARLESS_HOME"), "XDG_CONFIG_HOME": os.Getenv("XDG_CONFIG_HOME"),
		"SONARLESS_INSTANCE": "sltest", "PATH": "/usr/bin",
	})

	// Plain stop: containers only.
	var out bytes.Buffer
	if err := runStop(context.Background(), &out, e, false, ce); err != nil {
		t.Fatal(err)
	}
	if len(ran) != 0 {
		t.Errorf("plain stop touched the gateway service: %v", ran)
	}
	select {
	case <-exited:
		t.Fatal("plain stop stopped the gateway")
	case <-time.After(100 * time.Millisecond):
	}
	log, _ := os.ReadFile(calls)
	for _, n := range []string{"sltest-server", "sltest-mcp", "sltest-mcp-work", "sltest-mcp-old"} {
		if !strings.Contains(string(log), "stop "+n+"\n") {
			t.Errorf("container %s not stopped; docker calls:\n%s", n, log)
		}
	}
	for _, n := range []string{"sltest-mcp-mcp", "sltest-mcp-server"} { // instance "sltest-mcp"
		if strings.Contains(string(log), "stop "+n+"\n") {
			t.Errorf("stopped another instance's container %s; docker calls:\n%s", n, log)
		}
	}

	// stop --all: the gateway too.
	_ = os.WriteFile(calls, nil, 0o644)
	out.Reset()
	if err := runStop(context.Background(), &out, e, true, ce); err != nil {
		t.Fatal(err)
	}
	if want := "systemctl --user stop sonarless-mcp-gateway.service"; len(ran) != 1 || ran[0] != want {
		t.Errorf("service calls %v, want [%s]", ran, want)
	}
	select {
	case <-exited:
	case <-time.After(5 * time.Second):
		t.Fatal("gateway process still running after stop --all")
	}
	if _, err := os.Stat(filepath.Join(e.cfg.CacheDir, "gateway.pid")); !os.IsNotExist(err) {
		t.Errorf("gateway pid file kept (%v)", err)
	}
	log, _ = os.ReadFile(calls)
	if !strings.Contains(string(log), "stop sltest-mcp-work\n") || !strings.Contains(out.String(), "Gateway stopped.") {
		t.Errorf("stop --all output %q; docker calls:\n%s", out.String(), log)
	}
}

// writeUnit writes the systemd unit setup would, with env.
func writeUnit(t *testing.T, ce clients.Env, env map[string]string) {
	t.Helper()
	unit := clients.AutostartFile(ce)
	if err := os.MkdirAll(filepath.Dir(unit), 0o755); err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	b.WriteString("[Service]\nExecStart=\"/opt/bin/sonarless-mcp\" serve\n")
	for k, v := range env {
		fmt.Fprintf(&b, "Environment=\"%s=%s\"\n", k, strings.NewReplacer(`\`, `\\`, `"`, `\"`, "%", "%%").Replace(v))
	}
	b.WriteString("Restart=always\n")
	if err := os.WriteFile(unit, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestStopAllLeavesAnotherConfigsGateway(t *testing.T) {
	gw := fakeGateway(t, "gw-token", 0, time.Now())
	e, _ := testInstance(t, gw.URL) // a scratch SONARLESS_HOME
	fakeProcs(t, nil)
	var ran []string
	ce := clients.Env{Home: t.TempDir(), GOOS: "linux", Run: func(name string, args ...string) (string, error) {
		ran = append(ran, name+" "+strings.Join(args, " "))
		return "", nil
	}}
	// The login service was installed from the user's real config: no
	// SONARLESS_HOME, default instance.
	writeUnit(t, ce, map[string]string{"PATH": "/usr/bin"})

	var out bytes.Buffer
	if err := runStop(context.Background(), &out, e, true, ce); err != nil {
		t.Fatal(err)
	}
	if len(ran) != 0 {
		t.Errorf("stopped another config's login service: %v", ran)
	}
	if !strings.Contains(out.String(), "belongs to another sonarless-mcp config") ||
		!strings.Contains(out.String(), `its SONARLESS_HOME="", here "`+os.Getenv("SONARLESS_HOME")+`"`) ||
		strings.Contains(out.String(), "Gateway stopped.") {
		t.Errorf("output:\n%s", out.String())
	}
}

func TestServiceEnv(t *testing.T) {
	unit := "[Service]\nExecStart=\"/opt/bin/sonarless-mcp\" serve\n" +
		`Environment="SONARLESS_HOME=/home/me/my \"sq\" 100%%"` + "\n" +
		`Environment="SONARLESS_INSTANCE=work"` + "\n"
	if got := serviceEnv("linux", unit); got["SONARLESS_HOME"] != `/home/me/my "sq" 100%` || got["SONARLESS_INSTANCE"] != "work" {
		t.Errorf("systemd: %v", got)
	}
	plist := `<dict>
  <key>Label</key><string>io.github.brainervirus.sonarless-mcp.gateway</string>
  <key>EnvironmentVariables</key><dict>
    <key>SONARLESS_HOME</key><string>/Users/me/a &amp; b</string>
  </dict>
</dict>`
	got := serviceEnv("darwin", plist)
	if got["SONARLESS_HOME"] != "/Users/me/a & b" || len(got) != 1 {
		t.Errorf("launchd: %v", got)
	}
}

func TestAgentStatusTextCapsLists(t *testing.T) {
	e := statusFixture(t)
	table := map[int]string{4100: fakeExe + " serve"}
	for i := 0; i < 8; i++ {
		pid := 5000 + i
		table[pid] = fakeExe + " mcp /ws/many"
		for _, kind := range []string{idle.KindShim, idle.KindScan} {
			if _, err := idle.Register(e.cfg, idle.Proc{Kind: kind, PID: pid, Exe: fakeExe, Verb: "mcp", Workspace: fmt.Sprintf("/ws/many%d", i), Project: "p", Scanner: "cli"}); err != nil {
				t.Fatal(err)
			}
		}
	}
	fakeProcs(t, table)
	text := statusText(context.Background(), e)
	for _, s := range []string{"stdio bridges: 8 (", "running scans: 8 ("} {
		line := lineWith(text, s)
		if n := strings.Count(line, "/ws/many"); n != 5 || !strings.HasSuffix(line, "; +3 more)") {
			t.Errorf("%q names %d entries: %s", s, n, line)
		}
	}
}

func TestAgentStatusTextCapsSessions(t *testing.T) {
	sessions := make([]map[string]any, 9)
	for i := range sessions {
		sessions[i] = map[string]any{"workspace": fmt.Sprintf("/ws/s%d", i), "lastRequest": time.Now().Add(-time.Duration(i) * time.Minute)}
	}
	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"pid": 1, "started": time.Now(), "sessions": sessions})
	}))
	t.Cleanup(gw.Close)
	e, _ := testInstance(t, gw.URL)
	fakeProcs(t, nil)
	if err := os.MkdirAll(filepath.Dir(e.cfg.GatewayTokenFile()), 0o700); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(e.cfg.GatewayTokenFile(), []byte("t"), 0o600)
	line := lineWith(statusText(context.Background(), e), "gateway:")
	if !strings.Contains(line, "9 session(s): /ws/s0 ") || strings.Count(line, "/ws/s") != 5 ||
		strings.Contains(line, "/ws/s5") || !strings.HasSuffix(line, "; +4 more") {
		t.Errorf("sessions not capped to the 5 most recent: %s", line)
	}
}

func lineWith(text, prefix string) string {
	for _, l := range strings.Split(text, "\n") {
		if strings.HasPrefix(l, prefix) {
			return l
		}
	}
	return ""
}
