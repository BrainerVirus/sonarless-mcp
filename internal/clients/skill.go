package clients

import (
	_ "embed"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/BrainerVirus/sonarless-mcp/internal/update"
)

//go:embed skill/SKILL.md
var skillMD string

const (
	skillName   = "sonarless-mcp"
	skillMarker = "managed by sonarless-mcp setup"
)

// Version is the running sonarless-mcp version, stamped into every skill
// copy it writes (metadata.version); main sets it.
var Version = "dev"

// SkillDir returns where a client loads user skills from ("" if it has no
// skills support).
func (c Client) SkillDir(e Env) string {
	switch c.ID {
	case "claude":
		return filepath.Join(e.Home, ".claude", "skills")
	case "cursor":
		return filepath.Join(e.Home, ".cursor", "skills")
	case "opencode":
		if x := os.Getenv("XDG_CONFIG_HOME"); x != "" && e.GOOS != "windows" {
			return filepath.Join(x, "opencode", "skills")
		}
		return filepath.Join(e.Home, ".config", "opencode", "skills")
	case "codex":
		if h := os.Getenv("CODEX_HOME"); h != "" {
			return filepath.Join(h, "skills")
		}
		return filepath.Join(e.Home, ".codex", "skills")
	}
	return ""
}

func skillFile(dir string) string { return filepath.Join(dir, skillName, "SKILL.md") }

// ownSkill reports whether the skill file there is ours (or absent).
func ownSkill(file string) (exists, ours bool) {
	b, err := os.ReadFile(file)
	if err != nil {
		return false, true
	}
	return true, strings.Contains(string(b), skillMarker)
}

// RenderSkill returns SKILL.md with metadata.version set to version in its
// front matter.
func RenderSkill(version string) []byte {
	stamp := "metadata:\n  version: " + strconv.Quote(version) + "\n"
	if i := strings.Index(skillMD[3:], "\n---\n"); strings.HasPrefix(skillMD, "---\n") && i >= 0 {
		end := 3 + i + 1 // start of the closing ---
		return []byte(skillMD[:end] + stamp + skillMD[end:])
	}
	return []byte(skillMD)
}

// SkillVersionOf returns metadata.version from a SKILL.md's front matter,
// or "" when it has none.
func SkillVersionOf(b []byte) string {
	s := strings.ReplaceAll(string(b), "\r\n", "\n")
	rest, ok := strings.CutPrefix(s, "---\n")
	if !ok {
		return ""
	}
	front, _, ok := strings.Cut(rest, "\n---")
	if !ok {
		return ""
	}
	inMeta := false
	for _, line := range strings.Split(front, "\n") {
		switch {
		case line == "metadata:":
			inMeta = true
		case inMeta && strings.HasPrefix(line, " "):
			if v, ok := strings.CutPrefix(strings.TrimSpace(line), "version:"); ok {
				v = strings.TrimSpace(v)
				if u, err := strconv.Unquote(v); err == nil {
					v = u
				}
				return strings.Trim(v, "'")
			}
		default:
			inMeta = false
		}
	}
	return ""
}

// ForeignError is a skills directory slot that holds someone else's skill
// named sonarless-mcp; it is never replaced or removed.
type ForeignError struct{ Path string }

func (e *ForeignError) Error() string {
	return fmt.Sprintf("%s holds a skill sonarless-mcp did not install", e.Path)
}

// InstallSkillDir writes the skill, stamped with version, to
// <dir>/sonarless-mcp/SKILL.md and returns that file. A same-named skill the
// user wrote is refused with a *ForeignError.
func InstallSkillDir(dir, version string) (string, error) {
	f := skillFile(dir)
	if _, ours := ownSkill(f); !ours {
		return "", &ForeignError{Path: filepath.Dir(f)}
	}
	return f, writeFile(f, RenderSkill(version))
}

// UninstallSkillDir removes <dir>/sonarless-mcp when it holds our skill and
// reports whether it removed anything. A symlink there is removed, not its
// target; a user's same-named skill is refused with a *ForeignError.
func UninstallSkillDir(dir string) (bool, error) {
	slot := filepath.Join(dir, skillName)
	f := skillFile(dir)
	exists, ours := ownSkill(f)
	if !exists {
		return false, nil
	}
	if !ours {
		return false, &ForeignError{Path: slot}
	}
	if fi, err := os.Lstat(slot); err == nil && fi.Mode()&fs.ModeSymlink != 0 {
		return true, os.Remove(slot)
	}
	if err := os.Remove(f); err != nil {
		return false, err
	}
	_ = os.Remove(slot) // only if now empty
	return true, nil
}

func isForeign(err error) bool {
	var f *ForeignError
	return errors.As(err, &f)
}

// InstallSkill writes the sonarless-mcp skill for the client and records it
// (see SkillRegistry). It never replaces a same-named skill the user wrote.
// Returns the file written ("" if the client has no skills or the slot
// belongs to the user).
func (c Client) InstallSkill(e Env) (string, error) {
	dir := c.SkillDir(e)
	if dir == "" {
		return "", nil
	}
	f, err := InstallSkillDir(dir, Version)
	if isForeign(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	RecordSkills(e, []string{dir}, Version)
	return f, nil
}

// RemoveSkill deletes the skill if it is ours and forgets it.
func (c Client) RemoveSkill(e Env) error {
	dir := c.SkillDir(e)
	if dir == "" {
		return nil
	}
	removed, err := UninstallSkillDir(dir)
	if isForeign(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if removed {
		forgetSkill(e, dir)
	}
	return nil
}

// RefreshSkills keeps the skill copies sonarless-mcp installed at the
// running version, so an upgrade never leaves an older skill behind, and
// returns how many copies it rewrote. It is silent and best-effort.
//
// While the registry file does not exist yet, it adopts the copies already
// in the clients' skills directories. When this binary is a release newer
// than the version the copies were last synced to, and no managed copy is
// newer than it, it takes the registry lock without waiting and rewrites
// every managed copy (SkillRegistry.Sync): a copy the user deleted is
// forgotten, never re-created, a copy in a repository is left for
// `sonarless-mcp skill install --dir`, and a copy a newer sonarless-mcp wrote
// is never rewritten. Development builds write no copy, so two binaries on
// PATH never undo each other.
func RefreshSkills(e Env) int {
	reg, err := OpenSkillRegistry(e)
	if err != nil {
		return 0
	}
	if reg.Dirty() {
		_ = reg.Save()
	}
	if !skillSyncDue(reg) {
		return 0
	}
	unlock, ok := LockSkills(0)
	if !ok {
		return 0
	}
	defer unlock()
	// Another sonarless-mcp may have synced between the first read and the lock.
	if reg, err = OpenSkillRegistry(e); err != nil || !skillSyncDue(reg) {
		return 0
	}
	before := reg.Clone()
	results, _ := reg.Sync(Version)
	if !reg.Equal(before) {
		_ = reg.Save()
	}
	n := 0
	for _, r := range results {
		if r.Action == SyncUpdated {
			n++
		}
	}
	return n
}

// skillSyncDue reports whether this binary should rewrite the recorded
// copies: it is a release, newer than the version they were last synced to,
// and no managed copy is newer than it.
func skillSyncDue(reg *SkillRegistry) bool {
	v := Version
	if len(reg.Dirs) == 0 || !update.IsRelease(v) {
		return false
	}
	if update.IsRelease(reg.Synced) && !update.Newer(v, reg.Synced) {
		return false
	}
	for _, c := range reg.Copies(v) {
		if c.Managed && c.State == StateNewer {
			return false
		}
	}
	return true
}
