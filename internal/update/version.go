// Ported from ytrack (github.com/BrainerVirus/youtrack-cli, internal/update/version.go).

package update

import (
	"strconv"
	"strings"
)

// version is a parsed MAJOR.MINOR.PATCH[-PRERELEASE][+BUILD] (semver 2.0).
type version struct {
	core [3]int
	pre  []string
}

func parseVersion(s string) (version, bool) {
	s = strings.TrimPrefix(s, "v")
	if i := strings.IndexByte(s, '+'); i >= 0 {
		s = s[:i]
	}
	var v version
	core := s
	if i := strings.IndexByte(s, '-'); i >= 0 {
		core = s[:i]
		if s[i+1:] == "" {
			return version{}, false
		}
		v.pre = strings.Split(s[i+1:], ".")
	}
	parts := strings.Split(core, ".")
	if len(parts) != 3 {
		return version{}, false
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 || (len(p) > 1 && p[0] == '0') {
			return version{}, false
		}
		v.core[i] = n
	}
	return v, true
}

// IsRelease reports whether s is a semantic version (with or without "v").
// Development builds ("dev", "test", a git describe string) are not.
func IsRelease(s string) bool {
	_, ok := parseVersion(s)
	return ok
}

// Newer reports whether version a is newer than b. It is false when either
// is not a semantic version.
func Newer(a, b string) bool {
	va, okA := parseVersion(a)
	vb, okB := parseVersion(b)
	return okA && okB && compare(va, vb) > 0
}

func compare(a, b version) int {
	for i := range a.core {
		if a.core[i] != b.core[i] {
			if a.core[i] > b.core[i] {
				return 1
			}
			return -1
		}
	}
	// A prerelease sorts before its release.
	switch {
	case len(a.pre) == 0 && len(b.pre) == 0:
		return 0
	case len(a.pre) == 0:
		return 1
	case len(b.pre) == 0:
		return -1
	}
	for i := 0; i < len(a.pre) && i < len(b.pre); i++ {
		if c := comparePre(a.pre[i], b.pre[i]); c != 0 {
			return c
		}
	}
	switch {
	case len(a.pre) > len(b.pre):
		return 1
	case len(a.pre) < len(b.pre):
		return -1
	}
	return 0
}

func comparePre(a, b string) int {
	na, errA := strconv.Atoi(a)
	nb, errB := strconv.Atoi(b)
	switch {
	case errA == nil && errB == nil:
		if na == nb {
			return 0
		}
		if na > nb {
			return 1
		}
		return -1
	case errA == nil: // numeric identifiers sort before alphanumeric ones
		return -1
	case errB == nil:
		return 1
	}
	return strings.Compare(a, b)
}
