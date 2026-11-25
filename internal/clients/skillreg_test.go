package clients

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestMain keeps every test's skill registry out of the real config dir.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "sonarless-clients-test")
	if err != nil {
		panic(err)
	}
	_ = os.Setenv("SONARLESS_HOME", dir)
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

func setVersion(t *testing.T, v string) {
	t.Helper()
	old := Version
	Version = v
	t.Cleanup(func() { Version = old })
}

// skillEnv is a fresh home and registry; the client skills dirs live under it.
func skillEnv(t *testing.T) Env {
	t.Helper()
	t.Setenv("SONARLESS_HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("CODEX_HOME", "")
	return Env{Home: t.TempDir(), GOOS: "linux"}
}

func writeCopy(t *testing.T, dir, version string) {
	t.Helper()
	if _, err := InstallSkillDir(dir, version); err != nil {
		t.Fatal(err)
	}
}

func copyVersion(t *testing.T, dir string) string {
	t.Helper()
	b, err := os.ReadFile(skillFile(dir))
	if err != nil {
		t.Fatal(err)
	}
	return SkillVersionOf(b)
}

func TestRenderSkillStampsVersion(t *testing.T) {
	b := RenderSkill("1.2.3")
	if got := SkillVersionOf(b); got != "1.2.3" {
		t.Fatalf("version %q in:\n%.400s", got, b)
	}
	s := string(b)
	if !strings.HasPrefix(s, "---\nname: sonarless-mcp\n") || !strings.Contains(s, "  version: \"1.2.3\"\n---\n") || !strings.Contains(s, skillMarker) {
		t.Errorf("front matter:\n%.600s", s)
	}
	if SkillVersionOf([]byte(skillMD)) != "" {
		t.Error("the unstamped skill has a version")
	}
}

// statesFixture records copies at an older, the same and a newer version
// and a deleted one.
func statesFixture(t *testing.T) (Env, map[string]string) {
	e := skillEnv(t)
	dirs := map[string]string{
		"stale":   clientByID("claude").SkillDir(e),
		"current": clientByID("cursor").SkillDir(e),
		"newer":   clientByID("codex").SkillDir(e),
		"missing": clientByID("opencode").SkillDir(e),
	}
	writeCopy(t, dirs["stale"], "1.0.0")
	writeCopy(t, dirs["current"], "2.0.0")
	writeCopy(t, dirs["newer"], "3.0.0")
	writeCopy(t, dirs["missing"], "2.0.0")
	RecordSkills(e, []string{dirs["stale"], dirs["current"], dirs["newer"], dirs["missing"]}, "1.0.0")
	if err := os.RemoveAll(filepath.Join(dirs["missing"], skillName)); err != nil {
		t.Fatal(err)
	}
	return e, dirs
}

func TestSkillCopyStates(t *testing.T) {
	e, dirs := statesFixture(t)
	reg, err := OpenSkillRegistry(e)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, c := range reg.Copies("2.0.0") {
		got[c.Dir] = c.State
		if !c.Managed {
			t.Errorf("%s not managed", c.Dir)
		}
	}
	for state, dir := range dirs {
		if got[dir] != state {
			t.Errorf("%s: state %q, want %q", dir, got[dir], state)
		}
	}
}

func TestSyncKeepsNewerCopyUnlessForced(t *testing.T) {
	e, dirs := statesFixture(t)
	reg, _ := OpenSkillRegistry(e)
	results, ok := reg.Sync("2.0.0")
	if !ok {
		t.Fatalf("sync failed: %+v", results)
	}
	actions := map[string]string{}
	for _, r := range results {
		actions[r.Dir] = r.Action
	}
	want := map[string]string{"stale": SyncUpdated, "current": SyncUpdated, "newer": SyncKept, "missing": SyncDropped}
	for k, a := range want {
		if actions[dirs[k]] != a {
			t.Errorf("%s copy: %q, want %q", k, actions[dirs[k]], a)
		}
	}
	if v := copyVersion(t, dirs["newer"]); v != "3.0.0" {
		t.Errorf("newer copy rewritten to %s", v)
	}
	if v := copyVersion(t, dirs["stale"]); v != "2.0.0" {
		t.Errorf("stale copy at %s", v)
	}
	if len(reg.Dirs) != 3 || reg.Synced != "2.0.0" {
		t.Errorf("registry after sync: %+v", reg)
	}
	if _, err := os.Stat(filepath.Join(dirs["missing"], skillName)); !os.IsNotExist(err) {
		t.Error("sync re-created a missing copy")
	}

	results, _ = reg.SyncForce("2.0.0")
	for _, r := range results {
		if r.Dir == dirs["newer"] && r.Action != SyncUpdated {
			t.Errorf("forced: %q", r.Action)
		}
	}
	if v := copyVersion(t, dirs["newer"]); v != "2.0.0" {
		t.Errorf("--force left the newer copy at %s", v)
	}
}

func TestRefreshNeverRewritesNewerCopy(t *testing.T) {
	e, dirs := statesFixture(t)
	setVersion(t, "2.0.0")
	if n := RefreshSkills(e); n != 0 {
		t.Errorf("refresh rewrote %d copies with a newer one present", n)
	}
	if v := copyVersion(t, dirs["newer"]); v != "3.0.0" {
		t.Errorf("refresh rewrote the newer copy to %s", v)
	}

	// Without the newer copy, a newer release rewrites the stale ones.
	if err := os.RemoveAll(filepath.Join(dirs["newer"], skillName)); err != nil {
		t.Fatal(err)
	}
	if n := RefreshSkills(e); n != 2 {
		t.Errorf("refresh rewrote %d copies, want 2", n)
	}
	if v := copyVersion(t, dirs["stale"]); v != "2.0.0" {
		t.Errorf("stale copy at %s", v)
	}
	// Same version again: nothing to do.
	if n := RefreshSkills(e); n != 0 {
		t.Errorf("second refresh rewrote %d", n)
	}
}

func TestRefreshSkipsDevBuilds(t *testing.T) {
	e := skillEnv(t)
	dir := clientByID("claude").SkillDir(e)
	writeCopy(t, dir, "1.0.0")
	setVersion(t, "dev")
	if n := RefreshSkills(e); n != 0 || copyVersion(t, dir) != "1.0.0" {
		t.Errorf("a dev build rewrote %d copies", n)
	}
}

func TestOpenAdoptsExistingCopies(t *testing.T) {
	e := skillEnv(t)
	claude, cursor := clientByID("claude").SkillDir(e), clientByID("cursor").SkillDir(e)
	writeCopy(t, claude, "1.0.0")
	writeCopy(t, cursor, "1.5.0")
	user := clientByID("codex").SkillDir(e)
	_ = os.MkdirAll(filepath.Join(user, skillName), 0o755)
	_ = os.WriteFile(skillFile(user), []byte("my own notes"), 0o644)

	reg, err := OpenSkillRegistry(e)
	if err != nil {
		t.Fatal(err)
	}
	if !reg.Dirty() || len(reg.Dirs) != 2 || reg.Synced != "1.5.0" {
		t.Fatalf("adopted %+v", reg)
	}
	if err := reg.Save(); err != nil {
		t.Fatal(err)
	}
	if reg, _ = OpenSkillRegistry(e); reg.Dirty() || len(reg.Dirs) != 2 {
		t.Errorf("reopened %+v", reg)
	}
}

func TestRepoCopyIsManual(t *testing.T) {
	e := skillEnv(t)
	repo := filepath.Join(e.Home, "src", "app")
	_ = os.MkdirAll(filepath.Join(repo, ".git"), 0o755)
	dir := filepath.Join(repo, ".claude", "skills")
	writeCopy(t, dir, "1.0.0")
	if ManagedSkillDir(e, dir) {
		t.Error("a copy in a git work tree is managed")
	}
	if !ManagedSkillDir(e, filepath.Join(e.Home, "skills")) {
		t.Error("a copy under home outside a repo is not managed")
	}
	RecordSkills(e, []string{dir}, "1.0.0")
	reg, _ := OpenSkillRegistry(e)
	results, _ := reg.Sync("2.0.0")
	if len(results) != 1 || results[0].Action != SyncManual || copyVersion(t, dir) != "1.0.0" {
		t.Errorf("sync touched the repo copy: %+v", results)
	}
}

func TestUninstallRefusesForeignSkill(t *testing.T) {
	dir := t.TempDir()
	_ = os.MkdirAll(filepath.Join(dir, skillName), 0o755)
	_ = os.WriteFile(skillFile(dir), []byte("my own notes"), 0o644)
	if _, err := UninstallSkillDir(dir); !isForeign(err) {
		t.Errorf("err %v", err)
	}
	if _, err := InstallSkillDir(dir, "1.0.0"); !isForeign(err) {
		t.Errorf("install err %v", err)
	}
	if b, _ := os.ReadFile(skillFile(dir)); string(b) != "my own notes" {
		t.Error("touched the user's skill")
	}
}
