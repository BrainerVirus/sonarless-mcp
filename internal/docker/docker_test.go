package docker

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// fakeDocker puts a `docker` on PATH that logs its arguments and, for a
// container named "missing", fails the way the real CLI does.
func fakeDocker(t *testing.T) (logFile string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell-script fake docker")
	}
	dir := t.TempDir()
	logFile = filepath.Join(dir, "calls.log")
	script := "#!/bin/sh\necho \"$*\" >> '" + logFile + "'\n" +
		"case \"$*\" in *missing*) echo 'Error response from daemon: No such container: missing' >&2; exit 1 ;; esac\n"
	if err := os.WriteFile(filepath.Join(dir, "docker"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return logFile
}

func TestRemoveDropsAnonymousVolumes(t *testing.T) {
	calls := fakeDocker(t)
	if err := Remove(context.Background(), "perftest-server"); err != nil {
		t.Fatal(err)
	}
	log, _ := os.ReadFile(calls)
	if got := strings.TrimSpace(string(log)); got != "rm -fv perftest-server" {
		t.Errorf("docker called with %q, want %q (without -v the image's anonymous volumes leak on every recreate)", got, "rm -fv perftest-server")
	}
}

func TestRemoveMissingContainerIsNotAnError(t *testing.T) {
	fakeDocker(t)
	if err := Remove(context.Background(), "missing"); err != nil {
		t.Errorf("Remove of a missing container: %v, want nil", err)
	}
}
