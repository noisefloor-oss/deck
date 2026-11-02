package deploy

import (
	"os/exec"
	"path/filepath"
	"testing"
)

func TestA2APiHook(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is unavailable")
	}
	command := exec.Command(node, "--test", "test_a2a_pi_hook.mjs")
	command.Dir = filepath.Join("mempalace")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("a2a Pi hook tests failed: %v\n%s", err, output)
	}
}

func TestA2AOpencodeHook(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is unavailable")
	}
	command := exec.Command(node, "--test", "test_a2a_opencode_hook.mjs")
	command.Dir = filepath.Join("mempalace")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("a2a OpenCode hook tests failed: %v\n%s", err, output)
	}
}
