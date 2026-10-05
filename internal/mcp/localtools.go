package mcp

import (
	"context"
	"encoding/json"
)

// LocalTool is a tool the shim answers itself (sonarless-mcp's own features:
// scans, status, sync) instead of forwarding to a SonarQube MCP server.
type LocalTool struct {
	Name        string
	Description string
	InputSchema map[string]any
	// Run returns the text result; isError marks a failed (but recoverable) call.
	Run func(ctx context.Context, args map[string]any) (text string, isError bool)
}

func (s *Shim) localTool(name string) *LocalTool {
	for i := range s.Local {
		if s.Local[i].Name == name {
			return &s.Local[i]
		}
	}
	return nil
}

// withLocalTools appends the shim's own tools to a tools/list result.
func (s *Shim) withLocalTools(result json.RawMessage) json.RawMessage {
	if len(s.Local) == 0 {
		return result
	}
	var raw map[string]any
	if json.Unmarshal(result, &raw) != nil {
		return result
	}
	tools, _ := raw["tools"].([]any)
	for _, t := range s.Local {
		schema := t.InputSchema
		if schema == nil {
			schema = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		tools = append(tools, map[string]any{"name": t.Name, "description": t.Description, "inputSchema": schema})
	}
	raw["tools"] = tools
	b, err := json.Marshal(raw)
	if err != nil {
		return result
	}
	return b
}

// runLocal answers a tools/call for a local tool. Local tools don't need the
// SonarQube MCP container, so they work even while it is starting or down.
func (s *Shim) runLocal(ctx context.Context, req message, t *LocalTool) {
	var p struct {
		Arguments map[string]any `json:"arguments"`
	}
	_ = json.Unmarshal(req.Params, &p)
	if p.Arguments == nil {
		p.Arguments = map[string]any{}
	}
	text, isErr := t.Run(ctx, p.Arguments)
	res, _ := json.Marshal(map[string]any{
		"isError": isErr,
		"content": []map[string]string{{"type": "text", "text": text}},
	})
	s.send(message{JSONRPC: "2.0", ID: req.ID, Result: res})
}
