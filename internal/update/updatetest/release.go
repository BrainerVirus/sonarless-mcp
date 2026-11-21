// Ported from ytrack (github.com/BrainerVirus/youtrack-cli,
// internal/upgrade/upgradetest/release.go).

package updatetest

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/BrainerVirus/sonarless-mcp/internal/update"
)

// Platforms are the os/arch pairs .goreleaser.yaml builds.
var Platforms = []string{"linux/amd64", "linux/arm64", "darwin/amd64", "darwin/arm64", "windows/amd64", "windows/arm64"}

// Release describes a fake sonarless-mcp release.
type Release struct {
	Version    string // without "v"
	Prerelease bool
	// PublishedAt is the release's publication time; zero means OldRelease.
	PublishedAt time.Time
	// Binary returns the executable for a platform ("linux/amd64"); nil
	// means a marker text naming the version and platform, which Runner
	// echoes as the binary's `version` output.
	Binary func(platform string) []byte
	// Subject is the signer identity; ReleaseSubject when empty.
	Subject string
	// Issuer is the OIDC issuer; GitHubIssuer when empty.
	Issuer string
	// WrongChecksums lists a wrong digest for every archive.
	WrongChecksums bool
	// SignOther makes the bundle sign different bytes than the checksums file.
	SignOther bool
	// Unsigned leaves out the signature bundle, as releases before signing.
	Unsigned bool
}

// BinaryFor returns the binary content of r for platform.
func (r *Release) BinaryFor(platform string) []byte {
	if r.Binary != nil {
		return r.Binary(platform)
	}
	return []byte(fmt.Sprintf("sonarless-mcp version %s %s\n", r.Version, platform))
}

// OldRelease is the publication time of a Release without PublishedAt: long
// past any soak period.
var OldRelease = time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)

// Published returns r.PublishedAt, or OldRelease when it is zero.
func (r *Release) Published() time.Time {
	if r.PublishedAt.IsZero() {
		return OldRelease
	}
	return r.PublishedAt
}

// Tag returns "v" + Version.
func (r *Release) Tag() string { return "v" + r.Version }

// Assets builds the release files: one archive per platform, the checksums
// file and its Sigstore bundle, signed with s.
func (r *Release) Assets(t *testing.T, s *Sigstore) map[string][]byte {
	t.Helper()
	files := map[string][]byte{}
	var sums strings.Builder
	for _, p := range Platforms {
		goos, goarch, _ := strings.Cut(p, "/")
		name := update.ArchiveName(goos, goarch)
		data := archive(t, name, r.BinaryFor(p))
		files[name] = data
		sum := sha256.Sum256(data)
		digest := hex.EncodeToString(sum[:])
		if r.WrongChecksums {
			other := sha256.Sum256(append(data, 'x'))
			digest = hex.EncodeToString(other[:])
		}
		fmt.Fprintf(&sums, "%s  %s\n", digest, name)
	}
	checksums := []byte(sums.String())
	files[update.ChecksumsName] = checksums
	if !r.Unsigned {
		subject, issuer := r.Subject, r.Issuer
		if subject == "" {
			subject = ReleaseSubject
		}
		if issuer == "" {
			issuer = GitHubIssuer
		}
		signed := checksums
		if r.SignOther {
			signed = append([]byte("other "), checksums...)
		}
		files[update.BundleName] = s.Sign(signed, subject, issuer)
	}
	return files
}

func archive(t *testing.T, name string, bin []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	if strings.HasSuffix(name, ".zip") {
		zw := zip.NewWriter(&buf)
		for _, f := range []struct {
			name string
			data []byte
		}{{"LICENSE", []byte("MIT\n")}, {"README.md", []byte("# sonarless-mcp\n")}, {"sonarless-mcp.exe", bin}} {
			w, err := zw.Create(f.name)
			if err != nil {
				t.Fatal(err)
			}
			_, _ = w.Write(f.data)
		}
		if err := zw.Close(); err != nil {
			t.Fatal(err)
		}
		return buf.Bytes()
	}
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, f := range []struct {
		name string
		data []byte
		mode int64
	}{
		{"LICENSE", []byte("MIT\n"), 0o644},
		{"README.md", []byte("# sonarless-mcp\n"), 0o644},
		{"sonarless-mcp", bin, 0o755},
	} {
		if err := tw.WriteHeader(&tar.Header{Name: f.name, Mode: f.mode, Size: int64(len(f.data)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		_, _ = tw.Write(f.data)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// Server is a fake GitHub serving the releases API and release downloads
// over TLS. Use Server.Client() as the HTTP client and Server.URL as the API
// root.
//
// Paths: release download URLs are /download/<tag>/<name>; asset API URLs
// are /repos/BrainerVirus/sonarless-mcp/releases/assets/<id>, which with
// Accept: application/octet-stream redirect to /objects/<tag>/<name> (as
// GitHub redirects to its object storage).
type Server struct {
	*httptest.Server
	mu       sync.Mutex
	requests []string
	releases []*Release
	assets   map[string]map[string][]byte // tag → name → data
	ids      []assetKey                   // asset id - 1 → asset
	handle   func(w http.ResponseWriter, r *http.Request) bool
}

type assetKey struct{ tag, name string }

// DownloadPath is the path of the release download URL of name.
func DownloadPath(tag, name string) string { return "/download/" + tag + "/" + name }

// ObjectPath is where the asset API URL of name redirects.
func ObjectPath(tag, name string) string { return "/objects/" + tag + "/" + name }

const releasesAPI = "/repos/" + update.Repo + "/releases/"

// Handle installs h to see every request first; it answers the request by
// returning true (fault injection), or leaves it to the fake by returning
// false.
func (s *Server) Handle(h func(w http.ResponseWriter, r *http.Request) bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.handle = h
}

// NewServer serves releases; the last one that is not a prerelease is the
// latest.
func NewServer(t *testing.T, s *Sigstore, releases ...*Release) *Server {
	t.Helper()
	srv := &Server{releases: releases, assets: map[string]map[string][]byte{}}
	for _, r := range releases {
		srv.assets[r.Tag()] = r.Assets(t, s)
		for _, n := range sortedNames(srv.assets[r.Tag()]) {
			srv.ids = append(srv.ids, assetKey{r.Tag(), n})
		}
	}
	srv.Server = httptest.NewTLSServer(http.HandlerFunc(srv.serve))
	t.Cleanup(srv.Close)
	return srv
}

// Requests returns the paths requested so far.
func (s *Server) Requests() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.requests...)
}

// Downloads returns the requested paths that fetched a release file.
func (s *Server) Downloads() []string {
	var out []string
	for _, p := range s.Requests() {
		if strings.HasPrefix(p, "/download/") || strings.HasPrefix(p, "/objects/") {
			out = append(out, p)
		}
	}
	return out
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.requests = append(s.requests, r.URL.Path)
	h := s.handle
	s.mu.Unlock()
	if h != nil && h(w, r) {
		return
	}
	switch {
	case r.URL.Path == releasesAPI+"latest":
		var latest *Release
		for _, rel := range s.releases {
			if !rel.Prerelease {
				latest = rel
			}
		}
		if latest == nil {
			http.NotFound(w, r)
			return
		}
		s.writeRelease(w, latest)
	case strings.HasPrefix(r.URL.Path, releasesAPI+"tags/"):
		tag := strings.TrimPrefix(r.URL.Path, releasesAPI+"tags/")
		for _, rel := range s.releases {
			if rel.Tag() == tag {
				s.writeRelease(w, rel)
				return
			}
		}
		http.NotFound(w, r)
	case strings.HasPrefix(r.URL.Path, releasesAPI+"assets/"):
		id, err := strconv.Atoi(strings.TrimPrefix(r.URL.Path, releasesAPI+"assets/"))
		if err != nil || id < 1 || id > len(s.ids) {
			http.NotFound(w, r)
			return
		}
		a := s.ids[id-1]
		if r.Header.Get("Accept") != "application/octet-stream" {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"id": id, "name": a.name})
			return
		}
		http.Redirect(w, r, s.URL+ObjectPath(a.tag, a.name), http.StatusFound)
	case strings.HasPrefix(r.URL.Path, "/download/"), strings.HasPrefix(r.URL.Path, "/objects/"):
		_, rest, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
		tag, name, _ := strings.Cut(rest, "/")
		data, ok := s.assets[tag][name]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(data)
	default:
		http.NotFound(w, r)
	}
}

func (s *Server) writeRelease(w http.ResponseWriter, rel *Release) {
	names := sortedNames(s.assets[rel.Tag()])
	assets := make([]map[string]any, 0, len(names))
	for _, n := range names {
		assets = append(assets, map[string]any{
			"name":                 n,
			"url":                  s.URL + releasesAPI + "assets/" + strconv.Itoa(s.assetID(rel.Tag(), n)),
			"browser_download_url": s.URL + DownloadPath(rel.Tag(), n),
			"size":                 len(s.assets[rel.Tag()][n]),
		})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"tag_name":     rel.Tag(),
		"html_url":     "https://github.com/" + update.Repo + "/releases/tag/" + rel.Tag(),
		"prerelease":   rel.Prerelease,
		"draft":        false,
		"published_at": rel.Published().UTC().Format(time.RFC3339),
		"assets":       assets,
	})
}

func (s *Server) assetID(tag, name string) int {
	for i, a := range s.ids {
		if a == (assetKey{tag, name}) {
			return i + 1
		}
	}
	return 0
}

func sortedNames(files map[string][]byte) []string {
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// Runner is an update.Runner for the marker binaries: `version` prints the
// candidate file's content, which names its version. Binaries whose content
// starts with "legacy " answer only `--version`, as releases before the
// `version` command did.
func Runner(_ context.Context, exe string, args ...string) ([]byte, error) {
	b, err := os.ReadFile(exe)
	if err != nil {
		return nil, err
	}
	if bytes.HasPrefix(b, []byte("legacy ")) {
		if len(args) == 1 && args[0] == "--version" {
			return bytes.TrimPrefix(b, []byte("legacy ")), nil
		}
		return []byte(`Error: unknown command "version" for "sonarless-mcp"`), fmt.Errorf("exit status 1")
	}
	if len(args) == 1 && args[0] == "version" {
		return b, nil
	}
	return nil, fmt.Errorf("unexpected args %v", args)
}
