# Configuration

```sh
sonarless-mcp config          # every setting, its value and where it came from
sonarless-mcp --help          # every SONARLESS_* variable with its default
```

Settings are layered, lowest first:

1. built-in defaults;
2. the user file `config.env` in the config dir ([below](#files));
3. the project file `.sonarless.env` in the project root, for the per-project
   settings only (the server is shared by every project; a machine-wide key
   there is ignored with a warning);
4. environment variables.

`config.env` and `.sonarless.env` hold `KEY=value` lines:

```sh
# config.env: match the SonarQube version your CI uses
SONARLESS_SONARQUBE_VERSION=26.6.0.123539-community
SONARLESS_IDLE_TIMEOUT=30m
```

An unknown key in either file is ignored with a warning.

## Settings

| Setting | Default | |
|---|---|---|
| `SONARLESS_SONARQUBE_VERSION` | `26.6.0.123539-community` | `sonarqube` image tag |
| `SONARLESS_SERVER_PORT` | `9234` | web UI and API port (127.0.0.1) |
| `SONARLESS_ADMIN_USER` / `SONARLESS_ADMIN_PASS` | `admin` / `admin` | local admin |
| `SONARLESS_MCP_PORT` | `9235` | shared MCP container port (127.0.0.1) |
| `SONARLESS_MCP_IMAGE` | `sonarsource/sonarqube-mcp:latest` | SonarQube MCP server image |
| `SONARLESS_GATEWAY_PORT` | `9233` | gateway that HTTP clients connect to (127.0.0.1); re-run `setup` after changing it |
| `SONARLESS_MCP_TOOLSETS` | server default | comma-separated MCP toolsets to enable |
| `SONARLESS_MCP_READ_ONLY` | `false` | disable the MCP write tools |
| `SONARLESS_MCP_JAVA_OPTS` | `-Xmx64m -XX:+UseSerialGC -XX:TieredStopAtLevel=1 -XX:ReservedCodeCacheSize=32m -XX:MaxMetaspaceSize=128m -Xss512k` | JVM options of the shared MCP server (`JAVA_TOOL_OPTIONS`), about 200 MiB instead of ~300 with the same tool latency; `none` keeps the JVM defaults |
| `SONARLESS_IDLE_TIMEOUT` | `30m` | stop the containers after this long unused; a bare number is minutes; `0` keeps them running |
| `SONARLESS_WEB_HEAP` / `SONARLESS_CE_HEAP` / `SONARLESS_SEARCH_HEAP` | `256m` / `384m` / `256m` | Java heaps of SonarQube's web server, compute engine and Elasticsearch (SonarQube's own: 512m each); raise `SONARLESS_CE_HEAP` if a big project's report fails with OutOfMemoryError. Changing one recreates the server container; its data is kept |
| `SONARLESS_INSTANCE` | `sonarless` | names the containers (`-server`, `-mcp`), network and volumes |
| `SONARLESS_AUTO_UPDATE` | `true` | daily background upgrade to a signed release once it is a day old, plus the MCP image ([Upgrading](upgrading.md)) |
| `SONARLESS_UPDATE_NOTICE` | `true` | in a terminal, a one-line stderr notice when a newer release exists |
| `SONARLESS_MCP_AGENT_TOOLS` | `true` | expose the `sonarless_*` tools to agents |
| `SONARLESS_PLUGINS_DIR` | `<config dir>/plugins` | extra plugin jars (a `shellcheck` binary there is mounted into CLI scans) |

Per project (also in `.sonarless.env`):

| Setting | Default | |
|---|---|---|
| `SONARLESS_PROJECT_KEY` / `SONARLESS_PROJECT_NAME` | detected | override the [detected](scanning.md#project-key-detection) key and name |
| `SONARLESS_SCANNER` | auto | `cli`, `maven`, `gradle` or `dotnet` |
| `SONARLESS_SCAN_TESTS` | `false` | run tests in build-based scans (needed for coverage) |
| `SONARLESS_FAIL_ON_GATE` | `false` | `sonarless-mcp scan` exits 2 when the quality gate fails, like `--fail-on-gate` ([exit status](scanning.md#exit-status)) |
| `SONARLESS_SOURCES` | `.` | `sonar.sources` for CLI scans without `sonar-project.properties` |
| `SONARLESS_SCANNER_IMAGE` | `sonarsource/sonar-scanner-cli:12.4` | sonar-scanner CLI image |
| `SONARLESS_METRICS_FILE` | `sonar-metrics.json` | file `sonarless-mcp results` writes |

Read from the environment only:

| Variable | |
|---|---|
| `SONARLESS_HOME` | keep the config in this dir and the cache in its `cache/` subdir instead of the OS dirs (tests, a second install) |
| `SONARLESS_MCP_TOKEN` | the gateway token, for Cursor, VS Code and Codex ([AI clients](clients.md#gateway-token-and-autostart)) |
| `SONARLESS_REMOTE_TOKEN` | token for `remote add` / `remote update` instead of the prompt |
| `SONARLESS_GATEWAY_SUPERVISED` | set by the login service; not for you to set |

The installer has its own variables; see [Install](install.md#installer-options).

## Files

| | Linux | macOS | Windows |
|---|---|---|---|
| config dir | `~/.config/sonarless-mcp` (`$XDG_CONFIG_HOME`) | `~/Library/Application Support/sonarless-mcp` | `%AppData%\sonarless-mcp` |
| cache dir | `~/.cache/sonarless-mcp` (`$XDG_CACHE_HOME`) | `~/Library/Caches/sonarless-mcp` | `%LocalAppData%\sonarless-mcp` |

With `SONARLESS_HOME` set, the config dir is that directory and the cache dir
its `cache` subdirectory.

In the config dir: `config.env`, `mcp-gateway-token` (owner-only),
`remotes.json` and `remotes/<name>.token`, `skills.json` (the
[skill copies](clients.md#agent-skill)), and `plugins/`.

In the cache dir: logs (`gateway.log`, `update.log`, `watcher.log`),
`gateway.pid`, `update-state.json`, the Sigstore root cache `sigstore-tuf/`,
locks and the MCP tool cache.

Scan history lives in Docker volumes named after `SONARLESS_INSTANCE`, one set
per SonarQube version.

## SonarQube versions

SonarQube's embedded database cannot be upgraded across versions, so each
version keeps its own data volumes: a new `SONARLESS_SONARQUBE_VERSION` starts
with fresh history, and switching back restores the old one.
`sonarless-mcp reset --yes` wipes the current version's history.
