package cli

// Modeled on ytrack's `ytrack skill` (github.com/BrainerVirus/youtrack-cli,
// pkg/cmd/skill): list, sync, install, uninstall and print.

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/BrainerVirus/sonarless-mcp/internal/clients"
)

// skillEnv is where the skill commands look for clients and home; tests
// replace it.
var skillEnv = clients.DefaultEnv

func skillCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "skill",
		Short: "Print, install and keep up to date the sonarless-mcp agent skill",
		Long: `Print, install and keep up to date the agent skill that teaches coding
agents (Claude Code, Cursor, opencode, Codex) how to use sonarless-mcp. The
skill is built into this binary and stamped with its version. sonarless-mcp
remembers where it installed the skill and rewrites the copies in your home
directory when a newer sonarless-mcp first runs. These commands work offline.`,
		Args: cobra.NoArgs,
	}
	cmd.AddCommand(skillPrintCommand(), skillInstallCommand(), skillListCommand(), skillSyncCommand(), skillUninstallCommand())
	return cmd
}

func skillPrintCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "print",
		Short: "Print the skill's SKILL.md",
		Long:  "Print the skill's SKILL.md, with metadata.version set to this sonarless-mcp's version.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			_, err := cmd.OutOrStdout().Write(clients.RenderSkill(version))
			return err
		},
	}
}

func skillInstallCommand() *cobra.Command {
	var dir string
	var jsonOut bool
	cmd := &cobra.Command{
		Use:   "install [--dir <skills-dir>]",
		Short: "Install the skill into agents' skills directories",
		Long: `Write the skill to <skills-dir>/sonarless-mcp/SKILL.md, stamped with this
version. Without --dir, install into the skills directory of every detected
client (Claude Code ~/.claude/skills, Cursor ~/.cursor/skills, opencode
~/.config/opencode/skills, Codex ~/.codex/skills).

sonarless-mcp records every directory it installs into (see skill list) and
keeps the copies in your home directory at its own version. A copy in a
repository, such as --dir .claude/skills inside a git work tree, is recorded
but changes only when you run this command again there.

A same-named skill sonarless-mcp did not write is refused and left untouched.

Prints each SKILL.md written, one per line; --json prints {path, version}
objects, one per line.`,
		Example: `  sonarless-mcp skill install
  sonarless-mcp skill install --dir .claude/skills`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			e := skillEnv()
			var dirs []string
			if cmd.Flags().Changed("dir") {
				if dir == "" {
					return errors.New("--dir needs a directory, e.g. --dir ~/.claude/skills")
				}
				abs, err := filepath.Abs(expandHome(e, dir))
				if err != nil {
					return err
				}
				dirs = []string{abs}
			} else {
				for _, c := range clients.All() {
					if d := c.SkillDir(e); d != "" && c.Detected != nil && c.Detected(e) {
						dirs = append(dirs, d)
					}
				}
				if len(dirs) == 0 {
					return errors.New("no AI client with a skills directory found; pass --dir <skills-dir>")
				}
			}
			out, errOut := cmd.OutOrStdout(), cmd.ErrOrStderr()
			var installed []string
			var failed error
			for _, d := range dirs {
				path, err := clients.InstallSkillDir(d, version)
				var foreign *clients.ForeignError
				if errors.As(err, &foreign) {
					err = fmt.Errorf("%w; remove it or choose another --dir", foreign)
				} else if err != nil {
					err = fmt.Errorf("installing the skill into %s: %w", d, err)
				}
				if err != nil {
					failed = errors.Join(failed, err)
					continue
				}
				installed = append(installed, d)
				if jsonOut {
					_ = json.NewEncoder(out).Encode(struct {
						Path    string `json:"path"`
						Version string `json:"version"`
					}{path, version})
				} else {
					fmt.Fprintln(out, path)
				}
			}
			clients.RecordSkills(e, installed, version)
			if failed != nil {
				return failed
			}
			fmt.Fprintf(errOut, "Installed the sonarless-mcp skill %s; `sonarless-mcp skill list` shows every copy.\n", version)
			return nil
		},
	}
	cmd.Flags().StringVar(&dir, "dir", "", "agent skills directory to install into, e.g. ~/.claude/skills (default: every detected client's)")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "print {path, version} as JSON, one object per line")
	return cmd
}

// openSkillRegistry opens the registry under its lock, saving any copies
// adopted on the way. unlock is never nil.
func openSkillRegistry(e clients.Env) (*clients.SkillRegistry, func(), error) {
	unlock := func() {}
	if u, ok := clients.LockSkills(skillLockWait); ok {
		unlock = u
	}
	reg, err := clients.OpenSkillRegistry(e)
	if err != nil {
		unlock()
		return nil, nil, err
	}
	if reg.Dirty() {
		_ = reg.Save()
	}
	return reg, unlock, nil
}

// skillLockWait bounds how long a skill command waits for another
// sonarless-mcp that is syncing the copies; it then goes on without the lock.
const skillLockWait = 3 * time.Second

const noSkillCopies = "No installed copies of the sonarless-mcp skill are recorded; run `sonarless-mcp skill install`."

func updatesOf(c clients.SkillCopy) string {
	if c.Managed {
		return "auto"
	}
	return "manual"
}

func skillListCommand() *cobra.Command {
	var jsonOut bool
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List the installed copies of the skill",
		Long: `List the skills directories sonarless-mcp installed the skill into, with the
version each copy holds and its state:
  current  the version of this sonarless-mcp
  stale    an older version (or a copy written before versions were stamped)
  newer    a newer sonarless-mcp's version; this one leaves it alone
  missing  deleted, or replaced by a skill sonarless-mcp did not write

UPDATES says how a copy is kept at sonarless-mcp's version: auto for a copy in
your home directory, which sonarless-mcp rewrites when a newer release first
runs (or now, with skill sync); manual for a copy in a repository (a git work
tree), which changes only when you run skill install --dir there.

The first run also records copies installed before sonarless-mcp kept a list,
in the skills directories of Claude Code, Cursor, opencode and Codex.

JSON fields (--json), an array:
  dir      the skills directory, e.g. /home/me/.claude/skills
  path     the skill directory, dir/sonarless-mcp
  version  metadata.version of its SKILL.md; empty when missing or unstamped
  state    current, stale, newer or missing
  managed  true when sonarless-mcp keeps it up to date (UPDATES auto)`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			reg, unlock, err := openSkillRegistry(skillEnv())
			if err != nil {
				return err
			}
			unlock()
			copies := reg.Copies(version)
			out, errOut := cmd.OutOrStdout(), cmd.ErrOrStderr()
			if jsonOut {
				type row struct {
					Dir     string `json:"dir"`
					Path    string `json:"path"`
					Version string `json:"version"`
					State   string `json:"state"`
					Managed bool   `json:"managed"`
				}
				rows := make([]row, 0, len(copies))
				for _, c := range copies {
					rows = append(rows, row{c.Dir, c.Path, c.Version, c.State, c.Managed})
				}
				return json.NewEncoder(out).Encode(rows)
			}
			if len(copies) == 0 {
				fmt.Fprintln(errOut, noSkillCopies)
				return nil
			}
			tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
			fmt.Fprintln(tw, "DIR\tVERSION\tSTATE\tUPDATES")
			for _, c := range copies {
				v := c.Version
				if v == "" {
					v = "-"
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", c.Dir, v, c.State, updatesOf(c))
			}
			if err := tw.Flush(); err != nil {
				return err
			}
			skillListHints(errOut, copies)
			return nil
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "print the copies as a JSON array")
	return cmd
}

// skillListHints tells how to bring stale, newer and missing copies in line.
func skillListHints(w io.Writer, copies []clients.SkillCopy) {
	staleAuto, missing, newer := false, false, ""
	for _, c := range copies {
		switch {
		case c.State == clients.StateMissing:
			missing = true
		case c.State == clients.StateNewer:
			newer = c.Version
		case c.State == clients.StateStale && !c.Managed:
			v := c.Version
			if v == "" {
				v = "(unstamped)"
			}
			fmt.Fprintf(w, "%s is in a repository and holds the skill %s; update it with `sonarless-mcp skill install --dir %s`\n", c.Dir, v, c.Dir)
		case c.State == clients.StateStale:
			staleAuto = true
		}
	}
	if staleAuto {
		fmt.Fprintln(w, "Run `sonarless-mcp skill sync` to update the stale copies in your home directory now.")
	}
	if newer != "" {
		fmt.Fprintf(w, "A newer sonarless-mcp (%s) installed some copies; this one (%s) leaves them alone. Upgrade with `sonarless-mcp upgrade`.\n", newer, version)
	}
	if missing {
		fmt.Fprintln(w, "`sonarless-mcp skill sync` forgets the missing copies; `sonarless-mcp skill install --dir <dir>` puts one back.")
	}
}

func skillSyncCommand() *cobra.Command {
	var force bool
	cmd := &cobra.Command{
		Use:   "sync",
		Short: "Update every installed copy of the skill now",
		Long: `Rewrite every copy of the skill in your home directory (UPDATES auto in
skill list) at this sonarless-mcp's version, and forget every copy that is
missing; a missing copy is not re-created. A copy in a repository is left
for skill install --dir.

A copy a newer sonarless-mcp installed is kept (it is not downgraded);
--force rewrites it at this version too.

sonarless-mcp does this by itself the first time a newer release runs.

Prints one line per recorded directory: the action (updated, kept, dropped,
manual or failed), the skill directory and the version it holds. Exits 1
when a copy could not be rewritten.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			reg, unlock, err := openSkillRegistry(skillEnv())
			if err != nil {
				return err
			}
			defer unlock()
			if len(reg.Dirs) == 0 {
				fmt.Fprintln(cmd.ErrOrStderr(), noSkillCopies)
				return nil
			}
			before := reg.Clone()
			sync := reg.Sync
			if force {
				sync = reg.SyncForce
			}
			results, _ := sync(version)
			if !reg.Equal(before) {
				if err := reg.Save(); err != nil {
					fmt.Fprintf(cmd.ErrOrStderr(), "warning: could not save the list of skill copies: %v\n", err)
				}
			}
			var failed error
			for _, r := range results {
				v := r.Version
				switch r.Action {
				case clients.SyncDropped:
					v = "(missing)"
				case clients.SyncFailed:
					failed = errors.Join(failed, fmt.Errorf("updating %s: %w", r.Path, r.Err))
				}
				fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\t%s\n", r.Action, r.Path, v)
			}
			return failed
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "also rewrite copies a newer sonarless-mcp installed")
	return cmd
}

func skillUninstallCommand() *cobra.Command {
	var dir string
	cmd := &cobra.Command{
		Use:   "uninstall --dir <skills-dir>",
		Short: "Remove the skill from a skills directory",
		Long: `Remove <skills-dir>/sonarless-mcp and stop keeping it up to date. A
same-named skill sonarless-mcp did not write is refused and left untouched.
A symlink there is removed, not its target.`,
		Example: `  sonarless-mcp skill uninstall --dir ~/.cursor/skills`,
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if dir == "" {
				return errors.New("--dir is required, e.g. --dir ~/.cursor/skills")
			}
			e := skillEnv()
			root, err := filepath.Abs(expandHome(e, dir))
			if err != nil {
				return err
			}
			removed, err := clients.UninstallSkillDir(root)
			if err != nil {
				return err
			}
			reg, unlock, err := openSkillRegistry(e)
			if err != nil {
				return err
			}
			defer unlock()
			if reg.Forget(root) {
				if err := reg.Save(); err != nil {
					return fmt.Errorf("forgetting %s: %w", root, err)
				}
			}
			if removed {
				fmt.Fprintf(cmd.ErrOrStderr(), "Removed %s\n", filepath.Join(root, "sonarless-mcp"))
			} else {
				fmt.Fprintf(cmd.ErrOrStderr(), "No sonarless-mcp skill in %s\n", root)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&dir, "dir", "", "agent skills directory to remove the skill from")
	return cmd
}

// expandHome expands a leading "~" (as in --dir=~/.claude/skills, which most
// shells leave alone) to the home directory.
func expandHome(e clients.Env, dir string) string {
	rest, ok := strings.CutPrefix(dir, "~")
	if !ok || (rest != "" && rest[0] != '/' && rest[0] != filepath.Separator) || e.Home == "" {
		return dir
	}
	return filepath.Join(e.Home, rest)
}
