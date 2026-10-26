package harnessbump

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func prJSON(number int, branch, head, verdictState, verdictAt, gate, state string) map[string]any {
	name := strings.TrimSuffix(strings.TrimPrefix(branch, "harness-bump/"), "-1.2.3")
	files := []any{map[string]any{"path": "Dockerfile"}, map[string]any{"path": "docs/provider-contracts/README.md"}}
	if h, err := Lookup(name); err == nil {
		for _, r := range h.Repeats {
			files = append(files, map[string]any{"path": r.Path})
		}
	}
	pr := map[string]any{
		"number": number, "title": "chore(provider): pin " + name + " 1.2.3", "headRefName": branch, "headRefOid": head,
		"baseRefName": "main", "isCrossRepository": false, "files": files, "author": map[string]any{"login": "bump-token-owner"},
		"mergeable": "MERGEABLE", "mergeStateStatus": state,
		"reviews":           []any{},
		"statusCheckRollup": []any{map[string]any{"name": "lint", "conclusion": "SUCCESS"}},
	}
	if verdictState != "" {
		pr["reviews"] = []any{
			map[string]any{"author": map[string]any{"login": "someone"}, "state": "APPROVED", "commit": map[string]any{"oid": head}},
			map[string]any{"author": map[string]any{"login": "noise-review"}, "state": verdictState, "commit": map[string]any{"oid": verdictAt}, "body": "Reviewed head: " + verdictAt + "\nBase: base\n"},
		}
	}
	if gate != "" {
		pr["statusCheckRollup"] = append(pr["statusCheckRollup"].([]any), map[string]any{"name": "fast-checks", "conclusion": gate})
	}
	return pr
}

// threadsByPR is what the fake GraphQL answers per pull request: pages of
// isResolved values. A pull request absent from the map has no threads.
var threadsByPR = map[string][][]bool{}

func fakeGH(prs []map[string]any, calls *[]string) GH {
	return func(_ context.Context, args ...string) ([]byte, error) {
		*calls = append(*calls, strings.Join(args, " "))
		if args[0] == "pr" && args[1] == "list" {
			return json.Marshal(prs)
		}
		if args[0] == "pr" && args[1] == "view" {
			for _, pr := range prs {
				if strconv.Itoa(pr["number"].(int)) != args[2] {
					continue
				}
				copy := map[string]any{}
				copy["state"] = "OPEN"
				for k, v := range pr {
					copy[k] = v
				}
				if pr["mergeStateStatus"] == "BEHIND" {
					copy["headRefOid"] = pr["headRefOid"].(string) + "-updated"
				}
				return json.Marshal(copy)
			}
		}
		if args[0] == "run" && args[1] == "list" {
			return []byte("[]"), nil
		}
		if args[0] == "api" && args[1] == "user" {
			return []byte("bump-token-owner\n"), nil
		}
		if args[0] == "api" && args[1] == "graphql" {
			if strings.Contains(strings.Join(args, " "), "enqueuePullRequest") {
				for _, pr := range prs {
					if strings.Contains(strings.Join(args, " "), "id=PR-"+strconv.Itoa(pr["number"].(int))+" ") {
						pr["queued"] = true
					}
				}
				return []byte(`{"data":{"enqueuePullRequest":{"mergeQueueEntry":{"id":"queue-1"}}}}`), nil
			}
			number, cursor := "", ""
			for i, a := range args {
				if a == "-F" && strings.HasPrefix(args[i+1], "number=") {
					number = strings.TrimPrefix(args[i+1], "number=")
				}
				if a == "-F" && strings.HasPrefix(args[i+1], "cursor=") {
					cursor = strings.TrimPrefix(args[i+1], "cursor=")
				}
			}
			if strings.Contains(strings.Join(args, " "), "mergeQueueEntry") {
				for _, pr := range prs {
					if strconv.Itoa(pr["number"].(int)) != number {
						continue
					}
					base := "base"
					if b, ok := pr["queueBase"].(string); ok {
						base = b
					}
					var entry any
					if pr["queued"] == true {
						entry = map[string]any{"id": "queue-1"}
					}
					return json.Marshal(map[string]any{"data": map[string]any{"repository": map[string]any{"ref": map[string]any{"target": map[string]any{"oid": base}}, "pullRequest": map[string]any{"id": "PR-" + number, "headRefOid": pr["headRefOid"], "baseRefOid": "recorded-merge-base", "mergeQueueEntry": entry}}}})
				}
			}
			pages := threadsByPR[number]
			idx := 0
			if cursor != "" {
				idx, _ = strconv.Atoi(cursor)
			}
			var nodes []map[string]bool
			if idx < len(pages) {
				for _, r := range pages[idx] {
					nodes = append(nodes, map[string]bool{"isResolved": r})
				}
			}
			next := idx+1 < len(pages)
			return json.Marshal(map[string]any{"data": map[string]any{"repository": map[string]any{"pullRequest": map[string]any{"reviewThreads": map[string]any{
				"nodes": nodes, "pageInfo": map[string]any{"hasNextPage": next, "endCursor": strconv.Itoa(idx + 1)},
			}}}}})
		}
		joined := strings.Join(args, " ")
		if args[0] == "api" && strings.Contains(joined, "/compare/") {
			for _, pr := range prs {
				if strings.HasSuffix(args[1], "..."+pr["headRefOid"].(string)) {
					behind := 0
					if n, ok := pr["behind"].(int); ok {
						behind = n
					}
					return json.Marshal(map[string]int{"behind_by": behind})
				}
			}
		}
		if args[0] == "api" && strings.Contains(joined, "/comments") {
			for _, pr := range prs {
				if !strings.Contains(joined, "/issues/"+strconv.Itoa(pr["number"].(int))+"/comments") {
					continue
				}
				if strings.Contains(joined, "--method POST") {
					pr["queueIntent"] = true
					return []byte(`{}`), nil
				}
				if pr["queueIntent"] == true {
					return []byte("<!-- harness-bump-queue-" + strconv.Itoa(pr["number"].(int)) + "-" + pr["headRefOid"].(string) + " -->"), nil
				}
				return nil, nil
			}
		}
		return nil, nil
	}
}

func TestSettleMergesOnlyAnApprovedGreenCleanHead(t *testing.T) {
	prs := []map[string]any{
		prJSON(1, "harness-bump/codex-1.2.3", "aaa", "APPROVED", "aaa", "SUCCESS", "CLEAN"),
		prJSON(2, "harness-bump/kimi-1.2.3", "bbb", "APPROVED", "old", "SUCCESS", "CLEAN"),
		prJSON(3, "harness-bump/omp-1.2.3", "ccc", "CHANGES_REQUESTED", "ccc", "SUCCESS", "CLEAN"),
		prJSON(4, "harness-bump/pi-1.2.3", "ddd", "APPROVED", "ddd", "", "CLEAN"),
		prJSON(5, "harness-bump/qwen-1.2.3", "eee", "APPROVED", "eee", "FAILURE", "CLEAN"),
		prJSON(6, "harness-bump/cline-1.2.3", "fff", "APPROVED", "fff", "SUCCESS", "BEHIND"),
		prJSON(7, "harness-bump/kilo-1.2.3", "ggg", "", "", "SUCCESS", "CLEAN"),
		prJSON(8, "feature/other", "hhh", "APPROVED", "hhh", "SUCCESS", "CLEAN"),
		prJSON(9, "harness-bump/grok-1.2.3", "iii", "APPROVED", "iii", "SUCCESS", "BLOCKED"),
	}
	// Same prefix, not the job's: a fork, a title that is not a bump, a
	// branch naming another harness than the title, an unknown harness,
	// a file beyond the bump's own, and a base other than main. Each is
	// approved, green and clean, and none moves.
	fork := prJSON(10, "harness-bump/mimo-1.2.3", "jjj", "APPROVED", "jjj", "SUCCESS", "CLEAN")
	fork["isCrossRepository"] = true
	titled := prJSON(11, "harness-bump/qoder-1.2.3", "kkk", "APPROVED", "kkk", "SUCCESS", "CLEAN")
	titled["title"] = "feat: something else"
	crossed := prJSON(12, "harness-bump/kilo-1.2.3", "lll", "APPROVED", "lll", "SUCCESS", "CLEAN")
	crossed["title"] = "chore(provider): pin omp 1.2.3"
	other := prJSON(16, "harness-bump/mimo-1.2.3", "ppp", "APPROVED", "ppp", "SUCCESS", "CLEAN")
	other["title"] = "chore(provider): pin mimo 1.2.4"
	unknown := prJSON(13, "harness-bump/nope-1.2.3", "mmm", "APPROVED", "mmm", "SUCCESS", "CLEAN")
	extra := prJSON(14, "harness-bump/cline-1.2.3", "nnn", "APPROVED", "nnn", "SUCCESS", "CLEAN")
	extra["files"] = append(extra["files"].([]any), map[string]any{"path": ".github/workflows/harness-bump-merge.yaml"})
	based := prJSON(15, "harness-bump/qwen-1.2.3", "ooo", "APPROVED", "ooo", "SUCCESS", "CLEAN")
	based["baseRefName"] = "release"
	stranger := prJSON(19, "harness-bump/pi-1.2.3", "sss", "APPROVED", "sss", "SUCCESS", "CLEAN")
	stranger["author"] = map[string]any{"login": "some-writer"}
	prs = append(prs, fork, titled, crossed, unknown, extra, based, other, stranger)
	// Approved, green and clean, but a review thread is unresolved on the
	// second page of its threads: it waits.
	prs = append(prs, prJSON(17, "harness-bump/crush-1.2.3", "qqq", "APPROVED", "qqq", "SUCCESS", "CLEAN"))
	threadsByPR = map[string][][]bool{"17": {{true, true}, {true, false}}, "1": {{true}}}
	defer func() { threadsByPR = map[string][][]bool{} }()
	prs = append(prs, prJSON(18, "harness-bump/opencode-1.2.3", "rrr", "APPROVED", "rrr", "SUCCESS", "CLEAN"))
	var calls []string
	// Every head is the job's own bump except #18, which someone changed.
	same := func(_ context.Context, h Harness, version, head string) (bool, error) {
		if h.Name == "" || version != "1.2.3" {
			return false, nil
		}
		return head != "rrr", nil
	}
	// Every head has its bump run's evidence except #20, a job-shaped
	// pull request nobody's run vouches for.
	prs = append(prs, prJSON(20, "harness-bump/qoder-1.2.3", "ttt", "APPROVED", "ttt", "SUCCESS", "CLEAN"))
	gated := func(_ context.Context, _ Harness, _ string, head string) (bool, error) { return head != "ttt", nil }
	decisions, err := Settle(context.Background(), fakeGH(prs, &calls), "o/r", "noise-review[bot]", 0, true, same, gated)
	if err != nil {
		t.Fatal(err)
	}
	want := map[int]Action{1: ActionQueued, 2: ActionReview, 3: ActionChangesRequested, 4: ActionWait, 5: ActionBlocked, 6: ActionUpdateBranch, 7: ActionReview, 9: ActionWait,
		10: ActionIgnored, 11: ActionIgnored, 12: ActionIgnored, 13: ActionIgnored, 14: ActionIgnored, 15: ActionIgnored, 16: ActionIgnored, 17: ActionWait, 18: ActionHandOff, 19: ActionIgnored, 20: ActionHandOff}
	if len(decisions) != len(want) {
		t.Fatalf("decisions = %+v", decisions)
	}
	for _, d := range decisions {
		if d.Action != want[d.Number] {
			t.Errorf("#%d: %s (%s), want %s", d.Number, d.Action, d.Reason, want[d.Number])
		}
		if (d.Action == ActionQueued || d.Action == ActionUpdateBranch) != d.Applied {
			t.Errorf("#%d: applied = %v for %s", d.Number, d.Applied, d.Action)
		}
	}
	joined := strings.Join(calls, "\n")
	if !strings.Contains(joined, "enqueuePullRequest") || !strings.Contains(joined, "id=PR-1 -f head=aaa") {
		t.Errorf("#1 was not queued at the inspected head: %s", joined)
	}
	if !strings.Contains(joined, "api --method PUT repos/o/r/pulls/6/update-branch -f expected_head_sha=fff") {
		t.Errorf("#6 was not updated at the inspected head: %s", joined)
	}
	if strings.Contains(joined, "pr merge") || strings.Count(joined, "query=mutation(") != 1 {
		t.Errorf("something else merged: %s", joined)
	}
	threadPages := 0
	for _, call := range calls {
		if strings.Contains(call, "reviewThreads") && strings.Contains(call, "number=17") {
			threadPages++
		}
	}
	if threadPages != 2 {
		t.Errorf("#17's threads were not read across both pages: %s", joined)
	}
	if strings.Contains(joined, "--admin") || strings.Contains(joined, "--auto") || strings.Contains(joined, "--delete-branch") {
		t.Error("the merge bypassed the ruleset")
	}
}

func TestSettleHandsOffForeignHeadAndQueuesNext(t *testing.T) {
	prs := []map[string]any{
		prJSON(1, "harness-bump/codex-1.2.3", "foreign", "APPROVED", "foreign", "SUCCESS", "CLEAN"),
		prJSON(2, "harness-bump/kimi-1.2.3", "job-made", "APPROVED", "job-made", "SUCCESS", "CLEAN"),
	}
	var calls []string
	deterministic := func(_ context.Context, _ Harness, _, head string) (bool, error) {
		return head == "job-made", nil
	}
	evidence := func(context.Context, Harness, string, string) (bool, error) { return true, nil }
	decisions, err := Settle(context.Background(), fakeGH(prs, &calls), "o/r", "noise-review[bot]", 0, true, deterministic, evidence)
	if err != nil {
		t.Fatal(err)
	}
	if len(decisions) != 2 || decisions[0].Action != ActionHandOff || decisions[1].Action != ActionQueued {
		t.Fatalf("decisions = %+v", decisions)
	}
	joined := strings.Join(calls, "\n")
	if strings.Contains(joined, "id=PR-1 ") || !strings.Contains(joined, "id=PR-2 -f head=job-made") {
		t.Fatalf("settlement did not queue only the second PR: %s", joined)
	}
}

func TestSettlePreservesDeterministicOperationalErrors(t *testing.T) {
	prs := []map[string]any{prJSON(1, "harness-bump/codex-1.2.3", "head", "APPROVED", "head", "SUCCESS", "CLEAN")}
	want := errors.New("resolver unavailable")
	deterministic := func(context.Context, Harness, string, string) (bool, error) { return false, want }
	evidence := func(context.Context, Harness, string, string) (bool, error) { return true, nil }
	var calls []string
	if _, err := Settle(context.Background(), fakeGH(prs, &calls), "o/r", "noise-review[bot]", 0, true, deterministic, evidence); !errors.Is(err, want) {
		t.Fatalf("err = %v, want %v", err, want)
	}
	if strings.Contains(strings.Join(calls, "\n"), "enqueuePullRequest") {
		t.Fatal("an operational proof error queued a PR")
	}
}

func TestSettleWithoutApplyOnlyDecides(t *testing.T) {
	prs := []map[string]any{prJSON(1, "harness-bump/codex-1.2.3", "aaa", "APPROVED", "aaa", "SUCCESS", "CLEAN")}
	var calls []string
	always := func(context.Context, Harness, string, string) (bool, error) { return true, nil }
	decisions, err := Settle(context.Background(), fakeGH(prs, &calls), "o/r", "noise-review", 1, false, always, always)
	if err != nil || len(decisions) != 1 || decisions[0].Action != ActionMerge || decisions[0].Applied {
		t.Fatalf("decisions = %+v, err %v", decisions, err)
	}
	for _, c := range calls {
		if strings.Contains(c, "pr merge") || strings.Contains(c, "update-branch") {
			t.Errorf("a dry run wrote: %s", c)
		}
	}
	if _, err := Settle(context.Background(), fakeGH(prs, &calls), "o/r", "noise-review", 2, false, always, always); err != nil {
		t.Fatal(err)
	}
	if !sameLogin("noise-review", "noise-review[bot]") || sameLogin("noise-review", "diff-guard[bot]") {
		t.Error("sameLogin ignores the [bot] suffix and nothing else")
	}
}

func TestTipFromZip(t *testing.T) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, _ := zw.Create("probe.txt")
	_, _ = w.Write([]byte("ok"))
	w, _ = zw.Create("tip")
	_, _ = w.Write([]byte("abc123\n"))
	_ = zw.Close()
	tip, err := TipFromZip(buf.Bytes())
	if err != nil || tip != "abc123" {
		t.Errorf("tip = %q, %v", tip, err)
	}
	var none bytes.Buffer
	zw = zip.NewWriter(&none)
	_ = zw.Close()
	if _, err := TipFromZip(none.Bytes()); err == nil {
		t.Error("an artifact without a tip vouched for something")
	}
	if _, err := TipFromZip([]byte("not a zip")); err == nil {
		t.Error("garbage vouched for something")
	}
}

// Under the Actions token the user endpoint answers 403; the job's only
// author is then the bot, and settle goes on rather than failing.
func TestSettleUnderTheActionsToken(t *testing.T) {
	byBot := prJSON(1, "harness-bump/codex-1.2.3", "aaa", "APPROVED", "aaa", "SUCCESS", "CLEAN")
	byBot["author"] = map[string]any{"login": "github-actions"}
	byOwner := prJSON(2, "harness-bump/kimi-1.2.3", "bbb", "APPROVED", "bbb", "SUCCESS", "CLEAN")
	prs := []map[string]any{byBot, byOwner}
	var calls []string
	inner := fakeGH(prs, &calls)
	gh := func(ctx context.Context, args ...string) ([]byte, error) {
		if args[0] == "api" && args[1] == "user" {
			return nil, errors.New("gh: Resource not accessible by integration (HTTP 403)")
		}
		return inner(ctx, args...)
	}
	always := func(context.Context, Harness, string, string) (bool, error) { return true, nil }
	decisions, err := Settle(context.Background(), gh, "o/r", "noise-review", 0, false, always, always)
	if err != nil {
		t.Fatal(err)
	}
	want := map[int]Action{1: ActionMerge, 2: ActionIgnored}
	for _, d := range decisions {
		if d.Action != want[d.Number] {
			t.Errorf("#%d: %s (%s), want %s", d.Number, d.Action, d.Reason, want[d.Number])
		}
	}
}

// ExecGH carries what gh said into the error, from stderr as before and
// from stdout when that is where gh complained, and never more than a
// bounded head of either: fake ghs on PATH.
func TestExecGHCarriesWhatGhSaid(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	fake := func(script string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, "gh"), []byte("#!/bin/sh\n"+script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	fake("echo 'gh: Resource not accessible by integration (HTTP 403)' >&2\nexit 1\n")
	_, err := ExecGH(context.Background(), "api", "user")
	if err == nil || !strings.Contains(err.Error(), "Resource not accessible by integration") || !strings.Contains(err.Error(), "gh api user") {
		t.Errorf("stderr: err = %v; want gh's own words and the command", err)
	}
	fake("echo 'graphql: Could not resolve to a PullRequest'\nexit 1\n")
	_, err = ExecGH(context.Background(), "pr", "list")
	if err == nil || !strings.Contains(err.Error(), "Could not resolve to a PullRequest") {
		t.Errorf("stdout: err = %v; want gh's own words from stdout when stderr is silent", err)
	}
	fake("head -c 300000 /dev/zero | tr '\\0' x >&2\nhead -c 300000 /dev/zero | tr '\\0' y\nexit 1\n")
	_, err = ExecGH(context.Background(), "pr", "list")
	if err == nil || len(err.Error()) > maxSaid+200 {
		t.Errorf("a gh that says too much was carried in full: %d bytes", len(err.Error()))
	}
}
