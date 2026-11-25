package mcp

// AppCDS for the MCP server's JVM (measured in docs/performance.md): a static
// class-data archive built for one MCP image and one set of JVM options. A
// start never waits for it. With no finished archive the container starts
// plain and a one-off container (<instance>-cds, the name is the mutex) builds
// one afterwards; the next start of the MCP container maps it. The archive
// lives in the named volume <instance>-cds, mounted read-only, and the JVM
// runs with -Xshare:auto, so a missing, stale or corrupt archive costs startup
// time, never the start.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/BrainerVirus/sonarless-mcp/internal/config"
	"github.com/BrainerVirus/sonarless-mcp/internal/docker"
	"github.com/BrainerVirus/sonarless-mcp/internal/sonar"
)

// Archive states, as `status` and sonarless_status report them.
const (
	CDSDisabled = "disabled" // SONARLESS_MCP_APPCDS=false
	CDSNone     = "none"     // no archive for this image and options yet
	CDSBuilding = "building" // the one-off build container is running
	CDSReady    = "ready"    // built; the MCP container maps it from its next start
	CDSUsed     = "used"     // the running MCP container was started with it
	CDSFailed   = "failed"   // the build failed or the JVM rejected the archive
)

const (
	// LabelCDS is the archive key an MCP container was created with ("" none).
	LabelCDS = "sonarless-mcp.cds"
	// LabelCDSBuild marks the build container with the key it builds.
	LabelCDSBuild = "sonarless-mcp.cds-build"
	// cdsMount is where the MCP container sees the volume (read-only).
	cdsMount = "/cds"
	// cdsBuildDir is where the build container mounts the volume: the image's
	// own writable dir, so Docker gives a fresh volume that dir's owner (the
	// image's user, appuser 100:101) without a root container.
	cdsBuildDir = "/app/storage"
	// cdsRetry is how long a failed build or a rejected archive is left alone
	// before the next start tries again; a new image or new JVM options
	// (another key) are tried at once.
	cdsRetry = 24 * time.Hour
	// cdsBuildTimeout ends a build that hangs (it takes about 15 s).
	cdsBuildTimeout = 10 * time.Minute
)

// CDSStatus is the archive state of the local MCP server.
type CDSStatus struct {
	State  string `json:"state"`
	Key    string `json:"key,omitempty"`
	Detail string `json:"detail,omitempty"`
}

// cdsRecord is the last build's outcome, <cache dir>/<instance>-cds.json.
type cdsRecord struct {
	Key    string    `json:"key"`
	State  string    `json:"state"` // ready or failed
	Detail string    `json:"detail,omitempty"`
	At     time.Time `json:"at"`
	// Mapped is the start (StartedAt) of the local MCP container whose JVM
	// was seen mapping the archive: `used` is confirmed, not assumed.
	Mapped string `json:"mapped,omitempty"`
}

func cdsFile(cfg *config.Config) string {
	return filepath.Join(cfg.CacheDir, cfg.CDSName()+".json")
}

func readCDS(cfg *config.Config) cdsRecord {
	var r cdsRecord
	if b, err := os.ReadFile(cdsFile(cfg)); err == nil {
		_ = json.Unmarshal(b, &r)
	}
	return r
}

func writeCDS(cfg *config.Config, r cdsRecord) {
	if r.At.IsZero() {
		r.At = time.Now()
	}
	b, _ := json.Marshal(r)
	if err := os.MkdirAll(cfg.CacheDir, 0o755); err != nil {
		return
	}
	tmp := cdsFile(cfg) + ".tmp"
	if os.WriteFile(tmp, b, 0o644) == nil {
		_ = os.Rename(tmp, cdsFile(cfg))
	}
}

// jvmOpts are the MCP JVM's options as JAVA_TOOL_OPTIONS carries them ("" for
// the JVM defaults).
func jvmOpts(cfg *config.Config) string {
	return strings.TrimPrefix(javaToolOptions(cfg.Get(config.MCPJavaOpts)), "JAVA_TOOL_OPTIONS=")
}

// cdsKey names the archive of one image and one set of JVM options: an
// archive only fits the jar it was dumped from, and the dump must run with the
// options of the runs that map it.
func cdsKey(imageID, opts string) string {
	id := strings.TrimPrefix(imageID, "sha256:")
	if len(id) > 12 {
		id = id[:12]
	}
	h := sha256.Sum256([]byte(strings.Join(strings.Fields(opts), " ")))
	return id + "-" + hex.EncodeToString(h[:4])
}

// cdsArchive is the archive's path inside the MCP container.
func cdsArchive(key string) string { return cdsMount + "/" + key + ".jsa" }

// cdsRunOpts are the JVM options of a run that maps the archive. -Xshare:auto
// (the JVM's default, spelled out) runs without the archive when it does not
// fit, instead of failing the start; -XX:+VerifySharedSpaces checksums it
// first (about 60 ms), so a corrupt archive is rejected the same way instead
// of being mapped.
func cdsRunOpts(opts, key string) string {
	return strings.TrimSpace(opts + " -XX:SharedArchiveFile=" + cdsArchive(key) + " -Xshare:auto -XX:+VerifySharedSpaces")
}

// cdsPlan is what one start does about the archive.
type cdsPlan struct {
	key   string // archive key for this image and options; "" when off
	use   bool   // a finished archive for key exists: map it
	build bool   // none and none on its way: build one after the start
}

func (p cdsPlan) label() string {
	if p.use {
		return p.key
	}
	return ""
}

// planCDS decides what a start of an MCP container running imageID does
// about the archive. For the archive's owner, the local container, it first
// settles the last build (see reapCDS) and may reset the record; any other
// container only reads the record and maps a ready archive, so a remote never
// changes what the local container sees.
func planCDS(ctx context.Context, cfg *config.Config, imageID string, owner bool) cdsPlan {
	if !cfg.Bool(config.MCPAppCDS) || imageID == "" {
		return cdsPlan{}
	}
	p := cdsPlan{key: cdsKey(imageID, jvmOpts(cfg))}
	if !owner {
		rec := readCDS(cfg)
		p.use = rec.Key == p.key && rec.State == CDSReady && docker.VolumeExists(ctx, cfg.CDSName())
		return p
	}
	rec, building := reapCDS(ctx, cfg)
	switch {
	case building:
	case rec.Key == p.key && rec.State == CDSReady:
		if docker.VolumeExists(ctx, cfg.CDSName()) {
			p.use = true
		} else { // the volume was removed: build again
			writeCDS(cfg, cdsRecord{})
			p.build = true
		}
	case rec.Key == p.key && rec.State == CDSFailed && time.Since(rec.At) < cdsRetry:
	default:
		p.build = true
	}
	return p
}

// buildState reads the build container without changing anything: whether it
// runs, and for an exited one the record it leaves.
func buildState(ctx context.Context, cfg *config.Config) (c *docker.Container, running bool, rec *cdsRecord) {
	c, err := docker.Inspect(ctx, cfg.CDSName())
	if err != nil || c.Labels[LabelCDSBuild] == "" {
		return nil, false, nil
	}
	key := c.Labels[LabelCDSBuild]
	switch {
	case c.Running():
		return c, true, nil
	case c.Status == "created": // never started (docker run failed): nothing to settle
		return c, false, &cdsRecord{Key: key, State: CDSFailed, Detail: "the build container did not start"}
	case c.ExitCode == 0:
		return c, false, &cdsRecord{Key: key, State: CDSReady}
	}
	detail := fmt.Sprintf("the build exited with status %d", c.ExitCode)
	if line := lastLine(containerLogs(ctx, cfg.CDSName(), "")); line != "" {
		detail += ": " + line
	}
	return c, false, &cdsRecord{Key: key, State: CDSFailed, Detail: detail}
}

// reapCDS records the outcome of a finished build and removes its container,
// ends a build that hangs, and returns the record and whether a build runs.
func reapCDS(ctx context.Context, cfg *config.Config) (cdsRecord, bool) {
	c, running, rec := buildState(ctx, cfg)
	if c != nil && running {
		started, err := time.Parse(time.RFC3339Nano, c.StartedAt)
		if err != nil || time.Since(started) < cdsBuildTimeout {
			return readCDS(cfg), true
		}
		rec = &cdsRecord{Key: c.Labels[LabelCDSBuild], State: CDSFailed, Detail: "the build took longer than " + cdsBuildTimeout.String()}
	}
	if rec != nil {
		writeCDS(cfg, *rec)
		_ = docker.Remove(ctx, cfg.CDSName())
	}
	return readCDS(cfg), false
}

// containerLogs is a container's output, stdout and stderr, since an RFC 3339
// time ("" for all of it).
func containerLogs(ctx context.Context, name, since string) string {
	args := []string{"logs"}
	if since != "" {
		args = append(args, "--since", since)
	}
	var out bytes.Buffer
	_ = docker.Stream(ctx, &out, append(args, name)...)
	return out.String()
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}

// cdsRejection is the JVM's first complaint about the archive in a run's
// output ("" when it mapped it): with -Xshare:auto every reason to run
// without it (a changed jar, a bad checksum, a truncated file) is a
// [warning][cds] line.
func cdsRejection(logs string) string {
	for _, line := range strings.Split(logs, "\n") {
		if strings.Contains(line, "][cds") && (strings.Contains(line, "[warning]") || strings.Contains(line, "[error]")) {
			return strings.TrimSpace(line)
		}
	}
	return ""
}

// checkArchive looks at the start of a container created or started with the
// archive key: a rejection in the JVM's log, else whether the JVM maps the
// file. A rejected or unmapped archive is recorded as failed, so the next start
// runs plain and the status says why; a mapped one confirms `used`.
func (m *Container) checkArchive(ctx context.Context, key string) {
	if !m.BuildCDS { // only the local container's starts speak for the record
		return
	}
	c, err := docker.Inspect(ctx, m.name())
	if err != nil {
		return
	}
	why := cdsRejection(containerLogs(ctx, m.name(), c.StartedAt))
	if why != "" {
		why = "the JVM did not use the archive: " + why
	} else {
		switch mapped, unknown := archiveMapped(ctx, m.name(), key); {
		case unknown != "":
			if m.Log != nil {
				fmt.Fprintf(m.Log, "Could not check that %s maps the MCP class-data archive: %s\n", m.name(), unknown)
			}
			return
		case mapped:
			if rec := readCDS(m.Cfg); rec.Key == key && rec.State == CDSReady {
				rec.Mapped = c.StartedAt
				writeCDS(m.Cfg, rec)
			}
			return
		}
		why = "the JVM did not map " + cdsArchive(key) + " (the file is missing or unreadable)"
	}
	writeCDS(m.Cfg, cdsRecord{Key: key, State: CDSFailed, Detail: why})
	if m.Log != nil {
		fmt.Fprintf(m.Log, "MCP class-data archive not used (%s); %s runs without it\n", why, m.name())
	}
}

// archiveMapped reports whether a process in the running container maps the
// archive. One busybox sh and grep over /proc/*/maps does it; an image without
// them is read with cat alone: the JVM is the child of Docker's init (PID 1,
// --init). unknown says why neither worked.
func archiveMapped(ctx context.Context, name, key string) (mapped bool, unknown string) {
	path := cdsArchive(key)
	out, status, err := docker.Exec(ctx, name, "sh", "-c", "grep -l -F '"+path+"' /proc/[0-9]*/maps 2>/dev/null")
	switch {
	case err != nil:
		return false, err.Error()
	case strings.TrimSpace(out) != "":
		return true, ""
	case status == 1 || status == 2: // grep ran and found nothing
		return false, ""
	case status != 126 && status != 127:
		return false, fmt.Sprintf("docker exec exited with status %d", status)
	}
	kids, status, err := docker.Exec(ctx, name, "cat", "/proc/1/task/1/children")
	if err != nil || status != 0 {
		return false, "the image has neither sh and grep nor cat to read /proc with"
	}
	for _, pid := range strings.Fields(kids) {
		if maps, status, err := docker.Exec(ctx, name, "cat", "/proc/"+pid+"/maps"); err == nil && status == 0 && strings.Contains(maps, path) {
			return true, ""
		}
	}
	return false, ""
}

// startCDSBuild starts the one-off container that builds the archive for key,
// unless one already runs (its fixed name is the lock between processes). It
// returns at once: the build takes about 15 s and the MCP server is already up.
func (m *Container) startCDSBuild(ctx context.Context, key string) {
	if !m.BuildCDS {
		return
	}
	name := m.Cfg.CDSName()
	if _, err := docker.Run(ctx, "volume", "create", "--label", sonar.LabelManaged+"=true", "--label", InstanceLabel(m.Cfg), name); err != nil {
		writeCDS(m.Cfg, cdsRecord{Key: key, State: CDSFailed, Detail: err.Error()})
		return
	}
	imageID := docker.ImageID(ctx, m.Image)
	args := []string{"run", "-d", "--init", "--name", name,
		"--network", m.Cfg.Network(),
		"--label", sonar.LabelManaged + "=true",
		"--label", LabelCDSBuild + "=" + key,
		"--mount", "type=volume,src=" + name + ",dst=" + cdsBuildDir,
		"-e", "SONARQUBE_TRANSPORT=http",
		"-e", "SONARQUBE_HTTP_HOST=127.0.0.1",
		"-e", "SONARQUBE_URL=" + m.SonarURL,
		"-e", "SONARQUBE_LOG_TO_FILE_DISABLED=true",
		"-e", "STORAGE_PATH=/tmp/storage", // the volume holds archives only
		"-e", "CDS_DIR=" + cdsBuildDir,
		"-e", "CDS_KEY=" + key,
		"-e", "CDS_OPTS=" + jvmOpts(m.Cfg),
	}
	if env := toolsetsEnv(m.Cfg.Get(config.MCPToolsets)); env != "" {
		args = append(args, "-e", env)
	}
	if m.Cfg.Bool(config.MCPReadOnly) {
		args = append(args, "-e", "SONARQUBE_READ_ONLY=true")
	}
	args = append(args, "--entrypoint", "sh", imageID, "-c", cdsBuildScript)
	_, err := docker.Run(ctx, args...)
	switch {
	case err == nil:
		if m.Log != nil {
			fmt.Fprintf(m.Log, "Building the MCP class-data archive in the background (%s)...\n", name)
		}
	case strings.Contains(err.Error(), "already in use"):
		// Another process started the build first.
	default:
		_ = docker.Remove(ctx, name)
		writeCDS(m.Cfg, cdsRecord{Key: key, State: CDSFailed, Detail: err.Error()})
	}
}

// cdsBuildScript runs in the build container (the MCP image's busybox sh): a
// training start of the MCP server that lists the classes it loads, then
// -Xshare:dump with the same options into a temporary file, a test map of the
// result, and a rename, so the MCP container only ever sees a whole archive.
// It removes every other archive (only the current image's is kept) and any
// temporary file a killed build left. The training start needs SonarQube:
// the server checks it before it answers /health.
const cdsBuildScript = `set -u
d=$CDS_DIR key=$CDS_KEY
jar=${CDS_JAR:-/app/sonarqube-mcp-server.jar}
out="$d/$key.jsa" tmp="$d/.$key.jsa.tmp" lst="$d/.$key.classes"
fail() { echo "cds: $*" >&2; exit 1; }
# A finished background job stays a zombie until waited for, and kill -0
# still reaches a zombie; /proc tells them apart (kill -0 where there is none).
alive() {
  if [ -d /proc/self ]; then
    [ -r "/proc/$1/status" ] && ! grep -q '^State:[[:space:]]*Z' "/proc/$1/status"
  else
    kill -0 "$1" 2>/dev/null
  fi
}
rm -f "$d"/.*.tmp "$d"/.*.classes
trap 'rm -f "$tmp" "$lst"' EXIT
JAVA_TOOL_OPTIONS="$CDS_OPTS -XX:DumpLoadedClassList=$lst" ${CDS_ENTRYPOINT:-/usr/local/bin/docker-entrypoint} >/tmp/cds-train.log 2>&1 &
pid=$!
i=0
until wget -q -O /dev/null "${CDS_HEALTH:-http://127.0.0.1:8080}/health" 2>/dev/null; do
  i=$((i + 1))
  if [ "$i" -gt "${CDS_TRIES:-600}" ] || ! alive "$pid"; then
    tail -n 3 /tmp/cds-train.log >&2
    kill "$pid" 2>/dev/null
    fail "the training start never answered /health (is SonarQube reachable?)"
  fi
  sleep 0.2
done
wget -q -O /dev/null --header 'Content-Type: application/json' --header 'Accept: application/json, text/event-stream' \
  --post-data '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"sonarless-cds","version":"0"}}}' \
  "${CDS_HEALTH:-http://127.0.0.1:8080}/mcp" 2>/dev/null
kill "$pid"
wait "$pid"
[ -s "$lst" ] || fail "the training start wrote no class list"
java $CDS_OPTS -Xshare:dump -XX:SharedClassListFile="$lst" -XX:SharedArchiveFile="$tmp" -cp "$jar" >/tmp/cds-dump.log 2>&1 ||
  { tail -n 3 /tmp/cds-dump.log >&2; fail "the dump failed"; }
java $CDS_OPTS -Xshare:on -XX:+VerifySharedSpaces -XX:SharedArchiveFile="$tmp" -cp "$jar" -version >/dev/null 2>&1 ||
  fail "the new archive does not map"
mv -f "$tmp" "$out" || fail "could not publish the archive"
for f in "$d"/*.jsa; do [ "$f" = "$out" ] || rm -f "$f"; done
echo "cds: built $out from $(wc -l <"$lst") classes"
`

// ArchiveStatus is the local MCP server's archive state, for `status` and
// sonarless_status. It changes nothing: a finished build is settled by the
// next start.
func ArchiveStatus(ctx context.Context, cfg *config.Config) CDSStatus {
	if !cfg.Bool(config.MCPAppCDS) {
		return CDSStatus{State: CDSDisabled, Detail: config.MCPAppCDS + "=false"}
	}
	key := ""
	if id := docker.ImageID(ctx, cfg.Get(config.MCPImage)); id != "" {
		key = cdsKey(id, jvmOpts(cfg))
	}
	c, running, rec := buildState(ctx, cfg)
	if running {
		return CDSStatus{State: CDSBuilding, Key: c.Labels[LabelCDSBuild], Detail: "started " + c.StartedAt}
	}
	if rec == nil {
		r := readCDS(cfg)
		rec = &r
	}
	switch {
	case rec.Key == "" || rec.Key != key:
		return CDSStatus{State: CDSNone, Key: key, Detail: "built in the background after the next MCP start"}
	case rec.State == CDSFailed:
		return CDSStatus{State: CDSFailed, Key: key, Detail: rec.Detail + "; tried again after " + rec.At.Add(cdsRetry).Format(time.DateTime) + " or with a new image"}
	}
	if mc, err := docker.Inspect(ctx, cfg.MCPContainer()); err == nil && mc.Running() && mc.Labels[LabelCDS] == key {
		if rec.Mapped != "" && rec.Mapped == mc.StartedAt {
			return CDSStatus{State: CDSUsed, Key: key}
		}
		return CDSStatus{State: CDSReady, Key: key, Detail: "the running MCP server was started with it, but its JVM was not seen mapping it; checked again at its next start"}
	}
	return CDSStatus{State: CDSReady, Key: key, Detail: "the MCP server uses it from its next start"}
}

// RebuildArchive drops the archive record and builds the archive for the
// current MCP image and JVM options now, in the background build container;
// with wait it returns once the build ended. The SonarQube server must be up
// (the training start needs it). The MCP container m maps the new archive
// from its next start.
func RebuildArchive(ctx context.Context, m *Container, wait bool) (CDSStatus, error) {
	cfg := m.Cfg
	if !cfg.Bool(config.MCPAppCDS) {
		return CDSStatus{}, fmt.Errorf("the MCP class-data archive is off (%s=false)", config.MCPAppCDS)
	}
	imageID := docker.ImageID(ctx, m.Image)
	if imageID == "" {
		return CDSStatus{}, fmt.Errorf("MCP image %s is not pulled yet; use the MCP server once first", m.Image)
	}
	unlock, err := sonar.LockFile(cfg.CacheDir, m.name()+".lock")
	if err != nil {
		return CDSStatus{}, err
	}
	if _, running, _ := buildState(ctx, cfg); !running {
		_ = docker.Remove(ctx, cfg.CDSName()) // a finished build's container
		if err := os.Remove(cdsFile(cfg)); err != nil && !errors.Is(err, os.ErrNotExist) {
			unlock()
			return CDSStatus{}, err
		}
		m.startCDSBuild(ctx, cdsKey(imageID, jvmOpts(cfg)))
	}
	unlock()
	for deadline := time.Now().Add(cdsBuildTimeout); wait && time.Now().Before(deadline); {
		if _, running, _ := buildState(ctx, cfg); !running {
			reapCDS(ctx, cfg)
			break
		}
		select {
		case <-ctx.Done():
			return CDSStatus{}, ctx.Err()
		case <-time.After(cdsPoll):
		}
	}
	return ArchiveStatus(ctx, cfg), nil
}

// cdsPoll is how often RebuildArchive looks at the build container.
var cdsPoll = time.Second
