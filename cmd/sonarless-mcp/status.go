package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/BrainerVirus/sonarless-mcp/internal/clients"
	"github.com/BrainerVirus/sonarless-mcp/internal/docker"
	"github.com/BrainerVirus/sonarless-mcp/internal/idle"
	"github.com/BrainerVirus/sonarless-mcp/internal/mcp"
	"github.com/BrainerVirus/sonarless-mcp/internal/sonar"
	"github.com/spf13/cobra"
)

// statusReport is `status --json`.
type statusReport struct {
	Docker     string            `json:"docker,omitempty"` // why containers are missing: docker unavailable
	Containers []containerStatus `json:"containers"`
	Gateway    mcp.GatewayState  `json:"gateway"`
	Shims      []idle.Proc       `json:"shims"`
	Scans      []idle.Proc       `json:"scans"`
	Idle       idleStatus        `json:"idle"`
}

type containerStatus struct {
	Role   string `json:"role"` // server, mcp, remote
	Name   string `json:"name"`
	State  string `json:"state"` // absent, running, exited, ...
	Image  string `json:"image,omitempty"`
	URL    string `json:"url"`
	Memory string `json:"memory,omitempty"` // in use now, running containers only
}

type idleStatus struct {
	Timeout      string    `json:"timeout"` // "0s" = auto-stop disabled
	LastActivity time.Time `json:"lastActivity,omitzero"`
}

func statusCommand() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show containers (with memory), gateway, stdio bridges, running scans and idle-stop state",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			e, err := load("", false)
			if err != nil {
				return err
			}
			if err := docker.Available(cmd.Context()); err != nil {
				return err
			}
			r := collectStatus(cmd.Context(), e, nil)
			if asJSON {
				b, _ := json.MarshalIndent(r, "", "  ")
				fmt.Fprintln(cmd.OutOrStdout(), string(b))
				return nil
			}
			printStatus(cmd.OutOrStdout(), r)
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print the status as JSON")
	return cmd
}

// collectStatus gathers what `status` and the sonarless_status tool show.
// With docker unavailable (dockerErr) containers are skipped and Docker
// says why; the gateway, bridges and scans are still reported.
func collectStatus(ctx context.Context, e *env, dockerErr error) statusReport {
	r := statusReport{
		Containers: []containerStatus{},
		Gateway:    mcp.GatewayStatus(ctx, e.cfg),
		Shims:      []idle.Proc{},
		Scans:      []idle.Proc{},
	}
	for _, p := range idle.Running(e.cfg, idle.KindShim, idle.KindScan) { // one process query for both
		if p.Kind == idle.KindShim {
			r.Shims = append(r.Shims, p)
		} else {
			r.Scans = append(r.Scans, p)
		}
	}
	timeout, _ := e.cfg.IdleTimeout()
	r.Idle = idleStatus{Timeout: timeout.String(), LastActivity: idle.Last(e.cfg)}
	if dockerErr != nil {
		r.Docker = dockerErr.Error()
		return r
	}
	r.Containers = append(r.Containers,
		containerStatus{Role: "server", Name: e.cfg.ServerContainer(), URL: e.cfg.ServerURL()},
		containerStatus{Role: "mcp", Name: e.cfg.MCPContainer(), URL: e.cfg.MCPURL()})
	urls := map[string]string{}
	for _, c := range remoteContainers(e) {
		urls[c.Name] = c.SonarURL
	}
	for _, name := range remoteContainerNames(ctx, e) {
		r.Containers = append(r.Containers, containerStatus{Role: "remote", Name: name, URL: urls[name]})
	}
	var running []string
	for i := range r.Containers {
		c := &r.Containers[i]
		c.State = "absent"
		if ct, err := docker.Inspect(ctx, c.Name); err == nil {
			c.State, c.Image = ct.Status, ct.Image
			if ct.Running() {
				running = append(running, c.Name)
			}
		}
	}
	mem, _ := docker.Memory(ctx, running...)
	for i := range r.Containers {
		r.Containers[i].Memory = mem[r.Containers[i].Name]
	}
	return r
}

// scanLine describes a running scan: "app (maven, scanning 2m0s, /ws/app)".
func scanLine(p idle.Proc) string {
	state := p.State
	if state == "" {
		state = idle.ScanScanning
	}
	return fmt.Sprintf("%s (%s, %s %s, %s)", p.Project, p.Scanner, state, since(p.Started), p.Workspace)
}

func printStatus(w io.Writer, r statusReport) {
	for _, c := range r.Containers {
		state := c.State
		if c.Image != "" {
			state += " (" + c.Image + ")"
		}
		mem := c.Memory
		if mem == "" {
			mem = "-"
		}
		fmt.Fprintf(w, "%-7s %-20s %-48s %-9s %s\n", c.Role, c.Name, state, mem, c.URL)
	}
	g := r.Gateway
	switch {
	case !g.Running:
		fmt.Fprintf(w, "gateway not running (port %d)\n", g.Port)
	default:
		line := fmt.Sprintf("gateway running on port %d", g.Port)
		if g.PID > 0 {
			line += fmt.Sprintf(", pid %d", g.PID)
		}
		if g.Uptime != "" {
			line += ", up " + g.Uptime
		}
		fmt.Fprintf(w, "%s; %d session(s)\n", line, len(g.Sessions))
		for _, s := range g.Sessions {
			ws := s.Workspace
			if ws == "" {
				ws = "(no workspace)"
			}
			fmt.Fprintf(w, "  session %s, last request %s ago\n", ws, since(s.LastRequest))
		}
		if g.Note != "" {
			fmt.Fprintf(w, "  %s\n", g.Note)
		}
	}
	if len(r.Shims) == 0 {
		fmt.Fprintln(w, "bridge  no stdio bridges running")
	}
	for _, p := range r.Shims {
		fmt.Fprintf(w, "bridge  pid %d, %s, up %s\n", p.PID, orNone(p.Workspace), since(p.Started))
	}
	if len(r.Scans) == 0 {
		fmt.Fprintln(w, "scan    none running")
	}
	for _, p := range r.Scans {
		fmt.Fprintf(w, "scan    pid %d, %s\n", p.PID, scanLine(p))
	}
	timeout, _ := time.ParseDuration(r.Idle.Timeout)
	switch {
	case timeout == 0:
		fmt.Fprintln(w, "idle    auto-stop disabled")
	case r.Idle.LastActivity.IsZero():
		fmt.Fprintf(w, "idle    auto-stop after %s; no activity recorded\n", timeout)
	default:
		fmt.Fprintf(w, "idle    auto-stop after %s; last activity %s ago\n", timeout, since(r.Idle.LastActivity))
	}
}

func since(t time.Time) time.Duration { return time.Since(t).Round(time.Second) }

func orNone(ws string) string {
	if ws == "" {
		return "(no workspace)"
	}
	return ws
}

// remoteContainerNames is every remote MCP container: those of configured
// remotes plus any left from a removed remote (managed, named like one).
func remoteContainerNames(ctx context.Context, e *env) []string {
	seen := map[string]bool{}
	var names []string
	add := func(n string) {
		if !seen[n] {
			seen[n] = true
			names = append(names, n)
		}
	}
	for _, c := range remoteContainers(e) {
		add(c.Name)
	}
	managed, _ := docker.Names(ctx, sonar.LabelManaged+"=true")
	for _, n := range managed {
		if strings.HasPrefix(n, e.cfg.MCPContainer()+"-") {
			add(n)
		}
	}
	return names
}

// stopContainers stops the local MCP, the server and every remote MCP
// container (what `stop` and the idle watcher do).
func stopContainers(ctx context.Context, e *env) error {
	errs := []error{e.mcp.Stop(ctx), e.server.Stop(ctx)}
	for _, n := range remoteContainerNames(ctx, e) {
		errs = append(errs, docker.Stop(ctx, n))
	}
	return errors.Join(errs...)
}

// stopGateway stops the gateway: through its login service where setup
// installed one (a service manager would restart a killed gateway at once),
// then any gateway still recorded in the cache dir (Windows, or one Claude
// Code's helper started). It reports whether a gateway was asked to stop.
// A client that connects later starts it again (Claude Code's helper, or
// the login service at the next login).
func stopGateway(e *env, ce clients.Env) (bool, error) {
	var errs []error
	asked := false
	if clients.AutostartInstalled(ce) {
		base := filepath.Base(clients.AutostartFile(ce))
		switch ce.GOOS {
		case "linux":
			_, err := ce.Run("systemctl", "--user", "stop", base)
			errs, asked = append(errs, err), err == nil
		case "darwin":
			_, err := ce.Run("launchctl", "bootout", fmt.Sprintf("gui/%d/%s", os.Getuid(), strings.TrimSuffix(base, ".plist")))
			asked = err == nil // fails when not loaded: nothing to stop
		}
	}
	stopped, err := mcp.StopGateway(e.cfg)
	return asked || stopped, errors.Join(append(errs, err)...)
}

func stopCommand() *cobra.Command {
	var all bool
	cmd := &cobra.Command{
		Use:   "stop",
		Short: "Stop the SonarQube server and MCP containers (--all: the gateway too)",
		Long: `Stop the shared SonarQube server, the local MCP container and every remote
MCP container. Data is kept; the next request or scan starts them again.

--all also stops the gateway AI clients connect to. Plain stop leaves it
running on purpose: it holds no container memory, and connected clients
(and the login service that keeps it up) expect it to answer.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			e, err := load("", false)
			if err != nil {
				return err
			}
			return runStop(cmd.Context(), cmd.OutOrStdout(), e, all, clients.DefaultEnv())
		},
	}
	cmd.Flags().BoolVar(&all, "all", false, "also stop the gateway")
	return cmd
}

func runStop(ctx context.Context, w io.Writer, e *env, all bool, ce clients.Env) error {
	err := stopContainers(ctx, e)
	if all {
		stopped, gerr := stopGateway(e, ce)
		err = errors.Join(err, gerr)
		if stopped && gerr == nil {
			fmt.Fprintln(w, "Gateway stopped.")
		}
	}
	if err != nil {
		return err
	}
	fmt.Fprintln(w, "Stopped.")
	return nil
}
