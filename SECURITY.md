# Security policy

## Supported versions

Only the latest minor release gets security fixes: the newest `vX.Y.*` on the
[releases page](https://github.com/BrainerVirus/sonarless-mcp/releases).
Fixes ship as a new release, and installed binaries pick it up through the
signed background upgrade (or `sonarless-mcp upgrade`); older minor versions
are not patched.

| Version | Supported |
|---|---|
| latest minor (`X.Y.*`) | yes |
| anything older | no; upgrade |

## Reporting a vulnerability

Report privately through GitHub's security advisories:
[open a draft advisory](https://github.com/BrainerVirus/sonarless-mcp/security/advisories/new)
(Security tab → Report a vulnerability). Do not open a public issue, pull
request or discussion for a vulnerability.

Include:

- the sonarless-mcp version (`sonarless-mcp version`), OS and architecture;
- the affected part (CLI command, gateway, stdio bridge, self-upgrade,
  installer, client setup) and how it is configured;
- steps to reproduce, or a proof of concept;
- the impact you see: what an attacker can read, change or run, and from
  where (local user, another process, the network);
- any fix or mitigation you suggest, and whether and how you want credit.

Leave real tokens, passwords and private code out of the report.

You get an answer on the advisory; a confirmed issue is fixed in a new
release and the advisory is published once that release is out.
