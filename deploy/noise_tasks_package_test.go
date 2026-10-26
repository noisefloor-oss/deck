package deploy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	pinnedTasksRef     = "5961c31a5854670f96a64c83a5d8f917bb0889b7"
	pinnedTasksVersion = "2.9.1"
)

func TestRepositoryCarriesNoTaskStateProjection(t *testing.T) {
	for _, name := range []string{
		filepath.Join("..", ".tasks", "tasks", "tasks.json"),
		filepath.Join("..", ".tasks", "state.json"),
	} {
		if _, err := os.Stat(name); !os.IsNotExist(err) {
			if err != nil {
				t.Fatalf("stat retired task-state file %q: %v", name, err)
			}
			t.Errorf("retired task-state file %q is present", name)
		}
	}
}

func TestTaskAuthorityDocsTrackPinnedRelease(t *testing.T) {
	page := filepath.Join("docs", "project-tasks.md")
	readme, err := os.ReadFile(filepath.Join("..", "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(readme), "](docs/project-tasks.md)") {
		t.Errorf("README.md no longer links to %s", page)
	}
	internal := filepath.Join("docs", "internal", "host-migration-runbook.md")
	for _, name := range []string{page, internal} {
		raw, err := os.ReadFile(filepath.Join("..", name))
		if name == internal && os.IsNotExist(err) {
			continue // docs/internal is absent from the source export
		}
		if err != nil {
			t.Fatal(err)
		}
		body := string(raw)
		for _, required := range []string{
			"Tasks v" + pinnedTasksVersion,
			pinnedTasksRef,
			"schema 5",
			"tasks.sqlite",
		} {
			if !strings.Contains(body, required) {
				t.Errorf("%s is missing current task-authority text %q", name, required)
			}
		}
		for _, obsolete := range []string{
			"Tasks v1.3.0",
			"context schema 2",
			"noise tasks live init",
			"noise tasks migrate --rollback",
			"noise tasks migrate --finalize",
		} {
			if strings.Contains(body, obsolete) {
				t.Errorf("%s retains obsolete task-authority text %q", name, obsolete)
			}
		}
	}
}

func TestDockerfilePackagesCanonicalTaskCommands(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "Dockerfile"))
	if err != nil {
		t.Fatal(err)
	}
	body := string(raw)
	for _, required := range []string{
		"ARG NOISE_TASKS_REF=" + pinnedTasksRef,
		"ARG NOISE_TASKS_VERSION=" + pinnedTasksVersion,
		"https://github.com/noisefloor-oss/tasks.git",
		`test "$(git -C /src/noise-tasks rev-parse HEAD)" = "${NOISE_TASKS_REF}"`,
		"make package DESTDIR=/out PREFIX=/usr/local VERSION=\"${NOISE_TASKS_VERSION}\"",
		"COPY --from=noise-tasks-build --chmod=0555 /out/usr/local/bin/noise /usr/local/bin/noise",
		"COPY --from=noise-tasks-build --chmod=0555 /out/usr/local/bin/noise-tasks /usr/local/bin/noise-tasks",
		"COPY --from=noise-tasks-build --chmod=0555 /out/usr/local/bin/noisetask /usr/local/bin/noisetask",
		"COPY --from=noise-tasks-build --chmod=0555 /out/usr/local/lib/noise/payload/noise-tasks /usr/local/libexec/noise/noise-tasks",
		"noise tasks version",
		"noise tasks context --json",
		`c.schemaVersion!==5 || c.storeOwnerBoundary!=="" || c.storeSchemaVersion!==0 || c.engineSchemaVersion!==5`,
		"install -d -m 0700 /out",
		`c.storePath!=="/.tasks/tasks.sqlite"`,
		`c.preferencesPath!==".tasks/preferences.json"`,
		"noise tasks live feed --help",
		"noise tasks milestone list --help",
		"noise tasks set-milestone --help",
		"Usage of milestone list:",
		"Usage of set-milestone:",
		"removal is gated by the published migration plan",
		"noise: plugin not installed: planted",
	} {
		if !strings.Contains(body, required) {
			t.Errorf("Dockerfile is missing %q", required)
		}
	}
	for _, obsolete := range []string{
		"github.com/noisefloor-research/noisetask.git",
		"it will be removed in the next release",
		"VERSION=\"${NOISE_TASKS_REF}\"",
		"fde0edcc1f97eac5141ffce438ec0c6d4975677f",
		"4a06957f4ec57129854a6611c37bfaf530cd413d",
		"c.schemaVersion!==3",
		"c.live?.present",
		"noise tasks migrate --help",
	} {
		if strings.Contains(body, obsolete) {
			t.Errorf("Dockerfile retains obsolete task-engine pin %q", obsolete)
		}
	}
}

func TestDockerfileFetchesTasksAnonymouslyWithoutToken(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "Dockerfile"))
	if err != nil {
		t.Fatal(err)
	}
	body := string(raw)
	for _, required := range []string{
		"RUN --mount=type=secret,id=github_token \\",
		`if [ -s /run/secrets/github_token ]; then`,
		`GIT_CONFIG_GLOBAL="${tasks_git_config}"`,
		`git clone --no-checkout "${NOISE_TASKS_REPO}" /src/noise-tasks`,
	} {
		if !strings.Contains(body, required) {
			t.Errorf("Dockerfile is missing anonymous Tasks fetch behavior %q", required)
		}
	}
	if strings.Contains(body, "id=github_token,required=true") {
		t.Fatal("Dockerfile still requires a GitHub token to fetch the public Tasks repository")
	}
}

func TestDockerfileScopesOptionalTasksAuthenticationToFetch(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "Dockerfile"))
	if err != nil {
		t.Fatal(err)
	}
	body := string(raw)
	for _, required := range []string{
		`git config --file "${tasks_git_config}" http.extraheader "Authorization: Basic ${auth}"`,
		`unset auth`,
		`rm -f "${tasks_git_config}"`,
		`trap 'rm -f "${tasks_git_config}"' EXIT`,
	} {
		if !strings.Contains(body, required) {
			t.Errorf("Dockerfile is missing scoped Tasks authentication behavior %q", required)
		}
	}
	if strings.Contains(body, "git config --global") {
		t.Fatal("Dockerfile persists Tasks authentication in the build user's global config")
	}
}

func TestDevelopmentTaskPackageTracksReleasedModule(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "Makefile"))
	if err != nil {
		t.Fatal(err)
	}
	body := string(raw)
	for _, required := range []string{
		"override NOISE_TASKS_REF := " + pinnedTasksRef,
		"NOISE_TASKS_VERSION      ?= " + pinnedTasksVersion,
		"NOISE_TASKS_MODULE       := github.com/noisefloor-oss/tasks",
		"noise-tasks-package: verify-noise-tasks-source",
		"noisetask-install: verify-noise-tasks-source",
		`verify-noise-tasks-source.sh "$(NOISE_TASKS_SRC)" "$(NOISE_TASKS_REF)"`,
		"PREFIX=/usr/local VERSION=$(NOISE_TASKS_VERSION)",
		"install -d -o 0 -g 0 -m 0700 $(NOISE_TASKS_PACKAGE_ROOT)",
		"test -x $(NOISE_TASKS_PACKAGE_ROOT)/usr/local/libexec/noise-worktree-broker",
		"test -x $(NOISE_TASKS_PACKAGE_ROOT)/usr/local/libexec/noise-broker-mint",
	} {
		if !strings.Contains(body, required) {
			t.Errorf("Makefile is missing %q", required)
		}
	}
	if strings.Contains(body, "github.com/noisefloor-research/noisetask/internal/") {
		t.Fatal("Makefile retains the pre-release Tasks linker module")
	}
}
