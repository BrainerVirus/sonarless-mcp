# Install

sonarless-mcp is one binary for Linux, macOS and Windows (amd64 and arm64).
It needs Docker (Docker Desktop on macOS and Windows) to run SonarQube.

## The installer

One script, valid in both `sh` and PowerShell:

```sh
curl -fsSL https://raw.githubusercontent.com/BrainerVirus/sonarless-mcp/main/install | sh
```

```powershell
irm https://raw.githubusercontent.com/BrainerVirus/sonarless-mcp/main/install | iex
```

`install` hands off to the platform installer (`install.sh` or `install.ps1`),
which:

1. detects your OS and CPU and resolves the release (the latest one, or
   `SONARLESS_MCP_VERSION`) through the GitHub API;
2. downloads the archive and `checksums.txt`, checks the archive's SHA-256 and,
   when [cosign](https://docs.sigstore.dev/cosign/system_config/installation/)
   is on your PATH, the signature of `checksums.txt` (see
   [Release signing](upgrading.md#release-signing));
3. installs the binary in `~/.local/bin` (Windows:
   `%LOCALAPPDATA%\Programs\sonarless-mcp`);
4. makes sure that directory is on your PATH (below);
5. runs `sonarless-mcp setup`, the client picker (see [AI clients](clients.md)).

Run in a terminal, step 5 opens the picker on the terminal even though the
script came through a pipe. Run by an AI agent or a script (no terminal), it
runs `sonarless-mcp setup --list` instead and prints the command that finishes
the job, for example `sonarless-mcp setup --clients claude,cursor`. Arguments
after `sh -s --` are passed to `setup`:

```sh
curl -fsSL https://raw.githubusercontent.com/BrainerVirus/sonarless-mcp/main/install | sh -s -- --yes
```

### Installer options

| Variable | Effect |
|---|---|
| `SONARLESS_MCP_VERSION=v0.9.0` | install that release instead of the latest |
| `SONARLESS_MCP_INSTALL_DIR=<dir>` | install into `<dir>` |
| `SONARLESS_MCP_NO_SETUP=1` | install only; skip `setup` |
| `SONARLESS_MCP_NO_MODIFY_PATH=1` | never edit shell startup files |

These are read by the install scripts only; the binary's own variables are
listed by `sonarless-mcp --help` and in [Configuration](configuration.md).

### PATH

If `~/.local/bin` is not on your PATH, `install.sh` adds one guarded, marked
line to the startup file of every shell it finds: `~/.profile`, `~/.bashrc`
(and `~/.bash_profile` on macOS when it exists), zsh's `.zshenv`, and fish's
`conf.d/sonarless-mcp.fish`. Reruns change nothing. On Windows the directory
goes on your user PATH.

A terminal that was already open may not recognize (or highlight) the new
command yet: in zsh run `rehash`, in bash `hash -r`; fish notices on its own;
Warp reads PATH only when a tab starts, so open a new tab. The installer
detects your shell and terminal and prints the one that applies.

## With Go

```sh
go install github.com/BrainerVirus/sonarless-mcp/cmd/sonarless-mcp@latest
sonarless-mcp setup
```

This is a development build: it never upgrades itself in the background, and
`sonarless-mcp upgrade` without `--version` refuses to replace it.
`sonarless-mcp upgrade --version v0.9.0` replaces it with a signed release.

## Uninstall

```sh
sonarless-mcp setup --remove --yes   # unregister from every configured client, remove the skill copies and the gateway autostart
sonarless-mcp stop --all             # stop the containers and the gateway
```

Then delete the binary, the config dir and the cache dir
([file locations](configuration.md#files)). Scan history lives in Docker
volumes named after `SONARLESS_INSTANCE` (default `sonarless`), and the MCP
class-data archive in `<instance>-cds`; remove them with `docker volume rm` if
you want them gone too.
