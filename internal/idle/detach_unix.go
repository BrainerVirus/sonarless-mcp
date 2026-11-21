//go:build !windows

package idle

import (
	"os"
	"os/exec"
	"strconv"
	"syscall"
)

// detach runs the watcher in its own session so it outlives the MCP client
// that started it and never holds the client's terminal or pipes. Descriptors
// this process inherited (a caller's `flock` lock on fd 3, #1) get
// close-on-exec first: Go marks only the files it opens itself, so an
// inherited one would otherwise stay open in the daemon for its whole life.
func detach(cmd *exec.Cmd) {
	closeInheritedOnExec()
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}

// closeInheritedOnExec sets close-on-exec on every open descriptor above
// stderr. Go's own descriptors already have it, so only inherited ones change.
func closeInheritedOnExec() {
	dir := "/proc/self/fd"
	if _, err := os.Stat(dir); err != nil {
		dir = "/dev/fd"
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if fd, err := strconv.Atoi(e.Name()); err == nil && fd > 2 {
			syscall.CloseOnExec(fd)
		}
	}
}
