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
		"ARG DEVIN_VERSION=3000.14.30821",
		"ARG DEVIN_SHA256_AMD64=4dc43e3e4388bace60fe2aa8010cbb399402e724892d7175845b1cdef00e7ddb",
		"ARG DEVIN_SHA256_ARM64=3deb8af6b026060e975ed371936e139818020e5dc5abbcba07cde039f473a5a5",
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
