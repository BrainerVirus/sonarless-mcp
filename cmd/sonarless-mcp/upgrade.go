package main

// The upgrade command's UX and wording follow ytrack's `ytrack upgrade`
// (github.com/BrainerVirus/youtrack-cli, pkg/cmd/upgrade).

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/sigstore/sigstore-go/pkg/root"
	"github.com/spf13/cobra"

	"github.com/BrainerVirus/sonarless-mcp/internal/clients"
	"github.com/BrainerVirus/sonarless-mcp/internal/config"
	"github.com/BrainerVirus/sonarless-mcp/internal/docker"
	"github.com/BrainerVirus/sonarless-mcp/internal/idle"
	"github.com/BrainerVirus/sonarless-mcp/internal/sonar"
	"github.com/BrainerVirus/sonarless-mcp/internal/update"
)

// The upgrade's side effects outside the binary; tests replace them.
var (
	newUpgrader = defaultUpgrader
	// refreshSkills rewrites the installed agent skill copies; it returns
	// how many it changed.
	refreshSkills = func() int { return clients.RefreshSkills(clients.DefaultEnv()) }
	// refreshImage pulls the configured MCP image.
	refreshImage = pullMCPImage
	// spawnBackground starts the detached background run.
	spawnBackground = func(cfg *config.Config) error {
		return idle.Spawn(cfg, "update.log", "upgrade", "--background")
	}
)

func defaultUpgrader(cfg *config.Config, log io.Writer) *update.Upgrader {
	return &update.Upgrader{
		Current:     version,
		HTTP:        &http.Client{Timeout: 5 * time.Minute},
		TrustedRoot: func() (root.TrustedMaterial, error) { return update.PublicGoodTrustedRoot(cfg.CacheDir) },
		Executable:  os.Executable,
		GOOS:        runtime.GOOS,
		GOARCH:      runtime.GOARCH,
		Run:         update.RunBinary,
		Now:         time.Now,
		StatePath:   update.StatePath(cfg.CacheDir),
		Lock:        func() (func(), error) { return sonar.LockFile(cfg.CacheDir, "update.lock") },
		Log:         log,
	}
}

// autoUpdate starts the background run when one is due: once a day (release
// check, MCP image refresh) and once a newer release has soaked a day. It
// never blocks or fails the command that triggered it.
func autoUpdate(cfg *config.Config) {
	// A new release may carry a new skill text: keep installed copies current.
	refreshSkills()
	if !update.Enabled(cfg, version) {
		return
	}
	path, now := update.StatePath(cfg.CacheDir), time.Now()
	if s := update.ReadState(path); !s.RunDue(version, now) {
		return
	}
	update.UpdateState(path, func(s *update.State) { s.Auto.StartedAt = now })
	_ = spawnBackground(cfg)
}

func upgradeCommand() *cobra.Command {
	var check, background, allowUnsigned bool
	var tag string
	cmd := &cobra.Command{
		Use:     "upgrade",
		Aliases: []string{"update"},
		Short:   "Upgrade sonarless-mcp to the latest release and refresh the MCP image",
		Long: `Upgrade sonarless-mcp to the latest release, or to the release named by --version,
then pull the latest SonarQube MCP image (the shared container picks it up the
next time it starts from stopped).

The release's checksums file must carry a Sigstore (cosign keyless) signature
made by this repository's release workflow on GitHub Actions, and the archive
must match its SHA-256 in that file. The new binary must then run
` + "`sonarless-mcp version`" + `; only then is the running binary replaced, atomically.
Any failed check refuses the upgrade and changes nothing; the next start runs
the new binary.

Releases published before signing existed carry no signature. To go back to
one of them, name it and pass --allow-unsigned: it is then installed on its
checksum alone. A newer release without a signature, or any release whose
signature fails, is always refused.

sonarless-mcp also upgrades itself in the background, once a newer release is
a day old (checked daily); SONARLESS_AUTO_UPDATE=false turns that off. In a
terminal, a one-line notice on stderr names a newer release at most once a day;
SONARLESS_UPDATE_NOTICE=false turns it off.`,
		Example: `  sonarless-mcp upgrade
  sonarless-mcp upgrade --check
  sonarless-mcp upgrade --version v0.9.0
  sonarless-mcp upgrade --version v0.8.1 --allow-unsigned`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			e, err := load("", false)
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), 10*time.Minute)
			defer cancel()
			out, errOut := cmd.OutOrStdout(), cmd.ErrOrStderr()
			switch {
			case background:
				runBackground(ctx, e.cfg, out, errOut)
				return nil
			case check:
				return runCheck(ctx, newUpgrader(e.cfg, errOut), out, tag)
			}
			err = runUpgrade(ctx, newUpgrader(e.cfg, errOut), out, errOut, tag, allowUnsigned)
			if n := refreshSkills(); n > 0 {
				fmt.Fprintf(out, "refreshed the sonarless-mcp skill in %d client(s)\n", n)
			}
			refreshImage(ctx, e.cfg, out, errOut, "")
			return err
		},
	}
	cmd.Flags().BoolVar(&check, "check", false, "only report whether a newer release exists; change nothing")
	cmd.Flags().StringVar(&tag, "version", "", "install this release `tag` (e.g. v0.9.0) instead of the latest")
	cmd.Flags().BoolVar(&allowUnsigned, "allow-unsigned", false, "with --version: install an older release published before signing, on its checksum alone")
	cmd.Flags().BoolVar(&background, "background", false, "the daily background run: quiet, log-friendly, waits a day after a release")
	_ = cmd.Flags().MarkHidden("background")
	cmd.MarkFlagsMutuallyExclusive("check", "allow-unsigned")
	cmd.MarkFlagsMutuallyExclusive("background", "check")
	cmd.MarkFlagsMutuallyExclusive("background", "version")
	cmd.MarkFlagsMutuallyExclusive("background", "allow-unsigned")
	return cmd
}

// runCheck is `upgrade --check`: it reports and changes nothing.
func runCheck(ctx context.Context, u *update.Upgrader, out io.Writer, tag string) error {
	res, err := u.Check(ctx, tag)
	if err != nil {
		return err
	}
	switch {
	case res.Newer:
		fmt.Fprintf(out, "sonarless-mcp %s is available (installed: %s)\n%s\nRun `sonarless-mcp upgrade` to install it.\n", res.Latest, res.Current, res.URL)
	case !update.IsRelease(res.Current):
		fmt.Fprintf(out, "the latest release is %s (installed: %s, a development build)\n", res.Latest, res.Current)
	case tag != "" && res.Latest != res.Current:
		fmt.Fprintf(out, "sonarless-mcp %s is older than the installed %s\n", res.Latest, res.Current)
	default:
		fmt.Fprintf(out, "sonarless-mcp %s is up to date\n", res.Current)
	}
	return nil
}

func runUpgrade(ctx context.Context, u *update.Upgrader, out, errOut io.Writer, tag string, allowUnsigned bool) error {
	if !update.IsRelease(u.Current) && tag == "" {
		fmt.Fprintln(out, "development build; not self-updating (install a release, or name one with --version)")
		return nil
	}
	res, err := u.Upgrade(ctx, tag, allowUnsigned)
	if err != nil {
		return err
	}
	switch {
	case res.Updated:
		if res.Unsigned {
			fmt.Fprintf(errOut, "warning: %s has no signature; it was installed on its checksum alone (--allow-unsigned)\n", res.Latest)
		}
		verb := "Upgraded"
		if !res.Newer {
			verb = "Installed" // an older release, named with --version
		}
		fmt.Fprintf(out, "%s sonarless-mcp %s to %s (%s); restart your AI clients to use it\n", verb, res.Current, res.Latest, res.Path)
	case tag != "":
		fmt.Fprintf(out, "sonarless-mcp %s is already installed\n", res.Current)
	default:
		fmt.Fprintf(out, "sonarless-mcp %s is up to date\n", res.Current)
	}
	return nil
}

// runBackground is `upgrade --background`, which autoUpdate starts: its
// output goes to update.log, one timestamped line per event.
func runBackground(ctx context.Context, cfg *config.Config, out, errOut io.Writer) {
	stamp := time.Now().Format(time.RFC3339) + " "
	_ = os.Remove(filepath.Join(cfg.CacheDir, "last-update-check")) // the state file replaced it
	if update.IsRelease(version) {
		res, err := newUpgrader(cfg, errOut).Background(ctx)
		switch {
		case err != nil:
			fmt.Fprintf(errOut, "%supgrade failed: %v\n", stamp, err)
		case res.Updated:
			fmt.Fprintf(out, "%supgraded sonarless-mcp %s -> %s (%s); restart your AI clients to use it\n", stamp, res.Current, res.Latest, res.Path)
		case res.Waiting:
			fmt.Fprintf(out, "%ssonarless-mcp %s was published at %s; waiting %s before installing it\n",
				stamp, res.Latest, res.PublishedAt.Format(time.RFC3339), update.SoakPeriod)
		case res.BackingOff:
			fmt.Fprintf(out, "%sthe last background upgrade failed; next attempt after %s\n", stamp, res.RetryAt.Format(time.RFC3339))
		}
	}
	refreshSkills()
	refreshImage(ctx, cfg, out, errOut, stamp)
}

// pullMCPImage pulls the configured MCP image when Docker is available.
func pullMCPImage(ctx context.Context, cfg *config.Config, out, errOut io.Writer, stamp string) {
	if docker.Available(ctx) != nil {
		return
	}
	img := cfg.Get(config.MCPImage)
	before := docker.ImageID(ctx, img)
	if _, err := docker.Run(ctx, "pull", "-q", img); err != nil {
		fmt.Fprintf(errOut, "%spull %s: %v\n", stamp, img, err)
	} else if after := docker.ImageID(ctx, img); after != before {
		fmt.Fprintf(out, "%spulled a newer %s; used from the next MCP start\n", stamp, img)
	}
}
