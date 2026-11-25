package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sigstore/sigstore-go/pkg/root"
	"github.com/spf13/cobra"

	"github.com/BrainerVirus/sonarless-mcp/internal/config"
	"github.com/BrainerVirus/sonarless-mcp/internal/update"
	"github.com/BrainerVirus/sonarless-mcp/internal/update/updatetest"
)

var testNow = time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)

// upgradeFixture isolates the upgrade command: a temp SONARLESS_HOME, a
// binary of version current in a temp dir, a fake GitHub with releases, and
// recorders instead of the skill refresh, image pull and background spawn.
type upgradeFixture struct {
	srv                    *updatetest.Server
	exe                    string
	cacheDir               string
	skills, images, spawns int
	stdout, stderr         bytes.Buffer
}

func newUpgradeFixture(t *testing.T, current string, releases ...*updatetest.Release) *upgradeFixture {
	t.Helper()
	home := t.TempDir()
	t.Setenv("SONARLESS_HOME", home)
	unsetenv(t, config.AutoUpdate, config.UpdateNotice, "CI", "BUILD_NUMBER", "RUN_ID")
	fx := &upgradeFixture{cacheDir: filepath.Join(home, "cache")}
	s := updatetest.NewSigstore(t)
	fx.srv = updatetest.NewServer(t, s, releases...)
	fx.exe = filepath.Join(t.TempDir(), "sonarless-mcp")
	if err := os.WriteFile(fx.exe, []byte("installed "+current), 0o755); err != nil {
		t.Fatal(err)
	}

	oldVersion, oldNew, oldSkills, oldImage, oldSpawn := version, newUpgrader, refreshSkills, refreshImage, spawnBackground
	t.Cleanup(func() {
		version, newUpgrader, refreshSkills, refreshImage, spawnBackground = oldVersion, oldNew, oldSkills, oldImage, oldSpawn
	})
	version = current
	newUpgrader = func(cfg *config.Config, log io.Writer) *update.Upgrader {
		return &update.Upgrader{
			Current:     version,
			HTTP:        fx.srv.Client(),
			APIURL:      fx.srv.URL,
			TrustedRoot: func() (root.TrustedMaterial, error) { return s.VS, nil },
			NoSCT:       true,
			Executable:  func() (string, error) { return fx.exe, nil },
			GOOS:        "linux",
			GOARCH:      "amd64",
			Run:         updatetest.Runner,
			Now:         func() time.Time { return testNow },
			StatePath:   update.StatePath(cfg.CacheDir),
			Log:         log,
			Sleep:       func(context.Context, time.Duration) error { return nil },
		}
	}
	refreshSkills = func() int { fx.skills++; return 0 }
	refreshImage = func(context.Context, *config.Config, io.Writer, io.Writer, string) { fx.images++ }
	spawnBackground = func(*config.Config) error { fx.spawns++; return nil }
	return fx
}

// unsetenv removes names for the test, restoring them after it.
func unsetenv(t *testing.T, names ...string) {
	t.Helper()
	for _, n := range names {
		t.Setenv(n, "")
		_ = os.Unsetenv(n)
	}
}

// run executes `sonarless-mcp <args>` with the upgrade command.
func (fx *upgradeFixture) run(args ...string) error {
	r := &cobra.Command{Use: "sonarless-mcp", SilenceUsage: true, SilenceErrors: true}
	r.AddCommand(upgradeCommand())
	r.SetArgs(args)
	r.SetOut(&fx.stdout)
	r.SetErr(&fx.stderr)
	return r.ExecuteContext(context.Background())
}

func (fx *upgradeFixture) installed(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(fx.exe)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestUpgradeCheck(t *testing.T) {
	for _, name := range []string{"upgrade", "update"} {
		t.Run("given `"+name+" --check` and a newer release, it prints current and latest and changes nothing", func(t *testing.T) {
			fx := newUpgradeFixture(t, "1.0.0", &updatetest.Release{Version: "1.1.0"})
			if err := fx.run(name, "--check"); err != nil {
				t.Fatal(err)
			}
			if got := fx.stdout.String(); !strings.Contains(got, "sonarless-mcp 1.1.0 is available (installed: 1.0.0)") {
				t.Errorf("stdout = %q", got)
			}
			if got := fx.installed(t); got != "installed 1.0.0" {
				t.Errorf("binary changed: %q", got)
			}
			if d := fx.srv.Downloads(); len(d) != 0 {
				t.Errorf("downloaded %v", d)
			}
			if fx.skills+fx.images+fx.spawns != 0 {
				t.Errorf("side effects: skills %d, images %d, spawns %d", fx.skills, fx.images, fx.spawns)
			}
			if entries, _ := os.ReadDir(fx.cacheDir); len(entries) != 0 {
				t.Errorf("cache written: %v", entries)
			}
		})
	}

	t.Run("given --check and the latest installed, it says up to date", func(t *testing.T) {
		fx := newUpgradeFixture(t, "1.1.0", &updatetest.Release{Version: "1.1.0"})
		if err := fx.run("upgrade", "--check"); err != nil {
			t.Fatal(err)
		}
		if got := fx.stdout.String(); got != "sonarless-mcp 1.1.0 is up to date\n" {
			t.Errorf("stdout = %q", got)
		}
	})
}

func TestUpgradeCommand(t *testing.T) {
	t.Run("given a newer signed release, it installs it and refreshes the skill and the image", func(t *testing.T) {
		fx := newUpgradeFixture(t, "1.0.0", &updatetest.Release{Version: "1.1.0"})
		if err := fx.run("upgrade"); err != nil {
			t.Fatal(err)
		}
		if got := fx.stdout.String(); !strings.Contains(got, "Upgraded sonarless-mcp 1.0.0 to 1.1.0") {
			t.Errorf("stdout = %q", got)
		}
		if got := fx.installed(t); !strings.Contains(got, "1.1.0") {
			t.Errorf("binary = %q", got)
		}
		if fx.skills != 1 || fx.images != 1 {
			t.Errorf("skills %d, images %d", fx.skills, fx.images)
		}
	})

	t.Run("given a signature from another identity, it refuses and leaves the binary", func(t *testing.T) {
		fx := newUpgradeFixture(t, "1.0.0", &updatetest.Release{
			Version: "1.1.0", Subject: "https://github.com/someone/fork/.github/workflows/release.yml@refs/heads/main",
		})
		err := fx.run("upgrade")
		if err == nil || !strings.Contains(err.Error(), "refusing to upgrade") {
			t.Fatalf("err = %v", err)
		}
		if got := fx.installed(t); got != "installed 1.0.0" {
			t.Errorf("binary changed: %q", got)
		}
	})

	t.Run("given an older unsigned release and --allow-unsigned, it installs it with a warning", func(t *testing.T) {
		fx := newUpgradeFixture(t, "1.0.0",
			&updatetest.Release{Version: "0.8.1", Unsigned: true, Binary: func(string) []byte { return []byte("legacy 0.8.1") }},
			&updatetest.Release{Version: "1.0.0"})
		if err := fx.run("upgrade", "--version", "v0.8.1", "--allow-unsigned"); err != nil {
			t.Fatal(err)
		}
		if got := fx.stderr.String(); !strings.Contains(got, "warning: 0.8.1 has no signature") {
			t.Errorf("stderr = %q", got)
		}
		if got := fx.stdout.String(); !strings.Contains(got, "Installed sonarless-mcp 1.0.0 to 0.8.1") {
			t.Errorf("stdout = %q", got)
		}
	})

	t.Run("given --check with --allow-unsigned, the flags are refused", func(t *testing.T) {
		fx := newUpgradeFixture(t, "1.0.0", &updatetest.Release{Version: "1.0.0"})
		if err := fx.run("upgrade", "--check", "--allow-unsigned"); err == nil {
			t.Fatal("accepted --check --allow-unsigned")
		}
	})
}

func TestUpgradeBackground(t *testing.T) {
	t.Run("given a release younger than a day, it waits", func(t *testing.T) {
		fx := newUpgradeFixture(t, "1.0.0", &updatetest.Release{Version: "1.1.0", PublishedAt: testNow.Add(-2 * time.Hour)})
		if err := fx.run("upgrade", "--background"); err != nil {
			t.Fatal(err)
		}
		if got := fx.stdout.String(); !strings.Contains(got, "1.1.0 was published at") || !strings.Contains(got, "waiting 24h0m0s") {
			t.Errorf("stdout = %q", got)
		}
		if got := fx.installed(t); got != "installed 1.0.0" {
			t.Errorf("binary changed: %q", got)
		}
		if fx.images != 1 {
			t.Errorf("the MCP image was not refreshed (%d)", fx.images)
		}
	})

	t.Run("given a release older than a day, it installs it", func(t *testing.T) {
		fx := newUpgradeFixture(t, "1.0.0", &updatetest.Release{Version: "1.1.0", PublishedAt: testNow.Add(-25 * time.Hour)})
		if err := fx.run("update", "--background"); err != nil {
			t.Fatal(err)
		}
		if got := fx.installed(t); !strings.Contains(got, "1.1.0") {
			t.Errorf("binary = %q; log %q %q", got, fx.stdout.String(), fx.stderr.String())
		}
	})
}

func TestAutoUpdate(t *testing.T) {
	load := func(t *testing.T) *config.Config {
		t.Helper()
		cfg, _, err := config.Load("")
		if err != nil {
			t.Fatal(err)
		}
		return cfg
	}

	t.Run("given no state, it starts the background run and records it", func(t *testing.T) {
		fx := newUpgradeFixture(t, "1.0.0")
		cfg := load(t)
		autoUpdate(cfg)
		if fx.spawns != 1 {
			t.Fatalf("spawns = %d", fx.spawns)
		}
		if s := update.ReadState(update.StatePath(cfg.CacheDir)); s.Auto.StartedAt.IsZero() {
			t.Error("StartedAt not recorded")
		}
		autoUpdate(cfg)
		if fx.spawns != 1 {
			t.Errorf("started again at once: spawns = %d", fx.spawns)
		}
	})

	t.Run("given SONARLESS_AUTO_UPDATE=false, it starts nothing", func(t *testing.T) {
		fx := newUpgradeFixture(t, "1.0.0")
		t.Setenv(config.AutoUpdate, "false")
		autoUpdate(load(t))
		if fx.spawns != 0 {
			t.Errorf("spawns = %d", fx.spawns)
		}
	})

	t.Run("given a development build, it starts nothing", func(t *testing.T) {
		fx := newUpgradeFixture(t, "dev")
		autoUpdate(load(t))
		if fx.spawns != 0 {
			t.Errorf("spawns = %d", fx.spawns)
		}
	})
}

func TestVersionCommand(t *testing.T) {
	oldV, oldC, oldD := version, commit, date
	t.Cleanup(func() { version, commit, date = oldV, oldC, oldD })
	version, commit, date = "1.2.3", "abc1234", "2026-11-21"

	var out bytes.Buffer
	cmd := versionCommand()
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"--json"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	var got map[string]string
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("%v: %q", err, out.String())
	}
	for _, k := range []string{"version", "commit", "date", "go", "os", "arch"} {
		if got[k] == "" {
			t.Errorf("%s missing: %v", k, got)
		}
	}
	if got["version"] != "1.2.3" || got["commit"] != "abc1234" || got["date"] != "2026-11-21" {
		t.Errorf("got %v", got)
	}

	out.Reset()
	cmd = versionCommand()
	cmd.SetOut(&out)
	cmd.SetArgs(nil)
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out.String(), "sonarless-mcp version 1.2.3 (2026-11-21)\ncommit abc1234\ngo") {
		t.Errorf("text = %q", out.String())
	}
}
