package deploy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Both bootstraps hold everything that writes behind `noise deck
// sealed-check -gate` with serve's own arguments, so the gate and serve
// read one configuration and every spelling of the requirement gates the
// same way. The gate runs before provisioning, MemPalace, the courier and
// serve, and is not conditional on a shell reading of the environment.
func TestBootstrapsGateOnSealedCheckBeforeAnythingWrites(t *testing.T) {
	for _, path := range []string{
		filepath.Join("provider-hook", "noisedeck-bootstrap"),
		filepath.Join("mempalace", "noisedeck-bootstrap"),
	} {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		script := string(raw)
		gate := strings.Index(script, `noise deck sealed-check -gate "$@"`)
		if gate < 0 {
			t.Fatalf("%s does not run sealed-check -gate with serve's arguments", path)
		}
		for _, later := range []string{"/usr/local/bin/noisedeck-provision;", "/opt/mempalace/venv/bin/mempalace-mcp", "/usr/local/bin/a2a courier", "exec /usr/local/bin/noise deck serve"} {
			at := strings.Index(script, later)
			if at >= 0 && at < gate {
				t.Errorf("%s starts %q before the sealed-check gate", path, later)
			}
		}
		if strings.Contains(script, `NOISEDECK_SEALED_REQUIRE_ENCRYPTED_STATE:-false}" = "true"`) {
			t.Errorf("%s decides the requirement in shell instead of through the config the gate loads", path)
		}
		if !strings.Contains(script[gate:], "exit 78") {
			t.Errorf("%s does not refuse when the gate fails", path)
		}
	}
}
