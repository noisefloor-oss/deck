package deploy

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestGoCacheBaselineTrustAndRestore(t *testing.T) {
	var workflow struct {
		On          map[string]interface{} `yaml:"on"`
		Permissions map[string]string
		Concurrency struct {
			Group  string
			Cancel bool `yaml:"cancel-in-progress"`
		}
		Jobs map[string]struct {
			If      string
			Runner  string `yaml:"runs-on"`
			Timeout int    `yaml:"timeout-minutes"`
			Steps   []struct {
				Name, Uses, Run, If string
				With                map[string]string
			}
		}
	}
	if err := yaml.Unmarshal(readWorkflow(t, "ci-go-cache.yaml"), &workflow); err != nil {
		t.Fatal(err)
	}
	if len(workflow.On) != 2 {
		t.Fatal("baseline must have only trusted push and explicit dispatch triggers")
	}
	if _, ok := workflow.On["push"]; !ok {
		t.Fatal("missing push trigger")
	}
	if _, ok := workflow.On["workflow_dispatch"]; !ok {
		t.Fatal("missing explicit dispatch")
	}
	if len(workflow.Permissions) != 1 || workflow.Permissions["contents"] != "read" {
		t.Fatal("baseline needs read-only source permission")
	}
	if workflow.Concurrency.Group != "go-cache-${{ github.ref }}" || !workflow.Concurrency.Cancel {
		t.Fatal("superseded warmers must cancel")
	}
	seed := workflow.Jobs["seed"]
	if len(workflow.Jobs) != 1 || seed.If != "github.ref == 'refs/heads/main' && (github.event_name == 'push' || github.event_name == 'workflow_dispatch')" || seed.Runner != hostedCIRunner || seed.Timeout != 15 {
		t.Fatal("baseline must run only bounded hosted main jobs")
	}
	cache, compile := false, false
	for _, step := range seed.Steps {
		if step.Name == "Restore reusable Go compilation baseline" {
			cache = step.With["key"] == "deck-go-baseline-${{ runner.arch }}-1.26.10-${{ hashFiles('go.sum') }}-${{ github.sha }}" && step.With["restore-keys"] == "deck-go-baseline-${{ runner.arch }}-1.26.10-${{ hashFiles('go.sum') }}-"
		}
		if step.Name == "Compile packages and test dependencies without executing tests" {
			compile = strings.TrimSpace(step.Run) == "go list -deps -test -export ./... > /dev/null" && step.If == "steps.baseline.outputs.cache-hit != 'true'"
		}
		if strings.Contains(step.Run, "gh workflow run") || strings.Contains(step.Run, "go test ") {
			t.Fatal("warmer must not launch validation or execute tests")
		}
	}
	if !cache || !compile {
		t.Fatal("baseline must cache matching compilation without executing tests")
	}
	var fast struct {
		Jobs map[string]struct {
			Steps []struct{ With map[string]string }
		}
	}
	if err := yaml.Unmarshal(readWorkflow(t, "ci-fast.yaml"), &fast); err != nil {
		t.Fatal(err)
	}
	count := 0
	for name, job := range fast.Jobs {
		for _, step := range job.Steps {
			if strings.HasPrefix(step.With["key"], "deck-go-") {
				lines := strings.Split(strings.TrimSpace(step.With["restore-keys"]), "\n")
				if len(lines) != 2 || !strings.Contains(lines[0], "github.job") || lines[1] != "deck-go-baseline-${{ runner.arch }}-1.26.10-${{ hashFiles('go.sum') }}-" {
					t.Errorf("%s must restore its own lane before the matching trusted baseline", name)
				}
				count++
			}
		}
	}
	if count != 7 {
		t.Fatalf("checked %d Go caches, want all seven", count)
	}
}

func TestGoCacheCompilationDoesNotExecutePackageOrTestCode(t *testing.T) {
	dir := t.TempDir()
	for name, content := range map[string]string{
		"go.mod":       "module example/cachefixture\n\ngo 1.26.0\n",
		"warm.go":      "package warm\nfunc init() { panic(\"package code executed\") }\n",
		"warm_test.go": "package warm\nimport \"testing\"\nfunc TestMain(m *testing.M) { panic(\"test code executed\") }\nfunc TestX(t *testing.T) {}\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command("go", "list", "-deps", "-test", "-export", "./...")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOWORK=off", "GOMAXPROCS=2")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("compile-only fixture ran code or failed compilation: %v\n%s", err, output)
	}
}
