package idle

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/BrainerVirus/sonarless-mcp/internal/config"
)

// Short-lived work records itself in the cache dir (procs/), so `status`
// can list what runs on this machine: stdio shims and scans. A record whose
// process is gone, or whose pid now runs another program, is dropped.

// Kinds of process records.
const (
	KindShim = "shim"
	KindScan = "scan"
)

// Scan states.
const (
	ScanWaiting  = "waiting"  // for the server to start
	ScanScanning = "scanning" // the scanner runs, or the server processes its report
)

// Proc is one running stdio shim or scan.
type Proc struct {
	Kind      string    `json:"kind"`
	PID       int       `json:"pid"`
	Exe       string    `json:"exe"`
	Started   time.Time `json:"started"`
	Verb      string    `json:"verb"`                // the process's command: mcp, scan, serve
	State     string    `json:"state,omitempty"`     // scans: waiting, scanning
	Workspace string    `json:"workspace,omitempty"` // project root
	Project   string    `json:"project,omitempty"`   // scans: project key
	Scanner   string    `json:"scanner,omitempty"`   // scans: cli, maven, gradle, dotnet
}

func procsDir(cfg *config.Config) string { return filepath.Join(cfg.CacheDir, "procs") }

// Record is a registered process; its methods are no-ops on nil, so a
// failed Register needs no special casing.
type Record struct {
	file string
	p    Proc
}

// Register records p (PID, Exe, Verb and Started default to this process,
// now). Call Done on every exit path. Records name workspaces, so they are
// readable by the user only.
func Register(cfg *config.Config, p Proc) (*Record, error) {
	if p.PID == 0 {
		p.PID = os.Getpid()
	}
	if p.Exe == "" {
		p.Exe, _ = os.Executable()
	}
	if p.Verb == "" && len(os.Args) > 1 {
		p.Verb = os.Args[1]
	}
	if p.Started.IsZero() {
		p.Started = time.Now()
	}
	if err := os.MkdirAll(procsDir(cfg), 0o700); err != nil {
		return nil, err
	}
	_ = os.Chmod(procsDir(cfg), 0o700) // made by an older build with 0755
	r := &Record{file: filepath.Join(procsDir(cfg), fmt.Sprintf("%s-%d-%d.json", p.Kind, p.PID, p.Started.UnixNano())), p: p}
	if err := r.write(); err != nil {
		return nil, err
	}
	return r, nil
}

func (r *Record) write() error {
	b, _ := json.Marshal(r.p)
	tmp := r.file + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, r.file) // readers never see half a record
}

// Update records a new state.
func (r *Record) Update(state string) {
	if r != nil {
		r.p.State = state
		_ = r.write()
	}
}

// Done removes the record.
func (r *Record) Done() {
	if r != nil {
		_ = os.Remove(r.file)
	}
}

// Running returns the recorded processes of the given kinds that still run,
// oldest first, and deletes the records of those that don't. It asks the OS
// about all their pids at once.
func Running(cfg *config.Config, kinds ...string) []Proc {
	var files []string
	for _, kind := range kinds {
		f, _ := filepath.Glob(filepath.Join(procsDir(cfg), kind+"-*.json"))
		files = append(files, f...)
	}
	type rec struct {
		file string
		p    Proc
	}
	var recs []rec
	var pids []int
	for _, f := range files {
		var p Proc
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		if json.Unmarshal(b, &p) != nil || p.PID <= 0 {
			_ = os.Remove(f)
			continue
		}
		recs = append(recs, rec{f, p})
		pids = append(pids, p.PID)
	}
	if len(recs) == 0 {
		return nil
	}
	cmdlines := CommandLines(pids)
	var out []Proc
	for _, r := range recs {
		if !SameProgram(cmdlines[r.p.PID], r.p.Exe, r.p.Verb) {
			_ = os.Remove(r.file)
			continue
		}
		out = append(out, r.p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Started.Before(out[j].Started) })
	return out
}

// SameProgram reports whether a command line runs `<exe> <verb> ...`: its
// program (argv[0]) is exe, or a file of the same name (on PATH, a
// symlink; Windows names compare case-insensitively, ".exe" optional), and
// its first argument is verb. Merely mentioning sonarless-mcp in the
// arguments of another program does not count.
func SameProgram(cmdline, exe, verb string) bool {
	cmdline = strings.TrimSpace(cmdline)
	if cmdline == "" || exe == "" || verb == "" {
		return false
	}
	var prog, rest string
	switch {
	case strings.HasPrefix(cmdline, `"`): // Windows quotes a program path with spaces
		end := strings.Index(cmdline[1:], `"`)
		if end < 0 {
			return false
		}
		prog, rest = cmdline[1:end+1], cmdline[end+2:]
	case strings.HasPrefix(strings.ToLower(cmdline), strings.ToLower(exe)+" "): // a path with spaces, unquoted (/proc)
		prog, rest = cmdline[:len(exe)], cmdline[len(exe):]
	default:
		prog, rest, _ = strings.Cut(cmdline, " ")
	}
	args := strings.Fields(rest)
	return len(args) > 0 && args[0] == verb && programName(prog) == programName(exe)
}

// programName is a program path's file name, lower case, without ".exe".
func programName(p string) string {
	if i := strings.LastIndexAny(p, `/\`); i >= 0 {
		p = p[i+1:]
	}
	return strings.TrimSuffix(strings.ToLower(p), ".exe")
}

// CommandLine returns a running process's command line ("" if it is gone
// or the OS won't say).
func CommandLine(pid int) string { return CommandLines([]int{pid})[pid] }

// CommandLines returns the command lines of the pids that still run, in one
// OS query (one PowerShell start on Windows, not one per pid). A variable
// so tests can fake the process table.
var CommandLines = func(pids []int) map[int]string { return commandLines(runtime.GOOS, pids, runOutput) }

// runOutput runs a program and returns its stdout.
var runOutput = func(name string, args ...string) ([]byte, error) { return exec.Command(name, args...).Output() }

func commandLines(goos string, pids []int, run func(string, ...string) ([]byte, error)) map[int]string {
	out := map[int]string{}
	if len(pids) == 0 {
		return out
	}
	ids := make([]string, len(pids))
	for i, p := range pids {
		ids[i] = strconv.Itoa(p)
	}
	switch goos {
	case "linux":
		for _, id := range ids {
			if b, err := os.ReadFile("/proc/" + id + "/cmdline"); err == nil {
				p, _ := strconv.Atoi(id)
				out[p] = strings.TrimSpace(strings.ReplaceAll(string(b), "\x00", " "))
			}
		}
		return out
	case "windows":
		filter := "ProcessId=" + strings.Join(ids, " OR ProcessId=")
		b, err := run("powershell", "-NoProfile", "-NonInteractive", "-Command",
			"Get-CimInstance Win32_Process -Filter '"+filter+"' | ForEach-Object { \"$($_.ProcessId)`t$($_.CommandLine)\" }")
		if err != nil {
			return out
		}
		parseLines(b, out, func(line string) (string, string, bool) { return strings.Cut(line, "\t") })
		return out
	default: // macOS, BSDs
		b, _ := run("ps", "-o", "pid=,command=", "-p", strings.Join(ids, ","))
		parseLines(b, out, func(line string) (string, string, bool) {
			line = strings.TrimSpace(line)
			i := strings.IndexAny(line, " \t")
			if i < 0 {
				return "", "", false
			}
			return line[:i], line[i+1:], true
		})
		return out
	}
}

// parseLines reads "<pid><sep><command line>" lines into out.
func parseLines(b []byte, out map[int]string, split func(string) (string, string, bool)) {
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		id, cmd, ok := split(strings.TrimRight(sc.Text(), "\r"))
		if !ok {
			continue
		}
		if pid, err := strconv.Atoi(strings.TrimSpace(id)); err == nil && strings.TrimSpace(cmd) != "" {
			out[pid] = strings.TrimSpace(cmd)
		}
	}
}
