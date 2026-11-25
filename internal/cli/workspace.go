package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

// helperWorkspace is the directory Claude Code runs in, for the workspace
// header `ensure --headers` prints. Claude runs the helper in the project
// for project-scoped servers but in its config dir (~/.claude) for
// user-scoped ones, and its PWD variable can be stale (started from a GUI),
// so when the helper's own directory is the config dir it asks the process
// tree: helper <- shell <- Claude Code. "" when nothing better is known.
func helperWorkspace() string {
	skip := os.Getenv("CLAUDE_CONFIG_DIR")
	if skip == "" {
		home, _ := os.UserHomeDir()
		skip = filepath.Join(home, ".claude")
	}
	usable := func(d string) bool {
		return d != "" && filepath.Dir(d) != d && filepath.Clean(d) != filepath.Clean(skip)
	}
	if wd, err := os.Getwd(); err == nil && usable(wd) {
		return wd
	}
	pid := os.Getppid()
	for i := 0; i < 4 && pid > 1; i++ {
		cwd, ppid := procInfo(pid)
		if usable(cwd) {
			return cwd
		}
		pid = ppid
	}
	return ""
}

// procInfo returns a process's working directory and parent ("" / 0 when
// the OS doesn't tell: Windows has no cheap way).
var procInfo = func(pid int) (cwd string, ppid int) {
	switch runtime.GOOS {
	case "linux":
		cwd, _ = os.Readlink("/proc/" + strconv.Itoa(pid) + "/cwd")
		if b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat"); err == nil {
			// pid (comm) state ppid ...; comm may contain spaces and parens.
			if i := strings.LastIndexByte(string(b), ')'); i >= 0 {
				if f := strings.Fields(string(b)[i+1:]); len(f) > 1 {
					ppid, _ = strconv.Atoi(f[1])
				}
			}
		}
	case "darwin":
		if out, err := exec.Command("ps", "-o", "ppid=", "-p", strconv.Itoa(pid)).Output(); err == nil {
			ppid, _ = strconv.Atoi(strings.TrimSpace(string(out)))
		}
		if out, err := exec.Command("lsof", "-a", "-p", strconv.Itoa(pid), "-d", "cwd", "-Fn").Output(); err == nil {
			for _, l := range strings.Split(string(out), "\n") {
				if strings.HasPrefix(l, "n") {
					cwd = l[1:]
				}
			}
		}
	}
	return cwd, ppid
}
