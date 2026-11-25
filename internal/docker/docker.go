// Package docker is a thin wrapper over the docker CLI. Shelling out keeps the
// binary small and behaves the same on Linux, macOS and Windows (Docker
// Desktop), with no daemon API versioning to track.
package docker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
)

// ErrNotFound is returned when a container, network or volume doesn't exist.
var ErrNotFound = errors.New("not found")

// Run executes `docker args...` and returns trimmed stdout.
func Run(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "docker", args...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errb.String())
		if strings.Contains(msg, "No such") || strings.Contains(msg, "not found") {
			return "", fmt.Errorf("%w: %s", ErrNotFound, msg)
		}
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("docker %s: %s", args[0], msg)
	}
	return strings.TrimSpace(out.String()), nil
}

// Stream executes `docker args...`, sending its output to out (stdout and
// stderr both). Never os.Stdout from MCP code paths: there it's the protocol.
func Stream(ctx context.Context, out io.Writer, args ...string) error {
	cmd := exec.CommandContext(ctx, "docker", args...)
	cmd.Stdout, cmd.Stderr = out, out
	return cmd.Run()
}

// Exec runs a command in a running container (`docker exec name args...`)
// and returns its stdout and exit status. Docker's own failures have their
// own statuses: 126 when the command cannot run, 127 when the image lacks
// it. err is set only when the docker CLI itself could not run.
func Exec(ctx context.Context, name string, args ...string) (stdout string, status int, err error) {
	cmd := exec.CommandContext(ctx, "docker", append([]string{"exec", name}, args...)...)
	var out bytes.Buffer
	cmd.Stdout = &out
	err = cmd.Run()
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return out.String(), exit.ExitCode(), nil
	}
	return out.String(), 0, err
}

// Available reports whether the docker daemon is reachable.
func Available(ctx context.Context) error {
	if _, err := exec.LookPath("docker"); err != nil {
		return errors.New("docker CLI not found in PATH; install Docker Desktop or Docker Engine")
	}
	if _, err := Run(ctx, "version", "--format", "{{.Server.Version}}"); err != nil {
		return fmt.Errorf("docker daemon not reachable (is Docker running?): %w", err)
	}
	return nil
}

// Container is the subset of `docker inspect` sonarless needs.
type Container struct {
	Name    string
	Image   string // image reference the container was created from
	ImageID string // image ID the container runs
	Status  string // created, running, exited, ...
	// ExitCode is the last run's exit status (meaningful once exited).
	ExitCode int
	// StartedAt is when the container last started, as the daemon prints it
	// (RFC 3339), for `docker logs --since`.
	StartedAt string
	Labels    map[string]string
	Mounts    []Mount
}

// Mount is a container mount.
type Mount struct {
	Type        string // volume, bind
	Name        string // volume name (anonymous volumes have a hex name)
	Destination string
}

// Running reports whether the container is running.
func (c *Container) Running() bool { return c.Status == "running" }

// Inspect returns the container, or ErrNotFound.
func Inspect(ctx context.Context, name string) (*Container, error) {
	out, err := Run(ctx, "container", "inspect", name)
	if err != nil {
		return nil, err
	}
	var raw []struct {
		Name   string
		Image  string
		Config struct {
			Image  string
			Labels map[string]string
		}
		State struct {
			Status    string
			ExitCode  int
			StartedAt string
		}
		Mounts []Mount
	}
	if err := json.Unmarshal([]byte(out), &raw); err != nil || len(raw) == 0 {
		return nil, fmt.Errorf("parse docker inspect %s: %v", name, err)
	}
	r := raw[0]
	return &Container{
		Name:      strings.TrimPrefix(r.Name, "/"),
		Image:     r.Config.Image,
		ImageID:   r.Image,
		Status:    r.State.Status,
		ExitCode:  r.State.ExitCode,
		StartedAt: r.State.StartedAt,
		Labels:    r.Config.Labels,
		Mounts:    r.Mounts,
	}, nil
}

// ImageID returns the local ID of an image reference ("" if not pulled).
func ImageID(ctx context.Context, ref string) string {
	id, _ := Run(ctx, "image", "inspect", "--format", "{{.Id}}", ref)
	return id
}

// EnsureNetwork creates the network if missing.
func EnsureNetwork(ctx context.Context, name string) error {
	if _, err := Run(ctx, "network", "inspect", name); err == nil {
		return nil
	}
	_, err := Run(ctx, "network", "create", name)
	return err
}

// VolumeExists reports whether a named volume exists.
func VolumeExists(ctx context.Context, name string) bool {
	_, err := Run(ctx, "volume", "inspect", name)
	return err == nil
}

// Start starts a stopped container.
func Start(ctx context.Context, name string) error {
	_, err := Run(ctx, "start", name)
	return err
}

// Stop stops a container; a missing container is not an error.
func Stop(ctx context.Context, name string) error {
	_, err := Run(ctx, "stop", name)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	return err
}

// Remove force-removes a container and its anonymous volumes; a missing
// container is not an error. Named volumes (the server's data) are kept. The
// SonarQube image declares /opt/sonarqube/temp a volume, so without -v every
// recreate of the server container left one anonymous volume behind.
func Remove(ctx context.Context, name string) error {
	_, err := Run(ctx, "rm", "-fv", name)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	return err
}

// Names lists the containers (running or not) that carry label (a
// `key=value` filter).
func Names(ctx context.Context, label string) ([]string, error) {
	out, err := Run(ctx, "ps", "-a", "--filter", "label="+label, "--format", "{{.Names}}")
	if err != nil {
		return nil, err
	}
	return strings.Fields(out), nil
}

// Memory returns the memory each running container uses now, as docker
// prints it ("512MiB"), by container name. Names that aren't running are
// left out.
func Memory(ctx context.Context, names ...string) (map[string]string, error) {
	mem := map[string]string{}
	if len(names) == 0 {
		return mem, nil
	}
	args := append([]string{"stats", "--no-stream", "--format", "{{.Name}}\t{{.MemUsage}}"}, names...)
	out, err := Run(ctx, args...)
	if err != nil {
		return mem, err
	}
	for _, line := range strings.Split(out, "\n") {
		name, usage, ok := strings.Cut(strings.TrimSpace(line), "\t")
		if !ok {
			continue
		}
		used, _, _ := strings.Cut(usage, "/")
		mem[name] = strings.TrimSpace(used)
	}
	return mem, nil
}
