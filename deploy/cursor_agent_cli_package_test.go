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
		"ARG CURSOR_AGENT_VERSION=2026.11.22-e9042b2",
		"ARG CURSOR_AGENT_SHA256_AMD64=7663b1c5d1b56e095ef8e23bef38d66df6cfd665c4873470214e0e411f0f97b3",
		"ARG CURSOR_AGENT_SHA256_ARM64=7b7dd0020fb9a2a9e1e7e3f39e2f9f512d1dd9577174ecda8aa82ac31dbc9565",
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
