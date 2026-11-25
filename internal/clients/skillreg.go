// The registry of installed skill copies, their states and the sync that
// keeps them at the running version. Ported from ytrack
// (github.com/BrainerVirus/youtrack-cli, internal/skill/registry.go and
// lock.go).

package clients

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/gofrs/flock"

	"github.com/BrainerVirus/sonarless-mcp/internal/config"
	"github.com/BrainerVirus/sonarless-mcp/internal/update"
)

// registryFile, in the user config dir, lists the skills directories
// sonarless-mcp installed the skill into.
const registryFile = "skills.json"

// SkillRegistry is the list of skills directories holding a copy of the
// skill that sonarless-mcp installed or adopted, and the version every copy
// it keeps up to date was last synced to.
type SkillRegistry struct {
	// Synced is the version of the last sync that rewrote every copy.
	Synced string `json:"synced,omitempty"`
	// Dirs are absolute skills directories; each holds <dir>/sonarless-mcp.
	Dirs []string `json:"dirs"`

	env     Env
	path    string
	existed bool
}

// HomeSkillDirs returns the skills directories of the clients that load
// skills, in client order, without duplicates.
func HomeSkillDirs(e Env) []string {
	var dirs []string
	for _, c := range All() {
		if d := c.SkillDir(e); d != "" && !slices.Contains(dirs, filepath.Clean(d)) {
			dirs = append(dirs, filepath.Clean(d))
		}
	}
	return dirs
}

// SkillRegistryPath returns the registry file's path.
func SkillRegistryPath() (string, error) {
	dir, _, err := config.UserDirs()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, registryFile), nil
}

// OpenSkillRegistry reads the registry. While the file does not exist yet,
// the registry adopts every client skills directory (HomeSkillDirs) that
// holds the sonarless-mcp skill already, never one holding a user's skill,
// stamped Synced with the newest version among them; Save then creates the
// file.
func OpenSkillRegistry(e Env) (*SkillRegistry, error) {
	path, err := SkillRegistryPath()
	if err != nil {
		return nil, err
	}
	r := &SkillRegistry{env: e, path: path}
	b, err := os.ReadFile(path)
	switch {
	case err == nil:
		r.existed = true
		if err := json.Unmarshal(b, r); err != nil {
			return nil, fmt.Errorf("reading %s: %w", path, err)
		}
		return r, nil
	case !errors.Is(err, fs.ErrNotExist):
		return nil, err
	}
	for _, d := range HomeSkillDirs(e) {
		if exists, ours := ownSkill(skillFile(d)); exists && ours {
			r.Record(d)
		}
	}
	// Copies written before sonarless-mcp kept a stamp count as synced to
	// the newest of them, so an older sonarless-mcp never rewrites them.
	for _, c := range r.Copies("") {
		if update.IsRelease(c.Version) && (r.Synced == "" || update.Newer(c.Version, r.Synced)) {
			r.Synced = c.Version
		}
	}
	return r, nil
}

// Dirty reports whether the registry holds anything its file does not:
// copies adopted while the file did not exist yet.
func (r *SkillRegistry) Dirty() bool { return !r.existed && len(r.Dirs) > 0 }

// Save writes the registry through a temporary file renamed into place, so
// a concurrent reader sees the old file or the new one, never a torn one.
func (r *SkillRegistry) Save() error {
	if err := os.MkdirAll(filepath.Dir(r.path), 0o700); err != nil {
		return err
	}
	if r.Dirs == nil {
		r.Dirs = []string{}
	}
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(r.path), ".skills-*.json")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }() // gone after the rename
	if _, err := tmp.Write(append(b, '\n')); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), r.path); err != nil {
		return err
	}
	r.existed = true
	return nil
}

// Record adds the skills directory dir, made absolute, and reports whether
// the registry changed. Directories whose copy is the same one (one copy
// symlinked into several directories) are recorded once; the directory
// holding the copy itself is preferred over one holding a link to it.
func (r *SkillRegistry) Record(dir string) bool {
	dir, err := filepath.Abs(dir)
	if err != nil {
		return false
	}
	id, link := copyID(dir)
	for i, d := range r.Dirs {
		if d == dir {
			return false
		}
		if other, otherLink := copyID(d); other == id {
			if otherLink && !link {
				r.Dirs[i] = dir
				return true
			}
			return false
		}
	}
	r.Dirs = append(r.Dirs, dir)
	return true
}

// Forget removes dir, made absolute, and reports whether it was recorded.
func (r *SkillRegistry) Forget(dir string) bool {
	dir, err := filepath.Abs(dir)
	if err != nil {
		return false
	}
	n := len(r.Dirs)
	r.Dirs = slices.DeleteFunc(r.Dirs, func(d string) bool { return d == dir })
	return len(r.Dirs) != n
}

// copyID identifies the copy in <dir>/sonarless-mcp: its resolved path, and
// whether <dir>/sonarless-mcp is a symlink to it.
func copyID(dir string) (id string, link bool) {
	p := filepath.Join(dir, skillName)
	if fi, err := os.Lstat(p); err == nil && fi.Mode()&fs.ModeSymlink != 0 {
		link = true
	}
	if resolved, err := filepath.EvalSymlinks(p); err == nil {
		return resolved, link
	}
	return filepath.Clean(p), link
}

// Copy states.
const (
	StateCurrent = "current" // holds the skill at the running version
	StateStale   = "stale"   // holds the skill at an older (or unstamped) version
	StateNewer   = "newer"   // holds the skill from a newer sonarless-mcp, left alone by this one
	StateMissing = "missing" // holds no sonarless-mcp skill (deleted or replaced)
)

// SkillCopy is a recorded skill copy as found on disk.
type SkillCopy struct {
	Dir     string // the skills directory
	Path    string // Dir/sonarless-mcp
	Version string // metadata.version of its SKILL.md; empty when missing or unstamped
	State   string // StateCurrent, StateStale, StateNewer or StateMissing
	// Managed reports whether sonarless-mcp keeps the copy at its version
	// (see ManagedSkillDir); a copy in a repository is updated by hand.
	Managed bool
}

// InspectSkill returns the copy in the recorded directory dir, compared
// with version.
func InspectSkill(e Env, dir, version string) SkillCopy {
	c := SkillCopy{Dir: dir, Path: filepath.Join(dir, skillName), State: StateMissing, Managed: ManagedSkillDir(e, dir)}
	b, err := os.ReadFile(skillFile(dir))
	if err != nil || !strings.Contains(string(b), skillMarker) {
		return c
	}
	c.Version = SkillVersionOf(b)
	switch {
	case c.Version == version:
		c.State = StateCurrent
	case update.Newer(c.Version, version):
		c.State = StateNewer
	default:
		c.State = StateStale
	}
	return c
}

// Copies inspects every recorded copy.
func (r *SkillRegistry) Copies(version string) []SkillCopy {
	copies := make([]SkillCopy, 0, len(r.Dirs))
	for _, d := range r.Dirs {
		copies = append(copies, InspectSkill(r.env, d, version))
	}
	return copies
}

// ManagedSkillDir reports whether sonarless-mcp keeps the copy in skills
// directory dir at its own version: dir is a client's skills directory
// (HomeSkillDirs), or under the home directory and not inside a git work
// tree. A copy in a repository is the repository's: it changes only through
// `sonarless-mcp skill install --dir`.
func ManagedSkillDir(e Env, dir string) bool {
	dir = filepath.Clean(dir)
	if slices.Contains(HomeSkillDirs(e), dir) {
		return true
	}
	if e.Home == "" {
		return false
	}
	rel, err := filepath.Rel(e.Home, dir)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return false
	}
	return !inGitWorkTree(dir)
}

// inGitWorkTree reports whether dir or one of its parents has a .git entry.
func inGitWorkTree(dir string) bool {
	for {
		if _, err := os.Lstat(filepath.Join(dir, ".git")); err == nil {
			return true
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return false
		}
		dir = parent
	}
}

// Sync outcomes.
const (
	SyncUpdated = "updated" // rewritten at the version
	SyncDropped = "dropped" // missing or replaced by something else; forgotten
	SyncManual  = "manual"  // a repository copy; left as it is
	SyncKept    = "kept"    // a newer sonarless-mcp's copy; left as it is (SyncForce rewrites it)
	SyncFailed  = "failed"  // could not be rewritten; still recorded
)

// SyncResult is what Sync did with one recorded directory.
type SyncResult struct {
	SkillCopy
	Action string
	Err    error
}

// Sync rewrites every managed copy at version and forgets every recorded
// directory whose copy is gone or holds something else: a missing copy is
// never re-created. Copies in repositories and copies a newer sonarless-mcp
// wrote are left alone. When every managed copy was rewritten, Synced
// becomes version. It reports what it did per directory, and whether it all
// succeeded. The caller saves the registry.
func (r *SkillRegistry) Sync(version string) ([]SyncResult, bool) { return r.sync(version, false) }

// SyncForce is Sync that also rewrites copies a newer sonarless-mcp
// installed, bringing them back to version.
func (r *SkillRegistry) SyncForce(version string) ([]SyncResult, bool) { return r.sync(version, true) }

func (r *SkillRegistry) sync(version string, force bool) ([]SyncResult, bool) {
	var results []SyncResult
	ok := true
	keep := r.Dirs[:0:0]
	done := map[string]bool{} // copies already rewritten, by copyID
	for _, d := range r.Dirs {
		c := InspectSkill(r.env, d, version)
		res := SyncResult{SkillCopy: c}
		switch {
		case c.State == StateMissing:
			res.Action = SyncDropped
			results = append(results, res)
			continue
		case !c.Managed:
			res.Action = SyncManual
		case c.State == StateNewer && !force:
			res.Action = SyncKept
		default:
			id, _ := copyID(d)
			if !done[id] {
				if _, err := InstallSkillDir(d, version); err != nil {
					res.Action, res.Err, ok = SyncFailed, err, false
					break
				}
				done[id] = true
			}
			res.Action = SyncUpdated
			res.Version, res.State = version, StateCurrent
		}
		keep = append(keep, d)
		results = append(results, res)
	}
	r.Dirs = keep
	if ok {
		r.Synced = version
	}
	return results, ok
}

// Equal reports whether two registries record the same thing.
func (r *SkillRegistry) Equal(o *SkillRegistry) bool {
	return r.Synced == o.Synced && slices.Equal(r.Dirs, o.Dirs)
}

// Clone returns a copy of r for a later Equal.
func (r *SkillRegistry) Clone() *SkillRegistry {
	c := *r
	c.Dirs = slices.Clone(r.Dirs)
	return &c
}

// LockSkills takes the registry lock, waiting up to wait for another
// process to release it. It reports false when the lock cannot be taken
// (busy past wait, or a read-only config dir); the caller then goes on
// without it (commands) or skips its work (the background refresh, wait 0).
func LockSkills(wait time.Duration) (unlock func(), ok bool) {
	path, err := SkillRegistryPath()
	if err != nil {
		return nil, false
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, false
	}
	l := flock.New(filepath.Join(filepath.Dir(path), "skills.lock"))
	var locked bool
	if wait <= 0 {
		locked, err = l.TryLock()
	} else {
		ctx, cancel := context.WithTimeout(context.Background(), wait)
		defer cancel()
		locked, err = l.TryLockContext(ctx, 20*time.Millisecond)
	}
	if err != nil || !locked {
		return nil, false
	}
	return func() { _ = l.Unlock() }, true
}

// RecordSkills adds dirs to the registry and stamps it with the version
// just written, so an older sonarless-mcp never rewrites these copies.
// Best-effort: the skill is installed either way, and a registry that
// cannot be written only means the copies are not kept up to date.
func RecordSkills(e Env, dirs []string, version string) {
	if len(dirs) == 0 {
		return
	}
	if unlock, ok := LockSkills(skillLockWait); ok {
		defer unlock()
	}
	reg, err := OpenSkillRegistry(e)
	if err != nil {
		return
	}
	changed := reg.Dirty() || reg.Synced != version
	reg.Synced = version
	for _, d := range dirs {
		changed = reg.Record(d) || changed
	}
	if changed {
		_ = reg.Save()
	}
}

// forgetSkill removes dir from the registry. Best-effort, like RecordSkills.
func forgetSkill(e Env, dir string) {
	if unlock, ok := LockSkills(skillLockWait); ok {
		defer unlock()
	}
	reg, err := OpenSkillRegistry(e)
	if err != nil {
		return
	}
	if reg.Forget(dir) || reg.Dirty() {
		_ = reg.Save()
	}
}

// skillLockWait bounds how long a skill command waits for another
// sonarless-mcp that is syncing the copies; it then goes on without the lock.
const skillLockWait = 3 * time.Second
