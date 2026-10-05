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
	MCPImage         = "SONARLESS_MCP_IMAGE"
	MCPToolsets      = "SONARLESS_MCP_TOOLSETS"
	MCPReadOnly      = "SONARLESS_MCP_READ_ONLY"
	IdleTimeoutKey   = "SONARLESS_IDLE_TIMEOUT"
	ProjectKey       = "SONARLESS_PROJECT_KEY"
	ProjectName      = "SONARLESS_PROJECT_NAME"
	Sources          = "SONARLESS_SOURCES"
	Scanner          = "SONARLESS_SCANNER"
	ScannerImage     = "SONARLESS_SCANNER_IMAGE"
	ScanTests        = "SONARLESS_SCAN_TESTS"
	MetricsFile      = "SONARLESS_METRICS_FILE"
	AutoUpdate       = "SONARLESS_AUTO_UPDATE"
)

// Keys is the full list of supported settings.
var Keys = []Key{
	{Name: SonarQubeVersion, Default: "26.6.0.123539-community", Help: "SonarQube image tag (e.g. to match your CI's server)"},
	{Name: ServerPort, Default: "9234", Help: "host port for the SonarQube web UI/API (127.0.0.1)"},
	{Name: AdminUser, Default: "admin", Help: "local admin user"},
	{Name: AdminPass, Default: "admin", Help: "local admin password"},
	{Name: PluginsDirKey, Help: "dir of extra plugin jars (default: <config dir>/plugins)"},
	{Name: MCPPort, Default: "9235", Help: "host port for the shared MCP server (127.0.0.1)"},
	{Name: MCPImage, Default: "sonarsource/sonarqube-mcp:latest", Help: "SonarQube MCP server image"},
	{Name: MCPToolsets, Help: "comma-separated MCP toolsets to enable (server default when empty)"},
	{Name: MCPReadOnly, Default: "false", Help: "disable MCP write tools"},
	{Name: IdleTimeoutKey, Default: "30m", Help: "stop server+MCP after this long unused; 0 disables"},
	{Name: Instance, Default: "sonarless", Help: "prefix for the containers, network and volumes"},
	{Name: AutoUpdate, Default: "true", Help: "check daily for a new sonarless-mcp release and MCP image, installed on next start"},
	{Name: ProjectKey, Project: true, Help: "override the detected project key"},
	{Name: ProjectName, Project: true, Help: "override the detected project name"},
	{Name: Sources, Default: ".", Project: true, Help: "sonar.sources for CLI scans without sonar-project.properties"},
	{Name: Scanner, Project: true, Help: "force scanner: cli, maven, gradle or dotnet (auto when empty)"},
	{Name: ScannerImage, Default: "sonarsource/sonar-scanner-cli:12.1", Project: true, Help: "sonar-scanner CLI image"},
	{Name: ScanTests, Default: "false", Project: true, Help: "run tests during maven/gradle/dotnet scans (needed for coverage)"},
	{Name: MetricsFile, Default: "sonar-metrics.json", Project: true, Help: "file written by `sonarless-mcp results`"},
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
	if d := os.Getenv("SONARLESS_HOME"); d != "" {
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
	for _, n := range []string{ServerPort, MCPPort} {
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
		s := strings.TrimSpace(sc.Text())
		if s == "" || strings.HasPrefix(s, "#") {
			continue
		}
		s = strings.TrimPrefix(s, "export ")
		k, v, ok := strings.Cut(s, "=")
		if !ok {
			return nil, fmt.Errorf("%s:%d: expected KEY=VALUE", path, line)
		}
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		if len(v) >= 2 && (v[0] == '"' && v[len(v)-1] == '"' || v[0] == '\'' && v[len(v)-1] == '\'') {
			v = v[1 : len(v)-1]
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
