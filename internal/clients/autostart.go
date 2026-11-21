package clients

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// The gateway (`sonarless-mcp serve`) must be up before a direct client
// without a pre-connect hook connects, so setup starts it at login: a
// systemd user unit on Linux, a LaunchAgent on macOS, and on Windows a
// per-user Run key (no admin) that starts a tiny VBScript through wscript, a
// GUI program, which runs the console binary with a hidden window: no
// console flashes at login. Windows has no service manager, so there the
// gateway restarts itself after an update (detached, no window).

const (
	unitName    = "sonarless-mcp-gateway.service"
	launchLabel = "io.github.brainervirus.sonarless-mcp.gateway"
	runKey      = `HKCU\Software\Microsoft\Windows\CurrentVersion\Run`
	runValue    = "SonarlessMcpGateway"
	// SupervisedEnv tells the gateway a service manager restarts it, so on a
	// self-update it just exits instead of spawning its replacement.
	SupervisedEnv = "SONARLESS_GATEWAY_SUPERVISED"
)

// AutostartSupported reports whether this OS has a login autostart setup
// can manage.
func AutostartSupported(e Env) bool {
	switch e.GOOS {
	case "linux":
		return e.has("systemctl")
	case "darwin":
		return e.has("launchctl")
	case "windows":
		return e.has("reg") && e.has("wscript")
	}
	return false
}

// AutostartFile is the unit or LaunchAgent file ("" where unsupported).
func AutostartFile(e Env) string {
	switch e.GOOS {
	case "linux":
		return filepath.Join(e.appData(), "systemd", "user", unitName)
	case "darwin":
		return filepath.Join(e.Home, "Library", "LaunchAgents", launchLabel+".plist")
	case "windows":
		return filepath.Join(e.appData(), "sonarless-mcp", "gateway-autostart.vbs")
	}
	return ""
}

// AutostartInstalled reports whether setup installed the autostart entry.
func AutostartInstalled(e Env) bool { f := AutostartFile(e); return f != "" && isFile(f) }

// InstallAutostart writes the autostart entry for `exe serve` and starts
// (or restarts) the gateway through it. env is passed to the gateway (e.g.
// SONARLESS_HOME), so it reads the same config as the clients.
func InstallAutostart(e Env, exe, logFile string, env map[string]string) (string, error) {
	if !AutostartSupported(e) {
		return "", fmt.Errorf("no login autostart on %s", e.GOOS)
	}
	file := AutostartFile(e)
	if e.GOOS != "windows" {
		env = withSupervised(env)
	}
	var body string
	switch e.GOOS {
	case "windows":
		body = vbsLauncher(exe, env)
	case "linux":
		body = systemdUnit(exe, env)
	case "darwin":
		body = launchAgent(exe, logFile, env)
	}
	if err := writePrivateFile(file, []byte(body)); err != nil {
		return "", err
	}
	var err error
	switch e.GOOS {
	case "linux":
		if _, err = e.Run("systemctl", "--user", "daemon-reload"); err == nil {
			if _, err = e.Run("systemctl", "--user", "enable", unitName); err == nil {
				// restart, not start: a running gateway may be an older binary.
				_, err = e.Run("systemctl", "--user", "restart", unitName)
			}
		}
	case "darwin":
		domain := fmt.Sprintf("gui/%d", os.Getuid())
		_, _ = e.Run("launchctl", "bootout", domain+"/"+launchLabel)
		_, err = e.Run("launchctl", "bootstrap", domain, file)
	case "windows":
		if _, err = e.Run("reg", "add", runKey, "/v", runValue, "/t", "REG_SZ", "/d", `wscript.exe "`+file+`"`, "/f"); err == nil {
			_, err = e.Run("wscript", file) // start it now; returns at once
		}
	}
	if err != nil {
		_ = os.Remove(file) // half-installed is worse than none: callers fall back to the shim
		return "", err
	}
	return file, nil
}

// RemoveAutostart stops the gateway service and deletes its entry.
func RemoveAutostart(e Env) error {
	file := AutostartFile(e)
	if file == "" || !isFile(file) {
		return nil
	}
	switch e.GOOS {
	case "linux":
		_, _ = e.Run("systemctl", "--user", "disable", "--now", unitName)
	case "darwin":
		_, _ = e.Run("launchctl", "bootout", fmt.Sprintf("gui/%d/%s", os.Getuid(), launchLabel))
	case "windows":
		_, _ = e.Run("reg", "delete", runKey, "/v", runValue, "/f")
	}
	if err := os.Remove(file); err != nil {
		return err
	}
	if e.GOOS == "linux" {
		_, _ = e.Run("systemctl", "--user", "daemon-reload")
	}
	return nil
}

func withSupervised(env map[string]string) map[string]string {
	out := map[string]string{SupervisedEnv: "1"}
	for k, v := range env {
		out[k] = v
	}
	return out
}

func sortedEnv(env map[string]string) []string {
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys) // a stable file: re-running setup changes nothing
	return keys
}

// systemdQuote quotes a word for ExecStart/Environment lines.
func systemdQuote(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "%", "%%")
	return `"` + r.Replace(s) + `"`
}

func systemdUnit(exe string, env map[string]string) string {
	var b strings.Builder
	b.WriteString("# Written by `sonarless-mcp setup`; `sonarless-mcp setup --remove` deletes it.\n")
	b.WriteString("[Unit]\nDescription=sonarless-mcp gateway (one MCP endpoint for every AI client)\n\n[Service]\n")
	fmt.Fprintf(&b, "ExecStart=%s serve\n", systemdQuote(exe))
	for _, k := range sortedEnv(env) {
		fmt.Fprintf(&b, "Environment=%s\n", systemdQuote(k+"="+env[k]))
	}
	// KillMode=process: a restart must not take down the idle watcher the
	// gateway spawned (it outlives the gateway by design).
	b.WriteString("Restart=always\nRestartSec=2\nKillMode=process\n\n[Install]\nWantedBy=default.target\n")
	return b.String()
}

func xmlEscape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;").Replace(s)
}

func launchAgent(exe, logFile string, env map[string]string) string {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>` + launchLabel + `</string>
  <key>ProgramArguments</key><array><string>` + xmlEscape(exe) + `</string><string>serve</string></array>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
  <key>StandardOutPath</key><string>` + xmlEscape(logFile) + `</string>
  <key>StandardErrorPath</key><string>` + xmlEscape(logFile) + `</string>
  <key>EnvironmentVariables</key><dict>
`)
	for _, k := range sortedEnv(env) {
		fmt.Fprintf(&b, "    <key>%s</key><string>%s</string>\n", xmlEscape(k), xmlEscape(env[k]))
	}
	b.WriteString("  </dict>\n</dict>\n</plist>\n")
	return b.String()
}

// vbsLauncher starts `exe serve` with no window (Run style 0, don't wait).
func vbsLauncher(exe string, env map[string]string) string {
	q := func(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }
	var b strings.Builder
	b.WriteString("' Written by `sonarless-mcp setup`; `sonarless-mcp setup --remove` deletes it.\r\n")
	b.WriteString("Set sh = CreateObject(\"WScript.Shell\")\r\nSet env = sh.Environment(\"Process\")\r\n")
	for _, k := range sortedEnv(env) {
		fmt.Fprintf(&b, "env(%s) = %s\r\n", q(k), q(env[k]))
	}
	fmt.Fprintf(&b, "sh.Run %s, 0, False\r\n", q(`"`+exe+`" serve`))
	return b.String()
}
