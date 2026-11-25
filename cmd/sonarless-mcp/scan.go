package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/BrainerVirus/sonarless-mcp/internal/config"
	"github.com/BrainerVirus/sonarless-mcp/internal/scan"
	"github.com/spf13/cobra"
)

// Exit statuses of the binary, listed in the root --help.
const (
	exitError      = 1 // any error, a usage error included
	exitGateFailed = 2 // scan --fail-on-gate: the quality gate failed
)

// exitCodeError carries a non-default exit status out of a command.
type exitCodeError struct {
	code int
	err  error
}

func (e *exitCodeError) Error() string { return e.err.Error() }
func (e *exitCodeError) Unwrap() error { return e.err }

// exitCode is the process exit status for a command's error.
func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var ce *exitCodeError
	if errors.As(err, &ce) {
		return ce.code
	}
	return exitError
}

// runScan runs the scan; a variable so tests can fake it.
var runScan = scan.RunGate

func scanCommand(ctx context.Context) *cobra.Command {
	var opt scan.Options
	var failOnGate bool
	cmd := &cobra.Command{
		Use:   "scan [dir]",
		Short: "Scan a project (auto-detects key and maven/gradle/dotnet/cli scanner)",
		Long: "Scan a project (auto-detects key and maven/gradle/dotnet/cli scanner) and print\n" +
			"its quality gate. A failing gate exits 0 unless --fail-on-gate (or\n" +
			config.FailOnGate + "=true) is set; then it exits 2.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			e, err := load(dirArg(args), true)
			if err != nil {
				return err
			}
			autoUpdate(e.cfg)
			if opt.Scanner == "" {
				opt.Scanner = e.cfg.Get(config.Scanner)
			}
			if !cmd.Flags().Changed("tests") {
				opt.Tests = e.cfg.Bool(config.ScanTests)
			}
			if !cmd.Flags().Changed("fail-on-gate") {
				failOnGate = e.cfg.Bool(config.FailOnGate)
			}
			res, err := runScan(ctx, e.cfg, e.server, e.project, opt)
			if err != nil {
				return err
			}
			return gateVerdict(res, failOnGate)
		},
	}
	cmd.Flags().StringVar(&opt.Scanner, "scanner", "", "force scanner: cli, maven, gradle or dotnet")
	cmd.Flags().BoolVar(&opt.Tests, "tests", false, "run tests during build-based scans (for coverage)")
	cmd.Flags().StringArrayVarP(&opt.ExtraArgs, "define", "D", nil, "extra analysis property, e.g. -D sonar.exclusions=**/gen/**")
	cmd.Flags().BoolVar(&failOnGate, "fail-on-gate", false, "exit 2 when the quality gate fails (for CI and scripts)")
	return cmd
}

// gateVerdict turns a finished scan's gate into the command's result: a
// failing gate is reported, not an error, unless failOnGate. With failOnGate
// an unreadable gate is an error too, so CI never passes without a verdict.
func gateVerdict(res scan.Result, failOnGate bool) error {
	if !failOnGate {
		return nil
	}
	if res.GateErr != nil {
		return fmt.Errorf("could not read the quality gate: %w", res.GateErr)
	}
	if res.Gate == scan.GateFailed {
		return &exitCodeError{code: exitGateFailed, err: errors.New("quality gate failed (--fail-on-gate)")}
	}
	return nil
}
