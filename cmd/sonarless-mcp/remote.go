package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/BrainerVirus/sonarless-mcp/internal/clients"
	"github.com/BrainerVirus/sonarless-mcp/internal/config"
	"github.com/BrainerVirus/sonarless-mcp/internal/docker"
	"github.com/BrainerVirus/sonarless-mcp/internal/mcp"
	"github.com/BrainerVirus/sonarless-mcp/internal/remote"
	"github.com/BrainerVirus/sonarless-mcp/internal/sonar"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

// remoteContainers returns the MCP containers of all configured remotes.
func remoteContainers(e *env) []*mcp.Container {
	rs, _ := remote.Load(e.cfg)
	out := make([]*mcp.Container, len(rs))
	for i, r := range rs {
		out[i] = mcp.NewRemote(e.cfg, os.Stderr, r.Name, r.URL, r.Port)
	}
	return out
}

// shimBackends builds the local backend plus one per configured remote.
func shimBackends(e *env) ([]*mcp.Backend, error) {
	local := &mcp.Backend{
		Name:   remote.Local,
		About:  "the local SonarQube, scanned from your working copy (`sonarless-mcp scan`)",
		APIURL: e.cfg.ServerURL(),
		MCPURL: e.mcp.URL(),
		Ensure: func(ctx context.Context) error {
			if e.mcp.Alive(ctx) && e.server.Running(ctx) {
				return nil
			}
			if err := e.server.Ensure(ctx); err != nil {
				return err
			}
			return e.mcp.Ensure(ctx)
		},
		Token: func(ctx context.Context, _ bool) (string, error) { return e.server.Token(ctx) },
	}
	backends := []*mcp.Backend{local}
	rs, err := remote.Load(e.cfg)
	if err != nil {
		return nil, err
	}
	for _, r := range rs {
		r := r
		c := mcp.NewRemote(e.cfg, os.Stderr, r.Name, r.URL, r.Port)
		about := "remote SonarQube " + r.URL
		if r.Branch != "" {
			about += " (branch " + r.Branch + " by default)"
		}
		backends = append(backends, &mcp.Backend{
			Name: r.Name, About: about, APIURL: r.URL, MCPURL: c.URL(), Branch: r.Branch, Remote: true,
			Ensure: func(ctx context.Context) error {
				if c.Alive(ctx) {
					return nil
				}
				if err := docker.Available(ctx); err != nil {
					return err
				}
				if err := docker.EnsureNetwork(ctx, e.cfg.Network()); err != nil {
					return err
				}
				return c.Ensure(ctx)
			},
			Token: func(ctx context.Context, force bool) (string, error) {
				t, err := remote.Token(e.cfg, r.Name)
				if err == nil && force && !sonar.NewToken(r.URL, t).Valid(ctx) {
					return "", fmt.Errorf("token rejected by %s; update it with `sonarless-mcp remote add %s --url %s`", r.URL, r.Name, r.URL)
				}
				return t, err
			},
		})
	}
	return backends, nil
}

func remoteCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "remote",
		Short: "Manage remote SonarQube servers the MCP tools can also query",
		Long: `Add remote SonarQube servers (e.g. your company's CI server) next to the local
one. Every MCP tool then takes an optional "server" argument: "local" (default,
your working copy) or a remote's name (what CI found). Tools default to the
workspace's project key and the remote's branch.

Settings live in remotes.json in the config dir; each token in its own private
file (never in remotes.json).`,
	}

	var url, branch string
	var tokenStdin bool
	add := &cobra.Command{
		Use:   "add <name> --url <url>",
		Short: "Add or update a remote (token from --token-stdin, $SONARLESS_REMOTE_TOKEN or a prompt)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			e, err := load("", false)
			if err != nil {
				return err
			}
			token, err := readToken(tokenStdin)
			if err != nil {
				return err
			}
			return addRemote(cmd.Context(), e.cfg, remote.Remote{Name: args[0], URL: url, Branch: branch}, token)
		},
	}
	add.Flags().StringVar(&url, "url", "", "SonarQube URL, e.g. https://sonar.example.com")
	add.Flags().StringVar(&branch, "branch", "", "default branch for tools that take one (e.g. develop)")
	add.Flags().BoolVar(&tokenStdin, "token-stdin", false, "read the token from stdin")
	_ = add.MarkFlagRequired("url")

	list := &cobra.Command{
		Use:   "list",
		Short: "List remotes",
		RunE: func(cmd *cobra.Command, _ []string) error {
			e, err := load("", false)
			if err != nil {
				return err
			}
			rs, err := remote.Load(e.cfg)
			if err != nil {
				return err
			}
			if len(rs) == 0 {
				fmt.Println("No remotes. Add one with `sonarless-mcp remote add <name> --url <url>`.")
				return nil
			}
			state := make([]string, len(rs))
			var wg sync.WaitGroup
			for i, r := range rs {
				wg.Add(1)
				go func() { // probe in parallel with a short timeout: offline remotes are normal
					defer wg.Done()
					ctx, cancel := context.WithTimeout(cmd.Context(), 3*time.Second)
					defer cancel()
					_, err := sonar.NewToken(r.URL, "").Status(ctx)
					var apiErr *sonar.APIError
					state[i] = "reachable"
					if err != nil && !errors.As(err, &apiErr) {
						state[i] = "unreachable"
					}
				}()
			}
			wg.Wait()
			for i, r := range rs {
				b := r.Branch
				if b == "" {
					b = "(main branch)"
				}
				fmt.Printf("%-12s %-12s %-48s %s\n", r.Name, state[i], r.URL, b)
			}
			return nil
		},
	}

	remove := &cobra.Command{
		Use:   "remove <name>",
		Short: "Remove a remote, its token and its MCP container",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			e, err := load("", false)
			if err != nil {
				return err
			}
			if err := remote.Remove(e.cfg, args[0]); err != nil {
				return err
			}
			_ = docker.Remove(cmd.Context(), e.cfg.MCPContainer()+"-"+args[0])
			fmt.Printf("Removed remote %s.\n", args[0])
			return nil
		},
	}

	var importBranch string
	imp := &cobra.Command{
		Use:   "import <name>",
		Short: "Add a remote from a SonarQube MCP server already configured in an AI client",
		Long: `Look for a SonarQube MCP entry in your AI clients that isn't sonarless-mcp but
has SONARQUBE_URL and SONARQUBE_TOKEN (for example a docker-based "sonarqube"
server in VS Code or Cursor) and add it as a remote. The client entry is left
as is.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			e, err := load("", false)
			if err != nil {
				return err
			}
			found := clients.FindSonarQubeCredentials(clients.DefaultEnv())
			if len(found) == 0 {
				return errors.New("no SonarQube MCP entry with SONARQUBE_URL and SONARQUBE_TOKEN found in your AI clients")
			}
			f := found[0]
			fmt.Printf("Found %s in %s (%s)\n", f.URL, f.Client, f.File)
			return addRemote(cmd.Context(), e.cfg, remote.Remote{Name: args[0], URL: f.URL, Branch: importBranch}, f.Token)
		},
	}
	imp.Flags().StringVar(&importBranch, "branch", "", "default branch for tools that take one (e.g. develop)")

	cmd.AddCommand(add, list, remove, imp)
	return cmd
}

func addRemote(ctx context.Context, cfg *config.Config, r remote.Remote, token string) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	c := sonar.NewToken(strings.TrimRight(r.URL, "/"), token)
	reachable := true
	if _, err := c.Status(ctx); err != nil {
		var apiErr *sonar.APIError
		if !errors.As(err, &apiErr) {
			reachable = false // offline or VPN down: save now, verify on first use
		}
	}
	if reachable && !c.Valid(ctx) {
		return fmt.Errorf("%s rejected the token", r.URL)
	}
	saved, err := remote.Add(cfg, r, token)
	if err != nil {
		return err
	}
	if !reachable {
		fmt.Printf("warning: %s is unreachable right now (VPN or network down?); saved without verifying the token.\n", r.URL)
	}
	fmt.Printf("Added remote %q (%s", saved.Name, saved.URL)
	if saved.Branch != "" {
		fmt.Printf(", branch %s", saved.Branch)
	}
	fmt.Printf("). MCP tools now accept server: %q; restart your AI clients to see it.\n", saved.Name)
	return nil
}

func readToken(stdin bool) (string, error) {
	switch {
	case stdin:
		b, err := io.ReadAll(bufio.NewReader(os.Stdin))
		return strings.TrimSpace(string(b)), err
	case os.Getenv("SONARLESS_REMOTE_TOKEN") != "":
		return os.Getenv("SONARLESS_REMOTE_TOKEN"), nil
	case term.IsTerminal(int(os.Stdin.Fd())):
		fmt.Fprint(os.Stderr, "SonarQube token (input hidden): ")
		b, err := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Fprintln(os.Stderr)
		return strings.TrimSpace(string(b)), err
	}
	return "", errors.New("no token: use --token-stdin, $SONARLESS_REMOTE_TOKEN or run in a terminal")
}
