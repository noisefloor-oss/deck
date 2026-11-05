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
		"ARG CURSOR_AGENT_VERSION=2026.11.05-86047df",
		"ARG CURSOR_AGENT_SHA256_AMD64=170b77786cbf4c93064d64fff12b71a178b968730561c64afc2e8e752535442e",
		"ARG CURSOR_AGENT_SHA256_ARM64=131554bfdbf683d8ebfef83a2bb46ac4e5d060f198711040b07ed063c2471871",
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
