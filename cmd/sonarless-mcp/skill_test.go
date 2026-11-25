package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/BrainerVirus/sonarless-mcp/internal/clients"
)

// TestMain keeps the skill registry of every test in this package (the
// refresh runs with each auto-update) out of the real config dir.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "sonarless-cmd-test")
	if err != nil {
		panic(err)
	}
	_ = os.Setenv("SONARLESS_HOME", dir)
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

// skillTestEnv points the skill commands at a fresh home and registry and
// runs them as release 2.0.0.
func skillTestEnv(t *testing.T) clients.Env {
	t.Helper()
	t.Setenv("SONARLESS_HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("CODEX_HOME", "")
	e := clients.Env{Home: t.TempDir(), GOOS: runtime.GOOS, Look: func(string) (string, error) { return "", os.ErrNotExist }}
	oldEnv, oldVersion := skillEnv, version
	skillEnv, version = func() clients.Env { return e }, "2.0.0"
	t.Cleanup(func() { skillEnv, version = oldEnv, oldVersion })
	return e
}

func runSkill(t *testing.T, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	root := newRootCommand(context.Background())
	var out, errb bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&errb)
	root.SetArgs(append([]string{"skill"}, args...))
	err = root.ExecuteContext(context.Background())
	return out.String(), errb.String(), err
}

func skillDirOf(e clients.Env, id string) string {
	for _, c := range clients.All() {
		if c.ID == id {
			return c.SkillDir(e)
		}
	}
	return ""
}

// skillFixture records copies at an older, the same and a newer version and
// a deleted one.
func skillFixture(t *testing.T) map[string]string {
	e := skillTestEnv(t)
	dirs := map[string]string{
		"stale":   skillDirOf(e, "claude"),
		"current": skillDirOf(e, "cursor"),
		"newer":   skillDirOf(e, "codex"),
		"missing": skillDirOf(e, "opencode"),
	}
	for state, v := range map[string]string{"stale": "1.0.0", "current": "2.0.0", "newer": "3.0.0", "missing": "2.0.0"} {
		if _, err := clients.InstallSkillDir(dirs[state], v); err != nil {
			t.Fatal(err)
		}
	}
	clients.RecordSkills(e, []string{dirs["stale"], dirs["current"], dirs["newer"], dirs["missing"]}, "2.0.0")
	if err := os.RemoveAll(filepath.Join(dirs["missing"], "sonarless-mcp")); err != nil {
		t.Fatal(err)
	}
	return dirs
}

func TestSkillListShowsEachState(t *testing.T) {
	dirs := skillFixture(t)
	out, errOut, err := runSkill(t, "list")
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(out, "\n")
	for state, dir := range dirs {
		found := false
		for _, l := range lines {
			if f := strings.Fields(l); len(f) == 4 && f[0] == dir && f[2] == state && f[3] == "auto" {
				found = true
			}
		}
		if !found {
			t.Errorf("no %s row for %s in:\n%s", state, dir, out)
		}
	}
	for _, hint := range []string{"sonarless-mcp skill sync` to update the stale copies", "A newer sonarless-mcp (3.0.0)", "sonarless-mcp upgrade", "forgets the missing copies"} {
		if !strings.Contains(errOut, hint) {
			t.Errorf("hint %q missing from:\n%s", hint, errOut)
		}
	}

	out, _, err = runSkill(t, "list", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var rows []struct {
		Dir, Path, Version, State string
		Managed                   bool
	}
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		t.Fatalf("%v in %s", err, out)
	}
	got := map[string]string{}
	for _, r := range rows {
		got[r.Dir] = r.State
	}
	for state, dir := range dirs {
		if got[dir] != state {
			t.Errorf("--json: %s is %q, want %q", dir, got[dir], state)
		}
	}
}

func TestSkillListHintsRepoCopy(t *testing.T) {
	e := skillTestEnv(t)
	repo := filepath.Join(e.Home, "src", "app")
	_ = os.MkdirAll(filepath.Join(repo, ".git"), 0o755)
	dir := filepath.Join(repo, ".claude", "skills")
	if _, err := clients.InstallSkillDir(dir, "1.0.0"); err != nil {
		t.Fatal(err)
	}
	clients.RecordSkills(e, []string{dir}, "1.0.0")
	out, errOut, _ := runSkill(t, "list")
	if !strings.Contains(out, "manual") || !strings.Contains(errOut, "skill install --dir "+dir) {
		t.Errorf("out:\n%s\nerr:\n%s", out, errOut)
	}
}

func TestSkillSyncKeepsNewerUnlessForced(t *testing.T) {
	dirs := skillFixture(t)
	out, _, err := runSkill(t, "sync")
	if err != nil {
		t.Fatal(err)
	}
	newer := filepath.Join(dirs["newer"], "sonarless-mcp")
	for _, want := range []string{
		"kept\t" + newer + "\t3.0.0",
		"updated\t" + filepath.Join(dirs["stale"], "sonarless-mcp") + "\t2.0.0",
		"dropped\t" + filepath.Join(dirs["missing"], "sonarless-mcp") + "\t(missing)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("sync: %q missing from:\n%s", want, out)
		}
	}
	if b, _ := os.ReadFile(filepath.Join(newer, "SKILL.md")); clients.SkillVersionOf(b) != "3.0.0" {
		t.Error("sync rewrote the newer copy")
	}

	out, _, err = runSkill(t, "sync", "--force")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "updated\t"+newer+"\t2.0.0") || strings.Contains(out, "dropped") {
		t.Errorf("sync --force:\n%s", out)
	}
	if b, _ := os.ReadFile(filepath.Join(newer, "SKILL.md")); clients.SkillVersionOf(b) != "2.0.0" {
		t.Error("--force left the newer copy")
	}
}

func TestSkillInstallUninstallPrint(t *testing.T) {
	e := skillTestEnv(t)
	dir := filepath.Join(e.Home, "skills")
	out, _, err := runSkill(t, "install", "--dir", dir)
	file := filepath.Join(dir, "sonarless-mcp", "SKILL.md")
	if err != nil || strings.TrimSpace(out) != file {
		t.Fatalf("install: %q %v", out, err)
	}
	if b, _ := os.ReadFile(file); clients.SkillVersionOf(b) != "2.0.0" {
		t.Errorf("installed:\n%.300s", b)
	}
	if out, _, _ := runSkill(t, "list", "--json"); !strings.Contains(out, `"state":"current"`) {
		t.Errorf("not recorded: %s", out)
	}
	if out, _, _ := runSkill(t, "print"); !strings.Contains(out, `version: "2.0.0"`) {
		t.Errorf("print:\n%.300s", out)
	}
	if _, _, err := runSkill(t, "install"); err == nil || !strings.Contains(err.Error(), "--dir") {
		t.Errorf("install with no client found: %v", err)
	}

	if _, _, err := runSkill(t, "uninstall"); err == nil {
		t.Error("uninstall without --dir")
	}
	if _, errOut, err := runSkill(t, "uninstall", "--dir", dir); err != nil || !strings.Contains(errOut, "Removed") {
		t.Fatalf("uninstall: %q %v", errOut, err)
	}
	if _, err := os.Stat(file); !os.IsNotExist(err) {
		t.Error("skill left behind")
	}
	if out, _, _ := runSkill(t, "list", "--json"); strings.TrimSpace(out) != "[]" {
		t.Errorf("still recorded: %s", out)
	}

	// A user's same-named skill is refused both ways.
	_ = os.MkdirAll(filepath.Dir(file), 0o755)
	_ = os.WriteFile(file, []byte("mine"), 0o644)
	if _, _, err := runSkill(t, "install", "--dir", dir); err == nil || !strings.Contains(err.Error(), "did not install") {
		t.Errorf("install over a user skill: %v", err)
	}
	if _, _, err := runSkill(t, "uninstall", "--dir", dir); err == nil {
		t.Error("uninstall removed a user skill")
	}
	if b, _ := os.ReadFile(file); string(b) != "mine" {
		t.Error("user skill changed")
	}
}

func TestSkillInstallRelativeDirPrintsAbsolutePath(t *testing.T) {
	e := skillTestEnv(t)
	t.Chdir(e.Home)
	out, errOut, err := runSkill(t, "install", "--dir", filepath.Join("repo", "skills"))
	want := filepath.Join(e.Home, "repo", "skills", "sonarless-mcp", "SKILL.md")
	if real, rerr := filepath.EvalSymlinks(e.Home); rerr == nil && !strings.Contains(out, e.Home) {
		want = filepath.Join(real, "repo", "skills", "sonarless-mcp", "SKILL.md")
	}
	if err != nil || strings.TrimSpace(out) != want {
		t.Errorf("printed %q, want %q (%v)", out, want, err)
	}
	if !strings.Contains(errOut, "`sonarless-mcp skill list` shows every copy") {
		t.Errorf("stderr = %q", errOut)
	}
	if reg, _ := clients.OpenSkillRegistry(e); len(reg.Dirs) != 1 || reg.Dirs[0] != filepath.Dir(filepath.Dir(want)) {
		t.Errorf("recorded %v", reg.Dirs)
	}
}
