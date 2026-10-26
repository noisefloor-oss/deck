package deploy

import (
	"os/exec"
	"testing"
)

func TestReviewRebaseEvidence(t *testing.T) {
	output, err := exec.Command("python3", "-B", "test_review_rebase.py").CombinedOutput()
	if err != nil {
		t.Fatalf("review rebase evidence: %v\n%s", err, output)
	}
}
