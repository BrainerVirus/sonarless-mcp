// Package mcp runs ONE shared SonarQube MCP server (Streamable HTTP, in a
// container) for every client on the machine, and a small stdio shim that
// clients launch instead of a per-client MCP container.
package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/BrainerVirus/sonarless-mcp/internal/config"
	"github.com/BrainerVirus/sonarless-mcp/internal/docker"
	"github.com/BrainerVirus/sonarless-mcp/internal/sonar"
)

// Container manages one shared MCP server container: the local one (talking
// to the local SonarQube) or one per remote server.
type Container struct {
	Cfg      *config.Config
	Log      io.Writer
	Name     string // container name
	Port     int    // host port (bound to 127.0.0.1)
	SonarURL string // SonarQube URL as seen from inside the container
	// Image to run: a tag for the local container; for remotes the exact image
	// ID the local one runs, so every server exposes the same tool schemas.
	Image string
}

// NewLocal is the MCP container for the local SonarQube.
func NewLocal(cfg *config.Config, log io.Writer) *Container {
	return &Container{Cfg: cfg, Log: log, Name: cfg.MCPContainer(), Port: cfg.Int(config.MCPPort), SonarURL: cfg.ServerURLInNetwork(), Image: cfg.Get(config.MCPImage)}
}

// NewRemote is the MCP container for a remote SonarQube.
func NewRemote(cfg *config.Config, log io.Writer, name, url string, port int) *Container {
	return &Container{Cfg: cfg, Log: log, Name: cfg.MCPContainer() + "-" + name, Port: port, SonarURL: url, Image: cfg.Get(config.MCPImage)}
}

// RunningImageID is the image ID the container runs ("" if absent).
func (m *Container) RunningImageID(ctx context.Context) string {
	c, err := docker.Inspect(ctx, m.name())
	if err != nil {
		return ""
	}
	return c.ImageID
}

func (m *Container) name() string { return m.Name }

// URL is the container's MCP endpoint on the host.
func (m *Container) URL() string { return fmt.Sprintf("http://127.0.0.1:%d/mcp", m.Port) }

func (m *Container) spec() string {
	h := sha256.Sum256([]byte(strings.Join([]string{
		m.Image,
		fmt.Sprint(m.Port),
		m.Cfg.Network(),
		m.SonarURL,
		m.Cfg.Get(config.MCPToolsets),
		m.Cfg.Get(config.MCPReadOnly),
	}, "\x00")))
	return hex.EncodeToString(h[:8])
}

// Ensure starts (or creates/recreates) the MCP container and waits until it
// answers HTTP.
func (m *Container) Ensure(ctx context.Context) error {
	unlock, err := sonar.LockFile(m.Cfg.CacheDir, m.name()+".lock")
	if err != nil {
		return err
	}
	defer unlock()
	c, err := docker.Inspect(ctx, m.name())
	switch {
	case errors.Is(err, docker.ErrNotFound):
		if err := m.create(ctx); err != nil {
			return err
		}
	case err != nil:
		return err
	case c.Labels[sonar.LabelSpec] != m.spec():
		if m.Log != nil {
			fmt.Fprintf(m.Log, "MCP settings changed; recreating %s...\n", m.name())
		}
		if err := docker.Remove(ctx, m.name()); err != nil {
			return err
		}
		if err := m.create(ctx); err != nil {
			return err
		}
	case !c.Running() && c.ImageID != docker.ImageID(ctx, m.Image):
		// A newer image was pulled (auto-update); pick it up while nothing uses
		// the container. A running container is never swapped mid-session.
		if err := docker.Remove(ctx, m.name()); err != nil {
			return err
		}
		if err := m.create(ctx); err != nil {
			return err
		}
	case !c.Running():
		if err := docker.Start(ctx, m.name()); err != nil {
			return err
		}
	}
	return m.waitHTTP(ctx)
}

func (m *Container) create(ctx context.Context) error {
	if m.Log != nil {
		fmt.Fprintf(m.Log, "Creating shared MCP container %s (%s)...\n", m.name(), m.Image)
	}
	args := []string{"run", "-d", "--init", "--name", m.name(),
		"--network", m.Cfg.Network(),
		"-p", fmt.Sprintf("127.0.0.1:%d:8080", m.Port),
		"--label", sonar.LabelManaged + "=true",
		"--label", sonar.LabelSpec + "=" + m.spec(),
		"-e", "SONARQUBE_TRANSPORT=http",
		"-e", "SONARQUBE_HTTP_HOST=0.0.0.0",
		"-e", "SONARQUBE_URL=" + m.SonarURL,
		"-e", "SONARQUBE_LOG_TO_FILE_DISABLED=true",
	}
	if v := m.Cfg.Get(config.MCPToolsets); v != "" {
		args = append(args, "-e", "SONARQUBE_TOOLSETS="+v)
	}
	if m.Cfg.Bool(config.MCPReadOnly) {
		args = append(args, "-e", "SONARQUBE_READ_ONLY=true")
	}
	args = append(args, m.Image)
	_, err := docker.Run(ctx, args...)
	if err != nil && (strings.Contains(err.Error(), "address already in use") || strings.Contains(err.Error(), "port is already allocated")) {
		_ = docker.Remove(ctx, m.name()) // don't leave a container that can never start
		return fmt.Errorf("port %d is already in use by another program; stop it or set %s (local) or the remote's port to a free one", m.Port, config.MCPPort)
	}
	return err
}

// Alive reports whether the MCP endpoint answers HTTP at all (an
// unauthenticated probe gets a quick 401).
func (m *Container) Alive(ctx context.Context) bool {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, m.URL(), strings.NewReader("{}"))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return true
}

func (m *Container) waitHTTP(ctx context.Context) error {
	deadline := time.Now().Add(90 * time.Second)
	for !m.Alive(ctx) {
		if time.Now().After(deadline) {
			logs, _ := docker.Run(ctx, "logs", "--tail", "20", m.name())
			return fmt.Errorf("MCP container not answering on %s after 90s\n%s", m.URL(), logs)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
	return nil
}

// Stop stops the MCP container.
func (m *Container) Stop(ctx context.Context) error { return docker.Stop(ctx, m.name()) }

// Running reports whether the MCP container is running.
func (m *Container) Running(ctx context.Context) bool {
	c, err := docker.Inspect(ctx, m.name())
	return err == nil && c.Running()
}
