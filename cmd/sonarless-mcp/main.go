// Command sonarless-mcp runs local SonarQube for AI agents; the commands live
// in internal/cli.
package main

import "github.com/BrainerVirus/sonarless-mcp/internal/cli"

// Set by the release build (.goreleaser.yaml ldflags -X main.version, main.commit, main.date).
var (
	version = "dev"
	commit  = ""
	date    = ""
)

func main() { cli.Main(version, commit, date) }
