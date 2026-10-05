// Package project works out a workspace's SonarQube identity (key, name) and
// build system from every place a project can declare it, so local scans and
// the MCP server use the same key as CI.
package project

import (
	"encoding/json"
	"encoding/xml"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Build systems, which decide how a project is scanned.
const (
	BuildCLI    = "cli"    // sonar-scanner CLI (JS/TS, Python, Go, PHP, shell, ...)
	BuildMaven  = "maven"  // sonar-maven-plugin (needs compiled classes)
	BuildGradle = "gradle" // sonarqube gradle plugin
	BuildDotnet = "dotnet" // dotnet-sonarscanner begin/build/end
)

// Project is a detected SonarQube project.
type Project struct {
	Root       string            // project root (where the scan runs)
	Key        string            // sonar.projectKey
	Name       string            // sonar.projectName
	KeySource  string            // human-readable origin of Key
	NameSource string            // human-readable origin of Name
	Build      string            // one of the Build* constants
	PropsFile  string            // sonar-project.properties, if present
	Props      map[string]string // extra sonar.* properties found (sources, exclusions, ...)
}

// candidate is one possible key/name with its origin; lower rank wins.
type candidate struct {
	rank      int
	key, name string
	source    string
	props     map[string]string
}

// Ranks: explicit declarations beat build-tool defaults, which beat the dir name.
const (
	rankSonarProps = iota
	rankSonarcloudProps
	rankBuildExplicit
	rankPyproject
	rankDotnetExplicit
	rankCI
	rankBuildDefault
	rankDirName
)

// strongMarkers identify a project root; the topmost one under the git root wins.
var strongMarkers = []string{"sonar-project.properties", ".sonarcloud.properties", "pom.xml",
	"settings.gradle", "settings.gradle.kts", "build.gradle", "build.gradle.kts"}

// FindRoot returns the project root for dir: the topmost dir between dir and
// its git root holding a strong marker (so a Maven/Gradle module resolves to
// its parent build), else the nearest dir with any project file, else the
// git root, else dir itself.
func FindRoot(dir string) string {
	dir, _ = filepath.Abs(dir)
	gitRoot := ""
	var chain []string
	for d := dir; ; d = filepath.Dir(d) {
		chain = append(chain, d)
		if exists(filepath.Join(d, ".git")) {
			gitRoot = d
			break
		}
		if filepath.Dir(d) == d {
			break
		}
	}
	if gitRoot == "" {
		chain = []string{dir} // outside git: don't wander up into unrelated dirs
	}
	for i := len(chain) - 1; i >= 0; i-- {
		for _, m := range strongMarkers {
			if exists(filepath.Join(chain[i], m)) {
				return chain[i]
			}
		}
	}
	weak := []string{"package.json", "pyproject.toml", "go.mod", "composer.json", "Cargo.toml"}
	for _, d := range chain {
		for _, m := range weak {
			if exists(filepath.Join(d, m)) {
				return d
			}
		}
		if len(globIn(d, "*.sln")) > 0 || len(globIn(d, "*.csproj")) > 0 {
			return d
		}
	}
	if gitRoot != "" {
		return gitRoot
	}
	return dir
}

// Detect inspects root (use FindRoot first) and returns its project identity.
func Detect(root string) *Project {
	root, _ = filepath.Abs(root)
	p := &Project{Root: root, Build: detectBuild(root), Props: map[string]string{}}

	var cands []candidate
	if c, ok := fromProperties(root, "sonar-project.properties", rankSonarProps); ok {
		cands = append(cands, c)
		p.PropsFile = filepath.Join(root, "sonar-project.properties")
	}
	if c, ok := fromProperties(root, ".sonarcloud.properties", rankSonarcloudProps); ok {
		cands = append(cands, c)
	}
	cands = append(cands, fromMaven(root)...)
	cands = append(cands, fromGradle(root)...)
	cands = append(cands, fromPyproject(root)...)
	cands = append(cands, fromDotnet(root)...)
	cands = append(cands, fromCI(root)...)
	cands = append(cands, fromPackageJSON(root)...)
	cands = append(cands, candidate{rank: rankDirName, key: sanitizeKey(filepath.Base(root)), name: filepath.Base(root), source: "directory name"})

	sort.SliceStable(cands, func(i, j int) bool { return cands[i].rank < cands[j].rank })
	for _, c := range cands {
		if p.Key == "" && c.key != "" {
			p.Key, p.KeySource = c.key, c.source
		}
		if p.Name == "" && c.name != "" {
			p.Name, p.NameSource = c.name, c.source
		}
		for k, v := range c.props {
			if _, set := p.Props[k]; !set {
				p.Props[k] = v
			}
		}
	}
	if p.Name == "" {
		p.Name, p.NameSource = p.Key, p.KeySource
	}
	return p
}

func detectBuild(root string) string {
	switch {
	case exists(filepath.Join(root, "pom.xml")):
		return BuildMaven
	case exists(filepath.Join(root, "build.gradle")), exists(filepath.Join(root, "build.gradle.kts")),
		exists(filepath.Join(root, "settings.gradle")), exists(filepath.Join(root, "settings.gradle.kts")):
		return BuildGradle
	case len(globIn(root, "*.sln")) > 0, len(globIn(root, "*.csproj")) > 0:
		return BuildDotnet
	}
	return BuildCLI
}

// --- sonar-project.properties / .sonarcloud.properties ---

func fromProperties(root, file string, rank int) (candidate, bool) {
	props, err := readProperties(filepath.Join(root, file))
	if err != nil {
		return candidate{}, false
	}
	c := candidate{rank: rank, source: file, props: map[string]string{}}
	for k, v := range props {
		switch k {
		case "sonar.projectKey":
			c.key = v
		case "sonar.projectName":
			c.name = v
		default:
			if strings.HasPrefix(k, "sonar.") {
				c.props[k] = v
			}
		}
	}
	return c, true
}

// readProperties parses a Java .properties file (key=value or key: value,
// # and ! comments, backslash line continuations).
func readProperties(path string) (map[string]string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	lines := strings.Split(strings.ReplaceAll(string(b), "\r\n", "\n"), "\n")
	for i := 0; i < len(lines); i++ {
		line := strings.TrimSpace(lines[i])
		for strings.HasSuffix(line, `\`) && i+1 < len(lines) {
			i++
			line = strings.TrimSuffix(line, `\`) + strings.TrimSpace(lines[i])
		}
		if line == "" || line[0] == '#' || line[0] == '!' {
			continue
		}
		idx := strings.IndexAny(line, "=:")
		if idx < 0 {
			continue
		}
		out[strings.TrimSpace(line[:idx])] = strings.TrimSpace(line[idx+1:])
	}
	return out, nil
}

// --- Maven ---

type pom struct {
	GroupID    string `xml:"groupId"`
	ArtifactID string `xml:"artifactId"`
	Name       string `xml:"name"`
	Parent     struct {
		GroupID string `xml:"groupId"`
	} `xml:"parent"`
	Properties struct {
		Entries []struct {
			XMLName xml.Name
			Value   string `xml:",chardata"`
		} `xml:",any"`
	} `xml:"properties"`
}

func fromMaven(root string) []candidate {
	b, err := os.ReadFile(filepath.Join(root, "pom.xml"))
	if err != nil {
		return nil
	}
	var p pom
	if xml.Unmarshal(b, &p) != nil {
		return nil
	}
	var out []candidate
	explicit := candidate{rank: rankBuildExplicit, source: "pom.xml <properties>", props: map[string]string{}}
	for _, e := range p.Properties.Entries {
		v := strings.TrimSpace(e.Value)
		switch k := e.XMLName.Local; {
		case k == "sonar.projectKey":
			explicit.key = v
		case k == "sonar.projectName":
			explicit.name = v
		case strings.HasPrefix(k, "sonar."):
			explicit.props[k] = v
		}
	}
	if strings.Contains(explicit.key, "${") {
		explicit.key = "" // property reference; fall back to the plugin default
	}
	out = append(out, explicit)
	group := p.GroupID
	if group == "" {
		group = p.Parent.GroupID
	}
	if p.ArtifactID != "" {
		key := p.ArtifactID
		if group != "" {
			key = group + ":" + p.ArtifactID // sonar-maven-plugin default
		}
		name := p.Name
		if name == "" || strings.Contains(name, "${") {
			name = p.ArtifactID
		}
		out = append(out, candidate{rank: rankBuildDefault, key: key, name: name, source: "pom.xml groupId:artifactId"})
	}
	return out
}

// --- Gradle ---

var (
	// property("sonar.projectKey", "x") / property "sonar.projectKey", "x" / properties["sonar.projectKey"] = "x"
	gradleProp = regexp.MustCompile(`(?m)property\s*\(?\s*["'](sonar\.[A-Za-z.]+)["']\s*,\s*["']([^"'$]+)["']`)
	gradleMap  = regexp.MustCompile(`(?m)properties\s*\[\s*["'](sonar\.[A-Za-z.]+)["']\s*\]\s*=\s*["']([^"'$]+)["']`)
	gradleRoot = regexp.MustCompile(`(?m)^\s*rootProject\.name\s*=\s*["']([^"']+)["']`)
	gradleGrp  = regexp.MustCompile(`(?m)^\s*group\s*=?\s*["']([^"']+)["']`)
)

func fromGradle(root string) []candidate {
	var builds []string
	for _, f := range []string{"build.gradle", "build.gradle.kts"} {
		if b, err := os.ReadFile(filepath.Join(root, f)); err == nil {
			builds = append(builds, string(b))
		}
	}
	gp, _ := readProperties(filepath.Join(root, "gradle.properties"))
	if len(builds) == 0 && gp == nil && !exists(filepath.Join(root, "settings.gradle")) && !exists(filepath.Join(root, "settings.gradle.kts")) {
		return nil
	}
	explicit := candidate{rank: rankBuildExplicit, source: "gradle sonar { properties }", props: map[string]string{}}
	for _, src := range builds {
		for _, re := range []*regexp.Regexp{gradleProp, gradleMap} {
			for _, m := range re.FindAllStringSubmatch(src, -1) {
				switch m[1] {
				case "sonar.projectKey":
					explicit.key = m[2]
				case "sonar.projectName":
					explicit.name = m[2]
				default:
					explicit.props[m[1]] = m[2]
				}
			}
		}
	}
	for k, v := range gp { // systemProp.sonar.x=… or sonar.x=… in gradle.properties
		k = strings.TrimPrefix(k, "systemProp.")
		switch k {
		case "sonar.projectKey":
			if explicit.key == "" {
				explicit.key, explicit.source = v, "gradle.properties"
			}
		case "sonar.projectName":
			if explicit.name == "" {
				explicit.name = v
			}
		}
	}
	out := []candidate{explicit}

	name := ""
	for _, f := range []string{"settings.gradle", "settings.gradle.kts"} {
		if b, err := os.ReadFile(filepath.Join(root, f)); err == nil {
			if m := gradleRoot.FindStringSubmatch(string(b)); m != nil {
				name = m[1]
			}
		}
	}
	if name == "" {
		name = filepath.Base(root)
	}
	group := gp["group"]
	for _, src := range builds {
		if m := gradleGrp.FindStringSubmatch(src); m != nil {
			group = m[1]
		}
	}
	key := name
	if group != "" {
		key = group + ":" + name // gradle plugin default
	}
	out = append(out, candidate{rank: rankBuildDefault, key: key, name: name, source: "gradle group:rootProject.name"})
	return out
}

// --- pyproject.toml [tool.sonar] ---

func fromPyproject(root string) []candidate {
	b, err := os.ReadFile(filepath.Join(root, "pyproject.toml"))
	if err != nil {
		return nil
	}
	c := candidate{rank: rankPyproject, source: "pyproject.toml [tool.sonar]", props: map[string]string{}}
	in := false
	for _, line := range strings.Split(string(b), "\n") {
		s := strings.TrimSpace(line)
		if strings.HasPrefix(s, "[") {
			in = s == "[tool.sonar]"
			continue
		}
		if !in || s == "" || s[0] == '#' {
			continue
		}
		k, v, ok := strings.Cut(s, "=")
		if !ok {
			continue
		}
		k = strings.Trim(strings.TrimSpace(k), `"'`)
		v = strings.Trim(strings.TrimSpace(v), `"'`)
		k = strings.TrimPrefix(k, "sonar.")
		switch k {
		case "projectKey":
			c.key = v
		case "projectName":
			c.name = v
		default:
			c.props["sonar."+k] = v
		}
	}
	if c.key == "" && c.name == "" && len(c.props) == 0 {
		return nil
	}
	return []candidate{c}
}

// --- .NET ---

var (
	msbuildSetting = regexp.MustCompile(`<SonarQubeSetting\s+Include="(sonar\.[A-Za-z.]+)"\s*>\s*<Value>([^<$]+)</Value>`)
	analysisProp   = regexp.MustCompile(`<Property\s+Name="(sonar\.[A-Za-z.]+)"\s*>([^<$]+)</Property>`)
)

func fromDotnet(root string) []candidate {
	var out []candidate
	explicit := candidate{rank: rankDotnetExplicit, props: map[string]string{}}
	for _, f := range []string{"SonarQube.Analysis.xml", "Directory.Build.props"} {
		b, err := os.ReadFile(filepath.Join(root, f))
		if err != nil {
			continue
		}
		for _, re := range []*regexp.Regexp{analysisProp, msbuildSetting} {
			for _, m := range re.FindAllStringSubmatch(string(b), -1) {
				switch m[1] {
				case "sonar.projectKey":
					explicit.key, explicit.source = strings.TrimSpace(m[2]), f
				case "sonar.projectName":
					explicit.name = strings.TrimSpace(m[2])
				}
			}
		}
	}
	if explicit.key != "" || explicit.name != "" {
		if explicit.source == "" {
			explicit.source = "Directory.Build.props"
		}
		out = append(out, explicit)
	}
	if sln := globIn(root, "*.sln"); len(sln) > 0 {
		n := strings.TrimSuffix(filepath.Base(sln[0]), ".sln")
		out = append(out, candidate{rank: rankBuildDefault, key: sanitizeKey(n), name: n, source: filepath.Base(sln[0])})
	}
	return out
}

// --- CI pipelines ---

var ciKeyPatterns = []*regexp.Regexp{
	regexp.MustCompile(`-Dsonar\.projectKey=["']?([A-Za-z0-9_.:\-]+)`),
	regexp.MustCompile(`(?m)\bsonar\.projectKey\s*[=:]\s*["']?([A-Za-z0-9_.:\-]+)`),
	regexp.MustCompile(`(?m)^\s*(?:SONAR_PROJECT_KEY|SONAR_PROJECTKEY|projectKey)\s*:\s*["']?([A-Za-z0-9_.:\-]+)["']?\s*$`),
	regexp.MustCompile(`/k:["']?([A-Za-z0-9_.:\-]+)`), // dotnet-sonarscanner begin /k:KEY
}

var ciFiles = []string{".gitlab-ci.yml", "Jenkinsfile", "azure-pipelines.yml", "bitbucket-pipelines.yml",
	".circleci/config.yml", ".github/workflows/*.yml", ".github/workflows/*.yaml"}

func fromCI(root string) []candidate {
	for _, pattern := range ciFiles {
		matches, _ := filepath.Glob(filepath.Join(root, pattern))
		sort.Strings(matches)
		for _, f := range matches {
			b, err := os.ReadFile(f)
			if err != nil {
				continue
			}
			for _, re := range ciKeyPatterns {
				if m := re.FindStringSubmatch(string(b)); m != nil {
					rel, _ := filepath.Rel(root, f)
					return []candidate{{rank: rankCI, key: m[1], source: filepath.ToSlash(rel) + " (CI)"}}
				}
			}
		}
	}
	return nil
}

// --- package.json ---

func fromPackageJSON(root string) []candidate {
	b, err := os.ReadFile(filepath.Join(root, "package.json"))
	if err != nil {
		return nil
	}
	var pkg struct {
		Name  string            `json:"name"`
		Sonar map[string]string `json:"sonar"` // not a standard field, but honored if present
	}
	if json.Unmarshal(b, &pkg) != nil {
		return nil
	}
	var out []candidate
	if k := pkg.Sonar["projectKey"]; k != "" {
		out = append(out, candidate{rank: rankBuildExplicit, key: k, name: pkg.Sonar["projectName"], source: `package.json "sonar"`})
	}
	if pkg.Name != "" {
		out = append(out, candidate{rank: rankBuildDefault, key: sanitizeKey(pkg.Name), name: pkg.Name, source: "package.json name"})
	}
	return out
}

// sanitizeKey maps a name to SonarQube's allowed key charset
// ([A-Za-z0-9_.:-], at least one non-digit): "@scope/pkg" -> "scope:pkg".
func sanitizeKey(s string) string {
	s = strings.TrimPrefix(s, "@")
	s = strings.ReplaceAll(s, "/", ":")
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', strings.ContainsRune("_.:-", r):
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	return b.String()
}

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

func globIn(dir, pattern string) []string {
	m, _ := filepath.Glob(filepath.Join(dir, pattern))
	sort.Strings(m)
	return m
}
