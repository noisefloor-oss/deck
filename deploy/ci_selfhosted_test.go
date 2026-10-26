package deploy

import (
	"fmt"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const privateCIRunner = `${{ github.repository == 'noisefloor-oss/deck' && github.event.repository.visibility == 'public' && 'ubuntu-24.04-arm' || fromJSON('["self-hosted","Linux","ARM64"]') }}`
const privateCIAdmission = "github.event_name != 'pull_request' || github.event.pull_request.head.repo.full_name == github.repository"
const privateCIGate = "always()"

func nativeLinuxJob(name string) bool {
	switch name {
	case "scope", "documentation", "lint", "fast-checks":
		return true
	}
	return false
}

func TestPrivateCIRouting(t *testing.T) {
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
		want := privateCIRunner
		if nativeLinuxJob(name) {
			want = strings.Replace(privateCIRunner, `"ARM64"`, `"native-linux"`, 1)
			for _, step := range job.Steps {
				if strings.Contains(step.Run, "apt-get") || strings.Contains(step.Run, "linux-arm64") || strings.Contains(step.Run, "linux-amd64") {
					t.Errorf("%s requires a platform-specific lane", name)
				}
			}
		}
		if fmt.Sprint(job.RunsOn) != want {
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
