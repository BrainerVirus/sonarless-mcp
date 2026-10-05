// Package remote stores the remote SonarQube servers (e.g. a company's CI
// server) the MCP shim can query next to the local one. Non-secret settings
// live in remotes.json (safe to keep in dotfiles); each token lives in its own
// 0600 file under remotes/.
package remote

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/BrainerVirus/sonarless-mcp/internal/config"
)

// Remote is one remote SonarQube server.
type Remote struct {
	Name   string `json:"name"`
	URL    string `json:"url"`
	Branch string `json:"branch,omitempty"` // default branch for tools that take one
	Port   int    `json:"port"`             // host port of its shared MCP container
}

// Local is the reserved name of the built-in server.
const Local = "local"

var validName = regexp.MustCompile(`^[a-z][a-z0-9-]{0,30}$`)

func file(cfg *config.Config) string { return filepath.Join(cfg.ConfigDir, "remotes.json") }

// TokenFile is where a remote's token is stored.
func TokenFile(cfg *config.Config, name string) string {
	return filepath.Join(cfg.ConfigDir, "remotes", name+".token")
}

// Load returns the configured remotes, sorted by name.
func Load(cfg *config.Config) ([]Remote, error) {
	b, err := os.ReadFile(file(cfg))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var rs []Remote
	if err := json.Unmarshal(b, &rs); err != nil {
		return nil, fmt.Errorf("%s: %w", file(cfg), err)
	}
	sort.Slice(rs, func(i, j int) bool { return rs[i].Name < rs[j].Name })
	return rs, nil
}

func save(cfg *config.Config, rs []Remote) error {
	if err := os.MkdirAll(cfg.ConfigDir, 0o755); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(rs, "", "  ")
	tmp := file(cfg) + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, file(cfg))
}

// Add creates or updates a remote and stores its token. A new remote gets
// the next free MCP port after the local one.
func Add(cfg *config.Config, r Remote, token string) (Remote, error) {
	if !validName.MatchString(r.Name) || r.Name == Local {
		return r, fmt.Errorf("invalid name %q: use lowercase letters, digits and dashes (not %q)", r.Name, Local)
	}
	r.URL = strings.TrimRight(r.URL, "/")
	if !strings.HasPrefix(r.URL, "http://") && !strings.HasPrefix(r.URL, "https://") {
		return r, fmt.Errorf("url must start with http:// or https://")
	}
	if strings.TrimSpace(token) == "" {
		return r, errors.New("empty token")
	}
	rs, err := Load(cfg)
	if err != nil {
		return r, err
	}
	taken := map[int]bool{cfg.Int(config.ServerPort): true, cfg.Int(config.MCPPort): true}
	next := cfg.Int(config.MCPPort) + 1
	idx := -1
	for i, x := range rs {
		if x.Name == r.Name {
			idx = i
		}
		if x.Name != r.Name {
			taken[x.Port] = true
		}
		if x.Port >= next {
			next = x.Port + 1
		}
	}
	for taken[next] || !portFree(next) {
		next++
	}
	if idx >= 0 {
		if r.Port == 0 {
			r.Port = rs[idx].Port
		}
		rs[idx] = r
	} else {
		if r.Port == 0 {
			r.Port = next
		}
		rs = append(rs, r)
	}
	tf := TokenFile(cfg, r.Name)
	if err := os.MkdirAll(filepath.Dir(tf), 0o700); err != nil {
		return r, err
	}
	if err := os.WriteFile(tf, []byte(strings.TrimSpace(token)), 0o600); err != nil {
		return r, err
	}
	return r, save(cfg, rs)
}

// portFree reports whether nothing listens on 127.0.0.1:port.
func portFree(port int) bool {
	l, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return false
	}
	_ = l.Close()
	return true
}

// Remove deletes a remote and its token.
func Remove(cfg *config.Config, name string) error {
	rs, err := Load(cfg)
	if err != nil {
		return err
	}
	kept := rs[:0]
	found := false
	for _, r := range rs {
		if r.Name == name {
			found = true
			continue
		}
		kept = append(kept, r)
	}
	if !found {
		return fmt.Errorf("no remote named %q", name)
	}
	_ = os.Remove(TokenFile(cfg, name))
	return save(cfg, kept)
}

// Token reads a remote's stored token.
func Token(cfg *config.Config, name string) (string, error) {
	b, err := os.ReadFile(TokenFile(cfg, name))
	if err != nil {
		return "", fmt.Errorf("no token for remote %q (set it with `sonarless-mcp remote update %s --token`): %w", name, name, err)
	}
	return strings.TrimSpace(string(b)), nil
}

// Get returns one remote by name.
func Get(cfg *config.Config, name string) (Remote, bool, error) {
	rs, err := Load(cfg)
	if err != nil {
		return Remote{}, false, err
	}
	for _, r := range rs {
		if r.Name == name {
			return r, true, nil
		}
	}
	return Remote{}, false, nil
}
