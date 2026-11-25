package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
)

// benchVersion is part of every result key: bump it when grading or the
// world changes, so stored results are not reused.
const benchVersion = "1"

// CaseResult is bench/results/<model>/<case>.json.
type CaseResult struct {
	Case   string      `json:"case"`
	Model  string      `json:"model"`
	Key    string      `json:"key"`
	Passes int         `json:"passes"`
	Graded int         `json:"graded"`
	Pass   bool        `json:"pass"`
	Runs   []RunResult `json:"runs"`
}

// CaseRow is one case in summary.json.
type CaseRow struct {
	Case string `json:"case"`
	// Stale: the case's runs were made with an earlier version of the skill
	// or case (a partial re-run); its numbers are not this skill's.
	Stale   bool `json:"stale,omitempty"`
	Pass    bool `json:"pass"`
	Passes  int  `json:"passes"`
	Graded  int  `json:"graded"`
	Blocked int  `json:"blocked,omitempty"`
	Errors  int  `json:"errors,omitempty"`
	// NotLoaded counts graded runs whose model never loaded the skill.
	NotLoaded      int      `json:"notLoaded,omitempty"`
	SkillLoaded    int      `json:"skillLoaded"`
	Failed         []string `json:"failed,omitempty"`
	MeanTurns      float64  `json:"meanTurns"`
	MeanTotalInput float64  `json:"meanTotalInput"`
	SDTotalInput   float64  `json:"sdTotalInput"`
	MeanOutput     float64  `json:"meanOutput"`
	MeanDurationMs float64  `json:"meanDurationMs"`
	CostUSD        float64  `json:"costUSD"`
}

// Summary is bench/results/<model>/summary.json.
type Summary struct {
	Model      string    `json:"model"`
	SkillHash  string    `json:"skillHash"`
	SkillBytes int       `json:"skillBytes"`
	Cases      []CaseRow `json:"cases"`
	Totals     Totals    `json:"totals"`
}

// Totals add up every graded run; TotalInput counts cached prompt tokens too.
type Totals struct {
	Cases          int     `json:"cases"`
	CasesPassed    int     `json:"casesPassed"`
	Runs           int     `json:"runs"`
	Graded         int     `json:"graded"`
	Passes         int     `json:"passes"`
	PassRate       float64 `json:"passRate"`
	Tokens         Usage   `json:"tokens"`
	TotalInput     int     `json:"totalInput"`
	MeanTotalInput float64 `json:"meanTotalInputPerRun"`
	CostUSD        float64 `json:"costUSD"`
}

func graded(r RunResult) bool { return r.Status == "pass" || r.Status == "fail" }

func newCaseResult(c *Case, model, key string, runs []RunResult) CaseResult {
	sort.Slice(runs, func(i, j int) bool { return runs[i].Run < runs[j].Run })
	cr := CaseResult{Case: c.ID, Model: model, Key: key, Runs: runs}
	for _, r := range runs {
		if graded(r) {
			cr.Graded++
			if r.Status == "pass" {
				cr.Passes++
			}
		}
	}
	cr.Pass = casePasses(cr.Passes, cr.Graded)
	return cr
}

func summarize(model string, skillHash string, skillBytes int, results []CaseResult) Summary {
	s := Summary{Model: model, SkillHash: skillHash, SkillBytes: skillBytes}
	for _, cr := range results {
		row := CaseRow{Case: cr.Case, Pass: cr.Pass, Passes: cr.Passes, Graded: cr.Graded}
		var inputs []float64
		for _, r := range cr.Runs {
			s.Totals.Runs++
			switch r.Status {
			case "blocked":
				row.Blocked++
				continue
			case "error":
				row.Errors++
				continue
			case "fail":
				row.Failed = append(row.Failed, r.Failed)
			}
			if r.SkillLoaded {
				row.SkillLoaded++
			} else {
				row.NotLoaded++
			}
			row.MeanTurns += float64(r.Turns)
			row.MeanTotalInput += float64(r.Tokens.TotalInput())
			inputs = append(inputs, float64(r.Tokens.TotalInput()))
			row.MeanDurationMs += float64(r.DurationMs)
			row.MeanOutput += float64(r.Tokens.Output)
			row.CostUSD += r.CostUSD
			s.Totals.Tokens.Input += r.Tokens.Input
			s.Totals.Tokens.Output += r.Tokens.Output
			s.Totals.Tokens.CacheCreation += r.Tokens.CacheCreation
			s.Totals.Tokens.CacheRead += r.Tokens.CacheRead
			s.Totals.CostUSD += r.CostUSD
		}
		if cr.Graded > 0 {
			n := float64(cr.Graded)
			row.MeanTurns, row.MeanTotalInput, row.MeanOutput = round(row.MeanTurns/n), round(row.MeanTotalInput/n), round(row.MeanOutput/n)
			row.MeanDurationMs = round(row.MeanDurationMs / n)
			row.SDTotalInput = round(stddev(inputs))
		}
		row.CostUSD = round4(row.CostUSD)
		s.Cases = append(s.Cases, row)
		s.Totals.Cases++
		s.Totals.Graded += cr.Graded
		s.Totals.Passes += cr.Passes
		if cr.Pass {
			s.Totals.CasesPassed++
		}
	}
	s.Totals.TotalInput = s.Totals.Tokens.TotalInput()
	if s.Totals.Graded > 0 {
		s.Totals.PassRate = round4(float64(s.Totals.Passes) / float64(s.Totals.Graded))
		s.Totals.MeanTotalInput = round(float64(s.Totals.TotalInput) / float64(s.Totals.Graded))
	}
	s.Totals.CostUSD = round4(s.Totals.CostUSD)
	return s
}

func stddev(xs []float64) float64 {
	if len(xs) < 2 {
		return 0
	}
	mean := 0.0
	for _, x := range xs {
		mean += x
	}
	mean /= float64(len(xs))
	v := 0.0
	for _, x := range xs {
		v += (x - mean) * (x - mean)
	}
	return math.Sqrt(v / float64(len(xs)-1))
}

func round(f float64) float64  { return float64(int64(f*10+0.5)) / 10 }
func round4(f float64) float64 { return float64(int64(f*10000+0.5)) / 10000 }

// writeJSON writes v scrubbed, and refuses a file that still holds a home
// path or a token.
func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	sc := newScrubber("")
	text := sc.scrub(string(b)) + "\n"
	if err := sc.check(text); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return os.WriteFile(path, []byte(text), 0o600)
}

func readCaseResult(path string) (CaseResult, error) {
	var cr CaseResult
	b, err := os.ReadFile(path)
	if err != nil {
		return cr, err
	}
	return cr, json.Unmarshal(b, &cr)
}

// hashTree hashes every file of a directory by relative path and content.
func hashTree(dir string) (string, int, error) {
	h := sha256.New()
	size := 0
	fsys := os.DirFS(dir)
	err := fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := fs.ReadFile(fsys, p)
		if err != nil {
			return err
		}
		fmt.Fprintf(h, "%s\x00", p)
		h.Write(b)
		size += len(b)
		return nil
	})
	return hex.EncodeToString(h.Sum(nil))[:16], size, err
}

func readSummary(dir string) (Summary, error) {
	var s Summary
	b, err := fs.ReadFile(os.DirFS(dir), "summary.json")
	if err != nil {
		return s, err
	}
	return s, json.Unmarshal(b, &s)
}

// compare prints before and after per case and reports whether any case's
// pass rate dropped.
func compare(w io.Writer, before, after Summary) bool {
	rows := map[string]CaseRow{}
	for _, r := range before.Cases {
		rows[r.Case] = r
	}
	dropped := false
	fmt.Fprintf(w, "model %s: skill %d -> %d bytes\n", after.Model, before.SkillBytes, after.SkillBytes)
	fmt.Fprintf(w, "%-22s %9s %9s %12s %12s %8s\n", "case", "before", "after", "input before", "input after", "delta")
	for _, a := range after.Cases {
		b, ok := rows[a.Case]
		if !ok {
			fmt.Fprintf(w, "%-22s %9s %4d/%-4d %12s %12.0f\n", a.Case, "new", a.Passes, a.Graded, "", a.MeanTotalInput)
			continue
		}
		mark := ""
		if a.Stale {
			mark = "  (earlier skill)"
		}
		if rate(a.Passes, a.Graded) < rate(b.Passes, b.Graded) {
			mark, dropped = "  DROP", true
		}
		fmt.Fprintf(w, "%-22s %4d/%-4d %4d/%-4d %12.0f %12.0f %+7.1f%%%s\n", a.Case, b.Passes, b.Graded, a.Passes, a.Graded,
			b.MeanTotalInput, a.MeanTotalInput, pct(b.MeanTotalInput, a.MeanTotalInput), mark)
	}
	bt, at := before.Totals, after.Totals
	fmt.Fprintf(w, "%-22s %4d/%-4d %4d/%-4d %12.0f %12.0f %+7.1f%%\n", "total", bt.Passes, bt.Graded, at.Passes, at.Graded,
		bt.MeanTotalInput, at.MeanTotalInput, pct(bt.MeanTotalInput, at.MeanTotalInput))
	fmt.Fprintf(w, "pass rate %.1f%% -> %.1f%%, cost $%.4f -> $%.4f\n", 100*bt.PassRate, 100*at.PassRate, bt.CostUSD, at.CostUSD)
	bp, ap := perTurn(before), perTurn(after)
	fmt.Fprintf(w, "input per turn %.0f -> %.0f (%+.1f%%)\n", bp, ap, pct(bp, ap))
	return dropped
}

// perTurn is the mean prompt tokens per model turn over the graded runs: the
// per-request size of the context, apart from how many turns a run took.
func perTurn(s Summary) float64 {
	var input, turns float64
	for _, c := range s.Cases {
		input += c.MeanTotalInput * float64(c.Graded)
		turns += c.MeanTurns * float64(c.Graded)
	}
	if turns == 0 {
		return 0
	}
	return input / turns
}

func rate(p, n int) float64 {
	if n == 0 {
		return 0
	}
	return float64(p) / float64(n)
}

func pct(before, after float64) float64 {
	if before == 0 {
		return 0
	}
	return 100 * (after - before) / before
}

func resultKey(c *Case, skillHash, model string) string {
	return strings.Join([]string{benchVersion, c.hash, skillHash, model}, "-")
}

// writeSummary sums up every case of casesDir with a stored result for this
// model, marks a result made with another skill or case version stale, and
// writes summary.json.
func writeSummary(casesDir, out, model, skillHash string, skillBytes int) (Summary, error) {
	cases, err := loadCases(casesDir, nil)
	if err != nil {
		return Summary{}, err
	}
	var results []CaseResult
	stale := map[string]bool{}
	for _, c := range cases {
		cr, err := readCaseResult(filepath.Join(out, c.ID+".json"))
		if err == nil && cr.Model == model {
			results = append(results, cr)
			stale[c.ID] = cr.Key != resultKey(c, skillHash, model)
		}
	}
	s := summarize(model, skillHash, skillBytes, results)
	for i := range s.Cases {
		s.Cases[i].Stale = stale[s.Cases[i].Case]
	}
	return s, writeJSON(filepath.Join(out, "summary.json"), s)
}

// regrade grades every stored pass or fail again with the current rules. It
// is for a rule change only: a changed prompt, fixture or file needs new runs.
func regrade(casesDir, dir string) error {
	cases, err := loadCases(casesDir, nil)
	if err != nil {
		return err
	}
	for _, c := range cases {
		path := filepath.Join(dir, c.ID+".json")
		cr, err := readCaseResult(path)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		for i, r := range cr.Runs {
			// A fakeSilent error is graded when the current check no longer
			// sees a sonarless-mcp run the fake missed.
			silent := r.Status == "error" && r.Reason == fakeSilent && !(ranSonarless(r.Commands, r.Denials) && cliCalls(r.Calls) == 0)
			if !graded(r) && !silent {
				continue
			}
			r.Reason = ""
			failures := Grade(c, r.Calls, r.Commands, r.Answer)
			if slices.Contains(r.Failures, "max turns") {
				failures = append(failures, "max turns")
			}
			r.Failures, r.Failed, r.Status = failures, "", "pass"
			if len(failures) > 0 {
				r.Failed, r.Status = failures[0], "fail"
			}
			cr.Runs[i] = r
		}
		// Same runs, current rules: the key takes the case's new hash and keeps
		// the skill the runs were made with.
		key := cr.Key
		if parts := strings.SplitN(cr.Key, "-", 4); len(parts) == 4 {
			key = resultKey(c, parts[2], parts[3])
		}
		cr = newCaseResult(c, cr.Model, key, cr.Runs)
		if err := writeJSON(path, cr); err != nil {
			return err
		}
	}
	return nil
}
