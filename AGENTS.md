# AGENTS.md

Guidance for AI agents (and humans) working on sonarless-mcp.

## What this is

A single Go binary that runs local SonarQube for AI agents: one shared
SonarQube server and one shared SonarQube MCP server (Streamable HTTP) per
machine, an HTTP gateway (`sonarless-mcp serve`) most clients connect to and a
stdio bridge (`sonarless-mcp mcp`) for the rest, project key detection, scans
for any build system, remote SonarQube servers, an idle watcher, signed
self-update, an agent skill, and a setup wizard that registers the MCP server
in AI clients. It grew out of
[gitricko/sonarless](https://github.com/gitricko/sonarless); keep that credit
in the README and LICENSE.

## Layout

| Path | Owns |
|---|---|
| `cmd/sonarless-mcp/` | CLI commands (cobra; `newRootCommand` in `main.go` builds the tree); `setup.go` wizard, `gateway.go` `serve`/`ensure`, `status.go` `status`/`stop`, `remote.go`, `skill.go`, `upgrade.go` self-upgrade (`update` is an alias), `version.go`, `notice.go` the terminal update notice, `hints.go` flag typo hints, `agenttools.go` the `sonarless_*` MCP tools |
| `internal/config/` | layered settings: defaults < user `config.env` < project `.sonarless.env` < env |
| `internal/docker/` | thin `docker` CLI wrapper (no SDK) |
| `internal/sonar/` | SonarQube API client, server container lifecycle, shared token, file locks |
| `internal/mcp/` | shared MCP containers, the stdio shim (proxy, cache, per-server routing and defaults) and the HTTP gateway that serves the same shim per workspace to direct clients |
| `internal/project/` | project root + key detection from every config source |
| `internal/scan/` | cli / maven / gradle / dotnet scanners |
| `internal/idle/` | activity stamp, idle watcher, detached process spawning |
| `internal/update/` | release check, Sigstore- and checksum-verified, smoke-tested atomic self-replace, the one-day soak of background upgrades, the upgrade state file; `updatetest/` a fake release server for tests |
| `internal/remote/` | remote SonarQube servers (remotes.json + private token files) |
| `internal/clients/` | AI client detection, config editing (direct HTTP entry or stdio shim per client), gateway login autostart (systemd, launchd, Windows Run key), the embedded agent skill (`skill/SKILL.md`) |
| `internal/tui/` | the setup picker (bubbletea) |
| `docs/` | user docs, one page per topic; `README.md` stays the fast path and links them |
| `bench/` | the weak-model skill bench (`go run ./bench`, never in CI): a fake sonarless-mcp CLI and MCP server, cases and stored results |
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
- Gateway tests use `httptest` and a fake `docker` script on PATH; never run
  `setup` against your real client configs or start a gateway on the default
  port while testing (set `SONARLESS_GATEWAY_PORT` and `SONARLESS_HOME`).
- In `mcp` mode stdout is the MCP protocol: never print to stdout from code the
  shim can reach; progress goes to stderr.
- Anything several clients can do at once (create containers, refresh the
  token, update) takes a lock via `sonar.LockFile`.
- Changing user-visible behavior or settings: update the `docs/` page (and
  `README.md` if the fast path changes) in the same change, and
  `internal/clients/skill/SKILL.md` if agents should know about it. A new
  `SONARLESS_*` variable goes in `config.Keys` (a setting) or `config.OtherEnv`
  (environment only), so `sonarless-mcp --help` lists it.
- A change to `internal/clients/skill/SKILL.md` is measured with the skill
  bench before and after: `go run ./bench` (Haiku by default; it runs
  `claude -p` on your Claude Code login and spends subscription usage), then
  `go run ./bench compare bench/results/baseline/claude-haiku-4-5 bench/results/claude-haiku-4-5`.
  The bench's sonarless-mcp is a fake on `PATH` that never calls docker or the
  network; see [bench/README.md](bench/README.md). Keep the skill accurate:
  trim it, never teach a command the CLI does not have.
- `cmd/sonarless-mcp/docs_test.go` parses every `sonarless-mcp …` command line
  in code spans and fenced blocks of `README.md`, `AGENTS.md`, `docs/` and the
  skill against the real command tree, and fails with `file:line` on an unknown
  command or flag. Write a deliberately wrong command line in plain text, not
  in code.

## Commits and releases

- Commits follow [Conventional Commits](https://www.conventionalcommits.org/)
  (`feat:`, `fix:`, `perf:`, `refactor:`, `docs:`, `test:`, `ci:`, `build:`,
  `chore:`); CI enforces it with commitlint.
- Releases are fully automated: every push to `main` runs CI, then
  semantic-release picks the version from the commits since the last tag
  (`feat` → minor, `fix`/`perf` → patch, `!` or `BREAKING CHANGE:` → major),
  updates `CHANGELOG.md`, tags `vX.Y.Z`, publishes the GitHub Release with
  generated notes, and GoReleaser attaches binaries for linux/darwin/windows ×
  amd64/arm64 plus `checksums.txt` and its cosign keyless signature
  `checksums.txt.sigstore.json`.
- The signature's identity is `release.yml` on `refs/heads/main` (semantic-release
  runs on the push to main). `internal/update.ReleaseIdentity` and the install
  scripts pin it: renaming the workflow, moving releases to a tag-triggered
  workflow or signing anything else changes it, and then every installed binary
  refuses the new release. Change all three together, and only with a release
  path for binaries already out there.
- Never edit `CHANGELOG.md` by hand, create tags, or publish releases manually.
- Installed binaries upgrade themselves from those releases (`internal/update`):
  checked daily, installed once a release is a day old. The archive names in
  `.goreleaser.yaml` (`sonarless-mcp_<os>_<arch>`), `checksums.txt` and the
  `version` subcommand (the upgrade smoke test, with `--version` as fallback)
  are a contract with the installers and the upgrader, so don't change them.
- Upgrade tests run against `internal/update/updatetest`'s fake release server
  and a test Sigstore root; never let a test replace a real binary or reach
  GitHub.
- Exit statuses are a contract with CI and scripts: 0 success, 1 any error, 2
  a failing quality gate under `sonarless-mcp scan --fail-on-gate`
  (`cmd/sonarless-mcp/scan.go`, listed in the root `--help` and
  `docs/scanning.md`). Add a new one there, never reuse a number.

## Security

Vulnerabilities are reported privately through GitHub security advisories, as
[SECURITY.md](SECURITY.md) describes; never discuss one in a public issue or
PR before its fix is released.
