package mcp

import (
	"encoding/json"
	"fmt"
)

// annotateInitialize tells the agent which project this workspace maps to.
func (s *Shim) annotateInitialize(result json.RawMessage) json.RawMessage {
	if !s.inject.Load() {
		return result
	}
	var r map[string]any
	if json.Unmarshal(result, &r) != nil {
		return result
	}
	note := fmt.Sprintf("\n\nThis workspace is SonarQube project %q (detected from %s); tools default projectKey to it.",
		s.Project.Key, s.Project.KeySource)
	instr, _ := r["instructions"].(string)
	r["instructions"] = instr + note
	b, err := json.Marshal(r)
	if err != nil {
		return result
	}
	return b
}

// annotateTools makes projectKey optional (it is filled in by the shim) and
// documents the default, mirroring the server's own SONARQUBE_PROJECT_KEY
// behavior but per workspace.
func (s *Shim) annotateTools(result json.RawMessage) json.RawMessage {
	if !s.inject.Load() {
		return result
	}
	var r struct {
		Tools []map[string]any `json:"tools"`
	}
	var raw map[string]any
	if json.Unmarshal(result, &raw) != nil || json.Unmarshal(result, &r) != nil {
		return result
	}
	for _, t := range r.Tools {
		schema, _ := t["inputSchema"].(map[string]any)
		props, _ := schema["properties"].(map[string]any)
		for _, p := range []string{"projectKey", "projects"} {
			prop, ok := props[p].(map[string]any)
			if !ok {
				continue
			}
			desc, _ := prop["description"].(string)
			prop["description"] = fmt.Sprintf("%s (Defaults to this workspace's project %q.)", desc, s.Project.Key)
			if req, ok := schema["required"].([]any); ok {
				kept := req[:0]
				for _, x := range req {
					if x != p {
						kept = append(kept, x)
					}
				}
				schema["required"] = kept
			}
		}
	}
	raw["tools"] = r.Tools
	b, err := json.Marshal(raw)
	if err != nil {
		return result
	}
	return b
}

// projectArg returns, per tool, which argument carries the project: tools
// whose schema has projectKey take a string, search_sonar_issues_in_projects
// takes a list. Unknown tools are left alone.
func projectArg(tool string, cachedTools json.RawMessage) (string, bool) {
	var r struct {
		Tools []struct {
			Name        string `json:"name"`
			InputSchema struct {
				Properties map[string]json.RawMessage `json:"properties"`
			} `json:"inputSchema"`
		} `json:"tools"`
	}
	if json.Unmarshal(cachedTools, &r) != nil {
		return "", false
	}
	for _, t := range r.Tools {
		if t.Name != tool {
			continue
		}
		if _, ok := t.InputSchema.Properties["projectKey"]; ok {
			return "projectKey", true
		}
		if _, ok := t.InputSchema.Properties["projects"]; ok {
			return "projects", true
		}
	}
	return "", false
}

// injectProject fills in the workspace's project for a tools/call that
// didn't name one.
func (s *Shim) injectProject(params json.RawMessage) json.RawMessage {
	if !s.inject.Load() {
		return params
	}
	var p struct {
		Name      string         `json:"name"`
		Arguments map[string]any `json:"arguments"`
	}
	var raw map[string]any
	if json.Unmarshal(params, &raw) != nil || json.Unmarshal(params, &p) != nil {
		return params
	}
	arg, ok := projectArg(p.Name, s.cached("tools/list"))
	if !ok {
		return params
	}
	if p.Arguments == nil {
		p.Arguments = map[string]any{}
	}
	switch v := p.Arguments[arg].(type) {
	case nil:
	case string:
		if v != "" {
			return params
		}
	case []any:
		if len(v) > 0 {
			return params
		}
	default:
		return params
	}
	if arg == "projects" {
		p.Arguments[arg] = []string{s.Project.Key}
	} else {
		p.Arguments[arg] = s.Project.Key
	}
	raw["arguments"] = p.Arguments
	b, err := json.Marshal(raw)
	if err != nil {
		return params
	}
	return b
}
