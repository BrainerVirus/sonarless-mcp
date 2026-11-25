package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
)

// allowedTools is what the model may use: the skill, reading files in the
// project, the fake SonarQube MCP server's tools, and Bash for sonarless-mcp
// plus a few harmless commands. Everything else is denied (dontAsk), so a run
// cannot reach docker, the network, a real sonarless-mcp or files outside its
// world by another route.
var allowedTools = []string{
	"Skill", "Read(./**)", "mcp__sonarqube",
	"Bash(sonarless-mcp *)", "Bash(ls *)", "Bash(echo *)", "Bash(pwd)",
}

var disallowedTools = []string{"WebFetch", "WebSearch", "Task", "Agent"}

// Usage is the token count Claude Code reports for a run.
type Usage struct {
	Input         int `json:"input"`
	Output        int `json:"output"`
	CacheCreation int `json:"cacheCreation"`
	CacheRead     int `json:"cacheRead"`
}

// TotalInput is every prompt token, cached or not.
func (u Usage) TotalInput() int { return u.Input + u.CacheCreation + u.CacheRead }

// RunResult is one graded run of a case.
type RunResult struct {
	Run int `json:"run"`
	// Status is pass, fail, blocked (a host policy denied a plain
	// sonarless-mcp command; not graded) or error (the harness failed; not
	// graded). A run whose model never loaded the skill is graded all the
	// same, and SkillLoaded says so: with MCP tools at hand a model may skip
	// the skill, and that is part of what the bench measures.
	Status      string   `json:"status"`
	Failed      string   `json:"failed,omitempty"`
	Failures    []string `json:"failures,omitempty"`
	Reason      string   `json:"reason,omitempty"`
	Turns       int      `json:"turns"`
	Tokens      Usage    `json:"tokens"`
	CostUSD     float64  `json:"costUSD"`
	DurationMs  int64    `json:"durationMs"`
	SkillLoaded bool     `json:"skillLoaded"`
	// SkillListed: the init event lists the sonarless-mcp skill; MCPConnected:
	// it shows the fake MCP server connected; WorkitPlugin: it lists the
	// Workit plugin (it should not, the run drops user plugins).
	SkillListed  bool     `json:"skillListed"`
	MCPConnected bool     `json:"mcpConnected"`
	WorkitPlugin bool     `json:"workitPlugin,omitempty"`
	Commands     []string `json:"commands"`
	Calls        []Call   `json:"calls"`
	Denials      []string `json:"denials,omitempty"`
	Answer       string   `json:"answer"`
}

type runner struct {
	claude  string
	model   string
	skill   string // "" for the no-skill control
	fake    string // this executable, copied in as <harness>/bin/sonarless-mcp
	timeout time.Duration
	keep    bool
	logDir  string // scrubbed transcripts, not committed
}

// runCase runs one case once in a fresh world and grades it.
func (r *runner) runCase(ctx context.Context, c *Case, n int) (res RunResult) {
	res = RunResult{Run: n}
	world, err := os.MkdirTemp("", "sonarless-bench-")
	if err != nil {
		return errResult(res, err)
	}
	if !r.keep {
		defer os.RemoveAll(world)
	}
	project := filepath.Join(world, "project")
	harness := filepath.Join(world, "harness")
	if err := r.makeWorld(c, project, harness); err != nil {
		return errResult(res, err)
	}

	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	args := []string{
		"-p", "--model", r.model, "--output-format", "stream-json", "--verbose",
		"--max-turns", fmt.Sprint(c.MaxTurns),
		// Only the world's project settings: no user plugins or hooks (Workit's
		// gate included), no user skills, and no MCP server but the fake one.
		"--setting-sources", "project", "--mcp-config", filepath.Join(harness, "mcp.json"), "--strict-mcp-config",
		"--no-session-persistence", "--permission-mode", "dontAsk", "--max-budget-usd", "0.50",
		"--allowedTools",
	}
	args = append(args, allowedTools...)
	args = append(args, "--disallowedTools")
	args = append(args, disallowedTools...)
	// The claude binary is resolved on the caller's PATH; the run's own PATH
	// has no real sonarless-mcp.
	cmd := exec.CommandContext(ctx, r.claude, args...) //nolint:gosec // the claude binary and flags are the bench's own
	cmd.Dir = project
	cmd.Env = childEnv(os.Environ(), harness)
	cmd.Stdin = strings.NewReader(c.Prompt)
	var out, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &stderr
	start := time.Now()
	runErr := cmd.Run()
	res.DurationMs = time.Since(start).Milliseconds()

	sc := newScrubber(world)
	if r.logDir != "" {
		_ = os.MkdirAll(r.logDir, 0o750)
		_ = os.WriteFile(filepath.Join(r.logDir, fmt.Sprintf("%s.run%d.jsonl", c.ID, n)), []byte(sc.scrub(out.String())), 0o600)
	}
	t, err := parseTranscript(out.Bytes())
	if err != nil || !t.done {
		reason := fmt.Sprintf("no result event (exit: %v): %s", runErr, lastLine(stderr.String()))
		if err != nil {
			reason = err.Error()
		}
		res.Reason = sc.scrub(reason)
		res.Status = "error"
		return res
	}
	calls, err := readCalls(filepath.Join(harness, "calls.jsonl"))
	if err != nil {
		return errResult(res, err)
	}
	res.Turns, res.Tokens, res.CostUSD, res.SkillLoaded = t.turns, t.usage, t.cost, t.skillLoaded
	res.SkillListed, res.MCPConnected, res.WorkitPlugin = t.skillListed, t.mcpConnected, t.workitPlugin
	res.Commands, res.Calls, res.Denials, res.Answer = t.commands, calls, t.denials, t.answer
	defer func() { scrubRun(sc, &res) }()

	switch {
	case r.skill != "" && !t.skillListed:
		res.Status, res.Reason = "error", "the sonarless-mcp skill is not in the init event's skills"
	case r.skill == "" && t.skillListed:
		res.Status, res.Reason = "error", "the no-skill control lists a sonarless-mcp skill"
	case !t.mcpConnected:
		res.Status, res.Reason = "error", "the fake sonarqube MCP server did not connect"
	case t.isError && t.subtype != "error_max_turns":
		res.Status, res.Reason = "error", "claude: "+t.subtype
	case hostBlocked(t.denials):
		res.Status, res.Reason = "blocked", "a host policy denied a plain sonarless-mcp command"
	case ranSonarless(t.commands, t.denials) && cliCalls(calls) == 0:
		res.Status, res.Reason = "error", "sonarless-mcp ran but the fake logged nothing: a real sonarless-mcp may be first on PATH"
	default:
		res.Failures = Grade(c, calls, t.commands, t.answer)
		if t.subtype == "error_max_turns" {
			res.Failures = append(res.Failures, "max turns")
		}
		res.Status = "pass"
		if len(res.Failures) > 0 {
			res.Status, res.Failed = "fail", res.Failures[0]
		}
	}
	return res
}

// scrubRun replaces the world's paths and any token in what a run recorded.
func scrubRun(sc *scrubber, res *RunResult) {
	for i, c := range res.Commands {
		res.Commands[i] = sc.scrub(c)
	}
	for i, d := range res.Denials {
		res.Denials[i] = sc.scrub(d)
	}
	for _, c := range res.Calls {
		for j, a := range c.Argv {
			c.Argv[j] = sc.scrub(a)
		}
		for k, v := range c.Args {
			if s, ok := v.(string); ok {
				c.Args[k] = sc.scrub(s)
			}
		}
	}
	res.Answer, res.Reason = sc.scrub(res.Answer), sc.scrub(res.Reason)
}

func errResult(res RunResult, err error) RunResult {
	res.Status, res.Reason = "error", err.Error()
	return res
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return lines[len(lines)-1]
}

func (r *runner) makeWorld(c *Case, project, harness string) error {
	for _, d := range []string{filepath.Join(project, ".claude", "skills"), filepath.Join(harness, "bin"), filepath.Join(harness, "home")} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			return err
		}
	}
	if r.skill != "" {
		if err := copyTree(r.skill, filepath.Join(project, ".claude", "skills", "sonarless-mcp")); err != nil {
			return err
		}
	}
	for name, body := range c.Files {
		p := filepath.Join(project, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
			return err
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			return err
		}
	}
	b, err := json.Marshal(c)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(harness, "case.json"), b, 0o600); err != nil {
		return err
	}
	fake := filepath.Join(harness, "bin", "sonarless-mcp")
	if err := os.WriteFile(filepath.Join(harness, "mcp.json"), mcpConfig(fake), 0o600); err != nil {
		return err
	}
	return copyFile(r.fake, fake, 0o700)
}

// mcpConfig registers the fake as the `sonarqube` stdio MCP server, as
// `sonarless-mcp setup --stdio` registers the real one.
func mcpConfig(fake string) []byte {
	b, _ := json.Marshal(map[string]any{"mcpServers": map[string]any{
		"sonarqube": map[string]any{"type": "stdio", "command": fake, "args": []string{"mcp"}},
	}})
	return b
}

// childEnv is the parent environment without Workit's, sonarless-mcp's and
// docker's own variables (so a run never touches the calling session or a
// real install), with the fake first on PATH and every other directory that
// holds a sonarless-mcp dropped from it. Should a real sonarless-mcp run
// anyway, its home is in the harness, its docker socket does not exist and it
// neither upgrades nor checks for releases, so it cannot reach the user's
// containers.
func childEnv(parent []string, harness string) []string {
	var env []string
	for _, kv := range parent {
		k, v, _ := strings.Cut(kv, "=")
		switch {
		case strings.HasPrefix(k, "WORKIT_"), strings.HasPrefix(k, "SONARLESS_"), strings.HasPrefix(k, "DOCKER_"),
			k == "CLAUDECODE", k == "ANTHROPIC_API_KEY", k == "ANTHROPIC_AUTH_TOKEN":
			continue
		case k == "PATH":
			kv = "PATH=" + benchPath(harness, v)
		}
		env = append(env, kv)
	}
	return append(env,
		"SONARLESS_HOME="+filepath.Join(harness, "home"),
		"SONARLESS_AUTO_UPDATE=false",
		"SONARLESS_UPDATE_NOTICE=false",
		"DOCKER_HOST=unix://"+filepath.Join(harness, "no-docker.sock"),
	)
}

// benchPath puts the harness's bin first and drops every directory of path
// that holds a sonarless-mcp executable (the user's real install).
func benchPath(harness, path string) string {
	dirs := []string{filepath.Join(harness, "bin")}
	for _, d := range filepath.SplitList(path) {
		if d != "" && !hasSonarless(d) {
			dirs = append(dirs, d)
		}
	}
	return strings.Join(dirs, string(os.PathListSeparator))
}

func hasSonarless(dir string) bool {
	for _, name := range []string{"sonarless-mcp", "sonarless-mcp.exe"} {
		if fi, err := os.Stat(filepath.Join(dir, name)); err == nil && !fi.IsDir() {
			return true
		}
	}
	return false
}

type transcript struct {
	done, isError, skillLoaded bool
	skillListed, workitPlugin  bool
	mcpConnected               bool
	subtype, answer            string
	turns                      int
	cost                       float64
	usage                      Usage
	commands, denials          []string
}

func parseTranscript(b []byte) (transcript, error) {
	var t transcript
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(make([]byte, 0, 1<<20), 64<<20)
	for sc.Scan() {
		var ev struct {
			Type    string `json:"type"`
			Subtype string `json:"subtype"`
			Message struct {
				Content json.RawMessage `json:"content"`
			} `json:"message"`
			Skills     []string `json:"skills"`
			MCPServers []struct {
				Name   string `json:"name"`
				Status string `json:"status"`
			} `json:"mcp_servers"`
			Plugins []struct {
				Name string `json:"name"`
			} `json:"plugins"`
			Result            string  `json:"result"`
			IsError           bool    `json:"is_error"`
			NumTurns          int     `json:"num_turns"`
			TotalCostUSD      float64 `json:"total_cost_usd"`
			PermissionDenials []struct {
				ToolName  string         `json:"tool_name"`
				ToolInput map[string]any `json:"tool_input"`
			} `json:"permission_denials"`
			Usage struct {
				Input         int `json:"input_tokens"`
				Output        int `json:"output_tokens"`
				CacheCreation int `json:"cache_creation_input_tokens"`
				CacheRead     int `json:"cache_read_input_tokens"`
			} `json:"usage"`
		}
		if json.Unmarshal(sc.Bytes(), &ev) != nil {
			continue
		}
		switch ev.Type {
		case "system":
			if ev.Subtype == "init" {
				t.skillListed = slices.Contains(ev.Skills, "sonarless-mcp")
				for _, m := range ev.MCPServers {
					t.mcpConnected = t.mcpConnected || (m.Name == "sonarqube" && m.Status == "connected")
				}
				for _, p := range ev.Plugins {
					t.workitPlugin = t.workitPlugin || strings.Contains(p.Name, "workit")
				}
			}
		case "assistant":
			var blocks []struct {
				Type  string         `json:"type"`
				Name  string         `json:"name"`
				Input map[string]any `json:"input"`
			}
			_ = json.Unmarshal(ev.Message.Content, &blocks)
			for _, bl := range blocks {
				if bl.Type != "tool_use" {
					continue
				}
				switch bl.Name {
				case "Bash":
					t.commands = append(t.commands, fmt.Sprint(bl.Input["command"]))
				case "Skill":
					t.skillLoaded = t.skillLoaded || strings.Contains(fmt.Sprint(bl.Input["skill"]), "sonarless")
				case "Read":
					t.skillLoaded = t.skillLoaded ||
						strings.Contains(filepath.ToSlash(fmt.Sprint(bl.Input["file_path"])), "skills/sonarless-mcp/SKILL.md")
				}
			}
		case "result":
			t.done, t.isError, t.subtype, t.answer = true, ev.IsError, ev.Subtype, ev.Result
			t.turns, t.cost = ev.NumTurns, ev.TotalCostUSD
			t.usage = Usage{ev.Usage.Input, ev.Usage.Output, ev.Usage.CacheCreation, ev.Usage.CacheRead}
			for _, d := range ev.PermissionDenials {
				if c, ok := d.ToolInput["command"]; ok {
					t.denials = append(t.denials, fmt.Sprint(c))
				} else {
					t.denials = append(t.denials, d.ToolName)
				}
			}
		}
	}
	return t, sc.Err()
}

// plainSonarless is a single sonarless-mcp command with nothing the allow
// rule Bash(sonarless-mcp *) could refuse: no pipe, chain, redirect or
// expansion.
var plainSonarless = regexp.MustCompile("^\\s*sonarless-mcp\\s[^|;&<>$`\\n]*$")

// hostBlocked: the bench allows a plain sonarless-mcp command, so its denial
// came from a host policy (a hook or managed setting), not from the skill.
func hostBlocked(denials []string) bool {
	for _, d := range denials {
		if plainSonarless.MatchString(d) {
			return true
		}
	}
	return false
}

var sonarlessWord = regexp.MustCompile(`(^|[\s;&|(])sonarless-mcp(\s|$)`)

func ranSonarless(commands, denials []string) bool {
	denied := map[string]bool{}
	for _, d := range denials {
		denied[d] = true
	}
	for _, c := range commands {
		if !denied[c] && sonarlessWord.MatchString(c) {
			return true
		}
	}
	return false
}

// cliCalls counts the fake's CLI calls (not its MCP tool calls).
func cliCalls(calls []Call) int {
	n := 0
	for _, c := range calls {
		if c.Tool == "" {
			n++
		}
	}
	return n
}

func readCalls(path string) ([]Call, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var calls []Call
	for _, l := range bytes.Split(bytes.TrimSpace(b), []byte("\n")) {
		var c Call
		if json.Unmarshal(l, &c) == nil {
			calls = append(calls, c)
		}
	}
	return calls, nil
}

func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o750)
		}
		return copyFile(p, target, 0o600)
	})
}

func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}
