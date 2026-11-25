package main

// Usage errors that name the flag you were probably after. Ported from ytrack
// (github.com/BrainerVirus/youtrack-cli, pkg/cmd/root/root.go: flagHint,
// nearFlags, editDistance).

import (
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// usageErrorPrefixes are how cobra and pflag word a command line they could
// not parse.
var usageErrorPrefixes = []string{
	"unknown flag", "unknown shorthand flag", "unknown command", "invalid argument",
	"flag needs an argument", "bad flag syntax", "accepts ", "requires ",
}

func isUsageError(err error) bool {
	msg := err.Error()
	for _, p := range usageErrorPrefixes {
		if strings.HasPrefix(msg, p) {
			return true
		}
	}
	return false
}

// printUsageHint follows a usage error with the flag it was probably after
// and where the usage is.
func printUsageHint(w io.Writer, cmd *cobra.Command, err error, args []string) {
	if cmd == nil || err == nil || !isUsageError(err) {
		return
	}
	if hint := flagHint(cmd, err, args); hint != "" {
		fmt.Fprintln(w, hint)
	}
	fmt.Fprintf(w, "Run '%s --help' for usage.\n", cmd.CommandPath())
}

// flagHint names the flag a usage error was probably after (weak models guess
// flags), or "" when nothing is close.
func flagHint(cmd *cobra.Command, err error, args []string) string {
	if name, ok := strings.CutPrefix(err.Error(), "unknown flag: --"); ok {
		if near := nearFlags(cmd, name); len(near) > 0 {
			return "did you mean --" + strings.Join(near, " or --") + "?"
		}
		return ""
	}
	// A number given to a flag that takes no value: `--tests 12` (12 became
	// a stray argument) or `--tests=12` (not a bool).
	for i := 0; i < len(args); i++ {
		if args[i] == "--" {
			break // the rest are arguments
		}
		if prev := cmd.Flags().Lookup(strings.TrimPrefix(args[max(i-1, 0)], "--")); i > 0 && prev != nil && prev.Value.Type() != "bool" && !strings.Contains(args[i-1], "=") {
			continue // this word is the previous flag's value
		}
		name, ok := strings.CutPrefix(args[i], "--")
		if !ok {
			continue
		}
		var num string
		if n, v, eq := strings.Cut(name, "="); eq {
			name, num = n, v
		} else if i+1 < len(args) {
			num = args[i+1]
		}
		if !isNumber(num) {
			continue
		}
		fl := cmd.Flags().Lookup(name)
		if fl == nil || fl.Value.Type() != "bool" {
			continue
		}
		hint := "--" + name + " takes no value"
		cmd.Flags().VisitAll(func(o *pflag.Flag) {
			if strings.HasPrefix(o.Name, name+"-") && o.Value.Type() == "int" {
				hint += "; for a number use --" + o.Name + " " + num
			}
		})
		return hint
	}
	return ""
}

// nearFlags returns up to three of cmd's flags that start with name (two
// letters or more), contain it or are contained in it (three or more), or
// are a few edits from it (one edit per three letters, at most two), so a
// one-letter guess matches nothing.
func nearFlags(cmd *cobra.Command, name string) []string {
	var near []string
	cmd.Flags().VisitAll(func(fl *pflag.Flag) {
		if fl.Hidden || len(near) == 3 {
			return
		}
		n := fl.Name
		edits := min(len(name), len(n)) / 3
		if (len(name) >= 2 && strings.HasPrefix(n, name)) ||
			(len(name) >= 3 && strings.Contains(n, name)) ||
			(len(n) >= 3 && strings.Contains(name, n)) ||
			(edits > 0 && editDistance(n, name) <= min(edits, 2)) {
			near = append(near, n)
		}
	})
	return near
}

func isNumber(s string) bool {
	_, err := strconv.Atoi(s)
	return err == nil
}

// editDistance is the optimal string alignment distance between a and b:
// Levenshtein, plus a swap of two adjacent letters as one edit (--tets).
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
