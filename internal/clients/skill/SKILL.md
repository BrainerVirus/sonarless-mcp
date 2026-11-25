---
name: sonarless-mcp
description: Local SonarQube for this machine via sonarless-mcp — scanning the workspace, checking code quality and quality gates, comparing local results with a remote/CI SonarQube server, adding or updating remote servers, aligning local rules with CI, checking for a newer sonarless-mcp or outdated copies of this skill, and troubleshooting the SonarQube MCP setup. Use when the user mentions sonarless, sonarless-mcp, SonarQube, Sonar issues, code smells, quality gate, coverage, hotspots, or "what did CI find".
---
<!-- managed by sonarless-mcp setup: updated automatically; changes here are overwritten -->

# sonarless-mcp

sonarless-mcp runs one shared local SonarQube per machine and can also query **remote** SonarQube servers such as the company CI. The `sonarqube` MCP server you have is sonarless-mcp.

## Read results with the MCP tools

The CLI has no command that lists issues, hotspots, measures or the gate: read them with the `sonarqube` MCP tools (`search_sonar_issues_in_projects`, `get_project_quality_gate_status`, `get_component_measures`, `search_security_hotspots`, `show_rule`, …). Never guess a CLI command for them.

- Every tool takes **`server`**: `local` (default — the working copy as last scanned) or a remote's name (what CI found, on the remote's default branch).
- `projectKey` defaults to this workspace's project. If a tool still requires it (`sonarless_project` and `sonarless_scan` are missing: your client did not send its workspace), get the key with `sonarless-mcp project`.
- **sonarless_scan** — scan the workspace (1–5 min) and get the quality gate. Local results only change when you scan: run it after code changes, not to read results.
- **sonarless_remote_sync** — copy a remote's quality gate, rule sets and new-code definition to the local server so local scans pass/fail like CI. Follow with `sonarless_scan`.
- **sonarless_status** — containers and memory, gateway sessions, running scans (check before starting one), remotes and whether they're reachable now.
- **sonarless_project** — project key/name, where they came from, which servers have the project.

Call these as MCP tools. Their terminal forms are `sonarless-mcp scan`, `sonarless-mcp remote sync <name>`, `sonarless-mcp status` and `sonarless-mcp project`.

## Typical flows

- "What's wrong with my code?" → `sonarless_scan`, then the tools with `server: "local"`.
- "What did CI find?" → the tools with `server: "<remote>"`. "Compare with CI" → the same tool once with `local` and once with the remote. Differences usually mean different code (CI analyzed another commit — check `git fetch`) or different rules/gate: offer `sonarless_remote_sync`, don't run it unasked.
- "Will this pass CI's gate?" → `sonarless_remote_sync`, then `sonarless_scan`.
- A failing gate is a result, not an error: report its failing conditions.

## Terminal commands

Run these yourself when asked; they change nothing: `sonarless-mcp upgrade --check` (is a newer release out), `sonarless-mcp skill list` (each installed copy of this skill, its version and state), `sonarless-mcp version`, `sonarless-mcp status` (`--json` for details), `sonarless-mcp remote list`, `sonarless-mcp setup --list`.

Only when the user asks for the change: `sonarless-mcp upgrade` (it also upgrades itself in the background), `sonarless-mcp skill sync` (update outdated copies), `sonarless-mcp setup` (register in AI clients), `sonarless-mcp stop --all` (free everything; plain `stop` keeps the gateway), `sonarless-mcp config` (settings, e.g. `SONARLESS_SONARQUBE_VERSION` to match CI; `sonarless-mcp --help` lists every `SONARLESS_*` variable).

Tokens never go through chat. Even when the user pastes one, never put it in a command: give them these to run in their own terminal:

- Add a remote: `sonarless-mcp remote add <name> --url <url> [--branch <branch>]` (hidden token prompt; needs a **User token** `squ_…`, not an analysis token).
- Change it: `sonarless-mcp remote update <name> --token` / `--url <url>` / `--branch <branch>`; remove it: `sonarless-mcp remote remove <name>`.

A remote change applies to the next MCP request; if the client still lists the old `server` choices, reconnect the MCP server.

## Troubleshooting

- Remote unreachable: the user is probably off the VPN/network. Say so and answer from `local` in the same reply.
- Remote token rejected: the user runs `sonarless-mcp remote update <name> --token`.
- First call slow: SonarQube is starting (it stops after `SONARLESS_IDLE_TIMEOUT`, default 30 min, unused and resumes on demand).
- Project not found on a server: scan it locally, or it isn't analyzed on that remote.
- Exit status of `sonarless-mcp scan`: 0 even when the gate fails, unless `--fail-on-gate` (or `SONARLESS_FAIL_ON_GATE=true`, meant for CI and scripts) makes a failing gate exit 2; exit 1 is a failed scan.
- `sonarless-mcp upgrade` refused a release (signature or checksum check failed): the installed binary is unchanged; tell the user, don't work around it. `--allow-unsigned` is only for deliberately going back to a release from before signing (v0.8.1 or older).
- A usage error names the flag you probably meant (`did you mean --tests?`): retry with it; `sonarless-mcp <command> --help` lists the rest. This skill looks outdated: `sonarless-mcp skill list`, then `sonarless-mcp skill sync`.
- Terminal says `sonarless-mcp` not found right after install: `rehash` (zsh), `hash -r` (bash), or a new tab (Warp).
