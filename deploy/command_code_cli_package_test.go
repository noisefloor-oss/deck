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
		"ARG COMMAND_CODE_SEMCONV_VERSION=1.43.0",
		`npm install --prefix /opt/npm-harnesses/lib/command-code`,
		`"@opentelemetry/semantic-conventions@${COMMAND_CODE_SEMCONV_VERSION}"`,
		`for command_code_bin in command-code cmd cmdc commandcode; do`,
		`ln -s "../lib/command-code/node_modules/.bin/${command_code_bin}"`,
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
