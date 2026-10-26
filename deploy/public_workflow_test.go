package deploy

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

type publicWorkflow struct {
	Permissions map[string]string `yaml:"permissions"`
	Env         map[string]string `yaml:"env"`
	Events      map[string]any    `yaml:"on"`
	Jobs        map[string]struct {
		If          string            `yaml:"if"`
		Permissions map[string]string `yaml:"permissions"`
		Env         map[string]string `yaml:"env"`
		Steps       []struct {
			Name string            `yaml:"name"`
			Uses string            `yaml:"uses"`
			Run  string            `yaml:"run"`
			With map[string]any    `yaml:"with"`
			Env  map[string]string `yaml:"env"`
		} `yaml:"steps"`
	} `yaml:"jobs"`
}

func readPublicWorkflow(t *testing.T, name string) publicWorkflow {
	t.Helper()
	raw := readWorkflow(t, name)
	assertActionsPinnedBySHA(t, raw)
	var workflow publicWorkflow
	if err := yaml.Unmarshal(raw, &workflow); err != nil {
		t.Fatal(err)
	}
	return workflow
}

func TestPublicWorkflowRepositoryAdmission(t *testing.T) {
	const policy = "github.event.repository.visibility == 'public' && github.event.repository.fork == false && (github.repository == 'noisefloor-oss/deck' || vars.NOISEDECK_PUBLIC_RELEASE_REPOSITORY == github.repository)"
	for _, file := range []string{"public-image.yaml", "release.yml"} {
		workflow := readPublicWorkflow(t, file)
		for event := range workflow.Events {
			if event != "push" && event != "workflow_dispatch" {
				t.Errorf("%s must not admit untrusted event %s", file, event)
			}
		}
		for name, job := range workflow.Jobs {
			condition := strings.Join(strings.Fields(job.If), " ")
			if file == "release.yml" {
				prefix := "github.event_name == 'push' && startsWith(github.ref, 'refs/tags/v') && "
				if name == "build" {
					prefix = "!cancelled() && ((github.event_name == 'workflow_dispatch' && github.repository == 'noisefloor-oss/deck') || needs.release-create.result == 'success') && "
				}
				if !strings.HasPrefix(condition, prefix) {
					t.Fatalf("%s must retain its event/dependency guard", name)
				}
				condition = strings.TrimPrefix(condition, prefix)
			}
			if condition != policy {
				t.Fatalf("%s/%s admission = %q, want public non-fork repository-bound opt-in", file, name, condition)
			}
			for _, scenario := range []struct {
				name, repository, visibility, optIn string
				fork, want                          bool
			}{
				{"canonical-default", "noisefloor-oss/deck", "public", "", false, true},
				{"canonical-unrelated-variable", "noisefloor-oss/deck", "public", "other/deck", false, true},
				{"canary-opt-in", "canary/deck", "public", "canary/deck", false, true},
				{"case-insensitive-exact-slug", "Canary/Deck", "public", "canary/deck", false, true},
				{"canary-default-off", "canary/deck", "public", "", false, false},
				{"boolean-is-not-a-slug", "canary/deck", "public", "true", false, false},
				{"other-repository-opt-in", "canary/deck", "public", "other/deck", false, false},
				{"private-canonical", "noisefloor-oss/deck", "private", "", false, false},
				{"private-canary-opt-in", "canary/deck", "private", "canary/deck", false, false},
				{"internal-canary-opt-in", "canary/deck", "internal", "canary/deck", false, false},
				{"missing-visibility", "canary/deck", "", "canary/deck", false, false},
				{"fork-canonical", "noisefloor-oss/deck", "public", "", true, false},
				{"fork-canary-opt-in", "canary/deck", "public", "canary/deck", true, false},
			} {
				t.Run(file+"/"+name+"/"+scenario.name, func(t *testing.T) {
					// Evaluate the actual parsed scalar, not a second copy of the
					// admission formula. These expressions share Bash's boolean
					// operators; lowercase string operands match Actions equality.
					quote := func(v string) string { return strconv.Quote(strings.ToLower(v)) }
					condition := strings.NewReplacer(
						"github.event.repository.visibility", quote(scenario.visibility),
						"github.event.repository.fork", strconv.FormatBool(scenario.fork),
						"vars.NOISEDECK_PUBLIC_RELEASE_REPOSITORY", quote(scenario.optIn),
						"github.repository", quote(scenario.repository),
					).Replace(condition)
					cmd := exec.Command("bash", "-c", "[[ "+condition+" ]]")
					cmd.Env = []string{"PATH=" + os.Getenv("PATH")}
					out, err := cmd.CombinedOutput()
					if (err == nil) != scenario.want {
						t.Fatalf("admission = %v, want %v: %v %s", err == nil, scenario.want, err, out)
					}
				})
			}
		}
	}
}

func TestPublicWorkflowTokenIsolation(t *testing.T) {
	writeScripts := map[string]string{
		"Ensure the release for the tag exists": `gh release view "$GITHUB_REF_NAME" >/dev/null 2>&1 && exit 0
gh release create "$GITHUB_REF_NAME" --verify-tag \
  --title "$GITHUB_REF_NAME" \
  --notes "noise-deck $GITHUB_REF_NAME — binaries and SHA256SUMS attached by the release workflow."`,
		"Attach binaries and checksums to the release": `sha256sum noise-deck-linux-* > SHA256SUMS
cat SHA256SUMS
gh release upload "$GITHUB_REF_NAME" \
  noise-deck-linux-* SHA256SUMS --clobber`,
	}
	for _, file := range []string{"public-image.yaml", "release.yml"} {
		workflow := readPublicWorkflow(t, file)
		if len(workflow.Permissions) != 1 || workflow.Permissions["contents"] != "read" {
			t.Fatalf("%s default token permissions must be read-only", file)
		}
		for key := range workflow.Env {
			if strings.Contains(key, "TOKEN") {
				t.Fatalf("%s must not set workflow-scoped tokens", file)
			}
		}
		for name, job := range workflow.Jobs {
			writer := file == "release.yml" && (name == "release-create" || name == "asset-publish")
			if writer {
				if len(job.Permissions) != 1 || job.Permissions["contents"] != "write" {
					t.Fatalf("%s publication requires only contents:write", name)
				}
			} else if len(job.Permissions) != 0 {
				t.Fatalf("repository-code job %s must inherit read-only permissions", name)
			}
			for key := range job.Env {
				if strings.Contains(key, "TOKEN") {
					t.Fatalf("%s token %s must not be job-scoped", name, key)
				}
			}
			for _, step := range job.Steps {
				if strings.HasPrefix(step.Uses, "actions/checkout@") {
					if writer || step.With["persist-credentials"] != false {
						t.Fatalf("%s must not check out repository code with writing authority or retained credentials", name)
					}
				}
				if writer {
					if step.Uses != "" && !strings.HasPrefix(step.Uses, "actions/download-artifact@") {
						t.Fatalf("writer %s may only use the pinned artifact download action", name)
					}
					if step.Run != "" {
						if want, ok := writeScripts[step.Name]; !ok || strings.TrimSpace(step.Run) != want {
							t.Fatalf("write-token job %s executes more than the fixed release/artifact commands", name)
						}
						if step.Env["GH_TOKEN"] != "${{ github.token }}" || step.Env["GH_REPO"] != "${{ github.repository }}" {
							t.Fatalf("%s must scope publication authority to its own repository", name)
						}
					}
				} else {
					for key := range step.Env {
						if strings.Contains(key, "TOKEN") {
							t.Fatalf("repository-code step %s must not receive publication tokens", step.Name)
						}
					}
				}
			}
		}
	}
}

func TestPublicReleaseCandidateVersion(t *testing.T) {
	workflow := readPublicWorkflow(t, "release.yml")
	var script string
	for _, step := range workflow.Jobs["build"].Steps {
		if step.Name == "Select the build version" {
			script = step.Run
		}
	}
	if script == "" {
		t.Fatal("missing version selection")
	}
	for _, tc := range []struct {
		event, ref, want string
		valid            bool
	}{
		{"workflow_dispatch", "feature/qualification", "candidate-0123456789012345678901234567890123456789", true},
		{"push", "v0.4.0", "0.4.0", true},
		{"push", "v0.4.0-rc.1+build", "0.4.0-rc.1+build", true},
		{"push", "main", "", false},
		{"push", "v0.4.0;touch injected", "", false},
	} {
		t.Run(tc.event+"/"+tc.ref, func(t *testing.T) {
			dir := t.TempDir()
			output := filepath.Join(dir, "output")
			cmd := exec.Command("bash", "-eu", "-c", script)
			cmd.Dir = dir
			cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "GITHUB_EVENT_NAME=" + tc.event, "GITHUB_REF_NAME=" + tc.ref, "GITHUB_OUTPUT=" + output, "GITHUB_SHA=0123456789012345678901234567890123456789"}
			data, err := cmd.CombinedOutput()
			if (err == nil) != tc.valid {
				t.Fatalf("version selection: %v %s", err, data)
			}
			if tc.valid {
				got, err := os.ReadFile(output)
				if err != nil || string(got) != "value="+tc.want+"\n" {
					t.Fatalf("version output = %q, %v", got, err)
				}
			}
			if _, err := os.Stat(filepath.Join(dir, "injected")); !os.IsNotExist(err) {
				t.Fatal("ref executed as shell code")
			}
		})
	}
}

func TestPublicReleaseEventGuards(t *testing.T) {
	workflow := readPublicWorkflow(t, "release.yml")
	for _, tc := range []struct {
		name, event, ref, result, repo, visibility, optIn string
		cancelled, build, write                           bool
	}{
		{"branch-dispatch", "workflow_dispatch", "refs/heads/candidate", "skipped", "noisefloor-oss/deck", "public", "", false, true, false},
		{"tag-dispatch", "workflow_dispatch", "refs/tags/v0.4.0", "skipped", "noisefloor-oss/deck", "public", "", false, true, false},
		{"tag-push", "push", "refs/tags/v0.4.0", "success", "noisefloor-oss/deck", "public", "", false, true, true},
		{"failed-release", "push", "refs/tags/v0.4.0", "failure", "noisefloor-oss/deck", "public", "", false, false, true},
		{"skipped-release", "push", "refs/tags/v0.4.0", "skipped", "noisefloor-oss/deck", "public", "", false, false, true},
		{"cancelled-dispatch", "workflow_dispatch", "refs/heads/candidate", "skipped", "noisefloor-oss/deck", "public", "", true, false, false},
		{"mirror-dispatch", "workflow_dispatch", "refs/heads/candidate", "skipped", "mirror/deck", "public", "mirror/deck", false, false, false},
		{"mirror-tag", "push", "refs/tags/v0.4.0", "success", "mirror/deck", "public", "mirror/deck", false, true, true},
		{"private-dispatch", "workflow_dispatch", "refs/heads/candidate", "skipped", "noisefloor-oss/deck", "private", "", false, false, false},
	} {
		for name, job := range workflow.Jobs {
			t.Run(tc.name+"/"+name, func(t *testing.T) {
				want := tc.write
				if name == "build" {
					want = tc.build
				}
				// The publish job's implicit successful-needs gate is separate
				// from this explicit admission expression.
				quote := strconv.Quote
				expression := strings.NewReplacer(
					"!cancelled()", strconv.FormatBool(tc.cancelled)+" == false",
					"startsWith(github.ref, 'refs/tags/v')", strconv.FormatBool(strings.HasPrefix(tc.ref, "refs/tags/v"))+" == true",
					"github.event_name", quote(tc.event),
					"github.event.repository.visibility", quote(tc.visibility),
					"github.event.repository.fork", "false",
					"needs.release-create.result", quote(tc.result),
					"vars.NOISEDECK_PUBLIC_RELEASE_REPOSITORY", quote(tc.optIn),
					"github.repository", quote(tc.repo),
				).Replace(strings.Join(strings.Fields(job.If), " "))
				cmd := exec.Command("bash", "-c", "[[ "+expression+" ]]")
				data, err := cmd.CombinedOutput()
				if (err == nil) != want {
					t.Fatalf("guard = %v, want %v: %v %s", err == nil, want, err, data)
				}
			})
		}
	}
}

func TestPublicRunnerRouting(t *testing.T) {
	for _, file := range []string{"ci-fast.yaml", "release-acceptance.yaml", "public-image.yaml", "release.yml"} {
		var workflow map[string]any
		if err := yaml.Unmarshal(readWorkflow(t, file), &workflow); err != nil {
			t.Fatal(err)
		}
		for name, value := range workflow["jobs"].(map[string]any) {
			job := value.(map[string]any)
			route, ok := job["runs-on"].(string)
			if !ok {
				t.Fatalf("%s/%s has unexpected runner type", file, name)
			}
			// Existing backend selector owns this dynamic route.
			if name == "native-worker" {
				continue
			}
			rows := []map[string]any{{}}
			if strategy, ok := job["strategy"].(map[string]any); ok {
				if matrix, ok := strategy["matrix"].(map[string]any); ok {
					if include, ok := matrix["include"].([]any); ok {
						rows = nil
						for _, row := range include {
							rows = append(rows, row.(map[string]any))
						}
					}
				}
			}
			for _, row := range rows {
				arch := "ARM64"
				if name == "bwrap-proof" || file == "public-image.yaml" || row["arch"] == "amd64" {
					arch = "X64"
				}
				for _, scenario := range []struct {
					repo, visibility string
					hosted           bool
				}{
					{"noisefloor-oss/deck", "public", true},
					{"private-owner/deck", "private", false},
					{"noisefloor-oss/deck", "private", false},
					{"mirror/deck", "public", false},
				} {
					data, err := json.Marshal(row)
					if err != nil {
						t.Fatal(err)
					}
					// Only the boolean and fromJSON grammar used above is supported.
					// Unsupported functions or operators fail this evaluation.
					script := `import json,sys
expression,repo,visibility,row=sys.argv[1:]
row=json.loads(row)
expression=expression.removeprefix("${{").removesuffix("}}").strip()
expression=expression.replace("github.repository","repo").replace("github.event.repository.visibility","visibility").replace("matrix.os", "row['os']").replace("matrix.runner", "row['runner']").replace("&&"," and ").replace("||"," or ")
print(json.dumps(eval(expression,{"__builtins__":{}},{"repo":repo,"visibility":visibility,"row":row,"fromJSON":json.loads})))`
					output, err := exec.Command("python3", "-c", script, route, scenario.repo, scenario.visibility, string(data)).CombinedOutput()
					if err != nil {
						t.Fatalf("%s/%s: %v %s", file, name, err, output)
					}
					var actual any
					if err := json.Unmarshal(output, &actual); err != nil {
						t.Fatal(err)
					}
					var want any = []any{"self-hosted", "Linux", arch}
					if row["arch"] == "amd64" {
						want = []any{"self-hosted", "Linux", "X64", "ubuntu-24.04"}
					}
					if scenario.hosted {
						want = "ubuntu-24.04-arm"
						if arch == "X64" {
							want = "ubuntu-24.04"
						}
					}
					if !reflect.DeepEqual(actual, want) {
						t.Errorf("%s/%s %s/%s arch %s: got %v want %v", file, name, scenario.repo, scenario.visibility, arch, actual, want)
					}
				}
			}
		}
	}
}
