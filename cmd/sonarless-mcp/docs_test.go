package main

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// repoRoot is the module root, two levels above this package.
const repoRoot = "../.."

// Every SONARLESS_* variable the code reads must be in `sonarless-mcp --help`.
func TestRootHelpListsEveryEnvVar(t *testing.T) {
	help := newRootCommand(context.Background()).Long
	literal := regexp.MustCompile(`"(SONARLESS_[A-Z0-9_]+)"`)
	found := map[string]bool{}
	for _, dir := range []string{"cmd", "internal"} {
		err := filepath.WalkDir(filepath.Join(repoRoot, dir), func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
				return err
			}
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			for _, m := range literal.FindAllStringSubmatch(string(b), -1) {
				found[m[1]] = true
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(found) < 25 {
		t.Fatalf("found only %d SONARLESS_* variables in the source; the scanner is broken", len(found))
	}
	for name := range found {
		if !regexp.MustCompile(`(?m)^  ` + name + `$`).MatchString(help) {
			t.Errorf("sonarless-mcp --help does not list %s", name)
		}
	}
}

// docFiles are the markdown files whose command lines must match the CLI.
func docFiles(t *testing.T) []string {
	t.Helper()
	files := []string{"README.md", "AGENTS.md", "CONTRIBUTING.md", "SECURITY.md"}
	for _, dir := range []string{"docs", filepath.Join("internal", "clients", "skill")} {
		_ = filepath.WalkDir(filepath.Join(repoRoot, dir), func(p string, d fs.DirEntry, err error) error {
			if err == nil && !d.IsDir() && strings.HasSuffix(p, ".md") {
				rel, _ := filepath.Rel(repoRoot, p)
				files = append(files, rel)
			}
			return nil
		})
	}
	return files
}

// Every `sonarless-mcp …` command line in the README, docs/ and the agent
// skill must name real commands and flags: a renamed or removed one fails
// here, with the file and line, not in a user's terminal.
func TestDocsNameOnlyRealCommandsAndFlags(t *testing.T) {
	checked := 0
	for _, f := range docFiles(t) {
		b, err := os.ReadFile(filepath.Join(repoRoot, f))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		n, problems := docDrift(filepath.ToSlash(f), string(b))
		checked += n
		for _, p := range problems {
			t.Error(p)
		}
	}
	if checked < 60 {
		t.Errorf("found only %d sonarless-mcp command lines in the docs; the scanner is broken", checked)
	}
}

func TestDocDriftReportsFileAndLine(t *testing.T) {
	for text, want := range map[string]string{
		"# x\n\n```sh\nsonarless-mcp scan --tets\n```\n":                     `x.md:4: sonarless-mcp scan --tets: unknown flag: --tets`,
		"# x\r\n\r\n```sh\r\nsonarless-mcp scan --tets\r\n```\r\n":           `x.md:4: sonarless-mcp scan --tets: unknown flag: --tets`,
		"one\ntwo `sonarless-mcp remote lst` three\n":                        `x.md:2: sonarless-mcp remote lst: unknown command "lst" for "sonarless-mcp remote"`,
		"```\nok\n```\n\nRun `sonarless-mcp stats`.\n":                       `x.md:5: sonarless-mcp stats: unknown command "stats" for "sonarless-mcp"`,
		"Use `--no-such-flag`.\n":                                            `x.md:1: --no-such-flag is not a flag of any sonarless-mcp command`,
		"```sh\nsonarless-mcp skill install --dir x --jsn  # comment\n```\n": `x.md:2: sonarless-mcp skill install --dir x --jsn: unknown flag: --jsn`,
	} {
		t.Run(want, func(t *testing.T) {
			_, problems := docDrift("x.md", text)
			if len(problems) != 1 || problems[0] != want {
				t.Errorf("problems = %q, want [%q]", problems, want)
			}
		})
	}
	t.Run("placeholders, comments, other commands and real flags pass", func(t *testing.T) {
		text := "```sh\n" +
			"sonarless-mcp remote add <name> --url <url> [--branch <branch>]   # --bogus in a comment\n" +
			"sonarless-mcp scan -D sonar.exclusions=**/gen/** --tests\n" +
			"cosign verify-blob --bundle checksums.txt.sigstore.json checksums.txt\n" +
			"```\n" +
			"`sonarless-mcp <command> --help`, `sonarless-mcp status [--json]`, `--allow-unsigned`, " +
			"`claude mcp add -s user sonarqube -- /bin/sonarless-mcp mcp`, `sonarless-mcp remote list|remove <name>`\n"
		if n, problems := docDrift("x.md", text); n != 5 || len(problems) != 0 {
			t.Errorf("command lines %d, problems %q", n, problems)
		}
	})
}

var (
	fenceLine  = regexp.MustCompile("^\\s*```")
	inlineCode = regexp.MustCompile("`([^`\n]+)`")
	flagWord   = regexp.MustCompile(`^(--[a-z][a-z0-9-]*|-[A-Za-z])(=.*)?$`)
)

// docDrift checks every sonarless-mcp command line in a markdown text's
// fenced blocks and inline code spans against the real command tree. It
// returns how many command lines it checked and one "file:line: …" problem
// per unknown command or flag.
func docDrift(file, text string) (int, []string) {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	checked := 0
	var problems []string
	inFence := false
	for i, line := range strings.Split(text, "\n") {
		if fenceLine.MatchString(line) {
			inFence = !inFence
			continue
		}
		var snippets []string
		if inFence {
			snippets = []string{line}
		} else {
			for _, m := range inlineCode.FindAllStringSubmatch(line, -1) {
				snippets = append(snippets, m[1])
			}
		}
		for _, s := range snippets {
			words := shellWords(s)
			start := commandStart(words)
			if start < 0 {
				// A flag named on its own, outside a fenced block, must
				// exist on some command.
				if !inFence && len(words) == 1 {
					if m := flagWord.FindStringSubmatch(words[0]); m != nil && !anyCommandHas(newRootCommand(context.Background()), m[1]) {
						problems = append(problems, fmt.Sprintf("%s:%d: %s is not a flag of any sonarless-mcp command", file, i+1, m[1]))
					}
				}
				continue
			}
			checked++
			args := words[start+1:]
			if err := checkCommandLine(args); err != nil {
				msg, _, _ := strings.Cut(err.Error(), "\n") // drop cobra's suggestions
				problems = append(problems, fmt.Sprintf("%s:%d: %s: %s", file, i+1, strings.Join(words[start:], " "), msg))
			}
		}
	}
	return checked, problems
}

// checkCommandLine resolves args against a fresh command tree with Find and
// parses the rest with the resolved command's flags. A <placeholder> in a
// command position ends the resolution.
func checkCommandLine(args []string) error {
	root := newRootCommand(context.Background())
	root.InitDefaultHelpCmd()
	root.InitDefaultCompletionCmd()
	cmdArgs := args
	if i := slices.IndexFunc(args, isPlaceholder); i >= 0 {
		cmdArgs = args[:i]
	}
	cmd, rest, err := root.Find(cmdArgs)
	if err != nil {
		return err
	}
	rest = append(rest, args[len(cmdArgs):]...)
	if cmd.HasSubCommands() && !cmd.Runnable() {
		for _, w := range rest {
			if strings.HasPrefix(w, "-") {
				continue
			}
			if !isPlaceholder(w) {
				return fmt.Errorf("unknown command %q for %q", w, cmd.CommandPath())
			}
			break
		}
	}
	cmd.InitDefaultHelpFlag()
	cmd.InitDefaultVersionFlag()
	return cmd.ParseFlags(rest)
}

var envAssign = regexp.MustCompile(`^[A-Z_][A-Z0-9_]*=`)

// commandStart is the index of "sonarless-mcp" when a line runs it: first,
// after a "$" prompt or after VAR=value assignments; -1 otherwise.
func commandStart(words []string) int {
	for i, w := range words {
		switch {
		case w == "sonarless-mcp":
			return i
		case w == "$" || envAssign.MatchString(w):
		default:
			return -1
		}
	}
	return -1
}

func isPlaceholder(w string) bool { return strings.HasPrefix(w, "<") || w == "…" || w == "..." }

func anyCommandHas(cmd *cobra.Command, flag string) bool {
	cmd.InitDefaultHelpFlag()
	cmd.InitDefaultVersionFlag()
	for _, set := range []*pflag.FlagSet{cmd.Flags(), cmd.PersistentFlags()} {
		if name, ok := strings.CutPrefix(flag, "--"); ok {
			if set.Lookup(name) != nil {
				return true
			}
		} else if set.ShorthandLookup(flag[1:]) != nil {
			return true
		}
	}
	return slices.ContainsFunc(cmd.Commands(), func(c *cobra.Command) bool { return anyCommandHas(c, flag) })
}

// shellWords splits a command line into words as a POSIX shell would for
// quoting, drops the [ ] around optional parts and the backticks around
// inline commands in program output, and stops at a comment, pipe, list
// operator or redirection.
func shellWords(line string) []string {
	var words []string
	var cur strings.Builder
	inWord := false
	var quote rune
	flush := func() {
		if inWord {
			if w := strings.Trim(cur.String(), "[]`"); w != "" {
				words = append(words, w)
			}
			cur.Reset()
			inWord = false
		}
	}
	for _, r := range line {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
			} else {
				cur.WriteRune(r)
			}
		case r == '\'' || r == '"':
			quote, inWord = r, true
		case r == ' ' || r == '\t':
			flush()
		case r == '|' || r == ';' || r == '&' || r == '(' || (!inWord && (r == '#' || r == '>')):
			flush()
			return words
		default:
			cur.WriteRune(r)
			inWord = true
		}
	}
	flush()
	return words
}
