package idle

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/BrainerVirus/sonarless-mcp/internal/config"
)

func procsCfg(t *testing.T) *config.Config {
	t.Helper()
	t.Setenv("SONARLESS_HOME", t.TempDir())
	cfg, _, err := config.Load("")
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestRunningKeepsLiveRecordsOnly(t *testing.T) {
	cfg := procsCfg(t)
	orig := CommandLines
	t.Cleanup(func() { CommandLines = orig })
	var queries [][]int
	CommandLines = func(pids []int) map[int]string {
		queries = append(queries, pids)
		table := map[int]string{
			10: "/opt/bin/sonarless-mcp mcp /ws/a",
			11: "/usr/bin/vim notes.txt",                   // pid reused by another program
			13: "/usr/bin/grep -r sonarless-mcp mcp /ws/d", // reused by one that mentions us
			14: "/opt/bin/sonarless-mcp serve",             // reused by our gateway
			20: "/opt/bin/sonarless-mcp scan",
			// 12: gone
		}
		out := map[int]string{}
		for _, p := range pids {
			if c, ok := table[p]; ok {
				out[p] = c
			}
		}
		return out
	}
	exe, now := "/opt/bin/sonarless-mcp", time.Now()
	reg := func(p Proc) *Record {
		p.Exe = exe
		r, err := Register(cfg, p)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	reg(Proc{Kind: KindShim, PID: 10, Verb: "mcp", Workspace: "/ws/a", Started: now})
	reg(Proc{Kind: KindShim, PID: 11, Verb: "mcp", Workspace: "/ws/b", Started: now})
	reg(Proc{Kind: KindShim, PID: 12, Verb: "mcp", Workspace: "/ws/c", Started: now})
	reg(Proc{Kind: KindShim, PID: 13, Verb: "mcp", Workspace: "/ws/d", Started: now})
	reg(Proc{Kind: KindShim, PID: 14, Verb: "mcp", Workspace: "/ws/e", Started: now})
	scan := reg(Proc{Kind: KindScan, PID: 20, Verb: "scan", Workspace: "/ws/a", Project: "a", Scanner: "cli", State: ScanWaiting, Started: now})

	shims := Running(cfg, KindShim)
	if len(shims) != 1 || shims[0].PID != 10 || shims[0].Workspace != "/ws/a" {
		t.Fatalf("shims: %+v", shims)
	}
	if len(queries) != 1 || len(queries[0]) != 5 {
		t.Errorf("process queries %v, want one for all five shim pids", queries)
	}
	if files, _ := filepath.Glob(filepath.Join(procsDir(cfg), KindShim+"-*")); len(files) != 1 {
		t.Errorf("stale shim records kept: %v", files)
	}
	if scans := Running(cfg, KindScan); len(scans) != 1 || scans[0].Project != "a" || scans[0].State != ScanWaiting {
		t.Fatalf("scans: %+v", scans)
	}
	scan.Update(ScanScanning)
	if scans := Running(cfg, KindScan); len(scans) != 1 || scans[0].State != ScanScanning {
		t.Fatalf("scan after Update: %+v", scans)
	}
	scan.Done()
	if scans := Running(cfg, KindScan); len(scans) != 0 {
		t.Errorf("finished scan still listed: %+v", scans)
	}
	var nilRec *Record // a failed Register
	nilRec.Update(ScanScanning)
	nilRec.Done()
}

func TestRegisterDefaultsToThisProcess(t *testing.T) {
	cfg := procsCfg(t)
	r, err := Register(cfg, Proc{Kind: KindShim, Workspace: "/ws"})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Done()
	got := Running(cfg, KindShim) // the real process table: this test binary
	if len(got) != 1 || got[0].PID != os.Getpid() || got[0].Started.IsZero() || got[0].Verb == "" {
		t.Errorf("own record: %+v", got)
	}
}

func TestRecordsReadableByTheUserOnly(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no Unix permissions")
	}
	cfg := procsCfg(t)
	if err := os.MkdirAll(procsDir(cfg), 0o755); err != nil { // as an older build left it
		t.Fatal(err)
	}
	r, err := Register(cfg, Proc{Kind: KindScan, Workspace: "/ws/secret-project"})
	if err != nil {
		t.Fatal(err)
	}
	r.Update(ScanScanning) // rewrites the record
	defer r.Done()
	if fi, err := os.Stat(procsDir(cfg)); err != nil || fi.Mode().Perm() != 0o700 {
		t.Errorf("procs dir mode %v (%v), want 0700", fi.Mode().Perm(), err)
	}
	files, _ := filepath.Glob(filepath.Join(procsDir(cfg), "*"))
	if len(files) != 1 {
		t.Fatalf("records: %v", files)
	}
	if fi, err := os.Stat(files[0]); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("record mode %v (%v), want 0600", fi.Mode().Perm(), err)
	}
}

func TestCommandLinesOneQuery(t *testing.T) {
	type call struct{ name, args string }
	fake := func(out string, err error) (func(string, ...string) ([]byte, error), *[]call) {
		var calls []call
		return func(name string, args ...string) ([]byte, error) {
			calls = append(calls, call{name, strings.Join(args, " ")})
			return []byte(out), err
		}, &calls
	}

	t.Run("windows: one PowerShell for every pid", func(t *testing.T) {
		run, calls := fake("10\tC:\\Tools\\sonarless-mcp.exe mcp C:\\ws\r\n20\t\"C:\\Tools\\sonarless-mcp.exe\" scan\r\n30\t\r\n", nil)
		got := commandLines("windows", []int{10, 20, 30, 40}, run)
		if len(*calls) != 1 || (*calls)[0].name != "powershell" ||
			!strings.Contains((*calls)[0].args, "-Filter 'ProcessId=10 OR ProcessId=20 OR ProcessId=30 OR ProcessId=40'") {
			t.Fatalf("calls: %+v", *calls)
		}
		want := map[int]string{10: `C:\Tools\sonarless-mcp.exe mcp C:\ws`, 20: `"C:\Tools\sonarless-mcp.exe" scan`}
		if len(got) != len(want) || got[10] != want[10] || got[20] != want[20] {
			t.Errorf("got %v, want %v (no command line = not ours to judge, gone = absent)", got, want)
		}
	})
	t.Run("macOS: one ps for every pid", func(t *testing.T) {
		run, calls := fake("   10 /opt/bin/sonarless-mcp mcp /ws\n 2000 /opt/bin/sonarless-mcp serve\n", nil)
		got := commandLines("darwin", []int{10, 2000, 3}, run)
		if len(*calls) != 1 || (*calls)[0].args != "-o pid=,command= -p 10,2000,3" {
			t.Fatalf("calls: %+v", *calls)
		}
		if got[10] != "/opt/bin/sonarless-mcp mcp /ws" || got[2000] != "/opt/bin/sonarless-mcp serve" || len(got) != 2 {
			t.Errorf("got %v", got)
		}
	})
	t.Run("query fails: nothing runs", func(t *testing.T) {
		run, _ := fake("", errors.New("powershell missing"))
		if got := commandLines("windows", []int{10}, run); len(got) != 0 {
			t.Errorf("got %v", got)
		}
	})
	t.Run("no pids: no query", func(t *testing.T) {
		run, calls := fake("", nil)
		commandLines("windows", nil, run)
		if len(*calls) != 0 {
			t.Errorf("calls: %+v", *calls)
		}
	})
}

func TestSameProgram(t *testing.T) {
	const exe = "/opt/bin/sonarless-mcp"
	cases := []struct {
		cmdline, exe, verb string
		want               bool
	}{
		{"/opt/bin/sonarless-mcp mcp /ws", exe, "mcp", true},
		{"sonarless-mcp mcp", exe, "mcp", true},                       // started from PATH
		{"/home/me/.local/bin/sonarless-mcp scan", exe, "scan", true}, // a symlink
		{`"C:\Program Files\x\Sonarless-MCP.exe" mcp C:\ws`, `C:\Program Files\x\sonarless-mcp.exe`, "mcp", true},
		{`C:\Tools\sonarless-mcp.exe serve`, `C:\Tools\sonarless-mcp.exe`, "serve", true},
		{"/opt/my apps/sonarless-mcp mcp", "/opt/my apps/sonarless-mcp", "mcp", true}, // /proc: spaces unquoted
		{"/opt/bin/sonarless-mcp serve", exe, "mcp", false},                           // another verb
		{"/opt/bin/sonarless-mcp", exe, "mcp", false},                                 // no verb
		{"/usr/bin/grep sonarless-mcp mcp", exe, "mcp", false},                        // mentions us in its args
		{"/usr/bin/vim /opt/bin/sonarless-mcp mcp", exe, "mcp", false},
		{"/opt/bin/sonarless-mcp-old mcp", exe, "mcp", false}, // a name that merely starts like ours
		{`"C:\Tools\sonarless-mcp.exe mcp`, `C:\Tools\sonarless-mcp.exe`, "mcp", false},
		{"", exe, "mcp", false},
		{"/opt/bin/sonarless-mcp mcp", "", "mcp", false},
		{"/opt/bin/sonarless-mcp mcp", exe, "", false},
	}
	for _, c := range cases {
		if got := SameProgram(c.cmdline, c.exe, c.verb); got != c.want {
			t.Errorf("SameProgram(%q, %q, %q) = %v, want %v", c.cmdline, c.exe, c.verb, got, c.want)
		}
	}
}
