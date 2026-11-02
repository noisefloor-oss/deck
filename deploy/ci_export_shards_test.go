package deploy

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestExportTestLanesPartitionAllPackages(t *testing.T) {
	var workflow struct {
		Jobs map[string]struct {
			Strategy struct {
				FailFast bool                     `yaml:"fail-fast"`
				Matrix   struct{ Suite []string } `yaml:"matrix"`
			} `yaml:"strategy"`
			Steps []struct {
				Name string
				Run  string
				Env  map[string]string
			}
		}
	}
	if err := yaml.Unmarshal(readWorkflow(t, "ci-fast.yaml"), &workflow); err != nil {
		t.Fatal(err)
	}
	job := workflow.Jobs["source-export"]
	if job.Strategy.FailFast || strings.Join(job.Strategy.Matrix.Suite, ",") != "parallel,serialized,deploy" {
		t.Fatal("export must run all three independent lanes without fail-fast")
	}
	var script string
	for _, step := range job.Steps {
		if step.Name == "Test the extracted source" {
			script = step.Run
			if step.Env["EXPORT_SUITE"] != "${{ matrix.suite }}" {
				t.Fatal("export test command must select its matrix lane")
			}
		}
	}
	if script == "" {
		t.Fatal("missing export test command")
	}
	packages := map[string]string{
		"example/deck":                       "parallel",
		"example/deck/internal/future":       "parallel",
		"example/deck/internal/sessionextra": "parallel",
		"example/deck/deploy":                "deploy",
		"example/deck/deploy/export":         "deploy",
	}
	for _, name := range []string{"session", "terminal", "tmux", "acceptance", "reconciler", "tasks"} {
		packages["example/deck/internal/"+name] = "serialized"
		packages["example/deck/internal/"+name+"/child"] = "serialized"
	}
	dir := t.TempDir()
	stub := `#!/bin/bash
set -eu
if test "$1" = list; then
  test "${FAIL_LIST:-0}" != 1 || exit 17
  printf '%s\n' "$PACKAGES"
else
  printf '%s\n' "$GOMAXPROCS" "$@"
fi
`
	if err := os.WriteFile(filepath.Join(dir, "go"), []byte(stub), 0755); err != nil {
		t.Fatal(err)
	}
	var names []string
	for name := range packages {
		names = append(names, name)
	}
	workspace, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	collectorEnv := []string{"GITHUB_WORKSPACE=" + workspace, "RUNNER_TEMP=" + t.TempDir(), "GITHUB_SHA=fixture"}
	for _, cpus := range []int{1, 2, 4} {
		seen := map[string]int{}
		for _, suite := range job.Strategy.Matrix.Suite {
			cmd := exec.Command("bash", "-e", "-c", script)
			cmd.Env = append(append([]string{}, collectorEnv...), fmt.Sprintf("GOMAXPROCS=%d", cpus), "PATH="+dir+":"+os.Getenv("PATH"), "EXPORT_SUITE="+suite, "PACKAGES="+strings.Join(names, "\n"))
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("%s: %v %s", suite, err, out)
			}
			lines := strings.Split(strings.TrimSpace(string(out)), "\n")
			procs, parallel := fmt.Sprint(cpus), fmt.Sprint(cpus)
			if suite == "serialized" {
				procs, parallel = "1", "1"
			}
			if suite == "deploy" {
				parallel = "1"
			}
			if len(lines) < 7 || strings.Join(lines[:6], ",") != procs+",test,-json,-count=1,-p,"+parallel {
				t.Fatalf("%s test arguments: %q", suite, out)
			}
			for _, name := range lines[6:] {
				if packages[name] != suite {
					t.Errorf("%s assigned to %s, want %s", name, suite, packages[name])
				}
				seen[name]++
			}
			cmd = exec.Command("bash", "-e", "-c", script)
			cmd.Env = append(append([]string{}, collectorEnv...), fmt.Sprintf("GOMAXPROCS=%d", cpus), "PATH="+dir+":"+os.Getenv("PATH"), "EXPORT_SUITE="+suite, "FAIL_LIST=1")
			if err := cmd.Run(); err == nil {
				t.Fatal("go list failure must fail the lane")
			}
		}
		for name := range packages {
			if seen[name] != 1 {
				t.Errorf("%s ran %d times, want exactly once", name, seen[name])
			}
		}
	}
}
