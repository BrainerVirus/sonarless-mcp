// Ported from ytrack (github.com/BrainerVirus/youtrack-cli, internal/upgrade/files.go).

package update

import (
	"archive/tar"
	"archive/zip"
	"bufio"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// maxBinary caps the size of the binary extracted from an archive.
const maxBinary = 200 << 20

const (
	// ChecksumsName is the checksums file of every release.
	ChecksumsName = "checksums.txt"
	// BundleName is the Sigstore bundle that signs ChecksumsName.
	BundleName = ChecksumsName + ".sigstore.json"
)

// ArchiveName is the release archive for goos/goarch, as .goreleaser.yaml
// names it. The names carry no version, so releases/latest/download works,
// and every released updater looks for them: never change them.
func ArchiveName(goos, goarch string) string {
	if goos == "windows" {
		return fmt.Sprintf("sonarless-mcp_%s_%s.zip", goos, goarch)
	}
	return fmt.Sprintf("sonarless-mcp_%s_%s.tar.gz", goos, goarch)
}

// ExpectedSHA256 returns the hex digest listed for name in a checksums file
// ("<hex>  <name>" lines, as sha256sum writes them).
func ExpectedSHA256(checksums []byte, name string) (string, error) {
	sc := bufio.NewScanner(bytes.NewReader(checksums))
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) == 2 && strings.TrimPrefix(fields[1], "*") == name {
			if _, err := hex.DecodeString(fields[0]); err != nil || len(fields[0]) != 64 {
				return "", fmt.Errorf("checksums file has an invalid digest for %s", name)
			}
			return strings.ToLower(fields[0]), nil
		}
	}
	return "", fmt.Errorf("checksums file does not list %s", name)
}

// CheckSHA256 reports an error unless data hashes to the hex digest want.
func CheckSHA256(data []byte, want string) error {
	sum := sha256.Sum256(data)
	got := hex.EncodeToString(sum[:])
	if subtle.ConstantTimeCompare([]byte(got), []byte(strings.ToLower(want))) != 1 {
		return fmt.Errorf("checksum mismatch: got %s, want %s", got, want)
	}
	return nil
}

// ExtractBinary returns the sonarless-mcp executable from a release archive
// (tar.gz, or zip for Windows).
func ExtractBinary(archive []byte, name string) ([]byte, error) {
	bin := "sonarless-mcp"
	if strings.HasSuffix(name, ".zip") {
		bin = "sonarless-mcp.exe"
		zr, err := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
		if err != nil {
			return nil, fmt.Errorf("reading %s: %w", name, err)
		}
		for _, f := range zr.File {
			if path.Base(f.Name) != bin || f.FileInfo().IsDir() {
				continue
			}
			rc, err := f.Open()
			if err != nil {
				return nil, err
			}
			defer func() { _ = rc.Close() }()
			return readLimited(rc)
		}
		return nil, fmt.Errorf("%s does not contain %s", name, bin)
	}
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", name, err)
	}
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("%s does not contain %s", name, bin)
		}
		if err != nil {
			return nil, fmt.Errorf("reading %s: %w", name, err)
		}
		if hdr.Typeflag == tar.TypeReg && path.Base(hdr.Name) == bin {
			return readLimited(tr)
		}
	}
}

func readLimited(r io.Reader) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, maxBinary+1))
	if err != nil {
		return nil, err
	}
	if len(b) > maxBinary {
		return nil, errors.New("binary in the archive is too large")
	}
	return b, nil
}

// OldSuffix names the previous binary that a Windows upgrade moves aside.
const OldSuffix = ".old"

// ReplaceExecutable atomically replaces the file at exe with data, keeping
// its permissions. The new file is written next to exe and renamed over it,
// so exe is either the old or the new binary, never a partial one. check,
// when not nil, runs on the written candidate before the rename; an error
// from it leaves exe untouched. Windows cannot overwrite a running executable
// but can rename it, so there the old binary is first moved to exe+OldSuffix
// (removed by the next upgrade; a timestamped name while a process still
// runs from it).
func ReplaceExecutable(exe string, data []byte, check func(candidate string) error) error {
	info, err := os.Stat(exe)
	if err != nil {
		return err
	}
	dir := filepath.Dir(exe)
	// Keep exe's extension last, so Windows runs the candidate for check.
	base := filepath.Base(exe)
	tmp, err := os.CreateTemp(dir, "."+base+".new-*"+filepath.Ext(base))
	if err != nil {
		return fmt.Errorf("cannot write to %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, info.Mode().Perm()|0o100); err != nil && runtime.GOOS != "windows" {
		return err
	}
	if check != nil {
		if err := check(tmpName); err != nil {
			return err
		}
	}
	if runtime.GOOS != "windows" {
		return os.Rename(tmpName, exe)
	}
	old := exe + OldSuffix
	if err := os.Remove(old); err != nil && !os.IsNotExist(err) {
		old = fmt.Sprintf("%s%s-%d", exe, OldSuffix, time.Now().UnixNano())
	}
	if err := os.Rename(exe, old); err != nil {
		return fmt.Errorf("moving the running binary aside: %w", err)
	}
	if err := os.Rename(tmpName, exe); err != nil {
		if rerr := os.Rename(old, exe); rerr != nil {
			return fmt.Errorf("installing the new binary: %w (the previous one is at %s)", err, old)
		}
		return fmt.Errorf("installing the new binary: %w", err)
	}
	return nil
}

// RemoveOld deletes the binary a previous Windows upgrade moved aside.
func RemoveOld(exe string) { _ = os.Remove(exe + OldSuffix) }
