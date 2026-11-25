# Troubleshooting

Start with:

```sh
sonarless-mcp status      # what runs, how much memory it takes, when it stops itself
sonarless-mcp project     # which project key this directory resolves to
sonarless-mcp config      # effective settings and where each came from
sonarless-mcp setup --list
sonarless-mcp version
```

Exit status is 0 on success and 1 on any error. A usage error names the flag
you were probably after: `scan --tets` answers "did you mean --tests?", and
`scan --tests=12` says that `--tests` takes no value. `sonarless-mcp <command> --help`
lists the rest.

## Install and PATH

- **`sonarless-mcp: command not found` right after install.** Run `rehash`
  (zsh) or `hash -r` (bash), or open a new tab (Warp reads PATH only when a tab
  starts). New terminals pick it up from the shell startup files the installer
  edited ([Install](install.md#path)).
- **Docker not found or not running.** sonarless-mcp needs Docker (Docker
  Desktop on macOS and Windows). With Docker down, agents see only
  `sonarless_status` and a "not available" note.

## AI clients

- **A client does not show the `sonarqube` tools.** Restart it after `setup`.
  The first call after a cold start can take a minute while SonarQube starts;
  the tools appear once it answers.
- **Setup kept the stdio bridge for Cursor, VS Code or Codex.** Those clients
  read the gateway token from `SONARLESS_MCP_TOKEN`; export it in your shell
  profile (setup prints the line), open a new terminal and re-run setup
  ([AI clients](clients.md#gateway-token-and-autostart)).
- **Tools require `projectKey` and the `sonarless_project` / `sonarless_scan`
  tools are missing.** The client did not send its workspace. Run
  `sonarless-mcp project` in the workspace and pass the key explicitly.
- **Gateway problems.** Its log is `<cache dir>/gateway.log`;
  `sonarless-mcp ensure` starts it if it is not running.
- **Your own `sonarqube` server entry.** Setup leaves it alone and registers as
  `sonarqube-local`.

## Server and scans

- **The first call is slow.** SonarQube is starting: it stops itself after
  `SONARLESS_IDLE_TIMEOUT` (default 30m) unused and resumes on demand.
- **A big project's report fails with OutOfMemoryError.** Raise
  `SONARLESS_CE_HEAP` in `config.env` ([Configuration](configuration.md#settings)).
- **The wrong project key.** `sonarless-mcp project` shows where it came from;
  set `SONARLESS_PROJECT_KEY` in the project's `.sonarless.env` to override it.
- **No coverage in build-based scans.** Pass `--tests` or set
  `SONARLESS_SCAN_TESTS=true`.
- **Free the memory now.** `sonarless-mcp stop` (containers) or
  `sonarless-mcp stop --all` (the gateway too).
- **Start over.** `sonarless-mcp reset --yes` wipes the current SonarQube
  version's history.

## Remotes

- **"unreachable".** You are probably off the VPN or network; the local server
  keeps working.
- **Token rejected.** `sonarless-mcp remote update <name> --token`. Use a User
  token (`squ_…`), not an analysis token.
- **A new remote does not show up.** The next tool call already accepts it; if
  the client still lists the old `server` choices, reconnect the MCP server
  ([Remotes](remotes.md#asking-about-a-remote)).

## Upgrades and the skill

- **`upgrade` refused a release** (signature or checksum check failed). The
  installed binary is unchanged; do not work around it. `--allow-unsigned` is
  only for deliberately going back to a release from before signing (v0.8.1 or
  older); see [Upgrading](upgrading.md#release-signing).
- **Background upgrades.** The log is `<cache dir>/update.log`;
  `SONARLESS_AUTO_UPDATE=false` turns them off.
- **The agent skill looks outdated.** `sonarless-mcp skill list` shows each
  copy's version and state; `sonarless-mcp skill sync` updates the copies in
  your home dir, and `sonarless-mcp skill install --dir <dir>` a copy in a
  repository.
