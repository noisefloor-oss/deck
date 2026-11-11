package derived

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestGoRecipeIsPinnedDerivedImage(t *testing.T) {
	raw, err := os.ReadFile("go.Dockerfile")
	if err != nil {
		t.Fatal(err)
	}
	recipe := string(raw)

	for _, required := range []string{
		"# syntax=docker/dockerfile:1.7@sha256:",
		"FROM golang:1.26.11-bookworm@sha256:",
		"FROM deck-base AS runtime",
		"COPY --from=go-toolchain --chown=root:root /usr/local/go /usr/local/go",
		"GOTOOLCHAIN=local",
		"COPY --chmod=0444 deploy/derived/go-profile.sh /etc/profile.d/noisedeck-go.sh",
		"chown -R root:root /usr/local/go",
		"chmod -R go-w /usr/local/go",
		"go version /usr/local/go/bin/gofmt",
		"test ! -w /usr/local/go/bin/go",
		"CGO_ENABLED=1 go build",
	} {
		if !strings.Contains(recipe, required) {
			t.Errorf("Go recipe is missing %q", required)
		}
	}

	for _, forbidden := range []string{
		"go.dev/dl/",
		"curl |",
		"/workspace/tools",
		"deck:slim",
		"ARG DECK_IMAGE",
		"gofmt --version",
	} {
		if strings.Contains(recipe, forbidden) {
			t.Errorf("Go recipe contains forbidden mutable or incomplete source %q", forbidden)
		}
	}

	last := ""
	for _, line := range strings.Split(recipe, "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) >= 2 && strings.EqualFold(fields[0], "USER") {
			last = strings.Join(fields[1:], " ")
		}
	}
	if last != "${NOISEDECK_RUNTIME_UID}:${NOISEDECK_RUNTIME_GID}" {
		t.Fatalf("Go recipe's final USER = %q, want the runtime user", last)
	}
}

func TestGoProfileRestoresPathOnce(t *testing.T) {
	command := `PATH=/home/agent/.local/bin:/usr/local/bin:/usr/bin:/bin; ` +
		`. "$1"; . "$1"; printf '%s' "$PATH"`
	out, err := exec.Command("/bin/bash", "-c", command, "test", "go-profile.sh").Output()
	if err != nil {
		t.Fatalf("source Go profile: %v", err)
	}
	if got := string(out); got != "/usr/local/go/bin:/home/agent/.local/bin:/usr/local/bin:/usr/bin:/bin" {
		t.Fatalf("PATH after sourcing twice = %q", got)
	}
}

func TestGoComposeOverlaySelectsOnlyDerivedBuild(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "compose.go.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	overlay := string(raw)
	for _, required := range []string{
		"dockerfile: deploy/derived/go.Dockerfile",
		"target: runtime",
		"deck-base: ${NOISEDECK_DERIVED_BASE_CONTEXT:-service:go-base}",
	} {
		if !strings.Contains(overlay, required) {
			t.Errorf("Go Compose overlay is missing %q", required)
		}
	}
	for _, forbidden := range []string{"volumes:", "environment:", "browser", "/workspace/tools"} {
		if strings.Contains(overlay, forbidden) {
			t.Errorf("Go Compose overlay changes runtime topology with %q", forbidden)
		}
	}
}

func TestGoMemPalaceOverlayBuildsBaseInSameGraph(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "compose.go-mempalace.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	overlay := string(raw)
	for _, required := range []string{
		"go-base:",
		"target: mempalace",
		"scale: 0",
		"deck-base: service:go-base",
	} {
		if !strings.Contains(overlay, required) {
			t.Errorf("Go + MemPalace Compose overlay is missing %q", required)
		}
	}
	for _, forbidden := range []string{"image:", "NOISEDECK_DERIVED_BASE_IMAGE", "/workspace/tools"} {
		if strings.Contains(overlay, forbidden) {
			t.Errorf("Go + MemPalace overlay relies on an intermediate image with %q", forbidden)
		}
	}
}

func TestDeckDistributionsStillShipNoGoToolchain(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "Dockerfile"))
	if err != nil {
		t.Fatal(err)
	}
	dockerfile := string(raw)
	for _, forbidden := range []string{
		"FROM go-toolchain",
		"COPY --from=go-toolchain",
		"/etc/profile.d/noisedeck-go.sh",
	} {
		if strings.Contains(dockerfile, forbidden) {
			t.Errorf("Deck's ordinary distributions contain %q; Go belongs in the derived image", forbidden)
		}
	}
}
