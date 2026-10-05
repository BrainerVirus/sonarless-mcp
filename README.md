# sonarless-mcp

Local SonarQube for any project, built for AI agents: **one** shared SonarQube
server and **one** shared [SonarQube MCP server](https://github.com/SonarSource/sonarqube-mcp-server)
per machine, used by every client — Claude Code, Cursor, opencode, VS Code,
Codex — at once. Single binary for Linux, macOS and Windows.

> **Standing on the shoulders of [gitricko/sonarless](https://github.com/gitricko/sonarless).**
> sonarless had the great idea: SonarQube without a hosted server, just Docker
> and one command. sonarless-mcp is a Go rewrite of that idea, reshaped around
> agents using SonarQube through MCP. If you just want scans in a shell or a
> GitHub Action, use the original — and give it a ⭐.

## What's different from sonarless

| | sonarless | sonarless-mcp |
|---|---|---|
| Runs on | bash + jq + curl (Linux/macOS) | single Go binary (Linux/macOS/Windows) |
| MCP | — | `sonarless-mcp mcp`: one shared MCP container for all clients, project key filled in per workspace |
| Project key | directory name | `sonar-project.properties`, `.sonarcloud.properties`, Maven, Gradle (Groovy/Kotlin), `pyproject.toml`, .NET, CI pipelines, `package.json` |
| Scanner | sonar-scanner CLI | CLI, Maven, Gradle or dotnet-sonarscanner, picked per project |
| Resources | server runs until stopped | stops itself after 30 min unused, resumes on next use |
| Server version | fixed in script | `SONARLESS_SONARQUBE_VERSION` in a config file (e.g. to match your CI) |
| Scan history | lost when the container is recreated | kept in per-version named volumes |

## Install

One installer for Linux, macOS and Windows — tell your AI agent:

```text
Install sonarless-mcp: https://raw.githubusercontent.com/BrainerVirus/sonarless-mcp/main/install
```

or run it yourself — same script, copy the line for your shell:

**Linux / macOS**

```sh
curl -fsSL https://raw.githubusercontent.com/BrainerVirus/sonarless-mcp/main/install | sh
```

**Windows (PowerShell)**

```powershell
irm https://raw.githubusercontent.com/BrainerVirus/sonarless-mcp/main/install | iex
```

`install` is a single file that is valid in both `sh` and PowerShell; it hands
off to the platform installer, which detects your CPU, downloads the matching
release, verifies its SHA-256 checksum, installs the binary (`~/.local/bin` or
`%LOCALAPPDATA%\Programs\sonarless-mcp`, added to your PATH), then opens the
client picker (run by an agent, it lists the detected clients and the command
to register them instead):

```
Register the SonarQube MCP server in:

  Select all available
  Clear all

› [x] Claude Code · detected
  [x] Cursor · already configured
  [ ] VS Code · not installed
  [x] opencode · detected
  [ ] Codex · not installed

↑/↓ move · space toggle · a all · n none · enter confirm · esc cancel
```

If `~/.local/bin` isn't on your PATH yet, the installer adds it to your shell's
startup file (zsh, bash or fish) once. A shell that was already open may need
`rehash` (zsh) or `hash -r` (bash) — or a new terminal — to see the new
command; the installer prints the exact one.

Installed clients are preselected; untick any you don't want, or clear them
all. Run it again any time with `sonarless-mcp setup` (`--remove` to
unregister, `--yes` / `--clients claude,cursor` for scripts). Installer options:
`SONARLESS_MCP_VERSION=v1.2.3` pins a release, `SONARLESS_MCP_INSTALL_DIR`
changes the target, `SONARLESS_MCP_NO_SETUP=1` skips the picker.

Or with Go: `go install github.com/BrainerVirus/sonarless-mcp/cmd/sonarless-mcp@latest`.

Requires Docker (Docker Desktop on macOS/Windows).

## Updating

Installed binaries keep themselves current: once a day, starting the MCP shim,
`start` or `scan` checks GitHub Releases in the background, downloads a newer
release, verifies its checksum, smoke-tests it and swaps it in, so the next
start runs it. The same check pulls the latest SonarQube MCP image, which the
shared container picks up the next time it starts from stopped (never mid-session).
Run `sonarless-mcp update` to do it now; `SONARLESS_AUTO_UPDATE=false` turns the
daily check off. Log: `<cache dir>/sonarless-mcp/update.log`.

## Use with AI agents

`sonarless-mcp setup` registers a `sonarqube` MCP server in each client
(Claude Code via `claude mcp add`, Cursor `~/.cursor/mcp.json`, opencode
`opencode.json`, VS Code user `mcp.json`, Codex `config.toml`), editing only
that entry. If a client already has a different `sonarqube` server (say, your
company's remote SonarQube), it is left untouched and sonarless-mcp registers as
`sonarqube-local` instead. To do it by hand, `sonarless-mcp mcp-config <client>` prints the
snippet.

How it works:

```
Claude Code ─┐                      ┌──────────────────────────┐   ┌──────────────┐
Cursor ──────┼─ sonarless-mcp mcp ──▶ sonarless-mcp container  ├──▶│ SonarQube    │
opencode ────┘  (tiny stdio shim,   │ (SonarQube MCP, HTTP,    │   │ (one server) │
                 one per client)    │  127.0.0.1:9235)         │   └──────────────┘
                                    └──────────────────────────┘
```

- Each client runs a few-MB shim instead of its own Java MCP container.
- The shim starts or resumes the shared containers, keeps one token valid
  (regenerated under a lock, so clients never revoke each other's), and
  answers `initialize`/`tools/list` from cache during a cold start so clients
  don't time out.
- Tools that take a `projectKey` default to the workspace's project (only when
  that project exists on the server), and the agent is told which project it is.
- After `SONARLESS_IDLE_TIMEOUT` (default 30m) without MCP calls, scans or web
  UI traffic, a background watcher stops both containers. The next request
  resumes them, history intact.

## Remote SonarQube servers

Next to the local server you can add any number of remote ones — typically your
company's CI server — and ask the agent about either:

```sh
sonarless-mcp remote add work --url http://sonar.internal:9000 --branch develop   # token: hidden prompt
sonarless-mcp remote update work --token                # new token (hidden prompt); keeps url/branch
sonarless-mcp remote update work --url http://10.0.0.5:9000   # server moved; keeps token
sonarless-mcp remote update work --branch main                # CI now analyzes main; keeps url/token
sonarless-mcp remote list        # name, reachable/unreachable, url, default branch
sonarless-mcp remote import work --branch develop   # reuse a SonarQube MCP entry another client already has
sonarless-mcp remote remove work
```

Every MCP tool then takes an optional `server` argument: `local` (default — your
working copy, scanned in real time) or a remote's name (what CI found, e.g. on
`develop`). The workspace's project key and the remote's branch are filled in.

Remotes are built to stay out of the way:

- Nothing touches a remote unless a tool call asks for it: startup, `initialize`
  and the tool list only use the local server.
- Before a remote call, a 3-second reachability check (cached 30s) runs; if the
  server is down or you're off the VPN, the agent gets a clear tool error and
  keeps using the local server. Reconnect and the next call works.
- A remote's MCP container only starts on its first successful use and stops
  with the rest when idle, so configured-but-unused remotes cost nothing.
- Tokens are stored per remote in a private file (`remotes/<name>.token`, 0600),
  never in `remotes.json`; a remote can be added while offline and is verified
  on first use.

## Scan from the command line

```sh
sonarless-mcp scan            # detect, start server, scan, wait for the quality gate
sonarless-mcp results         # key metrics, saved to sonar-metrics.json
sonarless-mcp project         # what was detected and where it came from
sonarless-mcp status | start | stop
sonarless-mcp reset --yes     # wipe this version's history and start fresh
```

Scanners: Maven projects use `./mvnw` → `mvn` → a Maven container; Gradle
projects use `./gradlew` → `gradle` → a Gradle container (the SonarQube plugin is
applied by an init script if the build doesn't declare it); .NET uses
`dotnet-sonarscanner`; everything else uses the sonar-scanner CLI container.
Build-based scans skip tests unless `--tests` (needed for coverage).

## Project key detection

First match wins:

1. `SONARLESS_PROJECT_KEY` (env or `.sonarless.env`)
2. `sonar-project.properties`, then `.sonarcloud.properties`
3. explicit build config: `pom.xml` `<properties>`, Gradle `sonar { properties { ... } }` / `gradle.properties`, `package.json` `"sonar"`
4. `pyproject.toml` `[tool.sonar]`
5. .NET `SonarQube.Analysis.xml` / `Directory.Build.props`
6. CI pipelines (GitHub Actions, GitLab CI, Jenkinsfile, Azure Pipelines, Bitbucket, CircleCI): `-Dsonar.projectKey=…`, `/k:…`, `projectKey:` — so local keys match CI
7. build defaults: Maven `groupId:artifactId`, Gradle `group:rootProject.name`, `.sln` name, `package.json` name
8. the directory name

The project root is the topmost Maven/Gradle/Sonar root under the git root, so
running from inside a module scans the whole build.

## Configuration

`sonarless-mcp config` prints every setting and where it came from. Layers, lowest first:

1. built-in defaults
2. user file: `~/.config/sonarless-mcp/config.env` (Linux), `~/Library/Application Support/sonarless-mcp/config.env` (macOS), `%AppData%\sonarless-mcp\config.env` (Windows)
3. project file `.sonarless.env` (scan settings only — the server is shared)
4. environment variables

```sh
# config.env — match the SonarQube version your CI uses
SONARLESS_SONARQUBE_VERSION=26.6.0.123539-community
SONARLESS_IDLE_TIMEOUT=30m
```

| Setting | Default | |
|---|---|---|
| `SONARLESS_SONARQUBE_VERSION` | `26.6.0.123539-community` | `sonarqube` image tag |
| `SONARLESS_SERVER_PORT` | `9234` | web UI/API port (127.0.0.1) |
| `SONARLESS_ADMIN_USER` / `SONARLESS_ADMIN_PASS` | `admin` / `admin` | local admin |
| `SONARLESS_MCP_PORT` / `SONARLESS_MCP_IMAGE` | `9235` / `sonarsource/sonarqube-mcp:latest` | shared MCP server |
| `SONARLESS_MCP_TOOLSETS` / `SONARLESS_MCP_READ_ONLY` | server defaults / `false` | passed to the MCP server |
| `SONARLESS_IDLE_TIMEOUT` | `30m` | `0` keeps everything running |
| `SONARLESS_INSTANCE` | `sonarless` | names the containers (`-server`, `-mcp`), network and volumes |
| `SONARLESS_AUTO_UPDATE` | `true` | daily background update check (binary + MCP image) |
| `SONARLESS_PLUGINS_DIR` | `<config dir>/plugins` | extra plugin jars (a `shellcheck` binary there is mounted into CLI scans) |
| `SONARLESS_PROJECT_KEY` / `SONARLESS_PROJECT_NAME` | detected | per project |
| `SONARLESS_SCANNER` / `SONARLESS_SCAN_TESTS` | auto / `false` | per project |
| `SONARLESS_SOURCES` / `SONARLESS_SCANNER_IMAGE` / `SONARLESS_METRICS_FILE` | `.` / `sonarsource/sonar-scanner-cli:12.1` / `sonar-metrics.json` | per project |

### About SonarQube versions

SonarQube's embedded database can't be upgraded across versions, so each
version keeps its own data volumes: a new version starts with fresh history,
and switching back restores the old one. `sonarless-mcp reset --yes` wipes the
current version's history.

## Contributing

Conventional Commits drive fully automated releases (semantic-release +
GoReleaser); see [AGENTS.md](AGENTS.md) for layout, testing rules and the
release flow.

## Credits

- [gitricko/sonarless](https://github.com/gitricko/sonarless) — the original
  idea and CLI this project grew from (MIT).
- [SonarSource/sonarqube-mcp-server](https://github.com/SonarSource/sonarqube-mcp-server) — the MCP server this shares.

MIT licensed; see [LICENSE](LICENSE).
