// Command sonarless-mcp runs SonarQube locally: one shared server and one shared
// MCP server per machine, scans for any build system, and an MCP stdio shim
// for AI clients (Claude Code, Cursor, opencode, VS Code, ...).
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/BrainerVirus/sonarless-mcp/internal/config"
	"github.com/BrainerVirus/sonarless-mcp/internal/docker"
	"github.com/BrainerVirus/sonarless-mcp/internal/idle"
	"github.com/BrainerVirus/sonarless-mcp/internal/mcp"
	"github.com/BrainerVirus/sonarless-mcp/internal/project"
	"github.com/BrainerVirus/sonarless-mcp/internal/scan"
	"github.com/BrainerVirus/sonarless-mcp/internal/sonar"
	"github.com/spf13/cobra"
)

var version = "dev" // set by the release build

// env bundles what most commands need.
type env struct {
	cfg     *config.Config
	project *project.Project
	server  *sonar.Server
	mcp     *mcp.Container
}

// load resolves the project for dir (when withProject) and the config.
func load(dir string, withProject bool) (*env, error) {
	e := &env{}
	root := ""
	if withProject {
		if dir == "" {
			dir, _ = os.Getwd()
		}
		root = project.FindRoot(dir)
	}
	cfg, warnings, err := config.Load(root)
	if err != nil {
		return nil, err
	}
	for _, w := range warnings {
		fmt.Fprintln(os.Stderr, "warning:", w)
	}
	e.cfg = cfg
	e.server = &sonar.Server{Cfg: cfg, Log: os.Stderr}
	e.mcp = &mcp.Container{Cfg: cfg, Log: os.Stderr}
	if withProject {
		p := project.Detect(root)
		if k := cfg.Get(config.ProjectKey); k != "" {
			p.Key, p.KeySource = k, cfg.Source(config.ProjectKey)
		}
		if n := cfg.Get(config.ProjectName); n != "" {
			p.Name, p.NameSource = n, cfg.Source(config.ProjectName)
		}
		e.project = p
	}
	return e, nil
}

func dirArg(args []string) string {
	if len(args) > 0 {
		return args[0]
	}
	return ""
}

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	root := &cobra.Command{
		Use:           "sonarless-mcp",
		Short:         "Local SonarQube for any project, plus a shared MCP server for AI agents",
		SilenceUsage:  true,
		SilenceErrors: true,
		Version:       version,
	}

	root.AddCommand(&cobra.Command{
		Use:   "start",
		Short: "Start (or create) the shared SonarQube server",
		RunE: func(cmd *cobra.Command, _ []string) error {
			e, err := load("", false)
			if err != nil {
				return err
			}
			if err := e.server.Ensure(ctx); err != nil {
				return err
			}
			idle.Touch(e.cfg)
			_ = idle.EnsureWatcher(e.cfg)
			fmt.Printf("SonarQube is up: %s (%s/%s)\n", e.cfg.ServerURL(), e.cfg.Get(config.AdminUser), e.cfg.Get(config.AdminPass))
			return nil
		},
	})

	root.AddCommand(&cobra.Command{
		Use:   "stop",
		Short: "Stop the shared SonarQube server and MCP container",
		RunE: func(cmd *cobra.Command, _ []string) error {
			e, err := load("", false)
			if err != nil {
				return err
			}
			if err := stopAll(ctx, e); err != nil {
				return err
			}
			fmt.Println("Stopped.")
			return nil
		},
	})

	var resetYes bool
	reset := &cobra.Command{
		Use:   "reset",
		Short: "Delete the current SonarQube version's scan history and start fresh",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if !resetYes {
				return errors.New("this deletes the scan history of the current SonarQube version; re-run with --yes")
			}
			e, err := load("", false)
			if err != nil {
				return err
			}
			_ = e.mcp.Stop(ctx)
			if err := e.server.Reset(ctx); err != nil {
				return err
			}
			fmt.Printf("Fresh SonarQube is up: %s\n", e.cfg.ServerURL())
			return nil
		},
	}
	reset.Flags().BoolVar(&resetYes, "yes", false, "confirm deleting this version's scan history")
	root.AddCommand(reset)

	root.AddCommand(&cobra.Command{
		Use:   "status",
		Short: "Show server, MCP and idle-stop state",
		RunE: func(cmd *cobra.Command, _ []string) error {
			e, err := load("", false)
			if err != nil {
				return err
			}
			if err := docker.Available(ctx); err != nil {
				return err
			}
			for _, c := range []struct{ label, name, url string }{
				{"server", e.cfg.ServerContainer(), e.cfg.ServerURL()},
				{"mcp", e.cfg.MCPContainer(), e.cfg.MCPURL()},
			} {
				state := "absent"
				if ct, err := docker.Inspect(ctx, c.name); err == nil {
					state = ct.Status + " (" + ct.Image + ")"
				}
				fmt.Printf("%-7s %-14s %-48s %s\n", c.label, c.name, state, c.url)
			}
			timeout, _ := e.cfg.IdleTimeout()
			if timeout == 0 {
				fmt.Println("idle    auto-stop disabled")
			} else if last := idle.Last(e.cfg); last.IsZero() {
				fmt.Printf("idle    auto-stop after %s; no activity recorded\n", timeout)
			} else {
				fmt.Printf("idle    auto-stop after %s; last activity %s ago\n", timeout, time.Since(last).Round(time.Second))
			}
			return nil
		},
	})

	var scanOpt scan.Options
	scanCmd := &cobra.Command{
		Use:   "scan [dir]",
		Short: "Scan a project (auto-detects key and maven/gradle/dotnet/cli scanner)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			e, err := load(dirArg(args), true)
			if err != nil {
				return err
			}
			if scanOpt.Scanner == "" {
				scanOpt.Scanner = e.cfg.Get(config.Scanner)
			}
			if !cmd.Flags().Changed("tests") {
				scanOpt.Tests = e.cfg.Bool(config.ScanTests)
			}
			return scan.Run(ctx, e.cfg, e.server, e.project, scanOpt)
		},
	}
	scanCmd.Flags().StringVar(&scanOpt.Scanner, "scanner", "", "force scanner: cli, maven, gradle or dotnet")
	scanCmd.Flags().BoolVar(&scanOpt.Tests, "tests", false, "run tests during build-based scans (for coverage)")
	scanCmd.Flags().StringArrayVarP(&scanOpt.ExtraArgs, "define", "D", nil, "extra analysis property, e.g. -D sonar.exclusions=**/gen/**")
	root.AddCommand(scanCmd)

	var resultsOut string
	results := &cobra.Command{
		Use:   "results [dir]",
		Short: "Print the project's key metrics and save them as JSON",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			e, err := load(dirArg(args), true)
			if err != nil {
				return err
			}
			metrics := []string{"alert_status", "bugs", "vulnerabilities", "code_smells", "security_hotspots",
				"coverage", "duplicated_lines_density", "ncloc", "reliability_rating", "security_rating",
				"sqale_rating", "security_review_rating", "open_issues"}
			raw, err := e.server.Admin().Measures(ctx, e.project.Key, metrics)
			if err != nil {
				return fmt.Errorf("%w (is the server running and was %s scanned?)", err, e.project.Key)
			}
			var pretty map[string]any
			_ = json.Unmarshal(raw, &pretty)
			b, _ := json.MarshalIndent(pretty, "", "  ")
			if resultsOut == "" {
				resultsOut = filepath.Join(e.project.Root, e.cfg.Get(config.MetricsFile))
			}
			if err := os.WriteFile(resultsOut, append(b, '\n'), 0o644); err != nil {
				return err
			}
			printMeasures(pretty)
			fmt.Printf("\nSaved to %s\n", resultsOut)
			return nil
		},
	}
	results.Flags().StringVarP(&resultsOut, "output", "o", "", "JSON output file (default: SONAR_METRICS_PATH in the project)")
	root.AddCommand(results)

	root.AddCommand(&cobra.Command{
		Use:   "project [dir]",
		Short: "Show the detected project key, name, scanner and where they came from",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			e, err := load(dirArg(args), true)
			if err != nil {
				return err
			}
			p := e.project
			fmt.Printf("root:    %s\nkey:     %s  (%s)\nname:    %s  (%s)\nscanner: %s\n", p.Root, p.Key, p.KeySource, p.Name, p.NameSource, p.Build)
			if len(p.Props) > 0 {
				fmt.Println("properties:")
				for _, k := range config.SortedKeys(p.Props) {
					fmt.Printf("  %s=%s\n", k, p.Props[k])
				}
			}
			return nil
		},
	})

	configCmd := &cobra.Command{
		Use:   "config [dir]",
		Short: "Show effective settings and where each comes from",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			e, err := load(dirArg(args), true)
			if err != nil {
				return err
			}
			fmt.Printf("# user config: %s\n# project config: %s\n", e.cfg.UserConfigFile(), filepath.Join(e.project.Root, ".sonarless.env"))
			for _, l := range e.cfg.Describe() {
				fmt.Println(l)
			}
			return nil
		},
	}
	root.AddCommand(configCmd)

	mcpCmd := &cobra.Command{
		Use:   "mcp [workspace]",
		Short: "MCP stdio server for AI clients (shares one SonarQube MCP for all of them)",
		Long: `Speak MCP on stdin/stdout, proxying to the shared SonarQube MCP server.

Starts the shared SonarQube server and MCP container on demand (resuming the
existing ones), keeps the token valid, and defaults every tool's projectKey to
the workspace's project. Point your client at "sonarless-mcp mcp"; see
"sonarless-mcp mcp-config <client>" for ready-made snippets.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			e, err := load(dirArg(args), true)
			if err != nil {
				return err
			}
			e.server.Log, e.mcp.Log = os.Stderr, os.Stderr // stdout is the protocol
			shim := &mcp.Shim{Cfg: e.cfg, Server: e.server, MCP: e.mcp, Project: e.project, Log: os.Stderr}
			return shim.Run(ctx, os.Stdin, os.Stdout)
		},
	}
	root.AddCommand(mcpCmd)

	root.AddCommand(&cobra.Command{
		Use:       "mcp-config <claude|opencode|cursor|vscode|codex>",
		Short:     "Print how to register `sonarless-mcp mcp` in an AI client",
		Args:      cobra.ExactArgs(1),
		ValidArgs: []string{"claude", "opencode", "cursor", "vscode", "codex"},
		RunE: func(cmd *cobra.Command, args []string) error {
			exe, err := os.Executable()
			if err != nil {
				return err
			}
			if r, err := filepath.EvalSymlinks(exe); err == nil && !strings.Contains(r, "go-build") {
				exe = r
			}
			s, err := clientSnippet(args[0], exe)
			if err != nil {
				return err
			}
			fmt.Println(s)
			return nil
		},
	})

	root.AddCommand(setupCommand())

	root.AddCommand(&cobra.Command{
		Use:    "daemon",
		Short:  "Idle watcher (started automatically)",
		Hidden: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			e, err := load("", false)
			if err != nil {
				return err
			}
			names := []string{e.cfg.ServerContainer(), e.cfg.MCPContainer()}
			return idle.Watch(ctx, e.cfg, names, func(ctx context.Context) error { return stopAll(ctx, e) }, os.Stderr)
		},
	})

	if err := root.ExecuteContext(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func stopAll(ctx context.Context, e *env) error {
	return errors.Join(e.mcp.Stop(ctx), e.server.Stop(ctx))
}

func printMeasures(m map[string]any) {
	comp, _ := m["component"].(map[string]any)
	ms, _ := comp["measures"].([]any)
	for _, x := range ms {
		mm, _ := x.(map[string]any)
		fmt.Printf("  %-26s %v\n", mm["metric"], mm["value"])
	}
}

func clientSnippet(client, exe string) (string, error) {
	q, _ := json.Marshal(exe)
	switch client {
	case "claude":
		return fmt.Sprintf("claude mcp add -s user sonarqube -- %s mcp", shellQuote(exe)), nil
	case "opencode":
		return fmt.Sprintf(`Add to ~/.config/opencode/opencode.json under "mcp":
  "sonarqube": { "type": "local", "enabled": true, "command": [%s, "mcp"] }`, q), nil
	case "cursor":
		return fmt.Sprintf(`Add to ~/.cursor/mcp.json:
{ "mcpServers": { "sonarqube": { "command": %s, "args": ["mcp", "${workspaceFolder}"] } } }`, q), nil
	case "vscode":
		return fmt.Sprintf(`Add to your VS Code user mcp.json:
{ "servers": { "sonarqube": { "type": "stdio", "command": %s, "args": ["mcp", "${workspaceFolder}"] } } }`, q), nil
	case "codex":
		return fmt.Sprintf(`Add to ~/.codex/config.toml:
[mcp_servers.sonarqube]
command = %s
args = ["mcp"]`, q), nil
	}
	return "", fmt.Errorf("unknown client %q", client)
}

func shellQuote(s string) string {
	if !strings.ContainsAny(s, " '\"$`\\") {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
