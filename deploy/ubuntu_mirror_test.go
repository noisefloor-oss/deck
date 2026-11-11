package deploy

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestUbuntuMirrorPreservesArchivePolicy(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "sources.list.d")
	if err := os.Mkdir(dir, 0755); err != nil {
		t.Fatal(err)
	}
	fixtures := map[string]string{
		"apt-mirrors.txt":               "http://azure.archive.ubuntu.com/ubuntu/\nhttps://archive.ubuntu.com/ubuntu/\n",
		"apt-mirrors-security.txt":      "http://security.ubuntu.com/ubuntu/\n",
		"sources.list":                  "deb [signed-by=/usr/share/keyrings/ubuntu-archive-keyring.gpg] http://archive.ubuntu.com/ubuntu noble main\n",
		"sources.list.d/ubuntu.sources": "Types: deb\nURIs: http://azure.archive.ubuntu.com/ubuntu/ https://security.ubuntu.com/ubuntu/\nSuites: noble noble-updates noble-security\nComponents: main universe\nSigned-By: /usr/share/keyrings/ubuntu-archive-keyring.gpg\n",
		"sources.list.d/ports.list":     "deb http://ports.ubuntu.com/ubuntu-ports noble main restricted\n",
		"sources.list.d/other.list":     "deb https://deb.nodesource.com/node_24.x nodistro main\ndeb http://archive.ubuntu.com/ubuntu-other noble main\n",
	}
	for name, content := range fixtures {
		if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}
	for attempt := 0; attempt < 2; attempt++ {
		if out, err := exec.Command("sh", "ubuntu-mirror.sh", root).CombinedOutput(); err != nil {
			t.Fatalf("configure mirror: %v: %s", err, out)
		}
		for name, original := range fixtures {
			got, err := os.ReadFile(filepath.Join(root, name))
			if err != nil {
				t.Fatal(err)
			}
			want := original
			if name != "sources.list.d/other.list" {
				for _, host := range []string{"http://archive.ubuntu.com", "http://azure.archive.ubuntu.com", "https://security.ubuntu.com", "http://ports.ubuntu.com", "https://archive.ubuntu.com", "http://security.ubuntu.com"} {
					want = strings.ReplaceAll(want, host, "https://mirrors.mit.edu")
				}
			}
			if string(got) != want {
				t.Errorf("%s: got %q, want %q", name, got, want)
			}
		}
	}
}
