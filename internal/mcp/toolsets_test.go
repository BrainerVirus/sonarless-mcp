package mcp

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BrainerVirus/sonarless-mcp/internal/config"
)

// upstreamHTTPTools is every tool the SonarQube MCP server (1.33) lists over
// HTTP with its default toolsets, by toolset (ToolCategory in its source).
var upstreamHTTPTools = map[string]string{
	"get_file_coverage_details":       "coverage",
	"search_files_by_coverage":        "coverage",
	"get_duplications":                "duplications",
	"search_duplicated_files":         "duplications",
	"search_sonar_issues_in_projects": "issues",
	"change_sonar_issue_status":       "issues",
	"get_component_measures":          "measures",
	"search_metrics":                  "measures",
	"search_my_sonarqube_projects":    "projects",
	"list_branches":                   "projects",
	"list_pull_requests":              "projects",
	"get_project_quality_gate_status": "quality-gates",
	"list_quality_gates":              "quality-gates",
	"show_rule":                       "rules",
	"search_security_hotspots":        "security-hotspots",
	"show_security_hotspot":           "security-hotspots",
	"change_security_hotspot_status":  "security-hotspots",
	"search_dependency_risks":         "dependency-risks",
}

// TestDefaultToolsetsKeepDocumentedTools: every SonarQube MCP tool the skill
// or the docs name stays in the default toolsets.
func TestDefaultToolsetsKeepDocumentedTools(t *testing.T) {
	enabled := map[string]bool{}
	for _, ts := range strings.Split(config.DefaultMCPToolsets, ",") {
		enabled[ts] = true
	}
	if enabled["dependency-risks"] {
		t.Error("dependency-risks (SonarQube Enterprise only) is in the default")
	}
	root := filepath.Join("..", "..")
	files := []string{filepath.Join(root, "README.md"), filepath.Join(root, "internal", "clients", "skill", "SKILL.md")}
	docs, _ := filepath.Glob(filepath.Join(root, "docs", "*.md"))
	named := 0
	for _, f := range append(files, docs...) {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for tool, ts := range upstreamHTTPTools {
			// The docs name the dropped tool to explain the default.
			if ts != "dependency-risks" && strings.Contains(string(b), tool) {
				named++
				if !enabled[ts] {
					t.Errorf("%s names %s, but its toolset %s is not in the default", f, tool, ts)
				}
			}
		}
	}
	if named == 0 {
		t.Fatal("no SonarQube MCP tool named in the skill or docs: the scan is broken")
	}
}
