package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/BrainerVirus/sonarless-mcp/internal/config"
)

const testImageID = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// cdsDocker is a fake `docker` on PATH backed by a directory: c/<name>.json
// is a container's inspect output (absent: no such container), v/<name> a
// volume, logs/<name> a container's output, image the image ID. `run`
// creates a running container with the labels it was given and fails with
// a name conflict like the real CLI; `rm` removes one. Every call is logged.
type cdsDocker struct {
	t   *testing.T
	dir string
}

func newCDSDocker(t *testing.T) *cdsDocker {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell-script fake docker")
	}
	d := &cdsDocker{t: t, dir: t.TempDir()}
	for _, sub := range []string{"c", "v", "logs"} {
		if err := os.MkdirAll(filepath.Join(d.dir, sub), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	d.write("image", testImageID)
	script := `#!/bin/sh
D='` + d.dir + `'
echo "$*" >> "$D/calls.log"
last=""
for a in "$@"; do last=$a; done
case "$1 $2" in
"container inspect")
  [ -f "$D/c/$3.json" ] || { echo "Error: No such container: $3" >&2; exit 1; }
  cat "$D/c/$3.json" ;;
"image inspect") cat "$D/image" ;;
"volume inspect")
  [ -f "$D/v/$3" ] || { echo "Error: no such volume: $3" >&2; exit 1; } ;;
"volume create") touch "$D/v/$last" ;;
logs*) cat "$D/logs/$last" 2>/dev/null ;;
rm*) rm -f "$D/c/$last.json" ;;
run*)
  name="" labels="" prev=""
  for a in "$@"; do
    case "$prev" in
    --name) name=$a ;;
    --label) labels="$labels${labels:+,}\"${a%%=*}\":\"${a#*=}\"" ;;
    esac
    prev=$a
  done
  if [ -f "$D/c/$name.json" ]; then
    echo "docker: Error response from daemon: Conflict. The container name \"/$name\" is already in use by container \"abc\"." >&2
    exit 125
  fi
  printf '[{"Name":"/%s","Image":"%s","Config":{"Image":"img","Labels":{%s}},"State":{"Status":"running","StartedAt":"%s"}}]' \
    "$name" "$(cat "$D/image")" "$labels" "2026-11-25T10:00:00.123456789Z" > "$D/c/$name.json"
  echo cid ;;
esac
exit 0
`
	if err := os.WriteFile(filepath.Join(d.dir, "docker"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", d.dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return d
}

func (d *cdsDocker) write(rel, content string) {
	d.t.Helper()
	if err := os.WriteFile(filepath.Join(d.dir, rel), []byte(content), 0o644); err != nil {
		d.t.Fatal(err)
	}
}

// container records a container: status, exit code and labels.
func (d *cdsDocker) container(name, status string, exit int, labels map[string]string) {
	var l []string
	for k, v := range labels {
		l = append(l, fmt.Sprintf("%q:%q", k, v))
	}
	d.write("c/"+name+".json", fmt.Sprintf(`[{"Name":"/%s","Image":%q,"Config":{"Image":"img","Labels":{%s}},"State":{"Status":%q,"ExitCode":%d,"StartedAt":%q}}]`,
		name, testImageID, strings.Join(l, ","), status, exit, time.Now().UTC().Format(time.RFC3339Nano)))
}

func (d *cdsDocker) calls() []string {
	b, _ := os.ReadFile(filepath.Join(d.dir, "calls.log"))
	return strings.Split(strings.TrimSpace(string(b)), "\n")
}

// runOf is the `docker run` call that created name ("" if none).
func (d *cdsDocker) runOf(name string) string {
	for _, c := range d.calls() {
		if strings.HasPrefix(c, "run ") && strings.Contains(c, " --name "+name+" ") {
			return c
		}
	}
	return ""
}

func (d *cdsDocker) index(prefix string) int {
	for i, c := range d.calls() {
		if strings.HasPrefix(c, prefix) {
			return i
		}
	}
	return -1
}

// cdsContainer is the local MCP container of instance sltest, answering on
// a fake MCP server.
func cdsContainer(t *testing.T, env ...string) *Container {
	t.Helper()
	t.Setenv("SONARLESS_HOME", t.TempDir())
	t.Setenv("SONARLESS_INSTANCE", "sltest")
	t.Setenv(config.MCPAppCDS, "")
	for i := 0; i+1 < len(env); i += 2 {
		t.Setenv(env[i], env[i+1])
	}
	cfg, _, err := config.Load("")
	if err != nil {
		t.Fatal(err)
	}
	c := NewLocal(cfg, nil)
	c.Port = fakeMCP(t, true).Port
	return c
}

func testKey(c *Container) string { return cdsKey(testImageID, jvmOpts(c.Cfg)) }

func TestNoArchiveStartsPlainThenBuildsInTheBackground(t *testing.T) {
	d := newCDSDocker(t)
	c := cdsContainer(t)
	if err := c.Ensure(context.Background()); err != nil {
		t.Fatal(err)
	}
	mcpRun := d.runOf("sltest-mcp")
	if mcpRun == "" || strings.Contains(mcpRun, "SharedArchiveFile") || strings.Contains(mcpRun, "--mount") {
		t.Fatalf("MCP container not started plain: %q", mcpRun)
	}
	build := d.runOf("sltest-cds")
	if build == "" {
		t.Fatalf("no archive build started; calls:\n%s", strings.Join(d.calls(), "\n"))
	}
	if d.index("run -d --init --name sltest-mcp ") > d.index("run -d --init --name sltest-cds ") {
		t.Error("the build started before the MCP container")
	}
	for _, want := range []string{
		"run -d --init --name sltest-cds ",
		" --label sonarless-mcp.cds-build=" + testKey(c) + " ",
		" --mount type=volume,src=sltest-cds,dst=/app/storage ",
		" -e CDS_OPTS=" + c.Cfg.Get(config.MCPJavaOpts) + " ",
		" -e SONARQUBE_URL=http://sltest-server:9000 ",
		" --entrypoint sh " + testImageID + " -c ",
	} {
		if !strings.Contains(build, want) {
			t.Errorf("build run lacks %q:\n%s", want, build)
		}
	}
	if strings.Contains(build, " -p ") {
		t.Error("the build container publishes a port")
	}
	if got := ArchiveStatus(context.Background(), c.Cfg); got.State != CDSBuilding || got.Key != testKey(c) {
		t.Errorf("status while building = %+v", got)
	}
}

func TestFinishedArchiveIsUsedOnTheNextStart(t *testing.T) {
	d := newCDSDocker(t)
	c := cdsContainer(t)
	key := testKey(c)
	d.write("v/sltest-cds", "")
	writeCDS(c.Cfg, cdsRecord{Key: key, State: CDSReady})
	// Stopped, created plain before the archive existed.
	d.container("sltest-mcp", "exited", 143, map[string]string{"sonarless-mcp.spec": c.spec()})

	if err := c.Ensure(context.Background()); err != nil {
		t.Fatal(err)
	}
	run := d.runOf("sltest-mcp")
	for _, want := range []string{
		" --label sonarless-mcp.cds=" + key + " ",
		" --mount type=volume,src=sltest-cds,dst=/cds,readonly ",
		"-XX:SharedArchiveFile=/cds/" + key + ".jsa -Xshare:auto -XX:+VerifySharedSpaces ",
	} {
		if !strings.Contains(run, want) {
			t.Errorf("MCP run lacks %q:\n%s", want, run)
		}
	}
	if d.runOf("sltest-cds") != "" {
		t.Error("a build started although the archive is finished")
	}
	if got := ArchiveStatus(context.Background(), c.Cfg); got.State != CDSUsed {
		t.Errorf("status = %+v, want used", got)
	}
}

func TestRunningContainerIsNotSwappedForAnArchive(t *testing.T) {
	d := newCDSDocker(t)
	c := cdsContainer(t)
	d.write("v/sltest-cds", "")
	writeCDS(c.Cfg, cdsRecord{Key: testKey(c), State: CDSReady})
	d.container("sltest-mcp", "running", 0, map[string]string{"sonarless-mcp.spec": c.spec()})
	if err := c.Ensure(context.Background()); err != nil {
		t.Fatal(err)
	}
	if run := d.runOf("sltest-mcp"); run != "" {
		t.Errorf("running container recreated: %s", run)
	}
	if got := ArchiveStatus(context.Background(), c.Cfg); got.State != CDSReady {
		t.Errorf("status = %+v, want ready (used from the next start)", got)
	}
}

func TestRejectedArchiveFallsBackToPlain(t *testing.T) {
	for name, warning := range map[string]string{
		"stale":     "[0.056s][warning][cds] /app/sonarqube-mcp-server.jar timestamp has changed.",
		"corrupt":   "[0.020s][warning][cds] Checksum verification failed.",
		"truncated": "[0.003s][warning][cds] The shared archive file has been truncated.",
	} {
		t.Run(name, func(t *testing.T) {
			d := newCDSDocker(t)
			c := cdsContainer(t)
			key := testKey(c)
			d.write("v/sltest-cds", "")
			writeCDS(c.Cfg, cdsRecord{Key: key, State: CDSReady})
			d.write("logs/sltest-mcp", "Picked up JAVA_TOOL_OPTIONS: -Xmx64m\n"+warning+"\nINFO SonarQube MCP Server started\n")
			if err := c.Ensure(context.Background()); err != nil {
				t.Fatal(err)
			}
			got := ArchiveStatus(context.Background(), c.Cfg)
			if got.State != CDSFailed || !strings.Contains(got.Detail, warning) {
				t.Fatalf("status after a rejected archive = %+v", got)
			}
			if !strings.Contains(strings.Join(d.calls(), "\n"), "logs --since 2026-11-25T10:00:00.123456789Z sltest-mcp") {
				t.Errorf("logs not read from this start on:\n%s", strings.Join(d.calls(), "\n"))
			}

			// The container stops (idle); the next start runs plain and
			// builds nothing: a rejection is not retried in a loop.
			d.container("sltest-mcp", "exited", 143, map[string]string{"sonarless-mcp.spec": c.spec(), LabelCDS: key})
			before := len(d.calls())
			if err := c.Ensure(context.Background()); err != nil {
				t.Fatal(err)
			}
			calls := strings.Join(d.calls()[before:], "\n")
			if !strings.Contains(calls, "run -d --init --name sltest-mcp ") || strings.Contains(calls, "SharedArchiveFile") {
				t.Errorf("start after the rejection did not recreate the container plain:\n%s", calls)
			}
			if strings.Contains(calls, "--name sltest-cds") {
				t.Errorf("rejected archive rebuilt at once:\n%s", calls)
			}
		})
	}
}

func TestArchiveOfAnotherImageIsNotUsed(t *testing.T) {
	d := newCDSDocker(t)
	c := cdsContainer(t)
	d.write("v/sltest-cds", "")
	old := cdsKey("sha256:ffffffffffff0000", jvmOpts(c.Cfg))
	writeCDS(c.Cfg, cdsRecord{Key: old, State: CDSReady})
	if err := c.Ensure(context.Background()); err != nil {
		t.Fatal(err)
	}
	if run := d.runOf("sltest-mcp"); strings.Contains(run, "SharedArchiveFile") || strings.Contains(run, "--mount") {
		t.Errorf("archive of another image used: %s", run)
	}
	if build := d.runOf("sltest-cds"); !strings.Contains(build, "cds-build="+testKey(c)) {
		t.Errorf("no build for the new image: %q", build)
	}
}

func TestArchiveKeyFollowsImageAndOptions(t *testing.T) {
	a := cdsKey(testImageID, "-Xmx64m -XX:+UseSerialGC")
	if a != cdsKey(testImageID, " -Xmx64m  -XX:+UseSerialGC ") {
		t.Error("whitespace changes the key")
	}
	if a == cdsKey("sha256:ffffffffffff0000", "-Xmx64m -XX:+UseSerialGC") {
		t.Error("another image has the same key")
	}
	if a == cdsKey(testImageID, "-Xmx128m -XX:+UseSerialGC") {
		t.Error("other JVM options have the same key")
	}
	if !strings.HasPrefix(a, "0123456789ab-") {
		t.Errorf("key %q does not start with the short image ID", a)
	}
}

func TestDisabledNeitherBuildsNorUses(t *testing.T) {
	d := newCDSDocker(t)
	c := cdsContainer(t, config.MCPAppCDS, "false")
	d.write("v/sltest-cds", "")
	writeCDS(c.Cfg, cdsRecord{Key: cdsKey(testImageID, jvmOpts(c.Cfg)), State: CDSReady})
	if err := c.Ensure(context.Background()); err != nil {
		t.Fatal(err)
	}
	calls := strings.Join(d.calls(), "\n")
	if strings.Contains(calls, "sltest-cds") || strings.Contains(calls, "SharedArchiveFile") {
		t.Errorf("AppCDS off, but docker was asked:\n%s", calls)
	}
	if got := ArchiveStatus(context.Background(), c.Cfg); got.State != CDSDisabled {
		t.Errorf("status = %+v, want disabled", got)
	}
	// Turning it off changes the spec, so a running container picks it up.
	on := cdsContainer(t)
	if on.spec() == c.spec() {
		t.Error("SONARLESS_MCP_APPCDS=false does not change the container spec")
	}
}

func TestDefaultKeepsTheSpecOfOlderInstalls(t *testing.T) {
	c := cdsContainer(t)
	// The spec as it was computed before AppCDS existed.
	h := sha256.Sum256([]byte(strings.Join([]string{c.Image, fmt.Sprint(c.Port), c.Cfg.Network(), c.SonarURL,
		c.Cfg.Get(config.MCPToolsets), c.Cfg.Get(config.MCPReadOnly), c.Cfg.Get(config.MCPJavaOpts)}, "\x00")))
	if c.spec() != hex.EncodeToString(h[:8]) {
		t.Error("the default AppCDS setting changed the spec; every running MCP container would be recreated")
	}
}

func TestFinishedBuildIsSettled(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		d := newCDSDocker(t)
		c := cdsContainer(t)
		key := testKey(c)
		d.write("v/sltest-cds", "")
		d.container("sltest-cds", "exited", 0, map[string]string{LabelCDSBuild: key})
		if got := ArchiveStatus(context.Background(), c.Cfg); got.State != CDSReady {
			t.Errorf("status of a finished build = %+v, want ready", got)
		}
		if err := c.Ensure(context.Background()); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(strings.Join(d.calls(), "\n"), "rm -fv sltest-cds") {
			t.Error("finished build container not removed")
		}
		if run := d.runOf("sltest-mcp"); !strings.Contains(run, "SharedArchiveFile=/cds/"+key+".jsa") {
			t.Errorf("start after a finished build does not use the archive: %s", run)
		}
	})
	t.Run("failure", func(t *testing.T) {
		d := newCDSDocker(t)
		c := cdsContainer(t)
		key := testKey(c)
		d.container("sltest-cds", "exited", 1, map[string]string{LabelCDSBuild: key})
		d.write("logs/sltest-cds", "cds: the dump failed\n")
		if err := c.Ensure(context.Background()); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(d.runOf("sltest-mcp"), "SharedArchiveFile") {
			t.Error("archive used after a failed build")
		}
		if d.runOf("sltest-cds") != "" {
			t.Error("failed build retried at once")
		}
		got := ArchiveStatus(context.Background(), c.Cfg)
		if got.State != CDSFailed || !strings.Contains(got.Detail, "cds: the dump failed") || !strings.Contains(got.Detail, "status 1") {
			t.Errorf("status after a failed build = %+v", got)
		}

		// A day later the next start tries again.
		rec := readCDS(c.Cfg)
		rec.At = time.Now().Add(-cdsRetry - time.Minute)
		writeCDS(c.Cfg, rec)
		_ = os.Remove(filepath.Join(d.dir, "c", "sltest-mcp.json"))
		if err := c.Ensure(context.Background()); err != nil {
			t.Fatal(err)
		}
		if d.runOf("sltest-cds") == "" {
			t.Error("failed build not retried after a day")
		}
	})
}

func TestBuildAlreadyRunningIsNotStartedTwice(t *testing.T) {
	d := newCDSDocker(t)
	c := cdsContainer(t)
	d.container("sltest-cds", "running", 0, map[string]string{LabelCDSBuild: testKey(c)})
	if err := c.Ensure(context.Background()); err != nil {
		t.Fatal(err)
	}
	if run := d.runOf("sltest-cds"); run != "" {
		t.Errorf("second build started: %s", run)
	}
}

func TestBuildLosingTheRaceIsNotAFailure(t *testing.T) {
	d := newCDSDocker(t)
	c := cdsContainer(t)
	// Another process created the build container between the check and
	// this run: the fake answers the run with a name conflict.
	d.container("sltest-cds", "running", 0, nil)
	c.startCDSBuild(context.Background(), testKey(c))
	if rec := readCDS(c.Cfg); rec.State != "" {
		t.Errorf("losing the build race recorded %+v", rec)
	}
}

func TestRemoteContainersNeverBuild(t *testing.T) {
	d := newCDSDocker(t)
	c := cdsContainer(t)
	r := NewRemote(c.Cfg, nil, "work", "http://sonar.example", c.Port)
	if err := r.Ensure(context.Background()); err != nil {
		t.Fatal(err)
	}
	if d.runOf("sltest-cds") != "" {
		t.Error("a remote's start built an archive")
	}
}

func TestRemovedVolumeIsRebuilt(t *testing.T) {
	d := newCDSDocker(t)
	c := cdsContainer(t)
	writeCDS(c.Cfg, cdsRecord{Key: testKey(c), State: CDSReady}) // but no volume
	if err := c.Ensure(context.Background()); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(d.runOf("sltest-mcp"), "--mount") {
		t.Error("mounted a volume that does not exist")
	}
	if d.runOf("sltest-cds") == "" {
		t.Error("no rebuild after the volume was removed")
	}
}

func TestCDSRejection(t *testing.T) {
	for line, rejected := range map[string]bool{
		"[0.003s][warning][cds] The shared archive file has been truncated.":                                    true,
		"[0.056s][warning][cds] /app/sonarqube-mcp-server.jar timestamp has changed.":                           true,
		"[0.001s][error][cds] An error has occurred while processing the shared archive file.":                  true,
		"Picked up JAVA_TOOL_OPTIONS: -Xmx64m -XX:SharedArchiveFile=/cds/x.jsa -Xshare:auto":                    false,
		"INFO SonarQube MCP Server - Using discovered analyzers, enabling languages: []":                        false,
		"[0.003s][info][cds] full module graph: disabled because archive was created without full module graph": false,
	} {
		if got := cdsRejection("x\n"+line+"\ny") != ""; got != rejected {
			t.Errorf("%q rejected = %v, want %v", line, got, rejected)
		}
	}
}

// The build script itself, run by sh against fake java, wget and entrypoint:
// it publishes atomically, keeps only the new archive, and leaves no
// temporary file behind when the dump fails (a full disk, say).
func TestBuildScript(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("sh script")
	}
	bin := t.TempDir()
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\n"+body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// The training start writes its class list, then serves until SIGTERM.
	write("entrypoint", `for o in $JAVA_TOOL_OPTIONS; do case "$o" in -XX:DumpLoadedClassList=*) echo java/lang/Object > "${o#*=}" ;; esac; done
trap 'exit 0' TERM
while :; do sleep 0.05; done
`)
	// /health answers once the class list is written, as the real server
	// answers after its startup.
	write("wget", `[ -n "${WGET_FAIL:-}" ] && exit 1
for f in "$CDS_DIR"/.*.classes; do [ -s "$f" ] && exit 0; done
exit 1
`)
	write("java", `for a in "$@"; do case "$a" in -XX:SharedArchiveFile=*) f=${a#*=} ;; esac; done
case "$*" in
*-Xshare:dump*)
  echo partial > "$f"
  [ -n "${DUMP_FAIL:-}" ] && { echo "No space left on device" >&2; exit 1; }
  echo archive > "$f" ;;
esac
exit 0
`)
	run := func(t *testing.T, env ...string) (dir, out string, err error) {
		dir = t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "old.jsa"), []byte("old"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, ".killed.jsa.tmp"), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command("sh", "-c", cdsBuildScript)
		cmd.Env = append(os.Environ(), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"),
			"CDS_DIR="+dir, "CDS_KEY=k1", "CDS_OPTS=-Xmx64m", "CDS_JAR=/x.jar", "CDS_ENTRYPOINT="+filepath.Join(bin, "entrypoint"))
		cmd.Env = append(cmd.Env, env...)
		b, err := cmd.CombinedOutput()
		return dir, string(b), err
	}
	names := func(dir string) string {
		es, _ := os.ReadDir(dir)
		var n []string
		for _, e := range es {
			n = append(n, e.Name())
		}
		return strings.Join(n, " ")
	}

	t.Run("publishes", func(t *testing.T) {
		dir, out, err := run(t)
		if err != nil {
			t.Fatalf("%v: %s", err, out)
		}
		if got := names(dir); got != "k1.jsa" {
			t.Errorf("files after a build: %q, want only k1.jsa", got)
		}
		if b, _ := os.ReadFile(filepath.Join(dir, "k1.jsa")); string(b) != "archive\n" {
			t.Errorf("published archive = %q", b)
		}
	})
	t.Run("dump fails", func(t *testing.T) {
		dir, out, err := run(t, "DUMP_FAIL=1")
		if err == nil || !strings.Contains(out, "cds: the dump failed") || !strings.Contains(out, "No space left") {
			t.Fatalf("failed dump: %v %q", err, out)
		}
		if got := names(dir); got != "old.jsa" {
			t.Errorf("files after a failed dump: %q, want the old archive only", got)
		}
	})
	t.Run("no SonarQube", func(t *testing.T) {
		// CDS_TRIES bounds the wait where no /proc tells a zombie apart.
		dir, out, err := run(t, "WGET_FAIL=1", "CDS_ENTRYPOINT=true", "CDS_TRIES=3")
		if err == nil || !strings.Contains(out, "never answered /health") {
			t.Fatalf("training without a server: %v %q", err, out)
		}
		if got := names(dir); got != "old.jsa" {
			t.Errorf("files after a failed training: %q", got)
		}
	})
}
