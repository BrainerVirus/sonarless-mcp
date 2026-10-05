//go:build !windows

package idle

import (
	"os/exec"
	"syscall"
)

// detach runs the watcher in its own session so it outlives the MCP client
// that started it and never holds the client's terminal or pipes.
func detach(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}
