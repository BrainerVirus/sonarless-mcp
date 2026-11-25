# AI clients

sonarless-mcp registers one `sonarqube` MCP server in Claude Code, Cursor,
opencode, VS Code and Codex. Every client shares one SonarQube server and one
SonarQube MCP container on the machine.

## Setup

```sh
sonarless-mcp setup                          # picker: installed clients preselected
sonarless-mcp setup --list                   # what is detected/configured; change nothing
sonarless-mcp setup --yes                    # every detected client, no picker
sonarless-mcp setup --clients claude,cursor  # these clients, no picker
sonarless-mcp setup --stdio --yes            # the stdio bridge everywhere instead of direct HTTP
sonarless-mcp setup --remove --yes           # unregister from every configured client
```

Client ids: `claude`, `cursor`, `opencode`, `vscode`, `codex`. Without a
terminal, setup needs `--yes`, `--clients` or `--list`.

Setup edits only the `sonarqube` entry of each client's config: Claude Code
through `claude mcp add-json` (user scope), Cursor `~/.cursor/mcp.json`,
opencode `opencode.json`, VS Code's user `mcp.json`, Codex `config.toml`. If a
client already has a different `sonarqube` server (say, your company's remote
SonarQube), it is left untouched and sonarless-mcp registers as
`sonarqube-local` instead. To register by hand, print the snippets for a
client:

```sh
sonarless-mcp mcp-config claude   # or opencode, cursor, vscode, codex
```

Restart the clients after setup. The first tool call pulls the SonarQube
images (a few GB); `sonarless-mcp start` gets that out of the way.

## How clients connect

```text
Claude Code ─┐  direct HTTP        ┌───────────────────────┐   ┌──────────────────────────┐   ┌──────────────┐
Cursor ──────┼────────────────────▶│ sonarless-mcp gateway │──▶│ sonarless-mcp container  ├──▶│ SonarQube    │
opencode ────┤                     │ (one per machine,     │   │ (SonarQube MCP, HTTP,    │   │ (one server) │
Codex ───────┤                     │  127.0.0.1:9233)      │   │  127.0.0.1:9235)         │   └──────────────┘
VS Code ─────┘                     └───────────────────────┘   │                          │
(--stdio or unmet needs: sonarless-mcp mcp, one per session) ─▶└──────────────────────────┘
```

- **The gateway** (`sonarless-mcp serve`) is one Streamable HTTP endpoint,
  `http://127.0.0.1:9233/sonarless-mcp` (`SONARLESS_GATEWAY_PORT`), for every
  client, so no per-client process runs. The client sends its directory in an
  `X-Sonarless-Workspace` header, and the gateway keeps one session per
  workspace (forgotten after an hour unused).
- **The stdio bridge** (`sonarless-mcp mcp [workspace]`) does the same for one
  client session over stdin/stdout. Setup uses it for a client whose
  requirements for the gateway (below) are not met, and everywhere with
  `--stdio`.
- Both start (or resume) the containers on the first request, so nothing needs
  to run when a client connects. If Docker itself is down, the agent sees
  `sonarless_status` and a "not available" note instead of the SonarQube tools.
- Tokens are kept valid (regenerated under a lock, so clients never revoke each
  other's), and `initialize` / `tools/list` are answered from cache during a
  cold start so clients don't time out. A client connecting for the very first
  time while SonarQube still starts sees only the `sonarless_*` tools and is
  told (`notifications/tools/list_changed`) when the rest appear.
- Tools that take a `projectKey` default to the workspace's project (when that
  project exists on the server), and the agent is told which project it is.

### Gateway token and autostart

The gateway answers only requests that carry the token in
`<config dir>/mcp-gateway-token` (created by setup, readable only by you) and
refuses requests from web pages (`Origin` / `Host` checks). Client configs
reference the token instead of holding it:

| Client | Entry | Token from | Workspace from | Gateway started by |
|---|---|---|---|---|
| Claude Code | `type: http` | `headersHelper`: `sonarless-mcp ensure --headers` | the helper: Claude Code's working directory | that same helper, before each connection |
| opencode | `type: remote`, `oauth: false` (2.x under `mcp.servers`, 1.x under `mcp`) | `{file:<token file>}` | `{env:PWD}` | login autostart |
| Cursor | `url` + `headers` | `${env:SONARLESS_MCP_TOKEN}` | `${workspaceFolder}` | login autostart |
| VS Code | `type: http` in the user `mcp.json` | `${env:SONARLESS_MCP_TOKEN}` | `${workspaceFolder}` | login autostart |
| Codex | `url` + `bearer_token_env_var` | `SONARLESS_MCP_TOKEN` | `env_http_headers` → `PWD` | login autostart |

Cursor, VS Code and Codex can only read the token from the environment. Add the
line setup prints to your shell profile, open a new terminal and re-run setup:

```sh
export SONARLESS_MCP_TOKEN="$(cat ~/.config/sonarless-mcp/mcp-gateway-token)"   # Linux path; setup prints yours
```

Until a client's requirement is met (or if the autostart cannot be installed),
setup registers the stdio bridge for it and says why.

The login autostart is a systemd user unit
(`~/.config/systemd/user/sonarless-mcp-gateway.service`) on Linux, a
LaunchAgent (`~/Library/LaunchAgents/io.github.brainervirus.sonarless-mcp.gateway.plist`)
on macOS, and on Windows a per-user Run key (no admin) that starts
`%AppData%\sonarless-mcp\gateway-autostart.vbs` through `wscript`, so no
console window shows. Setup installs it when a chosen client needs it. Its log
is `<cache dir>/gateway.log`.

`sonarless-mcp setup --remove`, once no client uses the gateway any more,
removes the autostart and stops a running gateway (it records its pid in
`<cache dir>/gateway.pid` and stops that pid only if it still runs
`sonarless-mcp serve`). `sonarless-mcp stop --all` stops the gateway too (see
[Scanning and the server](scanning.md#stop-and-reset)).

## What agents get

Besides SonarQube's own tools, the `sonarqube` server offers sonarless-mcp's
features as tools, so any client can use them without knowing the CLI:

| Tool | Does |
|---|---|
| `sonarless_status` | containers and their memory, gateway sessions, running bridges and scans, remotes and whether they're reachable |
| `sonarless_project` | detected project key and name, their source, which servers have the project |
| `sonarless_scan` | scan the workspace locally and report the quality gate |
| `sonarless_remote_sync` | align local rules with a remote (CI); only when remotes exist |

Anything that needs a token (adding a remote, changing its token) stays a
terminal command: the tools tell the agent the exact command to give you, and a
token never passes through the chat. `SONARLESS_MCP_AGENT_TOOLS=false` hides
these tools.

Every SonarQube tool also takes an optional `server` argument: `local`
(default) or the name of a [remote](remotes.md).

## Agent skill

`sonarless-mcp setup` also installs a `sonarless-mcp` skill for the clients
with skills (Claude Code `~/.claude/skills`, Cursor `~/.cursor/skills`,
opencode `~/.config/opencode/skills`, Codex `~/.codex/skills`): a short guide
the agent loads when you ask about SonarQube or sonarless. It never overwrites
a skill of yours with the same name.

```sh
sonarless-mcp skill list            # each copy: dir, version, state, auto|manual
sonarless-mcp skill list --json
sonarless-mcp skill sync            # rewrite the copies in your home dir at this version now
sonarless-mcp skill sync --force    # also downgrade copies a newer sonarless-mcp wrote
sonarless-mcp skill install                      # into every detected client
sonarless-mcp skill install --dir .claude/skills # e.g. into a repository
sonarless-mcp skill uninstall --dir .claude/skills
sonarless-mcp skill print           # the SKILL.md this binary carries
```

Each copy is stamped with the version that wrote it. A copy is `current` (this
version), `stale` (older, or written before copies were stamped), `newer` (a
newer sonarless-mcp wrote it; this one leaves it alone) or `missing` (deleted,
or replaced by a skill sonarless-mcp did not write; `skill sync` forgets it and
never re-creates it).

Copies in your home directory update themselves (`auto`) the first time a newer
release runs. A copy inside a git repository is `manual`: it changes only when
you run `skill install --dir` there again. The list of copies is `skills.json`
in the config dir; the first run also adopts copies earlier versions
installed. `skill sync` exits 1 when a copy could not be rewritten.
