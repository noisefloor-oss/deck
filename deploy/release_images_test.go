package deploy

import (
	"os"
	"os/exec"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestReleaseImages(t *testing.T) {
	command := exec.Command("python3", "release-images-test.py")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("release image tests: %v\n%s", err, output)
	}
}

func TestReleaseImagesWorkflow(t *testing.T) {
	raw, err := os.ReadFile("../.github/workflows/release-images.yaml")
	if os.IsNotExist(err) {
		t.Skip("private release workflow is absent")
	}
	if err != nil {
		t.Fatal(err)
	}
	assertActionsPinnedBySHA(t, raw)
	for _, guard := range []string{"github.event.workflow_run.name == 'Release acceptance'", "github.event.workflow_run.path == '.github/workflows/release-acceptance.yaml'", "r.get('name') == 'Release acceptance'", "r.get('path') == '.github/workflows/release-acceptance.yaml'"} {
		if !strings.Contains(string(raw), guard) {
			t.Fatalf("repair certification must not qualify main publication: missing %s", guard)
		}
	}
	for _, want := range []string{"workflows: [Release acceptance]", "github.event.repository.private", "head_repository.full_name == github.repository", "conclusion == 'success'", "packages: write", "cancel-in-progress: false", "persist-credentials: false", "No successful exact-main acceptance", "--password-stdin", "python3 deploy/release-images.py publish"} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("release workflow missing %q", want)
		}
	}
	for _, forbidden := range []string{"pull_request_target:", "ssh ", "--password \""} {
		if strings.Contains(string(raw), forbidden) {
			t.Errorf("release workflow contains %q", forbidden)
		}
	}
	var workflow struct {
		Jobs map[string]struct {
			Steps []struct {
				Uses string
				With map[string]string
			}
		}
	}
	if err := yaml.Unmarshal(raw, &workflow); err != nil {
		t.Fatal(err)
	}
	for _, job := range workflow.Jobs {
		for _, step := range job.Steps {
			if strings.HasPrefix(step.Uses, "actions/upload-artifact@") && (step.With["path"] != "${{ runner.temp }}/release-publication.json" || step.With["if-no-files-found"] != "error") {
				t.Fatal("only source-bound publication metadata may be uploaded, never image or licensed font bytes")
			}
		}
	}
	wrapper, err := os.ReadFile("production-compose.sh")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(wrapper), "deploy-cached)") || !strings.Contains(string(wrapper), "release-images.py\" select") {
		t.Fatal("canonical wrapper does not admit cached deployments")
	}
}
