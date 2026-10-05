package remote

import (
	"os"
	"runtime"
	"strings"
	"testing"

	"github.com/BrainerVirus/sonarless-mcp/internal/config"
)

func testCfg(t *testing.T) *config.Config {
	t.Helper()
	t.Setenv("SONARLESS_HOME", t.TempDir())
	t.Setenv(config.MCPPort, "")
	cfg, _, err := config.Load("")
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestAddLoadRemove(t *testing.T) {
	cfg := testCfg(t)
	w, err := Add(cfg, Remote{Name: "work", URL: "https://sonar.example.com/", Branch: "develop"}, "tok1\n")
	if err != nil {
		t.Fatal(err)
	}
	if w.Port != 9236 || w.URL != "https://sonar.example.com" {
		t.Errorf("got %+v", w)
	}
	o, _ := Add(cfg, Remote{Name: "oss", URL: "https://sonarcloud.io"}, "tok2")
	if o.Port != 9237 {
		t.Errorf("second port = %d", o.Port)
	}
	w2, _ := Add(cfg, Remote{Name: "work", URL: "https://sonar.example.com", Branch: "main"}, "tok3")
	if w2.Port != 9236 {
		t.Errorf("update moved port to %d", w2.Port)
	}
	rs, _ := Load(cfg)
	if len(rs) != 2 || rs[0].Name != "oss" || rs[1].Branch != "main" {
		t.Errorf("load = %+v", rs)
	}
	if tok, _ := Token(cfg, "work"); tok != "tok3" {
		t.Errorf("token = %q", tok)
	}
	b, _ := os.ReadFile(file(cfg))
	if strings.Contains(string(b), "tok") {
		t.Error("token leaked into remotes.json")
	}
	if fi, _ := os.Stat(TokenFile(cfg, "work")); runtime.GOOS != "windows" && fi.Mode().Perm() != 0o600 {
		t.Errorf("token mode %v", fi.Mode().Perm())
	}
	if err := Remove(cfg, "work"); err != nil {
		t.Fatal(err)
	}
	if _, err := Token(cfg, "work"); err == nil {
		t.Error("token kept after remove")
	}
	if err := Remove(cfg, "work"); err == nil {
		t.Error("removing twice succeeded")
	}
}

func TestValidation(t *testing.T) {
	cfg := testCfg(t)
	for _, r := range []Remote{{Name: "local", URL: "https://x"}, {Name: "Bad Name", URL: "https://x"}, {Name: "ok", URL: "ftp://x"}} {
		if _, err := Add(cfg, r, "t"); err == nil {
			t.Errorf("accepted %+v", r)
		}
	}
	if _, err := Add(cfg, Remote{Name: "ok", URL: "https://x"}, "  "); err == nil {
		t.Error("accepted empty token")
	}
}
