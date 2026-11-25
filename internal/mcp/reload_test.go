package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/BrainerVirus/sonarless-mcp/internal/remote"
)

// writeRemotes writes remotes.json as `remote add` would (temp file + rename).
func writeRemotes(t *testing.T, dir string, rs []remote.Remote) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(rs)
	tmp := filepath.Join(dir, "remotes.json.tmp")
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, filepath.Join(dir, "remotes.json")); err != nil {
		t.Fatal(err)
	}
}

// slowSonar is fakeSonar whose tools/call of "slow" waits for release.
func slowSonar(t *testing.T, started chan<- struct{}, release <-chan struct{}) *httptest.Server {
	t.Helper()
	inner := fakeSonar(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), `"name":"slow"`) {
			started <- struct{}{}
			<-release
		}
		req, _ := http.NewRequestWithContext(r.Context(), r.Method, inner.URL+r.URL.Path, strings.NewReader(string(body)))
		req.Header = r.Header.Clone()
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		w.Header().Set("Content-Type", resp.Header.Get("Content-Type"))
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestGatewaySessionSeesRemoteAddedWhileRunning(t *testing.T) {
	started, release := make(chan struct{}, 1), make(chan struct{})
	srv, g, _ := newTestGateway(t, slowSonar(t, started, release), func(context.Context) error { return nil })
	var released sync.Once
	unblock := func() { released.Do(func() { close(release) }) }
	t.Cleanup(unblock) // a failure must not leave the slow handler (and srv.Close) hanging

	// Sessions build one backend per remotes.json entry, as shimBackends does.
	ci := fakeSonar(t)
	var ciCalls, builds atomic.Int64
	base := g.NewSession
	g.NewSession = func(ws string) (*Shim, error) {
		builds.Add(1)
		s, err := base(ws)
		if err != nil {
			return nil, err
		}
		rs, _ := remote.Load(g.Cfg)
		for _, r := range rs {
			s.Backends = append(s.Backends, &Backend{Name: r.Name, About: "remote " + r.URL, APIURL: r.URL,
				MCPURL: r.URL + "/mcp", Branch: r.Branch, Remote: true,
				Ensure: func(context.Context) error { ciCalls.Add(1); return nil },
				Token:  func(context.Context, bool) (string, error) { return "ci-token", nil }})
		}
		return s, nil
	}

	stream := openStream(t, srv)
	call := func(id int, name, server string) string {
		args := "{}"
		if server != "" {
			args = fmt.Sprintf(`{"server":%q}`, server)
		}
		_, body := post(t, srv, fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"tools/call","params":{"name":%q,"arguments":%s}}`,
			id, name, args), nil)
		return body
	}
	post(t, srv, `{"jsonrpc":"2.0","id":0,"method":"tools/list"}`, nil) // defaults follow the listed schemas
	if body := call(1, "show_rule", "work"); !strings.Contains(body, `unknown server \"work\"`) {
		t.Fatalf("before the remote exists: %s", body)
	}

	// A call is in flight on the local server when the remote is added.
	inflight := make(chan string, 1)
	go func() { inflight <- call(2, "slow", "") }()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("slow call never reached the server")
	}
	writeRemotes(t, g.Cfg.ConfigDir, []remote.Remote{{Name: "work", URL: ci.URL, Branch: "develop", Port: 1}})

	// The very next request routes to it: no TTL, no new session.
	if body := call(3, "get_component_measures", "work"); !strings.Contains(body, `\"branch\":\"develop\"`) {
		t.Errorf("next call after remote add: %s", body)
	}
	if ciCalls.Load() == 0 {
		t.Error("the new remote's backend was never started")
	}
	select {
	case d := <-stream:
		if !strings.Contains(d, "notifications/tools/list_changed") {
			t.Errorf("stream sent %s", d)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no tools/list_changed after the remotes changed")
	}
	_, list := post(t, srv, `{"jsonrpc":"2.0","id":4,"method":"tools/list"}`, nil)
	if !strings.Contains(list, `"enum":["local","work"]`) {
		t.Errorf("tools/list does not offer the new server: %s", list)
	}

	unblock()
	select {
	case body := <-inflight:
		if !strings.Contains(body, `"id":2`) || strings.Contains(body, "isError") {
			t.Errorf("in-flight call: %s", body)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("in-flight call never completed")
	}
	// One build for the session, one reload; unchanged file: no more.
	call(5, "show_rule", "")
	if n := builds.Load(); n != 2 {
		t.Errorf("sessions built %d times, want 2", n)
	}

	// Removing it takes effect the same way.
	if err := os.Remove(filepath.Join(g.Cfg.ConfigDir, "remotes.json")); err != nil {
		t.Fatal(err)
	}
	if body := call(6, "show_rule", "work"); !strings.Contains(body, `unknown server \"work\"`) {
		t.Errorf("after remote remove: %s", body)
	}
}

func TestGatewaySessionUsesTokenChangedOnItsOwn(t *testing.T) {
	srv, g, _ := newTestGateway(t, fakeSonar(t), func(context.Context) error { return nil })
	inner := fakeSonar(t)
	var seen atomic.Value // last bearer token the remote's MCP endpoint got
	ci := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/mcp" {
			seen.Store(r.Header.Get("Authorization"))
		}
		req, _ := http.NewRequestWithContext(r.Context(), r.Method, inner.URL+r.URL.Path, r.Body)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		w.Header().Set("Content-Type", resp.Header.Get("Content-Type"))
		_, _ = io.Copy(w, resp.Body)
	}))
	t.Cleanup(ci.Close)

	// Remote backends read their token file, as shimBackends does.
	base := g.NewSession
	g.NewSession = func(ws string) (*Shim, error) {
		s, err := base(ws)
		if err != nil {
			return nil, err
		}
		rs, _ := remote.Load(g.Cfg)
		for _, r := range rs {
			s.Backends = append(s.Backends, &Backend{Name: r.Name, APIURL: r.URL, MCPURL: r.URL + "/mcp", Remote: true,
				Ensure: func(context.Context) error { return nil },
				Token:  func(context.Context, bool) (string, error) { return remote.Token(g.Cfg, r.Name) }})
		}
		return s, nil
	}
	writeToken := func(tok string, at time.Time) {
		t.Helper()
		f := remote.TokenFile(g.Cfg, "work")
		if err := os.MkdirAll(filepath.Dir(f), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(f, []byte(tok), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(f, at, at); err != nil { // coarse-mtime filesystems
			t.Fatal(err)
		}
	}
	writeToken("token-aaaa", time.Now().Add(-time.Minute))
	writeRemotes(t, g.Cfg.ConfigDir, []remote.Remote{{Name: "work", URL: ci.URL, Port: 1}})
	call := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"show_rule","arguments":{"server":"work"}}}`

	post(t, srv, call, nil)
	if got, _ := seen.Load().(string); got != "Bearer token-aaaa" {
		t.Fatalf("first call sent %q", got)
	}
	// Only the token file changes (same size, remotes.json untouched).
	writeToken("token-bbbb", time.Now())
	post(t, srv, call, nil)
	if got, _ := seen.Load().(string); got != "Bearer token-bbbb" {
		t.Errorf("call after a token-only change sent %q, want the new token", got)
	}
}
