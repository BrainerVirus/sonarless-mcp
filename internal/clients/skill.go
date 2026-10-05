package clients

import (
	_ "embed"
	"os"
	"path/filepath"
	"strings"
)

//go:embed skill/SKILL.md
var skillMD string

const (
	skillName   = "sonarless-mcp"
	skillMarker = "managed by sonarless-mcp setup"
)

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

// InstallSkill writes the sonarless-mcp skill for the client. It never
// replaces a same-named skill the user wrote. Returns the file written ("" if
// the client has no skills or the slot belongs to the user).
func (c Client) InstallSkill(e Env) (string, error) {
	dir := c.SkillDir(e)
	if dir == "" {
		return "", nil
	}
	f := skillFile(dir)
	if _, ours := ownSkill(f); !ours {
		return "", nil
	}
	return f, writeFile(f, []byte(skillMD))
}

// RemoveSkill deletes the skill if it is ours.
func (c Client) RemoveSkill(e Env) error {
	dir := c.SkillDir(e)
	if dir == "" {
		return nil
	}
	f := skillFile(dir)
	if exists, ours := ownSkill(f); !exists || !ours {
		return nil
	}
	if err := os.Remove(f); err != nil {
		return err
	}
	_ = os.Remove(filepath.Dir(f)) // only if now empty
	return nil
}

// RefreshSkills rewrites installed copies of the skill (after an update) and
// returns how many it touched; it never installs into new clients.
func RefreshSkills(e Env) int {
	n := 0
	for _, c := range All() {
		dir := c.SkillDir(e)
		if dir == "" {
			continue
		}
		f := skillFile(dir)
		if exists, ours := ownSkill(f); exists && ours {
			if b, _ := os.ReadFile(f); string(b) != skillMD && writeFile(f, []byte(skillMD)) == nil {
				n++
			}
		}
	}
	return n
}
