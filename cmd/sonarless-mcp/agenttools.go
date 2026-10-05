package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/BrainerVirus/sonarless-mcp/internal/config"
	"github.com/BrainerVirus/sonarless-mcp/internal/docker"
	"github.com/BrainerVirus/sonarless-mcp/internal/idle"
	"github.com/BrainerVirus/sonarless-mcp/internal/mcp"
	"github.com/BrainerVirus/sonarless-mcp/internal/remote"
	"github.com/BrainerVirus/sonarless-mcp/internal/scan"
	"github.com/BrainerVirus/sonarless-mcp/internal/sonar"
)

// secretsNote tells agents how token-bearing changes are made: by the user
// in a terminal (hidden prompt), never through the chat.
const secretsNote = "Adding a remote or changing its token needs the user's token, so the user runs it in a " +
	"terminal (the token prompt is hidden): `sonarless-mcp remote add <name> --url <url> [--branch <branch>]` or " +
	"`sonarless-mcp remote update <name> --token|--url <url>|--branch <branch>`. Never ask for a token in chat."

// agentTools are sonarless-mcp's own features exposed to AI agents next to
// the SonarQube tools, so an agent can scan, check and sync without knowing
// the CLI.
func agentTools(e *env) []mcp.LocalTool {
	if !e.cfg.Bool(config.AgentTools) {
		return nil
	}
	rs, _ := remote.Load(e.cfg)
	remoteNames := make([]any, len(rs))
	for i, r := range rs {
		remoteNames[i] = r.Name
	}
	tools := []mcp.LocalTool{
		{
			Name: "sonarless_status",
			Description: "sonarless-mcp status: the local SonarQube and MCP containers, configured remote SonarQube " +
				"servers (and whether they are reachable right now), idle auto-stop and version. Use it to diagnose " +
				"problems or before suggesting remote changes.",
			Run: func(ctx context.Context, _ map[string]any) (string, bool) { return statusText(ctx, e), false },
		},
		{
			Name: "sonarless_project",
			Description: "How sonarless-mcp sees this workspace: SonarQube project key and name, where they were " +
				"detected (sonar-project.properties, pom.xml, Gradle, CI pipeline, ...), the scanner it would use, " +
				"and which servers (local, remotes) know the project.",
			Run: func(ctx context.Context, _ map[string]any) (string, bool) { return projectText(ctx, e), false },
		},
		{
			Name: "sonarless_scan",
			Description: "Scan this workspace with the local SonarQube (starting it if needed) and return the " +
				"quality gate result. Takes about 1-5 minutes. Use it after changing code to refresh what the " +
				"SonarQube tools report for server \"local\".",
			InputSchema: map[string]any{"type": "object", "properties": map[string]any{
				"scanner": map[string]any{"type": "string", "enum": []any{"cli", "maven", "gradle", "dotnet"},
					"description": "Override the detected scanner (rarely needed)."},
				"tests": map[string]any{"type": "boolean",
					"description": "Run tests during maven/gradle/dotnet scans (needed for coverage; slower)."},
			}},
			Run: func(ctx context.Context, args map[string]any) (string, bool) { return scanText(ctx, e, args) },
		},
	}
	if len(rs) > 0 {
		tools = append(tools, mcp.LocalTool{
			Name: "sonarless_remote_sync",
			Description: "Make local scans judge this project like a remote server (typically CI): copy its quality " +
				"gate, the rule sets the project uses there and its new-code definition to the local server. Run " +
				"sonarless_scan afterwards to see the project through the remote's rules.",
			InputSchema: map[string]any{"type": "object", "required": []any{"remote"}, "properties": map[string]any{
				"remote": map[string]any{"type": "string", "enum": remoteNames, "description": "Remote to copy from."},
			}},
			Run: func(ctx context.Context, args map[string]any) (string, bool) { return syncText(ctx, e, args) },
		})
	}
	return tools
}

func statusText(ctx context.Context, e *env) string {
	var b strings.Builder
	fmt.Fprintf(&b, "sonarless-mcp %s\n", version)
	if err := docker.Available(ctx); err != nil {
		fmt.Fprintf(&b, "docker: %v\n", err)
	} else {
		for _, c := range []struct{ label, name, url string }{
			{"local server", e.cfg.ServerContainer(), e.cfg.ServerURL()},
			{"local mcp", e.cfg.MCPContainer(), e.cfg.MCPURL()},
		} {
			state := "not created yet (starts on first use)"
			if ct, err := docker.Inspect(ctx, c.name); err == nil {
				state = ct.Status
			}
			fmt.Fprintf(&b, "%s: %s (%s)\n", c.label, state, c.url)
		}
	}
	timeout, _ := e.cfg.IdleTimeout()
	if timeout > 0 {
		fmt.Fprintf(&b, "idle auto-stop: after %s unused (resumes on next use)\n", timeout)
	}
	rs, _ := remote.Load(e.cfg)
	if len(rs) == 0 {
		b.WriteString("remotes: none.\n")
	} else {
		reach := probeRemotes(ctx, rs)
		b.WriteString("remotes (query with the `server` argument of the SonarQube tools):\n")
		for i, r := range rs {
			br := r.Branch
			if br == "" {
				br = "main branch"
			}
			fmt.Fprintf(&b, "  %s: %s, %s, default branch %s\n", r.Name, r.URL, reach[i], br)
		}
	}
	b.WriteString(secretsNote + "\n")
	return b.String()
}

// probeRemotes checks all remotes in parallel with a short timeout.
func probeRemotes(ctx context.Context, rs []remote.Remote) []string {
	out := make([]string, len(rs))
	var wg sync.WaitGroup
	for i, r := range rs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			pctx, cancel := context.WithTimeout(ctx, 3*time.Second)
			defer cancel()
			_, err := sonar.NewToken(r.URL, "").Status(pctx)
			var apiErr *sonar.APIError
			out[i] = "reachable"
			if err != nil && !errors.As(err, &apiErr) {
				out[i] = "UNREACHABLE right now (VPN/network down?)"
			}
		}()
	}
	wg.Wait()
	return out
}

func projectText(ctx context.Context, e *env) string {
	p := e.project
	var b strings.Builder
	fmt.Fprintf(&b, "workspace root: %s\nproject key: %s (from %s)\nproject name: %s\nscanner: %s\n", p.Root, p.Key, p.KeySource, p.Name, p.Build)
	has := func(base, token string) string {
		pctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		err := sonar.NewToken(base, token).Do(pctx, http.MethodGet, "/api/components/show", url.Values{"component": {p.Key}}, nil)
		var apiErr *sonar.APIError
		switch {
		case err == nil:
			return "has it"
		case errors.As(err, &apiErr):
			return "doesn't have it (scan it / not analyzed there)"
		}
		return "unknown (server not running or unreachable)"
	}
	fmt.Fprintf(&b, "local server: %s\n", has(e.cfg.ServerURL(), e.server.ReadToken()))
	rs, _ := remote.Load(e.cfg)
	for _, r := range rs {
		tok, _ := remote.Token(e.cfg, r.Name)
		fmt.Fprintf(&b, "remote %s: %s\n", r.Name, has(r.URL, tok))
	}
	return b.String()
}

func scanText(ctx context.Context, e *env, args map[string]any) (string, bool) {
	opt := scan.Options{Tests: e.cfg.Bool(config.ScanTests), Scanner: e.cfg.Get(config.Scanner)}
	if v, ok := args["scanner"].(string); ok && v != "" {
		opt.Scanner = v
	}
	if v, ok := args["tests"].(bool); ok {
		opt.Tests = v
	}
	var log bytes.Buffer
	opt.Out = &log
	idle.Touch(e.cfg)
	err := scan.Run(ctx, e.cfg, e.server, e.project, opt)
	if err != nil {
		return fmt.Sprintf("Scan of %s failed: %v\n\nLast output:\n%s", e.project.Key, err, tail(log.String(), 40)), true
	}
	return fmt.Sprintf("Scanned %s.\n%s\nThe SonarQube tools (server \"local\") now reflect this scan.",
		e.project.Key, gateSummary(ctx, e)), false
}

func gateSummary(ctx context.Context, e *env) string {
	var r struct {
		ProjectStatus struct {
			Status     string
			Conditions []struct {
				MetricKey, Status, ActualValue, ErrorThreshold, Comparator string
			}
		}
	}
	if err := e.server.Admin().Do(ctx, http.MethodGet, "/api/qualitygates/project_status", url.Values{"projectKey": {e.project.Key}}, &r); err != nil {
		return "Quality gate: unknown (" + err.Error() + ")"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Quality gate: %s", r.ProjectStatus.Status)
	for _, c := range r.ProjectStatus.Conditions {
		if c.Status == "ERROR" {
			fmt.Fprintf(&b, "\n  failing: %s = %s (threshold %s %s)", c.MetricKey, c.ActualValue, c.Comparator, c.ErrorThreshold)
		}
	}
	return b.String()
}

func syncText(ctx context.Context, e *env, args map[string]any) (string, bool) {
	name, _ := args["remote"].(string)
	r, exists, err := remote.Get(e.cfg, name)
	if err != nil || !exists {
		return fmt.Sprintf("No remote named %q. %s", name, secretsNote), true
	}
	token, err := remote.Token(e.cfg, name)
	if err != nil {
		return err.Error(), true
	}
	if err := e.server.Ensure(ctx); err != nil {
		return "Local SonarQube didn't start: " + err.Error(), true
	}
	rep, err := sonar.SyncProject(ctx, sonar.NewToken(r.URL, token), e.server.Admin(), e.project.Key, e.project.Name, r.Name)
	if err != nil {
		return fmt.Sprintf("Sync from %s failed: %v", name, err), true
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Synced %s from %s (%s).\nquality gate: %s\n", e.project.Key, name, r.URL, rep.Gate)
	for _, s := range rep.GateSkipped {
		fmt.Fprintf(&b, "  skipped condition: %s\n", s)
	}
	if len(rep.Profiles) > 0 {
		fmt.Fprintf(&b, "rule sets: %s\n", strings.Join(rep.Profiles, ", "))
	}
	if len(rep.ProfilesSame) > 0 {
		fmt.Fprintf(&b, "rule sets already identical for %d languages\n", len(rep.ProfilesSame))
	}
	for _, f := range rep.RuleFailures {
		fmt.Fprintf(&b, "  %s\n", f)
	}
	if rep.NewCode != "" {
		fmt.Fprintf(&b, "new code: %s\n", rep.NewCode)
	}
	b.WriteString("Run sonarless_scan to see the project through these rules.")
	return b.String(), false
}

func tail(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}
