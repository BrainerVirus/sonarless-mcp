# Upgrading

```sh
sonarless-mcp upgrade                    # install the latest release now (alias: update)
sonarless-mcp upgrade --check            # print installed and latest; change nothing
sonarless-mcp upgrade --version v0.9.0   # install that release (also an older one)
sonarless-mcp version                    # version, commit, build date, Go, OS/CPU
sonarless-mcp version --json
```

`upgrade` downloads the release for your OS and CPU, checks the signature of
its `checksums.txt` and the archive's SHA-256 ([Release signing](#release-signing)),
runs the new binary's `version` as a smoke test, and only then replaces the
running binary atomically. Any failed check leaves the installed binary
untouched and exits 1.

It also pulls the latest SonarQube MCP image (`SONARLESS_MCP_IMAGE`), which the
shared MCP container picks up the next time it starts from stopped, never in
the middle of a session. Then it has the new binary run `skill sync`, so the
[agent skill](clients.md#agent-skill) copies in your home directory take the
new version at once (a downgrade leaves them alone). Restart your AI clients
to use the new binary; the gateway restarts itself when its binary changes.

A development build (`go install`, `go build`) never upgrades itself, and
`upgrade` without `--version` refuses to replace it.

## Background upgrades

Installed release binaries keep themselves current. At most once a day, the
gateway, the stdio bridge, `start` and `scan` start a background
`upgrade` that installs a newer release once it has been published for a full
day, so a broken release can be pulled before it reaches you. The same run
pulls the MCP image. Its log is `<cache dir>/update.log`
([file locations](configuration.md#files)).

`SONARLESS_AUTO_UPDATE=false` turns background upgrades off; `upgrade` still
works.

## Update notice

In an interactive terminal, sonarless-mcp prints one line on stderr when a
newer release exists (at most once a day), and one after a background upgrade
installed one:

```text
A new release of sonarless-mcp is available: 0.9.0 → 0.10.0; run `sonarless-mcp upgrade` (https://github.com/BrainerVirus/sonarless-mcp/releases/tag/v0.10.0)
```

It shows only when both stdout and stderr are a terminal and never in CI, and
never for `mcp`, `serve`, `ensure`, `upgrade` or `--json` output, so agents, MCP clients
and scripts never see it. `SONARLESS_UPDATE_NOTICE=false` turns it off.

## Release signing

Every release's `checksums.txt` carries a [Sigstore](https://www.sigstore.dev/)
keyless signature, `checksums.txt.sigstore.json`, made by this repository's
release workflow on GitHub Actions. `upgrade` and the background run verify it
against that identity before trusting the checksums, and refuse a release whose
signature is missing or made by anyone else. Sigstore's trusted root comes from
its TUF repository, anchored on the TUF root built into the binary and cached
in `<cache dir>/sigstore-tuf`.

To check a download by hand:

```sh
cosign verify-blob \
  --bundle checksums.txt.sigstore.json \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  --certificate-identity-regexp '^https://github\.com/BrainerVirus/sonarless-mcp/\.github/workflows/release\.yml@refs/heads/main$' \
  checksums.txt
sha256sum --ignore-missing -c checksums.txt
```

### Going back to an unsigned release

Releases up to v0.8.1 were published before signing existed. To go back to one
of them, name it and opt in:

```sh
sonarless-mcp upgrade --version v0.8.1 --allow-unsigned
```

That installs it on its checksum alone. `--allow-unsigned` only accepts a
release that is v0.8.1 or older and older than the installed one; a later
release without a valid signature is always refused.

The install scripts do the same check when `cosign` is on your PATH: they
accept a missing signature only when the release (latest or a pinned
`SONARLESS_MCP_VERSION`) is v0.8.1 or older. Without cosign they check the
SHA-256 only.
