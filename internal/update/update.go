// Package update keeps an installed sonarless-mcp current from GitHub
// Releases: `sonarless-mcp upgrade` installs the latest (or a named) release,
// a daily background run installs a newer release once it is SoakPeriod old,
// and the terminal notice names a newer release. Every install verifies the
// Sigstore signature of the release's checksums file (ReleaseIdentity), the
// archive's checksum and the new binary's `version` before it atomically
// replaces the running binary. Development builds (version "dev", snapshots)
// never update themselves.
//
// The design and much of the code are ported from ytrack
// (github.com/BrainerVirus/youtrack-cli: internal/update, internal/upgrade,
// pkg/cmd/upgrade).
package update

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/sigstore/sigstore-go/pkg/root"

	"github.com/BrainerVirus/sonarless-mcp/internal/config"
)

// IssuesURL is where a release that fails its checks should be reported.
const IssuesURL = "https://github.com/" + Repo + "/issues"

// NoticeEnv turns the terminal update notice off when set to "false", "0" or
// "off".
const NoticeEnv = "SONARLESS_UPDATE_NOTICE"

// Enabled reports whether this build may upgrade itself in the background.
func Enabled(cfg *config.Config, version string) bool {
	return cfg.Bool(config.AutoUpdate) && IsRelease(version) && !strings.Contains(version, "SNAPSHOT")
}

// Runner runs the sonarless-mcp binary at exe with args and returns its
// combined output.
type Runner func(ctx context.Context, exe string, args ...string) ([]byte, error)

// RunBinary is the real Runner. The binary runs with the background upgrade
// and the notice turned off, so it never starts one of its own.
func RunBinary(ctx context.Context, exe string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, exe, args...) //nolint:gosec // exe is the verified sonarless-mcp binary
	cmd.Env = append(os.Environ(), config.AutoUpdate+"=false", NoticeEnv+"=false")
	return cmd.CombinedOutput()
}

// SmokeTest runs the candidate binary's `version` and refuses it unless it
// exits 0 and names version. Releases before the `version` command only
// answer `--version`, so that is tried when `version` fails.
func SmokeTest(ctx context.Context, run Runner, candidate, version string) error {
	out, err := run(ctx, candidate, "version")
	if err != nil {
		var ferr error
		if out, ferr = run(ctx, candidate, "--version"); ferr != nil {
			return fmt.Errorf("%w: the new binary failed `sonarless-mcp version`: %w", ErrRefused, err)
		}
	}
	if !bytes.Contains(out, []byte(version)) {
		return fmt.Errorf("%w: the new binary's `sonarless-mcp version` does not name %s: %q", ErrRefused, version, bytes.TrimSpace(out))
	}
	return nil
}

// Upgrader finds, verifies and installs releases. Every field but Current is
// a dependency tests replace.
type Upgrader struct {
	Current string // the running version
	HTTP    *http.Client
	APIURL  string // DefaultAPIURL when empty
	// TrustedRoot returns the Sigstore trusted material signatures must
	// chain to; NoSCT is for test Sigstores only.
	TrustedRoot func() (root.TrustedMaterial, error)
	NoSCT       bool
	// Executable returns the path of the running binary.
	Executable func() (string, error)
	GOOS       string
	GOARCH     string
	Run        Runner
	Now        func() time.Time
	// StatePath is the state file (StatePath(cacheDir)).
	StatePath string
	// Lock serialises installs across processes; nil takes no lock.
	Lock func() (unlock func(), err error)
	// Log gets progress lines (downloads, fallbacks); nil discards them.
	Log io.Writer
	// Sleep waits between download retries; nil waits for real.
	Sleep func(ctx context.Context, d time.Duration) error
}

// Result is what an upgrade or a check found and did.
type Result struct {
	Current, Latest string
	URL             string    // the release page
	PublishedAt     time.Time // when Latest was published
	Path            string    // the binary replaced
	Newer           bool      // Latest is newer than Current
	Updated         bool      // the binary was replaced
	Unsigned        bool      // installed on its checksum alone (--allow-unsigned)
	Waiting         bool      // background: Latest is not SoakPeriod old yet
	BackingOff      bool      // background: an earlier failure's backoff is active
	RetryAt         time.Time // with BackingOff: when the next attempt may run
}

func (u *Upgrader) client() *Client {
	return &Client{HTTP: u.HTTP, APIURL: u.APIURL, UserAgent: "sonarless-mcp/" + u.Current}
}

func (u *Upgrader) log() io.Writer {
	if u.Log == nil {
		return io.Discard
	}
	return u.Log
}

func (u *Upgrader) release(ctx context.Context, tag string) (*Release, error) {
	if tag == "" {
		return u.client().Latest(ctx)
	}
	rel, err := u.client().Tag(ctx, tag)
	if errors.Is(err, ErrReleaseNotFound) {
		return nil, fmt.Errorf("release %s not found", tag)
	}
	return rel, err
}

// Check reports the latest release (or the one named by tag) against the
// running version. It changes nothing: no download, no state, no binary.
func (u *Upgrader) Check(ctx context.Context, tag string) (*Result, error) {
	rel, err := u.release(ctx, tag)
	if err != nil {
		return nil, err
	}
	return u.result(rel), nil
}

func (u *Upgrader) result(rel *Release) *Result {
	return &Result{
		Current: u.Current, Latest: rel.Version(), URL: rel.URL, PublishedAt: rel.PublishedAt,
		Newer: Newer(rel.Version(), u.Current),
	}
}

// Upgrade installs the latest release when it is newer than the running one,
// or the release named by tag (also an older one). allowUnsigned installs a
// release without a signature on its checksum alone, and only an older
// release named by tag: releases before signing existed have none, while a
// newer one must always be signed.
func (u *Upgrader) Upgrade(ctx context.Context, tag string, allowUnsigned bool) (*Result, error) {
	if allowUnsigned && tag == "" {
		return nil, errors.New("--allow-unsigned needs --version naming an older release")
	}
	rel, err := u.release(ctx, tag)
	if err != nil {
		return nil, err
	}
	res := u.result(rel)
	switch {
	case tag == "" && !res.Newer && IsRelease(u.Current):
		return res, nil
	case tag != "" && res.Latest == strings.TrimPrefix(u.Current, "v"):
		return res, nil
	case allowUnsigned && !Newer(u.Current, res.Latest):
		return nil, fmt.Errorf("--allow-unsigned only installs a release older than the running %s; %s is not", u.Current, res.Latest)
	}
	if err := u.install(ctx, rel, res, allowUnsigned); err != nil {
		return nil, err
	}
	UpdateState(u.StatePath, func(s *State) { s.Auto.RecordSuccess(u.Current, res.Latest, u.Now(), false) })
	return res, nil
}

// Background is the daily background run: it refreshes the cached latest
// release and installs it once it is newer and at least SoakPeriod old,
// unless an earlier failure's backoff is active. It records the outcome in
// the state file, where the next terminal notice reports it.
func (u *Upgrader) Background(ctx context.Context) (*Result, error) {
	now := u.Now()
	rel, err := u.client().Latest(ctx)
	if err != nil {
		UpdateState(u.StatePath, func(s *State) { s.CheckedAt = now })
		return nil, err
	}
	UpdateState(u.StatePath, func(s *State) {
		s.CheckedAt = now
		if rel.Stable() {
			s.SetLatest(rel)
		}
	})
	res := u.result(rel)
	res.Newer = res.Newer && rel.Stable()
	if !res.Newer {
		return res, nil
	}
	if !Soaked(rel.PublishedAt, now) {
		res.Waiting = true
		return res, nil
	}
	if a := ReadState(u.StatePath).Auto; a.BackingOff(now) {
		res.BackingOff, res.RetryAt = true, a.RetryAt
		return res, nil
	}
	if err := u.install(ctx, rel, res, false); err != nil {
		UpdateState(u.StatePath, func(s *State) { s.Auto.RecordFailure(res.Latest, err, now) })
		return nil, err
	}
	UpdateState(u.StatePath, func(s *State) { s.Auto.RecordSuccess(u.Current, res.Latest, now, true) })
	return res, nil
}

// install downloads and verifies rel, runs the new binary's `version` and
// swaps it in for the running binary. Any failure leaves it untouched.
func (u *Upgrader) install(ctx context.Context, rel *Release, res *Result, allowUnsigned bool) error {
	exe, err := u.Executable()
	if err != nil {
		return fmt.Errorf("cannot locate the sonarless-mcp binary: %w", err)
	}
	resolved, err := filepath.EvalSymlinks(exe)
	if err != nil {
		resolved = exe
	}
	res.Path = resolved
	if u.Lock != nil {
		unlock, err := u.Lock()
		if err != nil {
			return err
		}
		defer unlock()
	}
	RemoveOld(resolved)
	refused := func(err error) error { return fmt.Errorf("%w; %s was not changed", err, resolved) }

	material, err := u.TrustedRoot()
	if err != nil {
		return refused(fmt.Errorf("%w: %w", ErrRefused, err))
	}
	d := &Downloader{
		HTTP:      u.HTTP,
		Trust:     Trust{Material: material, Identity: ReleaseIdentity, NoSCT: u.NoSCT},
		GOOS:      u.GOOS,
		GOARCH:    u.GOARCH,
		UserAgent: "sonarless-mcp/" + u.Current,
		APIURL:    u.APIURL,
		Sleep:     u.Sleep,
		Notice:    func(msg string) { fmt.Fprintln(u.log(), msg) },
	}
	fmt.Fprintf(u.log(), "downloading sonarless-mcp %s for %s/%s\n", res.Latest, u.GOOS, u.GOARCH)
	bin, unsigned, err := d.Binary(ctx, rel, allowUnsigned)
	if err != nil {
		if errors.Is(err, ErrUnsigned) {
			err = fmt.Errorf("%w; %s", err, unsignedHint(u.Current, res.Latest))
		}
		if errors.Is(err, ErrRefused) {
			return refused(err)
		}
		return err
	}
	res.Unsigned = unsigned
	smoke := func(candidate string) error { return SmokeTest(ctx, u.Run, candidate, res.Latest) }
	if err := ReplaceExecutable(resolved, bin, smoke); err != nil {
		if errors.Is(err, ErrRefused) {
			return refused(err)
		}
		return fmt.Errorf("replacing %s: %w", resolved, err)
	}
	res.Updated = true
	return nil
}

// unsignedHint says what to do about a release without a signature.
func unsignedHint(current, target string) string {
	if Newer(current, target) {
		return "releases published before sonarless-mcp signed them carry none; to install this older release " +
			"on its checksum alone, run `sonarless-mcp upgrade --version v" + target + " --allow-unsigned`"
	}
	return "a release newer than the running one must be signed; please report it at " + IssuesURL
}
