package deploy

import (
	"os"
	"strings"
	"testing"
)

func TestDockerfilePinsQoderCLI(t *testing.T) {
	raw, err := os.ReadFile("../Dockerfile")
	if err != nil {
		t.Fatalf("read Dockerfile: %v", err)
	}
	dockerfile := string(raw)
	for _, required := range []string{
		"ARG QODER_VERSION=1.1.78",
		`"@qoder-ai/qodercli@${QODER_VERSION}"`,
		`test "$(qoder --version)" = "${QODER_VERSION}"`,
		"qoder --help >/tmp/qoder-help",
		"--permission-mode",
		"--dangerously-skip-permissions",
		"--resume",
		"--session-id",
	} {
		if !strings.Contains(dockerfile, required) {
			t.Errorf("Dockerfile missing Qoder package contract %q", required)
		}
	}
	// The build-time checks must not bake first-run provider state into the
	// image: Qoder's postinstall writes ~/.qoder/entry, and --version/--help
	// write the ~/.qoder log tree and append to the global git ignore under
	// ~/.config/git, so install and probes run against a scratch home that is
	// deleted in the same layer.
	for _, required := range []string{
		`check=$(mktemp -d)`,
		`export HOME="$check"`,
		`npm_config_cache=/root/.npm npm install --global "@qoder-ai/qodercli@${QODER_VERSION}"`,
		`rm -rf "$check" /tmp/qoder-help`,
	} {
		if !strings.Contains(dockerfile, required) {
			t.Errorf("Dockerfile Qoder checks missing scratch-home isolation %q", required)
		}
	}
}
