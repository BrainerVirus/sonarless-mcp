package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/BrainerVirus/sonarless-mcp/internal/clients"
	"github.com/BrainerVirus/sonarless-mcp/internal/docker"
	"github.com/BrainerVirus/sonarless-mcp/internal/tui"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

func setupCommand() *cobra.Command {
	var yes, remove, list bool
	var only []string
	cmd := &cobra.Command{
		Use:   "setup",
		Short: "Register sonarless-mcp in your AI clients (detects what's installed)",
		Long: `Detect Claude Code, Cursor, opencode, VS Code and Codex, and register the
"sonarqube" MCP server (backed by "sonarless-mcp mcp") in the ones you pick.
Installed clients are preselected. With --remove, unregister instead.

Non-interactive: --list shows what was found without changing anything; --yes
picks every detected client (or every configured one with --remove);
--clients picks an explicit list.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			exe, err := installedExe()
			if err != nil {
				return err
			}
			env := clients.DefaultEnv()
			all := clients.All()

			fmt.Println("Detecting AI clients...")
			opts := make([]tui.Option, len(all))
			byID := map[string]clients.Client{}
			for i, c := range all {
				byID[c.ID] = c
				detected, configured := c.Detected(env), c.Configured(env)
				tag := "not installed"
				switch {
				case configured:
					tag = "already configured"
				case detected:
					tag = "detected"
				}
				avail := detected
				if remove {
					avail = configured
				}
				opts[i] = tui.Option{ID: c.ID, Label: c.Label, Tag: tag, Available: avail, Selected: avail}
			}

			if list {
				for _, o := range opts {
					fmt.Printf("  %-10s %-12s %s\n", o.ID, o.Label, o.Tag)
				}
				return nil
			}

			var chosen []string
			switch {
			case len(only) > 0:
				for _, id := range only {
					if _, ok := byID[id]; !ok {
						return fmt.Errorf("unknown client %q (want %s)", id, clientIDs(all))
					}
				}
				chosen = only
			case yes:
				for _, o := range opts {
					if o.Available {
						chosen = append(chosen, o.ID)
					}
				}
			case !term.IsTerminal(int(os.Stdin.Fd())):
				return errors.New("no terminal for the picker; use --yes or --clients claude,cursor,...")
			default:
				heading := "Register the SonarQube MCP server in:"
				if remove {
					heading = "Remove the SonarQube MCP server from:"
				}
				var ok bool
				if chosen, ok, err = tui.MultiSelect(heading, opts); err != nil {
					return err
				} else if !ok {
					fmt.Println("Cancelled; nothing changed.")
					return nil
				}
			}
			if len(chosen) == 0 {
				fmt.Println("Nothing selected; nothing changed.")
				return nil
			}

			var failed []string
			for _, id := range chosen {
				c := byID[id]
				if remove {
					if err := c.Remove(env); err != nil {
						fmt.Printf("  ✗ %-12s %v\n", c.Label, err)
						failed = append(failed, c.Label)
						continue
					}
					_ = c.RemoveSkill(env)
					fmt.Printf("  ✓ %-12s removed from %s\n", c.Label, c.Where(env))
					continue
				}
				name, err := c.Register(env, exe)
				if err != nil {
					fmt.Printf("  ✗ %-12s %v\n", c.Label, err)
					failed = append(failed, c.Label)
					continue
				}
				note := ""
				if name != clients.ServerName {
					note = fmt.Sprintf(" (your existing %q server was left as is)", clients.ServerName)
				}
				fmt.Printf("  ✓ %-12s registered as %q in %s%s\n", c.Label, name, c.Where(env), note)
				if f, err := c.InstallSkill(env); err != nil {
					fmt.Printf("    (skill not installed: %v)\n", err)
				} else if f != "" {
					fmt.Printf("    + skill %s\n", f)
				}
			}
			if !remove {
				fmt.Println("\nRestart the clients to load it. The first tool call pulls the SonarQube images (a few GB);")
				fmt.Println("run `sonarless-mcp start` now to get that out of the way.")
				if err := docker.Available(context.Background()); err != nil {
					fmt.Println("\nwarning:", err)
				}
			}
			if len(failed) > 0 {
				return fmt.Errorf("failed for: %s", strings.Join(failed, ", "))
			}
			return nil
		},
	}
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "no picker: use every detected (or, with --remove, configured) client")
	cmd.Flags().StringSliceVar(&only, "clients", nil, "no picker: these clients (claude,cursor,opencode,vscode,codex)")
	cmd.Flags().BoolVar(&remove, "remove", false, "unregister instead of register")
	cmd.Flags().BoolVar(&list, "list", false, "only show which clients are detected/configured; change nothing")
	return cmd
}

// installedExe is the path clients should launch: this binary, unless it is
// a `go run` build that disappears after exit.
func installedExe() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	if strings.Contains(exe, "go-build") {
		return "", errors.New("run setup from an installed sonarless-mcp binary, not `go run`")
	}
	if r, err := filepath.EvalSymlinks(exe); err == nil {
		exe = r
	}
	return exe, nil
}

func clientIDs(all []clients.Client) string {
	ids := make([]string, len(all))
	for i, c := range all {
		ids[i] = c.ID
	}
	return strings.Join(ids, ", ")
}
