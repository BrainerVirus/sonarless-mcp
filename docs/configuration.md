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
| `SONARLESS_MCP_TOOLSETS` | `coverage,duplications,issues,measures,projects,quality-gates,rules,security-hotspots` | comma-separated MCP toolsets to enable; see [MCP toolsets](#mcp-toolsets) |
| `SONARLESS_MCP_READ_ONLY` | `false` | disable the MCP write tools |
| `SONARLESS_MCP_JAVA_OPTS` | `-Xmx64m -XX:+UseSerialGC -XX:TieredStopAtLevel=1 -XX:ReservedCodeCacheSize=32m -XX:MaxMetaspaceSize=128m -Xss512k` | JVM options of the shared MCP server (`JAVA_TOOL_OPTIONS`), about 200 MiB instead of ~300 with the same tool latency; `none` keeps the JVM defaults; measured in [Performance](performance.md#mcp-jvm-native-memory) |
| `SONARLESS_MCP_APPCDS` | `true` | start the MCP server from a class-data archive built in the background for its image and JVM options: ready in about 2 s instead of 3.5 to 4, about 30 MiB less memory; `false` neither builds nor uses one ([below](#mcp-class-data-archive-appcds)) |
| `SONARLESS_IDLE_TIMEOUT` | `30m` | stop the containers after this long unused; a bare number is minutes; `0` keeps them running |
| `SONARLESS_WEB_HEAP` / `SONARLESS_CE_HEAP` / `SONARLESS_SEARCH_HEAP` | `256m` / `384m` / `256m` | Java heaps of SonarQube's web server, compute engine and Elasticsearch (SonarQube's own: 512m each); raise `SONARLESS_CE_HEAP` if a big project's report fails with OutOfMemoryError. Changing one recreates the server container; its data is kept. The 256m Elasticsearch heap is [measured](performance.md#elasticsearch-heap-256m-against-384m) |
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

### MCP toolsets

Every tool the MCP server lists costs agent context on each session. The
default is the SonarQube MCP server's own default without `dependency-risks`:
its one tool, `search_dependency_risks`, needs SonarQube's software composition
analysis (Enterprise), so on the local Community server it can only fail. That
drops about 7% of the tool list (1.4 of 21.7 KB with one remote, MCP server
1.33); every tool the skill and these docs name stays.

- `SONARLESS_MCP_TOOLSETS=default` leaves the choice to the MCP server again
  (`dependency-risks` comes back), for a remote with Advanced Security.
- To add other toolsets, list them: for example
  `coverage,duplications,issues,measures,projects,quality-gates,rules,security-hotspots,dependency-risks,sources,languages,system,webhooks,portfolios`
  also enables the server's opt-in Server toolsets. The names are the
  server's `SONARQUBE_TOOLSETS`; an unknown name is ignored and `projects` is
  always on.

The setting applies to the local and every remote MCP container; changing it
recreates them on next use.

### MCP class-data archive (AppCDS)

The MCP server is a Java program that loads about 12,000 classes at every
start. With `SONARLESS_MCP_APPCDS=true` (the default) sonarless-mcp keeps those
classes in a class-data archive the JVM maps instead of loading them again:
the server answers in about 2 s instead of 3.5 to 4, and uses about 30 MiB less
memory ([measured](performance.md#appcds-for-the-mcp-jvm)).

- **A start never waits for it.** Without a finished archive for the current
  MCP image and `SONARLESS_MCP_JAVA_OPTS`, the MCP container starts as before;
  once it answers, a one-off container `<instance>-cds` builds the archive in
  the background (about 10 s: a training start of the MCP server, then the
  dump). The next start of the MCP container (after the idle stop, `stop`, or
  an image update) uses it. A running container is never restarted for it.
- **One archive, the current one.** It lives in the Docker volume
  `<instance>-cds` (about 100 MiB), mounted read-only into the MCP containers.
  A new MCP image or new JVM options get a new archive; the build removes the
  old one.
- **Never a failed start.** The JVM checks the archive before it maps it
  (`-Xshare:auto -XX:+VerifySharedSpaces`) and runs without it when it does not
  fit or is damaged. A failed build or a rejected archive is recorded and tried
  again after a day, or at once for a new image or new JVM options.
- **The training start needs the local SonarQube**, as every MCP start does
  (the server checks it before it answers); the build only runs right after
  the MCP server got ready.
- **`used` is checked, not assumed.** After a start with the archive,
  sonarless-mcp reads the JVM's log for a rejection and then looks in
  `/proc/<pid>/maps` inside the container for the mapped file. An archive the
  JVM rejected or did not map (a missing file, say) is recorded as failed with
  the reason, and the next start runs plain. Only the local MCP container is
  checked and recorded: remote MCP containers map a ready archive the same way
  (read-only, `-Xshare:auto`) but never change its state.
- **`status` and `sonarless-mcp appcds` show the state**: `none` (built after
  the next start), `building`, `ready` (used from the next start), `used`,
  `failed` (with the reason and when it is tried again) or `disabled`. The
  last build's outcome is `<cache dir>/<instance>-cds.json`.

```sh
sonarless-mcp appcds                   # the archive's state
sonarless-mcp appcds rebuild           # build it again now (starts SonarQube if needed), wait for it
sonarless-mcp appcds rebuild --no-wait # start the build and return
```

`appcds rebuild` retries at once what a start would leave for a day (a failed
build, a rejected archive). The MCP server uses the new archive from its next
start: `sonarless-mcp stop` or the idle stop.

`SONARLESS_MCP_APPCDS=false` recreates the MCP containers without the archive
on next use and builds none; `docker volume rm <instance>-cds` frees its disk
space.

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
locks, the MCP tool cache and `<instance>-cds.json` (the last
[class-data archive](#mcp-class-data-archive-appcds) build).

Scan history lives in Docker volumes named after `SONARLESS_INSTANCE`, one set
per SonarQube version. The MCP class-data archive lives in the volume
`<instance>-cds`.

## SonarQube versions

SonarQube's embedded database cannot be upgraded across versions, so each
version keeps its own data volumes: a new `SONARLESS_SONARQUBE_VERSION` starts
with fresh history, and switching back restores the old one.
`sonarless-mcp reset --yes` wipes the current version's history.
