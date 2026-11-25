# sonarless-mcp

Local SonarQube for any project, built for AI agents: **one** shared SonarQube
server and **one** shared [SonarQube MCP server](https://github.com/SonarSource/sonarqube-mcp-server)
per machine, used by every client (Claude Code, Cursor, opencode, VS Code,
Codex) at once. Single binary for Linux, macOS and Windows.

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
| Remote servers | — | ask about your CI's SonarQube too, and make local scans judge like it |

## Install

Requires Docker (Docker Desktop on macOS and Windows). Tell your AI agent:

```text
Install sonarless-mcp: https://raw.githubusercontent.com/BrainerVirus/sonarless-mcp/main/install
```

or run the same script yourself.

**Linux / macOS**

```sh
curl -fsSL https://raw.githubusercontent.com/BrainerVirus/sonarless-mcp/main/install | sh
```

**Windows (PowerShell)**

```powershell
irm https://raw.githubusercontent.com/BrainerVirus/sonarless-mcp/main/install | iex
```

The installer downloads the release for your OS and CPU, verifies its checksum
(and its signature when `cosign` is installed), puts the binary on your PATH
and opens the client picker. Options, `go install`, PATH details and
uninstalling: [docs/install.md](docs/install.md).

## Set up your AI clients

The installer runs this for you; run it again any time:

```sh
sonarless-mcp setup                          # picker: installed clients preselected
sonarless-mcp setup --clients claude,cursor  # no picker (scripts, agents)
```

```text
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

Setup registers a `sonarqube` MCP server in each client you pick (or
`sonarqube-local` when you already have a `sonarqube` entry of your own) and
installs a short agent skill. Restart the clients. Most connect directly over
HTTP to one local gateway; Cursor, VS Code and Codex need
`SONARLESS_MCP_TOKEN` in your shell for that, and setup prints the line to add.
How it works, the gateway, the tools agents get and the skill:
[docs/clients.md](docs/clients.md).

## First scan

Ask your agent "what does SonarQube say about this project?", or in a
terminal, inside the project:

```sh
sonarless-mcp scan       # detect the project, start SonarQube, scan, print the quality gate
sonarless-mcp results    # key metrics, saved to sonar-metrics.json
sonarless-mcp status     # what is running and how much memory it takes
```

The first run pulls the SonarQube images (a few GB). The server stops itself
after 30 minutes unused and resumes on the next request, history intact.
Scanners, project key detection, `stop` and `reset`:
[docs/scanning.md](docs/scanning.md).

To compare with your CI's SonarQube and make local scans judge like it:

```sh
sonarless-mcp remote add work --url http://sonar.internal:9000 --branch develop   # token: hidden prompt
sonarless-mcp remote sync work
```

More in [docs/remotes.md](docs/remotes.md).

## Upgrading

```sh
sonarless-mcp upgrade            # install the latest release now
sonarless-mcp upgrade --check    # is there a newer one?
sonarless-mcp version
```

Installed releases also upgrade themselves in the background once a newer
release is a day old, and every release is signed and checked before it
replaces the binary. Pinning, downgrades, the update notice and release
signing: [docs/upgrading.md](docs/upgrading.md).

## Documentation

- [Install](docs/install.md): installer options, PATH, `go install`, uninstall
- [AI clients](docs/clients.md): setup, the gateway, agent tools, the agent skill
- [Scanning and the server](docs/scanning.md): scan, results, project key detection, status, stop, reset
- [Remote servers](docs/remotes.md): add, update, import, sync with CI
- [Configuration](docs/configuration.md): every setting, file locations, SonarQube versions
- [Upgrading](docs/upgrading.md): upgrade, background upgrades, release signing
- [Troubleshooting](docs/troubleshooting.md)

`sonarless-mcp --help` lists every command and every `SONARLESS_*` variable;
`sonarless-mcp <command> --help` describes one command.

## Contributing

Conventional Commits drive fully automated releases (semantic-release +
GoReleaser); see [AGENTS.md](AGENTS.md) for layout, testing rules and the
release flow.

## Credits

- [gitricko/sonarless](https://github.com/gitricko/sonarless) — the original
  idea and CLI this project grew from (MIT).
- [SonarSource/sonarqube-mcp-server](https://github.com/SonarSource/sonarqube-mcp-server) — the MCP server this shares.

MIT licensed; see [LICENSE](LICENSE).
