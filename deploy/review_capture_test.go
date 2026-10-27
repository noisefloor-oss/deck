package deploy

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestReviewCaptureContract(t *testing.T) {
	for _, mode := range []string{"body", "body-file", "findings", "empty", "unknown", "conflict", "missing-value", "invalid-event", "wrong-head", "bad-findings", "compose-failure", "validation-failure", "oversized-bytes", "oversized-envelope", "multiline-model"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			head, base := strings.Repeat("1", 40), strings.Repeat("2", 40)
			content := "🔵 [info] synthetic review finding"
			if mode == "oversized-bytes" {
				content = strings.Repeat("⬡", 22000)
			}
			if mode == "oversized-envelope" {
				content = strings.Repeat("x", 65460)
			}
			findings := filepath.Join(dir, "findings")
			if err := os.WriteFile(findings, []byte(content+"\n"), 0600); err != nil {
				t.Fatal(err)
			}
			// The review tool owns vocabulary and envelope validation. This fixture
			// verifies delegation and error propagation, without duplicating its rules.
			lib := `contains_secret() { return 1; }
_crb_parse_findings() { [ "$MODE" != bad-findings ] || return 1; cat; }
_crb_compose_body() { [ "$MODE" != compose-failure ] || return 1; printf 'tool-envelope:%s:%s:%s\n%s' "$1" "$2" "$3" "$6"; }
_crb_validate_composed_body() { [ "$MODE" != validation-failure ]; }
`
			if err := os.WriteFile(filepath.Join(dir, "review.sh"), []byte(lib), 0600); err != nil {
				t.Fatal(err)
			}
			args := []string{"../scripts/harness-bump/review-capture.sh", "review", "--repo", "acme/deck", "--pr", "7", "--event", "APPROVE", "--expect-head", head}
			switch mode {
			case "body":
				args = append(args, "--body", content)
			case "body-file":
				args = append(args, "--body-file", findings)
			case "empty":
				args = append(args, "--body", "")
			case "unknown":
				args = append(args, "--unknown", findings)
			case "conflict":
				args = append(args, "--body", content, "--findings", findings)
			case "missing-value":
				args = append(args, "--findings")
			case "invalid-event":
				args = append(args, "--event", "PASS", "--findings", findings)
			case "multiline-model":
				args = append(args, "--reviewer-model", "model\nforged", "--findings", findings)
			case "wrong-head":
				args = append(args, "--expect-head", base, "--findings", findings)
			default:
				args = append(args, "--findings", findings)
			}
			capture := filepath.Join(dir, "capture")
			cmd := exec.Command("bash", args...)
			cmd.Env = append(os.Environ(), "JAIL_CAPTURE_PATH="+capture, "JAIL_REVIEW_LIB="+dir, "JAIL_HEAD="+head, "JAIL_BASE="+base, "JAIL_REPO=acme/deck", "JAIL_PR=7", "MODE="+mode)
			out, err := cmd.CombinedOutput()
			got, readErr := os.ReadFile(capture)
			valid := mode == "body" || mode == "body-file" || mode == "findings"
			if !valid {
				if err == nil || !os.IsNotExist(readErr) || strings.Contains(string(out), `"captured":true`) {
					t.Fatalf("invalid review accepted: %v %s %s", err, out, got)
				}
				return
			}
			want := "APPROVE\n" + content + "\n"
			if mode == "findings" {
				want = "APPROVE\ntool-envelope:APPROVE:" + head + ":" + base + "\n" + content + "\n"
			}
			if err != nil || readErr != nil || string(got) != want {
				t.Fatalf("capture failed: %v %v %s; got %q want %q", err, readErr, out, got, want)
			}
		})
	}
}
