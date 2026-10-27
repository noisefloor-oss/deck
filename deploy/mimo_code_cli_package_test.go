package deploy

import (
	"os"
	"strings"
	"testing"
)

func TestDockerfilePinsMimoCodeCLI(t *testing.T) {
	raw, err := os.ReadFile("../Dockerfile")
	if err != nil {
		t.Fatalf("read Dockerfile: %v", err)
	}
	dockerfile := string(raw)
	for _, required := range []string{
		"ARG MIMO_VERSION=0.1.16",
		`"@mimo-ai/cli@${MIMO_VERSION}"`,
		`test "$(mimo --version)" = "${MIMO_VERSION}"`,
		"mimo --help >/tmp/mimo-help 2>&1",
		"--trust",
		"--yolo",
	} {
		if !strings.Contains(dockerfile, required) {
			t.Errorf("Dockerfile missing MiMo Code package contract %q", required)
		}
	}
	// The build-time checks must not bake first-run provider state into the
	// image: MiMo writes its config/data/state trees even for --version and
	// runs its one-time database migration for --help, so they run against a
	// scratch home that is deleted in the same layer.
	for _, required := range []string{
		`check=$(mktemp -d)`,
		`export HOME="$check"`,
		`rm -rf "$check" /tmp/mimo-help`,
	} {
		if !strings.Contains(dockerfile, required) {
			t.Errorf("Dockerfile MiMo Code checks missing scratch-home isolation %q", required)
		}
	}
}
