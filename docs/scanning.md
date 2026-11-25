# Scanning and the server

## Scan

```sh
sonarless-mcp scan                     # detect, start the server, scan, wait for the quality gate
sonarless-mcp scan ../other-project    # another directory
sonarless-mcp scan --tests             # build-based scans: run the tests too (coverage)
sonarless-mcp scan --scanner gradle    # force cli, maven, gradle or dotnet
sonarless-mcp scan -D sonar.exclusions=**/gen/**   # extra analysis property (repeatable)
sonarless-mcp results                  # key metrics, saved to sonar-metrics.json
sonarless-mcp results -o metrics.json
sonarless-mcp project                  # detected root, key, name, scanner and where they came from
```

`scan` prints the quality gate and the dashboard URL when the report is
processed. It exits 0 whatever the gate says and 1 only when the scan itself
fails, so read the gate from its output, from `results` or through the MCP
tools.

Scanners, picked per project:

- Maven: `./mvnw`, then `mvn`, then a Maven container;
- Gradle: `./gradlew`, then `gradle`, then a Gradle container (the SonarQube
  plugin is applied by an init script if the build does not declare it);
- .NET: `dotnet-sonarscanner`;
- everything else: the sonar-scanner CLI container (`SONARLESS_SCANNER_IMAGE`).

Build-based scans skip tests unless `--tests` (or `SONARLESS_SCAN_TESTS=true`).
A `shellcheck` binary in the plugins dir is mounted into CLI scans.

## Project key detection

First match wins:

1. `SONARLESS_PROJECT_KEY` (environment or `.sonarless.env`)
2. `sonar-project.properties`, then `.sonarcloud.properties`
3. explicit build config: `pom.xml` `<properties>`, Gradle `sonar { properties { ... } }` / `gradle.properties`, `package.json` `"sonar"`
4. `pyproject.toml` `[tool.sonar]`
5. .NET `SonarQube.Analysis.xml` / `Directory.Build.props`
6. CI pipelines (GitHub Actions, GitLab CI, Jenkinsfile, Azure Pipelines, Bitbucket, CircleCI): `-Dsonar.projectKey=…`, `/k:…`, `projectKey:`, so local keys match CI
7. build defaults: Maven `groupId:artifactId`, Gradle `group:rootProject.name`, `.sln` name, `package.json` name
8. the directory name

The project root is the topmost Maven, Gradle or Sonar root under the git root,
so running from inside a module scans the whole build.

## Status

```sh
sonarless-mcp status          # containers, gateway, bridges, scans, idle stop
sonarless-mcp status --json
```

`status` lists the server, local MCP and remote MCP containers (state, image,
memory in use), the gateway (pid, port, uptime, and each workspace session with
its last request), the stdio bridges (`sonarless-mcp mcp`) still running, the
scans in progress, CLI or agent-started (`waiting` for the server, or
`scanning`), and when the idle watcher stops everything. `--json` prints the
same as JSON (`containers`, `gateway`, `shims`, `scans`, `idle`).

## Start, stop and reset

```sh
sonarless-mcp start           # start (or create) the SonarQube server
sonarless-mcp stop            # stop the server and every MCP container
sonarless-mcp stop --all      # ...and the gateway
sonarless-mcp reset --yes     # wipe this SonarQube version's history and start fresh
```

You rarely need `start`: the first scan or MCP request starts (or resumes) the
containers, and after `SONARLESS_IDLE_TIMEOUT` (default 30m) without MCP
calls, scans or web UI traffic, a background watcher stops them again. The
next request resumes them with their history. `SONARLESS_IDLE_TIMEOUT=0` keeps
them running.

### Stop and reset

`stop` stops the server, the local MCP container and every remote MCP
container of this instance, including one left from a removed remote (found by
its `sonarless-mcp.instance` label, never by name, so another
`SONARLESS_INSTANCE` is untouched). Data is kept. It leaves the gateway
running: it holds no container memory and connected clients expect it to
answer.

`stop --all` stops the gateway too, through its login service when setup
installed one for this config (systemd stops it until the next login, launchd
until the next login or `setup`). Claude Code starts it again on its next
connection; clients that rely on the login autostart wait for the next login,
`sonarless-mcp setup` or a manual `sonarless-mcp serve`. The login service is
one per user: if it was installed with another `SONARLESS_HOME`,
`SONARLESS_INSTANCE` or XDG dir, `stop --all` leaves it running and says so.

`reset --yes` deletes the current SonarQube version's scan history and starts a
fresh server. Without `--yes` it changes nothing.

The web UI is at `http://127.0.0.1:9234` (`SONARLESS_SERVER_PORT`), user
`admin`, password `admin` (`SONARLESS_ADMIN_USER`, `SONARLESS_ADMIN_PASS`).
