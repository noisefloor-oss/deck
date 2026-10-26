package provider

import (
	"os"
	"os/exec"
	"testing"
)

func TestPiConversationHook(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node unavailable")
	}
	cmd := exec.Command(node, "--test", "assets/conversation-transport.test.mjs")
	cmd.Env = os.Environ()
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("Pi lifecycle contract: %v\n%s", err, output)
	}
}
