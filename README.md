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
| MCP | — | one shared MCP container for all clients, reached directly over HTTP through one gateway (or a stdio bridge), project key filled in per workspace |
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
release, verifies its SHA-256 checksum (and, when [cosign](https://docs.sigstore.dev/cosign/system_config/installation/)
is installed, the checksums file's signature; see [Release signing](#release-signing)),
installs the binary (`~/.local/bin` or
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

If `~/.local/bin` isn't on your PATH yet, the installer adds it for every shell
it finds — `~/.profile`, `~/.bashrc` (and `~/.bash_profile` on macOS), zsh's
`.zshenv`, fish's `conf.d` — with one guarded, marked line each, so reruns
change nothing (`SONARLESS_MCP_NO_MODIFY_PATH=1` opts out). On Windows it goes
on your user PATH. In a terminal that was already open, the new command may not be recognized
(or highlighted) yet: in zsh run `rehash`, in bash `hash -r`; fish notices on
its own; Warp reads PATH only when a tab starts, so open a new tab there. The
installer detects your shell and terminal and prints the one that applies.

Installed clients are preselected; untick any you don't want, or clear them
all. Run it again any time with `sonarless-mcp setup` (`--remove` to
unregister, `--yes` / `--clients claude,cursor` for scripts). Installer options:
`SONARLESS_MCP_VERSION=v1.2.3` pins a release, `SONARLESS_MCP_INSTALL_DIR`
changes the target, `SONARLESS_MCP_NO_SETUP=1` skips the picker.

Or with Go: `go install github.com/BrainerVirus/sonarless-mcp/cmd/sonarless-mcp@latest`
(a development build: it never upgrades itself in the background; `sonarless-mcp upgrade --version <tag>`
replaces it with a signed release).

Requires Docker (Docker Desktop on macOS/Windows).

## Upgrading

```sh
sonarless-mcp upgrade                    # install the latest release now (alias: update)
sonarless-mcp upgrade --check            # print installed and latest; change nothing
sonarless-mcp upgrade --version v0.9.0   # install that release (also an older one)
sonarless-mcp version                    # version, commit, build date, Go, OS/CPU (--json)
```

`upgrade` downloads the release for your OS and CPU, checks its signature and
checksum (below), runs the new binary's `version` as a smoke test and only then
replaces the running binary atomically, so a failed check leaves the installed
one untouched. It also pulls the latest SonarQube MCP image, which the shared
container picks up the next time it starts from stopped (never mid-session),
and refreshes the agent skill in your clients. Restart your AI clients to use
the new binary.

Installed binaries also keep themselves current: once a day, the gateway,
starting the MCP bridge, `start` or `scan` check GitHub Releases in the
background and install a newer release once it is **a day old**, so a broken
release can be pulled before it reaches you. The same run pulls the MCP image.
`SONARLESS_AUTO_UPDATE=false` turns the background run off; `upgrade` still
works. Log: `<cache dir>/sonarless-mcp/update.log`.

In an interactive terminal, sonarless-mcp prints one line on stderr when a
newer release exists (at most once a day), and one when a
background upgrade has installed one:

```
A new release of sonarless-mcp is available: 0.9.0 → 0.10.0; run `sonarless-mcp upgrade` (https://github.com/BrainerVirus/sonarless-mcp/releases/tag/v0.10.0)
```

It only shows when both stdout and stderr are a terminal, never for `mcp`,
`serve`, `upgrade` or `--json` output, so agents, MCP clients and scripts never
see it. `SONARLESS_UPDATE_NOTICE=false` turns it off.

### Release signing

Every release's `checksums.txt` carries a [Sigstore](https://www.sigstore.dev/)
keyless signature (`checksums.txt.sigstore.json`) made by this repository's
release workflow on GitHub Actions. `upgrade` (and the background run) verify
it against that identity before trusting the checksums, and refuse a release
whose signature is missing or made by anyone else. Sigstore's trusted root comes
from its TUF repository, anchored on the TUF root built into the binary and
cached in `<cache dir>/sonarless-mcp/sigstore-tuf`. To check a download by hand:

```sh
cosign verify-blob \
  --bundle checksums.txt.sigstore.json \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  --certificate-identity-regexp '^https://github\.com/BrainerVirus/sonarless-mcp/\.github/workflows/release\.yml@refs/heads/main$' \
  checksums.txt
sha256sum --ignore-missing -c checksums.txt
```

Releases up to v0.8.1 were published before signing existed. To go back to one
of them, name it and opt in: `sonarless-mcp upgrade --version v0.8.1 --allow-unsigned`
installs it on its checksum alone. The flag only accepts a release older than
the installed one; a newer release without a valid signature is always
refused. The install scripts do the same check when `cosign` is on your PATH:
they first resolve "latest" to its tag through the GitHub API, and accept a
missing signature only when that release (latest or a pinned
`SONARLESS_MCP_VERSION`) is v0.8.1 or older. Without cosign they check the
SHA-256 only.

## Use with AI agents

`sonarless-mcp setup` registers a `sonarqube` MCP server in each client
(Claude Code via `claude mcp add-json`, Cursor `~/.cursor/mcp.json`, opencode
`opencode.json`, VS Code user `mcp.json`, Codex `config.toml`), editing only
that entry. If a client already has a different `sonarqube` server (say, your
company's remote SonarQube), it is left untouched and sonarless-mcp registers as
`sonarqube-local` instead. To do it by hand, `sonarless-mcp mcp-config <client>` prints the
snippet.

How it works:

```
Claude Code ─┐  direct HTTP        ┌───────────────────────┐   ┌──────────────────────────┐   ┌──────────────┐
Cursor ──────┼────────────────────▶│ sonarless-mcp gateway │──▶│ sonarless-mcp container  ├──▶│ SonarQube    │
opencode ────┤                     │ (one per machine,     │   │ (SonarQube MCP, HTTP,    │   │ (one server) │
Codex ───────┤                     │  127.0.0.1:9233)      │   │  127.0.0.1:9235)         │   └──────────────┘
VS Code ─────┘                     └───────────────────────┘   │                          │
(--stdio or unmet needs: sonarless-mcp mcp, one per session) ─▶└──────────────────────────┘
```

- Clients that speak Streamable HTTP connect to one gateway (`sonarless-mcp
  serve`, ~10 MB) instead of each launching a process. The gateway does what
  the stdio bridge does, per workspace: the client sends its directory in an
  `X-Sonarless-Workspace` header, so `projectKey` defaults and the `sonarless_*`
  tools work as before.
- The gateway only answers with the token in `<config dir>/mcp-gateway-token`
  (created by setup, readable only by you), and refuses requests from web pages
  (`Origin`/`Host` checks). Client configs reference the token instead of
  holding it:

  | Client | Entry | Token from | Workspace from | Gateway started by |
  |---|---|---|---|---|
  | Claude Code | `type: http` | `headersHelper`: `sonarless-mcp ensure --headers` | the helper: Claude Code's working directory (not `${PWD}`, which is stale when Claude starts from a GUI) | that same helper, before each connection |
  | opencode | `type: remote`, `oauth: false` (2.x under `mcp.servers`, 1.x under `mcp`) | `{file:<token file>}` | `{env:PWD}` | login autostart |
  | Cursor | `url` + `headers` | `${env:SONARLESS_MCP_TOKEN}` | `${workspaceFolder}` | login autostart |
  | VS Code | `type: http` in the user `mcp.json` | `${env:SONARLESS_MCP_TOKEN}` | `${workspaceFolder}` | login autostart |
  | Codex | `url` + `bearer_token_env_var` | `SONARLESS_MCP_TOKEN` | `env_http_headers` → `PWD` | login autostart |

  `setup --remove`, once no client uses the gateway any more, removes the
  autostart and stops a running gateway (it records its pid in the cache dir;
  the pid is only stopped if it still runs `sonarless-mcp serve`).
  Login autostart is a systemd user unit (`~/.config/systemd/user/sonarless-mcp-gateway.service`)
  on Linux, a LaunchAgent on macOS, and on Windows a per-user Run key (no admin)
  that starts `%AppData%\sonarless-mcp\gateway-autostart.vbs` through `wscript`,
  so no console window shows. Setup installs it when a chosen client needs it.
  Cursor, VS Code and Codex can only read the token from the environment:
  add `export SONARLESS_MCP_TOKEN="$(cat <token file>)"` to your shell profile
  (setup prints the exact line), open a new terminal and re-run setup. Until a
  client's requirement is met (or if the autostart can't be installed), setup
  registers the stdio bridge for it and says why.
  `setup --stdio` uses the bridge everywhere.
- Containers start (or resume) on the first request, through the gateway or
  the bridge: nothing needs to be running when a client connects. If Docker
  itself is down, the agent sees `sonarless_status` and a "not available"
  note instead of the SonarQube tools.
- Tokens are kept valid (regenerated under a lock, so clients never revoke each
  other's), and `initialize`/`tools/list` are answered from cache during a cold
  start so clients don't time out. A client connecting for the very first time
  while SonarQube is still starting sees only the `sonarless_*` tools and is
  told (`notifications/tools/list_changed`) when the rest appear: on stdout by
  the bridge, on the gateway's GET event stream for direct clients. A direct
  client that never opens that stream picks them up on its next connection.
- Tools that take a `projectKey` default to the workspace's project (only when
  that project exists on the server), and the agent is told which project it is.
- After `SONARLESS_IDLE_TIMEOUT` (default 30m) without MCP calls (gateway or
  bridge), scans or web UI traffic, a background watcher stops both containers.
  The next request resumes them, history intact. The gateway itself keeps
  running; it restarts itself when the binary updates.

## What agents get

Besides SonarQube's own tools, the `sonarqube` MCP server offers sonarless-mcp's
features as tools, so any client can use them without knowing the CLI:

| Tool | Does |
|---|---|
| `sonarless_status` | containers and their memory, gateway sessions, running bridges and scans, remotes and whether they're reachable |
| `sonarless_project` | detected project key/name, its source, which servers have it |
| `sonarless_scan` | scan the workspace locally and report the quality gate |
| `sonarless_remote_sync` | align local rules with a remote (CI) — when remotes exist |

Anything that needs a token (adding a remote, changing its token) stays a
terminal command: the tools tell the agent the exact command to give you, and a
token never passes through the chat. `SONARLESS_MCP_AGENT_TOOLS=false` hides them.

`sonarless-mcp setup` also installs a **sonarless-mcp skill** for clients with
skills (Claude Code, Cursor, opencode, Codex) — a short guide the agent loads
when you ask about SonarQube or sonarless, kept current by updates and never
written over a skill of yours with the same name.

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
sonarless-mcp remote sync work   # make local scans judge this project like CI (gate, rule sets, new code)
sonarless-mcp remote remove work
```

Use a **User token** (`squ_…`, *My Account → Security*): analysis tokens
(`sqa_`/`sqp_`) can only submit scans, not read results, so they're refused.

Every MCP tool then takes an optional `server` argument: `local` (default — your
working copy, scanned in real time) or a remote's name (what CI found, e.g. on
`develop`). The workspace's project key and the remote's branch are filled in.

### Judge local scans like CI

`sonarless-mcp remote sync work` (run in a project) copies the remote's quality
gate, the quality profiles (rule sets) the project uses there and its new-code
definition to your local server, and applies them to the project — so a local
`scan` fails on the same conditions CI would (e.g. 80% coverage, 3% duplication)
before you push. Copies are named `work: <name>`; nothing built-in is changed,
and built-in profiles that are identical on both servers (same SonarQube
version) are left as is. Run it again to pick up changes on the remote.

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
sonarless-mcp status [--json] # containers and their memory, gateway, bridges, running scans
sonarless-mcp start           # start the SonarQube server
sonarless-mcp stop [--all]    # stop the server and every MCP container; --all: the gateway too
sonarless-mcp reset --yes     # wipe this version's history and start fresh
```

`status` lists the server, local MCP and remote MCP containers (state, image,
memory in use), the gateway (pid, port, uptime, and each workspace session with
its last request), the stdio bridges (`sonarless-mcp mcp`) still running and the
scans in progress, CLI or agent-started (`waiting` for the server to start, or
`scanning`). `--json` prints the same as JSON.

`stop` stops the server, the local MCP container and every remote MCP
container of this instance, including one left from a removed remote (found by
its `sonarless-mcp.instance` label, never by name, so another
`SONARLESS_INSTANCE` is untouched). It leaves the gateway running: it holds no
container memory and connected clients expect it to answer. `stop --all` stops
it too, through its login service where setup installed one for this config
(systemd stops it until the next login, launchd until the next login or
`setup`); the next client that needs it starts it again. The login service is
one per user: if it was installed with another `SONARLESS_HOME`,
`SONARLESS_INSTANCE` or XDG dir, `stop --all` leaves it running and says so.

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
| `SONARLESS_GATEWAY_PORT` | `9233` | gateway that HTTP clients connect to (127.0.0.1); re-run `setup` after changing it |
| `SONARLESS_MCP_TOOLSETS` / `SONARLESS_MCP_READ_ONLY` | server defaults / `false` | passed to the MCP server |
| `SONARLESS_MCP_JAVA_OPTS` | `-Xmx64m -XX:+UseSerialGC -XX:TieredStopAtLevel=1 -XX:ReservedCodeCacheSize=32m -XX:MaxMetaspaceSize=128m -Xss512k` | JVM options of the shared MCP server (`JAVA_TOOL_OPTIONS`); about 200 MiB instead of ~300 with the same tool latency; `none` keeps the JVM defaults |
| `SONARLESS_IDLE_TIMEOUT` | `30m` | `0` keeps everything running |
| `SONARLESS_WEB_HEAP` / `SONARLESS_CE_HEAP` / `SONARLESS_SEARCH_HEAP` | `256m` / `384m` / `256m` | Java heaps of SonarQube's web server, compute engine and Elasticsearch (SonarQube's own: 512m each); raise `SONARLESS_CE_HEAP` if a big project's report fails with OutOfMemoryError. Changing one recreates the server container; its data is kept |
| `SONARLESS_INSTANCE` | `sonarless` | names the containers (`-server`, `-mcp`), network and volumes |
| `SONARLESS_AUTO_UPDATE` | `true` | daily background upgrade to a signed release once it is a day old (binary + MCP image) |
| `SONARLESS_UPDATE_NOTICE` | `true` | in a terminal, a one-line stderr notice when a newer release exists |
| `SONARLESS_MCP_AGENT_TOOLS` | `true` | expose the `sonarless_*` tools to agents |
| `SONARLESS_PLUGINS_DIR` | `<config dir>/plugins` | extra plugin jars (a `shellcheck` binary there is mounted into CLI scans) |
| `SONARLESS_PROJECT_KEY` / `SONARLESS_PROJECT_NAME` | detected | per project |
| `SONARLESS_SCANNER` / `SONARLESS_SCAN_TESTS` | auto / `false` | per project |
| `SONARLESS_SOURCES` / `SONARLESS_SCANNER_IMAGE` / `SONARLESS_METRICS_FILE` | `.` / `sonarsource/sonar-scanner-cli:12.4` / `sonar-metrics.json` | per project |

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
