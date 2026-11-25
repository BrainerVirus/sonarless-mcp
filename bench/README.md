# Skill bench

The agent skill in `internal/clients/skill/` has to work on small models while
costing as few tokens as possible. The bench measures both on a real model:
each case gives Claude Code a SonarQube task in a throwaway project with the
skill installed, and grades which `sonarless-mcp` commands and MCP tools it
used. It needs a Claude Code login, spends subscription usage and never runs
in CI (`go test ./...` only runs its unit tests, which grade canned calls and
never start a model).

## Run it

```sh
go run ./bench                                    # Haiku (claude-haiku-4-5), every case once
go run ./bench --case scan-gate,flag-typo         # some cases
go run ./bench --model claude-sonnet-4-5 --repeat 3
go run ./bench compare bench/results/baseline/claude-haiku-4-5 bench/results/claude-haiku-4-5
go run ./bench --no-skill                         # control: the same worlds without the skill
```

Run it from the repository root.

- `--model` takes any Claude Code model id or alias (default
  `claude-haiku-4-5`); results go to `bench/results/<model>/`: one
  `<case>.json` with every run, and `summary.json` with the totals.
- `--repeat n` runs each case n times, each in a fresh world. A case passes
  when at least two thirds of its graded runs pass.
- `--budget-runs n` (default 30) caps the `claude -p` runs one invocation
  starts; `--jobs n` (default 3) runs that many at once. A run is capped at
  $0.50 (`--max-budget-usd`).
- Stored runs are reused when their key matches: the case file, the skill's
  files, the model and the bench version. An edit to the skill re-runs every
  case; `--force` re-runs anyway.
- Exit 1 when a case fails, a run hits a harness error or runs were left over
  the budget.
- `regrade <dir>` grades the stored runs of a results dir again with the
  current cases, without a model (after fixing a rule).
- `compare <before> <after>` prints each case's passes and mean input tokens
  per run side by side, and exits 1 when a case's pass rate dropped. Keep the
  results of the skill before a change in `bench/results/baseline/<model>/`
  to compare against.

Scrubbed transcripts of each run land in `bench/.transcripts/<model>/`
(ignored by git) for debugging a failure.

## What a run does

Each run gets a temp dir with two parts:

- `project/`, the model's working directory: a small TypeScript project with
  a `sonar-project.properties`, and the skill copied to
  `.claude/skills/sonarless-mcp/`.
- `harness/`, outside the project: `bin/sonarless-mcp` (the fake),
  `case.json` (its fixtures), `mcp.json` (the fake registered as the
  `sonarqube` stdio MCP server, as `setup --stdio` registers the real one) and
  `calls.jsonl` (its log).

Then, in `project/`, with the prompt on stdin:

```sh
claude -p --model <m> --output-format stream-json --verbose --max-turns <n> \
  --setting-sources project --mcp-config <harness>/mcp.json --strict-mcp-config \
  --no-session-persistence --permission-mode dontAsk --max-budget-usd 0.50 \
  --allowedTools Skill 'Read(./**)' mcp__sonarqube 'Bash(sonarless-mcp *)' 'Bash(ls *)' 'Bash(echo *)' 'Bash(pwd)' \
  --disallowedTools WebFetch WebSearch Task Agent
```

`--setting-sources project` loads no user settings, skills or plugins (so a
host plugin's hooks do not gate the run), and `--strict-mcp-config` no MCP
server but the fake. A plain `sonarless-mcp` command that is still denied came
from a host policy: that run is `blocked` and not graded. Anything else
outside the allow list is denied by `dontAsk` and listed under `denials`.

### Nothing reaches docker or the network

The child environment drops `WORKIT_*`, `SONARLESS_*`, `DOCKER_*`,
`CLAUDECODE`, `ANTHROPIC_API_KEY` and `ANTHROPIC_AUTH_TOKEN`, puts
`harness/bin` first on `PATH` and removes every other `PATH` directory that
holds a `sonarless-mcp` (your real install). It also points `SONARLESS_HOME`
into the harness, turns off upgrades and the update notice, and sets
`DOCKER_HOST` to a socket that does not exist, so even a real binary reached
by mistake could not touch your containers. A run where the model ran
`sonarless-mcp` but the fake logged nothing is a harness error.

### The fake sonarless-mcp

`harness/bin/sonarless-mcp` is the bench binary itself (it acts as
sonarless-mcp when run under that name). It parses each command line with the
real command tree (`internal/cli.NewRootCommand`, built but never executed):
an unknown command or flag fails with the real message, exit 1 and the same
flag hint (`did you mean --fail-on-gate?`), and `--help` prints the real help.
It never runs a command: a call is answered from the case's `fixtures.rules`
(regex on the call line, first match wins), then from built-in replies (scan
passes the gate, `upgrade --check` finds v0.15.0, `skill list` shows one stale
copy, `remote add` refuses to prompt for a token without a terminal).

`sonarless-mcp mcp` is a fake SonarQube MCP server on stdio. It lists the
`sonarless_*` tools and the SonarQube tools agents use most, with the real
descriptions, the `server` argument of a workspace with one remote (`work`)
and the real server instructions, and answers each call the same way.

Each CLI run and each tool call is appended to `calls.jsonl` with its exit
code (a tool error counts as exit 1).

## Cases

`bench/cases/<id>.json`:

| Field | Meaning |
| --- | --- |
| `prompt` | the user message |
| `maxTurns` | `--max-turns` (default 10) |
| `files` | files written into the project |
| `fixtures` | `rules` (`match`, `stdout`, `stderr`, `exit`) |
| `require` | rules that must hold |
| `forbid` | rules that must not hold |
| `answer` | regex the final message must match (case-insensitive) |

A rule matches lines of one source: `calls` (default) or `commands` (each
Bash command as the model wrote it). A CLI call reads
`sonarless-mcp <argv joined by spaces>` (`--flag=value` written
`--flag value`); a tool call reads `mcp <tool> <arguments as JSON, keys
sorted>`, such as `mcp get_project_quality_gate_status {"server":"work"}`. A
line matches when every `match` regex and no `not` regex matches. A `require`
rule needs one matching call that exited 0; with `before`, its first match
must come before the first other line matching every `before` regex. A
`forbid` rule breaks on `times` matching lines (default 1).

| Case | What it checks |
| --- | --- |
| `scan-gate` | `sonarless_scan` (or `sonarless-mcp scan`) once; a failing gate is reported as a result |
| `file-issues` | one file's local issues through `search_sonar_issues_in_projects`, no rescan, no remote |
| `ci-gate` | CI's gate: `get_project_quality_gate_status` with `server: "work"` |
| `compare-ci` | local and `work` issues side by side; offers `sonarless_remote_sync` without running it |
| `pass-ci-gate` | `sonarless_remote_sync`, then a scan |
| `upgrade-check` | `sonarless-mcp upgrade --check`, never a real upgrade |
| `skill-list` | `sonarless-mcp skill list`, no `skill sync` |
| `flag-typo` | a mistyped `--fail-on-gates`: the hint, then `scan --fail-on-gate` |
| `add-remote` | a token never goes through the agent: the user gets the `remote add` command |
| `remote-offline` | the remote is unreachable: say so and fall back to `local` |

A new case gets a row in
`TestGivenEachCaseWhenGradedItPassesTheTaughtRunAndFailsTheTrap`
(`bench/bench_test.go`): the run the skill teaches must pass and the trap
must fail, so a broken regex fails in `go test`, not in a model run.

## What a result holds

Each run records its status (`pass`, `fail`, `blocked`, `error`), the first
failed rule and all of them, turns, tokens (`input`, `output`,
`cacheCreation`, `cacheRead`; "total input" is their prompt-side sum),
`costUSD`, duration, whether the skill was listed at start and loaded by the
model, whether the fake MCP server connected, the Bash commands, the fake's
calls, denials and the final answer. A run whose model never loaded the skill
is graded all the same (with the MCP tools at hand a model may skip the
skill); `summary.json` counts those runs under `notLoaded`.

Before writing, the bench replaces the world's path with `<world>`, the home
directory with `~` and the login name with `<user>`, masks anything shaped
like a SonarQube token (`squ_`, `sqa_`, `sqp_`), an Anthropic key or a bearer
header as `<token>`, and refuses to write a file that still holds a home path
or a token.
