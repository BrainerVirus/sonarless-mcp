package mcp

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/BrainerVirus/sonarless-mcp/internal/config"
)

// A reload that fails (an unreadable remotes.json, a token that cannot be
// read yet) must be tried again for the same files, not only after the next
// change: otherwise the session keeps its old remotes until someone edits
// remotes.json again.
func TestFailedRemoteReloadIsRetried(t *testing.T) {
	old := reloadRetryAfter
	t.Cleanup(func() { reloadRetryAfter = old })
	reloadRetryAfter = 20 * time.Millisecond

	dir := t.TempDir()
	calls := 0
	s := &Shim{
		Cfg:      &config.Config{ConfigDir: dir},
		Backends: []*Backend{{Name: "local"}},
		Reload: func() ([]*Backend, error) {
			calls++
			if calls == 1 {
				return nil, errors.New("remotes.json is being written")
			}
			return []*Backend{{Name: "local"}, {Name: "work"}}, nil
		},
	}
	s.remotesSig = s.remotesStamp()
	if err := os.WriteFile(filepath.Join(dir, "remotes.json"), []byte(`{"work":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	s.reloadRemotes() // the change is seen; this reload fails
	if calls != 1 || s.backend("work") != nil {
		t.Fatalf("after the failed reload: calls %d, work %v", calls, s.backend("work"))
	}
	s.reloadRemotes() // same files, inside the pause: not retried yet
	if calls != 1 {
		t.Fatalf("retried at once (calls %d): a failure must not reload on every request", calls)
	}
	time.Sleep(30 * time.Millisecond)
	s.reloadRemotes() // same files, after the pause: retried and applied
	if calls != 2 || s.backend("work") == nil {
		t.Fatalf("after the pause: calls %d, work %v", calls, s.backend("work"))
	}
	s.reloadRemotes() // success clears the retry: nothing more to do
	if calls != 2 {
		t.Errorf("reloaded again after success (calls %d)", calls)
	}
}
