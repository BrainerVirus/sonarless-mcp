package project

import (
	"os"
	"path/filepath"
	"testing"
)

func write(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for name, body := range files {
		p := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestDetect(t *testing.T) {
	cases := []struct {
		name     string
		files    map[string]string
		key, src string
		projName string
		build    string
	}{
		{
			name: "sonar-project.properties wins over everything",
			files: map[string]string{
				"sonar-project.properties": "# c\nsonar.projectKey=acme_web\nsonar.projectName = Acme Web\nsonar.sources=src\n",
				"pom.xml":                  `<project><groupId>g</groupId><artifactId>a</artifactId></project>`,
				".github/workflows/ci.yml": "run: mvn sonar:sonar -Dsonar.projectKey=from-ci\n",
			},
			key: "acme_web", src: "sonar-project.properties", projName: "Acme Web", build: BuildMaven,
		},
		{
			name: "maven explicit property",
			files: map[string]string{"pom.xml": `<project><groupId>com.acme</groupId><artifactId>svc</artifactId>
				<properties><java.version>21</java.version><sonar.projectKey>acme-svc</sonar.projectKey></properties></project>`},
			key: "acme-svc", src: "pom.xml <properties>", projName: "svc", build: BuildMaven,
		},
		{
			name:  "maven default groupId:artifactId, groupId from parent",
			files: map[string]string{"pom.xml": `<project><parent><groupId>com.acme</groupId></parent><artifactId>svc</artifactId><name>Service</name></project>`},
			key:   "com.acme:svc", src: "pom.xml groupId:artifactId", projName: "Service", build: BuildMaven,
		},
		{
			name: "maven property reference falls back to default",
			files: map[string]string{"pom.xml": `<project><groupId>g</groupId><artifactId>a</artifactId>
				<properties><sonar.projectKey>${project.artifactId}</sonar.projectKey></properties></project>`},
			key: "g:a", src: "pom.xml groupId:artifactId", projName: "a", build: BuildMaven,
		},
		{
			name: "CI key beats build defaults",
			files: map[string]string{
				"pom.xml":        `<project><groupId>g</groupId><artifactId>a</artifactId></project>`,
				".gitlab-ci.yml": "sonar:\n  script: mvn verify sonar:sonar -Dsonar.projectKey=team_a -Dsonar.host.url=$SONAR_HOST\n",
			},
			key: "team_a", src: ".gitlab-ci.yml (CI)", projName: "a", build: BuildMaven,
		},
		{
			name: "gradle kotlin dsl explicit",
			files: map[string]string{
				"settings.gradle.kts": `rootProject.name = "shop"`,
				"build.gradle.kts":    "group = \"com.acme\"\nsonar {\n  properties {\n    property(\"sonar.projectKey\", \"acme_shop\")\n  }\n}\n",
			},
			key: "acme_shop", src: "gradle sonar { properties }", projName: "shop", build: BuildGradle,
		},
		{
			name: "gradle groovy default group:name",
			files: map[string]string{
				"settings.gradle": "rootProject.name = 'shop'\n",
				"build.gradle":    "plugins { id 'java' }\ngroup 'com.acme'\n",
			},
			key: "com.acme:shop", src: "gradle group:rootProject.name", projName: "shop", build: BuildGradle,
		},
		{
			name:  "gradle.properties systemProp",
			files: map[string]string{"build.gradle": "", "gradle.properties": "systemProp.sonar.projectKey=gp_key\n"},
			key:   "gp_key", src: "gradle.properties", build: BuildGradle,
		},
		{
			name:  "pyproject tool.sonar",
			files: map[string]string{"pyproject.toml": "[project]\nname = \"x\"\n\n[tool.sonar]\nprojectKey = \"py_key\"\nprojectName = \"Py\"\nsources = \"src\"\n"},
			key:   "py_key", src: "pyproject.toml [tool.sonar]", projName: "Py", build: BuildCLI,
		},
		{
			name: "dotnet analysis xml beats sln name",
			files: map[string]string{
				"App.sln":                "",
				"SonarQube.Analysis.xml": `<SonarQubeAnalysisProperties><Property Name="sonar.projectKey">net_key</Property></SonarQubeAnalysisProperties>`,
			},
			key: "net_key", src: "SonarQube.Analysis.xml", projName: "App", build: BuildDotnet,
		},
		{
			name:  "dotnet CI /k:",
			files: map[string]string{"App.sln": "", "azure-pipelines.yml": "- script: dotnet sonarscanner begin /k:\"az_key\" /d:sonar.host.url=x\n"},
			key:   "az_key", src: "azure-pipelines.yml (CI)", projName: "App", build: BuildDotnet,
		},
		{
			name:  "package.json scoped name sanitized",
			files: map[string]string{"package.json": `{"name":"@acme/web-app"}`},
			key:   "acme:web-app", src: "package.json name", projName: "@acme/web-app", build: BuildCLI,
		},
		{
			name:  "github action with: projectKey",
			files: map[string]string{"package.json": `{"name":"x"}`, ".github/workflows/q.yaml": "      - uses: sonarsource/sonarqube-scan-action@v5\n        with:\n          args: >\n            -Dsonar.projectKey=gh_key\n"},
			key:   "gh_key", src: ".github/workflows/q.yaml (CI)", projName: "x", build: BuildCLI,
		},
		{
			name:  "ci variables are not keys",
			files: map[string]string{".gitlab-ci.yml": "script: sonar-scanner -Dsonar.projectKey=$CI_PROJECT_NAME\n"},
			build: BuildCLI, src: "directory name",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			write(t, root, tc.files)
			p := Detect(root)
			if tc.key == "" {
				tc.key = filepath.Base(root)
			}
			if p.Key != tc.key || p.KeySource != tc.src {
				t.Errorf("key = %q from %q, want %q from %q", p.Key, p.KeySource, tc.key, tc.src)
			}
			if tc.projName != "" && p.Name != tc.projName {
				t.Errorf("name = %q, want %q", p.Name, tc.projName)
			}
			if p.Build != tc.build {
				t.Errorf("build = %q, want %q", p.Build, tc.build)
			}
		})
	}
}

func TestPropsCollected(t *testing.T) {
	root := t.TempDir()
	write(t, root, map[string]string{"sonar-project.properties": "sonar.projectKey=k\nsonar.exclusions=**/gen/**,\\\n  **/vendor/**\n"})
	p := Detect(root)
	if got := p.Props["sonar.exclusions"]; got != "**/gen/**,**/vendor/**" {
		t.Errorf("continuation not joined: %q", got)
	}
	if p.PropsFile == "" {
		t.Error("PropsFile not set")
	}
}

func TestFindRoot(t *testing.T) {
	repo := t.TempDir()
	write(t, repo, map[string]string{
		".git/HEAD":           "ref: refs/heads/main\n",
		"pom.xml":             "<project/>",
		"module-a/pom.xml":    "<project/>",
		"module-a/src/A.java": "",
		"web/package.json":    `{"name":"web"}`,
	})
	if got := FindRoot(filepath.Join(repo, "module-a", "src")); got != repo {
		t.Errorf("maven module: got %s, want parent build %s", got, repo)
	}

	mono := t.TempDir()
	write(t, mono, map[string]string{".git/HEAD": "", "apps/web/package.json": `{"name":"web"}`, "apps/web/src/x.ts": ""})
	want := filepath.Join(mono, "apps", "web")
	if got := FindRoot(filepath.Join(want, "src")); got != want {
		t.Errorf("monorepo package: got %s, want %s", got, want)
	}

	plain := t.TempDir()
	if got := FindRoot(plain); got != plain {
		t.Errorf("no markers: got %s, want %s", got, plain)
	}
}

func TestSanitizeKey(t *testing.T) {
	for in, want := range map[string]string{"@a/b": "a:b", "my app!": "my_app_", "ok.key-1:x": "ok.key-1:x"} {
		if got := sanitizeKey(in); got != want {
			t.Errorf("sanitizeKey(%q) = %q, want %q", in, got, want)
		}
	}
}
