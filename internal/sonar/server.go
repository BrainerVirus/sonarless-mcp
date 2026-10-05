package sonar

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/BrainerVirus/sonarless-mcp/internal/config"
	"github.com/BrainerVirus/sonarless-mcp/internal/docker"
	"github.com/gofrs/flock"
)

// Container labels and the name of the token sonarless-mcp owns.
const (
	LabelManaged = "sonarless-mcp.managed"
	LabelSpec    = "sonarless-mcp.spec"
	tokenName    = "sonarless-mcp"
)

// Server manages the shared SonarQube container.
type Server struct {
	Cfg *config.Config
	Log io.Writer // progress output; never stdout in MCP mode
}

func (s *Server) logf(format string, a ...any) {
	if s.Log != nil {
		fmt.Fprintf(s.Log, format+"\n", a...)
	}
}

func (s *Server) name() string { return s.Cfg.ServerContainer() }

// Admin returns an admin API client for the host URL.
func (s *Server) Admin() *Client {
	return NewAdmin(s.Cfg.ServerURL(), s.Cfg.Get(config.AdminUser), s.Cfg.Get(config.AdminPass))
}

// spec hashes everything that requires recreating the container to change.
func (s *Server) spec() string {
	h := sha256.Sum256([]byte(strings.Join([]string{
		s.Cfg.ServerImage(),
		s.Cfg.Get(config.ServerPort),
		s.Cfg.Network(),
		s.Cfg.PluginsDir(),
	}, "\x00")))
	return hex.EncodeToString(h[:8])
}

// volume returns the named volume for one SonarQube dir (data, extensions,
// logs) of the configured version. Volumes are per version because SonarQube
// can't upgrade its embedded H2 database: a new version starts with fresh
// history, and switching back to an older version finds its data untouched.
func (s *Server) volume(dir string) string {
	var b strings.Builder
	for _, r := range s.Cfg.Get(config.SonarQubeVersion) {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '.' || r == '_' || r == '-' {
			b.WriteRune(r)
		} else {
			b.WriteRune('_')
		}
	}
	return s.name() + "-" + b.String() + "-" + dir
}

// Running reports whether the server container is running.
func (s *Server) Running(ctx context.Context) bool {
	c, err := docker.Inspect(ctx, s.name())
	return err == nil && c.Running()
}

// Ensure starts (or creates/recreates) the server and waits until it is UP
// with working admin credentials.
func (s *Server) Ensure(ctx context.Context) error {
	if err := docker.Available(ctx); err != nil {
		return err
	}
	unlock, err := LockFile(s.Cfg.CacheDir, s.name()+".lock")
	if err != nil {
		return err
	}
	defer unlock()
	if err := docker.EnsureNetwork(ctx, s.Cfg.Network()); err != nil {
		return err
	}
	c, err := docker.Inspect(ctx, s.name())
	switch {
	case errors.Is(err, docker.ErrNotFound):
		s.logf("Creating SonarQube container %s (%s)...", s.name(), s.Cfg.ServerImage())
		if err := s.create(ctx); err != nil {
			return err
		}
	case err != nil:
		return err
	case c.Labels[LabelSpec] == s.spec():
		if !c.Running() {
			s.logf("Starting SonarQube container %s...", s.name())
			if err := docker.Start(ctx, s.name()); err != nil {
				return err
			}
		}
	default:
		if c.Image != s.Cfg.ServerImage() {
			history := "starts with empty history (SonarQube can't upgrade its embedded database)"
			if docker.VolumeExists(ctx, s.volume("data")) {
				history = "uses its saved history"
			}
			s.logf("SonarQube version changed (%s -> %s); %s, and %s's data is kept for switching back.",
				c.Image, s.Cfg.ServerImage(), history, c.Image)
		} else {
			s.logf("SonarQube settings changed; recreating %s (data kept)...", s.name())
		}
		if err := docker.Remove(ctx, s.name()); err != nil {
			return err
		}
		if err := s.create(ctx); err != nil {
			return err
		}
	}
	return s.waitReady(ctx)
}

func (s *Server) create(ctx context.Context) error {
	args := []string{"run", "-d", "--name", s.name(),
		"--network", s.Cfg.Network(),
		"-p", "127.0.0.1:" + s.Cfg.Get(config.ServerPort) + ":9000",
		"--label", LabelManaged + "=true",
		"--label", LabelSpec + "=" + s.spec(),
		"-v", s.volume("data") + ":/opt/sonarqube/data",
		"-v", s.volume("extensions") + ":/opt/sonarqube/extensions",
		"-v", s.volume("logs") + ":/opt/sonarqube/logs",
	}
	if d := s.Cfg.PluginsDir(); d != "" {
		args = append(args, "-v", d+":/opt/sonarqube/extensions/plugins")
	}
	args = append(args, s.Cfg.ServerImage())
	_, err := docker.Run(ctx, args...)
	return err
}

// waitReady polls until the server is UP, then checks the admin credentials.
// DB_MIGRATION_NEEDED is only upgradable on external databases; with
// per-version volumes it means the volume holds another version's data.
func (s *Server) waitReady(ctx context.Context) error {
	admin := s.Admin()
	deadline := time.Now().Add(5 * time.Minute)
	for {
		st, err := admin.Status(ctx)
		if err == nil && st == "UP" {
			break
		}
		if err == nil && st == "DB_MIGRATION_NEEDED" {
			if err := admin.MigrateDB(ctx); err != nil {
				return fmt.Errorf("SonarQube needs a database upgrade it can't do: %w; start this version fresh with `sonarless-mcp reset --yes`", err)
			}
		}
		if time.Now().After(deadline) {
			logs, _ := docker.Run(ctx, "logs", "--tail", "30", s.name())
			return fmt.Errorf("SonarQube not UP after 5m (last status %q)\n%s", st, logs)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
	if !admin.Valid(ctx) {
		return fmt.Errorf("admin login %s failed on %s; fix %s/%s or run `sonarless-mcp reset --yes`",
			s.Cfg.Get(config.AdminUser), s.Cfg.ServerURL(), config.AdminUser, config.AdminPass)
	}
	return nil
}

// Stop stops the server container.
func (s *Server) Stop(ctx context.Context) error { return docker.Stop(ctx, s.name()) }

// Reset deletes the container and the current version's data, then starts fresh.
func (s *Server) Reset(ctx context.Context) error {
	if err := docker.Remove(ctx, s.name()); err != nil {
		return err
	}
	for _, dir := range []string{"data", "extensions", "logs"} {
		if v := s.volume(dir); docker.VolumeExists(ctx, v) {
			if _, err := docker.Run(ctx, "volume", "rm", v); err != nil {
				return err
			}
		}
	}
	_ = os.Remove(s.tokenFile())
	return s.Ensure(ctx)
}

func (s *Server) tokenFile() string { return filepath.Join(s.Cfg.ConfigDir, "token") }

// ReadToken returns the stored token without validating it.
func (s *Server) ReadToken() string {
	b, _ := os.ReadFile(s.tokenFile())
	return strings.TrimSpace(string(b))
}

// Token returns a valid user token, creating it if needed. Regeneration is
// serialized with a file lock and re-checked after locking, so concurrent
// clients never revoke a token another client just created.
func (s *Server) Token(ctx context.Context) (string, error) {
	if t := s.ReadToken(); t != "" && NewToken(s.Cfg.ServerURL(), t).Valid(ctx) {
		return t, nil
	}
	unlock, err := LockFile(s.Cfg.CacheDir, "token.lock")
	if err != nil {
		return "", err
	}
	defer unlock()
	if t := s.ReadToken(); t != "" && NewToken(s.Cfg.ServerURL(), t).Valid(ctx) {
		return t, nil
	}
	admin := s.Admin()
	if err := admin.RevokeToken(ctx, tokenName); err != nil {
		return "", err
	}
	t, err := admin.GenerateToken(ctx, tokenName)
	if err != nil {
		return "", err
	}
	if err := writePrivate(s.tokenFile(), t); err != nil {
		return "", err
	}
	return t, nil
}

func writePrivate(path, content string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp-" + strconv.Itoa(os.Getpid())
	if err := os.WriteFile(tmp, []byte(content), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// LockFile takes an exclusive cross-process lock, so concurrent clients don't
// race to create the same container. Call the returned func to release it.
func LockFile(dir, name string) (func(), error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	l := flock.New(filepath.Join(dir, name))
	if err := l.Lock(); err != nil {
		return nil, err
	}
	return func() { _ = l.Unlock() }, nil
}
