package update

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestNewer(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"v0.2.0", "0.1.9", true}, {"v1.0.0", "v0.9.9", true}, {"v0.1.0", "0.1.0", false},
		{"v0.1.0", "0.2.0", false}, {"v0.1.1", "0.1.0-SNAPSHOT-abc", true}, {"v1.0.0", "dev", false},
	}
	for _, c := range cases {
		if got := Newer(c.a, c.b); got != c.want {
			t.Errorf("Newer(%q, %q) = %v", c.a, c.b, got)
		}
	}
}

// fakeRelease serves a release whose binary is a shell script printing version.
func fakeRelease(t *testing.T, tag, binVersion string, corrupt bool) {
	t.Helper()
	script := fmt.Sprintf("#!/bin/sh\necho 'sonarless-mcp version %s'\n", binVersion)
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	_ = tw.WriteHeader(&tar.Header{Name: "sonarless-mcp", Mode: 0o755, Size: int64(len(script)), Typeflag: tar.TypeReg})
	_, _ = tw.Write([]byte(script))
	_ = tw.Close()
	_ = gz.Close()
	archive := buf.Bytes()
	sum := sha256.Sum256(archive)
	asset := AssetName(runtime.GOOS, runtime.GOARCH)
	if corrupt {
		archive = append(archive, 0)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/releases/latest"):
			fmt.Fprintf(w, `{"tag_name":%q}`, tag)
		case strings.HasSuffix(r.URL.Path, "/"+asset):
			_, _ = w.Write(archive)
		case strings.HasSuffix(r.URL.Path, "/checksums.txt"):
			fmt.Fprintf(w, "%s  %s\n", hex.EncodeToString(sum[:]), asset)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	APIBase, DownloadBase = srv.URL, srv.URL
	t.Cleanup(func() { APIBase, DownloadBase = "https://api.github.com", "https://github.com" })
}

func currentExe(t *testing.T) string {
	t.Helper()
	exe := filepath.Join(t.TempDir(), "sonarless-mcp")
	if err := os.WriteFile(exe, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	return exe
}

func TestInstall(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake binary is a shell script")
	}
	fakeRelease(t, "v0.3.0", "0.3.0", false)
	exe := currentExe(t)
	if err := install(context.Background(), "v0.3.0", exe); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(exe)
	if !strings.Contains(string(b), "0.3.0") {
		t.Errorf("binary not replaced: %q", b)
	}
	if fi, _ := os.Stat(exe); fi.Mode().Perm()&0o100 == 0 {
		t.Error("not executable")
	}
	if entries, _ := os.ReadDir(filepath.Dir(exe)); len(entries) != 1 {
		t.Errorf("leftover files: %v", entries)
	}
}

func TestInstallRejectsBadChecksum(t *testing.T) {
	fakeRelease(t, "v0.3.0", "0.3.0", true)
	exe := currentExe(t)
	if err := install(context.Background(), "v0.3.0", exe); err == nil || !strings.Contains(err.Error(), "checksum") {
		t.Fatalf("err = %v", err)
	}
	if b, _ := os.ReadFile(exe); string(b) != "old" {
		t.Error("binary changed despite bad checksum")
	}
}

func TestInstallRejectsBrokenBinary(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake binary is a shell script")
	}
	fakeRelease(t, "v0.3.0", "0.2.9", false) // reports the wrong version
	exe := currentExe(t)
	if err := install(context.Background(), "v0.3.0", exe); err == nil || !strings.Contains(err.Error(), "smoke test") {
		t.Fatalf("err = %v", err)
	}
	if b, _ := os.ReadFile(exe); string(b) != "old" {
		t.Error("binary changed despite failed smoke test")
	}
}
