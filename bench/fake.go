package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// The fake sonarless-mcp is this binary run under that name from a world's
// harness/bin. It knows the real command tree (commands, flags, the flag typo
// hint) but never runs a command: it answers from the case's fixtures, then
// from built-in replies, and logs each call to harness/calls.jsonl. It starts
// no container, calls no docker and opens no network connection; `mcp` serves
// a fake SonarQube MCP server on stdio (fakemcp.go).

// flagSpec is one flag of a command.
type flagSpec struct {
	name, short string
	value       bool // takes a value
	help        string
}

// cmdSpec is one command of the tree, by its path below the root.
type cmdSpec struct {
	path  string
	short string
	use   string
	flags []flagSpec
}

// commands mirrors cmd/sonarless-mcp (TestGivenTheFakeCommandTreeThenEachCommandAndFlagIsInTheRealSource
// checks every name against that source).
var commands = []cmdSpec{
	{path: "", short: "Local SonarQube for any project, plus a shared MCP server for AI agents", use: "sonarless-mcp [command]",
		flags: []flagSpec{{name: "version", short: "v", help: "version for sonarless-mcp"}}},
	{path: "start", short: "Start (or create) the shared SonarQube server"},
	{path: "reset", short: "Delete the current SonarQube version's scan history and start fresh",
		flags: []flagSpec{{name: "yes", help: "confirm deleting this version's scan history"}}},
	{path: "results", use: "results [dir]", short: "Print the project's key metrics and save them as JSON",
		flags: []flagSpec{{name: "output", short: "o", value: true, help: "JSON output file (default: SONARLESS_METRICS_FILE in the project root)"}}},
	{path: "project", use: "project [dir]", short: "Show the detected project key, name, scanner and where they came from"},
	{path: "config", use: "config [dir]", short: "Show effective settings and where each comes from"},
	{path: "mcp", use: "mcp [workspace]", short: "MCP stdio server for AI clients (shares one SonarQube MCP for all of them)"},
	{path: "mcp-config", use: "mcp-config <claude|opencode|cursor|vscode|codex>", short: "Print how to register sonarless-mcp in an AI client (direct HTTP or the stdio bridge)"},
	{path: "scan", use: "scan [dir]", short: "Scan a project (auto-detects key and maven/gradle/dotnet/cli scanner)",
		flags: []flagSpec{
			{name: "scanner", value: true, help: "force scanner: cli, maven, gradle or dotnet"},
			{name: "tests", help: "run tests during build-based scans (for coverage)"},
			{name: "define", short: "D", value: true, help: "extra analysis property, e.g. -D sonar.exclusions=**/gen/**"},
			{name: "fail-on-gate", help: "exit 2 when the quality gate fails (for CI and scripts)"},
		}},
	{path: "setup", short: "Register sonarless-mcp in your AI clients (detects what's installed)",
		flags: []flagSpec{
			{name: "yes", short: "y", help: "no picker: use every detected (or, with --remove, configured) client"},
			{name: "clients", value: true, help: "no picker: these clients (claude,cursor,opencode,vscode,codex)"},
			{name: "remove", help: "unregister instead of register"},
			{name: "list", help: "only show which clients are detected/configured; change nothing"},
			{name: "stdio", help: "register the stdio bridge (`sonarless-mcp mcp`) everywhere instead of direct HTTP"},
		}},
	{path: "status", short: "Show containers (with memory), gateway, stdio bridges, running scans and idle-stop state",
		flags: []flagSpec{{name: "json", help: "print the status as JSON"}}},
	{path: "stop", short: "Stop the SonarQube server and MCP containers (--all: the gateway too)",
		flags: []flagSpec{{name: "all", help: "also stop the gateway"}}},
	{path: "version", short: "Show the sonarless-mcp version, commit, build date, Go version and platform",
		flags: []flagSpec{{name: "json", help: "print the version information as JSON"}}},
	{path: "upgrade", short: "Upgrade sonarless-mcp to the latest release and refresh the MCP image",
		flags: []flagSpec{
			{name: "check", help: "only report whether a newer release exists; change nothing"},
			{name: "version", value: true, help: "install this release tag (e.g. v0.9.0) instead of the latest"},
			{name: "allow-unsigned", help: "with --version: install an older release published before signing, on its checksum alone"},
		}},
	{path: "serve", short: "Run the gateway AI clients connect to over HTTP (started at login by setup)"},
	{path: "ensure", short: "Start the gateway unless it is running (cheap when it is)",
		flags: []flagSpec{{name: "headers", help: "print the client's auth headers as JSON (Claude Code headersHelper)"}}},
	{path: "remote", short: "Manage remote SonarQube servers the MCP tools can also query"},
	{path: "remote add", use: "remote add <name> --url <url>", short: "Add or update a remote (token from --token-stdin, $SONARLESS_REMOTE_TOKEN or a prompt)",
		flags: []flagSpec{
			{name: "url", value: true, help: "SonarQube URL, e.g. https://sonar.example.com"},
			{name: "branch", value: true, help: "default branch for tools that take one (e.g. develop)"},
			{name: "token-stdin", help: "read the token from stdin"},
		}},
	{path: "remote update", use: "remote update <name> [--url <url>] [--branch <branch>] [--token]", short: "Change a remote's URL, branch and/or token, keeping everything else",
		flags: []flagSpec{
			{name: "url", value: true, help: "new SonarQube URL"},
			{name: "branch", value: true, help: "new default branch (\"\" for the server's main branch)"},
			{name: "token", help: "replace the token (hidden prompt)"},
			{name: "token-stdin", help: "replace the token, read from stdin"},
		}},
	{path: "remote list", short: "List remotes"},
	{path: "remote remove", use: "remote remove <name>", short: "Remove a remote, its token and its MCP container"},
	{path: "remote import", use: "remote import <name>", short: "Add a remote from a SonarQube MCP server already configured in an AI client",
		flags: []flagSpec{{name: "branch", value: true, help: "default branch for tools that take one (e.g. develop)"}}},
	{path: "remote sync", use: "remote sync <name> [dir]", short: "Make local scans judge the project like the remote (quality gate, rule sets, new code)"},
	{path: "skill", short: "Print, install and keep up to date the sonarless-mcp agent skill"},
	{path: "skill print", short: "Print the skill's SKILL.md"},
	{path: "skill install", use: "skill install [--dir <skills-dir>]", short: "Install the skill into agents' skills directories",
		flags: []flagSpec{
			{name: "dir", value: true, help: "agent skills directory to install into, e.g. ~/.claude/skills (default: every detected client's)"},
			{name: "json", help: "print {path, version} as JSON, one object per line"},
		}},
	{path: "skill list", short: "List the installed copies of the skill",
		flags: []flagSpec{{name: "json", help: "print the copies as a JSON array"}}},
	{path: "skill sync", short: "Update every installed copy of the skill now",
		flags: []flagSpec{{name: "force", help: "also rewrite copies a newer sonarless-mcp installed"}}},
	{path: "skill uninstall", use: "skill uninstall --dir <skills-dir>", short: "Remove the skill from a skills directory",
		flags: []flagSpec{{name: "dir", value: true, help: "agent skills directory to remove the skill from"}}},
}

// aliases are command names cobra also accepts.
var aliases = map[string]string{"update": "upgrade"}

func findCmd(path string) *cmdSpec {
	for i := range commands {
		if commands[i].path == path {
			return &commands[i]
		}
	}
	return nil
}

func children(path string) []*cmdSpec {
	var out []*cmdSpec
	for i := range commands {
		p := commands[i].path
		parent, _, _ := strings.Cut(p, " ")
		if path == "" && p != "" && !strings.Contains(p, " ") || path != "" && parent == path && p != path {
			out = append(out, &commands[i])
		}
	}
	return out
}

func lastWord(path string) string {
	f := strings.Fields(path)
	return f[len(f)-1]
}

// parsed is a command line the fake understood.
type parsed struct {
	cmd   *cmdSpec
	flags map[string]string // set flags by long name ("true" for a bool)
	args  []string
	help  bool
}

// usageError is what cobra or pflag would print for a bad command line.
type usageError struct {
	cmd *cmdSpec
	msg string
}

func (e *usageError) Error() string { return e.msg }

// parseArgv resolves argv to a command and its flags the way cobra does:
// leading words pick the subcommand, the rest are flags and arguments.
func parseArgv(argv []string) (*parsed, error) {
	cmd := findCmd("")
	rest := argv
	for len(rest) > 0 && !strings.HasPrefix(rest[0], "-") {
		word := rest[0]
		if a, ok := aliases[word]; ok && cmd.path == "" {
			word = a
		}
		next := strings.TrimSpace(cmd.path + " " + word)
		if c := findCmd(next); c != nil {
			cmd, rest = c, rest[1:]
			continue
		}
		if cmd.path == "" {
			return nil, &usageError{cmd, fmt.Sprintf("unknown command %q for %q", rest[0], "sonarless-mcp")}
		}
		break
	}
	p := &parsed{cmd: cmd, flags: map[string]string{}}
	for i := 0; i < len(rest); i++ {
		a := rest[i]
		switch {
		case a == "--":
			p.args = append(p.args, rest[i+1:]...)
			return p, nil
		case a == "--help" || a == "-h":
			p.help = true
		case strings.HasPrefix(a, "--"):
			name, val, hasVal := strings.Cut(a[2:], "=")
			f := lookupFlag(cmd, name, "")
			if f == nil {
				return nil, &usageError{cmd, "unknown flag: --" + name}
			}
			switch {
			case f.value && !hasVal:
				if i+1 >= len(rest) {
					return nil, &usageError{cmd, "flag needs an argument: --" + name}
				}
				i++
				val = rest[i]
			case !f.value && !hasVal:
				val = "true"
			}
			p.flags[f.name] = val
		case strings.HasPrefix(a, "-") && len(a) > 1:
			short := a[1:2]
			f := lookupFlag(cmd, "", short)
			if f == nil {
				return nil, &usageError{cmd, fmt.Sprintf("unknown shorthand flag: %q in %s", short, a)}
			}
			val := "true"
			if f.value {
				val = strings.TrimPrefix(a[2:], "=")
				if val == "" {
					if i+1 >= len(rest) {
						return nil, &usageError{cmd, "flag needs an argument: -" + short}
					}
					i++
					val = rest[i]
				}
			}
			p.flags[f.name] = val
		default:
			p.args = append(p.args, a)
		}
	}
	return p, nil
}

func lookupFlag(cmd *cmdSpec, name, short string) *flagSpec {
	for i := range cmd.flags {
		f := &cmd.flags[i]
		if name != "" && f.name == name || short != "" && f.short == short {
			return f
		}
	}
	return nil
}

// nearFlags is cmd/sonarless-mcp/hints.go's rule: flags that start with name
// (two letters or more), contain it or are contained in it (three or more),
// or are a few edits from it (one edit per three letters, at most two).
func nearFlags(cmd *cmdSpec, name string) []string {
	var near []string
	for _, f := range cmd.flags {
		if len(near) == 3 {
			break
		}
		n := f.name
		edits := min(len(name), len(n)) / 3
		if (len(name) >= 2 && strings.HasPrefix(n, name)) ||
			(len(name) >= 3 && strings.Contains(n, name)) ||
			(len(n) >= 3 && strings.Contains(name, n)) ||
			(edits > 0 && editDistance(n, name) <= min(edits, 2)) {
			near = append(near, n)
		}
	}
	return near
}

func editDistance(a, b string) int {
	d := make([][]int, len(a)+1)
	for i := range d {
		d[i] = make([]int, len(b)+1)
		d[i][0] = i
	}
	for j := range d[0] {
		d[0][j] = j
	}
	for i := 1; i <= len(a); i++ {
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			d[i][j] = min(d[i-1][j]+1, d[i][j-1]+1, d[i-1][j-1]+cost)
			if i > 1 && j > 1 && a[i-1] == b[j-2] && a[i-2] == b[j-1] {
				d[i][j] = min(d[i][j], d[i-2][j-2]+1)
			}
		}
	}
	return d[len(a)][len(b)]
}

func commandPath(c *cmdSpec) string { return strings.TrimSpace("sonarless-mcp " + c.path) }

// usageText is what the real binary prints for a usage error.
func usageText(e *usageError) string {
	var b strings.Builder
	fmt.Fprintln(&b, "error:", e.msg)
	if name, ok := strings.CutPrefix(e.msg, "unknown flag: --"); ok {
		if near := nearFlags(e.cmd, name); len(near) > 0 {
			fmt.Fprintln(&b, "did you mean --"+strings.Join(near, " or --")+"?")
		}
	}
	fmt.Fprintf(&b, "Run '%s --help' for usage.\n", commandPath(e.cmd))
	return b.String()
}

func helpText(c *cmdSpec) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n\nUsage:\n", c.short)
	use := c.use
	if use == "" {
		use = c.path
	}
	if c.path == "" {
		fmt.Fprintf(&b, "  %s\n", use)
	} else {
		fmt.Fprintf(&b, "  sonarless-mcp %s [flags]\n", use)
	}
	if kids := children(c.path); len(kids) > 0 {
		fmt.Fprintln(&b, "\nAvailable Commands:")
		for _, k := range kids {
			fmt.Fprintf(&b, "  %-14s %s\n", lastWord(k.path), k.short)
		}
	}
	fmt.Fprintln(&b, "\nFlags:")
	for _, f := range c.flags {
		short := "    "
		if f.short != "" {
			short = "-" + f.short + ", "
		}
		v := ""
		if f.value {
			v = " string"
		}
		fmt.Fprintf(&b, "  %s--%-18s %s\n", short, f.name+v, f.help)
	}
	fmt.Fprintf(&b, "  -h, --%-18s help for %s\n", "help", lastWordOr(c.path, "sonarless-mcp"))
	if c.path == "" {
		fmt.Fprintln(&b, "\nExit status: 0 on success, 1 on any error (a usage error names the flag you")
		fmt.Fprintln(&b, "probably meant), 2 when scan --fail-on-gate finds a failing quality gate.")
	}
	return b.String()
}

func lastWordOr(path, root string) string {
	if path == "" {
		return root
	}
	return lastWord(path)
}

// fakeReply is a built-in answer to a parsed command line.
type fakeReply struct {
	stdout, stderr string
	exit           int
}

// builtinReply answers a command the case's fixtures did not.
func builtinReply(p *parsed) fakeReply {
	switch p.cmd.path {
	case "":
		if p.flags["version"] != "" {
			return fakeReply{stdout: "sonarless-mcp version v0.14.2\n"}
		}
		return fakeReply{stdout: helpText(p.cmd)}
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
		return fakeReply{stdout: "PATH                                       VERSION  STATE\n~/.claude/skills/sonarless-mcp              v0.14.2  current\n~/.config/opencode/skills/sonarless-mcp    v0.12.0  outdated (sonarless-mcp skill sync updates it)\n"}
	case "skill sync":
		return fakeReply{stdout: "Updated 1 copy: ~/.config/opencode/skills/sonarless-mcp (v0.12.0 -> v0.14.2)\n"}
	case "remote list":
		return fakeReply{stdout: "NAME  URL                        BRANCH   REACHABLE\nwork  http://sonar.example.com   develop  yes\n"}
	case "remote add", "remote update":
		if p.cmd.path == "remote add" && p.flags["url"] == "" {
			return fakeReply{stderr: "error: --url is required\n", exit: 1}
		}
		if p.cmd.path == "remote add" || p.flags["token"] != "" {
			if p.flags["token-stdin"] != "" {
				return fakeReply{stderr: "error: no token on stdin\n", exit: 1}
			}
			return fakeReply{stderr: "error: the token prompt needs a terminal: run this in your own terminal (the token never goes through an agent)\n", exit: 1}
		}
		return fakeReply{stdout: "Updated remote " + strings.Join(p.args, " ") + ".\n"}
	case "remote", "skill":
		return fakeReply{stdout: helpText(p.cmd)}
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
		var ue *usageError
		if e, ok := err.(*usageError); ok {
			ue = e
		}
		return fakeReply{stderr: usageText(ue), exit: 1}
	}
	if p.help {
		return fakeReply{stdout: helpText(p.cmd)}
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
