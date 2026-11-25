package scan

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/BrainerVirus/sonarless-mcp/internal/config"
	"github.com/BrainerVirus/sonarless-mcp/internal/idle"
	"github.com/BrainerVirus/sonarless-mcp/internal/project"
	"github.com/BrainerVirus/sonarless-mcp/internal/sonar"
)

func scanEnv(t *testing.T) (*config.Config, *project.Project) {
	t.Helper()
	t.Setenv("SONARLESS_HOME", t.TempDir())
	t.Setenv("SONARLESS_INSTANCE", "sltest")
	t.Setenv("SONARLESS_IDLE_TIMEOUT", "0")
	cfg, _, err := config.Load("")
	if err != nil {
		t.Fatal(err)
	}
	return cfg, &project.Project{Root: t.TempDir(), Key: "app", Name: "app", Build: project.BuildCLI}
}

func fakePrepare(t *testing.T, f func(context.Context, *config.Config, *sonar.Server, *project.Project) (string, error)) {
	t.Helper()
	orig := prepare
	prepare = f
	t.Cleanup(func() { prepare = orig })
}

func TestScanRecordedWhileWaitingForTheServer(t *testing.T) {
	cfg, p := scanEnv(t)
	boom := errors.New("server did not start")
	var seen []idle.Proc
	fakePrepare(t, func(ctx context.Context, cfg *config.Config, _ *sonar.Server, _ *project.Project) (string, error) {
		seen = idle.Running(cfg, idle.KindScan)
		return "", boom
	})
	err := Run(context.Background(), cfg, &sonar.Server{Cfg: cfg, Log: io.Discard}, p, Options{Out: io.Discard})
	if !errors.Is(err, boom) {
		t.Fatalf("err %v", err)
	}
	if len(seen) != 1 || seen[0].State != idle.ScanWaiting || seen[0].Project != "app" || seen[0].Scanner != "cli" || seen[0].PID != os.Getpid() {
		t.Errorf("while waiting for the server, status saw %+v", seen)
	}
	if left := idle.Running(cfg, idle.KindScan); len(left) != 0 {
		t.Errorf("failed scan still recorded: %+v", left)
	}
}

func TestScanRecordedAsScanningUntilItEnds(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-script fake docker")
	}
	cfg, p := scanEnv(t)
	fakePrepare(t, func(context.Context, *config.Config, *sonar.Server, *project.Project) (string, error) {
		return "tok", nil
	})
	// The scanner container: a fake docker that copies the scan records
	// while it "runs", then fails.
	dir := t.TempDir()
	seen := filepath.Join(dir, "seen")
	script := "#!/bin/sh\ncat '" + filepath.Join(cfg.CacheDir, "procs") + "'/scan-*.json > '" + seen + "'\nexit 1\n"
	if err := os.WriteFile(filepath.Join(dir, "docker"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	err := Run(context.Background(), cfg, &sonar.Server{Cfg: cfg, Log: io.Discard}, p, Options{Out: io.Discard})
	if err == nil || !strings.Contains(err.Error(), "cli scan failed") {
		t.Fatalf("err %v", err)
	}
	b, _ := os.ReadFile(seen)
	if !strings.Contains(string(b), `"state":"scanning"`) {
		t.Errorf("record while the scanner ran: %s", b)
	}
	if left, _ := filepath.Glob(filepath.Join(cfg.CacheDir, "procs", "scan-*")); len(left) != 0 {
		t.Errorf("scan records left after it ended: %v", left)
	}
}

// fakeSonar serves the two calls a scan makes after the scanner ends: the
// pending report tasks (none) and the quality gate, which answers gateStatus,
// or HTTP 500 when gateStatus is "".
func fakeSonar(t *testing.T, gateStatus string) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/ce/component":
			_, _ = io.WriteString(w, `{"queue":[]}`)
		case "/api/qualitygates/project_status":
			if gateStatus == "" {
				http.Error(w, `{"errors":[{"msg":"boom"}]}`, http.StatusInternalServerError)
				return
			}
			_, _ = io.WriteString(w, `{"projectStatus":{"status":"`+gateStatus+`"}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("SONARLESS_SERVER_PORT", u.Port())
}

// fakeScanner puts a docker on PATH that succeeds, standing in for the
// scanner container.
func fakeScanner(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "docker"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestRunGateReturnsTheGate(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-script fake docker")
	}
	for _, status := range []string{"OK", GateFailed, "NONE"} {
		t.Run(status, func(t *testing.T) {
			fakeSonar(t, status)
			cfg, p := scanEnv(t)
			fakePrepare(t, func(context.Context, *config.Config, *sonar.Server, *project.Project) (string, error) {
				return "tok", nil
			})
			fakeScanner(t)
			var out strings.Builder
			res, err := RunGate(context.Background(), cfg, &sonar.Server{Cfg: cfg, Log: io.Discard}, p, Options{Out: &out})
			if err != nil {
				t.Fatalf("a %s gate is a result, not an error: %v", status, err)
			}
			if res.Gate != status || res.GateErr != nil {
				t.Errorf("result %+v, want gate %s", res, status)
			}
			if !strings.Contains(out.String(), "Quality gate: "+status) {
				t.Errorf("output does not report the gate:\n%s", out.String())
			}
		})
	}
}

func TestRunGateReportsAnUnreadableGate(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-script fake docker")
	}
	fakeSonar(t, "")
	cfg, p := scanEnv(t)
	fakePrepare(t, func(context.Context, *config.Config, *sonar.Server, *project.Project) (string, error) {
		return "tok", nil
	})
	fakeScanner(t)
	var out strings.Builder
	res, err := RunGate(context.Background(), cfg, &sonar.Server{Cfg: cfg, Log: io.Discard}, p, Options{Out: &out})
	if err != nil {
		t.Fatalf("an unreadable gate does not fail the scan: %v", err)
	}
	if res.Gate != "" || res.GateErr == nil {
		t.Errorf("result %+v, want no gate and its error", res)
	}
	if !strings.Contains(out.String(), "Quality gate: unknown (") {
		t.Errorf("output does not say the gate is unknown:\n%s", out.String())
	}
}
