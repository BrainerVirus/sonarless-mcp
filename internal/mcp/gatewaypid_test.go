package mcp

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/BrainerVirus/sonarless-mcp/internal/config"
)

func pidCfg(t *testing.T) *config.Config {
	t.Helper()
	t.Setenv("SONARLESS_HOME", t.TempDir())
	cfg, _, err := config.Load("")
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

// fakeProcesses replaces the process lookup and kill with a table.
func fakeProcesses(t *testing.T, cmdlines map[int]string) *[]int {
	t.Helper()
	var killed []int
	oc, ot := processCommand, terminate
	processCommand = func(pid int) string { return cmdlines[pid] }
	terminate = func(pid int) error { killed = append(killed, pid); return nil }
	t.Cleanup(func() { processCommand, terminate = oc, ot })
	return &killed
}

func recordPID(t *testing.T, cfg *config.Config, p gatewayPID) {
	t.Helper()
	b, _ := json.Marshal(p)
	if err := os.MkdirAll(cfg.CacheDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(gatewayPIDFile(cfg), b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestStopGateway(t *testing.T) {
	cases := []struct {
		name, cmdline, exe string
		stop               bool
	}{
		{"running gateway", "/opt/bin/sonarless-mcp serve", "/opt/bin/sonarless-mcp", true},
		{"windows gateway", `"C:\Program Files\sonarless-mcp\Sonarless-MCP.exe" serve`, `C:\Program Files\sonarless-mcp\sonarless-mcp.exe`, true},
		{"pid reused by another program", "/usr/bin/vim notes.txt", "/opt/bin/sonarless-mcp", false},
		{"pid reused by a stdio shim", "/opt/bin/sonarless-mcp mcp", "/opt/bin/sonarless-mcp", false},
		{"process gone", "", "/opt/bin/sonarless-mcp", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := pidCfg(t)
			killed := fakeProcesses(t, map[int]string{4242: tc.cmdline})
			recordPID(t, cfg, gatewayPID{PID: 4242, Exe: tc.exe, Started: time.Now()})
			stopped, err := StopGateway(cfg)
			if err != nil || stopped != tc.stop || (len(*killed) == 1) != tc.stop {
				t.Errorf("stopped=%v err=%v killed=%v, want stop=%v", stopped, err, *killed, tc.stop)
			}
			if _, err := os.Stat(gatewayPIDFile(cfg)); !os.IsNotExist(err) {
				t.Error("pid file left behind")
			}
		})
	}
	t.Run("nothing recorded", func(t *testing.T) {
		cfg := pidCfg(t)
		killed := fakeProcesses(t, nil)
		if stopped, err := StopGateway(cfg); stopped || err != nil || len(*killed) > 0 {
			t.Errorf("stopped=%v err=%v killed=%v", stopped, err, *killed)
		}
	})
}

func TestServeRecordsAndClearsItsPID(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	t.Setenv(config.GatewayPort, strconv.Itoa(port))
	cfg := pidCfg(t)
	g := &Gateway{Cfg: cfg, Token: gwToken, NewSession: func(string) (*Shim, error) { return nil, nil }}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- g.Serve(ctx) }()
	deadline := time.Now().Add(5 * time.Second)
	for !GatewayAlive(context.Background(), cfg) {
		if time.Now().After(deadline) {
			t.Fatal("gateway did not come up")
		}
		time.Sleep(20 * time.Millisecond)
	}
	p, ok := readGatewayPID(cfg)
	if !ok || p.PID != os.Getpid() || p.Exe == "" {
		t.Errorf("pid file %+v, %v", p, ok)
	}
	cancel()
	if err := <-done; err != nil {
		t.Errorf("Serve: %v", err)
	}
	if _, ok := readGatewayPID(cfg); ok {
		t.Error("pid file kept after the gateway stopped")
	}
}

func TestProcessCommandSeesThisProcess(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("PowerShell lookup; covered by the fakes above")
	}
	exe, _ := os.Executable()
	if got := processCommand(os.Getpid()); !strings.Contains(got, filepath.Base(exe)) {
		t.Errorf("processCommand(self) = %q, want it to name %s", got, filepath.Base(exe))
	}
}
