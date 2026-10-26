package deploy

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestSidecarResolverOverlays(t *testing.T) {
	for file, service := range map[string]string{"compose.browser.yaml": "chromium", "compose.preview-tunnel.yaml": "preview-tunnel"} {
		t.Run(service, func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join("..", file))
			if err != nil {
				t.Fatal(err)
			}
			var document struct {
				Services map[string]struct {
					Volumes []yaml.Node `yaml:"volumes"`
				} `yaml:"services"`
			}
			if err := yaml.Unmarshal(raw, &document); err != nil {
				t.Fatal(err)
			}
			count := 0
			for _, node := range document.Services[service].Volumes {
				if node.Kind != yaml.MappingNode {
					continue
				}
				var mount struct {
					Type     string `yaml:"type"`
					Source   string `yaml:"source"`
					Target   string `yaml:"target"`
					ReadOnly bool   `yaml:"read_only"`
					Bind     struct {
						CreateHostPath *bool  `yaml:"create_host_path"`
						SELinux        string `yaml:"selinux"`
					} `yaml:"bind"`
				}
				if err := node.Decode(&mount); err != nil {
					t.Fatal(err)
				}
				if mount.Target != "/etc/resolv.conf" {
					continue
				}
				count++
				if mount.Type != "bind" || mount.Source != "./deploy/sidecar-resolv.conf" || !mount.ReadOnly || mount.Bind.CreateHostPath == nil || *mount.Bind.CreateHostPath || mount.Bind.SELinux != "z" {
					t.Fatalf("unsafe resolver mount: %+v", mount)
				}
			}
			if count != 1 {
				t.Fatalf("resolver mounts = %d, want 1", count)
			}
		})
	}
	raw, err := os.ReadFile("sidecar-resolv.conf")
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	for _, line := range strings.Split(string(raw), "\n") {
		if line = strings.TrimSpace(line); line != "" && !strings.HasPrefix(line, "#") {
			lines = append(lines, line)
		}
	}
	if strings.Join(lines, "\n") != "nameserver 127.0.0.11\noptions ndots:0" {
		t.Fatalf("unexpected resolver configuration: %q", lines)
	}
}

func TestProductionResolverRejectsUnsafeMount(t *testing.T) {
	for name, mutate := range map[string]func(map[string]any){
		"writable":         func(m map[string]any) { m["read_only"] = false },
		"volume":           func(m map[string]any) { m["type"] = "volume" },
		"relative":         func(m map[string]any) { m["source"] = "resolver" },
		"private label":    func(m map[string]any) { m["bind"].(map[string]any)["selinux"] = "Z" },
		"no label":         func(m map[string]any) { m["bind"] = map[string]any{} },
		"create directory": func(m map[string]any) { m["bind"].(map[string]any)["create_host_path"] = true },
		"wrong target":     func(m map[string]any) { m["target"] = "/wrong" },
	} {
		t.Run(name, func(t *testing.T) {
			fixture := productionFixture()
			browser := fixture["services"].(map[string]any)["chromium"].(map[string]any)
			mutate(browser["volumes"].([]any)[0].(map[string]any))
			if out, err := verifyProductionFixture(t, fixture); err == nil {
				t.Fatalf("accepted unsafe resolver: %s", out)
			}
		})
	}
}

func TestProductionResolverHostPreflight(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "resolver")
	if err := os.WriteFile(file, []byte("nameserver 127.0.0.11\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(file, link); err != nil {
		t.Fatal(err)
	}
	private := filepath.Join(dir, "private")
	if err := os.WriteFile(private, []byte("nameserver 127.0.0.11\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, path string
		ok         bool
	}{
		{"regular readable", file, true}, {"missing", filepath.Join(dir, "missing"), false},
		{"directory", dir, false}, {"symlink", link, false}, {"unreadable by sidecar", private, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := productionFixture()
			browser := fixture["services"].(map[string]any)["chromium"].(map[string]any)
			browser["volumes"].([]any)[0].(map[string]any)["source"] = tc.path
			payload, err := json.Marshal(fixture)
			if err != nil {
				t.Fatal(err)
			}
			// Label enforcement has its own matrix in TestProductionResolverActualSELinuxLabel.
			script := `import pathlib, runpy, sys
from unittest.mock import patch
original = pathlib.Path.exists
def exists(path):
    return False if str(path) == '/sys/fs/selinux/enforce' else original(path)
with patch.object(pathlib.Path, 'exists', exists):
    sys.argv.append('--check-host-files')
    runpy.run_path('verify-production-compose.py', run_name='__main__')
`
			command := exec.Command("python3", "-c", script)
			command.Stdin = bytes.NewReader(payload)
			out, err := command.CombinedOutput()
			if (err == nil) != tc.ok {
				t.Fatalf("host preflight: %v\n%s", err, out)
			}
		})
	}
}
