package deploy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDockerfileInstallsPinnedKiloCLI(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "Dockerfile"))
	if err != nil {
		t.Fatal(err)
	}
	body := string(raw)
	for _, required := range []string{
		"ARG KILO_VERSION=7.8.18",
		"RUN --mount=type=cache,target=/root/.npm,sharing=locked",
		`"@kilocode/cli@${KILO_VERSION}"`,
		`test "$(kilo --version)" = "${KILO_VERSION}"`,
		"kilo --help >/tmp/kilo-help 2>&1",
		"grep -q -- '--session' /tmp/kilo-help",
		"grep -q -- '--auto' /tmp/kilo-help",
	} {
		if !strings.Contains(body, required) {
			t.Fatalf("Dockerfile missing pinned Kilo CLI marker %q", required)
		}
	}
	installStart := strings.Index(body, "RUN --mount=type=cache,target=/root/.npm,sharing=locked")
	installEnd := strings.Index(body[installStart:], "# omp is a bun-based retrieved-identity provider")
	if installStart < 0 || installEnd < 0 {
		t.Fatal("Dockerfile missing bounded npm install block")
	}
	if strings.Contains(body[installStart:installStart+installEnd], "rm -rf /root/.npm") {
		t.Fatal("Dockerfile races Kilo background activity by deleting its mounted npm cache")
	}
}
