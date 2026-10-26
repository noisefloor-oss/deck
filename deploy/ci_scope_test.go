package deploy

import (
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

type scopeWorkflowJob struct {
	If          string
	Needs       any
	Outputs     map[string]string
	Permissions map[string]string
	Steps       []struct {
		Name string
		ID   string `yaml:"id"`
		If   string
		Run  string
		Env  map[string]string
		With map[string]any
	}
}

func scopeWorkflow(t *testing.T) map[string]scopeWorkflowJob {
	t.Helper()
	var workflow struct{ Jobs map[string]scopeWorkflowJob }
	if err := yaml.Unmarshal(readWorkflow(t, "ci-fast.yaml"), &workflow); err != nil {
		t.Fatal(err)
	}
	return workflow.Jobs
}

func TestCIScopeWorkflowWiring(t *testing.T) {
	jobs := scopeWorkflow(t)
	scope := jobs["scope"]
	if scope.If != privateCIAdmission || scope.Outputs["jobs"] != "${{ steps.select.outputs.jobs }}" || scope.Outputs["mode"] != "${{ steps.select.outputs.mode }}" {
		t.Fatal("scope must admit trusted private changes or public changes and publish its plan")
	}
	selection, tests, history := false, false, false
	for _, step := range scope.Steps {
		if step.ID == "select" {
			selection = step.Run == "python3 -B .github/scripts/ci_scope.py"
		}
		if step.Name == "Test CI selection" {
			tests = strings.Contains(step.Run, "unittest discover") && strings.Contains(step.Run, "test_*.py")
		}
		if step.Name == "Check out source" {
			history = step.With["fetch-depth"] == 0 && step.With["persist-credentials"] == false
		}
	}
	if !selection || !tests || !history {
		t.Fatal("scope requires complete Git history and executable selection tests")
	}
	for _, name := range []string{"frontend", "probes", "lint", "go-serialized", "go-parallel", "deploy-suite", "experiment-enabled", "jail-probe"} {
		job := jobs[name]
		want := "(github.event_name != 'pull_request' || github.event.pull_request.head.repo.full_name == github.repository) && contains(fromJSON(needs.scope.outputs.jobs || '[]'), '" + name + "')"
		if job.Needs != "scope" || job.If != want {
			t.Errorf("%s must retain the trust guard and follow the selected plan", name)
		}
	}
	if jobs["source-export"].Needs != "scope" || jobs["documentation"].Needs != "scope" || jobs["documentation"].If != "" {
		t.Fatal("archive checks must follow scope; lightweight documentation validation must not skip")
	}
	deploySuite := false
	for _, step := range jobs["deploy-suite"].Steps {
		if step.Name == "Run deploy package tests" {
			deploySuite = strings.Contains(step.Run, "-p 1 ./deploy/...")
		}
	}
	if !deploySuite {
		t.Fatal("deploy-suite must test every Go package under deploy/, which go-parallel excludes")
	}
	formatting := false
	for _, step := range jobs["go-parallel"].Steps {
		if step.Name == "Check Go formatting" {
			formatting = strings.Contains(step.Run, "gofmt -l")
		}
	}
	if !formatting {
		t.Fatal("Go-only changes must retain formatting checks without the frontend job")
	}
}

// The scope job reads the pull request to verify its head commits are
// authored by the pull request's account. Without pull-requests: read the
// job token cannot see a private repository's PR, the identity step fails
// every run, and every dependent job is skipped - observed live on PR
// 1559's first Fast checks run.
func TestCIScopeWorkflowCanReadPullRequests(t *testing.T) {
	scope := scopeWorkflow(t)["scope"]
	if scope.Permissions["contents"] != "read" || scope.Permissions["actions"] != "read" || scope.Permissions["pull-requests"] != "read" {
		t.Fatalf("scope must grant contents, actions and pull-requests read; has %v", scope.Permissions)
	}
	identity := false
	for _, step := range scope.Steps {
		if step.Name == "Verify head commits are authored by the pull request's author" {
			identity = step.If == "github.event_name == 'pull_request'" &&
				strings.Contains(step.Run, "verify-merge-gate.py --identity-only")
		}
	}
	if !identity {
		t.Fatal("scope must run the identity-only merge-gate check on pull_request events")
	}
}

func TestCIScopeAggregateFailsClosed(t *testing.T) {
	jobs := scopeWorkflow(t)
	var script string
	for _, step := range jobs["fast-checks"].Steps {
		if step.Name == "Certify fast checks" {
			script = step.Run
		}
	}
	if script == "" {
		t.Fatal("missing aggregate script")
	}
	full := []string{"documentation", "frontend", "probes", "lint", "go-serialized", "go-parallel", "deploy-suite", "experiment-enabled", "jail-probe", "source-export"}
	type scenario struct {
		name, mode, event, trusted string
		selected                   []string
		override                   map[string]string
		success                    bool
	}
	cases := []scenario{
		{"full", "full", "push", "true", full, nil, true},
		{"selected jail failed", "full", "push", "true", full, map[string]string{"jail-probe": "failure"}, false},
		{"selected jail skipped", "full", "push", "true", full, map[string]string{"jail-probe": "skipped"}, false},
		{"docs", "selective", "pull_request", "true", []string{"documentation"}, nil, true},
		{"queue docs", "selective", "merge_group", "true", []string{"documentation"}, nil, true},
		{"frontend", "selective", "pull_request", "true", []string{"documentation", "frontend", "probes"}, nil, true},
		{"backend", "selective", "pull_request", "true", []string{"documentation", "lint", "go-serialized", "go-parallel", "experiment-enabled"}, nil, true},
		{"scope failure", "selective", "pull_request", "true", []string{"documentation"}, map[string]string{"scope": "failure"}, false},
		{"scope skipped", "selective", "pull_request", "true", []string{"documentation"}, map[string]string{"scope": "skipped"}, false},
		{"selected failure", "selective", "pull_request", "true", []string{"documentation"}, map[string]string{"documentation": "failure"}, false},
		{"selected skipped", "selective", "pull_request", "true", []string{"documentation", "frontend"}, map[string]string{"frontend": "skipped"}, false},
		{"selected cancelled", "selective", "pull_request", "true", []string{"documentation", "probes"}, map[string]string{"probes": "cancelled"}, false},
		{"unselected ran", "selective", "pull_request", "true", []string{"documentation"}, map[string]string{"lint": "success"}, false},
		{"unknown suite", "selective", "pull_request", "true", []string{"documentation", "fake"}, nil, false},
		{"duplicate suite", "selective", "pull_request", "true", []string{"documentation", "documentation"}, nil, false},
		{"missing documentation", "selective", "pull_request", "true", []string{"frontend"}, nil, false},
		{"invalid mode", "", "pull_request", "true", full, nil, false},
		{"full cannot narrow", "full", "pull_request", "true", []string{"documentation"}, nil, false},
		{"fork retains archive", "full", "pull_request", "false", full, nil, true},
		{"fork cannot narrow", "selective", "pull_request", "false", []string{"documentation"}, nil, false},
		{"fork archive failed", "full", "pull_request", "false", full, map[string]string{"source-export": "failure"}, false},
	}
	for _, event := range []string{"push", "workflow_dispatch"} {
		cases = append(cases, scenario{"non-PR " + event, "selective", event, "true", []string{"documentation"}, nil, false})
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			needs := map[string]map[string]string{"scope": {"result": "success"}}
			for _, name := range full {
				needs[name] = map[string]string{"result": "skipped"}
			}
			for _, name := range tc.selected {
				if needs[name] != nil {
					needs[name]["result"] = "success"
				}
			}
			if tc.trusted == "false" {
				for _, name := range full {
					if name != "documentation" && name != "source-export" {
						needs[name]["result"] = "skipped"
					}
				}
			}
			for name, result := range tc.override {
				needs[name]["result"] = result
			}
			encoded, _ := json.Marshal(needs)
			selected, _ := json.Marshal(tc.selected)
			cmd := exec.Command("bash", "-e", "-c", script)
			cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "NEEDS_JSON=" + string(encoded), "SELECTED_JOBS=" + string(selected), "SCOPE_MODE=" + tc.mode, "EVENT_NAME=" + tc.event, "TRUSTED_CHANGE=" + tc.trusted, "PROBE_SHARDS=[1,2,3,\"gateway\",\"previews\"]", "PYTHONOPTIMIZE=1"}
			if out, err := cmd.CombinedOutput(); (err == nil) != tc.success {
				t.Errorf("gate: %v %s", err, out)
			}
			if tc.name == "full" {
				for _, shards := range []string{`[1,2,3]`, `[]`, `[true]`, `[1,1]`, `["unknown"]`} {
					probe := exec.Command("bash", "-e", "-c", script)
					probe.Env = append(append([]string{}, cmd.Env[:len(cmd.Env)-2]...), "PROBE_SHARDS="+shards)
					if out, err := probe.CombinedOutput(); err == nil {
						t.Errorf("full plan admitted browser lanes %s: %s", shards, out)
					}
				}
			}
		})
	}
}
