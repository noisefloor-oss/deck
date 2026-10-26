package deploy

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestFastChecksWorkflowContract(t *testing.T) {
	raw := readWorkflow(t, "ci-fast.yaml")
	var workflow map[string]any
	if err := yaml.Unmarshal(raw, &workflow); err != nil {
		t.Fatalf("parse fast checks workflow: %v", err)
	}
	events, ok := workflow["on"].(map[string]any)
	if !ok || len(events) != 3 {
		t.Fatalf("fast checks must run only for PRs, merge groups and manual dispatch: %v", workflow["on"])
	}
	for _, event := range []string{"pull_request", "merge_group", "workflow_dispatch"} {
		if _, ok := events[event]; !ok {
			t.Errorf("fast checks is missing the %s trigger", event)
		}
	}
	jobs, ok := workflow["jobs"].(map[string]any)
	if !ok {
		t.Fatal("fast checks workflow has no jobs map")
	}
	// The gate job certifies suite results only; it has no checkout.
	for _, jobName := range []string{
		"frontend", "lint", "go-serialized", "go-parallel", "deploy-suite",
		"experiment-enabled", "jail-probe", "source-export",
	} {
		job, ok := jobs[jobName].(map[string]any)
		if !ok {
			t.Fatalf("fast checks workflow has no %s job", jobName)
		}
		wantRunner := privateCIRunner
		if nativeLinuxJob(jobName) {
			wantRunner = strings.Replace(wantRunner, `"ARM64"`, `"native-linux"`, 1)
		}
		assertRunnerName(t, jobName, job, wantRunner)
		assertJobHasStep(t, jobName, job, "Check out source")
	}
	gate, ok := jobs["fast-checks"].(map[string]any)
	if !ok {
		t.Fatal("fast checks workflow has no fast-checks gate job")
	}
	assertJobHasStep(t, "fast-checks", gate, "Certify fast checks")
	if got, ok := gate["if"].(string); !ok || !strings.Contains(got, "always()") {
		t.Fatalf("fast-checks gate must run always() to certify skipped/failed suites, if = %v", gate["if"])
	}
	needs, ok := gate["needs"].([]any)
	if !ok || len(needs) != 11 {
		t.Fatalf("fast-checks needs = %v, want scope, documentation and all nine suite jobs", gate["needs"])
	}
	for i, want := range []string{
		"scope", "documentation", "frontend", "probes", "lint", "go-serialized", "go-parallel", "deploy-suite",
		"experiment-enabled", "jail-probe", "source-export",
	} {
		if got, _ := needs[i].(string); got != want {
			t.Fatalf("fast-checks needs[%d] = %v, want %q", i, needs[i], want)
		}
	}

	for _, jobName := range []string{
		"frontend", "lint", "go-serialized", "go-parallel", "deploy-suite",
		"experiment-enabled", "jail-probe", "source-export",
	} {
		job := jobs[jobName].(map[string]any)
		for _, unwanted := range []string{
			"Select the native acceptance runner",
			"Prove the pinned Bubblewrap executable identity",
			"Build native runtime as 1000:1000",
			"Start with clean volumes and require readiness",
			"Launch real Chromium sidecar",
			"Verify MemPalace image with a fresh home volume",
			"Build the slim image and verify what it contains",
			"Report image variant sizes",
		} {
			assertJobLacksStep(t, jobName, job, unwanted)
		}
	}
	frontend := jobs["frontend"].(map[string]any)
	for _, step := range []string{
		"Install pinned pnpm",
		"Install pinned Node.js",
		"Check Go formatting",
		"Test and build frontend",
		"Verify tests left the checkout clean",
	} {
		assertJobHasStep(t, "frontend", frontend, step)
	}
	lint := jobs["lint"].(map[string]any)
	for _, step := range []string{
		"Restore Go build and module caches",
		"Check reachable Go vulnerabilities",
		"Restore pinned linter binary",
		"Build pinned Go linter and reject new debt",
	} {
		assertJobHasStep(t, "lint", lint, step)
	}
	serialized := jobs["go-serialized"].(map[string]any)
	for _, step := range []string{
		"Restore Go build and module caches",
		"Restore cached pinned tmux",
		"Install tested tmux",
		"Run serialized Go packages",
	} {
		assertJobHasStep(t, "go-serialized", serialized, step)
	}
	parallel := jobs["go-parallel"].(map[string]any)
	for _, step := range []string{
		"Restore Go build and module caches",
		"Test and build parallel Go packages",
	} {
		assertJobHasStep(t, "go-parallel", parallel, step)
	}
	deploySuite := jobs["deploy-suite"].(map[string]any)
	for _, step := range []string{
		"Restore Go build and module caches",
		"Run deploy package tests",
	} {
		assertJobHasStep(t, "deploy-suite", deploySuite, step)
	}

	text := string(raw)
	// The fast gate must stay attached to pre-merge events with a tight
	// read-only permission scope and its own cancellation group.
	for _, required := range []string{
		"pull_request:",
		"workflow_dispatch:",
		"merge_group:",
		"contents: read",
		"fast-checks-${{ github.ref }}",
	} {
		if !strings.Contains(text, required) {
			t.Errorf("fast checks workflow is missing %q", required)
		}
	}
	for _, required := range []string{
		`GOMAXPROCS: "4"`,
		"go install golang.org/x/vuln/cmd/govulncheck@v1.8.0",
		"govulncheck ./...",
		`--new-from-rev "${lint_base}"`,
		`elif test "${GITHUB_EVENT_NAME}" = workflow_dispatch && test "${GITHUB_REF}" != refs/heads/main; then`,
		`lint_base=origin/main`,
		// The linter binary, the Go build/module caches, and tmux are cached
		// so repeat runs pay neither compilation nor apt.
		"GOLANGCI_LINT_VERSION: v2.6.2",
		"golangci-lint-${{ env.GOLANGCI_LINT_VERSION }}-go-1.26.10-${{ runner.arch }}",
		`deck-go-${{ runner.arch }}-1.26.10-${{ hashFiles('go.sum') }}`,
		"restore-keys: deck-go-${{ runner.arch }}-1.26.10-",
		"~/.cache/go-build",
		"~/go/pkg/mod",
		// The tmux-backed and timing-sensitive packages stay serialized in
		// their own job; the rest run at -p 4 in parallel with it.
		// -count=1 everywhere: the shared build cache would otherwise replay
		// cached green test results across runs, hollowing out the gate.
		`GOMAXPROCS=1 python3 -B .github/scripts/test_metrics.py run --suite go-serialized --output "$RUNNER_TEMP/test-metrics/go-serialized.json" -- go test -count=1 -p 1 \`,
		`./internal/session/... \`,
		`./internal/terminal/... \`,
		`./internal/tmux/... \`,
		`./internal/acceptance/... \`,
		`./internal/reconciler/... \`,
		`./internal/tasks/...`,
		`grep -vE '/(deploy|internal/(session|terminal|tmux|acceptance|reconciler|tasks))(/|$)'`,
		`go test -count=1 -p 4 "${parallel_packages[@]}"`,
		`go build -p 4 ./cmd/noisedeck ./cmd/noisedeck-provision`,
		`GOMAXPROCS=4 python3 -B .github/scripts/test_metrics.py run --suite deploy-suite --output "$RUNNER_TEMP/test-metrics/deploy-suite.json" -- go test -count=1 -p 1 ./deploy/`,
		`name: Save test metrics`,
		`retention-days: 3`,
		`name: test-metrics-source-export-${{ matrix.suite }}-${{ github.run_attempt }}`,
		// The double-build bwrap reproducibility proof is delegated to the
		// slow acceptance workflow inside the fast gate.
		`NOISEDECK_CI_FAST: "1"`,
		"TMUX_VERSION: 3.6a",
		"TMUX_SHA256: b6d8d9c76585db8ef5fa00d4931902fa4b8cbe8166f528f44fc403961a3f3759",
		"https://github.com/tmux/tmux/releases/download/${TMUX_VERSION}/tmux-${TMUX_VERSION}.tar.gz",
		`sha256sum --check --strict`,
		`test "$(tmux -V)" = "tmux ${TMUX_VERSION}"`,
		// The install step must fail loudly rather than silently skip tests.
		"tmux-${{ env.TMUX_VERSION }}-dist",
		"tmux-${{ env.TMUX_VERSION }}-linux-${{ runner.arch }}",
		`make DESTDIR="${dist}" install`,
	} {
		if !strings.Contains(text, required) {
			t.Errorf("fast checks workflow is missing %q", required)
		}
	}
	for _, obsolete := range []string{
		// The fully serialized test invocation was a local-runner constraint.
		"GOMAXPROCS=1 go test -p 1 ./...",
		"deploy/select-native-runner.sh",
		// Scheduled slow acceptance owns these; the fast gate is per-commit.
		"schedule:",
		// Pinned tmux must not regress to an opt-in that skips tests.
		"NOISEDECK_RUN_PINNED_TMUX",
	} {
		if strings.Contains(text, obsolete) {
			t.Errorf("fast checks workflow retains %q", obsolete)
		}
	}
	assertActionsPinnedBySHA(t, raw)
}

func TestSourceExportGate(t *testing.T) {
	raw := readWorkflow(t, "ci-fast.yaml")
	var workflow struct {
		Jobs map[string]struct {
			If    string `yaml:"if"`
			Steps []struct {
				Name      string `yaml:"name"`
				Run       string `yaml:"run"`
				Directory string `yaml:"working-directory"`
			} `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(raw, &workflow); err != nil {
		t.Fatal(err)
	}
	job, ok := workflow.Jobs["source-export"]
	if !ok || job.If != "contains(fromJSON(needs.scope.outputs.jobs || '[]'), 'source-export')" {
		t.Fatal("source export must follow scope without a fork guard")
	}
	documentation, ok := workflow.Jobs["documentation"]
	if !ok || documentation.If != "" {
		t.Fatal("public archive and Markdown validation must run for every scope, including forks")
	}
	foundArchiveCheck := false
	for _, step := range documentation.Steps {
		if step.Run == `python3 -B .github/scripts/test_metrics.py run --suite documentation --output "$RUNNER_TEMP/test-metrics/documentation.json" -- go test -count=1 ./deploy/export/` {
			foundArchiveCheck = true
		}
	}
	if !foundArchiveCheck {
		t.Fatal("documentation must validate the actual public archive and Markdown links")
	}
	commands := map[string]string{
		"Extract the curated source archive": "git archive HEAD | tar --extract --directory",
		"Build and vet the extracted source": "go build ./...\ngo vet ./...",
		"Test the extracted source":          "packages=$(go list ./...)",
	}
	for name, command := range commands {
		found := false
		for _, step := range job.Steps {
			if step.Name != name {
				continue
			}
			found = true
			if !strings.Contains(step.Run, command) {
				t.Errorf("%s must run %q", name, command)
			}
			if strings.Contains(name, "the extracted source") && step.Directory != "${{ env.EXPORT_DIR }}" {
				t.Errorf("%s must run inside the extracted archive", name)
			}
		}
		if !found {
			t.Errorf("missing export step %s", name)
		}
	}
	gate := workflow.Jobs["fast-checks"]
	if gate.If != privateCIGate {
		t.Fatal("the aggregate must certify the export result even for fork pull requests")
	}
	for _, required := range []string{"NEEDS_JSON: ${{ toJSON(needs) }}", "SELECTED_JOBS: ${{ needs.scope.outputs.jobs }}",
		"TRUSTED_CHANGE: ${{ github.event_name != 'pull_request' || github.event.pull_request.head.repo.full_name == github.repository }}"} {
		if !strings.Contains(string(raw), required) {
			t.Errorf("fast checks must certify export result: missing %q", required)
		}
	}
}

func TestReleaseAcceptanceWorkflowContract(t *testing.T) {
	raw := readWorkflow(t, "release-acceptance.yaml")
	var workflow map[string]any
	if err := yaml.Unmarshal(raw, &workflow); err != nil {
		t.Fatalf("parse release acceptance workflow: %v", err)
	}
	text := string(raw)

	// The slow acceptance path no longer gates pull requests.
	if strings.Contains(text, "pull_request:") {
		t.Error("release acceptance workflow must not trigger on pull_request")
	}
	for _, required := range []string{"schedule:", "'17 6,18 * * *'"} {
		if !strings.Contains(text, required) {
			t.Errorf("release acceptance workflow is missing %q", required)
		}
	}

	jobs, ok := workflow["jobs"].(map[string]any)
	if !ok {
		t.Fatal("release acceptance workflow has no jobs map")
	}
	portableJob, ok := jobs["portable-checks"].(map[string]any)
	if !ok {
		t.Fatal("release acceptance workflow has no portable-checks job")
	}
	assertRunnerName(t, "portable-checks", portableJob, privateCIRunner)
	assertJobLacksStep(t, "portable-checks", portableJob, "Reconcile M1 runner watchdog")
	assertJobHasStep(t, "portable-checks", portableJob, "Test and build frontend")
	assertJobHasStep(t, "portable-checks", portableJob, "Select the native acceptance runner")
	assertJobLacksStep(t, "portable-checks", portableJob, "Build native runtime as 1000:1000")

	worker, ok := jobs["native-worker"].(map[string]any)
	if !ok {
		t.Fatal("release acceptance workflow has no native-worker job")
	}
	assertRunnerName(t, "native-worker", worker, `${{ fromJSON(needs.portable-checks.outputs.native_runner) }}`)
	for _, step := range []string{
		"Set up bounded Blacksmith BuildKit cache",
		"Build native runtime as 1000:1000",
		"Start with clean volumes and require readiness",
		"Verify provider configuration ownership",
		"Launch real Chromium sidecar",
		"Verify MemPalace image with a fresh home volume",
		"Build the slim image and verify what it contains",
		"Report image variant sizes",
	} {
		assertJobHasStep(t, "native-worker", worker, step)
	}
	for _, retired := range []string{
		"Reconcile M1 runner watchdog",
		"Start the M1 linux/amd64 engine",
		"Set up bounded M1 BuildKit builder",
		"Vet, test, and build Go under Rosetta",
		"Prime Rosetta provider translations",
		"Build M1 transfer images",
		"Export M1 images for native UAT",
		"Upload M1 images for native UAT",
	} {
		assertJobLacksStep(t, "native-worker", worker, retired)
	}
	// Host-side Go work moved to the fast per-commit workflow.
	for _, unwanted := range []string{
		"Install pinned Go",
		"Install tested tmux",
		"Build pinned Go linter and reject new debt",
		"Test and build frontend",
		"Prove the pinned Bubblewrap executable identity",
		"Upload pinned Bubblewrap evidence",
		"Remove Bubblewrap evidence staging",
	} {
		assertJobLacksStep(t, "native-worker", worker, unwanted)
	}
	workerNeeds, ok := worker["needs"].([]any)
	if !ok || len(workerNeeds) != 2 || workerNeeds[0] != "source" || workerNeeds[1] != "portable-checks" {
		t.Fatalf("native-worker needs = %v, want source and portable-checks", worker["needs"])
	}
	bwrapProof, ok := jobs["bwrap-proof"].(map[string]any)
	if !ok {
		t.Fatal("release acceptance workflow has no bwrap-proof job")
	}
	assertRunnerName(t, "bwrap-proof", bwrapProof, `${{ github.repository == 'noisefloor-oss/deck' && github.event.repository.visibility == 'public' && 'ubuntu-24.04' || fromJSON('["self-hosted","Linux","X64"]') }}`)
	for _, step := range []string{
		"Check out source",
		"Prove the pinned Bubblewrap executable identity",
		"Upload pinned Bubblewrap evidence",
		"Remove Bubblewrap evidence staging",
	} {
		assertJobHasStep(t, "bwrap-proof", bwrapProof, step)
	}

	if _, ok := jobs["m1-native-uat"]; ok {
		t.Fatal("release acceptance workflow retains retired m1-native-uat job")
	}

	gate, ok := jobs["native-linux-amd64"].(map[string]any)
	if !ok {
		t.Fatal("release acceptance workflow has no native-linux-amd64 certification job")
	}
	assertRunnerName(t, "native-linux-amd64", gate, privateCIRunner)
	assertJobHasStep(t, "native-linux-amd64", gate, "Certify selected native backend")
	gateNeeds, ok := gate["needs"].([]any)
	if !ok || len(gateNeeds) != 3 {
		t.Fatalf("native-linux-amd64 needs = %v, want portable-checks, bwrap-proof, native-worker", gate["needs"])
	}
	if gateIf, _ := gate["if"].(string); !strings.Contains(gateIf, "!cancelled()") {
		t.Fatalf("native-linux-amd64 gate must skip (not fail) when a dependency was cancelled, if = %v", gate["if"])
	}

	for _, required := range []string{
		"deploy/select-native-runner.sh",
		`group: release-acceptance-${{ github.event_name == 'push' && 'push' || github.run_id }}`,
		`test "${BWRAP_RESULT}" = success`,
		"NOISEDECK_NATIVE_BACKEND: ${{ github.repository == 'noisefloor-oss/deck' && github.event.repository.visibility == 'public' && 'github' || inputs.native_backend || vars.NOISEDECK_NATIVE_BACKEND || 'self-hosted' }}",
		"fromJSON(needs.portable-checks.outputs.native_runner)",
		"useblacksmith/setup-docker-builder@9309da73a81f66976a6d750572e221508b1e2682",
		`max-cache-size-mb: "5120"`,
		`max-parallelism: "4"`,
		`nofallback: "false"`,
		`case "${NOISEDECK_CI_BACKEND}" in`,
		`blacksmith) test -n "${CANDIDATE}" || test -f docker-container-buildkitd.toml`,
		"rm -f buildkitd.toml docker-container-buildkitd.toml",
		`test "$(docker image inspect "deck:ci-${GITHUB_RUN_ID}" --format '{{.Os}}/{{.Architecture}}')" = linux/amd64`,
		"actions/upload-artifact@ea165f8d65b6e75b540449e92b4886f43607fa02",
		"Upload pinned Bubblewrap evidence",
		"bubblewrap-linux-amd64-${{ github.run_id }}-${{ github.run_attempt }}",
		"bwrap-amd64-rootfs.tar",
		"bubblewrap-source-archive-sha256",
		"bubblewrap-executable-sha256",
		"bubblewrap-rootfs-tar-sha256",
		"Remove Bubblewrap evidence staging",
		"find /proof -mindepth 1 -delete",
		"NOISEDECK_RUNTIME_GID=1000",
		"NOISEDECK_BUILD_VERSION=ci-${GITHUB_RUN_ID}",
		"NOISE_BROWSER_TEST_IMAGE: noise-browser-ci:",
		"COMPOSE_PROJECT_NAME: deck-ci-",
		`docker image rm "deck:ci-`,
		`fetch-depth: 0`,
		"up --detach --wait --wait-timeout 120 app",
		"http://127.0.0.1:8837/readyz",
		"opencode/plugins/noisedeck-provider-hook.js",
		"deploy/browser_sidecar_acceptance.sh",
		// Both distributions are built, checked for what they contain,
		// and reported with their sizes where a reviewer sees them
		// without opening a log.
		"SLIM_TEST_IMAGE: deck:slim-ci-",
		"GO_TEST_IMAGE: deck:go-ci-",
		"--file compose.go-mempalace.yaml build app",
		"test \"$(mempalace --version)\" = \"MemPalace 3.10.0\"",
		"test \"$(go env GOTOOLCHAIN)\" = local",
		"--file compose.yaml --file compose.slim.yaml build app",
		"slim image unexpectedly contains ${excluded}",
		"### Image variant sizes",
		">>\"${GITHUB_STEP_SUMMARY}\"",
		`test "${slim}" -lt "${full}"`,
		"if: always()",
		"down --volumes --remove-orphans",
		`deploy/bwrap/build-bwrap.sh amd64 "/proof/build-${run}"`,
		`golang:1.26.10-bookworm@sha256:ee3901c61e423732605a51f1b8157c849ec01194ea0a274213b5202b2c11e7bf`,
		`for run in 1 2`,
		`test "$(grep 'executable-sha256' "${proof_root}/amd64" | awk '{print $3}' | sort -u | awk 'END {print NR}')" = 1`,
		`test "$(grep 'rootfs-tar-sha256' "${proof_root}/amd64" | awk '{print $3}' | sort -u | awk 'END {print NR}')" = 1`,
		`if [ -z "${BWRAP_PROOF_ROOT:-}" ] || [ ! -d "${BWRAP_PROOF_ROOT}" ]; then`,
	} {
		if !strings.Contains(text, required) {
			t.Errorf("release acceptance workflow is missing %q", required)
		}
	}

	assertActionsPinnedBySHA(t, raw)

	setup := strings.Index(text, "- name: Set up bounded Blacksmith BuildKit cache")
	build := strings.Index(text, "- name: Build native runtime as 1000:1000")
	if setup < 0 || build < 0 || setup >= build {
		t.Fatalf("Blacksmith builder setup must precede the native build: setup=%d build=%d", setup, build)
	}
	for _, required := range []string{
		`install -d -m 0711 "${CI_ROOT}"`,
		`install -d -m 0777`,
		`install -d -m 1777 "${CI_ROOT}/tmp"`,
		`chcon -t container_file_t -l s0 deploy/sidecar-resolv.conf`,
		`getfattr -n security.selinux deploy/sidecar-resolv.conf`,
		`if [ -e /sys/fs/selinux/enforce ]; then`,
		"- name: Verify disposable mount access",
		`test "$(/tmp/probe)" = mount-access-ok`,
	} {
		if !strings.Contains(text, required) {
			t.Errorf("release acceptance workflow is missing %q", required)
		}
	}
	if strings.Contains(text, `sudo chown 1000:1000 "${CI_ROOT}/tmp"`) {
		t.Error("disposable Compose fixture must not require privileged ownership changes")
	}
	if strings.Contains(text, "cp deploy/sidecar-resolv.conf") {
		t.Error("the production wrapper must use the labeled resolver already in the disposable checkout")
	}
	if strings.Contains(text, "'chromium': {'volumes': [resolver]}") {
		t.Error("the default Compose file set must not introduce a resolver-only chromium service")
	}
	fixture := strings.Index(text, "- name: Create disposable Compose fixture")
	mountProbe := strings.Index(text, "- name: Verify disposable mount access")
	if fixture < 0 || mountProbe < 0 || fixture >= mountProbe || mountProbe >= build {
		t.Fatalf("disposable mount access must be proved before the native build: fixture=%d probe=%d build=%d", fixture, mountProbe, build)
	}
	// The hosted-runner disk reclaim must happen before any image build:
	// without it the runtime, browser, and MemPalace builds plus the BuildKit
	// cache exceed the disk left after the preinstalled toolchains.
	reclaim := strings.Index(text, "- name: Reclaim hosted runner disk before image builds")
	if reclaim < 0 || reclaim >= build {
		t.Fatalf("hosted runner disk reclaim must precede the native build: reclaim=%d build=%d", reclaim, build)
	}
	assertJobStepIf(t, "native-worker", worker, "Reclaim hosted runner disk before image builds",
		"runner.environment == 'github-hosted' && (runner.os == 'Linux' && needs.portable-checks.outputs.native_backend == 'github')")
	for _, reclaimed := range []string{
		"/usr/local/lib/android",
		"/opt/hostedtoolcache",
		"/usr/share/dotnet",
		"/opt/ghc",
		"/usr/local/share/powershell",
	} {
		if !strings.Contains(text, reclaimed) {
			t.Errorf("release acceptance workflow does not reclaim %q", reclaimed)
		}
	}
	for _, obsolete := range []string{
		"'17 9 * * 1'",
		"m1-native-uat",
		"m1-linux-amd64-build",
		"noisefloor-macos",
		"runner.os == 'macOS'",
		"Rosetta",
		"colima start noisedeck-ci",
		"M1_IMAGE_ARTIFACT",
		"M1_BUILDX_BUILDER",
		"BUILDKIT_ROSETTA_IMAGE",
		"actions/download-artifact@",
		"Verify host thermal and runner limits",
		"Create CPU-bounded native builder",
		"BUILDX_CPU_QUOTA",
		"RUNNER_CPU_QUOTA",
		"sort -u | wc -l",
		// The toolchains target is retired: Go ships in no runtime image.
		"compose.toolchains.yaml",
		// Host-side Linux Go work moved to the fast per-commit workflow.
		"Vet, test, and build Go with bounded parallelism",
		"Build pinned Go linter and reject new debt",
	} {
		if strings.Contains(text, obsolete) {
			t.Errorf("release acceptance workflow retains %q", obsolete)
		}
	}
}

func TestReleaseAcceptanceCandidateCaches(t *testing.T) {
	var workflow struct {
		Jobs map[string]struct {
			Steps []struct {
				Uses, If string
				With     map[string]any
			}
		}
	}
	if err := yaml.Unmarshal(readWorkflow(t, "release-acceptance.yaml"), &workflow); err != nil {
		t.Fatal(err)
	}
	for name, job := range workflow.Jobs {
		for _, step := range job.Steps {
			switch {
			case strings.HasPrefix(step.Uses, "actions/cache/"):
				if !strings.HasPrefix(step.Uses, "actions/cache/restore@") {
					t.Errorf("%s must only restore caches", name)
				}
			case strings.HasPrefix(step.Uses, "actions/cache@"):
				t.Errorf("%s must not save candidate caches", name)
			case strings.HasPrefix(step.Uses, "actions/setup-go@"):
				if step.With["cache"] != false {
					t.Errorf("%s must disable Go caches", name)
				}
			case strings.HasPrefix(step.Uses, "actions/setup-node@"):
				if cache := step.With["cache"]; cache != nil && cache != "" {
					t.Errorf("%s must disable Node caches", name)
				}
			case strings.HasPrefix(step.Uses, "useblacksmith/setup-docker-builder@"):
				if step.If != "needs.portable-checks.outputs.native_backend == 'blacksmith' && inputs.source_sha == ''" {
					t.Errorf("%s must skip the shared BuildKit cache for candidates", name)
				}
			}
		}
	}
}

func readWorkflow(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", ".github", "workflows", name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return raw
}

func assertActionsPinnedBySHA(t *testing.T, raw []byte) {
	t.Helper()
	shaPin := regexp.MustCompile(`(?m)^\s*uses:\s*[^#\s]+@[0-9a-f]{40}(?:\s|$)`)
	uses := regexp.MustCompile(`(?m)^\s*uses:`).FindAll(raw, -1)
	pins := shaPin.FindAll(raw, -1)
	if len(uses) == 0 || len(pins) != len(uses) {
		t.Fatalf("every action must use a full commit SHA: %d uses, %d pins", len(uses), len(pins))
	}
}

func assertRunnerName(t *testing.T, jobName string, job map[string]any, want string) {
	t.Helper()
	if strings.HasPrefix(want, "[") {
		labels, ok := job["runs-on"].([]any)
		expected := strings.Fields(strings.Trim(want, "[]"))
		if !ok || len(labels) != len(expected) {
			t.Fatalf("%s runs-on = %v, want label list %q", jobName, job["runs-on"], want)
		}
		for i, label := range labels {
			if label != expected[i] {
				t.Fatalf("%s runner label %d = %v, want %s", jobName, i, label, expected[i])
			}
		}
		return
	}
	if got, ok := job["runs-on"].(string); !ok || got != want {
		t.Fatalf("%s runs-on = %v, want %q", jobName, job["runs-on"], want)
	}
}

func assertJobHasStep(t *testing.T, jobName string, job map[string]any, want string) {
	t.Helper()
	for _, name := range jobStepNames(t, jobName, job) {
		if name == want {
			return
		}
	}
	t.Fatalf("%s has no %q step", jobName, want)
}

func assertJobLacksStep(t *testing.T, jobName string, job map[string]any, unwanted string) {
	t.Helper()
	for _, name := range jobStepNames(t, jobName, job) {
		if name == unwanted {
			t.Fatalf("%s must not contain %q step", jobName, unwanted)
		}
	}
}

func assertJobStepIf(t *testing.T, jobName string, job map[string]any, wantName, wantIf string) {
	t.Helper()
	steps, ok := job["steps"].([]any)
	if !ok {
		t.Fatalf("%s has no steps list", jobName)
	}
	for _, entry := range steps {
		step, ok := entry.(map[string]any)
		if !ok {
			t.Fatalf("%s has a non-map step", jobName)
		}
		name, _ := step["name"].(string)
		if name != wantName {
			continue
		}
		gotIf, hasIf := step["if"].(string)
		if !hasIf || gotIf != wantIf {
			t.Fatalf("%s step %q if = %q (present=%v), want %q", jobName, wantName, gotIf, hasIf, wantIf)
		}
		return
	}
	t.Fatalf("%s has no %q step", jobName, wantName)
}

func jobStepNames(t *testing.T, jobName string, job map[string]any) []string {
	t.Helper()
	steps, ok := job["steps"].([]any)
	if !ok {
		t.Fatalf("%s has no steps list", jobName)
	}
	names := make([]string, 0, len(steps))
	for _, entry := range steps {
		step, ok := entry.(map[string]any)
		if !ok {
			t.Fatalf("%s has a non-map step", jobName)
		}
		name, _ := step["name"].(string)
		names = append(names, name)
	}
	return names
}

func TestReviewJailScriptContract(t *testing.T) {
	path := filepath.Join("..", "scripts", "harness-bump", "review-jail.sh")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(path); err != nil || info.Mode()&0o111 == 0 {
		t.Errorf("review-jail.sh must be executable: %v", err)
	}
	if out, err := exec.Command("bash", "-n", path).CombinedOutput(); err != nil {
		t.Fatalf("review-jail.sh does not parse: %v\n%s", err, out)
	}
	text := string(raw)
	// The API fixture runs with a dummy key, dropped capabilities, no privilege
	// escalation, an unprivileged user, read-only tool mounts, a copied work
	// tree, and a stub review command that can only capture. The host alone
	// validates and posts.
	for _, required := range []string{
		"--env OPENAI_API_KEY=jail", "--cap-drop ALL", "--security-opt no-new-privileges",
		":/opt/node:ro,z", ":/opt/codex:ro,z", ":/usr/local/go:ro,z", ":/opt/gomod:ro,z", ":/opt/broker/noise-review:ro,Z", ":/home/jail/.codex:Z", ":/work:Z",
		`cp -a "${JAIL_WORK}/." "${jail}/work"`, `rm -rf "${jail}/work/.git"`, `mkdir -p "${CODEX_HOME}"`, "--network none", `--volume "${JAIL_SOCKET}:/run/broker/broker.sock:ro,Z"`, `--volume "${JAIL_BUMP}:/opt/broker/bump:ro,z"`,
		"an unlinkable broker socket", "a writable broker mount",
		`/opt/broker/bump jail-dial -listen "${JAIL_DIAL}"`, `dial="${dial%%/*}"`, "--env GOPROXY=off", `grep -rIl -- "-----BEGIN [A-Z ]*PRIVATE KEY-----" /run /home/jail /tmp /etc /opt/broker`,
		"https://api.openai.com/ && leak", "169.254.169.254", `"${broker}/v1/files")" = "404"`, `"${broker}/v1/responses")" = "405"`,
		`--user "${uid}:${gid}"`, `= "lo "`, "getent hosts", "not-the-model", `\"store\":true`, `\"web_search\"`, `\"input_image\"`, "a file or URL inside a tool output", "/opt/broker/noise-review review --repo x --pr 0 --event COMMENT", "the verdict channel works from the jail to the host",
		"model_providers.jail.base_url=${JAIL_BROKER}", "model_providers.jail.env_key=OPENAI_API_KEY", "-c 'web_search=disabled'",
		`"${JAIL_BUMP}" verdict -capture`, `{commit_id: $commit, event: .event, body: .body}`, `GH_TOKEN="$("${JAIL_TOOL}" token)" gh api --method POST "repos/${JAIL_REPO}/pulls/${JAIL_PR}/reviews" --input`,
		`jq -r .commit_id)" = "${JAIL_HEAD}"`,
		`-head "${JAIL_HEAD}" -base "${JAIL_BASE}"`,
		`"${JAIL_BUMP}" review-refs`, `jq -r .head`, `jq -r .base`, "no verdict posted",
		"--hostile", "hostile probe found nothing",
	} {
		if !strings.Contains(text, required) {
			t.Errorf("review-jail.sh is missing %q", required)
		}
	}
	for _, forbidden := range []string{"label=disable", "review-home:", "app.pem:", "--env GH_TOKEN", "--env NOISE_REVIEW", "--privileged", "docker.sock:", "--network host", "--add-host", "--network bridge", "--network \"${", "--user 1000", "chown", "--dns"} {
		if strings.Contains(text, forbidden) {
			t.Errorf("review-jail.sh must not contain %q", forbidden)
		}
	}
}

// Every run block of the harness bump workflows parses as bash: a bad
// substitution would otherwise surface only when the configured reviewer
// reached it.
// The reviewer jail's boundary is a pull request gate: the real pinned
// Codex and container run with canary credentials and the hostile probe
// must find nothing.
func TestJailProbeGate(t *testing.T) {
	raw := readWorkflow(t, "ci-fast.yaml")
	var workflow map[string]any
	if err := yaml.Unmarshal(raw, &workflow); err != nil {
		t.Fatalf("parse fast workflow: %v", err)
	}
	jobs := workflow["jobs"].(map[string]any)
	job, ok := jobs["jail-probe"].(map[string]any)
	if !ok {
		t.Fatal("fast workflow has no jail-probe job")
	}
	assertRunnerName(t, "jail-probe", job, privateCIRunner)
	assertJobHasStep(t, "jail-probe", job, "Run the hostile probe inside the real jail with canary credentials")
	text := string(raw)
	for _, required := range []string{"GH_TOKEN: canary-not-a-token", "NOISE_REVIEW_PRIVATE_KEY: canary-not-a-key", "OPENAI_API_KEY=sk-canary-",
		"scripts/harness-bump/review-jail.sh --hostile", `test "${rc}" -eq 3`, "npm install --global --ignore-scripts --prefix", "NEEDS_JSON: ${{ toJSON(needs) }}",
		// Positive evidence: the real Codex sent a request the broker
		// admitted and the broker reached the API, which refused the canary.
		`-stats-file "${RUNNER_TEMP}/evidence/stats.json"`, `test "${admitted}" -gt "${before}"`, `test "$(jq -r .forwarded "${RUNNER_TEMP}/evidence/stats.json")" -ge 1`, `test "$(jq -r .last_upstream_status "${RUNNER_TEMP}/evidence/stats.json")" -eq 401`,
		`JAIL_SOCKET="${RUNNER_TEMP}/broker/broker.sock"`} {
		if !strings.Contains(text, required) {
			t.Errorf("fast workflow is missing %q", required)
		}
	}
	if strings.Contains(text, `-stats-file "${RUNNER_TEMP}/broker/`) {
		t.Error("the broker's evidence must live outside every jail mount")
	}
	if strings.Contains(text, "secrets.OPENAI_API_KEY") || strings.Contains(text, "secrets.NOISE_REVIEW") || strings.Contains(text, "secrets.HARNESS_BUMP_TOKEN") {
		t.Error("the jail probe gate must hold no real credential")
	}
}
