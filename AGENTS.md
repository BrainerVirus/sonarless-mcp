# AGENTS.md

Guidance for AI agents (and humans) working on sonarless-mcp.

## What this is

A single Go binary that runs local SonarQube for AI agents: one shared
SonarQube server and one shared SonarQube MCP server (Streamable HTTP) per
machine, a stdio shim (`sonarless-mcp mcp`) that every client launches, project
key detection, scans for any build system, an idle watcher, self-update, and a
setup wizard that registers the MCP server in AI clients. It grew out of
[gitricko/sonarless](https://github.com/gitricko/sonarless); keep that credit
in the README and LICENSE.

## Layout

| Path | Owns |
|---|---|
| `cmd/sonarless-mcp/` | CLI commands (cobra); `setup.go` wizard, `update.go` self-update |
| `internal/config/` | layered settings: defaults < user `config.env` < project `.sonarless.env` < env |
| `internal/docker/` | thin `docker` CLI wrapper (no SDK) |
| `internal/sonar/` | SonarQube API client, server container lifecycle, shared token, file locks |
| `internal/mcp/` | shared MCP containers + the stdio shim (proxy, cache, per-server routing and defaults) |
| `internal/project/` | project root + key detection from every config source |
| `internal/scan/` | cli / maven / gradle / dotnet scanners |
| `internal/idle/` | activity stamp, idle watcher, detached process spawning |
| `internal/update/` | release check, checksum-verified self-replace |
| `internal/remote/` | remote SonarQube servers (remotes.json + private token files) |
| `internal/clients/` | AI client detection and config editing |
| `internal/tui/` | the setup picker (bubbletea) |
| `install` | single entry script, valid sh and PowerShell; hands off to the platform installer |
| `install.sh`, `install.ps1` | platform installers (download, verify, install, then `setup`) |

## Develop

```sh
go build ./cmd/sonarless-mcp
go vet ./... && go test ./...
gofmt -l .                      # must print nothing
git config core.hooksPath .githooks   # once per clone: local commit-msg check
```

- Must build and pass tests on Linux, macOS and Windows (CI runs all three).
  Use `filepath`, never hard-coded `/`; guard Unix-only behavior with
  `runtime.GOOS` or `_windows.go` / `_unix.go` files.
- Unit tests never need Docker or the network; use temp dirs, `SONARLESS_HOME`,
  `httptest`, and injectable functions (see `clients.Env`, `update.APIBase`).
- Live checks against Docker must be isolated from a real install:
  `SONARLESS_HOME=<tmp> SONARLESS_INSTANCE=<name> SONARLESS_SERVER_PORT=<p> SONARLESS_MCP_PORT=<p>`,
  and remove only the containers, volumes and network of that instance afterwards.
- In `mcp` mode stdout is the MCP protocol: never print to stdout from code the
  shim can reach; progress goes to stderr.
- Anything several clients can do at once (create containers, refresh the
  token, update) takes a lock via `sonar.LockFile`.
- Changing user-visible behavior or settings: update `README.md` in the same change.

## Commits and releases

- Commits follow [Conventional Commits](https://www.conventionalcommits.org/)
  (`feat:`, `fix:`, `perf:`, `refactor:`, `docs:`, `test:`, `ci:`, `build:`,
  `chore:`); CI enforces it with commitlint.
- Releases are fully automated: every push to `main` runs CI, then
  semantic-release picks the version from the commits since the last tag
  (`feat` → minor, `fix`/`perf` → patch, `!` or `BREAKING CHANGE:` → major),
  updates `CHANGELOG.md`, tags `vX.Y.Z`, publishes the GitHub Release with
  generated notes, and GoReleaser attaches binaries for linux/darwin/windows ×
  amd64/arm64 plus `checksums.txt`.
- Never edit `CHANGELOG.md` by hand, create tags, or publish releases manually.
- Installed binaries update themselves from those releases once a day
  (`internal/update`); the archive names in `.goreleaser.yaml`
  (`sonarless-mcp_<os>_<arch>`) are a contract with the installers and the
  updater, so don't change them.
