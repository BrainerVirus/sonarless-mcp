package main

import (
	"bufio"
	"encoding/json"
	"io"
)

// The fake SonarQube MCP server: `sonarless-mcp mcp` in a world. It speaks
// MCP over stdio (newline-delimited JSON-RPC), lists the tools the real shim
// lists (the sonarless_* tools plus the SonarQube tools agents use most, with
// the `server` argument of a workspace that has one remote, "work") and
// answers each call from the case's fixtures, then from canned results. It
// reaches no SonarQube, docker or network.

const fakeInstructions = "SonarQube MCP server." +
	"\n\nThis workspace is SonarQube project \"demo-app\" (detected from sonar-project.properties); tools default projectKey to it." +
	"\n\nEvery tool takes an optional `server` argument: \"local\" = the local SonarQube, scanned from your working copy (`sonarless-mcp scan`);" +
	" \"work\" = remote SonarQube http://sonar.example.com (branch develop by default)." +
	" Use the local server by default (the current working copy); query a remote one only when the user asks" +
	" about CI or remote results. Remotes can be offline (e.g. VPN not connected): if one is unreachable, say so" +
	" and carry on with the local server." +
	"\n\nsonarless-mcp also provides its own tools (sonarless_status, sonarless_project, sonarless_scan" +
	", sonarless_remote_sync when remotes exist) to check setup, scan this workspace and align local rules with CI."

var serverArg = map[string]any{
	"type": "string", "enum": []string{"local", "work"},
	"description": "Which SonarQube to query (default \"local\"): \"local\" = the local SonarQube, scanned from your working copy (`sonarless-mcp scan`); \"work\" = remote SonarQube http://sonar.example.com (branch develop by default).",
}

func str(desc string) map[string]any { return map[string]any{"type": "string", "description": desc} }

func strs(desc string) map[string]any {
	return map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": desc}
}

func tool(name, desc string, props map[string]any, required ...string) map[string]any {
	if required == nil {
		required = []string{}
	}
	return map[string]any{
		"name": name, "description": desc,
		"inputSchema": map[string]any{"type": "object", "properties": props, "required": required},
	}
}

const branchDesc = "Branch name for branch-based analysis. Discover names with list_branches. For pull request analysis, use pullRequest instead."

// fakeTools are the tools the fake lists, with the real descriptions.
var fakeTools = []map[string]any{
	tool("sonarless_status", "sonarless-mcp status: the local SonarQube and MCP containers, configured remote SonarQube servers (and whether they are reachable right now), idle auto-stop and version. Use it to diagnose problems or before suggesting remote changes.", map[string]any{}),
	tool("sonarless_project", "How sonarless-mcp sees this workspace: SonarQube project key and name, where they were detected (sonar-project.properties, pom.xml, Gradle, CI pipeline, ...), the scanner it would use, and which servers (local, remotes) know the project.", map[string]any{}),
	tool("sonarless_scan", "Scan this workspace with the local SonarQube (starting it if needed) and return the quality gate result. Takes about 1-5 minutes. Use it after changing code to refresh what the SonarQube tools report for server \"local\".", map[string]any{
		"scanner": map[string]any{"type": "string", "enum": []string{"cli", "maven", "gradle", "dotnet"}, "description": "Override the detected scanner (rarely needed)."},
		"tests":   map[string]any{"type": "boolean", "description": "Run tests during maven/gradle/dotnet scans (needed for coverage; slower)."},
	}),
	tool("sonarless_remote_sync", "Make local scans judge this project like a remote server (typically CI): copy its quality gate, the rule sets the project uses there and its new-code definition to the local server. Run sonarless_scan afterwards to see the project through the remote's rules.", map[string]any{
		"remote": map[string]any{"type": "string", "enum": []string{"work"}, "description": "Remote to copy from."},
	}, "remote"),
	tool("search_sonar_issues_in_projects", "Search for issues (bugs, vulnerabilities, code smells) in my projects. Filter by severities=['HIGH','BLOCKER'] for critical issues, impactSoftwareQualities=['SECURITY'] for security, issueStatuses=['OPEN'] to exclude resolved.", map[string]any{
		"projectKeys":             strs("An optional list of SonarQube project keys to look in"),
		"files":                   strs("An optional list of component keys (files, directories, modules) to filter issues"),
		"severities":              strs("An optional list of severities to filter by (INFO, LOW, MEDIUM, HIGH, BLOCKER)"),
		"impactSoftwareQualities": strs("An optional list of software qualities to filter by (MAINTAINABILITY, RELIABILITY, SECURITY)"),
		"issueStatuses":           strs("An optional list of issue statuses to filter by (OPEN, CONFIRMED, FALSE_POSITIVE, ACCEPTED, FIXED)"),
		"branch":                  str(branchDesc),
		"server":                  serverArg,
	}),
	tool("get_project_quality_gate_status", "Get the Quality Gate Status for a project. Either 'analysisId', 'projectId' or 'projectKey' must be provided.", map[string]any{
		"projectKey": str("The optional project key to get the status for, for example 'my_project'"),
		"branch":     str(branchDesc),
		"server":     serverArg,
	}),
	tool("get_component_measures", "Get SonarQube measures for a project, such as ncloc, complexity, violations, coverage, etc.", map[string]any{
		"projectKey": str("The SonarQube project key (e.g. my_project). Defaults to this workspace's project."),
		"metricKeys": strs("The metric keys to retrieve (e.g. ncloc, complexity, violations, coverage)"),
		"branch":     str(branchDesc),
		"server":     serverArg,
	}),
	tool("show_rule", "Shows detailed information about a SonarQube rule.", map[string]any{
		"key":    str("The rule key (e.g. javascript:EmptyBlock)"),
		"server": serverArg,
	}, "key"),
	tool("search_security_hotspots", "Search for security hotspots in a project.", map[string]any{
		"projectKey": str("The project key. Defaults to this workspace's project."),
		"server":     serverArg,
	}),
	tool("list_branches", "List the branches SonarQube analyzed for a project.", map[string]any{
		"projectKey": str("The project key. Defaults to this workspace's project."),
		"server":     serverArg,
	}),
}

// builtinToolReply is a tool's canned result when no fixture rule matches.
func builtinToolReply(name string) string {
	switch name {
	case "sonarless_status":
		return "SonarQube server: running (1.9 GiB). MCP: local running, work running. Remotes: work http://sonar.example.com reachable. Scans: none running. Idle stop after 30m unused. Version v0.14.2."
	case "sonarless_project":
		return `{"key":"demo-app","keySource":"sonar-project.properties","name":"Demo App","scanner":"cli","servers":{"local":"scanned","work":"found"}}`
	case "sonarless_scan":
		return "Scan finished in 48s. Quality gate: PASSED (project demo-app)."
	case "sonarless_remote_sync":
		return "Copied from work: quality gate \"CI way\", 3 quality profiles, new code = previous version. Run sonarless_scan to see the project through work's rules."
	case "search_sonar_issues_in_projects":
		return `{"issues":[],"paging":{"pageIndex":1,"pageSize":100,"total":0}}`
	case "get_project_quality_gate_status":
		return `{"status":"OK","conditions":[]}`
	case "get_component_measures":
		return `{"component":{"key":"demo-app","measures":[{"metric":"coverage","value":"78.4"},{"metric":"ncloc","value":"5120"}]}}`
	case "show_rule":
		return `{"key":"typescript:S1854","name":"Unused assignments should be removed","severity":"MAJOR"}`
	case "search_security_hotspots":
		return `{"hotspots":[],"paging":{"total":0}}`
	case "list_branches":
		return `{"branches":[{"name":"main","isMain":true}]}`
	}
	return "{}"
}

type rpcMsg struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// serveFakeMCP answers MCP requests on in until it closes.
func serveFakeMCP(harness string, c *Case, in io.Reader, out io.Writer) int {
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 0, 1<<20), 16<<20)
	enc := json.NewEncoder(out)
	for sc.Scan() {
		var m rpcMsg
		if json.Unmarshal(sc.Bytes(), &m) != nil || len(m.ID) == 0 {
			continue // notifications need no answer
		}
		result, rpcErr := handleMCP(harness, c, m)
		resp := map[string]any{"jsonrpc": "2.0", "id": m.ID}
		if rpcErr != nil {
			resp["error"] = rpcErr
		} else {
			resp["result"] = result
		}
		if enc.Encode(resp) != nil {
			return 1
		}
	}
	return 0
}

func handleMCP(harness string, c *Case, m rpcMsg) (any, map[string]any) {
	switch m.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(m.Params, &p)
		if p.ProtocolVersion == "" {
			p.ProtocolVersion = "2025-06-18"
		}
		return map[string]any{
			"protocolVersion": p.ProtocolVersion,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "sonarless-mcp", "version": "0.14.2"},
			"instructions":    fakeInstructions,
		}, nil
	case "ping":
		return map[string]any{}, nil
	case "tools/list":
		return map[string]any{"tools": fakeTools}, nil
	case "tools/call":
		var p struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		}
		if err := json.Unmarshal(m.Params, &p); err != nil {
			return nil, map[string]any{"code": -32602, "message": err.Error()}
		}
		return callTool(harness, c, p.Name, p.Arguments), nil
	}
	return nil, map[string]any{"code": -32601, "message": "method not found: " + m.Method}
}

func callTool(harness string, c *Case, name string, args map[string]any) map[string]any {
	call := Call{Tool: name, Args: args}
	text, exit := "", 0
	known := false
	for _, t := range fakeTools {
		known = known || t["name"] == name
	}
	if r, ok := fixtureReply(c, call.Line()); ok {
		text, exit = r.stdout+r.stderr, r.exit
	} else if !known {
		text, exit = "unknown tool: "+name, 1
	} else {
		text = builtinToolReply(name)
	}
	call.Exit = exit
	if err := logCall(harness, call); err != nil {
		text, exit = "fake sonarless-mcp: "+err.Error(), 1
	}
	return map[string]any{
		"content": []map[string]any{{"type": "text", "text": text}},
		"isError": exit != 0,
	}
}
