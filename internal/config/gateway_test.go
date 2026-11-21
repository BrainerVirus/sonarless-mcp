package config

import (
	"os"
	"runtime"
	"strings"
	"testing"
)

func TestGatewayTokenIsCreatedOnceAndPrivate(t *testing.T) {
	t.Setenv("SONARLESS_HOME", t.TempDir())
	c, _, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	tok, err := c.GatewayToken()
	if err != nil || len(tok) < 32 {
		t.Fatalf("token %q, %v", tok, err)
	}
	again, _ := c.GatewayToken()
	if again != tok {
		t.Error("token changed between calls: clients configured earlier would be locked out")
	}
	b, _ := os.ReadFile(c.GatewayTokenFile())
	if strings.HasSuffix(string(b), "\n") {
		t.Error("trailing newline: file references would send it as part of the token")
	}
	if fi, _ := os.Stat(c.GatewayTokenFile()); runtime.GOOS != "windows" && fi.Mode().Perm() != 0o600 {
		t.Errorf("token file mode %v, want 0600", fi.Mode().Perm())
	}
	if got := c.GatewayURL(); got != "http://127.0.0.1:9233/sonarless-mcp" {
		t.Errorf("GatewayURL = %s", got)
	}
}
