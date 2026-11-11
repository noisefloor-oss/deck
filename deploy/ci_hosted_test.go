package deploy

import (
	"fmt"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const privateCIRunner = `${{ github.repository == 'noisefloor-oss/deck' && github.event.repository.visibility == 'public' && 'ubuntu-24.04-arm' || fromJSON('["self-hosted","Linux","ARM64"]') }}`
const hostedCIRunner = "ubuntu-24.04-arm"

const privateCIAdmission = "github.event_name != 'pull_request' || github.event.pull_request.head.repo.full_name == github.repository"
const privateCIGate = "always()"

func TestHostedCIRouting(t *testing.T) {
	var workflow struct {
		Jobs map[string]struct {
			RunsOn any `yaml:"runs-on"`
			If     string
			Steps  []struct {
				If   string
				Uses string
				Run  string
				With map[string]string
			}
		}
	}
	raw := readWorkflow(t, "ci-fast.yaml")
	if err := yaml.Unmarshal(raw, &workflow); err != nil {
		t.Fatal(err)
	}
	for name, job := range workflow.Jobs {
		want := hostedCIRunner
		if fmt.Sprint(job.RunsOn) != want {
			t.Errorf("%s must use %s", name, want)
		}
		for _, step := range job.Steps {
			for _, install := range []string{"apt-get", "--with-deps"} {
				fetch := strings.Index(step.Run, install)
				setup := strings.Index(step.Run, "ubuntu-mirror.sh")
				if fetch >= 0 && (setup < 0 || setup > fetch) {
					t.Errorf("%s fetches Ubuntu dependencies before mirror setup", name)
				}
			}
			if !strings.HasPrefix(step.Uses, "actions/cache@") {
				continue
			}
			if strings.Contains(step.With["path"], "~/") && !strings.HasPrefix(step.If, "runner.environment != 'self-hosted'") {
				t.Errorf("%s must not unpack a hosted cache over persistent home files", name)
			}
			if strings.Contains(step.With["path"], "~/.cache/go-build") && !strings.Contains(step.With["key"], "deck-browser-go-") {
				for _, part := range []string{"github.job", "hashFiles('go.sum')", "github.sha"} {
					if !strings.Contains(step.With["key"], part) {
						t.Errorf("%s Go cache key lacks %s", name, part)
					}
				}
				if !strings.Contains(step.With["restore-keys"], "github.job") || strings.Contains(step.With["restore-keys"], "github.sha") {
					t.Errorf("%s must restore previous revisions of its own Go cache", name)
				}
				if name == "source-export" && (!strings.Contains(step.With["key"], "matrix.suite") || !strings.Contains(step.With["restore-keys"], "matrix.suite")) {
					t.Fatal("export test lanes must not compete for one immutable cache")
				}
			}
			for _, field := range []string{"key", "restore-keys"} {
				for _, key := range strings.Split(strings.TrimSpace(step.With[field]), "\n") {
					if key != "" && (!strings.Contains(key, "runner.arch") || strings.Contains(key, "amd64")) {
						t.Errorf("%s %s cache is not architecture-isolated: %s", name, field, key)
					}
				}
			}
		}
	}
	if workflow.Jobs["scope"].If != privateCIAdmission || workflow.Jobs["fast-checks"].If != privateCIGate {
		t.Fatal("private fork code must not enter the scope; the aggregate must always run and fail when scope is skipped")
	}
	for _, expected := range []string{
		"deck-go-${{ runner.arch }}-1.26.11-",
		"deck-browser-go-${{ runner.arch }}-1.26.11-",
		"playwright-chromium-${{ runner.arch }}-",
		"-go-1.26.11-${{ runner.arch }}",
		"-linux-${{ runner.arch }}",
	} {
		if !strings.Contains(string(raw), expected) {
			t.Errorf("missing architecture-isolated cache key: %s", expected)
		}
	}
}

func TestTimingCollectorUsesHostedRunner(t *testing.T) {
	var workflow struct {
		Jobs map[string]struct {
			RunsOn string `yaml:"runs-on"`
		}
	}
	if err := yaml.Unmarshal(readWorkflow(t, "ci-timings.yaml"), &workflow); err != nil {
		t.Fatal(err)
	}
	if workflow.Jobs["summarize"].RunsOn != hostedCIRunner {
		t.Fatal("timing reports must not wait on the local validation pool")
	}
}

// Every source-executing job must set its CPU budget before installing or running Go.
func TestFastChecksBoundCPUCapacity(t *testing.T) {
	var workflow struct {
		Jobs map[string]struct {
			Steps []struct {
				Name string
				Run  string
				Uses string
			}
		}
	}
	if err := yaml.Unmarshal(readWorkflow(t, "ci-fast.yaml"), &workflow); err != nil {
		t.Fatal(err)
	}
	for name, job := range workflow.Jobs {
		bounded := false
		for _, step := range job.Steps {
			if step.Name == "Bound CPU concurrency" {
				bounded = strings.Contains(step.Run, "bash .github/scripts/ci_capacity.sh") && strings.Contains(step.Run, `echo "GOMAXPROCS=${capacity}" >> "$GITHUB_ENV"`)
			}
			if strings.Contains(step.Uses, "actions/setup-go@") && !bounded {
				t.Errorf("%s installs Go without a CPU budget", name)
			}
			if strings.Contains(step.Run, "-p 4") || strings.Contains(step.Run, "GOMAXPROCS=4") || strings.Contains(step.Run, "--jobs 4") {
				t.Errorf("%s forces four-way concurrency", name)
			}
		}
	}
}
