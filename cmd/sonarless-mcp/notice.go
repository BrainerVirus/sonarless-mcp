package main

// Ported from ytrack's update notice (github.com/BrainerVirus/youtrack-cli,
// pkg/cmd/root/update_notice.go).

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
	"unicode"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/BrainerVirus/sonarless-mcp/internal/config"
	"github.com/BrainerVirus/sonarless-mcp/internal/update"
)

// noticeTimeout bounds the daily release check, which runs in the
// background while the command does.
const noticeTimeout = 3 * time.Second

// noticeGrace is how long a finished command waits for a release check still
// in flight before abandoning it, so a slow or unreachable server never
// delays the exit by more than this.
var noticeGrace = 300 * time.Millisecond

// What the notice depends on; tests replace them.
var (
	stdoutIsTTY  = func() bool { return term.IsTerminal(int(os.Stdout.Fd())) }
	stderrIsTTY  = func() bool { return term.IsTerminal(int(os.Stderr.Fd())) }
	noticeHTTP   = &http.Client{}
	noticeAPIURL = "" // update.DefaultAPIURL
	noticeNow    = time.Now
)

// noticeSkips are the commands that never print a notice: protocol servers
// and helpers whose output belongs to an AI client or a log, the upgrade
// itself, and shell completion.
var noticeSkips = map[string]bool{
	"mcp": true, "serve": true, "daemon": true, "ensure": true, "upgrade": true, "completion": true,
	cobra.ShellCompRequestCmd: true, cobra.ShellCompNoDescRequestCmd: true,
}

// updateNotice tells a person at a terminal about a newer release.
//
// It runs only when stdout and stderr are both terminals, never with --json,
// for the commands in noticeSkips, in CI, for development builds, or with
// SONARLESS_UPDATE_NOTICE=false. The latest release is cached in the state
// file and refreshed from GitHub at most once a day in a goroutine while the
// command runs; a refresh not finished noticeGrace after the command is
// abandoned and retried next time. The notice itself is one line on stderr,
// at most once a day per release, plus the last background upgrade failure,
// and a background upgrade that succeeded is announced once.
type updateNotice struct {
	statePath string
	current   string
	auto      bool // the background upgrade is on
	w         io.Writer
	now       time.Time
	done      chan struct{}
	cancel    context.CancelFunc
}

// startNotice returns the notice for cmd, with its release check started,
// or nil when no notice may print.
func startNotice(cmd *cobra.Command, w io.Writer) *updateNotice {
	if !update.IsRelease(version) || strings.Contains(version, "SNAPSHOT") || isCI() {
		return nil
	}
	for c := cmd; c != nil; c = c.Parent() {
		if noticeSkips[c.Name()] {
			return nil
		}
	}
	if fl := cmd.Flags().Lookup("json"); fl != nil && fl.Changed {
		return nil
	}
	if !stdoutIsTTY() || !stderrIsTTY() {
		return nil
	}
	cfg, _, err := config.Load("")
	if err != nil || !cfg.Bool(config.UpdateNotice) {
		return nil
	}
	n := &updateNotice{
		statePath: update.StatePath(cfg.CacheDir), current: version, auto: update.Enabled(cfg, version),
		w: w, now: noticeNow(),
	}
	if s := update.ReadState(n.statePath); s.CheckDue(n.now) {
		client := &update.Client{HTTP: noticeHTTP, APIURL: noticeAPIURL, UserAgent: "sonarless-mcp/" + version}
		ctx, cancel := context.WithTimeout(context.Background(), noticeTimeout)
		n.done, n.cancel = make(chan struct{}), cancel
		go func() {
			defer close(n.done)
			_ = update.CheckDaily(ctx, client, n.statePath, n.now)
		}()
	}
	return n
}

// isCI reports whether sonarless-mcp runs in a CI system.
func isCI() bool {
	return os.Getenv("CI") != "" || os.Getenv("BUILD_NUMBER") != "" || os.Getenv("RUN_ID") != ""
}

// print prints what is due once the command finished. A nil notice prints
// nothing.
func (n *updateNotice) print() {
	if n == nil {
		return
	}
	if n.done != nil {
		select {
		case <-n.done:
		case <-time.After(noticeGrace):
		}
		n.cancel()
	}
	n.printUpgraded()
	notice := update.Pending(n.statePath, n.current, n.now)
	if notice == nil {
		return
	}
	how := "run `sonarless-mcp upgrade`"
	if n.auto {
		how = "it installs itself in the background a day after its release, or run `sonarless-mcp upgrade` now"
	}
	fmt.Fprintf(n.w, "A new release of sonarless-mcp is available: %s → %s; %s (%s)\n", notice.Current, notice.Latest, how, notice.URL)
	if a := update.ReadState(n.statePath).Auto; a.Error != "" && a.FailedVersion == notice.Latest {
		fmt.Fprintf(n.w, "The background upgrade to %s failed at %s: %s; it retries after %s\n",
			a.FailedVersion, a.FailedAt.Local().Format(time.DateTime), printable(a.Error), a.RetryAt.Local().Format(time.DateTime))
	}
	update.MarkNotified(n.statePath, notice.Latest, n.now)
}

// printUpgraded announces, once, a background upgrade that succeeded.
func (n *updateNotice) printUpgraded() {
	a := update.ReadState(n.statePath).Auto
	if a.To == "" || a.Announced {
		return
	}
	fmt.Fprintf(n.w, "sonarless-mcp upgraded %s → %s in the background\n", a.From, a.To)
	update.UpdateState(n.statePath, func(s *update.State) { s.Auto.Announced = true })
}

// printable drops control characters, so a recorded error cannot drive the
// terminal.
func printable(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, s)
}
