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

// Container manages the shared MCP server container.
type Container struct {
	Cfg *config.Config
	Log io.Writer
}

func (m *Container) name() string { return m.Cfg.MCPContainer() }

func (m *Container) spec() string {
	h := sha256.Sum256([]byte(strings.Join([]string{
		m.Cfg.Get(config.MCPImage),
		m.Cfg.Get(config.MCPPort),
		m.Cfg.Network(),
		m.Cfg.ServerURLInNetwork(),
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
	case !c.Running():
		if err := docker.Start(ctx, m.name()); err != nil {
			return err
		}
	}
	return m.waitHTTP(ctx)
}

func (m *Container) create(ctx context.Context) error {
	if m.Log != nil {
		fmt.Fprintf(m.Log, "Creating shared MCP container %s (%s)...\n", m.name(), m.Cfg.Get(config.MCPImage))
	}
	args := []string{"run", "-d", "--init", "--name", m.name(),
		"--network", m.Cfg.Network(),
		"-p", "127.0.0.1:" + m.Cfg.Get(config.MCPPort) + ":8080",
		"--label", sonar.LabelManaged + "=true",
		"--label", sonar.LabelSpec + "=" + m.spec(),
		"-e", "SONARQUBE_TRANSPORT=http",
		"-e", "SONARQUBE_HTTP_HOST=0.0.0.0",
		"-e", "SONARQUBE_URL=" + m.Cfg.ServerURLInNetwork(),
		"-e", "SONARQUBE_LOG_TO_FILE_DISABLED=true",
	}
	if v := m.Cfg.Get(config.MCPToolsets); v != "" {
		args = append(args, "-e", "SONARQUBE_TOOLSETS="+v)
	}
	if m.Cfg.Bool(config.MCPReadOnly) {
		args = append(args, "-e", "SONARQUBE_READ_ONLY=true")
	}
	args = append(args, m.Cfg.Get(config.MCPImage))
	_, err := docker.Run(ctx, args...)
	return err
}

// Alive reports whether the MCP endpoint answers HTTP at all (an
// unauthenticated probe gets a quick 401).
func (m *Container) Alive(ctx context.Context) bool {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, m.Cfg.MCPURL(), strings.NewReader("{}"))
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
			return fmt.Errorf("MCP container not answering on %s after 90s\n%s", m.Cfg.MCPURL(), logs)
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
