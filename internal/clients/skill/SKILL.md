---
name: sonarless-mcp
description: Local SonarQube for this machine via sonarless-mcp — scanning the workspace, checking code quality and quality gates, comparing local results with a remote/CI SonarQube server, adding or updating remote servers, aligning local rules with CI, and troubleshooting the SonarQube MCP setup. Use when the user mentions sonarless, sonarless-mcp, SonarQube, Sonar issues, code smells, quality gate, coverage, hotspots, or "what did CI find".
---
<!-- managed by sonarless-mcp setup: updated automatically; changes here are overwritten -->

# sonarless-mcp

sonarless-mcp runs SonarQube locally (one shared server + one shared SonarQube MCP server per machine) and can also query **remote** SonarQube servers such as the company CI server. The `sonarqube` MCP server you have is sonarless-mcp.

## Tools you have

- **SonarQube tools** (`search_sonar_issues_in_projects`, `get_project_quality_gate_status`, `get_component_measures`, `show_rule`, …). `projectKey` defaults to this workspace's project. Every tool takes an optional **`server`** argument: `local` (default — the working copy as last scanned) or a remote's name (what CI found, on the remote's default branch).
- **sonarless_status** — local containers, remotes and whether they're reachable now.
- **sonarless_project** — detected project key/name, where they came from, which servers have the project.
- **sonarless_scan** — scan the workspace locally (1–5 min) and get the quality gate. Run it after code changes; local results only update when you scan.
- **sonarless_remote_sync** — copy a remote's quality gate, rule sets and new-code definition to the local server so local scans pass/fail like CI. Follow with `sonarless_scan`.

## Typical flows

- "What's wrong with my code?" → `sonarless_scan`, then SonarQube tools with `server: "local"`.
- "What did CI find?" / "compare with CI" → SonarQube tools with `server: "<remote>"`; differences with local usually mean different code (CI analyzed another commit — check `git fetch`) or different rules/gate (offer `sonarless_remote_sync`).
- "Will this pass CI's gate?" → `sonarless_remote_sync`, then `sonarless_scan`.

## Things only the user can do (terminal)

Tokens never go through chat. Ask the user to run these themselves:

- Add a remote: `sonarless-mcp remote add <name> --url <url> [--branch <branch>]` (hidden token prompt; needs a **User token** `squ_…`, not an analysis token).
- Change it: `sonarless-mcp remote update <name> --token` / `--url <url>` / `--branch <branch>`.
- Other: `sonarless-mcp remote list|remove <name>`, `sonarless-mcp setup` (register in AI clients), `sonarless-mcp update`, `sonarless-mcp config` (settings, e.g. `SONARLESS_SONARQUBE_VERSION` to match CI).

After adding/changing a remote, the AI client must be restarted to see it.

## Troubleshooting

- Remote "unreachable": the user is probably off the VPN/network; carry on with `local`.
- Remote token rejected: user runs `sonarless-mcp remote update <name> --token`.
- First call slow: SonarQube is starting (it stops itself after 30 min unused and resumes on demand).
- Project not found on a server: scan it locally, or it isn't analyzed on that remote.
- Terminal says `sonarless-mcp` not found right after install: `rehash` (zsh), `hash -r` (bash), or a new tab (Warp).
