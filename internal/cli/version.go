package cli

// Modeled on ytrack's `ytrack version` (github.com/BrainerVirus/youtrack-cli,
// pkg/cmd/version), with a --json form.

import (
	"encoding/json"
	"fmt"
	"io"
	"runtime"
	"runtime/debug"
	"strings"

	"github.com/spf13/cobra"
)

// The release build's short commit and commit date (YYYY-MM-DD), set by Main.
var (
	commit = ""
	date   = ""
)

// buildInfo is what `sonarless-mcp version` reports.
type buildInfo struct {
	Version string `json:"version"`
	Commit  string `json:"commit"`
	Date    string `json:"date"`
	Go      string `json:"go"`
	OS      string `json:"os"`
	Arch    string `json:"arch"`
}

// currentBuild describes the running binary. A build without the release
// ldflags (go install, go build) takes the commit and date Go recorded.
func currentBuild() buildInfo {
	b := buildInfo{Version: version, Commit: commit, Date: date, Go: runtime.Version(), OS: runtime.GOOS, Arch: runtime.GOARCH}
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, s := range info.Settings {
			switch {
			case s.Key == "vcs.revision" && b.Commit == "":
				b.Commit = s.Value[:min(len(s.Value), 7)]
			case s.Key == "vcs.time" && b.Date == "":
				b.Date = s.Value[:min(len(s.Value), 10)]
			}
		}
	}
	return b
}

func versionCommand() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "version",
		Short: "Show the sonarless-mcp version, commit, build date, Go version and platform",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return writeVersion(cmd.OutOrStdout(), currentBuild(), asJSON)
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print the version information as JSON")
	return cmd
}

// writeVersion renders b, e.g. "sonarless-mcp version 0.9.0 (2026-11-21)\ncommit abc1234\ngo1.27.4 linux/amd64\n".
func writeVersion(w io.Writer, b buildInfo, asJSON bool) error {
	if asJSON {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(b)
	}
	var s strings.Builder
	fmt.Fprintf(&s, "sonarless-mcp version %s", b.Version)
	if b.Date != "" {
		fmt.Fprintf(&s, " (%s)", b.Date)
	}
	s.WriteString("\n")
	if b.Commit != "" {
		fmt.Fprintf(&s, "commit %s\n", b.Commit)
	}
	fmt.Fprintf(&s, "%s %s/%s\n", b.Go, b.OS, b.Arch)
	_, err := io.WriteString(w, s.String())
	return err
}
