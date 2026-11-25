// Package config loads sonarless-mcp settings from layered sources:
// built-in defaults < user config.env < project .sonarless.env < environment.
//
// Server and MCP settings are shared by every project (one SonarQube for the
// whole machine), so a project file may only set scan-related keys.
package config

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Key describes one setting: its env name, default and whether a project
// file may override it.
type Key struct {
	Name    string
	Default string
	Project bool // settable from a project's .sonarless.env
	Help    string
	// HelpDefault, when set, stands for Default in --help (a long value).
	HelpDefault string
}

// Setting names.
const (
	Instance         = "SONARLESS_INSTANCE"
	SonarQubeVersion = "SONARLESS_SONARQUBE_VERSION"
	ServerPort       = "SONARLESS_SERVER_PORT"
	AdminUser        = "SONARLESS_ADMIN_USER"
	AdminPass        = "SONARLESS_ADMIN_PASS"
	PluginsDirKey    = "SONARLESS_PLUGINS_DIR"
	MCPPort          = "SONARLESS_MCP_PORT"
	GatewayPort      = "SONARLESS_GATEWAY_PORT"
	MCPImage         = "SONARLESS_MCP_IMAGE"
	MCPToolsets      = "SONARLESS_MCP_TOOLSETS"
	MCPReadOnly      = "SONARLESS_MCP_READ_ONLY"
	MCPJavaOpts      = "SONARLESS_MCP_JAVA_OPTS"
	IdleTimeoutKey   = "SONARLESS_IDLE_TIMEOUT"
	ProjectKey       = "SONARLESS_PROJECT_KEY"
	ProjectName      = "SONARLESS_PROJECT_NAME"
	Sources          = "SONARLESS_SOURCES"
	Scanner          = "SONARLESS_SCANNER"
	ScannerImage     = "SONARLESS_SCANNER_IMAGE"
	ScanTests        = "SONARLESS_SCAN_TESTS"
	FailOnGate       = "SONARLESS_FAIL_ON_GATE"
	MetricsFile      = "SONARLESS_METRICS_FILE"
	AutoUpdate       = "SONARLESS_AUTO_UPDATE"
	UpdateNotice     = "SONARLESS_UPDATE_NOTICE"
	AgentTools       = "SONARLESS_MCP_AGENT_TOOLS"
	WebHeap          = "SONARLESS_WEB_HEAP"
	CEHeap           = "SONARLESS_CE_HEAP"
	SearchHeap       = "SONARLESS_SEARCH_HEAP"
)

// DefaultMCPToolsets is the MCP server's own HTTP default minus
// dependency-risks: its one tool needs SonarQube's SCA (Enterprise), so on the
// local Community server it can only fail, and it is ~7% of tools/list. The
// categories with no HTTP tools (analysis, ide, cag) are left out.
const DefaultMCPToolsets = "coverage,duplications,issues,measures,projects,quality-gates,rules,security-hotspots"

// Keys is the full list of supported settings.
var Keys = []Key{
	{Name: SonarQubeVersion, Default: "26.6.0.123539-community", Help: "SonarQube image tag (e.g. to match your CI's server)"},
	{Name: ServerPort, Default: "9234", Help: "host port for the SonarQube web UI/API (127.0.0.1)"},
	{Name: AdminUser, Default: "admin", Help: "local admin user"},
	{Name: AdminPass, Default: "admin", Help: "local admin password"},
	{Name: PluginsDirKey, Help: "dir of extra plugin jars (default: <config dir>/plugins)"},
	{Name: MCPPort, Default: "9235", Help: "host port for the shared MCP server (127.0.0.1)"},
	{Name: GatewayPort, Default: "9233", Help: "host port of the gateway AI clients connect to directly over HTTP (127.0.0.1)"},
	{Name: MCPImage, Default: "sonarsource/sonarqube-mcp:latest", Help: "SonarQube MCP server image"},
	{Name: MCPToolsets, Default: DefaultMCPToolsets, HelpDefault: "the MCP server's own minus dependency-risks; see docs/configuration.md",
		Help: "comma-separated toolset names the MCP server enables; default hands the choice to the MCP server"},
	{Name: MCPReadOnly, Default: "false", Help: "disable MCP write tools"},
	{Name: MCPJavaOpts, Default: "-Xmx64m -XX:+UseSerialGC -XX:TieredStopAtLevel=1 -XX:ReservedCodeCacheSize=32m -XX:MaxMetaspaceSize=128m -Xss512k", Help: "JVM options of the shared MCP server (JAVA_TOOL_OPTIONS); a small heap and C1-only JIT keep it near 200 MiB; none leaves the JVM defaults"},
	{Name: IdleTimeoutKey, Default: "30m", Help: "stop server+MCP after this long unused; 0 disables"},
	{Name: WebHeap, Default: "256m", Help: "max Java heap of SonarQube's web server (SonarQube's own default is 512m)"},
	{Name: CEHeap, Default: "384m", Help: "max Java heap of SonarQube's compute engine, which processes scan reports; raise it if big projects fail with OutOfMemoryError (SonarQube's own: 512m)"},
	{Name: SearchHeap, Default: "256m", Help: "Java heap of SonarQube's Elasticsearch; it also maps index files outside the heap (SonarQube's own: 512m)"},
	{Name: Instance, Default: "sonarless", Help: "prefix for the containers, network and volumes"},
	{Name: AgentTools, Default: "true", Help: "expose sonarless_* tools (scan, status, project, remote sync) to agents next to the SonarQube tools"},
	{Name: AutoUpdate, Default: "true", Help: "upgrade daily in the background to a signed sonarless-mcp release once it is a day old, and pull a newer MCP image"},
	{Name: UpdateNotice, Default: "true", Help: "in a terminal, say on stderr (at most once a day) when a newer sonarless-mcp release exists"},
	{Name: ProjectKey, Project: true, Help: "override the detected project key"},
	{Name: ProjectName, Project: true, Help: "override the detected project name"},
	{Name: Sources, Default: ".", Project: true, Help: "sonar.sources for CLI scans without sonar-project.properties"},
	{Name: Scanner, Project: true, Help: "force scanner: cli, maven, gradle or dotnet (auto when empty)"},
	{Name: ScannerImage, Default: "sonarsource/sonar-scanner-cli:12.4", Project: true, Help: "sonar-scanner CLI image"},
	{Name: ScanTests, Default: "false", Project: true, Help: "run tests during maven/gradle/dotnet scans (needed for coverage)"},
	{Name: FailOnGate, Default: "false", Project: true, Help: "`sonarless-mcp scan` exits 2 when the quality gate fails, for CI and scripts (like scan --fail-on-gate)"},
	{Name: MetricsFile, Default: "sonar-metrics.json", Project: true, Help: "file written by `sonarless-mcp results`"},
}

// HomeEnv moves the config and cache dirs under one directory.
const HomeEnv = "SONARLESS_HOME"

// OtherEnv lists the SONARLESS_* variables sonarless-mcp reads that are not
// settings: they come from the environment only, never from a config file.
var OtherEnv = []Key{
	{Name: HomeEnv, Help: "keep the config in this dir and the cache in its cache/ subdir instead of the OS dirs (tests, a second install)"},
	{Name: GatewayTokenEnv, Help: "the gateway token, for clients that read it from the environment (Cursor, VS Code, Codex); setup connects them directly once it is set"},
	{Name: "SONARLESS_REMOTE_TOKEN", Help: "token for `remote add` / `remote update`, instead of the hidden prompt"},
	{Name: "SONARLESS_GATEWAY_SUPERVISED", Help: "set by the login service: on a self-update the gateway exits and the service restarts it (internal)"},
}

// EnvHelp describes every SONARLESS_* variable sonarless-mcp reads, for the
// root command's help.
func EnvHelp() string {
	var b strings.Builder
	b.WriteString("Settings: environment, user config.env, or (marked [project]) a project's\n" +
		".sonarless.env; `sonarless-mcp config` shows the effective values.\n")
	for _, k := range Keys {
		line := "  " + k.Name + "\n      " + k.Help
		switch {
		case k.HelpDefault != "":
			line += " (default: " + k.HelpDefault + ")"
		case k.Default != "":
			line += " (default: " + k.Default + ")"
		}
		if k.Project {
			line += " [project]"
		}
		b.WriteString(line + "\n")
	}
	b.WriteString("\nOther environment variables:\n")
	for _, k := range OtherEnv {
		b.WriteString("  " + k.Name + "\n      " + k.Help + "\n")
	}
	return b.String()
}

// Config is the resolved configuration.
type Config struct {
	values  map[string]string
	sources map[string]string // key -> where the value came from

	ConfigDir string // user config dir (config.env, token)
	CacheDir  string // activity stamp, locks, tool cache, logs
}

// UserDirs returns the per-OS config and cache dirs.
func UserDirs() (configDir, cacheDir string, err error) {
	if d := os.Getenv(HomeEnv); d != "" {
		return d, filepath.Join(d, "cache"), nil
	}
	c, err := os.UserConfigDir()
	if err != nil {
		return "", "", err
	}
	k, err := os.UserCacheDir()
	if err != nil {
		return "", "", err
	}
	return filepath.Join(c, "sonarless-mcp"), filepath.Join(k, "sonarless-mcp"), nil
}

// UserConfigFile is the path of the user-level config.env.
func (c *Config) UserConfigFile() string { return filepath.Join(c.ConfigDir, "config.env") }

// Load resolves the configuration. projectRoot may be empty; when set, its
// .sonarless.env is applied (scan keys only).
func Load(projectRoot string) (*Config, []string, error) {
	cfgDir, cacheDir, err := UserDirs()
	if err != nil {
		return nil, nil, err
	}
	c := &Config{values: map[string]string{}, sources: map[string]string{}, ConfigDir: cfgDir, CacheDir: cacheDir}
	var warnings []string

	for _, k := range Keys {
		if k.Default != "" {
			c.set(k.Name, k.Default, "default")
		}
	}

	userFile := c.UserConfigFile()
	if vals, err := ReadEnvFile(userFile); err == nil {
		for _, name := range SortedKeys(vals) {
			if _, ok := lookup(name); !ok {
				warnings = append(warnings, fmt.Sprintf("%s: unknown key %s ignored", userFile, name))
				continue
			}
			c.set(name, vals[name], userFile)
		}
	} else if !os.IsNotExist(err) {
		return nil, nil, err
	}

	if projectRoot != "" {
		pf := filepath.Join(projectRoot, ".sonarless.env")
		if vals, err := ReadEnvFile(pf); err == nil {
			for _, name := range SortedKeys(vals) {
				key, ok := lookup(name)
				switch {
				case !ok:
					warnings = append(warnings, fmt.Sprintf("%s: unknown key %s ignored", pf, name))
				case !key.Project:
					warnings = append(warnings, fmt.Sprintf("%s: %s is machine-wide (shared server); set it in %s instead", pf, name, userFile))
				default:
					c.set(name, vals[name], pf)
				}
			}
		} else if !os.IsNotExist(err) {
			return nil, nil, err
		}
	}

	for _, k := range Keys {
		if v, ok := os.LookupEnv(k.Name); ok && v != "" {
			c.set(k.Name, v, "env")
		}
	}

	if _, err := c.IdleTimeout(); err != nil {
		return nil, nil, err
	}
	for _, n := range []string{WebHeap, CEHeap, SearchHeap} {
		if v := c.values[n]; v != "" && !heapSize.MatchString(v) {
			return nil, nil, fmt.Errorf("%s=%q (from %s) is not a heap size such as 256m or 1g", n, v, c.sources[n])
		}
	}
	for _, n := range []string{ServerPort, MCPPort, GatewayPort} {
		if _, err := strconv.Atoi(c.values[n]); err != nil {
			return nil, nil, fmt.Errorf("%s=%q (from %s) is not a port number", n, c.values[n], c.sources[n])
		}
	}
	return c, warnings, nil
}

func lookup(name string) (Key, bool) {
	for _, k := range Keys {
		if k.Name == name {
			return k, true
		}
	}
	return Key{}, false
}

func (c *Config) set(name, v, src string) {
	c.values[name] = v
	c.sources[name] = src
}

// Get returns a setting's value ("" if unset).
func (c *Config) Get(name string) string { return c.values[name] }

// Source returns where a setting's value came from.
func (c *Config) Source(name string) string { return c.sources[name] }

// Int returns a numeric setting.
func (c *Config) Int(name string) int {
	n, _ := strconv.Atoi(c.values[name])
	return n
}

// Bool returns a boolean setting.
func (c *Config) Bool(name string) bool {
	b, _ := strconv.ParseBool(c.values[name])
	return b
}

// IdleTimeout parses SONARLESS_IDLE_TIMEOUT; 0 means never stop.
func (c *Config) IdleTimeout() (time.Duration, error) {
	v := strings.TrimSpace(c.values[IdleTimeoutKey])
	if v == "" || v == "0" {
		return 0, nil
	}
	if n, err := strconv.Atoi(v); err == nil { // bare number = minutes
		return time.Duration(n) * time.Minute, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("%s=%q (from %s): %v", IdleTimeoutKey, v, c.sources[IdleTimeoutKey], err)
	}
	return d, nil
}

// Docker resource names, all derived from SONARLESS_INSTANCE.

// ServerContainer is the SonarQube server container name.
func (c *Config) ServerContainer() string { return c.values[Instance] + "-server" }

// MCPContainer is the shared MCP container name.
func (c *Config) MCPContainer() string { return c.values[Instance] + "-mcp" }

// Network is the docker network shared by server, scanners and MCP.
func (c *Config) Network() string { return c.values[Instance] }

// ServerImage is the SonarQube image for the configured version.
func (c *Config) ServerImage() string { return "sonarqube:" + c.values[SonarQubeVersion] }

// PluginsDir is the host dir of extra plugin jars, or "" if none exists.
func (c *Config) PluginsDir() string {
	d := c.values[PluginsDirKey]
	if d == "" {
		d = filepath.Join(c.ConfigDir, "plugins")
	}
	if fi, err := os.Stat(d); err == nil && fi.IsDir() {
		return d
	}
	return ""
}

// ServerURL is the SonarQube URL as seen from the host.
func (c *Config) ServerURL() string {
	return fmt.Sprintf("http://localhost:%d", c.Int(ServerPort))
}

// ServerURLInNetwork is the SonarQube URL as seen from containers on the network.
func (c *Config) ServerURLInNetwork() string {
	return fmt.Sprintf("http://%s:9000", c.ServerContainer())
}

// MCPURL is the shared MCP endpoint on the host.
func (c *Config) MCPURL() string {
	return fmt.Sprintf("http://127.0.0.1:%d/mcp", c.Int(MCPPort))
}

// Describe lists every key with its value and source, for `sonarless-mcp config`.
func (c *Config) Describe() []string {
	var out []string
	for _, k := range Keys {
		v, src := c.values[k.Name], c.sources[k.Name]
		if src == "" {
			src = "unset"
		}
		out = append(out, fmt.Sprintf("%-28s %-36s # %s", k.Name, v, src))
	}
	return out
}

// ReadEnvFile parses KEY=VALUE lines (optional `export `, quotes, # comments).
func ReadEnvFile(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	vals := map[string]string{}
	sc := bufio.NewScanner(f)
	for line := 1; sc.Scan(); line++ {
		s := strings.TrimSpace(strings.TrimPrefix(sc.Text(), "\ufeff")) // BOM from Windows editors
		if s == "" || strings.HasPrefix(s, "#") {
			continue
		}
		s = strings.TrimPrefix(s, "export ")
		k, v, ok := strings.Cut(s, "=")
		if !ok {
			return nil, fmt.Errorf("%s:%d: expected KEY=VALUE", path, line)
		}
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		if len(v) > 0 && (v[0] == '"' || v[0] == '\'') {
			// Quoted: the value ends at the matching quote; anything after
			// (e.g. "# comment") is ignored, and # inside quotes is kept.
			if end := strings.IndexByte(v[1:], v[0]); end >= 0 {
				v = v[1 : end+1]
			}
		} else if i := strings.Index(v, " #"); i >= 0 {
			v = strings.TrimSpace(v[:i])
		}
		vals[k] = v
	}
	return vals, sc.Err()
}

// SortedKeys returns key names sorted, for stable output.
func SortedKeys(m map[string]string) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

// heapSize is a Java -Xmx value: a positive number of megabytes or gigabytes.
var heapSize = regexp.MustCompile(`^[1-9][0-9]*[mMgG]$`)

// ServerJavaOpts are the SONAR_*_JAVAOPTS environment entries for the heaps
// set in the config, each replacing SonarQube's own options for that process
// (its defaults: web and compute engine -Xmx512m -Xms128m, Elasticsearch
// -Xmx512m -Xms512m -XX:MaxDirectMemorySize=256m). sonarless sets smaller
// defaults, measured on one scan (#2); each key raises its process again.
func (c *Config) ServerJavaOpts() []string {
	var out []string
	if v := c.values[WebHeap]; v != "" {
		out = append(out, "SONAR_WEB_JAVAOPTS=-Xmx"+v+" -Xms128m -XX:+HeapDumpOnOutOfMemoryError")
	}
	if v := c.values[CEHeap]; v != "" {
		out = append(out, "SONAR_CE_JAVAOPTS=-Xmx"+v+" -Xms128m -XX:+HeapDumpOnOutOfMemoryError")
	}
	if v := c.values[SearchHeap]; v != "" {
		// Elasticsearch wants its minimum and maximum heap equal.
		out = append(out, "SONAR_SEARCH_JAVAOPTS=-Xmx"+v+" -Xms"+v+" -XX:MaxDirectMemorySize=256m -XX:+HeapDumpOnOutOfMemoryError")
	}
	return out
}
