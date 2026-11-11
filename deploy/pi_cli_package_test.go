package deploy

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestDockerfileInstallsPinnedPiHarness(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "Dockerfile"))
	if err != nil {
		t.Fatal(err)
	}
	body := string(raw)
	for _, required := range []string{
		"ARG PI_VERSION=1.5.2",
		`"@earendil-works/pi-coding-agent@${PI_VERSION}"`,
		"npm install --global --ignore-scripts",
		`test "$(pi --version)" = "${PI_VERSION}"`,
		`const ai = require("/usr/local/lib/node_modules/@earendil-works/pi-coding-agent/node_modules/@earendil-works/pi-ai/package.json")`,
		`ai.name !== "@earendil-works/pi-ai") process.exit(1)`,
		"PI_OFFLINE=1 ZAI_API_KEY=build-probe pi --list-models 'zai/glm-5.2'",
		`$1 == "zai" && $2 == "glm-5.2"`,
		"PI_SKIP_VERSION_CHECK=1",
		"PI_TELEMETRY=0",
		"--no-session",
		"--no-tools",
		"--no-context-files",
		`grep -Eq -- "(^|[[:space:]])${flag}([[:space:],]|$)" /tmp/pi-help || exit 1`,
	} {
		if !strings.Contains(body, required) {
			t.Errorf("Dockerfile is missing %q", required)
		}
	}
	if strings.Contains(body, "@mariozechner/pi-coding-agent") {
		t.Fatal("Dockerfile installs the deprecated Pi package")
	}
}

func TestPiDependencyProbeAllowsIndependentVersions(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is required to execute the image admission probe")
	}
	raw, err := os.ReadFile(filepath.Join("..", "Dockerfile"))
	if err != nil {
		t.Fatal(err)
	}
	probe := regexp.MustCompile(`node -e '(const ai = [^']+)'`).FindSubmatch(raw)
	if len(probe) != 2 {
		t.Fatal("Pi dependency admission probe not found")
	}
	for _, tc := range []struct {
		name, version string
		wantOK        bool
	}{
		{"@earendil-works/pi-ai", "1.0.4", true},
		{"@earendil-works/pi-ai", "1.1.0", true},
		{"@mariozechner/pi-ai", "1.0.4", false},
	} {
		t.Run(tc.name+"/"+tc.version, func(t *testing.T) {
			// Execute the actual Dockerfile probe with synthetic package metadata.
			out, err := exec.Command(node, "-e", `require("node:vm").runInNewContext(process.argv[1], {
require: () => ({name: process.argv[2], version: process.argv[3]}),
process: {exit: process.exit, argv: ["node", "1.0.4"]}, console
})`, string(probe[1]), tc.name, tc.version).CombinedOutput()
			if (err == nil) != tc.wantOK {
				t.Fatalf("probe success=%t, want %t: %s", err == nil, tc.wantOK, out)
			}
		})
	}
}

func TestPiCredentialInstructionsUsePersistentProviderHome(t *testing.T) {
	raw, err := os.ReadFile("compose.env.example")
	if err != nil {
		t.Fatal(err)
	}
	body := string(raw)
	for _, required := range []string{
		"~/.pi/agent",
		"ZAI Coding Plan (Global)",
		"~/.pi/agent/auth.json",
		"mode 0600",
		"Do not put the key in this Compose environment",
	} {
		if !strings.Contains(body, required) {
			t.Errorf("compose.env.example is missing %q", required)
		}
	}
	if strings.Contains(body, "ZAI_API_KEY=") {
		t.Fatal("compose.env.example must not request an ambient Z.ai credential")
	}
}
