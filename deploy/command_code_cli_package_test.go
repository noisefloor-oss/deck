package deploy

import (
	"os"
	"strings"
	"testing"
)

func TestDockerfilePinsCommandCodeCLI(t *testing.T) {
	raw, err := os.ReadFile("../Dockerfile")
	if err != nil {
		t.Fatalf("read Dockerfile: %v", err)
	}
	dockerfile := string(raw)
	for _, required := range []string{
		"ARG COMMAND_CODE_VERSION=1.88.0",
		`"command-code@${COMMAND_CODE_VERSION}"`,
		`test "$(command-code --no-auto-update --version)" = "${COMMAND_CODE_VERSION}"`,
		"command-code --no-auto-update --help",
		"--no-auto-update",
		"--session",
		"--resume",
		"--skip-onboarding",
		"--yolo",
	} {
		if !strings.Contains(dockerfile, required) {
			t.Errorf("Dockerfile missing Command Code package contract %q", required)
		}
	}
}
