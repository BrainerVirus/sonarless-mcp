package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/BrainerVirus/sonarless-mcp/internal/config"
	"github.com/BrainerVirus/sonarless-mcp/internal/project"
	"github.com/BrainerVirus/sonarless-mcp/internal/scan"
	"github.com/BrainerVirus/sonarless-mcp/internal/sonar"
)

// runScanCommand runs `sonarless-mcp scan <dir> args...` with the scan
// itself faked to end with res (or err), and returns the exit status main
// would use.
func runScanCommand(t *testing.T, dir string, res scan.Result, scanErr error, args ...string) int {
	t.Helper()
	orig := runScan
	runScan = func(context.Context, *config.Config, *sonar.Server, *project.Project, scan.Options) (scan.Result, error) {
		return res, scanErr
	}
	t.Cleanup(func() { runScan = orig })
	root := newRootCommand(context.Background())
	root.SetArgs(append([]string{"scan", dir}, args...))
	_, err := root.ExecuteC()
	return exitCode(err)
}

func scanCmdEnv(t *testing.T) string {
	t.Helper()
	t.Setenv("SONARLESS_HOME", t.TempDir())
	t.Setenv(config.AutoUpdate, "false")
	t.Setenv(config.FailOnGate, "")
	return t.TempDir()
}

func TestScanExitStatusFollowsTheGateOnlyWhenAsked(t *testing.T) {
	failing := scan.Result{Gate: scan.GateFailed}
	cases := []struct {
		name string
		env  string
		res  scan.Result
		args []string
		want int
	}{
		{"failing gate, no flag", "", failing, nil, 0},
		{"failing gate, --fail-on-gate", "", failing, []string{"--fail-on-gate"}, exitGateFailed},
		{"failing gate, env", "true", failing, nil, exitGateFailed},
		{"failing gate, env overridden by the flag", "true", failing, []string{"--fail-on-gate=false"}, 0},
		{"passing gate, --fail-on-gate", "", scan.Result{Gate: "OK"}, []string{"--fail-on-gate"}, 0},
		{"no gate, --fail-on-gate", "", scan.Result{Gate: "NONE"}, []string{"--fail-on-gate"}, 0},
		{"unreadable gate, --fail-on-gate", "", scan.Result{GateErr: errors.New("connection refused")}, []string{"--fail-on-gate"}, exitError},
		{"unreadable gate, no flag", "", scan.Result{GateErr: errors.New("connection refused")}, nil, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := scanCmdEnv(t)
			t.Setenv(config.FailOnGate, c.env)
			if got := runScanCommand(t, dir, c.res, nil, c.args...); got != c.want {
				t.Errorf("exit status %d, want %d", got, c.want)
			}
		})
	}
}

func TestScanFailOnGateFromProjectFile(t *testing.T) {
	dir := scanCmdEnv(t)
	if err := os.WriteFile(filepath.Join(dir, ".sonarless.env"), []byte(config.FailOnGate+"=true\r\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := runScanCommand(t, dir, scan.Result{Gate: scan.GateFailed}, nil); got != exitGateFailed {
		t.Errorf("exit status %d, want %d", got, exitGateFailed)
	}
}

func TestScanErrorExitsOneEvenWithFailOnGate(t *testing.T) {
	dir := scanCmdEnv(t)
	if got := runScanCommand(t, dir, scan.Result{}, errors.New("cli scan failed"), "--fail-on-gate"); got != exitError {
		t.Errorf("exit status %d, want %d", got, exitError)
	}
}
