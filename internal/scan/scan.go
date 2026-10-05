// Package scan runs a SonarQube analysis of a project against the shared
// local server, using the scanner that fits the project's build system.
package scan

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/BrainerVirus/sonarless-mcp/internal/config"
	"github.com/BrainerVirus/sonarless-mcp/internal/docker"
	"github.com/BrainerVirus/sonarless-mcp/internal/idle"
	"github.com/BrainerVirus/sonarless-mcp/internal/project"
	"github.com/BrainerVirus/sonarless-mcp/internal/sonar"
)

// Options tune a scan.
type Options struct {
	Scanner   string   // force cli|maven|gradle|dotnet; "" = auto
	Tests     bool     // run tests (coverage) in build-based scans
	ExtraArgs []string // extra -Dsonar.x=y properties
	Out       io.Writer
}

// Run scans p and waits for the server to finish processing the report.
func Run(ctx context.Context, cfg *config.Config, srv *sonar.Server, p *project.Project, opt Options) error {
	if opt.Out == nil {
		opt.Out = os.Stdout
	}
	kctx, stopKeepalive := context.WithCancel(ctx)
	defer stopKeepalive()
	go idle.Keepalive(kctx, cfg)

	if err := srv.Ensure(ctx); err != nil {
		return err
	}
	_ = idle.EnsureWatcher(cfg)
	token, err := srv.Token(ctx)
	if err != nil {
		return err
	}
	if err := srv.Admin().EnsureProject(ctx, p.Key, p.Name); err != nil {
		return fmt.Errorf("create project %s: %w", p.Key, err)
	}

	scanner := opt.Scanner
	if scanner == "" {
		scanner = p.Build
	}
	fmt.Fprintf(opt.Out, "Scanning %s\n  project: %s (%s)\n  name:    %s\n  scanner: %s\n", p.Root, p.Key, p.KeySource, p.Name, scanner)

	// sonarless-mcp waits for the report itself and reports the gate; a
	// scanner told to wait (sonar.qualitygate.wait in the project's settings)
	// would exit non-zero on a failing gate and look like a failed scan.
	props := map[string]string{"sonar.projectKey": p.Key, "sonar.projectName": p.Name, "sonar.qualitygate.wait": "false"}
	for _, kv := range opt.ExtraArgs {
		k, v, _ := strings.Cut(strings.TrimPrefix(kv, "-D"), "=")
		props[k] = v
	}

	switch scanner {
	case project.BuildMaven:
		err = runMaven(ctx, cfg, p, token, props, opt)
	case project.BuildGradle:
		err = runGradle(ctx, cfg, p, token, props, opt)
	case project.BuildDotnet:
		err = runDotnet(ctx, cfg, p, token, props, opt)
	case project.BuildCLI:
		err = runCLI(ctx, cfg, p, token, props, opt)
	default:
		return fmt.Errorf("unknown scanner %q (want cli, maven, gradle or dotnet)", scanner)
	}
	if err != nil {
		return fmt.Errorf("%s scan failed: %w", scanner, err)
	}
	return waitProcessed(ctx, cfg, srv, p.Key, opt.Out)
}

func defineArgs(prefix string, props map[string]string) []string {
	keys := make([]string, 0, len(props))
	for k := range props {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var out []string
	for _, k := range keys {
		out = append(out, prefix+k+"="+props[k])
	}
	return out
}

// runCLI uses the sonar-scanner CLI container on the shared network.
func runCLI(ctx context.Context, cfg *config.Config, p *project.Project, token string, props map[string]string, opt Options) error {
	if p.PropsFile == "" {
		if _, set := props["sonar.sources"]; !set {
			props["sonar.sources"] = cfg.Get(config.Sources)
		}
	}
	args := []string{"run", "--rm", "--network", cfg.Network(),
		"-e", "SONAR_HOST_URL=" + cfg.ServerURLInNetwork(),
		"-e", "SONAR_TOKEN=" + token,
		"-v", p.Root + ":/usr/src",
	}
	if runtime.GOOS == "linux" { // keep files written into the repo (.scannerwork) owned by the user
		args = append(args, "--user", fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid()), "-e", "HOME=/tmp")
	}
	if d := cfg.PluginsDir(); d != "" {
		if sc := filepath.Join(d, "shellcheck"); fileExists(sc) { // sonar-shellcheck-plugin runs it scanner-side
			args = append(args, "-v", sc+":/usr/local/bin/shellcheck:ro")
		}
	}
	args = append(args, cfg.Get(config.ScannerImage))
	args = append(args, defineArgs("-D", props)...)
	return docker.Stream(ctx, opt.Out, args...)
}

// runMaven prefers the project's wrapper, then a local mvn, then a container.
func runMaven(ctx context.Context, cfg *config.Config, p *project.Project, token string, props map[string]string, opt Options) error {
	goals := []string{"-B"}
	if !opt.Tests {
		goals = append(goals, "-DskipTests")
	}
	goals = append(goals, "verify", "org.sonarsource.scanner.maven:sonar-maven-plugin:sonar")
	if bin := wrapper(p.Root, "mvnw"); bin != "" {
		return local(ctx, p.Root, bin, append(goals, hostProps(cfg, token, props)...), opt.Out)
	}
	if bin, err := exec.LookPath("mvn"); err == nil {
		return local(ctx, p.Root, bin, append(goals, hostProps(cfg, token, props)...), opt.Out)
	}
	m2, err := cacheDir(".m2")
	if err != nil {
		return err
	}
	args := append([]string{"run", "--rm", "--network", cfg.Network(),
		"-v", p.Root + ":/usr/src", "-w", "/usr/src",
		"-v", m2 + ":/tmp/.m2", "-e", "MAVEN_CONFIG=/tmp/.m2"}, asUser()...)
	// user.home=/tmp: writable for any uid (the scanner caches in ~/.sonar).
	args = append(args, "maven:3-eclipse-temurin-21", "mvn", "-Duser.home=/tmp")
	args = append(args, goals...)
	args = append(args, networkProps(cfg, token, props)...)
	fmt.Fprintln(opt.Out, "  (no mvnw/mvn found; using the maven container)")
	return docker.Stream(ctx, opt.Out, args...)
}

// gradleInit applies the SonarQube plugin to builds that don't declare it.
const gradleInit = `initscript {
  repositories { gradlePluginPortal() }
  dependencies { classpath("org.sonarsource.scanner.gradle:sonarqube-gradle-plugin:latest.release") }
}
rootProject {
  afterEvaluate {
    if (!plugins.hasPlugin("org.sonarqube")) {
      apply plugin: org.sonarqube.gradle.SonarQubePlugin
    }
  }
}
`

func runGradle(ctx context.Context, cfg *config.Config, p *project.Project, token string, props map[string]string, opt Options) error {
	initFile := filepath.Join(cfg.CacheDir, "sonarless-init.gradle")
	if err := os.MkdirAll(cfg.CacheDir, 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(initFile, []byte(gradleInit), 0o644); err != nil {
		return err
	}
	tasks := []string{"--no-daemon"}
	if opt.Tests {
		tasks = append(tasks, "build", "sonar")
	} else {
		tasks = append(tasks, "assemble", "sonar", "-x", "test")
	}
	if bin := wrapper(p.Root, "gradlew"); bin != "" {
		return local(ctx, p.Root, bin, append(append([]string{"-I", initFile}, tasks...), hostProps(cfg, token, props)...), opt.Out)
	}
	if bin, err := exec.LookPath("gradle"); err == nil {
		return local(ctx, p.Root, bin, append(append([]string{"-I", initFile}, tasks...), hostProps(cfg, token, props)...), opt.Out)
	}
	gh, err := cacheDir(".gradle")
	if err != nil {
		return err
	}
	args := append([]string{"run", "--rm", "--network", cfg.Network(),
		"-v", p.Root + ":/usr/src", "-w", "/usr/src",
		"-v", gh + ":/gradle-home", "-e", "GRADLE_USER_HOME=/gradle-home",
		"-v", initFile + ":/sonarless-init.gradle:ro"}, asUser()...)
	args = append(args, "gradle:jdk21", "gradle", "-I", "/sonarless-init.gradle")
	args = append(args, tasks...)
	args = append(args, networkProps(cfg, token, props)...)
	fmt.Fprintln(opt.Out, "  (no gradlew/gradle found; using the gradle container)")
	return docker.Stream(ctx, opt.Out, args...)
}

// runDotnet uses the dotnet-sonarscanner global tool (begin, build, end).
func runDotnet(ctx context.Context, cfg *config.Config, p *project.Project, token string, props map[string]string, opt Options) error {
	if _, err := exec.LookPath("dotnet"); err != nil {
		return errors.New("dotnet SDK not found in PATH")
	}
	if out, _ := exec.CommandContext(ctx, "dotnet", "sonarscanner", "--help").CombinedOutput(); !strings.Contains(string(out), "begin") {
		return errors.New("dotnet-sonarscanner not installed; run: dotnet tool install --global dotnet-sonarscanner")
	}
	begin := []string{"sonarscanner", "begin", "/k:" + p.Key, "/n:" + p.Name,
		"/d:sonar.host.url=" + cfg.ServerURL(), "/d:sonar.token=" + token}
	for k, v := range props {
		if k != "sonar.projectKey" && k != "sonar.projectName" {
			begin = append(begin, "/d:"+k+"="+v)
		}
	}
	steps := [][]string{begin, {"build"}}
	if opt.Tests {
		steps = append(steps, []string{"test", "--no-build"})
	}
	steps = append(steps, []string{"sonarscanner", "end", "/d:sonar.token=" + token})
	for _, s := range steps {
		if err := local(ctx, p.Root, "dotnet", s, opt.Out); err != nil {
			return err
		}
	}
	return nil
}

func hostProps(cfg *config.Config, token string, props map[string]string) []string {
	all := map[string]string{"sonar.host.url": cfg.ServerURL(), "sonar.token": token}
	for k, v := range props {
		all[k] = v
	}
	return defineArgs("-D", all)
}

func networkProps(cfg *config.Config, token string, props map[string]string) []string {
	all := map[string]string{"sonar.host.url": cfg.ServerURLInNetwork(), "sonar.token": token}
	for k, v := range props {
		all[k] = v
	}
	return defineArgs("-D", all)
}

func wrapper(root, name string) string {
	if runtime.GOOS == "windows" {
		if p := filepath.Join(root, name+".cmd"); fileExists(p) {
			return p
		}
		if p := filepath.Join(root, name+".bat"); fileExists(p) {
			return p
		}
		return ""
	}
	if p := filepath.Join(root, name); fileExists(p) {
		return p
	}
	return ""
}

func local(ctx context.Context, dir, bin string, args []string, out io.Writer) error {
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir, cmd.Stdout, cmd.Stderr = dir, out, out
	return cmd.Run()
}

// waitProcessed waits for the background task that ingests the report.
func waitProcessed(ctx context.Context, cfg *config.Config, srv *sonar.Server, key string, out io.Writer) error {
	admin := srv.Admin()
	fmt.Fprint(out, "Waiting for SonarQube to process the report ")
	deadline := time.Now().Add(10 * time.Minute)
	for {
		n, err := admin.PendingTasks(ctx, key)
		if err == nil && n == 0 {
			break
		}
		if time.Now().After(deadline) {
			fmt.Fprintln(out)
			return errors.New("report still processing after 10m; check the web UI")
		}
		fmt.Fprint(out, ".")
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
	gate, _ := admin.QualityGate(ctx, key)
	fmt.Fprintf(out, "\nDone. Quality gate: %s\n  %s/dashboard?id=%s\n", gate, cfg.ServerURL(), key)
	return nil
}

// asUser runs a build container as the current user on Linux, so build
// output in the project and the shared caches stay owned by the user (Docker
// Desktop on macOS/Windows maps ownership itself).
func asUser() []string {
	if runtime.GOOS != "linux" {
		return nil
	}
	return []string{"--user", fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid()), "-e", "HOME=/tmp"}
}

// cacheDir returns ~/<name>, creating it as the user first: if Docker had to
// create a missing bind-mount source it would make it root-owned.
func cacheDir(name string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	d := filepath.Join(home, name)
	return d, os.MkdirAll(d, 0o755)
}

func fileExists(p string) bool { fi, err := os.Stat(p); return err == nil && !fi.IsDir() }
