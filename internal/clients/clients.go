// Package clients detects AI clients (Claude Code, Cursor, opencode, VS Code,
// Codex) and registers or removes the `sonarless-mcp mcp` server in each one's
// own config. Presence means a real executable or app install; a config dir
// alone doesn't count.
package clients

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// ServerName is the MCP server name registered in every client.
const ServerName = "sonarqube"

// Client is one supported AI client.
type Client struct {
	ID    string
	Label string
	// Detected: the client is installed. Configured: sonarqube is registered.
	Detected   func(env Env) bool
	Configured func(env Env) bool
	Register   func(env Env, exe string) (name string, err error)
	Remove     func(env Env) error
	// Where says which file or command a change goes to.
	Where func(env Env) string
}

// Env is what detection and registration look at (overridable in tests).
type Env struct {
	Home string
	GOOS string
	Look func(name string) (string, error) // PATH lookup
	Run  func(name string, args ...string) (string, error)
	App  func(path string) bool // an app install path exists
}

// DefaultEnv uses the real home, OS and PATH (plus common user bin dirs that
// non-interactive shells often lack).
func DefaultEnv() Env {
	home, _ := os.UserHomeDir()
	e := Env{Home: home, GOOS: runtime.GOOS, App: exists}
	e.Look = func(name string) (string, error) {
		if p, err := exec.LookPath(name); err == nil {
			return p, nil
		}
		for _, d := range extraBinDirs(e) {
			for _, n := range exeNames(e, name) {
				if p := filepath.Join(d, n); isFile(p) {
					return p, nil
				}
			}
		}
		return "", exec.ErrNotFound
	}
	e.Run = func(name string, args ...string) (string, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		path, err := e.Look(name)
		if err != nil {
			return "", err
		}
		out, err := exec.CommandContext(ctx, path, args...).CombinedOutput()
		if err != nil {
			return string(out), fmt.Errorf("%s %s: %v: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
		}
		return string(out), nil
	}
	return e
}

func extraBinDirs(e Env) []string {
	h := e.Home
	dirs := []string{filepath.Join(h, ".local", "bin"), filepath.Join(h, ".opencode", "bin"),
		filepath.Join(h, ".claude", "local"), filepath.Join(h, ".local", "share", "fnm", "aliases", "default", "bin"),
		filepath.Join(h, ".npm-global", "bin"), filepath.Join(h, ".bun", "bin"), filepath.Join(h, ".volta", "bin")}
	switch e.GOOS {
	case "darwin":
		dirs = append(dirs, "/opt/homebrew/bin", "/usr/local/bin")
	case "windows":
		if a := os.Getenv("APPDATA"); a != "" {
			dirs = append(dirs, filepath.Join(a, "npm"))
		}
	}
	return dirs
}

func exeNames(e Env, name string) []string {
	if e.GOOS == "windows" {
		return []string{name + ".exe", name + ".cmd", name}
	}
	return []string{name}
}

func (e Env) app(p string) bool { return e.App != nil && e.App(p) }

func (e Env) has(name string) bool { _, err := e.Look(name); return err == nil }

func isFile(p string) bool { fi, err := os.Stat(p); return err == nil && !fi.IsDir() }
func exists(p string) bool { _, err := os.Stat(p); return err == nil }

// appData is the per-OS dir where editors keep user settings.
func (e Env) appData() string {
	switch e.GOOS {
	case "darwin":
		return filepath.Join(e.Home, "Library", "Application Support")
	case "windows":
		if a := os.Getenv("APPDATA"); a != "" {
			return a
		}
		return filepath.Join(e.Home, "AppData", "Roaming")
	}
	if x := os.Getenv("XDG_CONFIG_HOME"); x != "" {
		return x
	}
	return filepath.Join(e.Home, ".config")
}

func (e Env) localAppData() string {
	if a := os.Getenv("LOCALAPPDATA"); a != "" {
		return a
	}
	return filepath.Join(e.Home, "AppData", "Local")
}

// All returns the supported clients in display order.
func All() []Client {
	return []Client{claude(), cursor(), opencode(), vscode(), codex()}
}

// --- ownership: only entries that run sonarless-mcp are ours ---

// AltServerName is used when the user already has a different "sonarqube"
// server (e.g. a remote SonarQube); that entry is never overwritten.
const AltServerName = "sonarqube-local"

var names = []string{ServerName, AltServerName}

// ours reports whether a config entry launches sonarless-mcp.
func ours(entry string) bool { return strings.Contains(entry, "sonarless-mcp") }

// pickName returns the name to register under: ServerName unless it is taken
// by someone else's server.
func pickName(entry func(name string) (string, bool)) string {
	if e, ok := entry(ServerName); ok && !ours(e) {
		return AltServerName
	}
	return ServerName
}

// --- Claude Code: managed through its own CLI ---

func claude() Client {
	entry := func(e Env) func(string) (string, bool) {
		return func(name string) (string, bool) {
			out, err := e.Run("claude", "mcp", "get", name)
			return out, err == nil
		}
	}
	return Client{
		ID: "claude", Label: "Claude Code",
		Detected: func(e Env) bool { return e.has("claude") },
		Configured: func(e Env) bool {
			if !e.has("claude") {
				return false
			}
			for _, n := range names {
				if out, ok := entry(e)(n); ok && ours(out) {
					return true
				}
			}
			return false
		},
		Register: func(e Env, exe string) (string, error) {
			name := pickName(entry(e))
			_, _ = e.Run("claude", "mcp", "remove", "-s", "user", name)
			_, err := e.Run("claude", "mcp", "add", "-s", "user", name, "--", exe, "mcp")
			return name, err
		},
		Remove: func(e Env) error {
			for _, n := range names {
				if out, ok := entry(e)(n); ok && ours(out) {
					if _, err := e.Run("claude", "mcp", "remove", "-s", "user", n); err != nil {
						return err
					}
				}
			}
			return nil
		},
		Where: func(Env) string { return "claude mcp (user scope)" },
	}
}

// jsonClient wires a client whose MCP servers live in one JSON object.
func jsonClient(id, label string, file func(Env) string, section string, detected func(Env) bool, value func(exe string) map[string]any) Client {
	entry := func(e Env) func(string) (string, bool) {
		return func(name string) (string, bool) { return jsonEntry(file(e), section, name) }
	}
	return Client{
		ID: id, Label: label, Detected: detected, Where: file,
		Configured: func(e Env) bool {
			for _, n := range names {
				if v, ok := entry(e)(n); ok && ours(v) {
					return true
				}
			}
			return false
		},
		Register: func(e Env, exe string) (string, error) {
			name := pickName(entry(e))
			return name, jsonSet(file(e), section, name, value(exe))
		},
		Remove: func(e Env) error {
			for _, n := range names {
				if v, ok := entry(e)(n); ok && ours(v) {
					if err := jsonDelete(file(e), section, n); err != nil {
						return err
					}
				}
			}
			return nil
		},
	}
}

// --- Cursor: ~/.cursor/mcp.json ---

func cursor() Client {
	return jsonClient("cursor", "Cursor",
		func(e Env) string { return filepath.Join(e.Home, ".cursor", "mcp.json") }, "mcpServers",
		func(e Env) bool {
			switch e.GOOS {
			case "darwin":
				if e.app("/Applications/Cursor.app") || e.app(filepath.Join(e.Home, "Applications", "Cursor.app")) {
					return true
				}
			case "windows":
				if e.app(filepath.Join(e.localAppData(), "Programs", "cursor", "Cursor.exe")) {
					return true
				}
			default:
				if e.app("/usr/share/cursor") || e.app("/opt/Cursor") || e.app("/opt/cursor") {
					return true
				}
			}
			return e.has("cursor")
		},
		func(exe string) map[string]any {
			return map[string]any{"command": exe, "args": []string{"mcp", "${workspaceFolder}"}}
		})
}

// --- opencode: ~/.config/opencode/opencode.json ---

func opencode() Client {
	return jsonClient("opencode", "opencode",
		func(e Env) string {
			if x := os.Getenv("XDG_CONFIG_HOME"); x != "" && e.GOOS != "windows" {
				return filepath.Join(x, "opencode", "opencode.json")
			}
			return filepath.Join(e.Home, ".config", "opencode", "opencode.json")
		}, "mcp",
		func(e Env) bool { return e.has("opencode") },
		func(exe string) map[string]any {
			return map[string]any{"type": "local", "enabled": true, "command": []string{exe, "mcp"}}
		})
}

// --- VS Code: user mcp.json ---

func vscode() Client {
	return jsonClient("vscode", "VS Code",
		func(e Env) string { return filepath.Join(e.appData(), "Code", "User", "mcp.json") }, "servers",
		func(e Env) bool {
			switch e.GOOS {
			case "darwin":
				if e.app("/Applications/Visual Studio Code.app") {
					return true
				}
			case "windows":
				if e.app(filepath.Join(e.localAppData(), "Programs", "Microsoft VS Code", "Code.exe")) {
					return true
				}
			}
			return e.has("code")
		},
		func(exe string) map[string]any {
			return map[string]any{"type": "stdio", "command": exe, "args": []string{"mcp", "${workspaceFolder}"}}
		})
}

// --- Codex: ~/.codex/config.toml ---

func codex() Client {
	file := func(e Env) string {
		if h := os.Getenv("CODEX_HOME"); h != "" {
			return filepath.Join(h, "config.toml")
		}
		return filepath.Join(e.Home, ".codex", "config.toml")
	}
	header := func(name string) string { return "[mcp_servers." + name + "]" }
	entry := func(e Env) func(string) (string, bool) {
		return func(name string) (string, bool) {
			b, err := os.ReadFile(file(e))
			if err != nil {
				return "", false
			}
			return tomlBody(string(b), header(name))
		}
	}
	return Client{
		ID: "codex", Label: "Codex", Where: file,
		Detected: func(e Env) bool { return e.has("codex") },
		Configured: func(e Env) bool {
			for _, n := range names {
				if v, ok := entry(e)(n); ok && ours(v) {
					return true
				}
			}
			return false
		},
		Register: func(e Env, exe string) (string, error) {
			name := pickName(entry(e))
			q, _ := json.Marshal(exe) // TOML basic strings share JSON's escaping
			block := header(name) + "\ncommand = " + string(q) + "\nargs = [\"mcp\"]\n"
			return name, tomlReplace(file(e), header(name), block)
		},
		Remove: func(e Env) error {
			for _, n := range names {
				if v, ok := entry(e)(n); ok && ours(v) {
					if err := tomlReplace(file(e), header(n), ""); err != nil {
						return err
					}
				}
			}
			return nil
		},
	}
}

// --- ordered JSON editing (keeps the user's key order and other settings) ---

type kv struct {
	Key string
	Val json.RawMessage
}

func parseObject(b []byte) ([]kv, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return nil, errors.New("not a JSON object")
	}
	var out []kv
	for dec.More() {
		t, err := dec.Token()
		if err != nil {
			return nil, err
		}
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil {
			return nil, err
		}
		out = append(out, kv{t.(string), v})
	}
	return out, nil
}

func encodeObject(obj []kv) []byte {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, e := range obj {
		if i > 0 {
			b.WriteByte(',')
		}
		k, _ := json.Marshal(e.Key)
		b.Write(k)
		b.WriteByte(':')
		b.Write(e.Val)
	}
	b.WriteByte('}')
	var out bytes.Buffer
	if json.Indent(&out, b.Bytes(), "", "  ") != nil {
		return b.Bytes()
	}
	out.WriteByte('\n')
	return out.Bytes()
}

func get(obj []kv, key string) (json.RawMessage, bool) {
	for _, e := range obj {
		if e.Key == key {
			return e.Val, true
		}
	}
	return nil, false
}

func put(obj []kv, key string, val json.RawMessage) []kv {
	for i := range obj {
		if obj[i].Key == key {
			obj[i].Val = val
			return obj
		}
	}
	return append(obj, kv{key, val})
}

func readObject(file string) ([]kv, error) {
	b, err := os.ReadFile(file)
	if os.IsNotExist(err) || len(bytes.TrimSpace(b)) == 0 {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	obj, err := parseObject(b)
	if err != nil {
		return nil, fmt.Errorf("%s: %v (only plain JSON is edited; fix or add the entry by hand)", file, err)
	}
	return obj, nil
}

// jsonEntry returns the raw JSON of section.name, if present.
func jsonEntry(file, section, name string) (string, bool) {
	obj, err := readObject(file)
	if err != nil {
		return "", false
	}
	sec, ok := get(obj, section)
	if !ok {
		return "", false
	}
	inner, err := parseObject(sec)
	if err != nil {
		return "", false
	}
	v, ok := get(inner, name)
	return string(v), ok
}

func jsonHas(file, section, name string) bool { _, ok := jsonEntry(file, section, name); return ok }

func jsonSet(file, section, name string, value any) error {
	obj, err := readObject(file)
	if err != nil {
		return err
	}
	var inner []kv
	if sec, ok := get(obj, section); ok {
		if inner, err = parseObject(sec); err != nil {
			return fmt.Errorf("%s: %q is not an object", file, section)
		}
	}
	v, _ := json.Marshal(value)
	inner = put(inner, name, v)
	obj = put(obj, section, encodeCompact(inner))
	return writeFile(file, encodeObject(obj))
}

func jsonDelete(file, section, name string) error {
	obj, err := readObject(file)
	if err != nil || obj == nil {
		return err
	}
	sec, ok := get(obj, section)
	if !ok {
		return nil
	}
	inner, err := parseObject(sec)
	if err != nil {
		return nil
	}
	kept := inner[:0]
	for _, e := range inner {
		if e.Key != name {
			kept = append(kept, e)
		}
	}
	obj = put(obj, section, encodeCompact(kept))
	return writeFile(file, encodeObject(obj))
}

func encodeCompact(obj []kv) json.RawMessage {
	var b bytes.Buffer
	_ = json.Compact(&b, encodeObject(obj))
	return b.Bytes()
}

// --- TOML block editing ---

// tomlBlock returns the line index of header in s, or -1.
func tomlBlock(s, header string) int {
	for i, l := range strings.Split(s, "\n") {
		if strings.TrimSpace(l) == header {
			return i
		}
	}
	return -1
}

// tomlBody returns the text of the table starting at header, if present.
func tomlBody(s, header string) (string, bool) {
	lines := strings.Split(s, "\n")
	start := tomlBlock(s, header)
	if start < 0 {
		return "", false
	}
	end := len(lines)
	for i := start + 1; i < len(lines); i++ {
		if strings.HasPrefix(strings.TrimSpace(lines[i]), "[") {
			end = i
			break
		}
	}
	return strings.Join(lines[start:end], "\n"), true
}

// tomlReplace replaces the table starting at header (up to the next table
// header) with block, appends block if absent, or deletes it when block is "".
func tomlReplace(file, header, block string) error {
	b, err := os.ReadFile(file)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	lines := strings.Split(string(b), "\n")
	start := tomlBlock(string(b), header)
	if start < 0 {
		if block == "" {
			return nil
		}
		s := strings.TrimRight(string(b), "\n")
		if s != "" {
			s += "\n\n"
		}
		return writeFile(file, []byte(s+block))
	}
	end := len(lines)
	for i := start + 1; i < len(lines); i++ {
		if t := strings.TrimSpace(lines[i]); strings.HasPrefix(t, "[") {
			end = i
			break
		}
	}
	var out []string
	out = append(out, lines[:start]...)
	if block != "" {
		out = append(out, strings.Split(strings.TrimRight(block, "\n"), "\n")...)
		if end < len(lines) {
			out = append(out, "")
		}
	}
	out = append(out, lines[end:]...)
	s := strings.TrimRight(strings.Join(out, "\n"), "\n") + "\n"
	return writeFile(file, []byte(s))
}

func writeFile(file string, b []byte) error {
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		return err
	}
	mode := os.FileMode(0o644)
	if fi, err := os.Stat(file); err == nil {
		mode = fi.Mode().Perm()
	}
	tmp := file + ".sonarless-tmp"
	if err := os.WriteFile(tmp, b, mode); err != nil {
		return err
	}
	return os.Rename(tmp, file)
}

// Credentials is a SonarQube URL + token found in an AI client's config.
type Credentials struct {
	Client, File, URL, Token string
}

// FindSonarQubeCredentials scans the JSON-configured clients for MCP entries
// that aren't sonarless-mcp but carry SONARQUBE_URL and SONARQUBE_TOKEN
// (e.g. a docker-run SonarQube MCP pointed at a company server).
func FindSonarQubeCredentials(e Env) []Credentials {
	type src struct {
		label, file, section string
	}
	srcs := []src{
		{"VS Code", filepath.Join(e.appData(), "Code", "User", "mcp.json"), "servers"},
		{"Cursor", filepath.Join(e.Home, ".cursor", "mcp.json"), "mcpServers"},
		{"opencode", filepath.Join(e.Home, ".config", "opencode", "opencode.json"), "mcp"},
	}
	var out []Credentials
	for _, s := range srcs {
		obj, err := readObject(s.file)
		if err != nil {
			continue
		}
		sec, ok := get(obj, s.section)
		if !ok {
			continue
		}
		entries, err := parseObject(sec)
		if err != nil {
			continue
		}
		for _, en := range entries {
			if ours(string(en.Val)) {
				continue
			}
			var v struct {
				Env         map[string]string `json:"env"`
				Environment map[string]string `json:"environment"`
			}
			if json.Unmarshal(en.Val, &v) != nil {
				continue
			}
			env := v.Env
			if env == nil {
				env = v.Environment
			}
			url, tok := env["SONARQUBE_URL"], env["SONARQUBE_TOKEN"]
			if url == "" || tok == "" || strings.Contains(url+tok, "${") {
				continue // missing, or a placeholder resolved by the client at runtime
			}
			out = append(out, Credentials{Client: s.label, File: s.file, URL: url, Token: tok})
		}
	}
	return out
}
