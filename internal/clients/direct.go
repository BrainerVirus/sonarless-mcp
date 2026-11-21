package clients

import (
	"encoding/json"
	"path/filepath"
	"strings"
)

// Needs says what a client must have to reach the shared gateway directly
// over HTTP (no per-client process) instead of launching the stdio shim.
type Needs int

const (
	// ShimOnly: the client keeps launching `sonarless-mcp mcp`.
	ShimOnly Needs = iota
	// DirectHook: the client runs a command before it connects (Claude
	// Code's headersHelper) that starts the gateway and hands it the token.
	DirectHook
	// DirectAutostart: the gateway must already run (login autostart); the
	// client reads the token from the token file.
	DirectAutostart
	// DirectAutostartEnv: as DirectAutostart, but the client can only read
	// the token from an environment variable (SONARLESS_MCP_TOKEN).
	DirectAutostartEnv
)

// Direct is what a direct client entry points at.
type Direct struct {
	Exe       string // sonarless-mcp binary (Claude's headers helper runs it)
	URL       string // gateway MCP endpoint
	TokenFile string // the gateway token, for clients with file references
	TokenEnv  string // the variable holding the token, for the others
}

// WorkspaceHeader matches mcp.WorkspaceHeader (kept here to avoid the import).
const WorkspaceHeader = "X-Sonarless-Workspace"

// HeadersHelper is the command Claude Code runs before each connection.
func (d Direct) HeadersHelper() string { return shellQuote(d.Exe) + " ensure --headers" }

// ClaudeDirect is Claude Code's user-scope entry. The helper starts the
// gateway and prints the token and workspace headers: a static ${PWD}
// header would carry Claude's inherited PWD, stale when it was started from
// a GUI or with a different working directory.
func ClaudeDirect(d Direct) map[string]any {
	return map[string]any{
		"type":          "http",
		"url":           d.URL,
		"headersHelper": d.HeadersHelper(),
	}
}

// OpencodeDirect reads the token from the file ({file:} takes an absolute
// path; forward slashes work on Windows too). OAuth is on by default for
// remote servers, so it is turned off.
func OpencodeDirect(d Direct, v2 bool) map[string]any {
	v := map[string]any{
		"type":  "remote",
		"url":   d.URL,
		"oauth": false,
		"headers": map[string]string{
			"Authorization": "Bearer {file:" + filepath.ToSlash(d.TokenFile) + "}",
			WorkspaceHeader: "{env:PWD}",
		},
	}
	if !v2 {
		v["enabled"] = true
	}
	return v
}

// VSCodeDirect is the editor-managed (profile mcp.json) HTTP entry; VS Code
// has no file reference or header helper, so the token comes from the
// environment like Cursor's.
func VSCodeDirect(d Direct) map[string]any {
	return map[string]any{
		"type": "http",
		"url":  d.URL,
		"headers": map[string]string{
			"Authorization": "Bearer ${env:" + d.TokenEnv + "}",
			WorkspaceHeader: "${workspaceFolder}",
		},
	}
}

func CursorDirect(d Direct) map[string]any {
	return map[string]any{
		"url": d.URL,
		"headers": map[string]string{
			"Authorization": "Bearer ${env:" + d.TokenEnv + "}",
			WorkspaceHeader: "${workspaceFolder}",
		},
	}
}

func CodexDirect(name string, d Direct) string {
	q := func(s string) string { b, _ := json.Marshal(s); return string(b) } // TOML basic strings share JSON's escaping
	return "[mcp_servers." + name + "]\n" +
		"url = " + q(d.URL) + "\n" +
		"bearer_token_env_var = " + q(d.TokenEnv) + "\n" +
		"env_http_headers = { " + q(WorkspaceHeader) + " = \"PWD\" }\n"
}

func shellQuote(s string) string {
	if !strings.ContainsAny(s, " '\"$`\\") {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// SetupOptions drive Setup.
type SetupOptions struct {
	Exe        string // sonarless-mcp binary for shim entries
	Direct     Direct
	EnvToken   bool              // Direct.TokenEnv holds the gateway token in setup's environment
	Stdio      bool              // register the stdio shim everywhere (no gateway)
	GatewayEnv map[string]string // passed to the autostarted gateway (e.g. SONARLESS_HOME)
	LogFile    string            // the gateway's log (macOS LaunchAgent)
}

// Result is what Setup did for one client.
type Result struct {
	Client   Client
	Name     string // registered server name
	Direct   bool   // HTTP entry for the gateway (else the stdio shim)
	Why      string // why a client that can go direct kept the shim
	NeedsEnv bool   // it kept the shim only because the token variable is missing
	Err      error
}

// Setup registers each client: directly with the gateway where what the
// client needs is in place (installing the login autostart when a chosen
// client relies on it), the stdio shim otherwise. autostart is the file
// installed ("" if none was needed or possible).
func Setup(e Env, chosen []Client, o SetupOptions) (results []Result, autostart string) {
	needAuto := false
	for _, c := range chosen {
		if !o.Stdio && c.RegisterDirect != nil && (c.Direct == DirectAutostart || c.Direct == DirectAutostartEnv && o.EnvToken) {
			needAuto = true
		}
	}
	autoWhy := ""
	if needAuto {
		var err error
		switch {
		case !AutostartSupported(e):
			autoWhy = "no login autostart for the gateway on " + e.GOOS
		default:
			if autostart, err = InstallAutostart(e, o.Exe, o.LogFile, o.GatewayEnv); err != nil {
				autoWhy = "gateway autostart failed: " + err.Error()
			}
		}
	}
	for _, c := range chosen {
		r := Result{Client: c}
		switch {
		case o.Stdio || c.RegisterDirect == nil:
		case c.Direct == DirectHook:
			r.Direct = true
		case c.Direct == DirectAutostartEnv && !o.EnvToken:
			r.Why = o.Direct.TokenEnv + " is not set to the gateway token in this shell"
			r.NeedsEnv = true
		case autostart == "":
			r.Why = autoWhy
		default:
			r.Direct = true
		}
		if r.Direct {
			r.Name, r.Err = c.RegisterDirect(e, o.Direct)
		} else {
			r.Name, r.Err = c.Register(e, o.Exe)
		}
		results = append(results, r)
	}
	return results, autostart
}
