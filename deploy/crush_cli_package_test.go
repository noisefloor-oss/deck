package deploy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDockerfileInstallsPinnedCrushCLI(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "Dockerfile"))
	if err != nil {
		t.Fatal(err)
	}
	body := string(raw)
	for _, required := range []string{
		"ARG CRUSH_VERSION=0.98.2",
		`"@charmland/crush@${CRUSH_VERSION}"`,
		`test "$(crush --version)" = "crush version v${CRUSH_VERSION}"`,
	} {
		if !strings.Contains(body, required) {
			t.Fatalf("Dockerfile missing pinned crush CLI marker %q", required)
		}
	}
}
