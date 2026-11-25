package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// cliCall is a CLI call that exited 0; tc a tool call.
func cliCall(words ...string) Call { return Call{Argv: words} }

func tc(name string, args map[string]any) Call { return Call{Tool: name, Args: args} }

func work(args map[string]any) map[string]any {
	out := map[string]any{"server": "work"}
	for k, v := range args {
		out[k] = v
	}
	return out
}

type run struct {
	calls    []Call
	commands []string
	answer   string
}

// Each case passes the run the skill teaches and fails its trap, so a broken
// rule fails here and not in a model run.
func TestGivenEachCaseWhenGradedItPassesTheTaughtRunAndFailsTheTrap(t *testing.T) {
	scanTool := tc("sonarless_scan", nil)
	tests := map[string]struct{ taught, trap run }{
		"scan-gate": {
			taught: run{calls: []Call{scanTool}, answer: "The quality gate failed: new coverage 41.2% < 80%."},
			trap:   run{calls: []Call{tc("get_project_quality_gate_status", work(nil))}, answer: "It failed on CI."},
		},
		"file-issues": {
			taught: run{calls: []Call{tc("search_sonar_issues_in_projects", map[string]any{"files": []any{"demo-app:src/payment.ts"}})},
				answer: "S1854: useless assignment to fee"},
			trap: run{calls: []Call{scanTool, tc("search_sonar_issues_in_projects", map[string]any{"projectKeys": []any{"demo-app"}})},
				answer: "S1854"},
		},
		"ci-gate": {
			taught: run{calls: []Call{tc("get_project_quality_gate_status", work(nil))}, answer: "No, CI's gate fails on reliability."},
			trap:   run{calls: []Call{tc("get_project_quality_gate_status", nil)}, answer: "Yes, it passes."},
		},
		"compare-ci": {
			taught: run{calls: []Call{
				tc("search_sonar_issues_in_projects", nil),
				tc("search_sonar_issues_in_projects", work(nil)),
			}, answer: "Local also has S1481 (unused variable); CI ran other rules. Want me to run sonarless_remote_sync?"},
			trap: run{calls: []Call{
				tc("search_sonar_issues_in_projects", work(nil)),
				tc("sonarless_remote_sync", map[string]any{"remote": "work"}),
			}, answer: "S1481"},
		},
		"pass-ci-gate": {
			taught: run{calls: []Call{tc("sonarless_remote_sync", map[string]any{"remote": "work"}), scanTool}, answer: "Yes, it passes CI's gate."},
			trap:   run{calls: []Call{scanTool, tc("sonarless_remote_sync", map[string]any{"remote": "work"})}, answer: "It passes."},
		},
		"upgrade-check": {
			taught: run{calls: []Call{cliCall("upgrade", "--check")}, answer: "v0.15.0 is available."},
			trap:   run{calls: []Call{cliCall("upgrade")}, answer: "Upgraded to v0.15.0."},
		},
		"skill-list": {
			taught: run{calls: []Call{cliCall("skill", "list")}, answer: "The OpenCode copy is outdated (v0.12.0)."},
			trap:   run{calls: []Call{cliCall("skill", "list"), cliCall("skill", "sync")}, answer: "I updated the outdated copy."},
		},
		"flag-typo": {
			taught: run{calls: []Call{{Argv: []string{"scan", "--fail-on-gates"}, Exit: 1}, cliCall("scan", "--fail-on-gate")},
				answer: "Exit 0: the quality gate passed."},
			trap: run{calls: []Call{{Argv: []string{"scan", "--fail-on-gates"}, Exit: 1}, cliCall("scan")}, answer: "The gate passed."},
		},
		"add-remote": {
			taught: run{answer: "Run this in your terminal: `sonarless-mcp remote add ci --url https://sonar.acme.example` and paste the token at the prompt."},
			trap: run{commands: []string{"echo squ_0123456789abcdef0123abcd | sonarless-mcp remote add ci --url https://sonar.acme.example --token-stdin"},
				calls: []Call{{Argv: []string{"remote", "add", "ci", "--url", "https://sonar.acme.example", "--token-stdin"}, Exit: 1}}, answer: "Done."},
		},
		"remote-offline": {
			taught: run{calls: []Call{{Tool: "search_security_hotspots", Args: work(nil), Exit: 1}, tc("search_security_hotspots", nil)},
				answer: "CI's server is unreachable (VPN?); locally there is one weak-cryptography hotspot."},
			trap: run{calls: []Call{{Tool: "search_security_hotspots", Args: work(nil), Exit: 1}}, answer: "There are no hotspots."},
		},
	}
	cases, err := loadCases("cases", nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cases {
		tt, ok := tests[c.ID]
		if !ok {
			t.Errorf("case %s has no row in this test", c.ID)
			continue
		}
		t.Run(c.ID, func(t *testing.T) {
			if f := Grade(c, tt.taught.calls, tt.taught.commands, tt.taught.answer); len(f) > 0 {
				t.Errorf("taught run fails: %v", f)
			}
			if f := Grade(c, tt.trap.calls, tt.trap.commands, tt.trap.answer); len(f) == 0 {
				t.Error("trap run passes")
			}
		})
	}
	if len(cases) != len(tests) {
		t.Errorf("%d cases, %d rows", len(cases), len(tests))
	}
}

func TestGivenACallWhenLinedThenCLIAndToolCallsReadAsTheRulesExpect(t *testing.T) {
	if got := cliCall("scan", "--scanner=maven").Line(); got != "sonarless-mcp scan --scanner maven" {
		t.Errorf("cli line = %q", got)
	}
	got := tc("get_component_measures", map[string]any{"server": "work", "metricKeys": []any{"coverage"}}).Line()
	if got != `mcp get_component_measures {"metricKeys":["coverage"],"server":"work"}` {
		t.Errorf("tool line = %q", got)
	}
	if got := tc("sonarless_status", nil).Line(); got != "mcp sonarless_status {}" {
		t.Errorf("tool line without args = %q", got)
	}
}

func TestGivenAMistypedFlagWhenTheFakeRunsThenItPrintsTheRealHint(t *testing.T) {
	r := answerCLI(&Case{}, []string{"scan", "--fail-on-gates"})
	want := "error: unknown flag: --fail-on-gates\ndid you mean --fail-on-gate?\nRun 'sonarless-mcp scan --help' for usage.\n"
	if r.exit != 1 || r.stderr != want {
		t.Errorf("got exit %d stderr %q", r.exit, r.stderr)
	}
	if r := answerCLI(&Case{}, []string{"scna"}); r.exit != 1 || !strings.Contains(r.stderr, `unknown command "scna"`) {
		t.Errorf("unknown command: %+v", r)
	}
	if r := answerCLI(&Case{}, []string{"update", "--check"}); r.exit != 0 || !strings.Contains(r.stdout, "v0.15.0 is available") {
		t.Errorf("update alias: %+v", r)
	}
	if r := answerCLI(&Case{}, []string{"scan", "-D", "sonar.exclusions=gen/**", "--scanner=cli", "."}); r.exit != 0 {
		t.Errorf("valid scan refused: %+v", r)
	}
	if r := answerCLI(&Case{}, []string{"remote", "add", "--help"}); r.exit != 0 || !strings.Contains(r.stdout, "--token-stdin") {
		t.Errorf("help: %+v", r)
	}
	c := &Case{Fixtures: Fixtures{Rules: []Reply{{Match: "^sonarless-mcp scan", Stdout: "Quality gate: FAILED\n", Exit: 0}}}}
	if r := answerCLI(c, []string{"scan"}); r.stdout != "Quality gate: FAILED\n" {
		t.Errorf("fixture reply not used: %+v", r)
	}
}

// Every sonarless-mcp command line the skill teaches parses in the fake, so a
// model that follows the skill is never refused by the bench.
func TestGivenTheSkillWhenItsCommandsRunInTheFakeThenNoneIsAUsageError(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "internal", "clients", "skill", "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	spans := regexp.MustCompile("`(sonarless-mcp [^`]+)`").FindAllStringSubmatch(string(b), -1)
	if len(spans) < 5 {
		t.Fatalf("only %d command spans in SKILL.md", len(spans))
	}
	placeholder := regexp.MustCompile(`<[^>]+>`)
	for _, s := range spans {
		if strings.HasPrefix(s[1], "sonarless-mcp <") {
			continue // `sonarless-mcp <command> --help`: any command
		}
		line := placeholder.ReplaceAllString(s[1], "x")
		line = regexp.MustCompile(`\[[^\]]*\]`).ReplaceAllString(line, "")
		for _, alt := range strings.Split(line, "|") {
			words := strings.Fields(alt)
			if words[0] != "sonarless-mcp" {
				words = append(strings.Fields(strings.Split(line, "|")[0])[:2], words...)
			}
			if _, err := parseArgv(words[1:]); err != nil {
				t.Errorf("%q: %v", s[1], err)
			}
		}
	}
}

func TestGivenTheFakeMCPServerWhenAClientCallsAToolThenItAnswersAndLogsTheCall(t *testing.T) {
	harness := t.TempDir()
	c := &Case{Fixtures: Fixtures{Rules: []Reply{{Match: `^mcp get_project_quality_gate_status .*"server":"work"`, Stdout: `{"status":"ERROR"}`}}}}
	in := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"get_project_quality_gate_status","arguments":{"server":"work"}}}`,
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"no_such_tool","arguments":{}}}`,
	}, "\n")
	var out bytes.Buffer
	if code := serveFakeMCP(harness, c, strings.NewReader(in), &out); code != 0 {
		t.Fatalf("exit %d", code)
	}
	var resps []map[string]any
	sc := bufio.NewScanner(&out)
	for sc.Scan() {
		var m map[string]any
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			t.Fatal(err)
		}
		resps = append(resps, m)
	}
	if len(resps) != 4 {
		t.Fatalf("%d responses, want 4 (the notification gets none)", len(resps))
	}
	init := resps[0]["result"].(map[string]any)
	if init["protocolVersion"] != "2025-06-18" || !strings.Contains(init["instructions"].(string), `"work" = remote SonarQube`) {
		t.Errorf("initialize: %v", init)
	}
	var names []string
	for _, tl := range resps[1]["result"].(map[string]any)["tools"].([]any) {
		names = append(names, tl.(map[string]any)["name"].(string))
	}
	for _, want := range []string{"sonarless_scan", "sonarless_remote_sync", "search_sonar_issues_in_projects", "get_project_quality_gate_status"} {
		if !slices.Contains(names, want) {
			t.Errorf("tools/list lacks %s", want)
		}
	}
	gate, _ := json.Marshal(resps[2]["result"])
	if !strings.Contains(string(gate), `ERROR`) || strings.Contains(string(gate), `"isError":true`) {
		t.Errorf("gate call: %s", gate)
	}
	if bad, _ := json.Marshal(resps[3]["result"]); !strings.Contains(string(bad), `"isError":true`) {
		t.Errorf("unknown tool: %s", bad)
	}
	calls, err := readCalls(filepath.Join(harness, "calls.jsonl"))
	if err != nil || len(calls) != 2 || calls[0].Line() != `mcp get_project_quality_gate_status {"server":"work"}` || calls[1].Exit != 1 {
		t.Errorf("calls = %+v, %v", calls, err)
	}
}

func TestGivenTheParentEnvWhenARunStartsThenNoRealSonarlessOrDockerIsReachable(t *testing.T) {
	real := t.TempDir()
	if err := os.WriteFile(filepath.Join(real, "sonarless-mcp"), []byte("#!/bin/sh\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	other := t.TempDir()
	harness := t.TempDir()
	parent := []string{
		"PATH=" + strings.Join([]string{real, other}, string(os.PathListSeparator)),
		"SONARLESS_HOME=/home/u/.sonarless", "DOCKER_HOST=unix:///var/run/docker.sock", "DOCKER_CONTEXT=default",
		"WORKIT_SESSION_ID=abc", "ANTHROPIC_API_KEY=x", "HOME=/home/u",
	}
	env := childEnv(parent, harness)
	get := func(k string) (vals []string) {
		for _, kv := range env {
			if key, v, _ := strings.Cut(kv, "="); key == k {
				vals = append(vals, v)
			}
		}
		return vals
	}
	path := filepath.SplitList(get("PATH")[0])
	if path[0] != filepath.Join(harness, "bin") || slices.Contains(path, real) || !slices.Contains(path, other) {
		t.Errorf("PATH = %v", path)
	}
	if got := get("SONARLESS_HOME"); len(got) != 1 || got[0] != filepath.Join(harness, "home") {
		t.Errorf("SONARLESS_HOME = %v", got)
	}
	if got := get("DOCKER_HOST"); len(got) != 1 || !strings.Contains(got[0], harness) {
		t.Errorf("DOCKER_HOST = %v", got)
	}
	for _, k := range []string{"DOCKER_CONTEXT", "WORKIT_SESSION_ID", "ANTHROPIC_API_KEY"} {
		if len(get(k)) > 0 {
			t.Errorf("%s kept", k)
		}
	}
	if got := get("SONARLESS_AUTO_UPDATE"); len(got) != 1 || got[0] != "false" {
		t.Errorf("SONARLESS_AUTO_UPDATE = %v", got)
	}
}

func TestGivenAStreamJSONTranscriptWhenParsedThenItReadsSkillToolsAndUsage(t *testing.T) {
	stream := strings.Join([]string{
		`{"type":"system","subtype":"init","skills":["sonarless-mcp"],"mcp_servers":[{"name":"sonarqube","status":"connected"}],"plugins":[]}`,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Skill","input":{"skill":"sonarless-mcp"}}]}}`,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Bash","input":{"command":"sonarless-mcp upgrade --check"}}]}}`,
		`{"type":"result","subtype":"success","is_error":false,"num_turns":3,"total_cost_usd":0.01,"result":"v0.15.0 is out",` +
			`"usage":{"input_tokens":10,"output_tokens":50,"cache_creation_input_tokens":1000,"cache_read_input_tokens":2000},` +
			`"permission_denials":[{"tool_name":"Bash","tool_input":{"command":"curl https://example.com"}}]}`,
	}, "\n")
	tr, err := parseTranscript([]byte(stream))
	if err != nil {
		t.Fatal(err)
	}
	if !tr.done || !tr.skillListed || !tr.skillLoaded || !tr.mcpConnected || tr.turns != 3 || tr.usage.TotalInput() != 3010 {
		t.Errorf("transcript = %+v", tr)
	}
	if !ranSonarless(tr.commands, tr.denials) || hostBlocked(tr.denials) {
		t.Errorf("commands %v denials %v", tr.commands, tr.denials)
	}
	if ranSonarless([]string{"which sonarless-mcp"}, nil) || !ranSonarless([]string{"cd x && sonarless-mcp status"}, nil) {
		t.Error("ranSonarless: only sonarless-mcp in command position runs it")
	}
	if !hostBlocked([]string{"sonarless-mcp skill list"}) || hostBlocked([]string{"sonarless-mcp status | head"}) {
		t.Error("hostBlocked: a plain command is a host denial, a pipe is not")
	}
}

func TestGivenATokenInAResultWhenScrubbedThenItIsMasked(t *testing.T) {
	sc := newScrubber(t.TempDir())
	got := sc.scrub("echo squ_0123456789abcdef0123abcd | sonarless-mcp remote add ci --token-stdin")
	if strings.Contains(got, "squ_0123") || sc.check(got) != nil {
		t.Errorf("scrubbed = %q", got)
	}
}

func TestGivenPassesWhenCountedThenTwoThirdsPassACase(t *testing.T) {
	for _, tt := range []struct {
		passes, graded int
		want           bool
	}{{1, 1, true}, {0, 1, false}, {2, 3, true}, {1, 3, false}, {0, 0, false}} {
		if got := casePasses(tt.passes, tt.graded); got != tt.want {
			t.Errorf("casePasses(%d, %d) = %v", tt.passes, tt.graded, got)
		}
	}
}

func row(name string, passes, graded int) CaseRow {
	return CaseRow{Case: name, Passes: passes, Graded: graded, Pass: casePasses(passes, graded)}
}

func TestGivenTwoSummariesWhenComparedThenOnlyAPassingCaseThatFailsNowRegresses(t *testing.T) {
	before := Summary{Cases: []CaseRow{row("single", 1, 1), row("repeat", 3, 3), row("fixed", 0, 1)}}
	for _, tt := range []struct {
		name    string
		after   []CaseRow
		regress bool
		mark    string
	}{
		{"a lower rate that still passes is a note", []CaseRow{row("single", 2, 3), row("repeat", 2, 3), row("fixed", 3, 3)}, false, "lower rate, still passes"},
		{"a single-run pass that fails now regresses", []CaseRow{row("single", 0, 1), row("repeat", 3, 3), row("fixed", 0, 1)}, true, "REGRESSED"},
		{"a repeat pass below two thirds regresses", []CaseRow{row("single", 1, 1), row("repeat", 1, 3), row("fixed", 0, 1)}, true, "REGRESSED"},
		{"a case failing before and after is no regression", []CaseRow{row("single", 1, 1), row("repeat", 3, 3), row("fixed", 0, 3)}, false, ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var out bytes.Buffer
			got := compare(&out, before, Summary{Cases: tt.after})
			if got != tt.regress {
				t.Errorf("regressed = %v, want %v\n%s", got, tt.regress, out.String())
			}
			if tt.mark != "" && !strings.Contains(out.String(), tt.mark) {
				t.Errorf("output lacks %q:\n%s", tt.mark, out.String())
			}
			if !tt.regress && strings.Contains(out.String(), "REGRESSED") {
				t.Errorf("unexpected REGRESSED:\n%s", out.String())
			}
		})
	}
}
