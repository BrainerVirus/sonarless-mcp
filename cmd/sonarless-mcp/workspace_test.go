package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestHelperWorkspace(t *testing.T) {
	cfgDir, project := t.TempDir(), t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfgDir)
	orig := procInfo
	t.Cleanup(func() { procInfo = orig })

	t.Run("project-scoped server: the helper runs in the project", func(t *testing.T) {
		t.Chdir(project)
		procInfo = func(int) (string, int) { t.Fatal("asked the process tree"); return "", 0 }
		if got, _ := filepath.EvalSymlinks(helperWorkspace()); got != mustEval(t, project) {
			t.Errorf("got %q, want %q", got, project)
		}
	})
	t.Run("user-scoped server: the helper runs in the config dir", func(t *testing.T) {
		t.Chdir(cfgDir)
		shell, claude := 100, 200
		procInfo = func(pid int) (string, int) {
			switch pid {
			case os.Getppid():
				return cfgDir, shell // the shell Claude ran the helper in
			case shell:
				return cfgDir, claude
			case claude:
				return project, 1 // Claude Code itself, in the project
			}
			return "", 0
		}
		if got := helperWorkspace(); got != project {
			t.Errorf("got %q, want Claude Code's directory %q", got, project)
		}
	})
	t.Run("nothing known", func(t *testing.T) {
		t.Chdir(cfgDir)
		procInfo = func(int) (string, int) { return "", 0 } // e.g. Windows
		if got := helperWorkspace(); got != "" {
			t.Errorf("got %q, want no workspace", got)
		}
	})
}

func mustEval(t *testing.T, p string) string {
	t.Helper()
	r, err := filepath.EvalSymlinks(p)
	if err != nil {
		t.Fatal(err)
	}
	return r
}
