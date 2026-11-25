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

// LabelInstance names the SONARLESS_INSTANCE an MCP container belongs to,
// so `stop` and the idle watcher find this instance's leftover remote
// containers without matching another instance's by name.
const LabelInstance = "sonarless-mcp.instance"

// InstanceLabel is the `key=value` label of cfg's instance.
func InstanceLabel(cfg *config.Config) string { return LabelInstance + "=" + cfg.Get(config.Instance) }

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
		m.Cfg.Get(config.MCPJavaOpts),
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
		"--label", InstanceLabel(m.Cfg),
		"--label", sonar.LabelSpec + "=" + m.spec(),
		"-e", "SONARQUBE_TRANSPORT=http",
		"-e", "SONARQUBE_HTTP_HOST=0.0.0.0",
		"-e", "SONARQUBE_URL=" + m.SonarURL,
		"-e", "SONARQUBE_LOG_TO_FILE_DISABLED=true",
	}
	if env := toolsetsEnv(m.Cfg.Get(config.MCPToolsets)); env != "" {
		args = append(args, "-e", env)
	}
	if m.Cfg.Bool(config.MCPReadOnly) {
		args = append(args, "-e", "SONARQUBE_READ_ONLY=true")
	}
	if env := javaToolOptions(m.Cfg.Get(config.MCPJavaOpts)); env != "" {
		args = append(args, "-e", env)
	}
	args = append(args, m.Image)
	_, err := docker.Run(ctx, args...)
	if err != nil && (strings.Contains(err.Error(), "address already in use") || strings.Contains(err.Error(), "port is already allocated")) {
		_ = docker.Remove(ctx, m.name()) // don't leave a container that can never start
		return fmt.Errorf("port %d is already in use by another program; stop it or set %s (local) or the remote's port to a free one", m.Port, config.MCPPort)
	}
	return err
}

// HealthURL is the MCP server's unauthenticated health endpoint.
func (m *Container) HealthURL() string { return fmt.Sprintf("http://127.0.0.1:%d/health", m.Port) }

// Alive reports whether the MCP server answers 200 on /health. The server's
// security filter answers it before authentication once Jetty is up, the same
// moment /mcp starts answering 401, and its tools are loaded before Jetty
// starts (SonarQubeMcpServer.start, 1.33). Unlike "any HTTP answer on /mcp", it
// rejects another program on the port, and it logs no auth failure per poll.
func (m *Container) Alive(ctx context.Context) bool {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, m.HealthURL(), nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
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

// toolsetsEnv is the SONARQUBE_TOOLSETS entry for the MCP server, or "" for
// "default" / empty: the server's own default toolsets.
func toolsetsEnv(v string) string {
	v = strings.TrimSpace(v)
	if v == "" || strings.EqualFold(v, "default") {
		return ""
	}
	return "SONARQUBE_TOOLSETS=" + v
}

// javaToolOptions is the JAVA_TOOL_OPTIONS entry for the MCP server's JVM
// (every JVM reads it), or "" for none / empty: the JVM's own defaults.
func javaToolOptions(opts string) string {
	opts = strings.TrimSpace(opts)
	if opts == "" || strings.EqualFold(opts, "none") {
		return ""
	}
	return "JAVA_TOOL_OPTIONS=" + opts
}
