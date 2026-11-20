package deploy

import (
	"os"
	"strings"
	"testing"
)

func TestDockerfilePinsCursorAgentCLI(t *testing.T) {
	raw, err := os.ReadFile("../Dockerfile")
	if err != nil {
		t.Fatalf("read Dockerfile: %v", err)
	}
	dockerfile := string(raw)
	for _, required := range []string{
		"ARG CURSOR_AGENT_VERSION=2026.11.19-f005e9d",
		"ARG CURSOR_AGENT_SHA256_AMD64=a25986eaf0c9330aeeec599a139711cb9f267c64562d073a72b751edf5161695",
		"ARG CURSOR_AGENT_SHA256_ARM64=5ff05be0d7c0b7754574dbcd677c35254a3ff190ed6457e71bbee0f276237e29",
		"https://downloads.cursor.com/lab/${CURSOR_AGENT_VERSION}/linux/${cursor_arch}/agent-cli-package.tar.gz",
		`test "$(cursor-agent --version)" = "${CURSOR_AGENT_VERSION}"`,
		"cursor-agent --help >/tmp/cursor-agent-help",
		"--trust",
		"--yolo",
		"--resume",
		"ln -s \"/opt/cursor-agent/${CURSOR_AGENT_VERSION}/dist-package/cursor-agent\" /usr/local/bin/cursor-agent",
	} {
		if !strings.Contains(dockerfile, required) {
			t.Errorf("Dockerfile missing Cursor Agent package contract %q", required)
		}
	}
	// The build-time checks must not bake first-run provider state into the
	// image: cursor-agent writes ~/.cursor/cli-config.json and populates the
	// Node compile cache under the cache home even for --version and --help,
	// so the probes run against a scratch home that is deleted in the same
	// layer.
	for _, required := range []string{
		`check=$(mktemp -d)`,
		`export HOME="$check"`,
		`rm -rf "$check" /tmp/cursor-agent-help`,
	} {
		if !strings.Contains(dockerfile, required) {
			t.Errorf("Dockerfile Cursor Agent checks missing scratch-home isolation %q", required)
		}
	}
}
