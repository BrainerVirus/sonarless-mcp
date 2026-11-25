package cli

import (
	"errors"
	"fmt"

	"github.com/BrainerVirus/sonarless-mcp/internal/config"
	"github.com/BrainerVirus/sonarless-mcp/internal/docker"
	"github.com/BrainerVirus/sonarless-mcp/internal/idle"
	"github.com/BrainerVirus/sonarless-mcp/internal/mcp"
	"github.com/spf13/cobra"
)

// appcdsCommand shows and rebuilds the MCP server's class-data archive.
func appcdsCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "appcds",
		Short: "Show the MCP server's class-data archive (AppCDS); rebuild it",
		Long: `The shared MCP server starts from a class-data archive built in the
background for its image and JVM options (` + config.MCPAppCDS + `). Without
a subcommand, print its state: none, building, ready, used, failed or disabled.

"appcds rebuild" builds it again now, instead of after a day for a failed
build or a rejected archive.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			e, err := load("", false)
			if err != nil {
				return err
			}
			if err := docker.Available(cmd.Context()); err != nil {
				return err
			}
			s := mcp.ArchiveStatus(cmd.Context(), e.cfg)
			fmt.Fprintln(cmd.OutOrStdout(), cdsLine(&s))
			return nil
		},
	}
	var noWait bool
	rebuild := &cobra.Command{
		Use:   "rebuild",
		Short: "Build the MCP class-data archive again now (starts SonarQube if needed)",
		Long: `Drop the archive's record and build it again for the current MCP image and
JVM options. The build is a training start of the MCP server, so it starts
the SonarQube server first if it is not running. It waits for the build
(about 10 s) unless --no-wait; the MCP server uses the new archive from its
next start (sonarless-mcp stop, or the idle stop).`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			e, err := load("", false)
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			if !e.cfg.Bool(config.MCPAppCDS) {
				return fmt.Errorf("the MCP class-data archive is off (%s=false)", config.MCPAppCDS)
			}
			if err := e.server.Ensure(ctx); err != nil {
				return err
			}
			idle.Touch(e.cfg)
			_ = idle.EnsureWatcher(e.cfg)
			s, err := mcp.RebuildArchive(ctx, e.mcp, !noWait)
			if err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), cdsLine(&s))
			if s.State == mcp.CDSFailed {
				return errors.New("the archive build failed")
			}
			return nil
		},
	}
	rebuild.Flags().BoolVar(&noWait, "no-wait", false, "start the build and return without waiting for it")
	cmd.AddCommand(rebuild)
	return cmd
}
