package harnessbump

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func ciPR(head, verdict, verdictAt, gate, state string) map[string]any {
	pr := prJSON(1, "harness-bump/cline-1.2.3", head, verdict, verdictAt, gate, state)
	pr["state"] = "OPEN"
	return pr
}

func ciProof(context.Context, Harness, string, string) (bool, error) { return true, nil }

func TestSettleDispatchesCIOnlyAfterTheUpdatedHeadIsObservedAndProven(t *testing.T) {
	old := ciPR("old", "APPROVED", "old", "SUCCESS", "BEHIND")
	updated := ciPR("updated", "APPROVED", "old", "", "CLEAN")
	var calls, proven []string
	base := fakeGH([]map[string]any{old}, &calls)
	views := 0
	gh := func(ctx context.Context, args ...string) ([]byte, error) {
		out, err := base(ctx, args...)
		switch strings.Join(args[:2], " ") {
		case "pr view":
			views++
			if views == 1 {
				return json.Marshal(old)
			}
			return json.Marshal(updated)
		case "run list":
			if !strings.Contains(strings.Join(args, " "), "--commit updated") {
				t.Fatalf("CI lookup did not name the updated head: %v", args)
			}
			return []byte("[]"), nil
		case "workflow run":
			if views < 3 || strings.Join(proven, ",") != "old,updated" {
				t.Fatalf("dispatch preceded observation and proof: views=%d, proven=%v", views, proven)
			}
			if got := strings.Join(args, " "); got != "workflow run ci-fast.yaml --repo o/r --ref harness-bump/cline-1.2.3" {
				t.Fatalf("wrong CI dispatch: %s", got)
			}
		}
		return out, err
	}
	evidence := func(_ context.Context, _ Harness, _, head string) (bool, error) {
		proven = append(proven, head)
		return true, nil
	}
	decisions, err := Settle(context.Background(), gh, "o/r", "noise-review", 1, true, ciProof, evidence)
	if err != nil || len(decisions) != 1 || !decisions[0].Applied || decisions[0].Action != ActionUpdateBranch {
		t.Fatalf("decisions=%+v, err=%v", decisions, err)
	}
	if !strings.Contains(decisions[0].Reason, "requested Fast checks on harness-bump/cline-1.2.3 at updated") {
		t.Fatalf("dispatch was not reported: %+v", decisions)
	}
	if strings.Count(strings.Join(calls, "\n"), "workflow run") != 1 || strings.Contains(strings.Join(calls, "\n"), "pr merge") {
		t.Fatalf("unexpected writes: %v", calls)
	}
}

func TestSettleRetriesMissingCIWithoutRepeatingTheBranchUpdate(t *testing.T) {
	for _, mode := range []string{"needs-review", "approved", "lost-dispatch-response"} {
		t.Run(mode, func(t *testing.T) {
			prs := []map[string]any{ciPR("old", "APPROVED", "old", "SUCCESS", "BEHIND")}
			updated := ciPR("updated", "APPROVED", "old", "", "CLEAN")
			var calls []string
			base := fakeGH(prs, &calls)
			dispatches, runExists := 0, false
			gh := func(ctx context.Context, args ...string) ([]byte, error) {
				out, err := base(ctx, args...)
				switch strings.Join(args[:2], " ") {
				case "pr view":
					return json.Marshal(updated)
				case "run list":
					if runExists {
						return []byte(`[{"databaseId":123}]`), nil
					}
					return []byte("[]"), nil
				case "workflow run":
					dispatches++
					if dispatches == 1 {
						runExists = mode == "lost-dispatch-response"
						return nil, errors.New("dispatch response lost")
					}
					runExists = true
				}
				return out, err
			}
			first, err := Settle(context.Background(), gh, "o/r", "noise-review", 1, true, ciProof, ciProof)
			if err == nil || !first[0].Applied || !strings.Contains(first[0].Reason, "dispatch response lost") {
				t.Fatalf("accepted update lost its result: %+v, %v", first, err)
			}
			if mode == "approved" {
				updated = ciPR("updated", "APPROVED", "updated", "", "CLEAN")
			}
			prs[0] = updated
			for range 2 {
				if _, err := Settle(context.Background(), gh, "o/r", "noise-review", 1, true, ciProof, ciProof); err != nil {
					t.Fatal(err)
				}
			}
			wantDispatches := 2
			if mode == "lost-dispatch-response" {
				wantDispatches = 1
			}
			joined := strings.Join(calls, "\n")
			if dispatches != wantDispatches || strings.Count(joined, "expected_head_sha=") != 1 || strings.Contains(joined, "pr merge") {
				t.Fatalf("recovery duplicated an update/CI or merged: %s", joined)
			}
		})
	}
}

func TestSettleMissingCIHonorsTrustAndDryRun(t *testing.T) {
	for _, mode := range []string{"foreign-author", "human-head", "no-evidence", "changes-requested", "dry-run", "dry-update"} {
		t.Run(mode, func(t *testing.T) {
			pr := ciPR("head", "APPROVED", "head", "", "CLEAN")
			apply, same, gated := true, true, true
			switch mode {
			case "foreign-author":
				pr["author"] = map[string]any{"login": "human"}
			case "human-head":
				same = false
			case "no-evidence":
				gated = false
			case "changes-requested":
				pr = ciPR("head", "CHANGES_REQUESTED", "head", "", "CLEAN")
			case "dry-run":
				apply = false
			case "dry-update":
				apply = false
				pr = ciPR("head", "APPROVED", "head", "SUCCESS", "BEHIND")
			}
			var calls []string
			deterministic := func(context.Context, Harness, string, string) (bool, error) { return same, nil }
			evidence := func(context.Context, Harness, string, string) (bool, error) { return gated, nil }
			decisions, err := Settle(context.Background(), fakeGH([]map[string]any{pr}, &calls), "o/r", "noise-review", 1, apply, deterministic, evidence)
			if err != nil || len(decisions) != 1 || decisions[0].Applied {
				t.Fatalf("decisions=%+v, err=%v", decisions, err)
			}
			for _, call := range calls {
				if strings.Contains(call, "workflow run") || strings.Contains(call, "update-branch") || strings.Contains(call, "pr merge") || strings.Contains(call, "run list") {
					t.Fatalf("untrusted or dry-run PR reached CI: %s", call)
				}
			}
		})
	}
}

func TestSettleUpdatedHumanHeadNeverReceivesCIDispatch(t *testing.T) {
	old := ciPR("old", "APPROVED", "old", "SUCCESS", "BEHIND")
	updated := ciPR("human", "APPROVED", "old", "", "CLEAN")
	for _, proof := range []string{"deterministic", "evidence"} {
		t.Run(proof, func(t *testing.T) {
			var calls []string
			base := fakeGH([]map[string]any{old}, &calls)
			gh := func(ctx context.Context, args ...string) ([]byte, error) {
				out, err := base(ctx, args...)
				if args[0] == "pr" && args[1] == "view" {
					return json.Marshal(updated)
				}
				return out, err
			}
			reject := func(_ context.Context, _ Harness, _, head string) (bool, error) { return head != "human", nil }
			deterministic, evidence := Deterministic(ciProof), Evidence(ciProof)
			if proof == "deterministic" {
				deterministic = reject
			} else {
				evidence = reject
			}
			decisions, err := Settle(context.Background(), gh, "o/r", "noise-review", 1, true, deterministic, evidence)
			if err == nil || !decisions[0].Applied || strings.Contains(strings.Join(calls, "\n"), "workflow run") {
				t.Fatalf("unproven updated head was dispatched or update forgotten: %+v, %v, %v", decisions, err, calls)
			}
		})
	}
}

func TestSettleExistingFastCheckNeverGetsAnotherDispatch(t *testing.T) {
	for _, gate := range []string{"QUEUED", "IN_PROGRESS", "SUCCESS", "FAILURE"} {
		t.Run(gate, func(t *testing.T) {
			pr := ciPR("head", "", "", gate, "CLEAN")
			var calls []string
			decisions, err := Settle(context.Background(), fakeGH([]map[string]any{pr}, &calls), "o/r", "noise-review", 1, true, ciProof, ciProof)
			if err != nil || decisions[0].Action != ActionReview || decisions[0].Applied {
				t.Fatalf("decisions=%+v, err=%v", decisions, err)
			}
			if joined := strings.Join(calls, "\n"); strings.Contains(joined, "workflow run") || strings.Contains(joined, "run list") {
				t.Fatalf("existing check was not enough to avoid another dispatch: %s", joined)
			}
		})
	}
}

func TestSettlePullRequestRunAwaitingApprovalDoesNotBlockExplicitCI(t *testing.T) {
	pr := ciPR("head", "", "", "", "CLEAN")
	var calls []string
	base := fakeGH([]map[string]any{pr}, &calls)
	gh := func(ctx context.Context, args ...string) ([]byte, error) {
		out, err := base(ctx, args...)
		switch strings.Join(args[:2], " ") {
		case "run list":
			if strings.Contains(strings.Join(args, " "), "--event workflow_dispatch") {
				return []byte("[]"), nil
			}
			return []byte(`[{"databaseId":99,"event":"pull_request","status":"action_required"}]`), nil
		case "pr view":
			return json.Marshal(pr)
		}
		return out, err
	}
	decisions, err := Settle(context.Background(), gh, "o/r", "noise-review", 1, true, ciProof, ciProof)
	if err != nil || !strings.Contains(decisions[0].Reason, "requested Fast checks") || strings.Count(strings.Join(calls, "\n"), "workflow run ci-fast.yaml") != 1 {
		t.Fatalf("approval-required PR run stranded explicit CI: %+v, %v, %v", decisions, err, calls)
	}
}

func TestSettleApprovesOnlyProvenExactHeadFastChecks(t *testing.T) {
	for _, mode := range []string{"approve", "denied", "head-moved", "foreign-author", "wrong-id", "wrong-head", "wrong-branch", "wrong-workflow", "wrong-event", "foreign-repo", "fork", "wrong-pr", "wrong-attached-head", "already-started", "no-proof", "no-evidence", "dry-run"} {
		t.Run(mode, func(t *testing.T) {
			pr := ciPR("head", "", "", "", "BLOCKED")
			run := map[string]any{
				"id": 99, "event": "pull_request", "path": ".github/workflows/ci-fast.yaml",
				"head_sha": "head", "head_branch": pr["headRefName"], "conclusion": "action_required",
				"repository": map[string]any{"full_name": "o/r"}, "head_repository": map[string]any{"full_name": "o/r"},
				"pull_requests": []any{map[string]any{"number": 1, "head": map[string]any{"sha": "head"}}},
			}
			switch mode {
			case "wrong-id":
				run["id"] = 98
			case "wrong-head":
				run["head_sha"] = "other"
			case "wrong-branch":
				run["head_branch"] = "other"
			case "wrong-workflow":
				run["path"] = ".github/workflows/deploy.yaml"
			case "wrong-event":
				run["event"] = "workflow_dispatch"
			case "foreign-repo":
				run["repository"] = map[string]any{"full_name": "other/repo"}
			case "fork":
				run["head_repository"] = map[string]any{"full_name": "other/repo"}
			case "wrong-pr":
				run["pull_requests"] = []any{map[string]any{"number": 2, "head": map[string]any{"sha": "head"}}}
			case "wrong-attached-head":
				run["pull_requests"] = []any{map[string]any{"number": 1, "head": map[string]any{"sha": "other"}}}
			case "already-started":
				run["conclusion"] = nil
			}
			var calls []string
			base := fakeGH([]map[string]any{pr}, &calls)
			proofCalls := 0
			gh := func(ctx context.Context, args ...string) ([]byte, error) {
				out, err := base(ctx, args...)
				call := strings.Join(args, " ")
				switch {
				case strings.HasPrefix(call, "run list"):
					if !strings.Contains(call, "--event pull_request --status action_required") {
						t.Fatalf("unexpected dispatch lookup: %s", call)
					}
					return []byte(`[{"databaseId":99,"conclusion":"action_required"}]`), nil
				case call == "api repos/o/r/actions/runs/99":
					return json.Marshal(run)
				case strings.HasPrefix(call, "pr view") && mode == "head-moved":
					return json.Marshal(ciPR("other", "", "", "", "BLOCKED"))
				case strings.HasPrefix(call, "pr view") && mode == "foreign-author":
					changed := ciPR("head", "", "", "", "BLOCKED")
					changed["author"] = map[string]any{"login": "human"}
					return json.Marshal(changed)
				case call == "api --method POST repos/o/r/actions/runs/99/approve":
					if proofCalls != 2 {
						t.Fatal("approval preceded candidate and image evidence proof")
					}
					if mode == "denied" {
						return nil, errors.New("403 forbidden")
					}
				}
				return out, err
			}
			deterministic := func(context.Context, Harness, string, string) (bool, error) {
				proofCalls++
				return mode != "no-proof", nil
			}
			evidence := func(context.Context, Harness, string, string) (bool, error) {
				proofCalls++
				return mode != "no-evidence", nil
			}
			decisions, err := Settle(context.Background(), gh, "o/r", "noise-review", 1, mode != "dry-run", deterministic, evidence)
			wantErr := mode != "approve" && mode != "already-started" && mode != "no-proof" && mode != "no-evidence" && mode != "dry-run"
			if (err != nil) != wantErr {
				t.Fatalf("decisions=%+v err=%v", decisions, err)
			}
			joined := strings.Join(calls, "\n")
			wantApproval := mode == "approve" || mode == "denied"
			if strings.Contains(joined, "/approve") != wantApproval || strings.Contains(joined, "workflow run") || strings.Contains(joined, "pr merge") {
				t.Fatalf("unexpected CI or merge writes: %s", joined)
			}
		})
	}
}

func TestSettleCIDispatchRechecksHeadAndReportsMissingTrigger(t *testing.T) {
	for _, mode := range []string{"head-moved", "trigger-missing", "check-appeared"} {
		t.Run(mode, func(t *testing.T) {
			pr := ciPR("head", "APPROVED", "head", "", "CLEAN")
			var calls []string
			base := fakeGH([]map[string]any{pr}, &calls)
			gh := func(ctx context.Context, args ...string) ([]byte, error) {
				out, err := base(ctx, args...)
				switch strings.Join(args[:2], " ") {
				case "run list":
					return []byte("[]"), nil
				case "pr view":
					if mode == "head-moved" {
						return json.Marshal(ciPR("human", "", "", "", "CLEAN"))
					}
					if mode == "check-appeared" {
						return json.Marshal(ciPR("head", "APPROVED", "head", "QUEUED", "CLEAN"))
					}
					return json.Marshal(pr)
				case "workflow run":
					return nil, errors.New("HTTP 422: Workflow does not have workflow_dispatch trigger")
				}
				return out, err
			}
			decisions, err := Settle(context.Background(), gh, "o/r", "noise-review", 1, true, ciProof, ciProof)
			if (err != nil) != (mode != "check-appeared") || decisions[0].Applied {
				t.Fatalf("decisions=%+v, err=%v", decisions, err)
			}
			joined := strings.Join(calls, "\n")
			if mode == "trigger-missing" {
				if !strings.Contains(decisions[0].Reason, "branch must contain ci-fast.yaml with workflow_dispatch") || !strings.Contains(decisions[0].Reason, "HTTP 422") || strings.Contains(joined, "--ref main") {
					t.Fatalf("missing trigger was not actionable: %+v, %s", decisions, joined)
				}
			} else if strings.Contains(joined, "workflow run") {
				t.Fatalf("stale CI decision dispatched: %s", joined)
			}
		})
	}
}

func TestSettleCanceledUpdateObservationKeepsTheAcceptedUpdate(t *testing.T) {
	pr := ciPR("old", "APPROVED", "old", "SUCCESS", "BEHIND")
	var calls []string
	base := fakeGH([]map[string]any{pr}, &calls)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	gh := func(ctx context.Context, args ...string) ([]byte, error) {
		out, err := base(ctx, args...)
		if args[0] == "pr" && args[1] == "view" {
			cancel()
			return json.Marshal(pr)
		}
		return out, err
	}
	decisions, err := Settle(ctx, gh, "o/r", "noise-review", 1, true, ciProof, ciProof)
	if err == nil || !decisions[0].Applied || !strings.Contains(decisions[0].Reason, "context canceled") || strings.Contains(strings.Join(calls, "\n"), "workflow run") {
		t.Fatalf("accepted update was lost on cancellation: %+v, %v, %v", decisions, err, calls)
	}
}

// A PR workflow starts before its aggregate check is created. Settlement must
// wait for that bound workflow, rather than schedule another full manual run.
func TestSettleWaitsForExistingPRWorkflowBeforeAggregate(t *testing.T) {
	for _, status := range []string{"queued", "in_progress", "pending", "waiting", "requested", "completed"} {
		t.Run(status, func(t *testing.T) {
			pr := ciPR("head", "APPROVED", "head", "", "CLEAN")
			var conclusion any
			if status == "completed" {
				conclusion = "success"
			}
			var calls []string
			base := fakeGH([]map[string]any{pr}, &calls)
			gh := func(ctx context.Context, args ...string) ([]byte, error) {
				out, err := base(ctx, args...)
				call := strings.Join(args, " ")
				switch {
				case strings.HasPrefix(call, "run list"):
					if strings.Contains(call, "--event pull_request") && !strings.Contains(call, "--status action_required") {
						return json.Marshal([]map[string]any{{"databaseId": 99, "status": status, "conclusion": conclusion}})
					}
					return []byte("[]"), nil
				case call == "api repos/o/r/actions/runs/99":
					return json.Marshal(map[string]any{
						"id": 99, "event": "pull_request", "path": ".github/workflows/ci-fast.yaml",
						"head_sha": "head", "head_branch": pr["headRefName"], "status": status, "conclusion": conclusion,
						"repository": map[string]any{"full_name": "o/r"}, "head_repository": map[string]any{"full_name": "o/r"},
						"pull_requests": []any{map[string]any{"number": 1, "head": map[string]any{"sha": "head"}}},
					})
				case strings.HasPrefix(call, "pr view"):
					return json.Marshal(pr)
				}
				return out, err
			}
			decisions, err := Settle(context.Background(), gh, "o/r", "noise-review", 1, true, ciProof, ciProof)
			if err != nil || len(decisions) != 1 || decisions[0].Action != ActionWait || decisions[0].Applied {
				t.Fatalf("an existing workflow is pending evidence, never a successful gate: %+v, %v", decisions, err)
			}
			joined := strings.Join(calls, "\n")
			if strings.Contains(joined, "workflow run") || strings.Contains(joined, "/approve") || strings.Contains(joined, "pr merge") {
				t.Fatalf("ordinary bound PR preflight caused a mutation: %s", joined)
			}
		})
	}
}

func TestSettleRejectsUnboundOrFailedExistingPRWorkflow(t *testing.T) {
	for _, mode := range []string{"wrong-id", "wrong-head", "wrong-branch", "wrong-pr", "wrong-attached-head", "wrong-repo", "fork", "wrong-event", "wrong-workflow", "unknown-status"} {
		t.Run(mode, func(t *testing.T) {
			pr := ciPR("head", "APPROVED", "head", "", "CLEAN")
			run := map[string]any{
				"id": 99, "event": "pull_request", "path": ".github/workflows/ci-fast.yaml",
				"head_sha": "head", "head_branch": pr["headRefName"], "status": "in_progress",
				"repository": map[string]any{"full_name": "o/r"}, "head_repository": map[string]any{"full_name": "o/r"},
				"pull_requests": []any{map[string]any{"number": 1, "head": map[string]any{"sha": "head"}}},
			}
			switch mode {
			case "wrong-id":
				run["id"] = 98
			case "wrong-head":
				run["head_sha"] = "other"
			case "wrong-branch":
				run["head_branch"] = "other"
			case "wrong-pr":
				run["pull_requests"] = []any{map[string]any{"number": 2, "head": map[string]any{"sha": "head"}}}
			case "wrong-attached-head":
				run["pull_requests"] = []any{map[string]any{"number": 1, "head": map[string]any{"sha": "other"}}}
			case "wrong-repo":
				run["repository"] = map[string]any{"full_name": "other/repo"}
			case "fork":
				run["head_repository"] = map[string]any{"full_name": "other/repo"}
			case "wrong-event":
				run["event"] = "workflow_dispatch"
			case "wrong-workflow":
				run["path"] = ".github/workflows/other.yaml"
			case "unknown-status":
				run["status"] = "unknown"
			}
			var calls []string
			base := fakeGH([]map[string]any{pr}, &calls)
			gh := func(ctx context.Context, args ...string) ([]byte, error) {
				out, err := base(ctx, args...)
				call := strings.Join(args, " ")
				switch {
				case strings.HasPrefix(call, "run list"):
					if strings.Contains(call, "--event pull_request") && !strings.Contains(call, "--status action_required") {
						return []byte(`[{"databaseId":99,"status":"in_progress"}]`), nil
					}
					return []byte("[]"), nil
				case call == "api repos/o/r/actions/runs/99":
					return json.Marshal(run)
				case strings.HasPrefix(call, "pr view"):
					return json.Marshal(pr)
				}
				return out, err
			}
			_, err := Settle(context.Background(), gh, "o/r", "noise-review", 1, true, ciProof, ciProof)
			if err == nil {
				t.Fatal("unbound, failed or unknown workflow was accepted")
			}
			joined := strings.Join(calls, "\n")
			if strings.Contains(joined, "workflow run") || strings.Contains(joined, "/approve") || strings.Contains(joined, "pr merge") {
				t.Fatalf("rejected workflow caused a mutation: %s", joined)
			}
		})
	}
}

func TestSettleRerunsFailedPreflightInsteadOfWedging(t *testing.T) {
	for _, conclusion := range []string{"cancelled", "failure"} {
		t.Run(conclusion, func(t *testing.T) {
			pr := ciPR("head", "APPROVED", "head", "", "CLEAN")
			run := map[string]any{
				"id": 99, "event": "pull_request", "path": ".github/workflows/ci-fast.yaml",
				"head_sha": "head", "head_branch": pr["headRefName"], "status": "completed", "conclusion": conclusion,
				"repository": map[string]any{"full_name": "o/r"}, "head_repository": map[string]any{"full_name": "o/r"},
				"pull_requests": []any{map[string]any{"number": 1, "head": map[string]any{"sha": "head"}}},
			}
			var calls []string
			base := fakeGH([]map[string]any{pr}, &calls)
			gh := func(ctx context.Context, args ...string) ([]byte, error) {
				out, err := base(ctx, args...)
				call := strings.Join(args, " ")
				switch {
				case strings.HasPrefix(call, "run list"):
					if strings.Contains(call, "--event pull_request") && !strings.Contains(call, "--status action_required") {
						return []byte(`[{"databaseId":99,"status":"completed"}]`), nil
					}
					return []byte("[]"), nil
				case call == "api repos/o/r/actions/runs/99":
					return json.Marshal(run)
				case strings.HasPrefix(call, "pr view"):
					return json.Marshal(pr)
				}
				return out, err
			}
			decisions, err := Settle(context.Background(), gh, "o/r", "noise-review", 1, true, ciProof, ciProof)
			if err != nil || len(decisions) != 1 || decisions[0].Action != ActionWait {
				t.Fatalf("failed preflight did not become a per-PR wait: %+v, %v", decisions, err)
			}
			joined := strings.Join(calls, "\n")
			if !strings.Contains(joined, "run rerun 99 --repo o/r") {
				t.Fatalf("settle did not rerun in the validated repository: %s", joined)
			}
			if strings.Contains(joined, "workflow run ci-fast.yaml") {
				t.Fatalf("rerun also dispatched a duplicate workflow: %s", joined)
			}
		})
	}
}

func TestSettlePropagatesPreflightRerunFailure(t *testing.T) {
	pr := ciPR("head", "APPROVED", "head", "", "CLEAN")
	run := map[string]any{
		"id": 99, "event": "pull_request", "path": ".github/workflows/ci-fast.yaml",
		"head_sha": "head", "head_branch": pr["headRefName"], "status": "completed", "conclusion": "cancelled",
		"repository": map[string]any{"full_name": "o/r"}, "head_repository": map[string]any{"full_name": "o/r"},
		"pull_requests": []any{map[string]any{"number": 1, "head": map[string]any{"sha": "head"}}},
	}
	var calls []string
	base := fakeGH([]map[string]any{pr}, &calls)
	gh := func(ctx context.Context, args ...string) ([]byte, error) {
		out, err := base(ctx, args...)
		call := strings.Join(args, " ")
		switch {
		case strings.HasPrefix(call, "run list"):
			if strings.Contains(call, "--event pull_request") && !strings.Contains(call, "--status action_required") {
				return []byte(`[{"databaseId":99,"status":"completed"}]`), nil
			}
			return []byte("[]"), nil
		case call == "api repos/o/r/actions/runs/99":
			return json.Marshal(run)
		case strings.HasPrefix(call, "run rerun"):
			return nil, errors.New("rerun refused")
		case strings.HasPrefix(call, "pr view"):
			return json.Marshal(pr)
		}
		return out, err
	}
	if _, err := Settle(context.Background(), gh, "o/r", "noise-review", 1, true, ciProof, ciProof); err == nil || !strings.Contains(err.Error(), "rerun refused") {
		t.Fatalf("rerun failure was swallowed: %v", err)
	}
}
