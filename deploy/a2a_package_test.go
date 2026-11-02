package deploy

import (
	"os"
	"strings"
	"testing"
)

func TestA2APackagePinsStandaloneSource(t *testing.T) {
	raw, err := os.ReadFile("../Dockerfile")
	if err != nil {
		t.Fatal(err)
	}
	body := string(raw)
	for _, required := range []string{
		"ARG NOISE_A2A_REF=4ebda82d51ae5c4c185c3a49f05dbb3074f01dc4",
		"ARG NOISE_A2A_VERSION=0.1.0",
		"ARG NOISE_A2A_REPO=https://github.com/noisefloor-oss/a2a.git",
		`git clone --no-checkout "${NOISE_A2A_REPO}" /src/a2a`,
		`test "$(git -C /src/a2a rev-parse HEAD)" = "${NOISE_A2A_REF}"`,
		"COPY --from=a2a-build /out/a2a /usr/local/bin/a2a",
		`test "$(a2a --version)" = "a2a ${NOISE_A2A_VERSION}"`,
	} {
		if !strings.Contains(body, required) {
			t.Errorf("Dockerfile is missing standalone a2a pin contract %q", required)
		}
	}
	for _, obsolete := range []string{
		"COPY deploy/a2a/a2a.py",
		"deploy/a2a/test_a2a.py",
	} {
		if strings.Contains(body, obsolete) {
			t.Errorf("Dockerfile retains bundled a2a source %q", obsolete)
		}
	}
	if _, err := os.Stat("a2a/a2a.py"); !os.IsNotExist(err) {
		t.Errorf("bundled a2a client remains: %v", err)
	}
	if _, err := os.Stat("a2a/test_a2a.py"); !os.IsNotExist(err) {
		t.Errorf("duplicated a2a suite remains: %v", err)
	}
}
