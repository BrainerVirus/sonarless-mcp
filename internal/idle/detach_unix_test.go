//go:build !windows

package idle

import (
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Issue #1: a descriptor this process inherited (a caller's `flock` lock on
// fd 3) must not reach the detached daemon, or the lock is held forever.
func TestDetachedChildDoesNotInheritInheritedFds(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "lock")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	fd := int(f.Fd())
	// Inherited descriptors arrive without close-on-exec; make this one look like that.
	if _, _, e := syscall.Syscall(syscall.SYS_FCNTL, uintptr(fd), syscall.F_SETFD, 0); e != 0 {
		t.Fatal(e)
	}
	out, err := os.CreateTemp(t.TempDir(), "out")
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	cmd := exec.Command("sh", "-c", "ls /dev/fd")
	cmd.Stdout = out
	detach(cmd)
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(out.Name())
	for _, name := range strings.Fields(string(b)) {
		if name == strconv.Itoa(fd) {
			t.Fatalf("the detached child inherited fd %d; its fds: %s", fd, b)
		}
	}
}

// A detached child that exits while the spawner lives on is reaped, not left <defunct>.
func TestSpawnedChildIsReaped(t *testing.T) {
	pidFile := t.TempDir() + "/pid"
	cmd := exec.Command("sh", "-c", `echo $$ > "$0"`, pidFile)
	detach(cmd)
	if err := start(cmd); err != nil {
		t.Fatal(err)
	}
	var pid int
	for i := 0; i < 250 && pid <= 0; i++ {
		b, _ := os.ReadFile(pidFile)
		pid, _ = strconv.Atoi(strings.TrimSpace(string(b)))
		time.Sleep(20 * time.Millisecond)
	}
	if pid <= 0 {
		t.Fatal("the child never wrote its pid")
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); err == syscall.ESRCH {
			return // gone: reaped
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("child %d still exists (a zombie) 5s after it exited", pid)
}
