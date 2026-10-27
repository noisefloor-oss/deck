package deploy

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

func TestMemPalaceAcceptanceProbePreservesPythonString(t *testing.T) {
	raw, err := os.ReadFile("../.github/workflows/release-acceptance.yaml")
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if !strings.Contains(line, `find_spec(\"chromadb\")`) {
			continue
		}
		line = strings.ReplaceAll(line, "/opt/mempalace/venv/bin/python", "python3")
		if output, err := exec.Command("sh", "-c", line).CombinedOutput(); err != nil {
			t.Fatalf("rendered ChromaDB absence probe failed: %v: %s", err, output)
		}
		return
	}
	t.Fatal("release acceptance has no ChromaDB absence probe")
}
