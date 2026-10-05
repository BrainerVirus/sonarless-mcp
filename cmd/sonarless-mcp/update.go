package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/BrainerVirus/sonarless-mcp/internal/config"
	"github.com/BrainerVirus/sonarless-mcp/internal/docker"
	"github.com/BrainerVirus/sonarless-mcp/internal/idle"
	"github.com/BrainerVirus/sonarless-mcp/internal/update"
	"github.com/spf13/cobra"
)

// autoUpdate starts the daily background update check when one is due. It
// never blocks or fails the command that triggered it.
func autoUpdate(cfg *config.Config) {
	if update.Enabled(cfg, version) && update.Due(cfg) {
		_ = idle.Spawn(cfg, "update.log", "update", "--background")
	}
}

func updateCommand() *cobra.Command {
	var background bool
	cmd := &cobra.Command{
		Use:   "update",
		Short: "Update sonarless-mcp to the latest release and refresh the MCP image",
		Long: `Download the latest release, verify its checksum, smoke-test it and replace
this binary; the next start runs it. Also pulls the latest SonarQube MCP image,
which the shared container picks up the next time it starts from stopped.

This runs automatically once a day in the background (SONARLESS_AUTO_UPDATE=false
turns that off).`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			e, err := load("", false)
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), 10*time.Minute)
			defer cancel()
			stamp := func() string {
				if background {
					return time.Now().Format(time.RFC3339) + " "
				}
				return ""
			}
			if version == "dev" {
				fmt.Println("development build; not self-updating (install a release to get updates)")
			} else if res, err := update.Check(ctx, e.cfg, version); err != nil {
				fmt.Fprintf(os.Stderr, "%supdate check failed: %v\n", stamp(), err)
			} else if res.Updated {
				fmt.Printf("%supdated sonarless-mcp %s -> %s (%s); restart your AI clients to use it\n", stamp(), res.Current, res.Latest, res.Path)
			} else if !background {
				fmt.Printf("sonarless-mcp %s is the latest\n", res.Current)
			}
			if docker.Available(ctx) == nil {
				img := e.cfg.Get(config.MCPImage)
				before := docker.ImageID(ctx, img)
				if _, err := docker.Run(ctx, "pull", "-q", img); err != nil {
					fmt.Fprintf(os.Stderr, "%spull %s: %v\n", stamp(), img, err)
				} else if after := docker.ImageID(ctx, img); after != before {
					fmt.Printf("%spulled a newer %s; used from the next MCP start\n", stamp(), img)
				}
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&background, "background", false, "quiet, log-friendly output (used by the daily check)")
	_ = cmd.Flags().MarkHidden("background")
	return cmd
}
