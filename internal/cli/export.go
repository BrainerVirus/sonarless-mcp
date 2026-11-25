package cli

import (
	"context"
	"io"

	"github.com/spf13/cobra"
)

// NewRootCommand builds the sonarless-mcp command tree without running it.
// The skill bench's fake sonarless-mcp parses command lines with it, so its
// commands, flags and help are the real ones.
func NewRootCommand(ctx context.Context) *cobra.Command { return newRootCommand(ctx) }

// PrintUsageHint writes what sonarless-mcp prints after a usage error: the
// flag it was probably after and where the usage is.
func PrintUsageHint(w io.Writer, cmd *cobra.Command, err error, args []string) {
	printUsageHint(w, cmd, err, args)
}
