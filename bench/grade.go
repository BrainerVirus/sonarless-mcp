package main

import (
	"encoding/json"
	"regexp"
	"strings"
)

// Call is one call the fake sonarless-mcp answered, as it logged it: a CLI
// run (Argv) or an MCP tool call (Tool and its Args).
type Call struct {
	Argv []string       `json:"argv,omitempty"`
	Tool string         `json:"tool,omitempty"`
	Args map[string]any `json:"args,omitempty"`
	Exit int            `json:"exit"`
}

// Line is what the rules match. A CLI run reads "sonarless-mcp <argv>": words
// joined by single spaces, without shell quoting, --flag=value written as
// --flag value. A tool call reads "mcp <tool> <args as JSON, keys sorted>".
func (c Call) Line() string {
	if c.Tool != "" {
		args, _ := json.Marshal(c.Args)
		if c.Args == nil {
			args = []byte("{}")
		}
		return "mcp " + c.Tool + " " + string(args)
	}
	words := make([]string, 0, len(c.Argv)+1)
	words = append(words, "sonarless-mcp")
	for _, a := range c.Argv {
		if strings.HasPrefix(a, "--") {
			if name, value, ok := strings.Cut(a, "="); ok {
				words = append(words, name, value)
				continue
			}
		}
		words = append(words, a)
	}
	return strings.Join(words, " ")
}

// Grade checks a run's calls, commands and final answer against the case.
// It returns every broken rule by name, in case order.
func Grade(c *Case, calls []Call, commands []string, answer string) []string {
	var failures []string
	for _, r := range c.Require {
		if !required(r, calls, commands) {
			failures = append(failures, "require: "+r.Name)
		}
	}
	for _, r := range c.Forbid {
		if countMatches(r, lines(r, calls, commands, false)) >= max(r.Times, 1) {
			failures = append(failures, "forbid: "+r.Name)
		}
	}
	if c.Answer != "" && !regexp.MustCompile("(?i)"+c.Answer).MatchString(answer) {
		failures = append(failures, "answer")
	}
	return failures
}

func required(r Rule, calls []Call, commands []string) bool {
	all := lines(r, calls, commands, false)
	if r.BeforeFailed {
		// Needed only before a call that failed (a guessed flag): a run that
		// never guessed needs no help.
		failed := make([]string, len(calls))
		for i, c := range calls {
			if c.Exit != 0 {
				failed[i] = c.Line()
			}
		}
		guess := firstMatch(Rule{Match: r.Before, Not: r.Match}, failed)
		if guess < 0 {
			return true
		}
		first := firstMatch(r, all)
		return first >= 0 && first < guess
	}
	i := firstMatch(r, lines(r, calls, commands, true))
	if i < 0 {
		return false
	}
	if len(r.Before) == 0 {
		return true
	}
	// Order is judged on every line, successful or not: a failed guess
	// before the help is still a guess. A line that matches the rule itself
	// (help on a command line that also names the flag) is not the guess.
	first := firstMatch(r, all)
	after := firstMatch(Rule{Match: r.Before, Not: r.Match}, all)
	return after < 0 || first < after
}

// lines returns the rule's source; with okOnly, a failed call is blank so
// indexes still line up with the full list.
func lines(r Rule, calls []Call, commands []string, okOnly bool) []string {
	if r.On == "commands" {
		return commands
	}
	out := make([]string, len(calls))
	for i, c := range calls {
		if !okOnly || c.Exit == 0 {
			out[i] = c.Line()
		}
	}
	return out
}

func firstMatch(r Rule, lines []string) int {
	if m := matching(r, lines); len(m) > 0 {
		return m[0]
	}
	return -1
}

func countMatches(r Rule, lines []string) int { return len(matching(r, lines)) }

func matching(r Rule, lines []string) []int {
	match := compile(r.Match)
	not := compile(r.Not)
	var out []int
	for i, l := range lines {
		if l != "" && all(match, l) && !anyOf(not, l) {
			out = append(out, i)
		}
	}
	return out
}

func compile(patterns []string) []*regexp.Regexp {
	out := make([]*regexp.Regexp, len(patterns))
	for i, p := range patterns {
		out[i] = regexp.MustCompile(p)
	}
	return out
}

func all(res []*regexp.Regexp, s string) bool {
	for _, re := range res {
		if !re.MatchString(s) {
			return false
		}
	}
	return true
}

func anyOf(res []*regexp.Regexp, s string) bool {
	for _, re := range res {
		if re.MatchString(s) {
			return true
		}
	}
	return false
}

// casePasses is the 2-of-3 rule: a case passes when at least two thirds of
// its graded runs pass.
func casePasses(passes, graded int) bool {
	return graded > 0 && 3*passes >= 2*graded
}
