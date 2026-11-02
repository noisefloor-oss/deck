package harnessbump

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestWaitForSettlementObservesGreenHeadWithoutAnotherEvent(t *testing.T) {
	id := strings.Repeat("a", 64)
	reads := 0
	var calls, proven []string
	decisions, err := WaitForSettlement(t.Context(), time.Millisecond, func(ctx context.Context) ([]Decision, error) {
		reads++
		head, gate := "old", "PENDING"
		if reads == 2 {
			head, gate = "new", "SUCCESS"
		}
		pr := prJSON(42, BranchPrefix+"all-"+id, head, "", "", gate, "CLEAN")
		pr["title"] = "chore(provider): pin all " + id
		proof := func(_ context.Context, _ Harness, _, head string) (bool, error) {
			proven = append(proven, head)
			return true, nil
		}
		return Settle(ctx, fakeGH([]map[string]any{pr}, &calls), "o/r", "noise-review[bot]", 42, true, proof, proof)
	})
	if err != nil || reads != 2 || len(decisions) != 1 || decisions[0].Action != ActionReview || decisions[0].Head != "new" {
		t.Fatalf("reads=%d decisions=%+v err=%v", reads, decisions, err)
	}
	if strings.Join(proven, ",") != "old,old,new,new" || strings.Contains(strings.Join(calls, "\n"), "pr merge") {
		t.Fatalf("admission or merge checks bypassed: proven=%v calls=%v", proven, calls)
	}
}

func TestWaitForSettlementContinuesAfterBranchUpdate(t *testing.T) {
	steps := []Decision{
		{Action: ActionUpdateBranch, Applied: true, Head: "old"},
		{Action: ActionWait, Head: "new"},
		{Action: ActionReview, Head: "new"},
	}
	reads := 0
	got, err := WaitForSettlement(t.Context(), time.Millisecond, func(context.Context) ([]Decision, error) {
		d := steps[reads]
		reads++
		return []Decision{d}, nil
	})
	if err != nil || reads != 3 || got[0].Head != "new" || got[0].Action != ActionReview {
		t.Fatalf("reads=%d decisions=%+v err=%v", reads, got, err)
	}
}

func TestWaitForSettlementStopsForTerminalDecisions(t *testing.T) {
	for _, action := range []Action{ActionBlocked, ActionChangesRequested, ActionHandOff, ActionIgnored, ActionMerge, ActionQueued, ActionMerged} {
		t.Run(string(action), func(t *testing.T) {
			reads := 0
			_, err := WaitForSettlement(t.Context(), time.Hour, func(context.Context) ([]Decision, error) {
				reads++
				return []Decision{{Action: action}}, nil
			})
			if err != nil || reads != 1 {
				t.Fatalf("reads=%d err=%v", reads, err)
			}
		})
	}
}

func TestWaitForSettlementDoesNotDelayReadyReviews(t *testing.T) {
	_, err := WaitForSettlement(t.Context(), time.Hour, func(context.Context) ([]Decision, error) {
		return []Decision{{Action: ActionWait}, {Action: ActionReview}}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestWaitForSettlementFailsVisiblyAtDeadline(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	_, err := WaitForSettlement(ctx, time.Hour, func(context.Context) ([]Decision, error) {
		return []Decision{{Number: 1053, Head: "pending-head", Action: ActionWait}}, nil
	})
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "1053") || !strings.Contains(err.Error(), "pending-head") {
		t.Fatalf("deadline must fail with the pending head: %v", err)
	}
}

func TestWaitForSettlementPropagatesAPIFailure(t *testing.T) {
	want := errors.New("GitHub unavailable")
	_, err := WaitForSettlement(t.Context(), time.Hour, func(context.Context) ([]Decision, error) {
		return nil, want
	})
	if !errors.Is(err, want) {
		t.Fatalf("got %v", err)
	}
}
