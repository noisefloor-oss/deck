package deploy

import (
	"os"
	"strings"
	"testing"
)

func TestDockerfilePinsQwenCodeCLI(t *testing.T) {
	raw, err := os.ReadFile("../Dockerfile")
	if err != nil {
		t.Fatalf("read Dockerfile: %v", err)
	}
	dockerfile := string(raw)
	for _, required := range []string{
		"ARG QWEN_VERSION=0.26.1",
		`"@qwen-code/qwen-code@${QWEN_VERSION}"`,
		`test "$(qwen --version)" = "${QWEN_VERSION}"`,
		"qwen probe --help",
		"--chat-recording",
		"--yolo",
		// Strict-parse guard: a flag the invoked command does not accept
		// must fail the build here (measured: exit 1), proving the build
		// does not verify flags through argv that --help would render for
		// regardless.
		"! qwen --approval-mode yolo sessions list",
	} {
		if !strings.Contains(dockerfile, required) {
			t.Errorf("Dockerfile missing Qwen Code package contract %q", required)
		}
	}
}
