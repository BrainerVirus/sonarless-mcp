package mcp

import (
	"context"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/BrainerVirus/sonarless-mcp/internal/config"
)

// statusCfg loads a config whose gateway port is srv's, with the test
// token in the token file.
func statusCfg(t *testing.T, srvURL string, withToken bool) *config.Config {
	t.Helper()
	u, _ := url.Parse(srvURL)
	t.Setenv("SONARLESS_GATEWAY_PORT", u.Port())
	cfg, _, err := config.Load("")
	if err != nil {
		t.Fatal(err)
	}
	if withToken {
		if err := os.MkdirAll(filepath.Dir(cfg.GatewayTokenFile()), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(cfg.GatewayTokenFile(), []byte(gwToken), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return cfg
}

func TestGatewayStatusListsSessions(t *testing.T) {
	srv, _, _ := newTestGateway(t, fakeSonar(t), func(context.Context) error { return nil })
	ws := t.TempDir()
	before := time.Now()
	list := `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`
	post(t, srv, list, map[string]string{WorkspaceHeader: ws})
	post(t, srv, list, nil)

	st := GatewayStatus(context.Background(), statusCfg(t, srv.URL, true))
	if !st.Running || st.PID != os.Getpid() || st.Version != "test" || st.Uptime == "" {
		t.Fatalf("gateway state: %+v", st)
	}
	if len(st.Sessions) != 2 {
		t.Fatalf("sessions: %+v", st.Sessions)
	}
	root := ""
	for _, s := range st.Sessions {
		if s.Workspace != "" {
			root = s.Workspace
		}
		if s.LastRequest.Before(before.Add(-time.Second)) {
			t.Errorf("session %q last request %s, before the requests", s.Workspace, s.LastRequest)
		}
	}
	if want := workspaceKey(ws); root != want {
		t.Errorf("workspace session %q, want %q", root, want)
	}
	if st.Note != "" {
		t.Errorf("note %q with a working /status", st.Note)
	}
}

func TestGatewayStatusEndpointNeedsToken(t *testing.T) {
	srv, _, _ := newTestGateway(t, fakeSonar(t), func(context.Context) error { return nil })
	for _, hdr := range []string{"", "Bearer nope"} {
		req, _ := http.NewRequest(http.MethodGet, srv.URL+statusPath, nil)
		if hdr != "" {
			req.Header.Set("Authorization", hdr)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("Authorization %q: %d, want 401", hdr, resp.StatusCode)
		}
	}
	// No token file: still running (health answers), sessions unknown, and
	// status never creates the token.
	cfg := statusCfg(t, srv.URL, false)
	st := GatewayStatus(context.Background(), cfg)
	if !st.Running || st.Note == "" || len(st.Sessions) != 0 {
		t.Errorf("without a token: %+v", st)
	}
	if _, err := os.Stat(cfg.GatewayTokenFile()); !os.IsNotExist(err) {
		t.Errorf("status created the token file (%v)", err)
	}
}

func TestGatewayStatusNotRunning(t *testing.T) {
	t.Setenv("SONARLESS_HOME", t.TempDir())
	ln, err := net.Listen("tcp", "127.0.0.1:0") // a port nobody serves once closed
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	cfg := statusCfg(t, "http://127.0.0.1:"+strconv.Itoa(port), true)

	fakeProcesses(t, map[int]string{4242: "/usr/bin/vim notes.txt"})
	recordPID(t, cfg, gatewayPID{PID: 4242, Exe: "/opt/bin/sonarless-mcp", Started: time.Now()})
	if st := GatewayStatus(context.Background(), cfg); st.Running || st.PID != 0 || st.Port != port {
		t.Errorf("pid reused by another program: %+v", st)
	}

	fakeProcesses(t, map[int]string{4242: "/opt/bin/sonarless-mcp serve"})
	started := time.Now().Add(-time.Hour)
	recordPID(t, cfg, gatewayPID{PID: 4242, Exe: "/opt/bin/sonarless-mcp", Started: started})
	st := GatewayStatus(context.Background(), cfg)
	if !st.Running || st.PID != 4242 || st.Uptime == "" || st.Note == "" {
		t.Errorf("gateway process that doesn't answer: %+v", st)
	}
}
