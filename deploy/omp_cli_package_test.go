package deploy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDockerfileInstallsPinnedOmpCLI(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "Dockerfile"))
	if err != nil {
		t.Fatal(err)
	}
	body := string(raw)
	for _, required := range []string{
		"ARG BUN_VERSION=1.3.14",
		"ARG OMP_VERSION=18.13.0",
		"unzip",
		"export BUN_INSTALL=/usr/local",
		`curl -fsSL https://bun.sh/install | bash -s "bun-v${BUN_VERSION}"`,
		`test "$(bun --version)" = "${BUN_VERSION}"`,
		`bun install --global "@oh-my-pi/pi-coding-agent@${OMP_VERSION}"`,
		`omp --version | grep -q "${OMP_VERSION}"`,
		"rm -rf /usr/local/install/cache /usr/local/.bun/install/cache",
	} {
		if !strings.Contains(body, required) {
			t.Errorf("Dockerfile is missing %q", required)
		}
	}
}
