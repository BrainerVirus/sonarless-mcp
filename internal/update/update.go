// Package update keeps an installed sonarless-mcp current: a daily background
// check against GitHub Releases downloads the new archive, verifies its
// checksum, smoke-tests the new binary and swaps it in, so the next start
// runs it. Development builds (version "dev", snapshots) never update.
package update

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/BrainerVirus/sonarless-mcp/internal/config"
	"github.com/BrainerVirus/sonarless-mcp/internal/sonar"
)

const (
	repo     = "BrainerVirus/sonarless-mcp"
	interval = 24 * time.Hour
)

// APIBase and DownloadBase are variables so tests can point them at a fake.
var (
	APIBase      = "https://api.github.com"
	DownloadBase = "https://github.com"
)

// Enabled reports whether this build may update itself.
func Enabled(cfg *config.Config, version string) bool {
	if !cfg.Bool(config.AutoUpdate) {
		return false
	}
	_, ok := parse(version)
	return ok && !strings.Contains(version, "SNAPSHOT")
}

func stamp(cfg *config.Config) string { return filepath.Join(cfg.CacheDir, "last-update-check") }

// Due reports whether a daily check is due.
func Due(cfg *config.Config) bool {
	fi, err := os.Stat(stamp(cfg))
	return err != nil || time.Since(fi.ModTime()) > interval
}

// Result describes what Check did.
type Result struct {
	Current, Latest string
	Updated         bool
	Path            string
}

// Check looks up the latest release and, if newer, installs it over the
// running executable. Safe to run concurrently: one runner holds a lock.
func Check(ctx context.Context, cfg *config.Config, current string) (*Result, error) {
	unlock, err := sonar.LockFile(cfg.CacheDir, "update.lock")
	if err != nil {
		return nil, err
	}
	defer unlock()
	_ = os.MkdirAll(cfg.CacheDir, 0o755)
	_ = os.WriteFile(stamp(cfg), nil, 0o644)

	latest, err := latestTag(ctx)
	if err != nil {
		return nil, err
	}
	res := &Result{Current: current, Latest: latest}
	if !Newer(latest, current) {
		return res, nil
	}
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	if r, err := filepath.EvalSymlinks(exe); err == nil {
		exe = r
	}
	res.Path = exe
	if err := install(ctx, latest, exe); err != nil {
		return nil, err
	}
	res.Updated = true
	return res, nil
}

func latestTag(ctx context.Context) (string, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, APIBase+"/repos/"+repo+"/releases/latest", nil)
	req.Header.Set("Accept", "application/vnd.github+json")
	b, err := get(req)
	if err != nil {
		return "", err
	}
	var r struct {
		TagName string `json:"tag_name"`
	}
	if err := json.Unmarshal(b, &r); err != nil || r.TagName == "" {
		return "", fmt.Errorf("unexpected release response: %v", err)
	}
	return r.TagName, nil
}

// AssetName is the release archive for this OS/CPU.
func AssetName(goos, goarch string) string {
	if goos == "windows" {
		return fmt.Sprintf("sonarless-mcp_%s_%s.zip", goos, goarch)
	}
	return fmt.Sprintf("sonarless-mcp_%s_%s.tar.gz", goos, goarch)
}

func install(ctx context.Context, tag, exe string) error {
	base := DownloadBase + "/" + repo + "/releases/download/" + tag + "/"
	asset := AssetName(runtime.GOOS, runtime.GOARCH)
	archive, err := download(ctx, base+asset)
	if err != nil {
		return err
	}
	sums, err := download(ctx, base+"checksums.txt")
	if err != nil {
		return err
	}
	if err := verify(archive, sums, asset); err != nil {
		return err
	}
	bin, err := extract(archive, asset)
	if err != nil {
		return err
	}

	dir := filepath.Dir(exe)
	next := filepath.Join(dir, ".sonarless-mcp.new"+filepath.Ext(exe))
	if err := os.WriteFile(next, bin, 0o755); err != nil {
		return fmt.Errorf("write %s: %w", next, err)
	}
	defer os.Remove(next)
	// Never swap in a binary that can't even report its version.
	out, err := exec.CommandContext(ctx, next, "--version").CombinedOutput()
	if err != nil || !strings.Contains(string(out), strings.TrimPrefix(tag, "v")) {
		return fmt.Errorf("new binary failed its smoke test: %v %s", err, strings.TrimSpace(string(out)))
	}
	// A running Windows exe can't be replaced but can be renamed aside; the
	// same dance is harmless elsewhere and keeps one rollback copy.
	old := exe + ".old"
	_ = os.Remove(old)
	if err := os.Rename(exe, old); err != nil {
		return fmt.Errorf("move current binary aside: %w", err)
	}
	if err := os.Rename(next, exe); err != nil {
		_ = os.Rename(old, exe)
		return fmt.Errorf("install new binary: %w", err)
	}
	if runtime.GOOS != "windows" {
		_ = os.Remove(old) // the running process keeps its open inode
	}
	return nil
}

func download(ctx context.Context, url string) ([]byte, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	return get(req)
}

func get(req *http.Request) ([]byte, error) {
	c := &http.Client{Timeout: 2 * time.Minute}
	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: HTTP %d", req.URL, resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 200<<20))
}

func verify(archive, sums []byte, asset string) error {
	got := sha256.Sum256(archive)
	for _, line := range strings.Split(string(sums), "\n") {
		f := strings.Fields(line)
		if len(f) == 2 && f[1] == asset {
			if f[0] != hex.EncodeToString(got[:]) {
				return fmt.Errorf("checksum mismatch for %s", asset)
			}
			return nil
		}
	}
	return fmt.Errorf("%s not listed in checksums.txt", asset)
}

func extract(archive []byte, asset string) ([]byte, error) {
	if strings.HasSuffix(asset, ".zip") {
		zr, err := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
		if err != nil {
			return nil, err
		}
		for _, f := range zr.File {
			if filepath.Base(f.Name) == "sonarless-mcp.exe" {
				rc, err := f.Open()
				if err != nil {
					return nil, err
				}
				defer rc.Close()
				return io.ReadAll(rc)
			}
		}
		return nil, errors.New("sonarless-mcp.exe not in archive")
	}
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return nil, err
	}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err != nil {
			return nil, errors.New("sonarless-mcp not in archive")
		}
		if filepath.Base(h.Name) == "sonarless-mcp" && h.Typeflag == tar.TypeReg {
			return io.ReadAll(tr)
		}
	}
}

// parse reads "v1.2.3" or "1.2.3" (pre-release suffixes ignored).
func parse(v string) ([3]int, bool) {
	v = strings.TrimPrefix(v, "v")
	if i := strings.IndexAny(v, "-+"); i >= 0 {
		v = v[:i]
	}
	parts := strings.Split(v, ".")
	var out [3]int
	if len(parts) != 3 {
		return out, false
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil {
			return out, false
		}
		out[i] = n
	}
	return out, true
}

// Newer reports whether a is a higher version than b.
func Newer(a, b string) bool {
	x, ok1 := parse(a)
	y, ok2 := parse(b)
	if !ok1 || !ok2 {
		return false
	}
	for i := range x {
		if x[i] != y[i] {
			return x[i] > y[i]
		}
	}
	return false
}
