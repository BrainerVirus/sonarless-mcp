package mcp

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/BrainerVirus/sonarless-mcp/internal/config"
	"github.com/BrainerVirus/sonarless-mcp/internal/project"
)

const toolsList = `{"tools":[
 {"name":"get_component_measures","inputSchema":{"type":"object","properties":{"projectKey":{"type":"string","description":"The project key"},"branch":{"type":"string"}},"required":["projectKey"]}},
 {"name":"search_sonar_issues_in_projects","inputSchema":{"type":"object","properties":{"projects":{"type":"array","items":{"type":"string"},"description":"Projects"}}}},
 {"name":"show_rule","inputSchema":{"type":"object","properties":{"key":{"type":"string"}},"required":["key"]}}
]}`

func newTestShim(t *testing.T) *Shim {
	t.Helper()
	t.Setenv("SONARLESS_HOME", t.TempDir())
	cfg, _, err := config.Load("")
	if err != nil {
		t.Fatal(err)
	}
	s := &Shim{Cfg: cfg, Project: &project.Project{Key: "acme_web", KeySource: "sonar-project.properties"}}
	s.inject.Store(true)
	s.store("tools/list", json.RawMessage(toolsList))
	return s
}

func args(t *testing.T, params json.RawMessage) map[string]any {
	t.Helper()
	var p struct {
		Arguments map[string]any `json:"arguments"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		t.Fatal(err)
	}
	return p.Arguments
}

func TestInjectProject(t *testing.T) {
	s := newTestShim(t)
	cases := []struct {
		name, in string
		want     map[string]any
	}{
		{"fills missing projectKey", `{"name":"get_component_measures","arguments":{"branch":"main"}}`,
			map[string]any{"projectKey": "acme_web", "branch": "main"}},
		{"fills empty projectKey", `{"name":"get_component_measures","arguments":{"projectKey":""}}`,
			map[string]any{"projectKey": "acme_web"}},
		{"keeps explicit projectKey", `{"name":"get_component_measures","arguments":{"projectKey":"other"}}`,
			map[string]any{"projectKey": "other"}},
		{"fills projects list", `{"name":"search_sonar_issues_in_projects"}`,
			map[string]any{"projects": []any{"acme_web"}}},
		{"keeps explicit projects", `{"name":"search_sonar_issues_in_projects","arguments":{"projects":["x"]}}`,
			map[string]any{"projects": []any{"x"}}},
		{"leaves tools without a project alone", `{"name":"show_rule","arguments":{"key":"go:S100"}}`,
			map[string]any{"key": "go:S100"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := args(t, s.injectProject(json.RawMessage(tc.in))); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}

	s.inject.Store(false) // project unknown to the server: never guess
	in := `{"name":"get_component_measures","arguments":{}}`
	if got := string(s.injectProject(json.RawMessage(in))); got != in {
		t.Errorf("injected without a known project: %s", got)
	}
}

func TestAnnotateTools(t *testing.T) {
	s := newTestShim(t)
	var r struct {
		Tools []struct {
			Name        string
			InputSchema struct {
				Properties map[string]struct{ Description string }
				Required   []string
			}
		}
	}
	if err := json.Unmarshal(s.annotateTools(json.RawMessage(toolsList)), &r); err != nil {
		t.Fatal(err)
	}
	m := r.Tools[0].InputSchema
	if len(m.Required) != 0 {
		t.Errorf("projectKey still required: %v", m.Required)
	}
	if !strings.Contains(m.Properties["projectKey"].Description, `"acme_web"`) {
		t.Errorf("default not documented: %q", m.Properties["projectKey"].Description)
	}
	if got := r.Tools[2].InputSchema.Required; !reflect.DeepEqual(got, []string{"key"}) {
		t.Errorf("unrelated required changed: %v", got)
	}
}

func TestAnnotateInitialize(t *testing.T) {
	s := newTestShim(t)
	var r map[string]any
	_ = json.Unmarshal(s.annotateInitialize(json.RawMessage(`{"protocolVersion":"x","instructions":"Base."}`)), &r)
	if in, _ := r["instructions"].(string); !strings.HasPrefix(in, "Base.") || !strings.Contains(in, "acme_web") {
		t.Errorf("instructions = %q", in)
	}
	if r["protocolVersion"] != "x" {
		t.Error("other fields lost")
	}
}

func TestReadSSE(t *testing.T) {
	body := "event: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":1,\ndata: \"result\":{}}\n\n: comment\ndata: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/progress\"}\n"
	msgs, err := readSSE(strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 2 || string(msgs[0].ID) != "1" || msgs[1].Method != "notifications/progress" {
		t.Errorf("got %+v", msgs)
	}
}

func TestDecodeMessages(t *testing.T) {
	if got := decodeMessages([]byte(`[{"jsonrpc":"2.0","id":1,"result":{}},{"jsonrpc":"2.0","id":2,"result":{}}]`)); len(got) != 2 {
		t.Errorf("batch: %d messages", len(got))
	}
	if got := decodeMessages([]byte("not json")); got != nil {
		t.Errorf("garbage: %v", got)
	}
}

func TestCacheRoundTrip(t *testing.T) {
	s := newTestShim(t)
	s.store("initialize", json.RawMessage(`{"a":1}`))
	if got := string(s.cached("initialize")); got != `{"a":1}` {
		t.Errorf("initialize cache = %s", got)
	}
	if s.cached("tools/list") == nil {
		t.Error("storing initialize dropped tools/list")
	}
	if _, err := os.Stat(filepath.Join(s.Cfg.CacheDir, "mcp-cache.json")); err != nil {
		t.Error(err)
	}
}
