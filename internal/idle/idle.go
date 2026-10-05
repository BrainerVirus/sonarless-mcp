// Package idle stops the shared SonarQube server and MCP container after a
// period without use. Activity is recorded as the mtime of a stamp file
// (touched by MCP requests and scans) plus inbound network traffic to the
// server container (web UI use). One watcher runs per machine, guarded by a
// file lock; it exits once it has stopped the containers.
package idle

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/BrainerVirus/sonarless-mcp/internal/config"
	"github.com/BrainerVirus/sonarless-mcp/internal/docker"
	"github.com/gofrs/flock"
)

const (
	pollEvery = time.Minute
	// webTraffic is the inbound bytes per poll above which the server counts
	// as in use (someone browsing the UI or calling the API directly).
	webTraffic = 64 << 10
)

func stampFile(cfg *config.Config) string { return filepath.Join(cfg.CacheDir, "last-activity") }

// Touch records activity now.
func Touch(cfg *config.Config) {
	p := stampFile(cfg)
	now := time.Now()
	if err := os.Chtimes(p, now, now); err != nil {
		_ = os.MkdirAll(cfg.CacheDir, 0o755)
		_ = os.WriteFile(p, nil, 0o644)
	}
}

// Last returns the time of the last recorded activity (zero if never).
func Last(cfg *config.Config) time.Time {
	fi, err := os.Stat(stampFile(cfg))
	if err != nil {
		return time.Time{}
	}
	return fi.ModTime()
}

// Keepalive touches the stamp every interval until ctx ends; for long scans.
func Keepalive(ctx context.Context, cfg *config.Config) {
	Touch(cfg)
	t := time.NewTicker(pollEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			Touch(cfg)
		}
	}
}

// EnsureWatcher starts the background watcher if idle stop is enabled and
// none is running. It never blocks on the watcher.
func EnsureWatcher(cfg *config.Config) error {
	timeout, _ := cfg.IdleTimeout()
	if timeout == 0 {
		return nil
	}
	if err := os.MkdirAll(cfg.CacheDir, 0o755); err != nil {
		return err
	}
	lock := flock.New(filepath.Join(cfg.CacheDir, "watcher.lock"))
	ok, err := lock.TryLock()
	if err != nil {
		return err
	}
	if !ok {
		return nil // a watcher already holds it
	}
	_ = lock.Unlock() // the child takes it; a race just means it exits early

	exe, err := os.Executable()
	if err != nil {
		return err
	}
	logf, err := os.OpenFile(filepath.Join(cfg.CacheDir, "watcher.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer logf.Close()
	cmd := exec.Command(exe, "daemon")
	cmd.Stdin, cmd.Stdout, cmd.Stderr = nil, logf, logf
	detach(cmd)
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}

// Watch runs the watcher loop in the foreground (the `daemon` command).
// stop is called once the idle timeout passes.
func Watch(ctx context.Context, cfg *config.Config, containers []string, stop func(context.Context) error, log io.Writer) error {
	timeout, _ := cfg.IdleTimeout()
	if timeout == 0 {
		return nil
	}
	lock := flock.New(filepath.Join(cfg.CacheDir, "watcher.lock"))
	ok, err := lock.TryLock()
	if err != nil || !ok {
		return err // another watcher owns the job
	}
	defer lock.Unlock()
	fmt.Fprintf(log, "%s watcher started (pid %d, idle timeout %s)\n", time.Now().Format(time.RFC3339), os.Getpid(), timeout)

	server := containers[0]
	lastRx := int64(-1)
	if Last(cfg).IsZero() {
		Touch(cfg)
	}
	for {
		anyRunning := false
		for _, name := range containers {
			if c, err := docker.Inspect(ctx, name); err == nil && c.Running() {
				anyRunning = true
			}
		}
		if !anyRunning {
			fmt.Fprintf(log, "%s nothing running; watcher exiting\n", time.Now().Format(time.RFC3339))
			return nil
		}
		if rx, err := inboundBytes(ctx, server); err == nil {
			if lastRx >= 0 && rx-lastRx > webTraffic {
				Touch(cfg)
			}
			lastRx = rx
		}
		if idle := time.Since(Last(cfg)); idle >= timeout {
			fmt.Fprintf(log, "%s idle for %s; stopping %s\n", time.Now().Format(time.RFC3339), idle.Round(time.Second), strings.Join(containers, ", "))
			return stop(ctx)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(pollEvery):
		}
	}
}

// inboundBytes reads the container's received bytes from `docker stats`.
func inboundBytes(ctx context.Context, name string) (int64, error) {
	out, err := docker.Run(ctx, "stats", "--no-stream", "--format", "{{.NetIO}}", name)
	if err != nil {
		return 0, err
	}
	in, _, _ := strings.Cut(out, "/")
	return parseSize(strings.TrimSpace(in))
}

// parseSize parses docker's human sizes: "1.5kB", "3.2MB", "12B", "1GiB".
func parseSize(s string) (int64, error) {
	units := []struct {
		suffix string
		mult   float64
	}{{"KiB", 1 << 10}, {"MiB", 1 << 20}, {"GiB", 1 << 30}, {"TiB", 1 << 40},
		{"kB", 1e3}, {"KB", 1e3}, {"MB", 1e6}, {"GB", 1e9}, {"TB", 1e12}, {"B", 1}}
	for _, u := range units {
		if strings.HasSuffix(s, u.suffix) {
			f, err := strconv.ParseFloat(strings.TrimSuffix(s, u.suffix), 64)
			if err != nil {
				return 0, err
			}
			return int64(f * u.mult), nil
		}
	}
	return 0, fmt.Errorf("unknown size %q", s)
}
