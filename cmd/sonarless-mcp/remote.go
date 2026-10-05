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
		// A broken remotes file must not take the local server down with it.
		fmt.Fprintf(os.Stderr, "sonarless-mcp: ignoring remotes: %v\n", err)
		return backends, nil
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
				// Run exactly the MCP server version the local container runs:
				// agents see one tool list, so every server must share its schemas.
				if id := e.mcp.RunningImageID(ctx); id != "" {
					c.Image = id
				}
				if c.Alive(ctx) && (c.Image == e.cfg.Get(config.MCPImage) || c.RunningImageID(ctx) == c.Image) {
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
				if err == nil && force {
					ok, cerr := sonar.NewToken(r.URL, t).CheckAuth(ctx)
					switch {
					case cerr != nil:
						return "", fmt.Errorf("couldn't verify the token with %s (network/VPN?): %v", r.URL, cerr)
					case !ok:
						return "", fmt.Errorf("token rejected by %s; update it with `sonarless-mcp remote update %s --token`", r.URL, r.Name)
					}
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
			if _, exists, err := remote.Get(e.cfg, args[0]); err != nil {
				return err
			} else if exists {
				return fmt.Errorf("remote %q already exists; change it with `sonarless-mcp remote update %s --url ... / --token / --branch ...`", args[0], args[0])
			}
			token, err := readToken(cmd.Context(), tokenStdin)
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

	var upURL, upBranch string
	var upToken, upTokenStdin bool
	update := &cobra.Command{
		Use:   "update <name> [--url <url>] [--branch <branch>] [--token]",
		Short: "Change a remote's URL, branch and/or token, keeping everything else",
		Long: `Change only what you pass: --url (e.g. the server moved), --branch, and/or
--token (asks for the new token with hidden input; --token-stdin or
$SONARLESS_REMOTE_TOKEN also work). A changed URL or token is verified against
the server; if it's unreachable (VPN down) the change is saved with a warning.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			e, err := load("", false)
			if err != nil {
				return err
			}
			r, exists, err := remote.Get(e.cfg, args[0])
			if err != nil {
				return err
			}
			if !exists {
				return fmt.Errorf("no remote named %q; add it with `sonarless-mcp remote add %s --url ...`", args[0], args[0])
			}
			changed := false
			if cmd.Flags().Changed("url") {
				r.URL, changed = upURL, true
			}
			if cmd.Flags().Changed("branch") {
				r.Branch, changed = upBranch, true
			}
			var token string
			if upToken || upTokenStdin {
				if token, err = readToken(cmd.Context(), upTokenStdin); err != nil {
					return err
				}
				changed = true
			} else if token, err = remote.Token(e.cfg, r.Name); err != nil {
				return err
			}
			if !changed {
				return errors.New("nothing to update: pass --url, --branch and/or --token")
			}
			if err := addRemote(cmd.Context(), e.cfg, r, token); err != nil {
				return err
			}
			// The remote's MCP container points at the old URL; it is recreated
			// on next use (its spec includes the URL).
			return nil
		},
	}
	update.Flags().StringVar(&upURL, "url", "", "new SonarQube URL")
	update.Flags().StringVar(&upBranch, "branch", "", "new default branch (\"\" for the server's main branch)")
	update.Flags().BoolVar(&upToken, "token", false, "replace the token (hidden prompt)")
	update.Flags().BoolVar(&upTokenStdin, "token-stdin", false, "replace the token, read from stdin")

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
			token, err := validToken(f.Token)
			if err != nil {
				return err
			}
			return addRemote(cmd.Context(), e.cfg, remote.Remote{Name: args[0], URL: f.URL, Branch: importBranch}, token)
		},
	}
	imp.Flags().StringVar(&importBranch, "branch", "", "default branch for tools that take one (e.g. develop)")

	sync := &cobra.Command{
		Use:   "sync <name> [dir]",
		Short: "Make local scans judge the project like the remote (quality gate, rule sets, new code)",
		Long: `Copy the remote server's quality gate, the quality profiles (rule sets) the
project uses there and its new-code definition to the local server, and apply
them to the project. Local scans then pass or fail on the same conditions as CI.

Copies are named "<remote>: <name>" so nothing local or built-in is changed;
built-in profiles identical on both servers (same SonarQube version) are kept
as they are. Run it again any time to pick up changes on the remote.`,
		Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			dir := ""
			if len(args) == 2 {
				dir = args[1]
			}
			e, err := load(dir, true)
			if err != nil {
				return err
			}
			r, exists, err := remote.Get(e.cfg, args[0])
			if err != nil {
				return err
			}
			if !exists {
				return fmt.Errorf("no remote named %q", args[0])
			}
			token, err := remote.Token(e.cfg, r.Name)
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			if err := e.server.Ensure(ctx); err != nil {
				return err
			}
			p := e.project
			fmt.Printf("Syncing %s (%s) from %s to the local server...\n", p.Key, p.KeySource, r.URL)
			rep, syncErr := sonar.SyncProject(ctx, sonar.NewToken(r.URL, token), e.server.Admin(), p.Key, p.Name, r.Name)
			if rep == nil {
				return syncErr
			}
			fmt.Printf("  quality gate:  %s\n", rep.Gate)
			for _, s := range rep.GateSkipped {
				fmt.Printf("    skipped condition: %s\n", s)
			}
			if len(rep.Profiles) > 0 {
				fmt.Printf("  rule sets:     %s\n", strings.Join(rep.Profiles, ", "))
			}
			if len(rep.ProfilesSame) > 0 {
				fmt.Printf("  rule sets already identical (built-in, SonarQube %s): %d languages\n", rep.LocalVersion, len(rep.ProfilesSame))
			}
			for _, f := range rep.RuleFailures {
				fmt.Printf("    %s\n", f)
			}
			if rep.NewCode != "" {
				fmt.Printf("  new code:      %s\n", rep.NewCode)
			}
			if rep.RemoteVersion != rep.LocalVersion {
				fmt.Printf("  note: remote runs SonarQube %s, local %s; set SONARLESS_SONARQUBE_VERSION to match for identical analyzers\n", rep.RemoteVersion, rep.LocalVersion)
			}
			for _, sk := range rep.ProfilesSkip {
				fmt.Printf("    rule set not copied: %s\n", sk)
			}
			if syncErr != nil {
				return fmt.Errorf("partly applied (above): %w", syncErr)
			}
			fmt.Println("Run `sonarless-mcp scan` to see the project through the remote's rules.")
			return nil
		},
	}

	cmd.AddCommand(add, update, list, remove, imp, sync)
	return cmd
}

func addRemote(ctx context.Context, cfg *config.Config, r remote.Remote, token string) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	c := sonar.NewToken(strings.TrimRight(r.URL, "/"), token)
	reachable := true
	if _, err := c.Status(ctx); err != nil {
		if ctx.Err() != nil && errors.Is(ctx.Err(), context.Canceled) {
			return errors.New("cancelled; nothing was saved")
		}
		var apiErr *sonar.APIError
		if !errors.As(err, &apiErr) {
			reachable = false // offline or VPN down: save now, verify on first use
		}
	}
	if reachable && !c.Valid(ctx) {
		return fmt.Errorf("%s rejected the token", r.URL)
	}
	_, existed, _ := remote.Get(cfg, r.Name)
	saved, err := remote.Add(cfg, r, token)
	if err != nil {
		return err
	}
	if !reachable {
		fmt.Printf("warning: %s is unreachable right now (VPN or network down?); saved without verifying the token.\n", r.URL)
	}
	verb := "Added"
	if existed {
		verb = "Updated"
	}
	fmt.Printf("%s remote %q (%s", verb, saved.Name, saved.URL)
	if saved.Branch != "" {
		fmt.Printf(", branch %s", saved.Branch)
	}
	fmt.Printf("). MCP tools now accept server: %q; restart your AI clients to see it.\n", saved.Name)
	return nil
}

// readToken gets a token from stdin, $SONARLESS_REMOTE_TOKEN or a hidden
// prompt. Ctrl+C at the prompt cancels cleanly: the terminal's echo is
// restored and nothing is saved.
func readToken(ctx context.Context, stdin bool) (string, error) {
	var tok string
	switch {
	case stdin:
		b, err := io.ReadAll(bufio.NewReader(os.Stdin))
		if err != nil {
			return "", err
		}
		tok = string(b)
	case os.Getenv("SONARLESS_REMOTE_TOKEN") != "":
		tok = os.Getenv("SONARLESS_REMOTE_TOKEN")
	case term.IsTerminal(int(os.Stdin.Fd())):
		fd := int(os.Stdin.Fd())
		state, err := term.GetState(fd)
		if err != nil {
			return "", err
		}
		fmt.Fprint(os.Stderr, "SonarQube token (input hidden, Ctrl+C to cancel): ")
		type result struct {
			b   []byte
			err error
		}
		done := make(chan result, 1)
		go func() { b, err := term.ReadPassword(fd); done <- result{b, err} }()
		select {
		case r := <-done:
			fmt.Fprintln(os.Stderr)
			if r.err != nil {
				return "", r.err
			}
			tok = string(r.b)
		case <-ctx.Done(): // our signal handler swallowed Ctrl+C; the read is still pending
			_ = term.Restore(fd, state)
			fmt.Fprintln(os.Stderr)
			return "", errors.New("cancelled; nothing was saved")
		}
	default:
		return "", errors.New("no token: use --token-stdin, $SONARLESS_REMOTE_TOKEN or run in a terminal")
	}
	return validToken(tok)
}

// validToken rejects empty or mangled input (whitespace, control
// characters, too short) before anything is saved.
func validToken(t string) (string, error) {
	t = strings.TrimSpace(t)
	if t == "" {
		return "", errors.New("empty token; nothing was saved")
	}
	for _, r := range t {
		if r <= ' ' || r == 0x7f {
			return "", errors.New("token contains spaces or control characters; nothing was saved")
		}
	}
	switch {
	case strings.HasPrefix(t, "sqa_"), strings.HasPrefix(t, "sqp_"):
		return "", errors.New("that's an analysis token (sqa_/sqp_): it can only submit scans, not read results; " +
			"create a User token (squ_) under My Account > Security instead; nothing was saved")
	}
	if len(t) < 16 {
		return "", fmt.Errorf("token is too short (%d characters) to be a SonarQube token; nothing was saved", len(t))
	}
	return t, nil
}
