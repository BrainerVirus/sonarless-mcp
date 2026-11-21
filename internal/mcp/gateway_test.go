package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/BrainerVirus/sonarless-mcp/internal/config"
	"github.com/BrainerVirus/sonarless-mcp/internal/idle"
	"github.com/BrainerVirus/sonarless-mcp/internal/project"
)

const gwToken = "test-gateway-token"

// fakeSonar is a SonarQube API + MCP endpoint: every project exists, and
// tools/call echoes the arguments it received.
func fakeSonar(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") {
			_, _ = io.WriteString(w, `{}`)
			return
		}
		var m message
		_ = json.NewDecoder(r.Body).Decode(&m)
		if len(m.ID) == 0 {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		var res string
		switch m.Method {
		case "initialize":
			res = `{"protocolVersion":"2025-06-18","capabilities":{"tools":{}},"serverInfo":{"name":"fake"}}`
		case "tools/list":
			res = toolsList
		case "tools/call":
			var p struct{ Arguments map[string]any }
			_ = json.Unmarshal(m.Params, &p)
			a, _ := json.Marshal(p.Arguments)
			b, _ := json.Marshal(map[string]any{"content": []map[string]string{{"type": "text", "text": string(a)}}})
			res = string(b)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":%s}`, m.ID, res)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// newTestGateway serves a gateway whose sessions talk to sonar; ensure is
// each session's container start. It returns the server and the workspaces
// sessions were created for.
func newTestGateway(t *testing.T, sonar *httptest.Server, ensure func(context.Context) error) (*httptest.Server, *Gateway, *[]string) {
	t.Helper()
	t.Setenv("SONARLESS_HOME", t.TempDir())
	t.Setenv("SONARLESS_IDLE_TIMEOUT", "0") // no watcher: it would spawn the test binary
	cfg, _, err := config.Load("")
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var workspaces []string
	g := &Gateway{Cfg: cfg, Token: gwToken, Version: "test", NewSession: func(ws string) (*Shim, error) {
		mu.Lock()
		workspaces = append(workspaces, ws)
		mu.Unlock()
		b := &Backend{Name: "local", About: "the local SonarQube", APIURL: sonar.URL, MCPURL: sonar.URL + "/mcp", Ensure: ensure,
			Token: func(context.Context, bool) (string, error) { return "sq-token", nil }}
		s := &Shim{Cfg: cfg, Backends: []*Backend{b}}
		if ws != "" {
			s.Project = &project.Project{Key: filepath.Base(ws), KeySource: "test"}
		}
		return s, nil
	}}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	srv := httptest.NewServer(g.Handler(ctx))
	t.Cleanup(srv.Close)
	return srv, g, &workspaces
}

func post(t *testing.T, srv *httptest.Server, body string, hdr map[string]string) (*http.Response, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, srv.URL+config.GatewayPath, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Authorization", "Bearer "+gwToken)
	for k, v := range hdr {
		if v == "" {
			req.Header.Del(k)
		} else {
			req.Header.Set(k, v)
		}
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

func TestGatewayRefusesWithoutTokenOrFromBrowsers(t *testing.T) {
	srv, _, _ := newTestGateway(t, fakeSonar(t), func(context.Context) error { return nil })
	list := `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`
	cases := []struct {
		name string
		hdr  map[string]string
		want int
	}{
		{"no token", map[string]string{"Authorization": ""}, http.StatusUnauthorized},
		{"wrong token", map[string]string{"Authorization": "Bearer nope"}, http.StatusUnauthorized},
		{"foreign origin", map[string]string{"Origin": "https://evil.example"}, http.StatusForbidden},
		{"sandboxed origin", map[string]string{"Origin": "null"}, http.StatusForbidden},
		{"local origin", map[string]string{"Origin": "http://localhost:3000"}, http.StatusOK},
		{"token", nil, http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if resp, body := post(t, srv, list, tc.hdr); resp.StatusCode != tc.want {
				t.Errorf("status %d, want %d (%s)", resp.StatusCode, tc.want, body)
			}
		})
	}

	// DNS rebinding: a request whose Host is not local.
	req, _ := http.NewRequest(http.MethodPost, srv.URL+config.GatewayPath, strings.NewReader(list))
	req.Host = "attacker.example"
	req.Header.Set("Authorization", "Bearer "+gwToken)
	if resp, err := http.DefaultClient.Do(req); err != nil || resp.StatusCode != http.StatusForbidden {
		t.Errorf("rebinding host: %v %v", resp.StatusCode, err)
	}

	resp, err := http.Get(srv.URL + "/health") // readiness needs no token
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Errorf("health: %v %v", resp, err)
	}
	if resp, _ := post(t, srv, `{"jsonrpc":"2.0","method":"notifications/initialized"}`, nil); resp.StatusCode != http.StatusAccepted {
		t.Errorf("notification: %d, want 202", resp.StatusCode)
	}
}

func TestGatewayDefaultsProjectPerWorkspace(t *testing.T) {
	srv, _, workspaces := newTestGateway(t, fakeSonar(t), func(context.Context) error { return nil })
	ws := filepath.Join(t.TempDir(), "acme_web")
	if err := os.MkdirAll(filepath.Join(ws, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	call := `{"jsonrpc":"2.0","id":9,"method":"tools/call","params":{"name":"get_component_measures","arguments":{}}}`
	// Clients list tools first; defaults follow the listed schemas.
	post(t, srv, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`, map[string]string{WorkspaceHeader: ws})

	_, body := post(t, srv, call, map[string]string{WorkspaceHeader: ws})
	if !strings.Contains(body, `\"projectKey\":\"acme_web\"`) {
		t.Errorf("projectKey not defaulted to the workspace's project: %s", body)
	}
	// An unexpanded placeholder (client without the variable) means no workspace.
	_, body = post(t, srv, call, map[string]string{WorkspaceHeader: "${PWD}"})
	if strings.Contains(body, "projectKey") {
		t.Errorf("defaulted a project without a workspace: %s", body)
	}
	_, _ = post(t, srv, call, map[string]string{WorkspaceHeader: ws})
	if want := []string{ws, ""}; strings.Join(*workspaces, "|") != strings.Join(want, "|") {
		t.Errorf("sessions %q, want %q (one per workspace, reused)", *workspaces, want)
	}
}

func TestGatewayRequestCountsAsActivity(t *testing.T) {
	srv, g, _ := newTestGateway(t, fakeSonar(t), func(context.Context) error { return nil })
	idle.Touch(g.Cfg)
	old := time.Now().Add(-2 * time.Hour)
	stamp := filepath.Join(g.Cfg.CacheDir, "last-activity")
	if err := os.Chtimes(stamp, old, old); err != nil {
		t.Fatal(err)
	}
	post(t, srv, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`, nil)
	if since := time.Since(idle.Last(g.Cfg)); since > time.Minute {
		t.Errorf("last activity %s ago after a direct request; the idle watcher would stop the containers", since)
	}
}

// fakeDocker puts a `docker` on PATH that reports the MCP container as
// stopped (with an up-to-date spec) and logs every call.
func fakeDocker(t *testing.T, spec string) (logFile string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell-script fake docker")
	}
	dir := t.TempDir()
	logFile = filepath.Join(dir, "calls.log")
	inspect := fmt.Sprintf(`[{"Name":"/sonarless-mcp","Image":"sha256:abc","Config":{"Image":"img","Labels":{"sonarless-mcp.spec":%q}},"State":{"Status":"exited"}}]`, spec)
	if err := os.WriteFile(filepath.Join(dir, "inspect.json"), []byte(inspect), 0o644); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\necho \"$*\" >> '" + logFile + "'\ncase \"$1\" in\n" +
		"  container) cat '" + filepath.Join(dir, "inspect.json") + "' ;;\n" +
		"  image) echo sha256:abc ;;\nesac\n"
	if err := os.WriteFile(filepath.Join(dir, "docker"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return logFile
}

func TestGatewayStartsStoppedContainers(t *testing.T) {
	sonar := fakeSonar(t)
	u, _ := url.Parse(sonar.URL)
	port, _ := strconv.Atoi(u.Port())
	var c *Container
	srv, g, _ := newTestGateway(t, sonar, func(ctx context.Context) error { return c.Ensure(ctx) })
	c = &Container{Cfg: g.Cfg, Name: "sonarless-mcp", Port: port, SonarURL: "http://sonarqube:9000", Image: "img"}
	calls := fakeDocker(t, c.spec())

	resp, body := post(t, srv, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"show_rule","arguments":{"key":"go:S100"}}}`, nil)
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, "go:S100") {
		t.Fatalf("tool call through the gateway: %d %s", resp.StatusCode, body)
	}
	log, _ := os.ReadFile(calls)
	if !strings.Contains(string(log), "start sonarless-mcp") {
		t.Errorf("stopped MCP container not started; docker calls:\n%s", log)
	}
}

// openStream opens the GET event stream and returns its data lines.
func openStream(t *testing.T, srv *httptest.Server) <-chan string {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, srv.URL+config.GatewayPath, nil)
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Authorization", "Bearer "+gwToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("GET stream: %v %v", resp, err)
	}
	lines := make(chan string, 16)
	go func() {
		defer resp.Body.Close()
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			if d, ok := strings.CutPrefix(sc.Text(), "data: "); ok {
				lines <- d
			}
		}
		close(lines)
	}()
	t.Cleanup(func() { resp.Body.Close() })
	return lines
}

func TestGatewayStreamPushesListChangedAfterColdStart(t *testing.T) {
	old := statelessWait
	statelessWait = 100 * time.Millisecond
	t.Cleanup(func() { statelessWait = old })
	release := make(chan struct{})
	srv, _, _ := newTestGateway(t, fakeSonar(t), func(ctx context.Context) error {
		select {
		case <-release: // SonarQube still starting until then
		case <-ctx.Done():
		}
		return nil
	})
	stream := openStream(t, srv)

	// First-ever start, nothing cached: answered degraded, without waiting.
	_, body := post(t, srv, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`, nil)
	if !strings.Contains(body, `"tools":[]`) {
		t.Fatalf("degraded tools/list = %s", body)
	}
	close(release)
	select {
	case d := <-stream:
		if !strings.Contains(d, "notifications/tools/list_changed") {
			t.Errorf("stream sent %s", d)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no tools/list_changed on the event stream")
	}
	// The client re-lists and now gets SonarQube's tools.
	if _, body := post(t, srv, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`, nil); !strings.Contains(body, "get_component_measures") {
		t.Errorf("tools/list after the start: %s", body)
	}
}

func TestGatewayStreamGetsNotificationSentBeforeItOpened(t *testing.T) {
	srv, g, _ := newTestGateway(t, fakeSonar(t), func(context.Context) error { return nil })
	s, err := g.session("")
	if err != nil {
		t.Fatal(err)
	}
	s.notify(message{Method: "notifications/tools/list_changed"})
	select {
	case d := <-openStream(t, srv):
		if !strings.Contains(d, "list_changed") {
			t.Errorf("got %s", d)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("pending notification lost")
	}
}
