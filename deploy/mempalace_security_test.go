package deploy

import (
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/pelletier/go-toml/v2"
)

func TestMemPalaceSecurityLock(t *testing.T) {
	for _, image := range []struct{ path, source string }{
		{"../Dockerfile", "/tmp/mempalace-src"},
		{"mempalace/relay.Dockerfile", "/tmp/source"},
	} {
		t.Run(image.path, func(t *testing.T) {
			raw, err := os.ReadFile(image.path)
			if err != nil {
				t.Fatal(err)
			}
			s := string(raw)
			copyLock := strings.Index(s, "COPY deploy/mempalace/security-uv.lock /tmp/security-uv.lock")
			applyLock := strings.Index(s, "cp /tmp/security-uv.lock "+image.source+"/uv.lock")
			sync := regexp.MustCompile(`uv sync\s+`).FindStringIndex(s)
			if copyLock < 0 || applyLock < copyLock || sync == nil || sync[0] < applyLock {
				t.Fatal("image must copy and apply the shared security lock before uv sync")
			}
			end := strings.Index(s[sync[1]:], "&&")
			if end < 0 {
				t.Fatal("cannot find end of uv sync command")
			}
			args := s[sync[1] : sync[1]+end]
			if !strings.Contains(args, "--locked") || strings.Contains(args, "--frozen") || strings.Contains(s, "security-constraints.toml") {
				t.Fatal("image must validate the lock against upstream metadata, without inert constraint config")
			}
		})
	}
	raw, err := os.ReadFile("mempalace/security-uv.lock")
	if err != nil {
		t.Fatal(err)
	}
	var lock struct {
		Packages []struct {
			Name         string   `toml:"name"`
			Version      string   `toml:"version"`
			Markers      []string `toml:"resolution-markers"`
			Dependencies []struct {
				Name string `toml:"name"`
			} `toml:"dependencies"`
		} `toml:"package"`
	}
	if err := toml.Unmarshal(raw, &lock); err != nil {
		t.Fatal(err)
	}
	floors := map[string]string{
		"anyio": "4.14.2", "certifi": "2026.10.19", "cryptography": "50.0.0",
		"filelock": "4.2.1", "fsspec": "2026.6.0", "idna": "3.15.0",
		"pillow": "13.0.0", "setuptools": "84.1.0", "soupsieve": "2.9.0",
		"urllib3": "2.8.0",
	}
	// Only these complete marker forms prove a vulnerable entry cannot be
	// selected on shipped Python 3.11+. New marker forms need an explicit check.
	legacy := regexp.MustCompile(`^python_full_version (< '3\.10'|== '3\.10\.\*')( and sys_platform (==|!=) 'win32')?$`)
	seen := map[string]bool{}
	for _, p := range lock.Packages {
		if p.Name == "mempalace" {
			hasCertifi := false
			for _, dependency := range p.Dependencies {
				hasCertifi = hasCertifi || dependency.Name == "certifi"
			}
			if !hasCertifi {
				t.Error("MemPalace must select certifi at runtime for the image TLS bundle checks")
			}
		}
		if p.Name == "chromadb" || p.Name == "chromadb-hnswlib" || p.Name == "chroma-hnswlib" {
			t.Errorf("vulnerable ChromaDB package remains in the runtime lock: %s %s", p.Name, p.Version)
		}
		floor, required := floors[p.Name]
		if !required {
			continue
		}
		version, err := securityVersion(p.Version)
		if err != nil {
			t.Fatal(err)
		}
		minimum, _ := securityVersion(floor)
		if version >= minimum {
			seen[p.Name] = true
			continue
		}
		if len(p.Markers) == 0 {
			t.Errorf("%s %s is below security floor %s without a legacy-only marker", p.Name, p.Version, floor)
		}
		for _, marker := range p.Markers {
			if !legacy.MatchString(marker) {
				t.Errorf("%s %s is below security floor %s and may ship: %s", p.Name, p.Version, floor, marker)
			}
		}
	}
	for name := range floors {
		if !seen[name] {
			t.Errorf("no patched lock entry for %s", name)
		}
	}
}

func securityVersion(s string) (uint64, error) {
	parts := strings.Split(s, ".")
	if len(parts) < 2 || len(parts) > 3 {
		return 0, fmt.Errorf("unsupported security package version %q", s)
	}
	var result uint64
	for i := 0; i < 3; i++ {
		var n uint64
		if i < len(parts) {
			var err error
			n, err = strconv.ParseUint(parts[i], 10, 16)
			if err != nil {
				return 0, err
			}
		}
		result = result<<16 | n
	}
	return result, nil
}

func TestMemPalacePythonTLSRootsPatch(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 is not installed")
	}
	cmd := exec.Command(python, "-m", "unittest", "discover", "-s", "mempalace", "-p", "test_patch_python_tls_roots.py", "-v")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("Python trust-store patch: %v: %s", err, output)
	}
}
