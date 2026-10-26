package deploy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDockerfileInstallsPinnedNoiseSdlcPlugin(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "Dockerfile"))
	if err != nil {
		t.Fatal(err)
	}
	body := string(raw)
	for _, required := range []string{
		"ARG NOISE_SDLC_REF=abadbafa9f8a8fd3f41013efeb57e8ae55ecc9e2",
		"ARG NOISE_SDLC_VERSION=0.4.1",
		"https://github.com/noisefloor-oss/sdlc.git",
		`test "$(git -C /src/noise-sdlc rev-parse HEAD)" = "${NOISE_SDLC_REF}"`,
		"CGO_ENABLED=0 go build -trimpath",
		"-o /out/usr/local/libexec/noise/noise-sdlc ./cmd/noise-sdlc",
		`test "$(/out/usr/local/libexec/noise/noise-sdlc --version)" = "${NOISE_SDLC_VERSION}"`,
		"COPY --from=noise-sdlc-build --chmod=0555 /out/usr/local/libexec/noise/noise-sdlc /usr/local/libexec/noise/noise-sdlc",
		`test "$(noise sdlc --version)" = "${NOISE_SDLC_VERSION}"`,
		`printf 'sdlc\ntasks\nvisual-diff'`,
		`printf 'deck\nsdlc\ntasks\nvisual-diff'`,
	} {
		if !strings.Contains(body, required) {
			t.Errorf("Dockerfile is missing pinned noise-sdlc plugin marker %q", required)
		}
	}
}

func TestDockerfileFetchesSdlcAnonymouslyWithoutToken(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "Dockerfile"))
	if err != nil {
		t.Fatal(err)
	}
	body := string(raw)
	for _, required := range []string{
		`sdlc_git_config="$(mktemp)"`,
		`GIT_CONFIG_GLOBAL="${sdlc_git_config}"`,
		`git clone --no-checkout "${NOISE_SDLC_REPO}" /src/noise-sdlc`,
		`rm -f "${sdlc_git_config}"`,
	} {
		if !strings.Contains(body, required) {
			t.Errorf("Dockerfile is missing scoped SDLC fetch behavior %q", required)
		}
	}
}
