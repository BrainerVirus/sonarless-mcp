package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func setup(t *testing.T, user, project string) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("SONARLESS_HOME", home)
	for _, k := range Keys { // isolate from the developer's environment
		t.Setenv(k.Name, "")
	}
	if user != "" {
		if err := os.WriteFile(filepath.Join(home, "config.env"), []byte(user), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	proj := t.TempDir()
	if project != "" {
		if err := os.WriteFile(filepath.Join(proj, ".sonarless.env"), []byte(project), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return proj
}

func TestLayering(t *testing.T) {
	proj := setup(t,
		"# pinned to CI\nexport SONARLESS_SONARQUBE_VERSION=\"26.4.0.121862-community\"\nSONARLESS_SERVER_PORT=9300 # comment\nSONARLESS_INSTANCE=work\nDOCKER_SONAR_SERVER=old\n",
		"SONARLESS_PROJECT_KEY=from_project\nSONARLESS_SERVER_PORT=1111\n")
	t.Setenv(ScanTests, "true")

	c, warnings, err := Load(proj)
	if err != nil {
		t.Fatal(err)
	}
	checks := map[string]string{
		ServerPort: "9300", // project file may not move the shared server
		ProjectKey: "from_project",
		ScanTests:  "true",
		AdminUser:  "admin",
	}
	for k, want := range checks {
		if got := c.Get(k); got != want {
			t.Errorf("%s = %q (from %s), want %q", k, got, c.Source(k), want)
		}
	}
	if got := c.ServerImage(); got != "sonarqube:26.4.0.121862-community" {
		t.Errorf("ServerImage = %q", got)
	}
	if c.ServerContainer() != "work-server" || c.MCPContainer() != "work-mcp" || c.Network() != "work" {
		t.Errorf("names: %s %s %s", c.ServerContainer(), c.MCPContainer(), c.Network())
	}
	joined := strings.Join(warnings, "\n")
	if !strings.Contains(joined, "unknown key DOCKER_SONAR_SERVER") || !strings.Contains(joined, "SONARLESS_SERVER_PORT is machine-wide") {
		t.Errorf("warnings = %v", warnings)
	}
}

func TestIdleTimeout(t *testing.T) {
	for in, want := range map[string]time.Duration{"0": 0, "45": 45 * time.Minute, "1h30m": 90 * time.Minute} {
		setup(t, IdleTimeoutKey+"="+in+"\n", "")
		c, _, err := Load("")
		if err != nil {
			t.Fatal(err)
		}
		if got, _ := c.IdleTimeout(); got != want {
			t.Errorf("%q -> %s, want %s", in, got, want)
		}
	}
	setup(t, IdleTimeoutKey+"=soon\n", "")
	if _, _, err := Load(""); err == nil {
		t.Error("bad duration accepted")
	}
}

func TestBadPort(t *testing.T) {
	setup(t, MCPPort+"=abc\n", "")
	if _, _, err := Load(""); err == nil {
		t.Error("bad port accepted")
	}
}
