---
name: sonarless-mcp
description: Local SonarQube for this machine via sonarless-mcp — scanning the workspace, checking code quality and quality gates, comparing local results with a remote/CI SonarQube server, adding or updating remote servers, aligning local rules with CI, and troubleshooting the SonarQube MCP setup. Use when the user mentions sonarless, sonarless-mcp, SonarQube, Sonar issues, code smells, quality gate, coverage, hotspots, or "what did CI find".
---
<!-- managed by sonarless-mcp setup: updated automatically; changes here are overwritten -->

# sonarless-mcp

sonarless-mcp runs SonarQube locally (one shared server + one shared SonarQube MCP server per machine) and can also query **remote** SonarQube servers such as the company CI server. The `sonarqube` MCP server you have is sonarless-mcp.

## Tools you have

- **SonarQube tools** (`search_sonar_issues_in_projects`, `get_project_quality_gate_status`, `get_component_measures`, `show_rule`, …). `projectKey` defaults to this workspace's project. If it doesn't (the tools still require `projectKey` and `sonarless_project`/`sonarless_scan` are missing, so your client did not send its workspace), run `sonarless-mcp project` in the workspace to get the key and pass it explicitly. Every tool takes an optional **`server`** argument: `local` (default — the working copy as last scanned) or a remote's name (what CI found, on the remote's default branch).
- **sonarless_status** — containers and their memory, the gateway and its sessions, running stdio bridges and scans (check it before starting a scan another session may already run), remotes and whether they're reachable now.
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
- Other: `sonarless-mcp remote list|remove <name>`, `sonarless-mcp setup` (register in AI clients; `--list` shows what is registered), `sonarless-mcp upgrade` (`--check` to see if one is due; it also upgrades itself in the background), `sonarless-mcp version`, `sonarless-mcp config` (settings, e.g. `SONARLESS_SONARQUBE_VERSION` to match CI; `sonarless-mcp --help` lists every `SONARLESS_*` variable).

After adding/changing/removing a remote, the next MCP request already uses it (no client restart); if the client still lists the old `server` choices, reconnect the MCP server.

## Troubleshooting

- Remote "unreachable": the user is probably off the VPN/network; carry on with `local`.
- Remote token rejected: user runs `sonarless-mcp remote update <name> --token`.
- First call slow: SonarQube is starting (it stops itself after `SONARLESS_IDLE_TIMEOUT`, default 30 min, unused and resumes on demand).
- What is running and how much memory it takes: `sonarless-mcp status` (`--json` for details: containers with memory, gateway sessions, stdio bridges, running scans). To free it all, the user runs `sonarless-mcp stop --all` (plain `stop` keeps the gateway).
- Project not found on a server: scan it locally, or it isn't analyzed on that remote.
- `sonarless_scan` or `sonarless-mcp scan` finished but the gate failed: that is a result, not an error; read the failing conditions with the SonarQube tools. Only `sonarless-mcp scan --fail-on-gate` (or `SONARLESS_FAIL_ON_GATE=true`, meant for CI and scripts) turns it into exit status 2; exit 1 is still a failed scan.
- `sonarless-mcp upgrade` refused a release (signature or checksum check failed): the installed binary is unchanged; tell the user, don't work around it. `--allow-unsigned` is only for deliberately going back to a release from before signing (v0.8.1 or older).
- This skill looks outdated, or a command says a flag is unknown: `sonarless-mcp skill list` shows each installed copy's version and state, `sonarless-mcp skill sync` updates them; a usage error names the flag you probably meant (`did you mean --tests?`), and `sonarless-mcp <command> --help` lists the rest.
- Terminal says `sonarless-mcp` not found right after install: `rehash` (zsh), `hash -r` (bash), or a new tab (Warp).
