// Command bench measures the sonarless-mcp agent skill on real models: each
// case runs `claude -p` in a throwaway project with the skill installed, a
// fake sonarless-mcp first on PATH and that fake as the `sonarqube` MCP
// server, then grades the CLI and MCP tool calls, the Bash commands and the
// final answer. It needs a Claude Code login and never runs in CI; the fake
// never calls docker or the network. See bench/README.md.
//
//	go run ./bench
//	go run ./bench compare bench/results/baseline/claude-haiku-4-5 bench/results/claude-haiku-4-5
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// defaultModel is the weak model the skill has to work on.
const defaultModel = "claude-haiku-4-5"

func main() {
	if strings.TrimSuffix(filepath.Base(os.Args[0]), ".exe") == "sonarless-mcp" {
		os.Exit(fakeMain(os.Args[1:]))
	}
	if len(os.Args) > 1 && os.Args[1] == "compare" {
		os.Exit(compareMain(os.Args[2:]))
	}
	if len(os.Args) > 1 && os.Args[1] == "regrade" {
		os.Exit(regradeMain(os.Args[2:]))
	}
	os.Exit(runMain(os.Args[1:]))
}

func compareMain(args []string) int {
	if len(args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: go run ./bench compare <before results dir> <after results dir>")
		return 1
	}
	before, err := readSummary(args[0])
	if err == nil {
		var after Summary
		if after, err = readSummary(args[1]); err == nil {
			if compare(os.Stdout, before, after) {
				fmt.Println("a case's pass rate dropped")
				return 1
			}
			return 0
		}
	}
	fmt.Fprintln(os.Stderr, "bench:", err)
	return 1
}

// regradeMain grades the stored runs of a results dir again with the current
// cases, without a model: for a grading fix.
func regradeMain(args []string) int {
	fl := flag.NewFlagSet("regrade", flag.ContinueOnError)
	casesDir := fl.String("cases", "bench/cases", "directory of case files")
	if err := fl.Parse(args); err != nil || fl.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: go run ./bench regrade [--cases dir] <results dir>")
		return 1
	}
	dir := fl.Arg(0)
	old, err := readSummary(dir)
	if err == nil {
		err = regrade(*casesDir, dir)
	}
	if err == nil {
		_, err = writeSummary(*casesDir, dir, old.Model, old.SkillHash, old.SkillBytes)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "bench:", err)
		return 1
	}
	return 0
}

type task struct {
	c   *Case
	run int
}

func runMain(args []string) int {
	fl := flag.NewFlagSet("bench", flag.ContinueOnError)
	model := fl.String("model", defaultModel, "Claude Code model id or alias")
	repeat := fl.Int("repeat", 1, "runs per case; a case passes when two thirds of its runs pass")
	casesDir := fl.String("cases", "bench/cases", "directory of case files")
	only := fl.String("case", "", "comma-separated case ids to run (default all)")
	out := fl.String("out", "", "results directory (default bench/results/<model>)")
	skillDir := fl.String("skill", filepath.Join("internal", "clients", "skill"), "skill directory installed in each world")
	noSkill := fl.Bool("no-skill", false, "control: the same worlds without the skill (default --out bench/results/no-skill/<model>)")
	jobs := fl.Int("jobs", 3, "runs in parallel")
	budget := fl.Int("budget-runs", 30, "most claude runs this invocation starts")
	force := fl.Bool("force", false, "re-run cases whose stored results match")
	keep := fl.Bool("keep", false, "keep each world under the temp dir")
	claude := fl.String("claude", "claude", "claude binary")
	timeout := fl.Duration("timeout", 6*time.Minute, "limit per run")
	if err := fl.Parse(args); err != nil {
		return 1
	}
	switch {
	case *out != "":
	case *noSkill:
		*out = filepath.Join("bench", "results", "no-skill", *model)
	default:
		*out = filepath.Join("bench", "results", *model)
	}
	var ids []string
	if *only != "" {
		ids = strings.Split(*only, ",")
	}
	cases, err := loadCases(*casesDir, ids)
	if err == nil {
		err = os.MkdirAll(*out, 0o750)
	}
	skillHash, skillBytes, herr := hashTree(*skillDir)
	logDir := filepath.Join("bench", ".transcripts", *model)
	if *noSkill {
		*skillDir, skillHash, skillBytes = "", "none", 0
		logDir += "-no-skill"
	}
	fake, xerr := os.Executable()
	for _, e := range []error{err, herr, xerr} {
		if e != nil {
			fmt.Fprintln(os.Stderr, "bench:", e)
			return 1
		}
	}
	r := &runner{
		claude: *claude, model: *model, skill: *skillDir, fake: fake, timeout: *timeout, keep: *keep,
		logDir: logDir,
	}

	// Reuse stored graded runs whose key matches; queue the missing ones.
	runs := map[string][]RunResult{}
	var queue []task
	for _, c := range cases {
		stored, err := readCaseResult(filepath.Join(*out, c.ID+".json"))
		if err == nil && stored.Key == resultKey(c, skillHash, *model) && !*force {
			for _, run := range stored.Runs {
				if graded(run) || run.Status == "blocked" {
					runs[c.ID] = append(runs[c.ID], run)
				}
			}
		}
		have := map[int]bool{}
		for _, run := range runs[c.ID] {
			have[run.Run] = true
		}
		for n := 1; n <= *repeat; n++ {
			if !have[n] {
				queue = append(queue, task{c, n})
			}
		}
	}
	skipped := 0
	if len(queue) > *budget {
		skipped = len(queue) - *budget
		queue = queue[:*budget]
	}
	fmt.Fprintf(os.Stderr, "bench: %s, %d cases, %d runs to start (%d over --budget-runs)\n", *model, len(cases), len(queue), skipped)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	var mu sync.Mutex
	var wg sync.WaitGroup
	work := make(chan task)
	harnessErrors := 0
	for range max(*jobs, 1) {
		wg.Go(func() {
			for t := range work {
				res := r.runCase(ctx, t.c, t.run)
				mu.Lock()
				runs[t.c.ID] = append(runs[t.c.ID], res)
				if res.Status == "error" {
					harnessErrors++
				}
				mu.Unlock()
				fmt.Fprintf(os.Stderr, "  %-16s run %d: %-5s %s turns=%d in=%d out=%d $%.4f skill=%v %s\n", t.c.ID, t.run, res.Status,
					res.Failed, res.Turns, res.Tokens.TotalInput(), res.Tokens.Output, res.CostUSD, res.SkillLoaded, res.Reason)
			}
		})
	}
	for _, t := range queue {
		mu.Lock()
		stopEarly := harnessErrors >= 2
		mu.Unlock()
		if stopEarly || ctx.Err() != nil {
			fmt.Fprintln(os.Stderr, "bench: stopping early after harness errors")
			break
		}
		work <- t
	}
	close(work)
	wg.Wait()

	failed := false
	for _, c := range cases {
		cr := newCaseResult(c, *model, resultKey(c, skillHash, *model), runs[c.ID])
		if err := writeJSON(filepath.Join(*out, c.ID+".json"), cr); err != nil {
			fmt.Fprintln(os.Stderr, "bench:", err)
			return 1
		}
		failed = failed || !cr.Pass
	}
	s, err := writeSummary(*casesDir, *out, *model, skillHash, skillBytes)
	if err != nil {
		fmt.Fprintln(os.Stderr, "bench:", err)
		return 1
	}
	fmt.Printf("%s: %d/%d cases pass, %d/%d runs (%.0f%%), mean input %.0f tokens/run, %d output tokens, $%.4f\n", *model,
		s.Totals.CasesPassed, s.Totals.Cases, s.Totals.Passes, s.Totals.Graded, 100*s.Totals.PassRate, s.Totals.MeanTotalInput,
		s.Totals.Tokens.Output, s.Totals.CostUSD)
	if failed || harnessErrors > 0 || skipped > 0 {
		return 1
	}
	return 0
}
