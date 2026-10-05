package sonar

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// fakeSonar is a minimal in-memory SonarQube for the sync endpoints.
type fakeSonar struct {
	mu       sync.Mutex
	version  string
	gates    map[string][]condition
	selected map[string]string // project -> gate
	profiles []profile
	assigned map[string]string // lang -> profile name
	restored []string          // restored backups
	newCode  map[string]string
	nextID   int
	metrics  map[string]bool // metrics the server knows; nil = all
}

func newFake(version string) *fakeSonar {
	return &fakeSonar{version: version, gates: map[string][]condition{}, selected: map[string]string{},
		assigned: map[string]string{}, newCode: map[string]string{}}
}

func (f *fakeSonar) handler(t *testing.T) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		_ = r.ParseForm()
		q := r.Form
		js := func(v any) { _ = json.NewEncoder(w).Encode(v) }
		switch r.URL.Path {
		case "/api/system/status":
			js(map[string]string{"status": "UP", "version": f.version})
		case "/api/components/show", "/api/projects/search":
			js(map[string]any{"components": []map[string]string{{"key": q.Get("component")}}})
		case "/api/projects/create":
		case "/api/qualitygates/get_by_project":
			js(map[string]any{"qualityGate": map[string]string{"name": f.selected[q.Get("project")]}})
		case "/api/qualitygates/show":
			conds, ok := f.gates[q.Get("name")]
			if !ok {
				http.Error(w, `{"errors":[{"msg":"not found"}]}`, http.StatusNotFound)
				return
			}
			js(gate{Name: q.Get("name"), Conditions: conds})
		case "/api/qualitygates/create":
			// Like SonarQube 10+: new gates start with default conditions.
			f.nextID++
			f.gates[q.Get("name")] = []condition{{ID: fmt.Sprint(f.nextID), Metric: "coverage", Op: "LT", Error: "80"}}
		case "/api/qualitygates/delete_condition":
			for name, cs := range f.gates {
				kept := cs[:0]
				for _, c := range cs {
					if c.ID != q.Get("id") {
						kept = append(kept, c)
					}
				}
				f.gates[name] = kept
			}
		case "/api/qualitygates/create_condition":
			if f.metrics != nil && !f.metrics[q.Get("metric")] {
				http.Error(w, `{"errors":[{"msg":"unknown metric"}]}`, http.StatusBadRequest)
				return
			}
			for _, c := range f.gates[q.Get("gateName")] {
				if c.Metric == q.Get("metric") {
					http.Error(w, `{"errors":[{"msg":"Condition on metric already exists."}]}`, http.StatusBadRequest)
					return
				}
			}
			f.nextID++
			f.gates[q.Get("gateName")] = append(f.gates[q.Get("gateName")],
				condition{ID: fmt.Sprint(f.nextID), Metric: q.Get("metric"), Op: q.Get("op"), Error: q.Get("error")})
		case "/api/qualitygates/select":
			f.selected[q.Get("projectKey")] = q.Get("gateName")
		case "/api/qualityprofiles/search":
			js(map[string]any{"profiles": f.profiles})
		case "/api/qualityprofiles/backup":
			fmt.Fprintf(w, "<?xml version='1.0'?><profile><name>%s</name><language>%s</language><rules/></profile>", q.Get("qualityProfile"), q.Get("language"))
		case "/api/qualityprofiles/restore":
			file, _, err := r.FormFile("backup")
			if err != nil {
				t.Errorf("restore without file: %v", err)
				return
			}
			b, _ := io.ReadAll(file)
			f.restored = append(f.restored, string(b))
			js(map[string]int{"ruleFailures": 2})
		case "/api/qualityprofiles/add_project":
			f.assigned[q.Get("language")] = q.Get("qualityProfile")
		case "/api/new_code_periods/show":
			js(map[string]any{"type": "NUMBER_OF_DAYS", "value": "30", "inherited": false})
		case "/api/new_code_periods/set":
			f.newCode[q.Get("project")] = q.Get("type") + " " + q.Get("value")
		default:
			t.Errorf("unexpected call %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	})
}

func TestSyncProject(t *testing.T) {
	remote, local := newFake("26.6"), newFake("26.6")
	remote.gates["Strict"] = []condition{{ID: "1", Metric: "coverage", Op: "LT", Error: "80"}, {ID: "2", Metric: "plugin_metric", Op: "GT", Error: "0"}}
	remote.selected["UI-Kit"] = "Strict"
	remote.profiles = []profile{
		{Name: "Sonar way", Language: "ts", IsBuiltIn: true},
		{Name: "Team web", Language: "web"},
	}
	local.profiles = []profile{{Name: "Sonar way", Language: "ts", IsBuiltIn: true}, {Name: "Sonar way", Language: "web", IsBuiltIn: true}}
	local.metrics = map[string]bool{"coverage": true}
	rs, ls := httptest.NewServer(remote.handler(t)), httptest.NewServer(local.handler(t))
	defer rs.Close()
	defer ls.Close()

	rep, err := SyncProject(context.Background(), NewToken(rs.URL, "t"), NewAdmin(ls.URL, "admin", "admin"), "UI-Kit", "UI Kit", "work")
	if err != nil {
		t.Fatal(err)
	}
	if rep.Gate != "work: Strict" || local.selected["UI-Kit"] != "work: Strict" {
		t.Errorf("gate = %q, selected %q", rep.Gate, local.selected["UI-Kit"])
	}
	if c := local.gates["work: Strict"]; len(c) != 1 || c[0].Metric != "coverage" || c[0].Error != "80" {
		t.Errorf("local conditions = %+v", c)
	}
	if len(rep.GateSkipped) != 1 || !strings.Contains(rep.GateSkipped[0], "plugin_metric") {
		t.Errorf("skipped = %v", rep.GateSkipped)
	}
	if len(rep.ProfilesSame) != 1 || rep.ProfilesSame[0] != "ts" {
		t.Errorf("same built-ins = %v", rep.ProfilesSame)
	}
	if local.assigned["web"] != "work: Team web" || len(local.restored) != 1 || !strings.Contains(local.restored[0], "<name>work: Team web</name>") {
		t.Errorf("custom profile: assigned %v restored %v", local.assigned, local.restored)
	}
	if len(rep.RuleFailures) != 1 || local.newCode["UI-Kit"] != "NUMBER_OF_DAYS 30" {
		t.Errorf("rule failures %v, new code %q", rep.RuleFailures, local.newCode["UI-Kit"])
	}

	// A second sync replaces conditions instead of duplicating them.
	remote.gates["Strict"] = []condition{{ID: "1", Metric: "coverage", Op: "LT", Error: "70"}}
	if _, err := SyncProject(context.Background(), NewToken(rs.URL, "t"), NewAdmin(ls.URL, "admin", "admin"), "UI-Kit", "UI Kit", "work"); err != nil {
		t.Fatal(err)
	}
	if c := local.gates["work: Strict"]; len(c) != 1 || c[0].Error != "70" {
		t.Errorf("after resync = %+v", c)
	}
}

func TestSyncCopiesBuiltInAcrossVersions(t *testing.T) {
	remote, local := newFake("26.6"), newFake("26.4")
	remote.gates["Sonar way"] = nil
	remote.selected["p"] = "Sonar way"
	remote.profiles = []profile{{Name: "Sonar way", Language: "ts", IsBuiltIn: true}}
	local.profiles = []profile{{Name: "Sonar way", Language: "ts", IsBuiltIn: true}}
	rs, ls := httptest.NewServer(remote.handler(t)), httptest.NewServer(local.handler(t))
	defer rs.Close()
	defer ls.Close()
	rep, err := SyncProject(context.Background(), NewToken(rs.URL, "t"), NewAdmin(ls.URL, "a", "a"), "p", "p", "work")
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Profiles) != 1 || local.assigned["ts"] != "work: Sonar way" {
		t.Errorf("built-in from another version not copied: %+v", rep)
	}
}

func TestRenameProfile(t *testing.T) {
	in := []byte("<?xml version='1.0'?>\n<profile>\n  <name>Old &amp; name</name>\n  <language>ts</language>\n</profile>")
	out, err := renameProfile(in, "work: A & B $1")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "<name>work: A &amp; B $1</name>") {
		t.Errorf("got %s", out)
	}
	if _, err := renameProfile([]byte("<nope/>"), "x"); err == nil {
		t.Error("accepted non-profile XML")
	}
}
