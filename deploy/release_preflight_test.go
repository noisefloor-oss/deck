package deploy

import (
	"os/exec"
	"testing"
)

func TestReleasePreflight(t *testing.T) {
	output, err := exec.Command("python3", "-B", "test_release_preflight.py").CombinedOutput()
	if err != nil {
		t.Fatalf("release preflight: %v\n%s", err, output)
	}
}
