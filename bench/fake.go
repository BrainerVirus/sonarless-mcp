package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/BrainerVirus/sonarless-mcp/internal/cli"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// The fake sonarless-mcp is this binary run under that name from a world's
// harness/bin. It parses each command line with the real command tree
// (internal/cli: commands, flags, help, the flag typo hint) but never runs a
// command: it answers from the case's fixtures, then
// from built-in replies, and logs each call to harness/calls.jsonl. It starts
// no container, calls no docker and opens no network connection; `mcp` serves
// a fake SonarQube MCP server on stdio (fakemcp.go).

// parsed is a command line the fake understood.
type parsed struct {
	cmd   *cobra.Command
	path  string            // the command's path below the root, e.g. "remote add"
	flags map[string]string // flags set on the command line, by long name
	args  []string
	help  bool
}

// usageError is a command line the real binary would refuse.
type usageError struct {
	cmd  *cobra.Command
	err  error
	argv []string
}

func (e *usageError) Error() string { return e.err.Error() }

// parseArgv resolves argv with the real command tree, the way cobra's
// ExecuteC does (find the command, parse its flags, validate its arguments),
// without running anything.
func parseArgv(argv []string) (*parsed, error) {
	root := cli.NewRootCommand(context.Background())
	root.InitDefaultHelpCmd()
	root.InitDefaultCompletionCmd(argv...)
	cmd, rest, err := root.Find(argv)
	if err != nil {
		return nil, &usageError{cmd, err, argv}
	}
	cmd.InitDefaultHelpFlag()
	cmd.InitDefaultVersionFlag()
	if err := cmd.ParseFlags(rest); err != nil {
		return nil, &usageError{cmd, err, argv}
	}
	p := &parsed{cmd: cmd, path: strings.TrimSpace(strings.TrimPrefix(cmd.CommandPath(), root.Name())), flags: map[string]string{}}
	p.help, _ = cmd.Flags().GetBool("help")
	cmd.Flags().Visit(func(f *pflag.Flag) { p.flags[f.Name] = f.Value.String() })
	p.args = cmd.Flags().Args()
	if !p.help {
		if err := cmd.ValidateArgs(p.args); err != nil {
			return nil, &usageError{cmd, err, argv}
		}
	}
	return p, nil
}

// usageText is what the real binary prints for a usage error (see
// internal/cli's Main).
func usageText(e *usageError) string {
	var b strings.Builder
	fmt.Fprintln(&b, "error:", e.err)
	cli.PrintUsageHint(&b, e.cmd, e.err, e.argv)
	return b.String()
}

// helpText is the command's real --help.
func (p *parsed) helpText() string {
	var b strings.Builder
	p.cmd.SetOut(&b)
	p.cmd.SetErr(&b)
	_ = p.cmd.Help()
	return b.String()
}

// fakeReply is a built-in answer to a parsed command line.
type fakeReply struct {
	stdout, stderr string
	exit           int
}

// builtinReply answers a command the case's fixtures did not.
func builtinReply(p *parsed) fakeReply {
	switch p.path {
	case "":
		if p.flags["version"] == "true" {
			return fakeReply{stdout: "sonarless-mcp version v0.14.2\n"}
		}
		return fakeReply{stdout: p.helpText()}
	case "version":
		return fakeReply{stdout: "sonarless-mcp v0.14.2 (commit 1a2b3c4, built 2026-11-02, go1.27.4, linux/amd64)\n"}
	case "scan":
		return fakeReply{stdout: "Scanning with the cli scanner (project demo-app)...\nAnalysis done in 48s.\nQuality gate: PASSED\nDashboard: http://localhost:9234/dashboard?id=demo-app\n"}
	case "project":
		return fakeReply{stdout: "Project key:  demo-app (sonar-project.properties)\nProject name: Demo App (sonar-project.properties)\nScanner:      cli\nServers:      local (scanned), work (found)\n"}
	case "status":
		return fakeReply{stdout: "SonarQube server  running  1.9 GiB\nMCP (local)       running  210 MiB\nMCP (work)        running  190 MiB\nGateway           running  2 sessions\nScans             none running\nRemotes           work  http://sonar.example.com  reachable\nIdle stop         after 30m unused\n"}
	case "upgrade":
		if p.flags["check"] != "" {
			return fakeReply{stdout: "sonarless-mcp v0.14.2 is installed; v0.15.0 is available.\nRun 'sonarless-mcp upgrade' to install it (installed binaries also upgrade themselves a day after a release).\n"}
		}
		return fakeReply{stdout: "Upgraded sonarless-mcp v0.14.2 -> v0.15.0.\n"}
	case "skill list":
		return fakeReply{stdout: "DIR                         VERSION  STATE    UPDATES\n~/.claude/skills            0.14.2   current  auto\n~/.config/opencode/skills   0.12.0   stale    auto\n" +
			"Run `sonarless-mcp skill sync` to update the stale copies in your home directory now.\n"}
	case "skill sync":
		return fakeReply{stdout: "Updated 1 copy: ~/.config/opencode/skills/sonarless-mcp (v0.12.0 -> v0.14.2)\n"}
	case "remote list":
		return fakeReply{stdout: "NAME  URL                        BRANCH   REACHABLE\nwork  http://sonar.example.com   develop  yes\n"}
	case "remote add", "remote update":
		if p.path == "remote add" && p.flags["url"] == "" {
			return fakeReply{stderr: "error: --url is required\n", exit: 1}
		}
		if p.path == "remote add" || p.flags["token"] != "" {
			if p.flags["token-stdin"] != "" {
				return fakeReply{stderr: "error: no token on stdin\n", exit: 1}
			}
			return fakeReply{stderr: "error: the token prompt needs a terminal: run this in your own terminal (the token never goes through an agent)\n", exit: 1}
		}
		return fakeReply{stdout: "Updated remote " + strings.Join(p.args, " ") + ".\n"}
	case "remote", "skill":
		return fakeReply{stdout: p.helpText()}
	}
	return fakeReply{stdout: "done\n"}
}

// fakeMain is main for the fake sonarless-mcp.
func fakeMain(argv []string) int {
	harness, c, err := loadHarness()
	if err != nil {
		fmt.Fprintln(os.Stderr, "fake sonarless-mcp:", err)
		return 1
	}
	if len(argv) > 0 && argv[0] == "mcp" {
		return serveFakeMCP(harness, c, os.Stdin, os.Stdout)
	}
	r := answerCLI(c, argv)
	fmt.Fprint(os.Stdout, r.stdout)
	fmt.Fprint(os.Stderr, r.stderr)
	if err := logCall(harness, Call{Argv: argv, Exit: r.exit}); err != nil {
		fmt.Fprintln(os.Stderr, "fake sonarless-mcp:", err)
		return 1
	}
	return r.exit
}

// answerCLI answers a command line: a usage error, --help, a fixture rule or
// a built-in reply.
func answerCLI(c *Case, argv []string) fakeReply {
	p, err := parseArgv(argv)
	if err != nil {
		return fakeReply{stderr: usageText(err.(*usageError)), exit: 1}
	}
	if p.help {
		return fakeReply{stdout: p.helpText()}
	}
	if r, ok := fixtureReply(c, Call{Argv: argv}.Line()); ok {
		return r
	}
	return builtinReply(p)
}

func fixtureReply(c *Case, line string) (fakeReply, bool) {
	for _, r := range c.Fixtures.Rules {
		if regexp.MustCompile(r.Match).MatchString(line) {
			return fakeReply{stdout: r.Stdout, stderr: r.Stderr, exit: r.Exit}, true
		}
	}
	return fakeReply{}, false
}

// loadHarness finds the world's harness (the fake lives in <harness>/bin) and
// its case.
func loadHarness() (string, *Case, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", nil, err
	}
	harness := filepath.Dir(filepath.Dir(exe))
	b, err := os.ReadFile(filepath.Join(harness, "case.json"))
	if err != nil {
		return "", nil, err
	}
	var c Case
	if err := json.Unmarshal(b, &c); err != nil {
		return "", nil, err
	}
	return harness, &c, nil
}

func logCall(harness string, call Call) error {
	b, err := json.Marshal(call)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(harness, "calls.jsonl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, err = f.Write(append(b, '\n'))
	return closeWith(err, f)
}

func closeWith(err error, c io.Closer) error {
	if cerr := c.Close(); err == nil {
		err = cerr
	}
	return err
}
