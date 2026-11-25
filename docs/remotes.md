# Remote SonarQube servers

Next to the local server you can add any number of remote ones, typically your
company's CI server, and ask the agent about either.

```sh
sonarless-mcp remote add work --url http://sonar.internal:9000 --branch develop   # token: hidden prompt
sonarless-mcp remote update work --token                      # new token (hidden prompt); keeps url/branch
sonarless-mcp remote update work --url http://10.0.0.5:9000   # server moved; keeps token
sonarless-mcp remote update work --branch main                # CI now analyzes main; keeps url/token
sonarless-mcp remote list                                     # name, reachable/unreachable, url, default branch
sonarless-mcp remote import work --branch develop             # reuse a SonarQube MCP entry another client already has
sonarless-mcp remote sync work                                # make local scans judge this project like CI
sonarless-mcp remote remove work                              # the remote, its token and its MCP container
```

The token comes from a hidden prompt, from stdin with `--token-stdin`, or from
`SONARLESS_REMOTE_TOKEN`:

```sh
sonarless-mcp remote add work --url https://sonar.example.com --token-stdin < token.txt
```

Use a **User token** (`squ_…`, *My Account → Security*): analysis tokens
(`sqa_`, `sqp_`) can only submit scans, not read results, so they are refused.
A remote can be added while its server is unreachable; a new URL or token is
checked against the server when it answers, and saved with a warning when it
does not.

`remote import <name>` looks for a SonarQube MCP entry in your AI clients that
is not sonarless-mcp but has `SONARQUBE_URL` and `SONARQUBE_TOKEN` (for example
a Docker-based `sonarqube` server in VS Code or Cursor) and adds it as a
remote. The client entry is left as is.

## Asking about a remote

Every MCP tool then takes an optional `server` argument: `local` (default, your
working copy as last scanned) or a remote's name (what CI found, on the
remote's default branch). The workspace's project key and the remote's branch
are filled in.

No restart is needed after `remote add`, `update` or `remove` (or an edit of
`remotes.json`): the gateway and the stdio bridge notice the change on their
next request, so that request can already use the new remote. Calls already
running finish where they started. Clients that follow tool-list change
notifications refresh the `server` choices at once; others show them after
they reconnect to the MCP server (for example `/mcp` in Claude Code).

## Judge local scans like CI

```sh
sonarless-mcp remote sync work      # in the project
sonarless-mcp remote sync work ../other-project
```

`remote sync` copies the remote's quality gate, the quality profiles (rule
sets) the project uses there and its new-code definition to your local server,
and applies them to the project, so a local `scan` fails on the same conditions
CI would (for example 80% coverage, 3% duplication) before you push. Copies are
named `work: <name>`; nothing built in is changed, and built-in profiles that
are identical on both servers (same SonarQube version) are left as they are.
Run it again to pick up changes on the remote.

## Staying out of the way

- Nothing touches a remote unless a tool call asks for it: startup,
  `initialize` and the tool list use only the local server.
- Before a remote call, a 3-second reachability check (cached for 30 seconds)
  runs. If the server is down or you are off the VPN, the agent gets a clear
  tool error and keeps using the local server; the next call after you
  reconnect works.
- A remote's MCP container starts on its first successful use and stops with
  the rest when idle, so configured but unused remotes cost nothing.
- Settings live in `remotes.json` in the config dir; each token in its own
  private file, `remotes/<name>.token` (mode 0600), never in `remotes.json`.
