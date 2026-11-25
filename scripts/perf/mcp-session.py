"""mcp-session.py <port> <token> <projectKey>: an agent-like MCP session against
an MCP container: initialize, tools/list, then 12 tool calls (projects, quality
gate, measures, six pages of issues, three rules). Prints one summary line;
failing calls go to stderr."""

import json
import sys
import time
import urllib.request

port, token, key = sys.argv[1], sys.argv[2], sys.argv[3]
url = f"http://127.0.0.1:{port}/mcp"
session = None


def call(method, params=None, id_=None):
    global session
    body = {"jsonrpc": "2.0", "method": method}
    if params is not None:
        body["params"] = params
    if id_ is not None:
        body["id"] = id_
    headers = {
        "Content-Type": "application/json",
        "Accept": "application/json, text/event-stream",
        "Authorization": "Bearer " + token,
    }
    if session:
        headers["Mcp-Session-Id"] = session
    req = urllib.request.Request(url, json.dumps(body).encode(), headers)
    with urllib.request.urlopen(req, timeout=120) as r:
        session = r.headers.get("Mcp-Session-Id") or session
        raw = r.read().decode()
    if id_ is None:
        return None
    for line in raw.splitlines():  # an SSE answer carries the JSON on its data: line
        if line.startswith("data:"):
            raw = line[5:]
    return json.loads(raw)


start = time.time()
call("initialize", {"protocolVersion": "2025-06-18", "capabilities": {},
                    "clientInfo": {"name": "perftest", "version": "0"}}, 1)
call("notifications/initialized")
tools = call("tools/list", {}, 2)["result"]["tools"]
calls = [
    ("search_my_sonarqube_projects", {}),
    ("get_project_quality_gate_status", {"projectKey": key}),
    ("get_component_measures", {"projectKey": key,
                                "metricKeys": ["ncloc", "bugs", "code_smells", "coverage", "duplicated_lines_density"]}),
]
calls += [("search_sonar_issues_in_projects", {"projectKeys": [key], "pageIndex": p, "pageSize": 100}) for p in range(1, 7)]
calls += [("show_rule", {"key": k}) for k in ("typescript:S1854", "typescript:S3776", "typescript:S1186")]
errors = 0
for i, (name, args) in enumerate(calls):
    res = call("tools/call", {"name": name, "arguments": args}, 10 + i)
    if "error" in res or res.get("result", {}).get("isError"):
        errors += 1
        print(name, json.dumps(res)[:300], file=sys.stderr)
print(f"tools={len(tools)} calls={len(calls)} tool_errors={errors} session_s={time.time() - start:.2f}")
