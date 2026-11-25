package main

import (
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// tokenPatterns match credentials that must never reach a result file:
// SonarQube user and analysis tokens, Anthropic keys and bearer headers.
var tokenPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)bearer\s+[A-Za-z0-9._~+/=-]{8,}`),
	regexp.MustCompile(`\bsq[uap]_[A-Za-z0-9]{8,}`),
	regexp.MustCompile(`sk-ant-[A-Za-z0-9_-]{8,}`),
}

// scrubber replaces machine-specific paths with placeholders and tokens with
// <token>, so a result file can be committed.
type scrubber struct {
	roots []placeholder
	user  string
}

type placeholder struct{ path, name string }

func newScrubber(world string) *scrubber {
	s := &scrubber{}
	add := func(path, name string) {
		if path == "" || path == "/" {
			return
		}
		s.roots = append(s.roots, placeholder{path, name})
		if real, err := filepath.EvalSymlinks(path); err == nil && real != path {
			s.roots = append(s.roots, placeholder{real, name})
		}
	}
	add(world, "<world>")
	if home, err := os.UserHomeDir(); err == nil {
		add(home, "~")
	}
	if u, err := user.Current(); err == nil {
		s.user = u.Username
	}
	// Longest first, so the world under the home becomes <world>, not ~/...
	sort.SliceStable(s.roots, func(i, j int) bool { return len(s.roots[i].path) > len(s.roots[j].path) })
	return s
}

func (s *scrubber) scrub(text string) string {
	for _, r := range s.roots {
		text = strings.ReplaceAll(text, r.path, r.name)
	}
	for _, re := range tokenPatterns {
		text = re.ReplaceAllString(text, "<token>")
	}
	if s.user != "" && len(s.user) > 3 {
		text = regexp.MustCompile(`\b`+regexp.QuoteMeta(s.user)+`\b`).ReplaceAllString(text, "<user>")
	}
	return text
}

// check refuses text that still holds a scrubbed path or a token.
func (s *scrubber) check(text string) error {
	for _, r := range s.roots {
		if strings.Contains(text, r.path) {
			return fmt.Errorf("result still holds %s", r.name)
		}
	}
	for _, re := range tokenPatterns {
		if re.MatchString(text) {
			return fmt.Errorf("result still holds a token")
		}
	}
	return nil
}
