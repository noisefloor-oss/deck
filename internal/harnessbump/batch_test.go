package harnessbump

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func batchFixture(t *testing.T) (string, *Resolver) {
	t.Helper()
	saved := Catalogue
	omp, _ := Lookup("omp")
	crush, _ := Lookup("crush")
	Catalogue = []Harness{omp, crush}
	t.Cleanup(func() { Catalogue = saved })
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "pi-coding-agent") {
			_, _ = fmt.Fprint(w, `{"version":"19.0.0"}`)
			return
		}
		_, _ = fmt.Fprint(w, `{"version":"0.93.0"}`)
	}))
	t.Cleanup(server.Close)
	return fixtureRepo(t), &Resolver{HTTP: server.Client(), NPMRegistry: server.URL}
}

func TestBatchAppliesEveryDiscoveredUpdateOnce(t *testing.T) {
	root, resolver := batchFixture(t)
	result, err := ApplyBatch(context.Background(), root, resolver)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Statuses) != 2 || len(result.Updated) != 2 || len(result.ID) != 64 {
		t.Fatalf("incomplete batch: %+v", result)
	}
	again, err := ApplyBatch(context.Background(), root, resolver)
	if err != nil || len(again.Updated) != 0 || again.ID != result.ID {
		t.Fatalf("not idempotent: %+v, %v", again, err)
	}
}

func TestBatchFailsBeforeMutationWhenAnUpstreamIsUnavailable(t *testing.T) {
	root, resolver := batchFixture(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "pi-coding-agent") {
			_, _ = fmt.Fprint(w, `{"version":"19.0.0"}`)
			return
		}
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	defer server.Close()
	resolver.NPMRegistry = server.URL
	before, _ := os.ReadFile(filepath.Join(root, "Dockerfile"))
	if _, err := ApplyBatch(context.Background(), root, resolver); err == nil {
		t.Fatal("partial discovery accepted")
	}
	after, _ := os.ReadFile(filepath.Join(root, "Dockerfile"))
	if string(before) != string(after) {
		t.Fatal("partial discovery changed pins")
	}
}

func TestBatchReconstructionRejectsExtraEditsWithRealGit(t *testing.T) {
	root, resolver := batchFixture(t)
	readmeFixture(t, root)
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", root}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=Fixture", "GIT_COMMITTER_NAME=Fixture", "GIT_AUTHOR_EMAIL=fixture@example.invalid", "GIT_COMMITTER_EMAIL=fixture@example.invalid")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "-b", "main")
	git("add", ".")
	git("commit", "-m", "base")
	git("remote", "add", "origin", root)
	git("checkout", "-b", "batch")
	result, err := ApplyBatch(context.Background(), root, resolver)
	if err != nil {
		t.Fatal(err)
	}
	git("add", ".")
	recordHistoricalFixtureDates(t, root, "omp", "crush")
	git("add", "README.md")
	git("commit", "-m", "batch")
	head := git("rev-parse", "HEAD")
	git("checkout", "main")
	if err := os.WriteFile(filepath.Join(root, "main-only"), []byte("advance\n"), 0600); err != nil {
		t.Fatal(err)
	}
	git("add", ".")
	git("commit", "-m", "advance")
	// A newer upstream release cannot turn an unchanged published batch into
	// a human edit. Even a discovery outage is irrelevant to its ownership.
	newer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, `{"version":"99.0.0"}`)
	}))
	t.Cleanup(newer.Close)
	resolver.NPMRegistry = newer.URL
	prove := Reconstruct(root, resolver)
	if ok, err := prove(context.Background(), Harness{Name: "all"}, result.ID, head); err != nil || !ok {
		t.Fatalf("valid advanced-base batch rejected: %v, %v", ok, err)
	}
	newer.Close()
	if ok, err := prove(context.Background(), Harness{Name: "all"}, result.ID, head); err != nil || !ok {
		t.Fatalf("published batch depends on live discovery: %v, %v", ok, err)
	}
	if ok, err := prove(context.Background(), Harness{Name: "all"}, strings.Repeat("a", 64), head); err != nil || ok {
		t.Fatalf("wrong identity admitted: %v, %v", ok, err)
	}
	git("checkout", "batch")
	if err := os.WriteFile(filepath.Join(root, "unexpected"), []byte("extra\n"), 0600); err != nil {
		t.Fatal(err)
	}
	git("add", ".")
	git("commit", "-m", "extra")
	modified := git("rev-parse", "HEAD")
	git("checkout", "main")
	if ok, err := prove(context.Background(), Harness{Name: "all"}, result.ID, modified); err != nil || ok {
		t.Fatalf("extra edit admitted: %v, %v", ok, err)
	}
}

func TestBatchProvenanceRecognizesActionsAppAuthor(t *testing.T) {
	pr := pullRequest{Title: "chore(provider): pin all " + strings.Repeat("a", 64), HeadRefName: BranchPrefix + "all-" + strings.Repeat("a", 64), BaseRefName: "main"}
	pr.Author.Login = "app/github-actions"
	pr.Files = append(pr.Files, struct {
		Path string `json:"path"`
	}{Path: "Dockerfile"})
	if err := provenance(pr, map[string]bool{"app/github-actions": true}); err != nil {
		t.Fatal(err)
	}
	pr.Author.Login = "app/unrelated"
	if err := provenance(pr, map[string]bool{"app/github-actions": true}); err == nil {
		t.Fatal("unrelated app admitted")
	}
}

func TestBatchSettlementNeedsEvidenceGreenCIAndExactApproval(t *testing.T) {
	for _, scenario := range []string{"pending", "failed", "review", "merge", "unproven", "stale-review", "changes-requested"} {
		t.Run(scenario, func(t *testing.T) {
			id := strings.Repeat("a", 64)
			gate, verdict, at := "SUCCESS", "", "head"
			if scenario == "pending" {
				gate = "PENDING"
			}
			if scenario == "failed" {
				gate = "FAILURE"
			}
			if scenario == "merge" || scenario == "stale-review" {
				verdict = "APPROVED"
			}
			if scenario == "stale-review" {
				at = "old"
			}
			if scenario == "changes-requested" {
				verdict = "CHANGES_REQUESTED"
			}
			pr := prJSON(42, "harness-bump/omp-1.2.3", "head", verdict, at, gate, "CLEAN")
			pr["title"], pr["headRefName"] = "chore(provider): pin all "+id, BranchPrefix+"all-"+id
			pr["author"] = map[string]any{"login": "app/github-actions"}
			var calls []string
			proof := func(context.Context, Harness, string, string) (bool, error) { return scenario != "unproven", nil }
			decisions, err := Settle(context.Background(), fakeGH([]map[string]any{pr}, &calls), "o/r", "noise-review[bot]", 42, true, proof, proof)
			if err != nil || len(decisions) != 1 {
				t.Fatalf("settle: %v %+v", err, decisions)
			}
			want := map[string]Action{"failed": ActionBlocked, "pending": ActionWait, "review": ActionReview, "merge": ActionQueued, "unproven": ActionHandOff, "stale-review": ActionReview, "changes-requested": ActionChangesRequested}[scenario]
			if decisions[0].Action != want {
				t.Fatalf("got %+v want %s", decisions[0], want)
			}
			queued := strings.Contains(strings.Join(calls, "\n"), "enqueuePullRequest")
			if queued != (scenario == "merge") {
				t.Fatalf("unsafe merge calls: %v", calls)
			}
		})
	}
}

func TestDropReturnsOneHarnessToItsPreBatchPin(t *testing.T) {
	root, resolver := batchFixture(t)
	result, err := ApplyBatch(context.Background(), root, resolver)
	if err != nil {
		t.Fatal(err)
	}
	fullID := result.ID
	if err := Drop(root, &result, "omp", "contract probe failed"); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(root, "Dockerfile"))
	if err != nil {
		t.Fatal(err)
	}
	pins := ReadPins(raw)
	if pins["OMP_VERSION"] != "18.1.2" || pins["CRUSH_VERSION"] != "0.93.0" {
		t.Fatalf("drop did not restore the pre-batch pin: %v", pins)
	}
	deployTest, err := os.ReadFile(filepath.Join(root, "deploy", "omp_cli_package_test.go"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(deployTest), "ARG OMP_VERSION=18.1.2") {
		t.Fatalf("drop left the deploy test on the failed version: %s", deployTest)
	}
	readme, err := os.ReadFile(filepath.Join(root, "docs", "provider-contracts", "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(readme), "| omp | 18.1.2 |") || !strings.Contains(string(readme), "Crush 0.93.0") {
		t.Fatalf("drop left the README inconsistent: %s", readme)
	}
	if len(result.Updated) != 1 || result.Updated[0] != "crush" {
		t.Fatalf("updated = %v", result.Updated)
	}
	if len(result.Dropped) != 1 || result.Dropped[0].Harness != "omp" || result.Dropped[0].Version != "18.1.2" || result.Dropped[0].Reason != "contract probe failed" {
		t.Fatalf("dropped = %+v", result.Dropped)
	}
	if _, ok := result.Revert["omp"]; ok {
		t.Fatal("a dropped harness keeps no revert")
	}
	wantID, err := BatchID(ReadPins(raw))
	if err != nil {
		t.Fatal(err)
	}
	if result.ID != wantID || result.ID == fullID {
		t.Fatalf("id = %s, want %s (full was %s)", result.ID, wantID, fullID)
	}
	// The next discovery sees the dropped harness as outdated again and
	// rebuilds the full batch: a kept-back harness is retried, never lost.
	again, err := ApplyBatch(context.Background(), root, resolver)
	if err != nil {
		t.Fatal(err)
	}
	if again.ID != fullID || len(again.Updated) != 1 || again.Updated[0] != "omp" {
		t.Fatalf("rediscovery after a drop = %+v", again)
	}
	if err := Drop(root, &result, "omp", "again"); err == nil {
		t.Fatal("a harness absent from the batch dropped")
	}
}

// A reduced batch is the job's own work: reconstruction moves only the
// surviving harness from the merge base and the trees compare equal.
func TestReducedBatchReconstructsWithRealGit(t *testing.T) {
	root, resolver := batchFixture(t)
	readmeFixture(t, root)
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", root}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=Fixture", "GIT_COMMITTER_NAME=Fixture", "GIT_AUTHOR_EMAIL=fixture@example.invalid", "GIT_COMMITTER_EMAIL=fixture@example.invalid")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "-b", "main")
	git("add", ".")
	git("commit", "-m", "base")
	git("remote", "add", "origin", root)
	git("checkout", "-b", "batch")
	result, err := ApplyBatch(context.Background(), root, resolver)
	if err != nil {
		t.Fatal(err)
	}
	if err := Drop(root, &result, "crush", "contract probe failed"); err != nil {
		t.Fatal(err)
	}
	git("add", ".")
	recordHistoricalFixtureDates(t, root, "omp")
	git("add", "README.md")
	git("commit", "-m", "batch")
	head := git("rev-parse", "HEAD")
	git("checkout", "main")
	prove := Reconstruct(root, resolver)
	if ok, err := prove(context.Background(), Harness{Name: "all"}, result.ID, head); err != nil || !ok {
		t.Fatalf("reduced batch rejected: %v, %v", ok, err)
	}
	if ok, err := prove(context.Background(), Harness{Name: "all"}, strings.Repeat("b", 64), head); err != nil || ok {
		t.Fatalf("wrong identity admitted: %v, %v", ok, err)
	}
}

func TestBatchEvidenceRejectsPerHarnessMarker(t *testing.T) {
	for _, marker := range []string{"", "2\n", "3\n", "4\n"} {
		tip, err := gatedTipFromZipVersion(evidenceZIP(t, evidenceTestHead, marker), allProbeGateVersion)
		if err != nil {
			t.Fatal(err)
		}
		if (tip == evidenceTestHead) != (marker == allProbeGateVersion) {
			t.Fatalf("marker %q admitted as batch: %q", marker, tip)
		}
	}
}
