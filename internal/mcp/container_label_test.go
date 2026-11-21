package mcp

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/BrainerVirus/sonarless-mcp/internal/config"
)

// Every MCP container carries its instance, so `stop` finds this
// instance's leftovers by label, never by a shared name prefix.
func TestContainersCarryTheirInstance(t *testing.T) {
	t.Setenv("SONARLESS_HOME", t.TempDir())
	t.Setenv("SONARLESS_INSTANCE", "sltest")
	cfg, _, err := config.Load("")
	if err != nil {
		t.Fatal(err)
	}
	calls := fakeDocker(t, "")
	for _, c := range []*Container{NewLocal(cfg, nil), NewRemote(cfg, nil, "work", "http://sonar.example", 19101)} {
		if err := c.create(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	log, _ := os.ReadFile(calls)
	runs := 0
	for _, l := range strings.Split(string(log), "\n") {
		if strings.HasPrefix(l, "run ") {
			runs++
			if !strings.Contains(l, " --label sonarless-mcp.instance=sltest ") {
				t.Errorf("container created without its instance label: %s", l)
			}
		}
	}
	if runs != 2 {
		t.Errorf("docker run calls: %d\n%s", runs, log)
	}
}
