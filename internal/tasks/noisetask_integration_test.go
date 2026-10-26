package tasks

// Integration against the real Tasks binaries, built from a local checkout
// (NOISEDECK_NOISE_TASKS_SRC, default /workspace/tasks) into a test temp
// dir. These tests prove the adoption contract the stub
// tests can only assert by convention:
//
//   - the console's read side serves the engine's repository-family SQLite
//     store through the schema-5 context (never a worktree file), and
//   - the runner's fixed noise tasks generate argv is accepted by the real CLI —
//     a usage error would exit 2; a deterministic invalid-provider refusal
//     (exit 1) happens only AFTER the store resolves, so it proves the whole
//     invocation surface except the provider call itself, which is never
//     exercised in tests.
//
// The tests skip when the source tree or the Go toolchain is unavailable;
// the stub-based tests above stay the deterministic contract suite.

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type taskBinaries struct {
	noise      string
	noiseTasks string
	shim       string
}

const tasksIntegrationVersion = "2.10.1"

// buildTaskBinaries compiles a complete fixed-libexec installation. Go's
// build cache makes repeat builds within a test run cheap.
func buildTaskBinaries(t *testing.T) taskBinaries {
	t.Helper()
	src := os.Getenv("NOISEDECK_NOISE_TASKS_SRC")
	if src == "" {
		src = os.Getenv("NOISEDECK_NOISETASK_SRC")
	}
	if src == "" {
		src = "/workspace/tasks"
	}
	if _, err := os.Stat(filepath.Join(src, "cmd", "noise", "main.go")); err != nil {
		t.Skipf("Tasks source not available (set NOISEDECK_NOISE_TASKS_SRC): %v", err)
	}
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not on PATH")
	}
	root := t.TempDir()
	binDir := filepath.Join(root, "bin")
	libexecRoot := filepath.Join(root, "libexec", "noise")
	if err := os.MkdirAll(libexecRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	binaries := taskBinaries{
		noise:      filepath.Join(binDir, "noise"),
		noiseTasks: filepath.Join(binDir, "noise-tasks"),
		shim:       filepath.Join(binDir, "noisetask"),
	}
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	ldflags := "-X github.com/noisefloor-oss/tasks/internal/buildinfo.Version=" + tasksIntegrationVersion + " " +
		"-X github.com/noisefloor-oss/tasks/internal/libexec.Root=" + libexecRoot
	build := func(output, pkg string) {
		t.Helper()
		cmd := exec.Command("go", "build", "-trimpath", "-ldflags", ldflags, "-o", output, pkg)
		cmd.Dir = src
		cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOTMPDIR="+filepath.Join(root, "go-tmp"))
		if err := os.MkdirAll(filepath.Join(root, "go-tmp"), 0o700); err != nil {
			t.Fatal(err)
		}
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("build %s: %v: %s", pkg, err, out)
		}
	}
	build(binaries.noise, "./cmd/noise")
	build(filepath.Join(libexecRoot, "noise-tasks"), "./cmd/noise-tasks")
	if err := os.Link(filepath.Join(libexecRoot, "noise-tasks"), binaries.noiseTasks); err != nil {
		t.Fatal(err)
	}
	build(binaries.shim, "./cmd/noisetask")
	return binaries
}

func engineRun(t *testing.T, bin string, args ...string) (stdout, stderr string, exit int) {
	return engineRunAt(t, "", bin, args...)
}

func engineRunAt(t *testing.T, dir, bin string, args ...string) (stdout, stderr string, exit int) {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Dir = dir
	var out, errb strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	if err != nil {
		var ee *exec.ExitError
		if !errors.As(err, &ee) {
			t.Fatalf("run %v: %v", args, err)
		}
		return out.String(), errb.String(), ee.ExitCode()
	}
	return out.String(), errb.String(), 0
}

// gitProject initializes one self-contained repository so the store path is
// deterministic: <project>/.git/noise-tasks/tasks.sqlite.
func gitProject(t *testing.T) string {
	t.Helper()
	project := t.TempDir()
	if stdout, stderr, exit := engineRunAt(t, project, "git", "init", "--quiet"); exit != 0 {
		t.Fatalf("git init exit=%d stdout=%q stderr=%q", exit, stdout, stderr)
	}
	return project
}

// A linked worktree of a family shares the primary's store; both the
// primary and the worktree must detect as the same family, while a
// store planted from a foreign family is refused.
func TestEngineContextAcceptsLinkedWorktreeRejectsForeignStore(t *testing.T) {
	binaries := buildTaskBinaries(t)

	primary := gitProject(t)
	if stdout, stderr, exit := engineRunAt(t, primary, "git", "config", "user.email", "t@example.com"); exit != 0 {
		t.Fatalf("git config exit=%d stdout=%q stderr=%q", exit, stdout, stderr)
	}
	if stdout, stderr, exit := engineRunAt(t, primary, "git", "config", "user.name", "t"); exit != 0 {
		t.Fatalf("git config exit=%d stdout=%q stderr=%q", exit, stdout, stderr)
	}
	if stdout, stderr, exit := engineRunAt(t, primary, "git", "commit", "--allow-empty", "--quiet", "-m", "seed"); exit != 0 {
		t.Fatalf("git commit exit=%d stdout=%q stderr=%q", exit, stdout, stderr)
	}
	worktree := t.TempDir()
	if stdout, stderr, exit := engineRunAt(t, primary, "git", "worktree", "add", "--quiet", worktree, "-b", "lane"); exit != 0 {
		t.Fatalf("git worktree add exit=%d stdout=%q stderr=%q", exit, stdout, stderr)
	}

	if _, _, exit := engineRunAt(t, primary, binaries.noiseTasks, "init", "--json"); exit != 0 {
		t.Fatalf("primary init exit=%d", exit)
	}

	// Both the primary and the linked worktree own the family store.
	for _, project := range []string{primary, worktree} {
		if _, err := DetectProjectWithEngine(context.Background(), binaries.noise, project); err != nil {
			t.Fatalf("detect %s: %v", project, err)
		}
	}

	// A foreign store planted into another family's git directory is
	// refused: its owner does not resolve back to this project.
	other := gitProject(t)
	if _, _, exit := engineRunAt(t, other, binaries.noiseTasks, "init", "--json"); exit != 0 {
		t.Fatalf("foreign init exit=%d", exit)
	}
	foreignStore := filepath.Join(other, ".git", "noise-tasks", "tasks.sqlite")
	victim := gitProject(t)
	victimDir := filepath.Join(victim, ".git", "noise-tasks")
	if err := os.MkdirAll(victimDir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(victimDir, "tasks.sqlite"), mustRead(t, foreignStore), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := DetectProjectWithEngine(context.Background(), binaries.noise, victim); err == nil {
		t.Fatal("foreign store accepted")
	}
}

// Swapping two families' stores makes each report the other as owner;
// the flat single-hop confirmation must refuse both instead of
// recursing through the cross-claim.
func TestEngineContextRefusesCyclicStoreOwners(t *testing.T) {
	binaries := buildTaskBinaries(t)
	a := gitProject(t)
	b := gitProject(t)
	if _, _, exit := engineRunAt(t, a, binaries.noiseTasks, "init", "--json"); exit != 0 {
		t.Fatalf("init a exit=%d", exit)
	}
	if _, _, exit := engineRunAt(t, b, binaries.noiseTasks, "init", "--json"); exit != 0 {
		t.Fatalf("init b exit=%d", exit)
	}
	storeA := mustRead(t, filepath.Join(a, ".git", "noise-tasks", "tasks.sqlite"))
	storeB := mustRead(t, filepath.Join(b, ".git", "noise-tasks", "tasks.sqlite"))
	if err := os.WriteFile(filepath.Join(a, ".git", "noise-tasks", "tasks.sqlite"), storeB, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(b, ".git", "noise-tasks", "tasks.sqlite"), storeA, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, project := range []string{a, b} {
		done := make(chan error, 1)
		go func(project string) {
			_, err := DetectProjectWithEngine(context.Background(), binaries.noise, project)
			done <- err
		}(project)
		select {
		case err := <-done:
			if err == nil {
				t.Fatalf("cyclic owner accepted for %s", project)
			}
		case <-time.After(15 * time.Second):
			t.Fatalf("cyclic owner detection hung for %s", project)
		}
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// A fresh project has no store: the console hides the panel, and onboarding
// publishes the PRD at the engine-selected docs root without creating any
// task state — store creation belongs to the engine's own setup/init.
func TestTasksContextOnboardingCreatesNoTaskState(t *testing.T) {
	binaries := buildTaskBinaries(t)
	root := gitProject(t)

	if _, err := DetectProjectWithEngine(context.Background(), binaries.noise, root); !errors.Is(err, ErrNotTaskmasterProject) {
		t.Fatalf("detect uninitialized: %v, want ErrNotTaskmasterProject", err)
	}
	if err := WritePRDFileWithEngine(context.Background(), binaries.noise, root, "PRD.md", "# new", false); err != nil {
		t.Fatalf("write new-project PRD: %v", err)
	}
	if raw, err := os.ReadFile(filepath.Join(root, ".tasks", "docs", "PRD.md")); err != nil || string(raw) != "# new" {
		t.Fatalf("canonical PRD = %q, %v", raw, err)
	}
	storePath := filepath.Join(root, ".git", "noise-tasks", "tasks.sqlite")
	if _, err := os.Lstat(storePath); !os.IsNotExist(err) {
		t.Fatalf("PRD upload created the store: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(root, ".tasks", "tasks")); !os.IsNotExist(err) {
		t.Fatalf("PRD upload seeded a retired-layout backlog: %v", err)
	}

	if out, stderr, exit := engineRunAt(t, root, binaries.noise, "tasks", "init"); exit != 0 {
		t.Fatalf("init exit = %d: stdout %s stderr %s", exit, out, stderr)
	}
	info, err := DetectProjectWithEngine(context.Background(), binaries.noise, root)
	if err != nil {
		t.Fatalf("detect initialized: %v", err)
	}
	if info.TasksPath != storePath || info.DocsPath != filepath.Join(root, ".tasks", "docs") {
		t.Fatalf("project info = %+v", info)
	}
}

// The read round-trip: an engine mutation of the store is visible on the
// console's next fresh read, and no worktree file ever becomes task state.
func TestNoisetaskStoreRoundTrip(t *testing.T) {
	binaries := buildTaskBinaries(t)
	root := gitProject(t)

	if out, stderr, exit := engineRunAt(t, root, binaries.noise, "tasks", "init"); exit != 0 {
		t.Fatalf("init exit = %d: stdout %s stderr %s", exit, out, stderr)
	}
	if _, err := ReadTasksWithEngine(context.Background(), binaries.noise, root); err != nil {
		t.Fatalf("read empty store: %v", err)
	}
	largeDetails := strings.Repeat("x", 64<<10)
	if out, stderr, exit := engineRunAt(t, root, binaries.noise, "tasks", "add",
		"--title", "first", "--details", largeDetails, "--json"); exit != 0 {
		t.Fatalf("add exit = %d: stdout %s stderr %s", exit, out, stderr)
	}
	if out, stderr, exit := engineRunAt(t, root, binaries.noise, "tasks", "add",
		"--title", "second", "--json"); exit != 0 {
		t.Fatalf("add exit = %d: stdout %s stderr %s", exit, out, stderr)
	}

	tags, err := ReadTasksWithEngine(context.Background(), binaries.noise, root)
	if err != nil {
		t.Fatalf("read store tasks: %v", err)
	}
	main := tags["main"]
	if len(main.Tasks) != 2 || main.Tasks[1].Title != "second" {
		t.Fatalf("store tasks = %+v", main.Tasks)
	}
	if len(main.Tasks[0].Details) != len(largeDetails) {
		t.Fatalf("details length = %d, want %d", len(main.Tasks[0].Details), len(largeDetails))
	}
	if main.Tasks[0].Revision <= 0 {
		t.Fatalf("task revision = %d", main.Tasks[0].Revision)
	}
	if _, err := os.Lstat(filepath.Join(root, ".tasks", "tasks")); !os.IsNotExist(err) {
		t.Fatalf("engine mutation created a worktree backlog: %v", err)
	}

	root2, err := openEngineTaskRoot(context.Background(), binaries.noise, root, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer root2.Close()
	feed, err := readLiveFeed(context.Background(), binaries.noise, root2, 0)
	if err != nil || liveFeedCursor(feed, 0) == 0 {
		t.Fatalf("live feed = %+v, %v", feed, err)
	}
}

// The generation contract through the real runner and the real binary,
// against an initialized store: the fixed argv is accepted (a usage error
// would be exit 2), and the engine's refusal lands in the run record for the
// user. An invalid provider selects the local refusal before any
// provider call.
func TestNoisetaskGenerateContract(t *testing.T) {
	requireGenerationSupport(t)
	binaries := buildTaskBinaries(t)
	t.Setenv("NOISE_TASKS_PROVIDER", "invalid")
	e := newJobEnv(t, binaries.noise)

	// Real store in place of the stub marker.
	if err := os.Remove(filepath.Join(e.project.CanonicalPath, ".tasks", "tasks.sqlite")); err != nil {
		t.Fatal(err)
	}
	if out, stderr, exit := engineRunAt(t, e.project.CanonicalPath, binaries.noise, "tasks", "init"); exit != 0 {
		t.Fatalf("init exit = %d: stdout %s stderr %s", exit, out, stderr)
	}

	run, err := e.runner.Generate(context.Background(), e.project.ID, "PRD.md", 0, "")
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	final := e.awaitRun(t, run.ID)
	if final.ExitCode == nil || *final.ExitCode != 1 {
		t.Fatalf("exit code = %v, want the engine's refusal (1, never usage 2); stdout %v",
			final.ExitCode, final.Stdout)
	}
	if final.Stdout == nil || !strings.Contains(*final.Stdout, "NOISE_TASKS_PROVIDER") {
		if final.Stdout == nil {
			t.Fatal("captured output is nil")
		}
		t.Fatalf("captured output = %q", *final.Stdout)
	}

	// The store is a working authority: the real engine appends to it and
	// the console's read side serves the result.
	if out, stderr, exit := engineRunAt(t, e.project.CanonicalPath, binaries.noise, "tasks", "add",
		"--title", "post-run", "--json"); exit != 0 {
		t.Fatalf("add exit = %d: stdout %s stderr %s", exit, out, stderr)
	}
	tags, err := ReadTasksWithEngine(context.Background(), binaries.noise, e.project.CanonicalPath)
	if err != nil {
		t.Fatalf("ReadTasks: %v", err)
	}
	if len(tags["main"].Tasks) != 1 || tags["main"].Tasks[0].Title != "post-run" {
		t.Fatalf("store round-trip: %+v", tags["main"].Tasks)
	}
}

func TestTaskBinaryCompatibilitySurface(t *testing.T) {
	binaries := buildTaskBinaries(t)
	wantVersion := "noise-tasks " + tasksIntegrationVersion + "\n"

	for name, command := range map[string]struct {
		bin  string
		args []string
	}{
		"host":       {binaries.noise, []string{"tasks", "version"}},
		"standalone": {binaries.noiseTasks, []string{"version"}},
	} {
		t.Run(name, func(t *testing.T) {
			stdout, stderr, exit := engineRun(t, command.bin, command.args...)
			if exit != 0 || stdout != wantVersion || stderr != "" {
				t.Fatalf("exit=%d stdout=%q stderr=%q", exit, stdout, stderr)
			}
		})
	}

	stdout, stderr, exit := engineRun(t, binaries.shim, "version")
	warning := "warning: noisetask is deprecated; use noise tasks (or noise-tasks); removal is gated by the published migration plan\n"
	if exit != 0 || stdout != wantVersion || stderr != warning {
		t.Fatalf("shim: exit=%d stdout=%q stderr=%q", exit, stdout, stderr)
	}

	project := t.TempDir()
	writeTM(t, project, sampleTasks, "PRD.md")
	t.Setenv("NOISE_TASKS_PROVIDER", "invalid")
	stdout, stderr, exit = engineRun(t, binaries.shim, "parse-prd", "--file",
		filepath.Join(project, ".taskmaster", "tasks", "tasks.json"), "--yes",
		filepath.Join(project, ".taskmaster", "docs", "PRD.md"))
	if exit != 1 || stdout != "" || strings.Count(stderr, warning) != 1 ||
		!strings.HasPrefix(stderr, warning) ||
		!strings.Contains(stderr, `unknown NOISE_TASKS_PROVIDER "invalid"`) {
		t.Fatalf("compatibility generate: exit=%d stdout=%q stderr=%q", exit, stdout, stderr)
	}
}

func TestTaskBinaryContextIsReadOnly(t *testing.T) {
	binaries := buildTaskBinaries(t)
	project := gitProject(t)
	t.Setenv("NOISE_PROJECT_DIR", project)

	storePath := filepath.Join(project, ".git", "noise-tasks", "tasks.sqlite")

	var umbrellaContext string
	for name, command := range map[string]struct {
		bin  string
		args []string
	}{
		"umbrella":   {binaries.noise, []string{"tasks", "context", "--json"}},
		"standalone": {binaries.noiseTasks, []string{"context", "--json"}},
	} {
		t.Run(name, func(t *testing.T) {
			stdout, stderr, exit := engineRun(t, command.bin, command.args...)
			if exit != 0 || stderr != "" {
				t.Fatalf("exit=%d stdout=%q stderr=%q", exit, stdout, stderr)
			}
			var got struct {
				SchemaVersion       int    `json:"schemaVersion"`
				Start               string `json:"start"`
				ProjectBoundary     string `json:"projectBoundary"`
				StorePath           string `json:"storePath"`
				StoreOwnerBoundary  string `json:"storeOwnerBoundary"`
				StoreSchemaVersion  int    `json:"storeSchemaVersion"`
				EngineSchemaVersion int    `json:"engineSchemaVersion"`
				DocsPath            string `json:"docsPath"`
				PreferencesPath     string `json:"preferencesPath"`
				Initialized         bool   `json:"initialized"`
				Writable            bool   `json:"writable"`
				Blocked             bool   `json:"blocked"`
				BlockedReason       string `json:"blockedReason"`
			}
			dec := json.NewDecoder(strings.NewReader(stdout))
			dec.DisallowUnknownFields()
			if err := dec.Decode(&got); err != nil {
				t.Fatalf("context JSON: %v: %s", err, stdout)
			}
			if got.SchemaVersion != 5 || got.Start != project || got.ProjectBoundary != project ||
				got.StorePath != storePath || got.StoreOwnerBoundary != "" ||
				got.StoreSchemaVersion != 0 || got.EngineSchemaVersion != 6 ||
				got.DocsPath != ".tasks/docs/PRD.md" ||
				got.PreferencesPath != ".tasks/preferences.json" ||
				got.Initialized || got.Blocked || got.BlockedReason != "" {
				t.Fatalf("context = %+v", got)
			}
			if umbrellaContext == "" {
				umbrellaContext = stdout
			} else if stdout != umbrellaContext {
				t.Fatalf("context output differs from umbrella form:\n%s\nwant:\n%s", stdout, umbrellaContext)
			}
		})
	}

	// Subcommand help follows the same successful stdout contract as root help.
	for _, args := range [][]string{{"tasks", "live", "feed", "--help"}, {"tasks", "milestone", "list", "--help"}, {"tasks", "set-milestone", "--help"}} {
		stdout, stderr, exit := engineRun(t, binaries.noise, args...)
		if exit != 0 || stderr != "" || !strings.HasPrefix(stdout, "usage: noise-tasks ") {
			t.Fatalf("help %v: exit=%d stdout=%q stderr=%q", args, exit, stdout, stderr)
		}
	}

	// A migration invocation without its required operation is a usage error.
	stdout, stderr, exit := engineRun(t, binaries.noise, "tasks", "migrate")
	if exit != 2 || stdout != "" {
		t.Fatalf("retired migrate: exit=%d stdout=%q stderr=%q", exit, stdout, stderr)
	}

	// Context performed no write: no store, no docs root, no receipt.
	for _, path := range []string{
		storePath,
		filepath.Join(project, ".tasks"),
		filepath.Join(project, ".git", "noise-tasks", "migration.json"),
	} {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Fatalf("read-only context created %s: %v", path, err)
		}
	}
}

func TestNoisetaskInstallTargetHonorsCompatibilityOverrides(t *testing.T) {
	src := os.Getenv("NOISEDECK_NOISE_TASKS_SRC")
	if src == "" {
		src = os.Getenv("NOISEDECK_NOISETASK_SRC")
	}
	if src == "" {
		src = "/workspace/tasks"
	}
	if _, err := os.Stat(filepath.Join(src, "cmd", "noisetask", "main.go")); err != nil {
		t.Skipf("Tasks source not available (set NOISEDECK_NOISE_TASKS_SRC): %v", err)
	}
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not on PATH")
	}
	if _, err := exec.LookPath("make"); err != nil {
		t.Skip("make not on PATH")
	}

	repoRoot, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	scratch := filepath.Join(repoRoot, ".review")
	if err := os.MkdirAll(scratch, 0o700); err != nil {
		t.Fatal(err)
	}
	installRoot, err := os.MkdirTemp(scratch, "noisetask-install-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(installRoot) })
	shim := filepath.Join(installRoot, "custom-tools", "bin", "noisetask")
	goTmp := filepath.Join(installRoot, "go-tmp")
	if err := os.MkdirAll(goTmp, 0o700); err != nil {
		t.Fatal(err)
	}

	install := exec.Command("make", "noisetask-install",
		"NOISETASK_SRC="+src,
		"NOISETASK_BIN="+shim,
		"NOISE_TASKS_VERSION=install-test")
	install.Dir = repoRoot
	install.Env = make([]string, 0, len(os.Environ())+1)
	for _, item := range os.Environ() {
		if !strings.HasPrefix(item, "NOISE_TASKS_SRC=") && !strings.HasPrefix(item, "NOISETASK_SRC=") {
			install.Env = append(install.Env, item)
		}
	}
	install.Env = append(install.Env, "GOTMPDIR="+goTmp)
	output, err := install.CombinedOutput()
	if err != nil {
		t.Fatalf("make noisetask-install: %v: %s", err, output)
	}
	warning := "warning: noisetask is deprecated; use noise tasks (or noise-tasks); removal is gated by the published migration plan\n"
	if strings.Count(string(output), warning) != 1 {
		t.Fatalf("install verification warning count = %d, output %q", strings.Count(string(output), warning), output)
	}
	plugin := filepath.Join(installRoot, "custom-tools", "libexec", "noise", "noise-tasks")
	for _, installed := range []string{shim, plugin} {
		info, err := os.Stat(installed)
		if err != nil {
			t.Fatalf("installed %s: %v", installed, err)
		}
		if info.Mode()&0o111 == 0 {
			t.Fatalf("installed %s is not executable: %v", installed, info.Mode())
		}
	}

	malicious := filepath.Join(installRoot, "malicious")
	if err := os.MkdirAll(malicious, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(malicious, "noise-tasks"), []byte("#!/bin/sh\necho unsafe\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(shim, "version")
	cmd.Env = []string{"PATH=" + malicious, "NOISE_LIBEXEC_ROOT=" + malicious}
	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("installed shim: %v: stderr %q", err, stderr.String())
	}
	if stdout.String() != "noise-tasks install-test\n" || stderr.String() != warning {
		t.Fatalf("installed shim: stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
}

func TestNoiseHostDoesNotSearchPATHForPlugins(t *testing.T) {
	binaries := buildTaskBinaries(t)
	malicious := t.TempDir()
	if err := os.WriteFile(filepath.Join(malicious, "noise-planted"), []byte("#!/bin/sh\necho unsafe\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(binaries.noise, "planted")
	env := make([]string, 0, len(os.Environ())+2)
	for _, item := range os.Environ() {
		if !strings.HasPrefix(item, "PATH=") && !strings.HasPrefix(item, "NOISE_LIBEXEC_ROOT=") {
			env = append(env, item)
		}
	}
	cmd.Env = append(env, "PATH="+malicious, "NOISE_LIBEXEC_ROOT="+malicious)
	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 127 || stdout.String() != "" ||
		stderr.String() != "noise: plugin not installed: planted\n" {
		t.Fatalf("exit=%v stdout=%q stderr=%q", err, stdout.String(), stderr.String())
	}
}
