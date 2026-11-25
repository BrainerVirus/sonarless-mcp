package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/BrainerVirus/sonarless-mcp/internal/clients"
	"github.com/BrainerVirus/sonarless-mcp/internal/config"
	"github.com/BrainerVirus/sonarless-mcp/internal/idle"
	"github.com/BrainerVirus/sonarless-mcp/internal/mcp"
	"github.com/spf13/cobra"
)

// gatewaySession builds the shim the gateway runs for one workspace: the
// same backends and sonarless_* tools as `sonarless-mcp mcp <workspace>`.
func gatewaySession(ws string) (*mcp.Shim, error) {
	e, err := load(ws, ws != "")
	if err != nil {
		return nil, err
	}
	backends, err := shimBackends(e)
	if err != nil {
		return nil, err
	}
	tools := agentTools(e)
	if e.project == nil {
		// No workspace sent: only the tool that needs none.
		kept := tools[:0]
		for _, t := range tools {
			if t.Name == "sonarless_status" {
				kept = append(kept, t)
			}
		}
		tools = kept
	}
	return &mcp.Shim{Cfg: e.cfg, Backends: backends, Local: tools, Project: e.project, Log: os.Stderr}, nil
}

func serveCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "serve",
		Short: "Run the gateway AI clients connect to over HTTP (started at login by setup)",
		Long: `Serve one Streamable HTTP MCP endpoint on 127.0.0.1 (SONARLESS_GATEWAY_PORT)
for every AI client, instead of one "sonarless-mcp mcp" process per client.
It does what the stdio shim does: starts the containers on the first request,
keeps tokens fresh, defaults projectKey per workspace (X-Sonarless-Workspace
header), routes the "server" argument to remotes and serves the sonarless_*
tools. Clients authenticate with the token in the gateway token file.

setup installs it as a login service (systemd user unit / LaunchAgent);
Claude Code starts it itself through "sonarless-mcp ensure".`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			e, err := load("", false)
			if err != nil {
				return err
			}
			token, err := e.cfg.GatewayToken()
			if err != nil {
				return err
			}
			exe, _ := os.Executable()
			ctx, cancel := context.WithCancel(cmd.Context())
			defer cancel()
			go dailyUpdates(ctx, e.cfg) // no shim runs for direct clients: the gateway keeps the binary current
			g := &mcp.Gateway{Cfg: e.cfg, Token: token, Version: version, Log: os.Stderr, NewSession: gatewaySession}
			replaced := make(chan struct{})
			go watchBinary(ctx, exe, g, func() { close(replaced); cancel() })
			err = g.Serve(ctx)
			if errors.Is(err, mcp.ErrGatewayRunning) {
				fmt.Fprintln(os.Stderr, "sonarless-mcp: gateway already running on", e.cfg.GatewayURL())
				return nil
			}
			select {
			case <-replaced:
				// Self-update replaced the binary: a service manager restarts
				// us; otherwise start the new binary ourselves.
				if os.Getenv(clients.SupervisedEnv) == "" {
					return idle.SpawnExe(exe, e.cfg, "gateway.log", "serve")
				}
			default:
			}
			return err
		},
	}
}

// dailyUpdates runs the update check the shim runs on each start; it only
// acts once a day (update.Due), the new binary is picked up by watchBinary.
func dailyUpdates(ctx context.Context, cfg *config.Config) {
	if !cfg.Bool(config.AutoUpdate) {
		return // also leaves installed skill copies alone
	}
	for {
		autoUpdate(cfg)
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Hour):
		}
	}
}

// watchBinary calls replaced once the executable on disk is a different
// file (self-update) and no request is in flight.
func watchBinary(ctx context.Context, exe string, g *mcp.Gateway, replaced func()) {
	orig, err := os.Stat(exe)
	if err != nil {
		return
	}
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if now, err := os.Stat(exe); err == nil && !os.SameFile(orig, now) && !g.Busy() {
			fmt.Fprintf(os.Stderr, "%s binary updated; restarting\n", time.Now().Format(time.RFC3339))
			replaced()
			return
		}
	}
}

func ensureCommand() *cobra.Command {
	var headers bool
	cmd := &cobra.Command{
		Use:   "ensure",
		Short: "Start the gateway unless it is running (cheap when it is)",
		Long: `Start the gateway ("sonarless-mcp serve") in the background unless it already
answers, and wait until it does. With --headers, also print the HTTP headers a
client sends it as JSON (the token, and the workspace Claude Code runs in): Claude Code runs "sonarless-mcp ensure --headers" as
the headersHelper of its direct entry, so the token never sits in its config.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			e, err := load("", false)
			if err != nil {
				return err
			}
			token, err := e.cfg.GatewayToken()
			if err != nil {
				return err
			}
			// Claude Code gives the helper 10s; the gateway itself starts in
			// milliseconds (containers start on its first request, not here).
			ctx, cancel := context.WithTimeout(cmd.Context(), 6*time.Second)
			defer cancel()
			ensureErr := mcp.EnsureGateway(ctx, e.cfg, 5*time.Second)
			if headers {
				if ensureErr != nil {
					fmt.Fprintln(os.Stderr, "sonarless-mcp:", ensureErr)
				}
				h := map[string]string{"Authorization": "Bearer " + token}
				if ws := helperWorkspace(); ws != "" {
					h[mcp.WorkspaceHeader] = ws
				}
				b, _ := json.Marshal(h)
				fmt.Println(string(b))
				return nil
			}
			if ensureErr != nil {
				return ensureErr
			}
			fmt.Println("gateway is up:", e.cfg.GatewayURL())
			return nil
		},
	}
	cmd.Flags().BoolVar(&headers, "headers", false, "print the client's auth headers as JSON (Claude Code headersHelper)")
	return cmd
}

// gatewayDirect is where direct client entries point.
func gatewayDirect(cfg *config.Config, exe string) clients.Direct {
	return clients.Direct{Exe: exe, URL: cfg.GatewayURL(), TokenFile: cfg.GatewayTokenFile(), TokenEnv: config.GatewayTokenEnv}
}

// gatewayEnv is what the autostarted gateway needs from setup's environment
// to read the same config as everything else.
func gatewayEnv() map[string]string {
	env := map[string]string{}
	for _, k := range []string{"SONARLESS_HOME", "XDG_CONFIG_HOME", "XDG_CACHE_HOME", "DOCKER_HOST", "PATH"} {
		if v, ok := os.LookupEnv(k); ok && v != "" {
			env[k] = v
		}
	}
	for _, k := range config.Keys {
		if v, ok := os.LookupEnv(k.Name); ok && v != "" && !k.Project {
			env[k.Name] = v
		}
	}
	return env
}

func gatewayLog(cfg *config.Config) string { return filepath.Join(cfg.CacheDir, "gateway.log") }

// removeGateway undoes what setup did for the gateway once no client uses
// it: the login autostart, and a gateway still running without a service
// manager (Windows, or one Claude Code's helper started).
func removeGateway(env clients.Env) {
	if clients.AutostartInstalled(env) {
		if err := clients.RemoveAutostart(env); err != nil {
			fmt.Printf("  ✗ gateway autostart not removed: %v\n", err)
		} else {
			fmt.Println("  ✓ gateway autostart removed")
		}
	}
	cfg, _, err := config.Load("")
	if err != nil {
		return
	}
	if stopped, err := mcp.StopGateway(cfg); err != nil {
		fmt.Printf("  ✗ gateway not stopped: %v\n", err)
	} else if stopped {
		fmt.Println("  ✓ gateway stopped")
	}
}
