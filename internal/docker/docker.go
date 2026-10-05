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
	"os"
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

// Stream executes `docker args...` with the process's stdio attached.
func Stream(ctx context.Context, args ...string) error {
	cmd := exec.CommandContext(ctx, "docker", args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd.Run()
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
	Name   string
	Image  string // image reference the container was created from
	Status string // created, running, exited, ...
	Labels map[string]string
	Mounts []Mount
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
		Config struct {
			Image  string
			Labels map[string]string
		}
		State  struct{ Status string }
		Mounts []Mount
	}
	if err := json.Unmarshal([]byte(out), &raw); err != nil || len(raw) == 0 {
		return nil, fmt.Errorf("parse docker inspect %s: %v", name, err)
	}
	r := raw[0]
	return &Container{
		Name:   strings.TrimPrefix(r.Name, "/"),
		Image:  r.Config.Image,
		Status: r.State.Status,
		Labels: r.Config.Labels,
		Mounts: r.Mounts,
	}, nil
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

// Remove force-removes a container; a missing container is not an error.
func Remove(ctx context.Context, name string) error {
	_, err := Run(ctx, "rm", "-f", name)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	return err
}
