package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/BrainerVirus/sonarless-mcp/internal/config"
	"github.com/BrainerVirus/sonarless-mcp/internal/project"
)

const toolsList = `{"tools":[
 {"name":"get_component_measures","inputSchema":{"type":"object","properties":{"projectKey":{"type":"string","description":"The project key"},"branch":{"type":"string"}},"required":["projectKey"]}},
 {"name":"search_sonar_issues_in_projects","inputSchema":{"type":"object","properties":{"projects":{"type":"array","items":{"type":"string"},"description":"Projects"}}}},
 {"name":"show_rule","inputSchema":{"type":"object","properties":{"key":{"type":"string"}},"required":["key"]}}
]}`

func newTestShim(t *testing.T) *Shim {
	t.Helper()
	t.Setenv("SONARLESS_HOME", t.TempDir())
	// No idle watcher: it spawns os.Executable() — the test binary here.
	t.Setenv("SONARLESS_IDLE_TIMEOUT", "0")
	cfg, _, err := config.Load("")
	if err != nil {
		t.Fatal(err)
	}
	local := &Backend{Name: "local", About: "the local SonarQube"}
	local.inject.Store(true)
	s := &Shim{Cfg: cfg, Backends: []*Backend{local}, Project: &project.Project{Key: "acme_web", KeySource: "sonar-project.properties"}}
	s.store("tools/list", json.RawMessage(toolsList))
	return s
}

func addRemote(s *Shim, inject bool) *Backend {
	r := &Backend{Name: "work", About: "remote CI server", Branch: "develop"}
	r.inject.Store(inject)
	s.Backends = append(s.Backends, r)
	return r
}

func args(t *testing.T, params json.RawMessage) map[string]any {
	t.Helper()
	var p struct {
		Arguments map[string]any `json:"arguments"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		t.Fatal(err)
	}
	return p.Arguments
}

func TestFillDefaults(t *testing.T) {
	s := newTestShim(t)
	local := s.local()
	cases := []struct {
		name, in string
		want     map[string]any
	}{
		{"fills missing projectKey", `{"name":"get_component_measures","arguments":{"branch":"main"}}`,
			map[string]any{"projectKey": "acme_web", "branch": "main"}},
		{"fills empty projectKey", `{"name":"get_component_measures","arguments":{"projectKey":""}}`,
			map[string]any{"projectKey": "acme_web"}},
		{"keeps explicit projectKey", `{"name":"get_component_measures","arguments":{"projectKey":"other"}}`,
			map[string]any{"projectKey": "other"}},
		{"fills projects list", `{"name":"search_sonar_issues_in_projects"}`,
			map[string]any{"projects": []any{"acme_web"}}},
		{"keeps explicit projects", `{"name":"search_sonar_issues_in_projects","arguments":{"projects":["x"]}}`,
			map[string]any{"projects": []any{"x"}}},
		{"leaves tools without a project alone", `{"name":"show_rule","arguments":{"key":"go:S100"}}`,
			map[string]any{"key": "go:S100"}},
		{"strips the server argument", `{"name":"show_rule","arguments":{"key":"k","server":"local"}}`,
			map[string]any{"key": "k"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := args(t, s.fillDefaults(local, json.RawMessage(tc.in))); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}

	local.inject.Store(false) // project unknown to that server: never guess
	if got := args(t, s.fillDefaults(local, json.RawMessage(`{"name":"get_component_measures","arguments":{}}`))); len(got) != 0 {
		t.Errorf("injected without a known project: %v", got)
	}
}

func TestRemoteDefaults(t *testing.T) {
	s := newTestShim(t)
	work := addRemote(s, true)
	got := args(t, s.fillDefaults(work, json.RawMessage(`{"name":"get_component_measures","arguments":{"server":"work"}}`)))
	want := map[string]any{"projectKey": "acme_web", "branch": "develop"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
	got = args(t, s.fillDefaults(work, json.RawMessage(`{"name":"get_component_measures","arguments":{"branch":"feature/x"}}`)))
	if got["branch"] != "feature/x" {
		t.Errorf("explicit branch overridden: %v", got)
	}
	if b := s.backend("work"); b != work || s.backend("nope") != nil {
		t.Error("backend lookup")
	}
}

func TestAnnotateTools(t *testing.T) {
	s := newTestShim(t)
	var r struct {
		Tools []struct {
			Name        string
			InputSchema struct {
				Properties map[string]struct {
					Description string
					Enum        []string
				}
				Required []string
			}
		}
	}
	if err := json.Unmarshal(s.annotateTools(json.RawMessage(toolsList)), &r); err != nil {
		t.Fatal(err)
	}
	m := r.Tools[0].InputSchema
	if len(m.Required) != 0 {
		t.Errorf("projectKey still required: %v", m.Required)
	}
	if !strings.Contains(m.Properties["projectKey"].Description, `"acme_web"`) {
		t.Errorf("default not documented: %q", m.Properties["projectKey"].Description)
	}
	if _, ok := m.Properties["server"]; ok {
		t.Error("server argument added without remotes")
	}
	if got := r.Tools[2].InputSchema.Required; !reflect.DeepEqual(got, []string{"key"}) {
		t.Errorf("unrelated required changed: %v", got)
	}

	addRemote(s, false)
	_ = json.Unmarshal(s.annotateTools(json.RawMessage(toolsList)), &r)
	for _, tool := range r.Tools {
		if got := tool.InputSchema.Properties["server"].Enum; !reflect.DeepEqual(got, []string{"local", "work"}) {
			t.Errorf("%s server enum = %v", tool.Name, got)
		}
	}
}

func TestAnnotateInitialize(t *testing.T) {
	s := newTestShim(t)
	addRemote(s, true)
	var r map[string]any
	_ = json.Unmarshal(s.annotateInitialize(json.RawMessage(`{"protocolVersion":"x","instructions":"Base."}`)), &r)
	in, _ := r["instructions"].(string)
	if !strings.HasPrefix(in, "Base.") || !strings.Contains(in, "acme_web") || !strings.Contains(in, `"work" = remote CI server`) {
		t.Errorf("instructions = %q", in)
	}
	if r["protocolVersion"] != "x" {
		t.Error("other fields lost")
	}
}

func TestReadSSE(t *testing.T) {
	body := "event: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":1,\ndata: \"result\":{}}\n\n: comment\ndata: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/progress\"}\n"
	msgs, err := readSSE(strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 2 || string(msgs[0].ID) != "1" || msgs[1].Method != "notifications/progress" {
		t.Errorf("got %+v", msgs)
	}
}

func TestDecodeMessages(t *testing.T) {
	if got := decodeMessages([]byte(`[{"jsonrpc":"2.0","id":1,"result":{}},{"jsonrpc":"2.0","id":2,"result":{}}]`)); len(got) != 2 {
		t.Errorf("batch: %d messages", len(got))
	}
	if got := decodeMessages([]byte("not json")); got != nil {
		t.Errorf("garbage: %v", got)
	}
}

func TestCacheRoundTrip(t *testing.T) {
	s := newTestShim(t)
	s.store("initialize", json.RawMessage(`{"a":1}`))
	if got := string(s.cached("initialize")); got != `{"a":1}` {
		t.Errorf("initialize cache = %s", got)
	}
	if s.cached("tools/list") == nil {
		t.Error("storing initialize dropped tools/list")
	}
	if _, err := os.Stat(filepath.Join(s.Cfg.CacheDir, "mcp-cache.json")); err != nil {
		t.Error(err)
	}
}

func TestUnreachableRemoteFailsFast(t *testing.T) {
	s := newTestShim(t)
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close() // nothing listens here any more: like a VPN-only host while offline
	booted := false
	work := &Backend{Name: "work", APIURL: url, Remote: true,
		Ensure: func(context.Context) error { booted = true; return nil }}
	s.Backends = append(s.Backends, work)

	start := time.Now()
	_, _, err := s.route(context.Background(), json.RawMessage(`{"name":"show_rule","arguments":{"server":"work"}}`))
	var te toolError
	if !errors.As(err, &te) || !strings.Contains(err.Error(), "unreachable") {
		t.Fatalf("err = %v", err)
	}
	if booted {
		t.Error("started the remote's container although it is unreachable")
	}
	if time.Since(start) > 4*time.Second {
		t.Errorf("took %s", time.Since(start))
	}
	// Cached: a second call doesn't probe again.
	start = time.Now()
	_, _, _ = s.route(context.Background(), json.RawMessage(`{"name":"show_rule","arguments":{"server":"work"}}`))
	if time.Since(start) > 100*time.Millisecond {
		t.Error("probe not cached")
	}

	_, _, err = s.route(context.Background(), json.RawMessage(`{"name":"show_rule","arguments":{"server":"nope"}}`))
	if err == nil || errors.As(err, &te) {
		t.Errorf("unknown server: %v", err)
	}
}

func TestToolFailureShape(t *testing.T) {
	s := newTestShim(t)
	var out bytes.Buffer
	s.out = &out
	s.toolFailure(message{ID: json.RawMessage("7")}, "work is unreachable")
	var m struct {
		ID     int
		Result struct {
			IsError bool `json:"isError"`
			Content []struct{ Type, Text string }
		}
	}
	if err := json.Unmarshal(out.Bytes(), &m); err != nil || m.ID != 7 || !m.Result.IsError || !strings.Contains(m.Result.Content[0].Text, "unreachable") {
		t.Errorf("got %s (%v)", out.String(), err)
	}
}

func TestLocalTools(t *testing.T) {
	s := newTestShim(t)
	addRemote(s, true)
	called := false
	s.Local = []LocalTool{{Name: "sonarless_status", Description: "status",
		Run: func(context.Context, map[string]any) (string, bool) { called = true; return "all good", false }}}

	var r struct {
		Tools []struct {
			Name        string
			InputSchema struct{ Properties map[string]any }
		}
	}
	_ = json.Unmarshal(s.annotateTools(json.RawMessage(toolsList)), &r)
	last := r.Tools[len(r.Tools)-1]
	if last.Name != "sonarless_status" {
		t.Fatalf("local tool not listed: %+v", r.Tools)
	}
	if _, ok := last.InputSchema.Properties["server"]; ok {
		t.Error("local tool got the server argument")
	}

	// Answered by the shim itself, even though nothing has booted.
	var out bytes.Buffer
	s.out = &out
	s.booted = make(chan struct{}) // never closed: SonarQube still "starting"
	s.handle(context.Background(), message{JSONRPC: "2.0", ID: json.RawMessage("5"), Method: "tools/call",
		Params: json.RawMessage(`{"name":"sonarless_status","arguments":{}}`)})
	if !called || !strings.Contains(out.String(), "all good") || !strings.Contains(out.String(), `"id":5`) {
		t.Errorf("called=%v out=%s", called, out.String())
	}
}

// runShim drives a shim over pipes; returns a send func and a channel of
// every message the shim writes.
func runShim(t *testing.T, s *Shim) (func(string), <-chan map[string]any) {
	t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() { cancel(); inW.Close() })
	go func() { _ = s.Run(ctx, inR, outW); outW.Close() }()
	msgs := make(chan map[string]any, 32)
	go func() {
		sc := bufio.NewScanner(outR)
		sc.Buffer(make([]byte, 1<<20), 1<<20)
		for sc.Scan() {
			var m map[string]any
			if json.Unmarshal(sc.Bytes(), &m) != nil {
				t.Errorf("non-JSON on stdout: %q", sc.Text())
			}
			msgs <- m
		}
		close(msgs)
	}()
	return func(line string) { _, _ = io.WriteString(inW, line+"\n") }, msgs
}

func next(t *testing.T, msgs <-chan map[string]any, within time.Duration) map[string]any {
	t.Helper()
	select {
	case m := <-msgs:
		return m
	case <-time.After(within):
		t.Fatal("no message from shim")
		return nil
	}
}

func TestNoReplyGetsAnswered(t *testing.T) {
	s := newTestShim(t)
	html := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = io.WriteString(w, "<html>proxy says hi</html>")
	}))
	defer html.Close()
	b := s.local()
	b.MCPURL, b.APIURL = html.URL, html.URL
	b.Ensure = func(context.Context) error { return nil }
	b.Token = func(context.Context, bool) (string, error) { return "t", nil }
	send, msgs := runShim(t, s)
	send(`{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"show_rule","arguments":{"key":"x"}}}`)
	m := next(t, msgs, 5*time.Second)
	res, _ := m["result"].(map[string]any)
	if m["id"] != float64(7) || res["isError"] != true {
		t.Errorf("got %v", m)
	}
}

func TestSlowFirstStartIsDegradedThenListChanged(t *testing.T) {
	s := newTestShim(t)
	_ = os.Remove(s.cacheFile()) // first ever start: nothing cached
	s.Local = []LocalTool{{Name: "sonarless_status", Run: func(context.Context, map[string]any) (string, bool) { return "ok", false }}}
	release := make(chan struct{})
	b := s.local()
	b.Ensure = func(ctx context.Context) error { <-release; return nil } // SonarQube still starting
	b.Token = func(context.Context, bool) (string, error) { return "t", nil }
	b.APIURL = "http://127.0.0.1:1"
	send, msgs := runShim(t, s)

	start := time.Now()
	send(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	m := next(t, msgs, 6*time.Second)
	if time.Since(start) > 5*time.Second {
		t.Errorf("tools/list waited %s", time.Since(start))
	}
	tools := m["result"].(map[string]any)["tools"].([]any)
	if len(tools) != 1 {
		t.Errorf("degraded tools = %v", tools)
	}
	close(release) // SonarQube is up now
	if n := next(t, msgs, 5*time.Second); n["method"] != "notifications/tools/list_changed" {
		t.Errorf("expected list_changed, got %v", n)
	}
}

func TestProjectRecheckAfterScan(t *testing.T) {
	s := newTestShim(t)
	exists := false
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !exists {
			http.Error(w, `{"errors":[{"msg":"not found"}]}`, http.StatusNotFound)
			return
		}
		_, _ = io.WriteString(w, `{}`)
	}))
	defer api.Close()
	b := s.local()
	b.inject.Store(false)
	b.APIURL = api.URL
	b.checkProject(context.Background(), s.Project, true)
	if b.inject.Load() {
		t.Fatal("injecting for a missing project")
	}
	exists = true // e.g. sonarless_scan just created it
	b.checked.Store(time.Now().Add(-projectRecheck).UnixNano())
	b.checkProject(context.Background(), s.Project, false)
	if !b.inject.Load() {
		t.Error("project found later is never defaulted")
	}
}
