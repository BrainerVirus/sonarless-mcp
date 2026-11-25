package update_test

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sigstore/sigstore-go/pkg/root"

	"github.com/BrainerVirus/sonarless-mcp/internal/update"
	"github.com/BrainerVirus/sonarless-mcp/internal/update/updatetest"
)

func TestNewer(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want bool
	}{
		{"v0.2.0", "0.1.9", true}, {"v1.10.0", "1.9.0", true}, {"v0.1.0", "0.1.0", false},
		{"v0.1.0", "0.2.0", false}, {"1.2.0", "1.2.0-rc.1", true}, {"1.2.0-rc.1", "1.2.0", false},
		{"v1.0.0", "dev", false}, {"1.2", "1.1.0", false},
	} {
		if got := update.Newer(tc.a, tc.b); got != tc.want {
			t.Errorf("Newer(%q, %q) = %v, want %v", tc.a, tc.b, got, tc.want)
		}
	}
}

var now = time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)

// fixture is an installed binary of version current, a fake GitHub with
// releases, and an Upgrader wired to both.
type fixture struct {
	srv   *updatetest.Server
	exe   string
	state string
	u     *update.Upgrader
	log   strings.Builder
}

func newFixture(t *testing.T, current string, releases ...*updatetest.Release) *fixture {
	t.Helper()
	s := updatetest.NewSigstore(t)
	fx := &fixture{srv: updatetest.NewServer(t, s, releases...)}
	// Upgrade resolves symlinks in the binary's path; macOS temp dirs sit
	// behind /var -> /private/var, so compare against the resolved path.
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	fx.exe = filepath.Join(dir, "bin", "sonarless-mcp")
	if err := os.MkdirAll(filepath.Dir(fx.exe), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fx.exe, []byte("installed "+current), 0o755); err != nil {
		t.Fatal(err)
	}
	fx.state = update.StatePath(filepath.Join(dir, "cache"))
	fx.u = &update.Upgrader{
		Current:     current,
		HTTP:        fx.srv.Client(),
		APIURL:      fx.srv.URL,
		TrustedRoot: func() (root.TrustedMaterial, error) { return s.VS, nil },
		NoSCT:       true,
		Executable:  func() (string, error) { return fx.exe, nil },
		GOOS:        "linux",
		GOARCH:      "amd64",
		Run:         updatetest.Runner,
		Now:         func() time.Time { return now },
		StatePath:   fx.state,
		Log:         &fx.log,
		Sleep:       func(context.Context, time.Duration) error { return nil },
	}
	return fx
}

func (fx *fixture) installed(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(fx.exe)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func (fx *fixture) assertUnchanged(t *testing.T, current string) {
	t.Helper()
	if got := fx.installed(t); got != "installed "+current {
		t.Errorf("binary changed: %q", got)
	}
	entries, _ := os.ReadDir(filepath.Dir(fx.exe))
	if len(entries) != 1 {
		t.Errorf("leftover files next to the binary: %v", entries)
	}
}

func TestUpgradeInstallsTheLatestSignedRelease(t *testing.T) {
	fx := newFixture(t, "1.0.0", &updatetest.Release{Version: "1.0.0"}, &updatetest.Release{Version: "1.1.0"})
	res, err := fx.u.Upgrade(context.Background(), "", false)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Updated || res.Unsigned || res.Latest != "1.1.0" || res.Path != fx.exe {
		t.Errorf("result = %+v", res)
	}
	if got := fx.installed(t); !strings.Contains(got, "1.1.0 linux/amd64") {
		t.Errorf("binary = %q", got)
	}
	// Windows has no exec bit, and it moves the running binary aside to
	// sonarless-mcp.old (removed by the next upgrade) instead of replacing it.
	want := []string{"sonarless-mcp"}
	if runtime.GOOS == "windows" {
		want = append(want, "sonarless-mcp"+update.OldSuffix)
	} else if fi, _ := os.Stat(fx.exe); fi.Mode().Perm()&0o100 == 0 {
		t.Error("the new binary is not executable")
	}
	entries, _ := os.ReadDir(filepath.Dir(fx.exe))
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if !slices.Equal(names, want) {
		t.Errorf("files next to the binary = %v, want %v", names, want)
	}
}

func TestUpgradeIsUpToDate(t *testing.T) {
	fx := newFixture(t, "1.1.0", &updatetest.Release{Version: "1.1.0"})
	res, err := fx.u.Upgrade(context.Background(), "", false)
	if err != nil || res.Updated || res.Newer {
		t.Fatalf("res = %+v, err = %v", res, err)
	}
	if d := fx.srv.Downloads(); len(d) != 0 {
		t.Errorf("downloaded %v", d)
	}
	fx.assertUnchanged(t, "1.1.0")
}

func TestUpgradeRefuses(t *testing.T) {
	for _, tc := range []struct {
		name string
		rel  updatetest.Release
		want string
	}{
		{"given a newer release without a signature", updatetest.Release{Unsigned: true}, "must be signed"},
		{"given a signature by another repository's workflow",
			updatetest.Release{Subject: "https://github.com/evil/sonarless-mcp/.github/workflows/release.yml@refs/heads/main"}, "signature verification failed"},
		{"given a signature by another workflow of this repository",
			updatetest.Release{Subject: "https://github.com/BrainerVirus/sonarless-mcp/.github/workflows/ci.yml@refs/heads/main"}, "signature verification failed"},
		{"given a signature from a branch other than main",
			updatetest.Release{Subject: "https://github.com/BrainerVirus/sonarless-mcp/.github/workflows/release.yml@refs/heads/feature"}, "signature verification failed"},
		{"given a signature from another OIDC issuer", updatetest.Release{Issuer: "https://accounts.google.com"}, "signature verification failed"},
		{"given a signature over other bytes", updatetest.Release{SignOther: true}, "signature verification failed"},
		{"given an archive that does not match its checksum", updatetest.Release{WrongChecksums: true}, "checksum mismatch"},
		{"given a binary that does not report its version",
			updatetest.Release{Binary: func(string) []byte { return []byte("sonarless-mcp version 0.0.1") }}, "does not name 1.1.0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rel := tc.rel
			rel.Version = "1.1.0"
			fx := newFixture(t, "1.0.0", &rel)
			res, err := fx.u.Upgrade(context.Background(), "", false)
			if err == nil {
				t.Fatalf("upgraded: %+v", res)
			}
			if !errors.Is(err, update.ErrRefused) || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), "was not changed") {
				t.Errorf("err = %v", err)
			}
			fx.assertUnchanged(t, "1.0.0")
		})
	}
}

func TestUpgradeRefusesBeforeDownloadingAnUnsignedArchive(t *testing.T) {
	fx := newFixture(t, "1.0.0", &updatetest.Release{Version: "1.1.0", Unsigned: true})
	_, err := fx.u.Upgrade(context.Background(), "", false)
	if !errors.Is(err, update.ErrUnsigned) {
		t.Fatalf("err = %v", err)
	}
	if d := fx.srv.Downloads(); len(d) != 0 {
		t.Errorf("downloaded %v", d)
	}
}

func TestAllowUnsigned(t *testing.T) {
	legacy := func(string) []byte { return []byte("legacy sonarless-mcp version 0.8.1") }
	releases := func() []*updatetest.Release {
		return []*updatetest.Release{
			{Version: "0.8.1", Unsigned: true, Binary: legacy},
			{Version: "0.9.0", SignOther: true},
			{Version: "1.0.0"},
			{Version: "1.1.0", Unsigned: true},
		}
	}

	t.Run("given an older unsigned release without the flag, it refuses and names the flag", func(t *testing.T) {
		fx := newFixture(t, "1.0.0", releases()...)
		_, err := fx.u.Upgrade(context.Background(), "v0.8.1", false)
		if !errors.Is(err, update.ErrRefused) || !strings.Contains(err.Error(), "--version v0.8.1 --allow-unsigned") {
			t.Fatalf("err = %v", err)
		}
		fx.assertUnchanged(t, "1.0.0")
	})

	t.Run("given an older unsigned release with the flag, it installs it on its checksum", func(t *testing.T) {
		fx := newFixture(t, "1.0.0", releases()...)
		res, err := fx.u.Upgrade(context.Background(), "v0.8.1", true)
		if err != nil {
			t.Fatal(err)
		}
		if !res.Updated || !res.Unsigned {
			t.Errorf("res = %+v", res)
		}
		if got := fx.installed(t); !strings.Contains(got, "0.8.1") {
			t.Errorf("binary = %q", got)
		}
	})

	t.Run("given an older release whose signature is invalid, the flag does not skip it", func(t *testing.T) {
		fx := newFixture(t, "1.0.0", releases()...)
		_, err := fx.u.Upgrade(context.Background(), "v0.9.0", true)
		if !errors.Is(err, update.ErrRefused) || !strings.Contains(err.Error(), "signature verification failed") {
			t.Fatalf("err = %v", err)
		}
		fx.assertUnchanged(t, "1.0.0")
	})

	t.Run("given a newer unsigned release, the flag is refused", func(t *testing.T) {
		fx := newFixture(t, "1.0.0", releases()...)
		_, err := fx.u.Upgrade(context.Background(), "v1.1.0", true)
		if err == nil || !strings.Contains(err.Error(), "only installs a release older") {
			t.Fatalf("err = %v", err)
		}
		fx.assertUnchanged(t, "1.0.0")
	})

	t.Run("given no --version, the flag is refused before any request", func(t *testing.T) {
		fx := newFixture(t, "1.0.0", releases()...)
		_, err := fx.u.Upgrade(context.Background(), "", true)
		if err == nil || !strings.Contains(err.Error(), "needs --version") {
			t.Fatalf("err = %v", err)
		}
		if r := fx.srv.Requests(); len(r) != 0 {
			t.Errorf("requests = %v", r)
		}
	})
}

func TestUpgradeToANamedRelease(t *testing.T) {
	t.Run("given an older signed release, it installs it", func(t *testing.T) {
		fx := newFixture(t, "1.1.0", &updatetest.Release{Version: "1.0.0"}, &updatetest.Release{Version: "1.1.0"})
		res, err := fx.u.Upgrade(context.Background(), "1.0.0", false)
		if err != nil || !res.Updated {
			t.Fatalf("res = %+v, err = %v", res, err)
		}
	})
	t.Run("given the installed release, it changes nothing", func(t *testing.T) {
		fx := newFixture(t, "1.1.0", &updatetest.Release{Version: "1.1.0"})
		res, err := fx.u.Upgrade(context.Background(), "v1.1.0", false)
		if err != nil || res.Updated {
			t.Fatalf("res = %+v, err = %v", res, err)
		}
		fx.assertUnchanged(t, "1.1.0")
	})
	t.Run("given a release that does not exist, it says so", func(t *testing.T) {
		fx := newFixture(t, "1.1.0", &updatetest.Release{Version: "1.1.0"})
		_, err := fx.u.Upgrade(context.Background(), "v9.0.0", false)
		if err == nil || err.Error() != "release v9.0.0 not found" {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestCheckChangesNothing(t *testing.T) {
	fx := newFixture(t, "1.0.0", &updatetest.Release{Version: "1.1.0"})
	res, err := fx.u.Check(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if !res.Newer || res.Current != "1.0.0" || res.Latest != "1.1.0" || res.Updated {
		t.Errorf("res = %+v", res)
	}
	if d := fx.srv.Downloads(); len(d) != 0 {
		t.Errorf("downloaded %v", d)
	}
	if _, err := os.Stat(fx.state); !os.IsNotExist(err) {
		t.Errorf("state written: %v", err)
	}
	fx.assertUnchanged(t, "1.0.0")
}

func TestBackground(t *testing.T) {
	t.Run("given a newer release younger than a day, it waits and installs nothing", func(t *testing.T) {
		fx := newFixture(t, "1.0.0", &updatetest.Release{Version: "1.1.0", PublishedAt: now.Add(-23 * time.Hour)})
		res, err := fx.u.Background(context.Background())
		if err != nil || !res.Waiting || res.Updated {
			t.Fatalf("res = %+v, err = %v", res, err)
		}
		if d := fx.srv.Downloads(); len(d) != 0 {
			t.Errorf("downloaded %v", d)
		}
		fx.assertUnchanged(t, "1.0.0")
		if s := update.ReadState(fx.state); s.LatestVersion != "1.1.0" || !s.CheckedAt.Equal(now) {
			t.Errorf("state = %+v", s)
		}
	})

	t.Run("given a newer release a day old, it installs it and records it for the notice", func(t *testing.T) {
		fx := newFixture(t, "1.0.0", &updatetest.Release{Version: "1.1.0", PublishedAt: now.Add(-25 * time.Hour)})
		res, err := fx.u.Background(context.Background())
		if err != nil || !res.Updated {
			t.Fatalf("res = %+v, err = %v", res, err)
		}
		if a := update.ReadState(fx.state).Auto; a.From != "1.0.0" || a.To != "1.1.0" || a.Announced {
			t.Errorf("auto = %+v", a)
		}
	})

	t.Run("given a refused release, it records the failure and backs off", func(t *testing.T) {
		fx := newFixture(t, "1.0.0", &updatetest.Release{Version: "1.1.0", Unsigned: true})
		if _, err := fx.u.Background(context.Background()); !errors.Is(err, update.ErrRefused) {
			t.Fatalf("err = %v", err)
		}
		a := update.ReadState(fx.state).Auto
		if a.Failures != 1 || a.FailedVersion != "1.1.0" || !a.RetryAt.Equal(now.Add(24*time.Hour)) {
			t.Errorf("auto = %+v", a)
		}
		before := len(fx.srv.Requests())
		res, err := fx.u.Background(context.Background())
		if err != nil || !res.BackingOff {
			t.Fatalf("res = %+v, err = %v", res, err)
		}
		if got := fx.srv.Requests()[before:]; len(got) != 1 {
			t.Errorf("requests while backing off = %v, want only the release check", got)
		}
		fx.assertUnchanged(t, "1.0.0")
	})

	t.Run("given a prerelease only, it is up to date", func(t *testing.T) {
		fx := newFixture(t, "1.0.0", &updatetest.Release{Version: "1.0.0"}, &updatetest.Release{Version: "1.1.0-rc.1", Prerelease: true})
		res, err := fx.u.Background(context.Background())
		if err != nil || res.Newer || res.Updated {
			t.Fatalf("res = %+v, err = %v", res, err)
		}
	})
}

func TestRunDue(t *testing.T) {
	soaked, fresh := now.Add(-25*time.Hour), now.Add(-time.Hour)
	for _, tc := range []struct {
		name  string
		state update.State
		want  bool
	}{
		{"never started", update.State{}, true},
		{"started a day ago", update.State{Auto: update.AutoState{StartedAt: now.Add(-24 * time.Hour)}}, true},
		{"started 2h ago, nothing newer", update.State{Auto: update.AutoState{StartedAt: now.Add(-2 * time.Hour)}}, false},
		{"started 2h ago, a newer release has soaked",
			update.State{LatestVersion: "1.1.0", LatestPublishedAt: soaked, Auto: update.AutoState{StartedAt: now.Add(-2 * time.Hour)}}, true},
		{"started 2h ago, a newer release is fresh",
			update.State{LatestVersion: "1.1.0", LatestPublishedAt: fresh, Auto: update.AutoState{StartedAt: now.Add(-2 * time.Hour)}}, false},
		{"started 10m ago, a newer release has soaked",
			update.State{LatestVersion: "1.1.0", LatestPublishedAt: soaked, Auto: update.AutoState{StartedAt: now.Add(-10 * time.Minute)}}, false},
		{"started 2h ago, soaked but backing off",
			update.State{LatestVersion: "1.1.0", LatestPublishedAt: soaked, Auto: update.AutoState{StartedAt: now.Add(-2 * time.Hour), RetryAt: now.Add(time.Hour)}}, false},
		{"started in the future (clock change)", update.State{Auto: update.AutoState{StartedAt: now.Add(time.Hour)}}, true},
	} {
		if got := tc.state.RunDue("1.0.0", now); got != tc.want {
			t.Errorf("%s: RunDue = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestPendingAtMostOncePerDay(t *testing.T) {
	path := update.StatePath(t.TempDir())
	if n := update.Pending(path, "1.0.0", now); n != nil {
		t.Fatalf("no state: %+v", n)
	}
	update.UpdateState(path, func(s *update.State) { s.LatestVersion, s.LatestURL = "1.1.0", "https://example.test/r" })
	n := update.Pending(path, "1.0.0", now)
	if n == nil || n.Latest != "1.1.0" || n.Current != "1.0.0" {
		t.Fatalf("pending = %+v", n)
	}
	update.MarkNotified(path, "1.1.0", now)
	if n := update.Pending(path, "1.0.0", now.Add(23*time.Hour)); n != nil {
		t.Errorf("announced again within a day: %+v", n)
	}
	if n := update.Pending(path, "1.0.0", now.Add(24*time.Hour)); n == nil {
		t.Error("not announced again after a day")
	}
	if n := update.Pending(path, "1.1.0", now.Add(48*time.Hour)); n != nil {
		t.Errorf("announced the running version: %+v", n)
	}
}

func TestCheckDaily(t *testing.T) {
	fx := newFixture(t, "1.0.0", &updatetest.Release{Version: "1.1.0"})
	c := &update.Client{HTTP: fx.srv.Client(), APIURL: fx.srv.URL}
	if err := update.CheckDaily(context.Background(), c, fx.state, now); err != nil {
		t.Fatal(err)
	}
	if err := update.CheckDaily(context.Background(), c, fx.state, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if got := fx.srv.Requests(); len(got) != 1 {
		t.Errorf("requests = %v, want one check a day", got)
	}
	if s := update.ReadState(fx.state); s.LatestVersion != "1.1.0" {
		t.Errorf("state = %+v", s)
	}
}

func TestSmokeTestFallsBackToVersionFlag(t *testing.T) {
	var calls [][]string
	run := func(_ context.Context, _ string, args ...string) ([]byte, error) {
		calls = append(calls, args)
		if args[0] == "version" {
			return []byte(`Error: unknown command "version"`), errors.New("exit status 1")
		}
		return []byte("sonarless-mcp version 0.8.1\n"), nil
	}
	if err := update.SmokeTest(context.Background(), run, "x", "0.8.1"); err != nil {
		t.Fatal(err)
	}
	if !slices.EqualFunc(calls, [][]string{{"version"}, {"--version"}}, slices.Equal) {
		t.Errorf("calls = %v", calls)
	}
}

func TestDownloadRefusesARedirectOffHTTPS(t *testing.T) {
	fx := newFixture(t, "1.0.0", &updatetest.Release{Version: "1.1.0"})
	fx.srv.Handle(func(w http.ResponseWriter, r *http.Request) bool {
		if strings.HasSuffix(r.URL.Path, "/"+update.ChecksumsName) {
			http.Redirect(w, r, "http://example.invalid/checksums.txt", http.StatusFound)
			return true
		}
		return false
	})
	_, err := fx.u.Upgrade(context.Background(), "", false)
	if !errors.Is(err, update.ErrRefused) || !strings.Contains(err.Error(), "off https") {
		t.Fatalf("err = %v", err)
	}
	fx.assertUnchanged(t, "1.0.0")
}

func TestDownloadRetriesThenFallsBackToTheAPI(t *testing.T) {
	fx := newFixture(t, "1.0.0", &updatetest.Release{Version: "1.1.0"})
	archive := updatetest.DownloadPath("v1.1.0", update.ArchiveName("linux", "amd64"))
	fx.srv.Handle(func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path == archive {
			w.WriteHeader(http.StatusBadGateway)
			return true
		}
		return false
	})
	res, err := fx.u.Upgrade(context.Background(), "", false)
	if err != nil || !res.Updated {
		t.Fatalf("res = %+v, err = %v", res, err)
	}
	n := 0
	for _, p := range fx.srv.Requests() {
		if p == archive {
			n++
		}
	}
	if n != 3 {
		t.Errorf("download URL attempts = %d, want 3", n)
	}
	if !strings.Contains(fx.log.String(), "through the GitHub API") {
		t.Errorf("log = %q", fx.log.String())
	}
}
