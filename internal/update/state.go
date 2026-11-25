// Ported from ytrack (github.com/BrainerVirus/youtrack-cli, internal/update/check.go
// and auto.go); sonarless keeps the state as JSON in its cache directory.

package update

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	// StateFile is the file in the cache directory that records the release
	// checks, notices and background upgrades.
	StateFile = "update-state.json"

	// CheckInterval is how long a release check is reused before asking
	// again, and the least time between two daily background runs.
	CheckInterval = 24 * time.Hour

	// SoakPeriod is how old a release must be before the background upgrade
	// installs it, so a broken release can be pulled before it spreads. A
	// manual `sonarless-mcp upgrade` does not wait.
	SoakPeriod = 24 * time.Hour

	// SpawnInterval is the least time between two background run starts, so
	// a child that dies before it records anything is not restarted on every
	// command.
	SpawnInterval = time.Hour

	maxBackoff  = 8 * 24 * time.Hour
	maxErrorLen = 500
)

// State is what the release checks remember between runs.
type State struct {
	CheckedAt     time.Time `json:"checked_at"`
	LatestVersion string    `json:"latest_version,omitempty"`
	LatestURL     string    `json:"latest_url,omitempty"`
	// LatestPublishedAt is when LatestVersion was published.
	LatestPublishedAt time.Time `json:"latest_published_at,omitempty"`
	// NotifiedAt and NotifiedVersion record the last notice printed, so a
	// newer release is announced once per CheckInterval.
	NotifiedAt      time.Time `json:"notified_at,omitempty"`
	NotifiedVersion string    `json:"notified_version,omitempty"`

	Auto AutoState `json:"auto_upgrade"`
}

// AutoState is what the background upgrade remembers between runs.
type AutoState struct {
	// StartedAt is when a command last started a background run.
	StartedAt time.Time `json:"started_at,omitempty"`

	// From, To and UpgradedAt record the last successful background upgrade;
	// Announced is set once a terminal was told about it.
	From       string    `json:"from,omitempty"`
	To         string    `json:"to,omitempty"`
	UpgradedAt time.Time `json:"upgraded_at,omitempty"`
	Announced  bool      `json:"announced,omitempty"`

	// The last failed attempt since the last success: the release it tried,
	// when, why, how many failures in a row, and when it may try again.
	FailedVersion string    `json:"failed_version,omitempty"`
	FailedAt      time.Time `json:"failed_at,omitempty"`
	Error         string    `json:"error,omitempty"`
	Failures      int       `json:"failures,omitempty"`
	RetryAt       time.Time `json:"retry_at,omitempty"`
}

// StatePath is the state file in cacheDir.
func StatePath(cacheDir string) string { return filepath.Join(cacheDir, StateFile) }

// stateMu serialises read-modify-write updates of the state file within the
// process: the notice's release check and its notice both write it.
var stateMu sync.Mutex

// ReadState loads the state from path. A missing or unreadable file is an
// empty state.
func ReadState(path string) State {
	var s State
	b, err := os.ReadFile(path)
	if err != nil || json.Unmarshal(b, &s) != nil {
		return State{}
	}
	return s
}

// WriteState saves the state to path atomically.
func WriteState(path string, s State) error {
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+StateFile+".*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(append(b, '\n')); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// UpdateState applies fn to the state at path and saves it (best effort: a
// cache directory that cannot be written only means the next run asks
// again).
func UpdateState(path string, fn func(*State)) {
	stateMu.Lock()
	defer stateMu.Unlock()
	s := ReadState(path)
	fn(&s)
	_ = WriteState(path, s)
}

// SetLatest caches rel as the latest release.
func (s *State) SetLatest(rel *Release) {
	s.LatestVersion, s.LatestURL, s.LatestPublishedAt = rel.Version(), rel.URL, rel.PublishedAt
}

// CheckDue reports whether the last release check is older than
// CheckInterval (or in the future), so a new one may go to the network.
func (s *State) CheckDue(now time.Time) bool {
	return s.CheckedAt.IsZero() || now.Sub(s.CheckedAt) >= CheckInterval || s.CheckedAt.After(now)
}

// Soaked reports whether a release published at published is at least
// SoakPeriod old at now. An unknown publication time is not soaked.
func Soaked(published, now time.Time) bool {
	return !published.IsZero() && now.Sub(published) >= SoakPeriod
}

// Backoff is the wait after the nth background upgrade failure in a row:
// 1 day, 2, 4, then 8 days at most.
func Backoff(n int) time.Duration {
	d := 24 * time.Hour
	for i := 1; i < n && d < maxBackoff; i++ {
		d *= 2
	}
	return min(d, maxBackoff)
}

// BackingOff reports whether a failed background upgrade still blocks a new
// attempt at now.
func (a *AutoState) BackingOff(now time.Time) bool {
	return !a.RetryAt.IsZero() && now.Before(a.RetryAt)
}

// RunDue reports whether a command should start the background run for
// current at now: once a day for the release check and the MCP image
// refresh, and earlier when the cached newer release has just soaked
// (unless a failure's backoff is active). Never twice within SpawnInterval.
func (s *State) RunDue(current string, now time.Time) bool {
	started := s.Auto.StartedAt
	if !started.IsZero() && !started.After(now) && now.Sub(started) < SpawnInterval {
		return false
	}
	if started.IsZero() || started.After(now) || now.Sub(started) >= CheckInterval {
		return true
	}
	return s.LatestVersion != "" && Newer(s.LatestVersion, current) &&
		Soaked(s.LatestPublishedAt, now) && !s.Auto.BackingOff(now)
}

// RecordFailure records a failed background upgrade to version and schedules
// the next attempt with Backoff.
func (a *AutoState) RecordFailure(version string, err error, now time.Time) {
	msg := strings.Join(strings.Fields(err.Error()), " ")
	if len(msg) > maxErrorLen {
		msg = msg[:maxErrorLen] + "…"
	}
	a.Failures++
	a.FailedVersion, a.FailedAt, a.Error = version, now, msg
	a.RetryAt = now.Add(Backoff(a.Failures))
}

// RecordSuccess records an upgrade from one version to another and clears
// any failure and backoff. background marks it for a one-time announcement.
func (a *AutoState) RecordSuccess(from, to string, now time.Time, background bool) {
	*a = AutoState{StartedAt: a.StartedAt}
	if background {
		a.From, a.To, a.UpgradedAt = from, to, now
	}
}

// Notice is a newer release worth telling the user about.
type Notice struct {
	Current, Latest, URL string
	PublishedAt          time.Time
}

// Pending returns the cached release at path as a Notice when it is newer
// than current and was not announced within CheckInterval. It reads only the
// state file, never the network.
func Pending(path, current string, now time.Time) *Notice {
	s := ReadState(path)
	if s.LatestVersion == "" || !Newer(s.LatestVersion, current) {
		return nil
	}
	if s.NotifiedVersion == s.LatestVersion && !s.NotifiedAt.IsZero() &&
		now.Sub(s.NotifiedAt) < CheckInterval && !s.NotifiedAt.After(now) {
		return nil
	}
	return &Notice{Current: current, Latest: s.LatestVersion, URL: s.LatestURL, PublishedAt: s.LatestPublishedAt}
}

// MarkNotified records that the notice for version was printed at now.
func MarkNotified(path, version string, now time.Time) {
	UpdateState(path, func(s *State) { s.NotifiedAt, s.NotifiedVersion = now, version })
}

// CheckDaily asks for the latest release when the last check recorded at
// path is older than CheckInterval and records it (also when it fails, so an
// offline machine asks once a day, not on every run). A check cancelled
// through ctx (abandoned by the caller) records nothing, so the next run asks
// again.
func CheckDaily(ctx context.Context, c *Client, path string, now time.Time) error {
	if s := ReadState(path); !s.CheckDue(now) {
		return nil
	}
	rel, err := c.Latest(ctx)
	if err != nil && errors.Is(ctx.Err(), context.Canceled) {
		return err
	}
	UpdateState(path, func(s *State) {
		s.CheckedAt = now
		if err == nil && rel.Stable() {
			s.SetLatest(rel)
		}
	})
	return err
}
