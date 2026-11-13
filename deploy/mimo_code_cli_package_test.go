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
		"ARG MIMO_VERSION=0.1.17",
		"ARG MIMO_SHA256_AMD64=92b0d0e5e7cc93691a072aa34a6496d01799e84ae0f13f369cdc14a07a35d66a",
		"ARG MIMO_SHA256_ARM64=c7685012abae6ab63e1b38ba52666ea8bac4a0fd33e2b86bd8ea2de726b80a43",
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
