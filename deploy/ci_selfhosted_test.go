package deploy

import (
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const privateCIRunner = `[self-hosted Linux ARM64]`
const privateCIAdmission = "github.event_name != 'pull_request' || github.event.pull_request.head.repo.full_name == github.repository"
const privateCIGate = "always()"

func TestPrivateCIRouting(t *testing.T) {
	var workflow struct {
		Jobs map[string]struct {
			RunsOn []string `yaml:"runs-on"`
			If     string
			Steps  []struct {
				If   string
				Uses string
				With map[string]string
			}
		}
	}
	raw := readWorkflow(t, "ci-fast.yaml")
	if err := yaml.Unmarshal(raw, &workflow); err != nil {
		t.Fatal(err)
	}
	for name, job := range workflow.Jobs {
		want := privateCIRunner
		if !slices.Equal(job.RunsOn, []string{"self-hosted", "Linux", "ARM64"}) {
			t.Errorf("%s must use %s", name, want)
		}
		for _, step := range job.Steps {
			if !strings.HasPrefix(step.Uses, "actions/cache@") {
				continue
			}
			if strings.Contains(step.With["path"], "~/") && !strings.HasPrefix(step.If, "runner.environment != 'self-hosted'") {
				t.Errorf("%s must not unpack a hosted cache over persistent home files", name)
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
		"deck-go-${{ runner.arch }}-1.26.10-",
		"deck-browser-go-${{ runner.arch }}-1.26.10-",
		"playwright-chromium-${{ runner.arch }}-",
		"-go-1.26.10-${{ runner.arch }}",
		"-linux-${{ runner.arch }}",
	} {
		if !strings.Contains(string(raw), expected) {
			t.Errorf("missing architecture-isolated cache key: %s", expected)
		}
	}
}
