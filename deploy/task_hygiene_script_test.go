package deploy

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// hygieneRepo stages a real git repository for scripts/task-hygiene.sh with
// stub gh and noise executables on PATH. The stubs answer only what the
// script asks: gh reports one merged pull request whose head is the tip of
// the branch "merged", and the engine reports one stale run, a done task 5
// and records every mutating command it receives in a log.
type hygieneRepo struct {
	root, repo, log, rootCommit string
	env                         []string
}

func stageHygieneRepo(t *testing.T) hygieneRepo {
	t.Helper()
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	bin := filepath.Join(root, "bin")
	log := filepath.Join(root, "engine.log")
	for _, dir := range []string{repo, bin} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	source := filepath.Join("..", "scripts", "task-hygiene.sh")
	info, err := os.Stat(source)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o111 == 0 {
		t.Fatal("source script is not executable")
	}
	script, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(repo, "scripts"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "scripts", "task-hygiene.sh"), script, info.Mode()); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(repo, "scripts", "task-hygiene.sh"), info.Mode()); err != nil {
		t.Fatal(err)
	}
	env := append(os.Environ(),
		"PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"),
		"HYGIENE_LOG="+log,
		"RECOVERED_FILE="+filepath.Join(root, "recovered"),
		"RECOVER_ON_PRUNE=1",
		"TASK_HYGIENE_NO_GH=0",
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.invalid",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.invalid",
	)
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		cmd.Env = env
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "-q", "-b", "main")
	git("commit", "-q", "--allow-empty", "-m", "root")
	rootCommit := git("rev-parse", "HEAD")
	git("commit", "-q", "--allow-empty", "-m", "tip")
	git("branch", "merged")
	git("branch", "unmerged")
	// A done task's worktree on a branch whose pull request merged.
	git("worktree", "add", "-q", "-b", "tasks/master/5", filepath.Join(repo, ".worktrees", "master-5"))
	// An in-progress task's worktree, and one with uncommitted work.
	git("worktree", "add", "-q", "-b", "tasks/master/6", filepath.Join(repo, ".worktrees", "master-6"))
	git("worktree", "add", "-q", "-b", "tasks/master/7", filepath.Join(repo, ".worktrees", "master-7"))
	if err := os.WriteFile(filepath.Join(repo, ".worktrees", "master-7", "draft.txt"), []byte("wip\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mergedTip := git("rev-parse", "merged")
	taskTip := git("rev-parse", "tasks/master/5")
	git("init", "--bare", "-q", filepath.Join(root, "origin.git"))
	git("remote", "add", "origin", filepath.Join(root, "origin.git"))
	for _, branch := range []string{"remote-no-pr", "remote-closed", "remote-open", "remote-merged", "remote-error"} {
		git("branch", branch)
		git("push", "-q", "origin", branch)
		git("branch", "-D", branch)
	}
	if err := os.WriteFile(filepath.Join(repo, "residue.txt"), []byte("tracked\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", "residue.txt")
	git("commit", "-q", "-m", "tracked residue fixture")
	if err := os.WriteFile(filepath.Join(repo, "residue.txt"), []byte("stashed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("stash", "push", "-m", "residue", "--", "residue.txt")

	// gh: a merged pull request exists for "merged" and for tasks/master/5.
	gh := `#!/bin/sh
case "$*" in
  "auth status") exit 0 ;;
  *"--head merged --state merged"*) printf '%s\n' "` + mergedTip + `" ;;
  *"--head tasks/master/5 --state merged"*) printf '%s\n' "` + taskTip + `" ;;
  *"--head remote-merged --state all"*) printf '%s\n' 'MERGED' ;;
  *"--head remote-open --state all"*) printf '%s\n' 'OPEN' ;;
  *"--head remote-closed --state all"*) printf '%s\n' 'CLOSED' ;;
  *"--head remote-no-pr --state all"*) printf '' ;;
  *"--head remote-error --state all"*) exit 1 ;;
  *"--state merged"*) printf '' ;;
  *"--state open"*) printf '' ;;
  *) echo "gh stub: unexpected $*" >&2; exit 1 ;;
esac
`
	recent := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)
	noise := `#!/bin/sh
echo "$*" >>"$HYGIENE_LOG"
case "$*" in
  "${FAIL_REPORT:-}")
    echo "engine report error" >&2
    printf '%s\n' invalid-json
    exit "${REPORT_EXIT:-0}" ;;
esac
case "$*" in
  "tasks hygiene -json")
    [ "${HYGIENE_WARNING:-0}" != 1 ] || echo "engine warning" >&2
    printf '%s\n' '{"schemaVersion":1,"now":"2026-10-03T00:00:00Z","staleRuns":[{"runId":41,"tag":"master","task":"5","owner":"x","attempt":1,"ttlSeconds":1,"attemptExpiresAt":"2026-10-03T00:00:00Z"}],"expiredClaims":[{"tag":"master","task":"9","owner":"y","expiredAt":"2026-09-01T00:00:00Z"}],"claimlessInProgress":[]}' ;;
  "tasks show -json -tag master 5") printf '%s\n' '{"id":"5","status":"done","revision":3}' ;;
  "tasks show -json -tag master 6") printf '%s\n' '{"id":"6","status":"in-progress","revision":1}' ;;
  "tasks show -json -tag master 7") printf '%s\n' '{"id":"7","status":"done","revision":1}' ;;
  "tasks milestone list -json") printf '%s\n' '{"milestones":[{"name":"finished","status":"open"},{"name":"active","status":"open"}]}' ;;
  "tasks list -json")
    python3 -c 'import json; d=json.loads("""{"tasks":[{"id":"1","milestone":"finished","status":"done","updatedAt":"2026-01-01T00:00:00Z"},{"id":"2","milestone":"finished","status":"cancelled","updatedAt":"2026-01-01T00:00:00Z"},{"id":"3","milestone":"active","status":"in-progress","updatedAt":"2026-01-01T00:00:00Z"},{"id":"4","milestone":"","status":"deferred","updatedAt":"2020-01-01T00:00:00Z"},{"id":"5","milestone":"","status":"blocked","updatedAt":"` + recent + `"}]}"""); d["tasks"] += [{"id":f"f{i}","status":"done","details":"x"*200} for i in range(1000)]; print(json.dumps(d))' ;;
  "tasks worktree list -json")
    status=cleanup-required
    [ ! -f "$RECOVERED_FILE" ] || status=removed
    printf '%s\n' '{"entries":[{"sagaId":77,"status":"'"$status"'","branch":"tasks/master/8","baseCommit":"` + rootCommit + `","path":".worktrees/master-8","operation":"remove","task":8},{"sagaId":78,"status":"removed","branch":"tasks/master/9","baseCommit":"` + rootCommit + `","path":".worktrees/master-9","operation":"remove","task":9},{"sagaId":79,"status":"cleanup-required","branch":"tasks/master/10","baseCommit":"` + rootCommit + `","operation":"provision","task":10}]}' ;;
  "tasks worktree prune")
    git show-ref --verify refs/heads/tasks/master/8 >>"$HYGIENE_LOG" || exit 1
    if [ -n "${MOVE_BRANCH_TO:-}" ]; then git update-ref refs/heads/tasks/master/8 "$MOVE_BRANCH_TO"; fi
    if [ "$RECOVER_ON_PRUNE" = 1 ]; then touch "$RECOVERED_FILE"; fi ;;
  "tasks run cancel -force"*) exit 0 ;;
  *) echo "noise stub: unexpected $*" >&2; exit 1 ;;
esac
`
	for name, body := range map[string]string{"gh": gh, "noise": noise} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return hygieneRepo{root: root, repo: repo, log: log, rootCommit: rootCommit, env: env}
}

func (h hygieneRepo) run(t *testing.T, args ...string) string {
	t.Helper()
	cmd := exec.Command(filepath.Join(h.repo, "scripts", "task-hygiene.sh"), append([]string{"--repo", h.repo}, args...)...)
	cmd.Dir = h.root
	cmd.Env = h.env
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("task-hygiene.sh %v: %v\n%s", args, err, out)
	}
	return string(out)
}

func (h hygieneRepo) engineLog(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(h.log)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return string(b)
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func TestTaskHygieneDryRunOnlyReports(t *testing.T) {
	h := stageHygieneRepo(t)
	out := h.run(t)
	for _, want := range []string{
		"would recreate branch tasks/master/8 at " + h.rootCommit + " so saga 77 can prove removal",
		"would cancel stale run 41 of task 5",
		"would remove worktree master-5 (task 5 done, branch merged)",
		"keep worktree master-6: task 6 is in-progress",
		"NEEDS A PERSON: worktree master-7 (task 7, done) has 1 uncommitted change(s)",
		"would delete local branch merged (merged pull request head)",
		"NEEDS A PERSON: branch unmerged has no merged pull request at its tip",
		"NEEDS A PERSON: task 9 still carries an expired claim of y",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("dry run output lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(h.engineLog(t), "tasks worktree prune") {
		t.Error("a dry run pruned engine worktree records")
	}
	if got := h.gitOut(t, "branch", "--list", "tasks/master/8", "tasks/master/9"); got != "" {
		t.Errorf("dry run created a branch: %s", got)
	}
	if strings.Contains(h.engineLog(t), "run cancel") {
		t.Error("a dry run cancelled a run")
	}
	for _, path := range []string{".worktrees/master-5", ".worktrees/master-6", ".worktrees/master-7"} {
		if !exists(filepath.Join(h.repo, path)) {
			t.Errorf("dry run removed %s", path)
		}
	}
	branches := strings.Fields(h.gitOut(t, "for-each-ref", "--format=%(refname:short)", "refs/heads/merged", "refs/heads/unmerged"))
	slices.Sort(branches)
	if !slices.Equal(branches, []string{"merged", "unmerged"}) {
		t.Errorf("dry run deleted a branch: %q", branches)
	}
}

func TestTaskHygieneApplyActsOnlyOnProvablySafeResidue(t *testing.T) {
	h := stageHygieneRepo(t)
	out := h.run(t, "--apply")
	for _, want := range []string{
		"did recreate branch tasks/master/8 at " + h.rootCommit + " so saga 77 can prove removal",
		"did delete recreated branch tasks/master/8 (saga 77 removed)",
		"did cancel stale run 41 of task 5",
		"NEEDS A PERSON: saga 79 has operation provision",
		"did remove worktree master-5 (task 5 done, branch merged)",
		"did delete local branch merged (merged pull request head)",
		"did prune remote-tracking branches",
		"did garbage-collect the repository",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("apply output lacks %q:\n%s", want, out)
		}
	}
	if !strings.Contains(h.engineLog(t), "tasks run cancel -force -request-id hygiene-") || !strings.Contains(h.engineLog(t), " 41\n") {
		t.Errorf("stale run 41 was not force-cancelled through the engine:\n%s", h.engineLog(t))
	}
	if want := "tasks worktree prune\n" + h.rootCommit + " refs/heads/tasks/master/8\ntasks worktree list -json\n"; !strings.Contains(h.engineLog(t), want) {
		t.Errorf("prune did not observe the recreated branch at the base commit before re-reading sagas:\n%s", h.engineLog(t))
	}
	if exists(filepath.Join(h.repo, ".worktrees", "master-5")) {
		t.Error("the done, merged, clean worktree survived --apply")
	}
	for _, path := range []string{".worktrees/master-6", ".worktrees/master-7"} {
		if !exists(filepath.Join(h.repo, path)) {
			t.Errorf("--apply removed %s, which is in progress or dirty", path)
		}
	}
	branches := strings.Fields(h.gitOut(t, "for-each-ref", "--format=%(refname:short)", "refs/heads/"))
	slices.Sort(branches)
	if !slices.Equal(branches, []string{"main", "tasks/master/6", "tasks/master/7", "unmerged"}) {
		t.Errorf("unexpected branches after --apply: %q", branches)
	}
}

func TestTaskHygieneReportsRemoteAndTaskResidueWithoutApplyingIt(t *testing.T) {
	h := stageHygieneRepo(t)
	out := h.run(t, "--apply")
	for _, want := range []string{
		"NEEDS A PERSON: remote branch origin/remote-no-pr has no pull request",
		"NEEDS A PERSON: remote branch origin/remote-closed has a closed, unmerged pull request",
		"keep remote branch origin/remote-open: pull request open",
		"NEEDS A PERSON: remote branch origin/remote-merged is merged; delete it",
		"NEEDS A PERSON: remote branch origin/remote-error pull request lookup failed",
		"NEEDS A PERSON: 1 stash(es) remain",
		"NEEDS A PERSON: milestone finished is open although every task is terminal",
		"NEEDS A PERSON: task 4 has been deferred since 2020-01-01T00:00:00Z",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("hygiene report lacks %q:\n%s", want, out)
		}
	}
	for _, unwanted := range []string{"milestone active is open", "task 5 has been blocked", "remote-error has no pull request"} {
		if strings.Contains(out, unwanted) {
			t.Errorf("active residue was incorrectly reported as finished or stale:\n%s", out)
		}
	}
	for _, branch := range []string{"remote-no-pr", "remote-closed", "remote-open", "remote-merged", "remote-error"} {
		if got := h.gitOut(t, "rev-parse", "refs/remotes/origin/"+branch); got == "" {
			t.Errorf("--apply deleted remote branch %s", branch)
		}
	}
	if got := strings.TrimSpace(h.gitOut(t, "stash", "list")); !strings.Contains(got, "residue") {
		t.Errorf("--apply removed the stash: %s", got)
	}
}

func TestTaskHygieneWithoutGhSkipsPullRequestClasses(t *testing.T) {
	h := stageHygieneRepo(t)
	h.env = append(h.env, "TASK_HYGIENE_NO_GH=1")
	out := h.run(t, "--apply")
	if !strings.Contains(out, "gh unavailable: pull-request checks skipped") {
		t.Errorf("missing gh is not announced:\n%s", out)
	}
	if !strings.Contains(out, "keep worktree master-5: task 5 is done but gh is unavailable") {
		t.Errorf("a done task's worktree is not reported as kept without gh:\n%s", out)
	}
	if !exists(filepath.Join(h.repo, ".worktrees", "master-5")) {
		t.Error("a done task's worktree was removed without a merged pull request check")
	}
	if !strings.Contains(h.gitOut(t, "branch", "--list", "merged"), "merged") {
		t.Error("a branch was deleted without a merged pull request check")
	}
}

func (h hygieneRepo) gitOut(t *testing.T, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = h.repo
	cmd.Env = h.env
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

func TestTaskHygieneKeepsBranchForStuckSaga(t *testing.T) {
	h := stageHygieneRepo(t)
	h.env = append(h.env, "RECOVER_ON_PRUNE=0")
	out := h.run(t, "--apply")
	if !strings.Contains(out, "NEEDS A PERSON: saga 77 is cleanup-required; keep recreated branch tasks/master/8") {
		t.Errorf("stuck saga is not reported:\n%s", out)
	}
	if got := strings.TrimSpace(h.gitOut(t, "rev-parse", "refs/heads/tasks/master/8")); got != h.rootCommit {
		t.Errorf("stuck saga branch = %s, want %s", got, h.rootCommit)
	}
}

func TestTaskHygieneKeepsExistingSagaBranch(t *testing.T) {
	h := stageHygieneRepo(t)
	tip := strings.TrimSpace(h.gitOut(t, "rev-parse", "HEAD"))
	h.gitOut(t, "branch", "tasks/master/8", tip)
	out := h.run(t, "--apply")
	if got := strings.TrimSpace(h.gitOut(t, "rev-parse", "refs/heads/tasks/master/8")); got != tip {
		t.Errorf("existing saga branch = %s, want %s", got, tip)
	}
	if strings.Contains(out, "recreate branch tasks/master/8") {
		t.Errorf("existing saga branch was recreated:\n%s", out)
	}
	if !exists(filepath.Join(h.root, "recovered")) {
		t.Error("saga did not flip to removed on prune")
	}
	if !strings.Contains(out, "NEEDS A PERSON: saga 79 has operation provision") {
		t.Errorf("provision saga is not reported:\n%s", out)
	}
	if strings.Contains(out, "recreate branch tasks/master/10") || h.gitOut(t, "branch", "--list", "tasks/master/10") != "" {
		t.Errorf("provision saga branch was recreated:\n%s", out)
	}
}

func TestTaskHygieneKeepsMovedSagaBranch(t *testing.T) {
	h := stageHygieneRepo(t)
	tip := strings.TrimSpace(h.gitOut(t, "rev-parse", "HEAD"))
	h.env = append(h.env, "MOVE_BRANCH_TO="+tip)
	cmd := exec.Command(filepath.Join(h.repo, "scripts", "task-hygiene.sh"), "--repo", h.repo, "--apply")
	cmd.Env = h.env
	out, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 1 {
		t.Fatalf("expected counted compare-and-delete failure, got %v:\n%s", err, out)
	}
	if got := strings.TrimSpace(h.gitOut(t, "rev-parse", "refs/heads/tasks/master/8")); got != tip {
		t.Errorf("moved saga branch = %s, want %s", got, tip)
	}
	for _, want := range []string{"branch tasks/master/8 moved", "did garbage-collect the repository"} {
		if !strings.Contains(string(out), want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
}

func TestTaskHygieneWarningDoesNotInterruptSweep(t *testing.T) {
	h := stageHygieneRepo(t)
	h.env = append(h.env, "HYGIENE_WARNING=1")
	out := h.run(t, "--apply")
	for _, want := range []string{"did cancel stale run 41 of task 5", "did garbage-collect the repository"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if log := h.engineLog(t); !strings.Contains(log, "tasks run cancel -force -request-id hygiene-") || !strings.Contains(log, " 41\n") {
		t.Errorf("stale run was not cancelled:\n%s", log)
	}
}

func TestTaskHygieneReportFailureContinuesSweep(t *testing.T) {
	for _, report := range []string{"hygiene", "worktree list"} {
		for _, code := range []string{"0", "1"} {
			t.Run(report+"/"+code, func(t *testing.T) {
				h := stageHygieneRepo(t)
				h.env = append(h.env, "FAIL_REPORT=tasks "+report+" -json", "REPORT_EXIT="+code)
				cmd := exec.Command(filepath.Join(h.repo, "scripts", "task-hygiene.sh"), "--repo", h.repo, "--apply")
				cmd.Env = h.env
				out, err := cmd.CombinedOutput()
				var exit *exec.ExitError
				if !errors.As(err, &exit) || exit.ExitCode() != 1 {
					t.Fatalf("expected counted report failure, got %v:\n%s", err, out)
				}
				for _, want := range []string{"FAILED read the engine " + strings.Fields(report)[0] + " report", "engine report error", "did remove worktree master-5", "did delete local branch merged", "did garbage-collect the repository"} {
					if !strings.Contains(string(out), want) {
						t.Errorf("output lacks %q:\n%s", want, out)
					}
				}
				if report == "hygiene" && strings.Contains(h.engineLog(t), "run cancel") {
					t.Error("cancelled a run after report failure")
				}
				if report == "worktree list" && strings.Contains(string(out), "recreate branch tasks/master/8") {
					t.Error("recreated a branch after report failure")
				}
			})
		}
	}
}
