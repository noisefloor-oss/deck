package deploy

import (
	"os"
	"strings"
	"testing"
)

func TestDockerfilePinsGrokCLI(t *testing.T) {
	raw, err := os.ReadFile("../Dockerfile")
	if err != nil {
		t.Fatalf("read Dockerfile: %v", err)
	}
	dockerfile := string(raw)
	for _, required := range []string{
		"ARG GROK_VERSION=1.0.70",
		`test "$(grok --version | awk '{print $2}')" = "${GROK_VERSION}"`,
		"grok --help >/tmp/grok-help",
		"--always-approve",
		"--resume",
		"--session-id",
	} {
		if !strings.Contains(dockerfile, required) {
			t.Errorf("Dockerfile missing Grok package contract %q", required)
		}
	}
	// The build-time install and checks must not bake first-run provider
	// state into the image: the package's postinstall bootstraps its binary
	// into ~/.grok/bin and --version/--help write the ~/.grok state trees,
	// so install and probes run against a scratch home that is deleted in
	// the same layer.
	for _, required := range []string{
		`check=$(mktemp -d)`,
		`export HOME="$check"`,
		`rm -rf "$check" /tmp/grok-help`,
	} {
		if !strings.Contains(dockerfile, required) {
			t.Errorf("Dockerfile Grok checks missing scratch-home isolation %q", required)
		}
	}
}
