package mcp

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/BrainerVirus/sonarless-mcp/internal/config"
)

// The running gateway records itself in the cache dir so `setup --remove`
// can stop it where no service manager does (Windows, or a gateway Claude
// Code's helper started). Before stopping, the pid is checked to still be
// a sonarless-mcp gateway: a pid the OS reused is never touched.

type gatewayPID struct {
	PID     int       `json:"pid"`
	Exe     string    `json:"exe"`
	Started time.Time `json:"started"`
}

func gatewayPIDFile(cfg *config.Config) string { return filepath.Join(cfg.CacheDir, "gateway.pid") }

func writeGatewayPID(cfg *config.Config) error {
	exe, _ := os.Executable()
	b, _ := json.Marshal(gatewayPID{PID: os.Getpid(), Exe: exe, Started: time.Now()})
	if err := os.MkdirAll(cfg.CacheDir, 0o755); err != nil {
		return err
	}
	return os.WriteFile(gatewayPIDFile(cfg), b, 0o644)
}

// removeGatewayPID deletes the record if it is this process's.
func removeGatewayPID(cfg *config.Config) {
	if p, ok := readGatewayPID(cfg); ok && p.PID == os.Getpid() {
		_ = os.Remove(gatewayPIDFile(cfg))
	}
}

func readGatewayPID(cfg *config.Config) (gatewayPID, bool) {
	var p gatewayPID
	b, err := os.ReadFile(gatewayPIDFile(cfg))
	if err != nil || json.Unmarshal(b, &p) != nil || p.PID <= 0 {
		return p, false
	}
	return p, true
}

// StopGateway stops the gateway recorded in the cache dir, if that process
// is still a sonarless-mcp gateway. It reports whether one was stopped.
func StopGateway(cfg *config.Config) (bool, error) {
	p, ok := readGatewayPID(cfg)
	if !ok {
		return false, nil
	}
	defer os.Remove(gatewayPIDFile(cfg)) // stopped now, or stale
	if !isGateway(processCommand(p.PID), p.Exe) {
		return false, nil // gone, or the pid now belongs to something else
	}
	return true, terminate(p.PID)
}

// isGateway reports whether a command line runs `<exe> serve` (same binary
// name; Windows names compare case-insensitively).
func isGateway(cmdline, exe string) bool {
	f := strings.Fields(cmdline)
	if len(f) < 2 || f[len(f)-1] != "serve" || exe == "" {
		return false
	}
	base := filepath.Base(exe)
	if i := strings.LastIndexAny(base, `/\`); i >= 0 {
		base = base[i+1:]
	}
	return strings.Contains(strings.ToLower(cmdline), strings.ToLower(base))
}

// processCommand returns a running process's command line ("" if it is
// gone or the OS won't say).
var processCommand = func(pid int) string {
	id := strconv.Itoa(pid)
	switch runtime.GOOS {
	case "linux":
		b, err := os.ReadFile("/proc/" + id + "/cmdline")
		if err != nil {
			return ""
		}
		return strings.TrimSpace(strings.ReplaceAll(string(b), "\x00", " "))
	case "windows":
		out, err := exec.Command("powershell", "-NoProfile", "-NonInteractive", "-Command",
			"(Get-CimInstance Win32_Process -Filter 'ProcessId="+id+"').CommandLine").Output()
		if err != nil {
			return ""
		}
		return strings.TrimSpace(string(out))
	default:
		out, err := exec.Command("ps", "-o", "command=", "-p", id).Output()
		if err != nil {
			return ""
		}
		return strings.TrimSpace(string(out))
	}
}

// terminate asks the gateway to stop: SIGTERM (it drains requests) where
// there are signals, TerminateProcess on Windows.
var terminate = func(pid int) error {
	p, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	if runtime.GOOS == "windows" {
		return p.Kill()
	}
	return p.Signal(syscall.SIGTERM)
}
