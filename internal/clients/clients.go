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
	"strconv"
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
	// Direct says what the client needs to reach the gateway over HTTP;
	// RegisterDirect (nil for ShimOnly) writes that entry instead of the shim.
	Direct         Needs
	RegisterDirect func(env Env, d Direct) (name string, err error)
	Remove         func(env Env) error
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
		Direct: DirectHook,
		RegisterDirect: func(e Env, d Direct) (string, error) {
			name := pickName(entry(e))
			j, _ := json.Marshal(ClaudeDirect(d))
			_, _ = e.Run("claude", "mcp", "remove", "-s", "user", name)
			_, err := e.Run("claude", "mcp", "add-json", "-s", "user", name, string(j))
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
// sections(e)[0] is where entries are written; every section is checked
// for (and cleaned of) entries an older layout left behind.
func jsonClient(id, label string, file func(Env) string, sections func(Env) []string, detected func(Env) bool,
	value func(e Env, exe string) map[string]any, needs Needs, direct func(e Env, d Direct) map[string]any) Client {
	entry := func(e Env) func(string) (string, bool) {
		return func(name string) (string, bool) { return jsonEntry(file(e), sections(e)[0], name) }
	}
	c := Client{
		ID: id, Label: label, Detected: detected, Where: file, Direct: needs,
		Configured: func(e Env) bool {
			for _, s := range sections(e) {
				for _, n := range names {
					if v, ok := jsonEntry(file(e), s, n); ok && ours(v) {
						return true
					}
				}
			}
			return false
		},
		Register: func(e Env, exe string) (string, error) {
			name := pickName(entry(e))
			return name, jsonSet(file(e), sections(e)[0], name, value(e, exe))
		},
		Remove: func(e Env) error {
			for _, s := range sections(e) {
				for _, n := range names {
					if v, ok := jsonEntry(file(e), s, n); ok && ours(v) {
						if err := jsonDelete(file(e), s, n); err != nil {
							return err
						}
					}
				}
			}
			return nil
		},
	}
	if direct != nil {
		c.RegisterDirect = func(e Env, d Direct) (string, error) {
			name := pickName(entry(e))
			return name, jsonSet(file(e), sections(e)[0], name, direct(e, d))
		}
	}
	return c
}

func static(section string) func(Env) []string {
	return func(Env) []string { return []string{section} }
}

// --- Cursor: ~/.cursor/mcp.json ---

func cursor() Client {
	return jsonClient("cursor", "Cursor",
		func(e Env) string { return filepath.Join(e.Home, ".cursor", "mcp.json") }, static("mcpServers"),
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
		func(_ Env, exe string) map[string]any {
			return map[string]any{"command": exe, "args": []string{"mcp", "${workspaceFolder}"}}
		}, DirectAutostartEnv, func(_ Env, d Direct) map[string]any { return CursorDirect(d) })
}

// --- opencode: ~/.config/opencode/opencode.json ---

func opencode() Client {
	return jsonClient("opencode", "opencode",
		opencodeFile,
		func(e Env) []string {
			if opencodeV2(e) {
				return []string{"mcp.servers", "mcp"}
			}
			return []string{"mcp"}
		},
		func(e Env) bool { return e.has("opencode") },
		func(e Env, exe string) map[string]any {
			v := map[string]any{"type": "local", "command": []string{exe, "mcp"}}
			if !opencodeV2(e) {
				v["enabled"] = true
			}
			return v
		}, DirectAutostart, func(e Env, d Direct) map[string]any { return OpencodeDirect(d, opencodeV2(e)) })
}

func opencodeFile(e Env) string {
	if x := os.Getenv("XDG_CONFIG_HOME"); x != "" && e.GOOS != "windows" {
		return filepath.Join(x, "opencode", "opencode.json")
	}
	return filepath.Join(e.Home, ".config", "opencode", "opencode.json")
}

// opencodeV2 reports whether opencode uses its v2 layout: servers under
// `mcp.servers` (not directly under `mcp`) and `disabled` (no `enabled`).
// The config file decides when it says; otherwise the installed version.
func opencodeV2(e Env) bool {
	if obj, err := readObject(opencodeFile(e)); err == nil {
		if mcp, ok := getPath(obj, []string{"mcp"}); ok {
			if _, ok := get(mcp, "servers"); ok {
				return true
			}
			if len(mcp) > 0 {
				return false // v1 entries straight under mcp
			}
		}
	}
	out, err := e.Run("opencode", "--version")
	if err != nil {
		return false
	}
	v := strings.TrimPrefix(strings.TrimSpace(out), "opencode ")
	v = strings.TrimPrefix(v, "v")
	major, _, _ := strings.Cut(v, ".")
	n, err := strconv.Atoi(major)
	return err == nil && n >= 2
}

// --- VS Code: user mcp.json ---

func vscode() Client {
	return jsonClient("vscode", "VS Code",
		func(e Env) string { return filepath.Join(e.appData(), "Code", "User", "mcp.json") }, static("servers"),
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
		func(_ Env, exe string) map[string]any {
			return map[string]any{"type": "stdio", "command": exe, "args": []string{"mcp", "${workspaceFolder}"}}
		}, DirectAutostartEnv, func(_ Env, d Direct) map[string]any { return VSCodeDirect(d) })
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
			if body, ok := tomlBody(string(b), header(name)); ok {
				return body, true
			}
			// `[mcp_servers]` + `name = { ... }` is the same entry, inline.
			return tomlInline(string(b), "[mcp_servers]", name)
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
		Direct: DirectAutostartEnv,
		RegisterDirect: func(e Env, d Direct) (string, error) {
			name := pickName(entry(e))
			return name, tomlReplace(file(e), header(name), CodexDirect(name, d))
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

// A section is a key, or a dotted path to a nested object ("mcp.servers").

// getPath returns the object at path, if every level is an object.
func getPath(obj []kv, path []string) ([]kv, bool) {
	for _, k := range path {
		v, ok := get(obj, k)
		if !ok {
			return nil, false
		}
		var err error
		if obj, err = parseObject(v); err != nil {
			return nil, false
		}
	}
	return obj, true
}

// editPath replaces the object at path (created when missing) with f's result.
func editPath(obj []kv, path []string, f func([]kv) []kv) ([]kv, error) {
	if len(path) == 0 {
		return f(obj), nil
	}
	var inner []kv
	if v, ok := get(obj, path[0]); ok {
		var err error
		if inner, err = parseObject(v); err != nil {
			return nil, fmt.Errorf("%q is not an object", path[0])
		}
	}
	inner, err := editPath(inner, path[1:], f)
	if err != nil {
		return nil, err
	}
	return put(obj, path[0], encodeCompact(inner)), nil
}

// jsonEntry returns the raw JSON of section.name, if present.
func jsonEntry(file, section, name string) (string, bool) {
	obj, err := readObject(file)
	if err != nil {
		return "", false
	}
	inner, ok := getPath(obj, strings.Split(section, "."))
	if !ok {
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
	v, _ := json.Marshal(value)
	obj, err = editPath(obj, strings.Split(section, "."), func(inner []kv) []kv { return put(inner, name, v) })
	if err != nil {
		return fmt.Errorf("%s: %v", file, err)
	}
	return writeFile(file, encodeObject(obj))
}

func jsonDelete(file, section, name string) error {
	obj, err := readObject(file)
	if err != nil || obj == nil {
		return err
	}
	if inner, ok := getPath(obj, strings.Split(section, ".")); !ok {
		return nil
	} else if _, ok := get(inner, name); !ok {
		return nil
	}
	obj, err = editPath(obj, strings.Split(section, "."), func(inner []kv) []kv {
		kept := inner[:0]
		for _, e := range inner {
			if e.Key != name {
				kept = append(kept, e)
			}
		}
		return kept
	})
	if err != nil {
		return nil
	}
	return writeFile(file, encodeObject(obj))
}

func encodeCompact(obj []kv) json.RawMessage {
	var b bytes.Buffer
	_ = json.Compact(&b, encodeObject(obj))
	return b.Bytes()
}

// --- TOML block editing ---

// tomlBlock returns the line index of header in s, or -1. A trailing
// comment after the header is allowed.
func tomlBlock(s, header string) int {
	for i, l := range strings.Split(s, "\n") {
		if tomlHeader(l) == header {
			return i
		}
	}
	return -1
}

// tomlHeader returns a line's table header without spaces/comment, or "".
func tomlHeader(line string) string {
	t := strings.TrimSpace(line)
	if !strings.HasPrefix(t, "[") {
		return ""
	}
	if i := strings.Index(t, "#"); i >= 0 {
		t = strings.TrimSpace(t[:i])
	}
	return t
}

// tomlInline returns `name = { ... }` defined inside [parent], if any.
func tomlInline(s, parent, name string) (string, bool) {
	in := false
	for _, l := range strings.Split(s, "\n") {
		if h := tomlHeader(l); h != "" {
			in = h == parent
			continue
		}
		if !in {
			continue
		}
		if k, _, ok := strings.Cut(strings.TrimSpace(l), "="); ok && strings.Trim(strings.TrimSpace(k), `"'`) == name {
			return l, true
		}
	}
	return "", false
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
		if tomlHeader(lines[i]) != "" {
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
		if tomlHeader(lines[i]) != "" {
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

func writeFile(file string, b []byte) error { return writeFileMode(file, b, 0) }

// writePrivateFile writes a file only its owner can read (0600), whatever
// mode an existing copy had: autostart files can carry settings such as
// SONARLESS_ADMIN_PASS. On Windows the mode is not enforced; the file lives
// under %AppData%, which only its user can read.
func writePrivateFile(file string, b []byte) error { return writeFileMode(file, b, 0o600) }

// writeFileMode writes b to file through a temporary file. A zero force
// keeps an existing file's mode (0644 for a new one); otherwise the file
// gets force.
func writeFileMode(file string, b []byte, force os.FileMode) error {
	// Write through symlinks (dotfiles managers link configs into place):
	// replacing the link with a file would detach it from the managed copy.
	if real, err := filepath.EvalSymlinks(file); err == nil {
		file = real
	}
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		return err
	}
	mode := os.FileMode(0o644)
	if fi, err := os.Stat(file); err == nil {
		mode = fi.Mode().Perm()
	}
	if force != 0 {
		mode = force
	}
	tmp := file + ".sonarless-tmp"
	if err := os.WriteFile(tmp, b, mode); err != nil {
		return err
	}
	if force != 0 { // WriteFile's mode passes through the umask; a forced mode must not
		if err := os.Chmod(tmp, mode); err != nil {
			_ = os.Remove(tmp)
			return err
		}
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
