package mcp

import (
	"encoding/json"
	"fmt"
	"strings"
)

// annotateInitialize tells the agent which project this workspace maps to
// and which servers it can query.
func (s *Shim) annotateInitialize(result json.RawMessage) json.RawMessage {
	var note strings.Builder
	if s.Project != nil && s.local().inject.Load() {
		fmt.Fprintf(&note, "\n\nThis workspace is SonarQube project %q (detected from %s); tools default projectKey to it.",
			s.Project.Key, s.Project.KeySource)
	}
	if len(s.Backends) > 1 {
		note.WriteString("\n\nEvery tool takes an optional `server` argument: " + s.serverHelp() +
			" Use the local server by default (the current working copy); query a remote one only when the user asks" +
			" about CI or remote results. Remotes can be offline (e.g. VPN not connected): if one is unreachable, say so" +
			" and carry on with the local server.")
	}
	if len(s.Local) > 0 {
		note.WriteString("\n\nsonarless-mcp also provides its own tools (sonarless_status, sonarless_project, sonarless_scan" +
			", sonarless_remote_sync when remotes exist) to check setup, scan this workspace and align local rules with CI.")
	}
	if note.Len() == 0 {
		return result
	}
	var r map[string]any
	if json.Unmarshal(result, &r) != nil {
		return result
	}
	instr, _ := r["instructions"].(string)
	r["instructions"] = instr + note.String()
	b, err := json.Marshal(r)
	if err != nil {
		return result
	}
	return b
}

func (s *Shim) serverHelp() string {
	var parts []string
	for _, b := range s.Backends {
		parts = append(parts, fmt.Sprintf("%q = %s", b.Name, b.About))
	}
	return strings.Join(parts, "; ") + "."
}

// annotateTools makes projectKey optional (the shim fills it in), documents
// the default, and adds the `server` argument when remotes are configured.
func (s *Shim) annotateTools(result json.RawMessage) json.RawMessage {
	return s.withLocalTools(s.annotateRemoteTools(result))
}

func (s *Shim) annotateRemoteTools(result json.RawMessage) json.RawMessage {
	inject := s.Project != nil && s.local().inject.Load()
	multi := len(s.Backends) > 1
	if !inject && !multi {
		return result
	}
	var raw map[string]any
	if json.Unmarshal(result, &raw) != nil {
		return result
	}
	tools, _ := raw["tools"].([]any)
	for _, x := range tools {
		t, _ := x.(map[string]any)
		schema, _ := t["inputSchema"].(map[string]any)
		if schema == nil {
			continue
		}
		props, _ := schema["properties"].(map[string]any)
		if props == nil {
			props = map[string]any{}
			schema["properties"] = props
		}
		if inject {
			for _, p := range []string{"projectKey", "projects"} {
				prop, ok := props[p].(map[string]any)
				if !ok {
					continue
				}
				desc, _ := prop["description"].(string)
				prop["description"] = fmt.Sprintf("%s (Defaults to this workspace's project %q.)", desc, s.Project.Key)
				if req, ok := schema["required"].([]any); ok {
					kept := req[:0]
					for _, r := range req {
						if r != p {
							kept = append(kept, r)
						}
					}
					schema["required"] = kept
				}
			}
		}
		if multi {
			enum := make([]any, len(s.Backends))
			for i, b := range s.Backends {
				enum[i] = b.Name
			}
			props["server"] = map[string]any{
				"type":        "string",
				"enum":        enum,
				"description": "Which SonarQube to query (default \"" + s.local().Name + "\"): " + s.serverHelp(),
			}
		}
	}
	b, err := json.Marshal(raw)
	if err != nil {
		return result
	}
	return b
}

// toolProps returns the input property names of a tool from the cached list.
func toolProps(tool string, cachedTools json.RawMessage) map[string]bool {
	var r struct {
		Tools []struct {
			Name        string `json:"name"`
			InputSchema struct {
				Properties map[string]json.RawMessage `json:"properties"`
			} `json:"inputSchema"`
		} `json:"tools"`
	}
	if json.Unmarshal(cachedTools, &r) != nil {
		return nil
	}
	for _, t := range r.Tools {
		if t.Name == tool {
			out := map[string]bool{}
			for k := range t.InputSchema.Properties {
				out[k] = true
			}
			return out
		}
	}
	return nil
}

// fillDefaults removes the shim's `server` argument and fills in the
// backend's defaults the agent didn't give: the workspace's project (only
// where it exists on that server) and the backend's branch.
func (s *Shim) fillDefaults(b *Backend, params json.RawMessage) json.RawMessage {
	var raw map[string]any
	if json.Unmarshal(params, &raw) != nil {
		return params
	}
	args, _ := raw["arguments"].(map[string]any)
	if args == nil {
		args = map[string]any{}
	}
	delete(args, "server")
	name, _ := raw["name"].(string)
	props := toolProps(name, s.cached("tools/list"))
	empty := func(k string) bool {
		switch v := args[k].(type) {
		case nil:
			return true
		case string:
			return v == ""
		case []any:
			return len(v) == 0
		}
		return false
	}
	if s.Project != nil && b.inject.Load() {
		switch {
		case props["projectKey"] && empty("projectKey"):
			args["projectKey"] = s.Project.Key
		case props["projects"] && empty("projects"):
			args["projects"] = []string{s.Project.Key}
		}
	}
	if b.Branch != "" && props["branch"] && empty("branch") {
		args["branch"] = b.Branch
	}
	raw["arguments"] = args
	out, err := json.Marshal(raw)
	if err != nil {
		return params
	}
	return out
}
