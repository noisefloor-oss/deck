package deploy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestComposeForwardsDependencyRepositoryLocations(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("..", "compose*.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	checked := 0
	for _, path := range files {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var compose struct {
			Services map[string]struct {
				Build struct {
					Context    string            `yaml:"context"`
					Dockerfile string            `yaml:"dockerfile"`
					Args       map[string]string `yaml:"args"`
				} `yaml:"build"`
			} `yaml:"services"`
		}
		if err := yaml.Unmarshal(raw, &compose); err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		for name, service := range compose.Services {
			build := service.Build
			rootDockerfile := build.Dockerfile == "Dockerfile" || (build.Context == "." && build.Dockerfile == "")
			if !rootDockerfile {
				continue
			}
			checked++
			for _, tc := range []struct {
				env  map[string]string
				want map[string]string
			}{
				{nil, map[string]string{
					"NOISE_TASKS_REPO":       "https://github.com/noisefloor-oss/tasks.git",
					"NOISE_A2A_REPO":         "https://github.com/noisefloor-oss/a2a.git",
					"NOISE_SDLC_REPO":        "https://github.com/noisefloor-oss/sdlc.git",
					"NOISE_VISUAL_DIFF_REPO": "https://github.com/noisefloor-oss/visual-diff.git",
				}},
				{map[string]string{"NOISEDECK_SOURCE_ORG": "my-fork"}, map[string]string{
					"NOISE_TASKS_REPO":       "https://github.com/my-fork/tasks.git",
					"NOISE_A2A_REPO":         "https://github.com/my-fork/a2a.git",
					"NOISE_SDLC_REPO":        "https://github.com/my-fork/sdlc.git",
					"NOISE_VISUAL_DIFF_REPO": "https://github.com/my-fork/visual-diff.git",
				}},
				{map[string]string{"NOISEDECK_SOURCE_ORG": "my-fork", "NOISE_SDLC_REPO": "https://github.com/mirror/sdlc.git"}, map[string]string{
					"NOISE_TASKS_REPO":       "https://github.com/my-fork/tasks.git",
					"NOISE_A2A_REPO":         "https://github.com/my-fork/a2a.git",
					"NOISE_SDLC_REPO":        "https://github.com/mirror/sdlc.git",
					"NOISE_VISUAL_DIFF_REPO": "https://github.com/my-fork/visual-diff.git",
				}},
			} {
				for variable, want := range tc.want {
					if got := interpolate(build.Args[variable], tc.env); got != want {
						t.Errorf("%s service %s with %v renders %s as %q, want %q", path, name, tc.env, variable, got, want)
					}
				}
			}
		}
	}
	if checked == 0 {
		t.Fatal("no root Dockerfile build declarations found")
	}
}

// interpolate expands Compose ${NAME:-default} references, nested defaults
// included; an unset or empty NAME takes its default.
func interpolate(value string, env map[string]string) string {
	for {
		start := strings.LastIndex(value, "${")
		if start < 0 {
			return value
		}
		end := strings.Index(value[start:], "}") + start
		name, fallback, _ := strings.Cut(value[start+2:end], ":-")
		resolved := env[name]
		if resolved == "" {
			resolved = fallback
		}
		value = value[:start] + resolved + value[end+1:]
	}
}

func TestImageWorkflowsForwardDependencyRepositoryVariables(t *testing.T) {
	for _, name := range []string{"release-acceptance.yaml", "release-images.yaml", "public-image.yaml"} {
		raw, err := os.ReadFile(filepath.Join("..", ".github", "workflows", name))
		if name == "release-images.yaml" && os.IsNotExist(err) {
			continue // Private registry publication is omitted from the public export.
		}
		if err != nil {
			t.Fatal(err)
		}
		var workflow struct {
			Env map[string]string `yaml:"env"`
		}
		if err := yaml.Unmarshal(raw, &workflow); err != nil {
			t.Fatal(err)
		}
		for _, variable := range []string{"NOISE_TASKS_REPO", "NOISE_A2A_REPO", "NOISE_SDLC_REPO"} {
			if workflow.Env[variable] != "${{ vars."+variable+" }}" {
				t.Errorf("%s does not forward repository variable %s", name, variable)
			}
		}
	}
}

func TestHarnessBumpBuildForwardsDependencyRepositories(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", ".github", "workflows", "harness-bump.yaml"))
	if os.IsNotExist(err) {
		t.Skip("private harness workflow is absent from the public export")
	}
	if err != nil {
		t.Fatal(err)
	}
	body := string(raw)
	for _, variable := range []string{"NOISE_A2A_REPO", "NOISE_TASKS_REPO", "NOISE_SDLC_REPO", "NOISE_VISUAL_DIFF_REPO"} {
		want := "--build-arg " + variable + `="${{ vars.` + variable + " ||"
		if !strings.Contains(body, want) {
			t.Errorf("harness bump does not forward %s to the direct Docker build", variable)
		}
	}
}
