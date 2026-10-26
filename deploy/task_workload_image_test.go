package deploy_test

import (
	"os"
	"strings"
	"testing"
)

func TestTaskWorkloadImageIsMinimalPinnedAndInert(t *testing.T) {
	raw, err := os.ReadFile("../Dockerfile")
	if err != nil {
		t.Fatal(err)
	}
	dockerfile := string(raw)
	marker := "FROM node:26.12.1-bookworm-slim@sha256:28147155d47dc31abe0a46e0d12b59b2fb3d0a2dcc5dc65fce76a074180b16fe AS task-workload-substrate"
	start := strings.Index(dockerfile, marker)
	if start < 0 {
		t.Fatal("Dockerfile has no task-workload stage")
	}
	end := strings.Index(dockerfile[start+len(marker):], "\nFROM ")
	if end < 0 {
		t.Fatal("Dockerfile has no bounded task-workload stage")
	}
	stage := dockerfile[start : start+len(marker)+end]
	for _, required := range []string{
		"HEALTHCHECK NONE", "STOPSIGNAL SIGTERM", "USER ${NOISEDECK_TASK_UID}:${NOISEDECK_TASK_GID}",
		"ENTRYPOINT [\"/usr/bin/tini\", \"-g\", \"--\", \"/usr/local/libexec/noisedeck-task-workload-latch\"]", "CMD []",
		"ENV HOME=/home/agent", "ca-certificates git openssh-client tini",
	} {
		if !strings.Contains(stage, required) {
			t.Errorf("task-workload stage missing %q", required)
		}
	}
	for _, forbidden := range []string{"FROM runtime", "noise-tasks", "noise-deck", "EXPOSE", "noisedeck-auth-mode-healthcheck", "docker.sock"} {
		if strings.Contains(stage, forbidden) {
			t.Errorf("task-workload stage inherits forbidden surface %q", forbidden)
		}
	}
}

func TestTaskWorkloadScratchIsRuntimeOwned(t *testing.T) {
	raw, err := os.ReadFile("../Dockerfile")
	if err != nil {
		t.Fatal(err)
	}
	dockerfile := string(raw)
	start := strings.Index(dockerfile, " AS task-workload-substrate")
	if start < 0 {
		t.Fatal("Dockerfile has no task-workload stage")
	}
	end := strings.Index(dockerfile[start+1:], "\nFROM ")
	if end < 0 {
		t.Fatal("Dockerfile has no task-workload stage")
	}
	stage := dockerfile[start : start+1+end]
	if strings.Contains(stage, "VOLUME") {
		t.Fatal("image creates an unverified anonymous writable volume")
	}
	for _, path := range []string{"/home/agent", "/run/noisedeck", "/tmp"} {
		if !strings.Contains(stage, path) {
			t.Errorf("task-workload does not prepare %s", path)
		}
	}
}

func TestTaskWorkloadLatchIsBuiltAsOnePinnedStaticArtifact(t *testing.T) {
	raw, err := os.ReadFile("../Dockerfile")
	if err != nil {
		t.Fatal(err)
	}
	dockerfile := string(raw)
	for _, required := range []string{
		"AS task-workload-latch-build", "CGO_ENABLED=0 go build -trimpath",
		"COPY --from=task-workload-latch-build --chmod=0555 /out/task-workload-latch /usr/local/libexec/noisedeck-task-workload-latch",
		"ENTRYPOINT [\"/usr/bin/tini\", \"-g\", \"--\", \"/usr/local/libexec/noisedeck-task-workload-latch\"]",
	} {
		if !strings.Contains(dockerfile, required) {
			t.Errorf("Dockerfile latch contract missing %q", required)
		}
	}
}
