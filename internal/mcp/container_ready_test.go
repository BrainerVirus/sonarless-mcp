package mcp

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/BrainerVirus/sonarless-mcp/internal/config"
)

// fakeMCP mimics the SonarQube MCP server's HTTP filters: /health answers 200
// before authentication, a tokenless /mcp request gets 401.
func fakeMCP(t *testing.T, health bool) *Container {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" && health {
			w.WriteHeader(http.StatusOK)
			return
		}
		if r.URL.Path == "/mcp" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	return &Container{Port: portOf(t, srv.Listener.Addr())}
}

func portOf(t *testing.T, a net.Addr) int {
	_, p, err := net.SplitHostPort(a.String())
	if err != nil {
		t.Fatal(err)
	}
	n, _ := strconv.Atoi(p)
	return n
}

func TestAliveProbesHealth(t *testing.T) {
	if !fakeMCP(t, true).Alive(context.Background()) {
		t.Error("a server answering 200 on /health is not alive")
	}
	// Another program on the port answers HTTP, but not the MCP server's
	// /health: the old "any answer on /mcp" probe took it for the server.
	if fakeMCP(t, false).Alive(context.Background()) {
		t.Error("a server without /health counts as alive")
	}
}

func TestAliveFalseWhenNothingListens(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := portOf(t, l.Addr())
	l.Close()
	if (&Container{Port: port}).Alive(context.Background()) {
		t.Error("a closed port counts as alive")
	}
}

func TestToolsetsEnv(t *testing.T) {
	for _, tc := range []struct{ value, want string }{
		{config.DefaultMCPToolsets, "SONARQUBE_TOOLSETS=" + config.DefaultMCPToolsets},
		{"issues,projects", "SONARQUBE_TOOLSETS=issues,projects"},
		{"default", ""},
		{" Default ", ""},
		{"", ""},
	} {
		if got := toolsetsEnv(tc.value); got != tc.want {
			t.Errorf("%q -> %q, want %q", tc.value, got, tc.want)
		}
	}
}
