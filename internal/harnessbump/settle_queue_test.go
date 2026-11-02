package harnessbump

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestSettleQueueGuards(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(map[string]any)
		want   Action
	}{
		{"queued behind", func(p map[string]any) { p["queued"] = true; p["mergeStateStatus"] = "BEHIND" }, ActionQueued},
		{"clean but behind", func(p map[string]any) { p["behind"] = 1 }, ActionUpdateBranch},
		{"old base approval", func(p map[string]any) { p["queueBase"] = "new-base" }, ActionReview},
		{"reassociated old review", func(p map[string]any) {
			p["reviews"].([]any)[1].(map[string]any)["body"] = "Reviewed head: old\nBase: base\n"
		}, ActionReview},
		{"missing review proof", func(p map[string]any) { p["reviews"].([]any)[1].(map[string]any)["body"] = "APPROVE" }, ActionReview},
		{"duplicate review proof", func(p map[string]any) {
			p["reviews"].([]any)[1].(map[string]any)["body"] = "Reviewed head: aaa\nBase: base\nBase: base\n"
		}, ActionReview},
		{"lost queue entry", func(p map[string]any) { p["queueIntent"] = true }, ActionHandOff},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := prJSON(1, "harness-bump/codex-1.2.3", "aaa", "APPROVED", "aaa", "SUCCESS", "CLEAN")
			tc.change(p)
			var calls []string
			always := func(context.Context, Harness, string, string) (bool, error) { return true, nil }
			got, err := Settle(context.Background(), fakeGH([]map[string]any{p}, &calls), "o/r", "noise-review", 1, false, always, always)
			if err != nil || len(got) != 1 || got[0].Action != tc.want || got[0].Applied {
				t.Fatalf("got %+v, err %v", got, err)
			}
			for _, call := range calls {
				if strings.Contains(call, "--method") || strings.Contains(call, "enqueuePullRequest") {
					t.Fatalf("dry decision mutated: %s", call)
				}
			}
		})
	}
}

func TestSettleLostQueueResponseDoesNotResubmit(t *testing.T) {
	p := prJSON(1, "harness-bump/codex-1.2.3", "aaa", "APPROVED", "aaa", "SUCCESS", "CLEAN")
	var calls []string
	underlying := fakeGH([]map[string]any{p}, &calls)
	writes := 0
	gh := func(ctx context.Context, args ...string) ([]byte, error) {
		raw, err := underlying(ctx, args...)
		if strings.Contains(strings.Join(args, " "), "enqueuePullRequest") {
			writes++
			return nil, errors.New("lost response")
		}
		return raw, err
	}
	always := func(context.Context, Harness, string, string) (bool, error) { return true, nil }
	if _, err := Settle(context.Background(), gh, "o/r", "noise-review", 1, true, always, always); err == nil {
		t.Fatal("lost response was hidden")
	}
	got, err := Settle(context.Background(), gh, "o/r", "noise-review", 1, true, always, always)
	if err != nil || len(got) != 1 || got[0].Action != ActionQueued || got[0].Applied || writes != 1 {
		t.Fatalf("got %+v, err %v, submissions %d", got, err, writes)
	}
	// If admission never happened or an entry was ejected, do not blindly retry.
	p["queued"] = false
	got, err = Settle(context.Background(), gh, "o/r", "noise-review", 1, true, always, always)
	if err != nil || len(got) != 1 || got[0].Action != ActionHandOff || writes != 1 {
		t.Fatalf("got %+v, err %v, submissions %d", got, err, writes)
	}
}

func TestEnqueueBumpRechecksImmutableRefs(t *testing.T) {
	for _, field := range []string{"headRefOid", "queueBase"} {
		t.Run(field, func(t *testing.T) {
			p := prJSON(1, "harness-bump/codex-1.2.3", "aaa", "APPROVED", "aaa", "SUCCESS", "CLEAN")
			var pr pullRequest
			raw, _ := json.Marshal(p)
			if err := json.Unmarshal(raw, &pr); err != nil {
				t.Fatal(err)
			}
			p[field] = "moved"
			var calls []string
			_, err := enqueueBump(context.Background(), fakeGH([]map[string]any{p}, &calls), "o/r", pr, "base")
			if err == nil {
				t.Fatal("moved refs admitted")
			}
			for _, call := range calls {
				if strings.Contains(call, "--method POST") || strings.Contains(call, "enqueuePullRequest") {
					t.Fatalf("moved refs mutated: %s", call)
				}
			}
		})
	}
}

func TestQueueMetadataFailsClosed(t *testing.T) {
	for _, raw := range []string{`{}`, `{"errors":[{"message":"denied"}]}`, `{"data":{"repository":{"pullRequest":{"id":"id","headRefOid":"head"}}}}`, `not json`} {
		gh := func(context.Context, ...string) ([]byte, error) { return []byte(raw), nil }
		if _, err := readBumpQueue(context.Background(), gh, "o/r", 1); err == nil {
			t.Errorf("accepted %s", raw)
		}
	}
}

func TestReviewAndSettlementBindTheLiveMainRef(t *testing.T) {
	head, base := strings.Repeat("a", 40), strings.Repeat("b", 40)
	p := prJSON(1, "harness-bump/codex-1.2.3", head, "APPROVED", head, "SUCCESS", "CLEAN")
	p["queueBase"] = base
	var calls []string
	gh := fakeGH([]map[string]any{p}, &calls)
	s, err := ReadReviewSnapshot(t.Context(), gh, "o/r", 1)
	if err != nil || s.Head != head || s.Base != base {
		t.Fatalf("snapshot %+v, error %v", s, err)
	}
	body := "Reviewed head: " + s.Head + "\nBase: " + s.Base
	if err := ValidateReviewBinding(Verdict{Body: body}, s.Head, s.Base); err != nil {
		t.Fatal(err)
	}
	p["reviews"].([]any)[1].(map[string]any)["body"] = body
	always := func(context.Context, Harness, string, string) (bool, error) { return true, nil }
	got, err := Settle(t.Context(), gh, "o/r", "noise-review", 1, false, always, always)
	if err != nil || len(got) != 1 || got[0].Action != ActionMerge {
		t.Fatalf("review did not converge: %+v %v", got, err)
	}
	p["queueBase"] = strings.Repeat("c", 40)
	newSnapshot, err := ReadReviewSnapshot(t.Context(), gh, "o/r", 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateReviewBinding(Verdict{Body: body}, newSnapshot.Head, newSnapshot.Base); err == nil {
		t.Fatal("post-time check admitted a moved base")
	}
}

func TestSettleRetainsPriorQueueDecisionOnLaterError(t *testing.T) {
	for _, stage := range []string{"proof", "threads"} {
		t.Run(stage, func(t *testing.T) {
			prs := []map[string]any{prJSON(1, "harness-bump/codex-1.2.3", "aaa", "APPROVED", "aaa", "SUCCESS", "CLEAN"), prJSON(2, "harness-bump/kimi-1.2.3", "bbb", "APPROVED", "bbb", "SUCCESS", "CLEAN")}
			var calls []string
			inner := fakeGH(prs, &calls)
			gh := func(ctx context.Context, args ...string) ([]byte, error) {
				joined := strings.Join(args, " ")
				if stage == "threads" && strings.Contains(joined, "reviewThreads") && strings.Contains(joined, "number=2") {
					return nil, errors.New("later API error")
				}
				return inner(ctx, args...)
			}
			proof := func(_ context.Context, _ Harness, _, head string) (bool, error) {
				if stage == "proof" && head == "bbb" {
					return false, errors.New("later proof error")
				}
				return true, nil
			}
			got, err := Settle(t.Context(), gh, "o/r", "noise-review", 0, true, proof, proof)
			if err == nil || len(got) != 1 || got[0].Action != ActionQueued || !got[0].Applied {
				t.Fatalf("lost successful queue result: %+v %v", got, err)
			}
		})
	}
}

func TestSettleReconcilesQueueTerminalStates(t *testing.T) {
	for _, tc := range []struct {
		name      string
		read      int
		state     string
		merge     string
		intent    bool
		queued    bool
		movedHead bool
		want      Action
		wantError bool
	}{
		{"merged after open list", 1, "MERGED", "merged-commit", true, false, false, ActionMerged, false},
		{"merged before enqueue", 2, "MERGED", "merged-commit", false, false, false, ActionMerged, false},
		{"closed after open list", 1, "CLOSED", "", false, false, false, ActionHandOff, false},
		{"closed before enqueue", 2, "CLOSED", "", false, false, false, ActionMerge, true},
		{"ejected from queue", 1, "OPEN", "", true, false, false, ActionHandOff, false},
		{"still queued", 1, "OPEN", "", true, true, false, ActionQueued, false},
		{"merged different head", 1, "MERGED", "merged-commit", true, false, true, ActionWait, false},
		{"different head before enqueue", 2, "MERGED", "merged-commit", false, false, true, ActionMerge, true},
		{"missing state", 1, "", "", true, false, false, "", true},
		{"unknown state", 1, "UNKNOWN", "", true, false, false, "", true},
		{"merged without commit", 1, "MERGED", "", true, false, false, "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := prJSON(1, "harness-bump/codex-1.2.3", "aaa", "APPROVED", "aaa", "SUCCESS", "CLEAN")
			var calls []string
			inner := fakeGH([]map[string]any{p}, &calls)
			reads := 0
			gh := func(ctx context.Context, args ...string) ([]byte, error) {
				joined := strings.Join(args, " ")
				if strings.Contains(joined, "query="+bumpQueueQuery) {
					reads++
					if reads == tc.read {
						p["state"], p["queueIntent"], p["queued"] = tc.state, tc.intent, tc.queued
						if tc.merge != "" {
							p["mergeCommit"] = map[string]any{"oid": tc.merge}
							p["queueBase"] = tc.merge
						}
						if tc.movedHead {
							p["headRefOid"] = "different-head"
						}
					}
				}
				return inner(ctx, args...)
			}
			always := func(context.Context, Harness, string, string) (bool, error) { return true, nil }
			got, err := Settle(t.Context(), gh, "o/r", "noise-review", 1, true, always, always)
			if (err != nil) != tc.wantError {
				t.Fatalf("decisions %+v, error %v", got, err)
			}
			if tc.want != "" && (len(got) != 1 || got[0].Action != tc.want || got[0].Applied) {
				t.Fatalf("decisions %+v, want %s without mutation", got, tc.want)
			}
			if tc.name == "closed after open list" && !strings.Contains(got[0].Reason, "closed without merging") {
				t.Fatalf("closed state was not reconciled: %+v", got)
			}
			for _, call := range calls {
				if strings.Contains(call, "--method") || strings.Contains(call, "mutation(") {
					t.Fatalf("terminal or uncertain state caused a mutation: %s", call)
				}
			}
		})
	}
}
