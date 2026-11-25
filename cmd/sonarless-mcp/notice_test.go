package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/BrainerVirus/sonarless-mcp/internal/config"
	"github.com/BrainerVirus/sonarless-mcp/internal/update"
	"github.com/BrainerVirus/sonarless-mcp/internal/update/updatetest"
)

// noticeFixture is a release build 1.0.0 at a terminal (both streams TTYs)
// with auto-upgrade off, whose cached release check, made at testNow, found
// 1.1.0. The fake GitHub fails the test if the notice goes to the network.
type noticeFixture struct {
	clock     time.Time
	statePath string
	srv       *updatetest.Server
}

func newNoticeFixture(t *testing.T) *noticeFixture {
	t.Helper()
	fx := newUpgradeFixture(t, "1.0.0", &updatetest.Release{Version: "1.1.0"})
	t.Setenv(config.AutoUpdate, "false")
	nf := &noticeFixture{clock: testNow, statePath: update.StatePath(fx.cacheDir), srv: fx.srv}
	update.UpdateState(nf.statePath, func(s *update.State) {
		s.CheckedAt, s.LatestVersion, s.LatestURL = testNow, "1.1.0", "https://github.com/BrainerVirus/sonarless-mcp/releases/tag/v1.1.0"
	})
	oldOut, oldErr, oldHTTP, oldAPI, oldNow := stdoutIsTTY, stderrIsTTY, noticeHTTP, noticeAPIURL, noticeNow
	t.Cleanup(func() {
		stdoutIsTTY, stderrIsTTY, noticeHTTP, noticeAPIURL, noticeNow = oldOut, oldErr, oldHTTP, oldAPI, oldNow
	})
	stdoutIsTTY = func() bool { return true }
	stderrIsTTY = func() bool { return true }
	noticeHTTP, noticeAPIURL = fx.srv.Client(), fx.srv.URL
	noticeNow = func() time.Time { return nf.clock }
	return nf
}

// command returns `sonarless-mcp <name>` with a --json flag, as cobra would
// hand it to PersistentPreRun after parsing args.
func command(t *testing.T, name string, args ...string) *cobra.Command {
	t.Helper()
	r := &cobra.Command{Use: "sonarless-mcp"}
	c := &cobra.Command{Use: name, Run: func(*cobra.Command, []string) {}}
	c.Flags().Bool("json", false, "")
	r.AddCommand(c)
	if err := c.ParseFlags(args); err != nil {
		t.Fatal(err)
	}
	return c
}

func notify(t *testing.T, cmd *cobra.Command) string {
	t.Helper()
	var stderr bytes.Buffer
	startNotice(cmd, &stderr).print()
	return stderr.String()
}

func TestNotice(t *testing.T) {
	t.Run("given a terminal and a newer release with auto-upgrade off, it prints one notice a day on stderr", func(t *testing.T) {
		nf := newNoticeFixture(t)
		got := notify(t, command(t, "status"))
		want := "A new release of sonarless-mcp is available: 1.0.0 → 1.1.0; run `sonarless-mcp upgrade` (https://github.com/BrainerVirus/sonarless-mcp/releases/tag/v1.1.0)\n"
		if got != want {
			t.Errorf("notice = %q, want %q", got, want)
		}
		nf.clock = testNow.Add(time.Hour)
		if got := notify(t, command(t, "status")); got != "" {
			t.Errorf("second notice the same day: %q", got)
		}
		nf.clock = testNow.Add(25 * time.Hour)
		if got := notify(t, command(t, "status")); !strings.Contains(got, "1.0.0 → 1.1.0") {
			t.Errorf("no notice the next day: %q", got)
		}
	})

	t.Run("given auto-upgrade on, the notice says it installs itself", func(t *testing.T) {
		newNoticeFixture(t)
		t.Setenv(config.AutoUpdate, "true")
		if got := notify(t, command(t, "scan")); !strings.Contains(got, "installs itself in the background a day after its release") {
			t.Errorf("notice = %q", got)
		}
	})

	for _, tc := range []struct {
		name  string
		setup func(t *testing.T)
		cmd   func(t *testing.T) *cobra.Command
	}{
		{"--json", nil, func(t *testing.T) *cobra.Command { return command(t, "status", "--json") }},
		{"the MCP stdio bridge", nil, func(t *testing.T) *cobra.Command { return command(t, "mcp") }},
		{"the gateway", nil, func(t *testing.T) *cobra.Command { return command(t, "serve") }},
		{"the upgrade command", nil, func(t *testing.T) *cobra.Command { return command(t, "upgrade") }},
		{"stdout piped", func(*testing.T) { stdoutIsTTY = func() bool { return false } }, nil},
		{"stderr piped", func(*testing.T) { stderrIsTTY = func() bool { return false } }, nil},
		{"SONARLESS_UPDATE_NOTICE=false", func(t *testing.T) { t.Setenv(config.UpdateNotice, "false") }, nil},
		{"CI", func(t *testing.T) { t.Setenv("CI", "true") }, nil},
		{"a development build", func(*testing.T) { version = "dev" }, nil},
	} {
		t.Run("given "+tc.name+", it prints nothing", func(t *testing.T) {
			newNoticeFixture(t)
			if tc.setup != nil {
				tc.setup(t)
			}
			cmd := command(t, "status")
			if tc.cmd != nil {
				cmd = tc.cmd(t)
			}
			if got := notify(t, cmd); got != "" {
				t.Errorf("notice = %q", got)
			}
		})
	}

	t.Run("given a check due, it asks GitHub at most once a day", func(t *testing.T) {
		nf := newNoticeFixture(t)
		nf.clock = testNow.Add(25 * time.Hour)
		notify(t, command(t, "status"))
		notify(t, command(t, "status"))
		if got := nf.srv.Requests(); len(got) != 1 {
			t.Errorf("requests = %v", got)
		}
	})

	t.Run("given a background upgrade that succeeded, it is announced once", func(t *testing.T) {
		nf := newNoticeFixture(t)
		version = "1.1.0"
		update.UpdateState(nf.statePath, func(s *update.State) { s.Auto.RecordSuccess("1.0.0", "1.1.0", testNow, true) })
		if got := notify(t, command(t, "status")); got != "sonarless-mcp upgraded 1.0.0 → 1.1.0 in the background\n" {
			t.Errorf("first = %q", got)
		}
		if got := notify(t, command(t, "status")); got != "" {
			t.Errorf("second = %q", got)
		}
	})
}
