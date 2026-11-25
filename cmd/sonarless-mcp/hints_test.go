package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// usageHint runs args against cmd (the real tree when nil) and returns what
// follows the error line.
func usageHint(t *testing.T, cmd *cobra.Command, args ...string) string {
	t.Helper()
	if cmd == nil {
		cmd = newRootCommand(context.Background())
	}
	cmd.SetArgs(args)
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	ran, err := cmd.ExecuteContextC(context.Background())
	if err == nil {
		t.Fatalf("%v: no error", args)
	}
	var b bytes.Buffer
	printUsageHint(&b, ran, err, args)
	return b.String()
}

func TestUnknownFlagNamesTheClosestFlag(t *testing.T) {
	got := usageHint(t, nil, "scan", "--tets")
	if !strings.Contains(got, "did you mean --tests?") || !strings.Contains(got, "Run 'sonarless-mcp scan --help' for usage.") {
		t.Errorf("got %q", got)
	}
	if got := usageHint(t, nil, "scan", "--scaner"); !strings.Contains(got, "did you mean --scanner?") {
		t.Errorf("--scaner: %q", got)
	}
}

func TestOneLetterUnknownFlagGuessesNothing(t *testing.T) {
	for _, args := range [][]string{{"scan", "--t"}, {"scan", "-t"}, {"scan", "--zzzz"}} {
		got := usageHint(t, nil, args...)
		if strings.Contains(got, "did you mean") {
			t.Errorf("%v: %q", args, got)
		}
		if !strings.Contains(got, "--help' for usage") {
			t.Errorf("%v: no usage pointer: %q", args, got)
		}
	}
}

// numbered is a command like ytrack's issue view: a bool flag with an int
// sibling, a string flag, and no arguments.
func numbered() *cobra.Command {
	c := &cobra.Command{Use: "view", Args: cobra.NoArgs, SilenceErrors: true, SilenceUsage: true, RunE: func(*cobra.Command, []string) error { return nil }}
	c.Flags().Bool("comments", false, "")
	c.Flags().Int("comments-limit", 0, "")
	c.Flags().String("name", "", "")
	c.Flags().Bool("json", false, "")
	return c
}

func TestNumberAfterBoolFlag(t *testing.T) {
	want := "--comments takes no value; for a number use --comments-limit 12"
	if got := usageHint(t, numbered(), "--comments", "12"); !strings.Contains(got, want) {
		t.Errorf("--comments 12: %q", got)
	}
	if got := usageHint(t, numbered(), "--comments=12"); !strings.Contains(got, want) {
		t.Errorf("--comments=12: %q", got)
	}
	// No int sibling: only that the flag takes no value.
	got := usageHint(t, numbered(), "--json", "3")
	if !strings.Contains(got, "--json takes no value") || strings.Contains(got, "for a number") {
		t.Errorf("--json 3: %q", got)
	}
	if got := usageHint(t, nil, "scan", "--tests=12"); !strings.Contains(got, "--tests takes no value\n") {
		t.Errorf("scan --tests=12: %q", got)
	}
}

func TestNumberHintStopsAtDashDashAndSkipsFlagValues(t *testing.T) {
	if got := usageHint(t, numbered(), "--", "--comments", "12"); strings.Contains(got, "takes no value") {
		t.Errorf("after --: %q", got)
	}
	// --comments is --name's value, so 12 is just a stray argument.
	if got := usageHint(t, numbered(), "--name", "--comments", "12"); strings.Contains(got, "takes no value") {
		t.Errorf("flag value: %q", got)
	}
}

func TestOtherErrorsGetNoHint(t *testing.T) {
	var b bytes.Buffer
	printUsageHint(&b, numbered(), errors.New("server unreachable"), []string{"--comments", "12"})
	if b.Len() != 0 {
		t.Errorf("got %q", b.String())
	}
}

func TestEditDistance(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want int
	}{{"tests", "tets", 1}, {"hots", "host", 1}, {"scanner", "scaner", 1}, {"abc", "abc", 0}, {"", "ab", 2}, {"define", "tests", 5}} {
		if got := editDistance(c.a, c.b); got != c.want {
			t.Errorf("editDistance(%q, %q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}
