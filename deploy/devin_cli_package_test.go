package deploy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDockerfileInstallsPinnedDevinCLI(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "Dockerfile"))
	if err != nil {
		t.Fatal(err)
	}
	body := string(raw)
	for _, required := range []string{
		"ARG DEVIN_VERSION=3000.12.43",
		"ARG DEVIN_SHA256_AMD64=6a19997b5fc2d6ba1a8c341d4e2b12f1b78cad5d415ee9c0ca413a8d52a99562",
		"ARG DEVIN_SHA256_ARM64=1eaa07d2181ed26f5789cf6b3617d39a2459c25f1bc5a51cc5cc703db7f95274",
		"https://static.devin.ai/cli/${DEVIN_VERSION}/devin-${DEVIN_VERSION}-${devin_arch}-unknown-linux.tar.gz",
		`echo "${devin_sha256}  /tmp/devin.tar.gz" | sha256sum -c -`,
		`tar --no-same-owner -xzf /tmp/devin.tar.gz`,
		`ln -s "/opt/devin/${DEVIN_VERSION}/bin/devin" /usr/local/bin/devin`,
		`test "$(devin --version | awk '{print $2}')" = "${DEVIN_VERSION}"`,
		`grep -Eq -- "(^|[[:space:]])${flag}([[:space:],]|$)" /tmp/devin-help || exit 1`,
		"--resume",
		"--model",
		"--print",
		"--permission-mode",
		"--respect-workspace-trust",
	} {
		if !strings.Contains(body, required) {
			t.Errorf("Dockerfile is missing %q", required)
		}
	}
	if strings.Contains(body, "https://cli.devin.ai/install.sh") {
		t.Fatal("Dockerfile follows Devin's moving installer instead of the pinned artifact")
	}
}

func TestComposeExampleDocumentsDevinUserAuth(t *testing.T) {
	raw, err := os.ReadFile("compose.env.example")
	if err != nil {
		t.Fatal(err)
	}
	body := string(raw)
	for _, required := range []string{
		"devin auth login",
		"~/.local/share/devin/credentials.toml",
		`"attribution": false`,
		`"auto_update": false`,
		"~/.config/devin/config.json",
	} {
		if !strings.Contains(body, required) {
			t.Errorf("compose.env.example is missing %q", required)
		}
	}
}
