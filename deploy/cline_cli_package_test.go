package deploy

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestDockerfileInstallsPinnedClineCLI(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "Dockerfile"))
	if err != nil {
		t.Fatal(err)
	}
	body := string(raw)
	for _, required := range []string{
		"ARG CLINE_VERSION=3.0.74",
		`"cline@${CLINE_VERSION}"`,
		`test "$(cline --version)" = "${CLINE_VERSION}"`,
		"cline --help",
		`require('/usr/local/lib/node_modules/cline/package.json').version`,
		"ln -sf ../lib/node_modules/cline/bin/cline /usr/local/bin/cline",
		"cline-final-help",
		"--tui",
		"--auto-approve <boolean>",
	} {
		if !strings.Contains(body, required) {
			t.Fatalf("Dockerfile missing pinned Cline CLI marker %q", required)
		}
	}
	lastGlobalInstall := strings.LastIndex(body, "npm install --global")
	finalLinkRepair := strings.LastIndex(body, "ln -sf ../lib/node_modules/cline/bin/cline /usr/local/bin/cline")
	if lastGlobalInstall == -1 || finalLinkRepair <= lastGlobalInstall {
		t.Fatal("Cline executable must be reconciled after the final global npm transaction")
	}
}

// COPY transfers package files, not ENV. Only FROM inherits this flag.
func clineUpdateEnvironment(instructions []string) (map[string]string, error) {
	values := make(map[string]string)
	probed := make(map[string]bool)
	var stage string
	for _, instruction := range instructions {
		fields := strings.Fields(instruction)
		if len(fields) == 0 {
			continue
		}
		switch fields[0] {
		case "FROM":
			stage = fields[len(fields)-1]
			values[stage] = values[fields[1]]
		case "ENV":
			for _, assignment := range fields[1:] {
				if value, ok := strings.CutPrefix(assignment, "CLINE_NO_AUTO_UPDATE="); ok {
					values[stage] = value
				}
			}
		case "RUN":
			if strings.Contains(instruction, "cline --version") || strings.Contains(instruction, "cline --help") {
				if values[stage] != "1" {
					return nil, fmt.Errorf("%s probes Cline with CLINE_NO_AUTO_UPDATE=%q", stage, values[stage])
				}
				probed[stage] = true
			}
		}
	}
	for _, stage := range []string{"npm-harnesses", "core", "runtime"} {
		if values[stage] != "1" {
			return nil, fmt.Errorf("%s must retain CLINE_NO_AUTO_UPDATE=1", stage)
		}
	}
	for _, stage := range []string{"npm-harnesses", "runtime"} {
		if !probed[stage] {
			return nil, fmt.Errorf("%s must probe Cline", stage)
		}
	}
	return values, nil
}

func TestClineAutoUpdateIsDisabledBeforeBuildAndRuntimeProbes(t *testing.T) {
	instructions := imageInstructions(t, "runtime")
	if _, err := clineUpdateEnvironment(instructions); err != nil {
		t.Fatal(err)
	}
	for _, missing := range []string{"npm-harnesses", "core"} {
		t.Run("missing-"+missing, func(t *testing.T) {
			mutated := append([]string(nil), instructions...)
			var stage string
			for i, instruction := range mutated {
				fields := strings.Fields(instruction)
				if len(fields) > 0 && fields[0] == "FROM" {
					stage = fields[len(fields)-1]
				}
				if stage == missing && strings.HasPrefix(instruction, "ENV ") {
					mutated[i] = strings.ReplaceAll(instruction, "CLINE_NO_AUTO_UPDATE=1", "")
				}
			}
			if _, err := clineUpdateEnvironment(mutated); err == nil {
				t.Fatalf("accepted a Dockerfile with unprotected %s", missing)
			}
		})
	}
}

func TestClinePackageSurvivesProbesAndPrefixRelocation(t *testing.T) {
	values, err := clineUpdateEnvironment(imageInstructions(t, "runtime"))
	if err != nil {
		t.Fatal(err)
	}
	const target = "../lib/node_modules/cline/bin/cline"
	makePackage := func(t *testing.T, prefix string) string {
		t.Helper()
		for _, path := range []string{"bin", "lib/node_modules/cline/bin"} {
			if err := os.MkdirAll(filepath.Join(prefix, path), 0o755); err != nil {
				t.Fatal(err)
			}
		}
		// Model the updater's bin removal synchronously so this regression test
		// does not depend on winning a detached-process race or contacting npm.
		launcher := `#!/bin/sh
if [ "${CLINE_NO_AUTO_UPDATE:-}" != 1 ]; then
  printf 'npm update -g cline --tag latest\n' >> "$CLINE_TEST_UPDATES"
  rm -- "$0"
fi
case "$1" in
  --version) printf '3.0.60\n' ;;
  --help) printf '%s\n' '--tui --auto-approve <boolean>' ;;
  *) exit 2 ;;
esac
`
		if err := os.WriteFile(filepath.Join(prefix, "lib/node_modules/cline/bin/cline"), []byte(launcher), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(prefix, "lib/node_modules/cline/package.json"), []byte(`{"name":"cline","version":"3.0.60","bin":{"cline":"bin/cline"}}`), 0o644); err != nil {
			t.Fatal(err)
		}
		bin := filepath.Join(prefix, "bin/cline")
		if err := os.Symlink(target, bin); err != nil {
			t.Fatal(err)
		}
		return bin
	}
	runProbe := func(t *testing.T, bin, flag, updates, probe string) string {
		t.Helper()
		cmd := exec.Command(bin, probe)
		cmd.Env = []string{"PATH=/usr/bin:/bin", "CLINE_NO_AUTO_UPDATE=" + flag, "CLINE_TEST_UPDATES=" + updates}
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%s %s: %v\n%s", bin, probe, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	root := t.TempDir()
	updates := filepath.Join(root, "updater-started")
	source := filepath.Join(root, "npm-harnesses")
	bin := makePackage(t, source)
	for _, stage := range []string{"npm-harnesses", "runtime"} {
		if stage == "runtime" {
			runtime := filepath.Join(root, "runtime")
			if out, err := exec.Command("cp", "-a", source, runtime).CombinedOutput(); err != nil {
				t.Fatalf("copy prefix: %v\n%s", err, out)
			}
			if err := os.Rename(source, source+"-hidden"); err != nil {
				t.Fatal(err)
			}
			bin = filepath.Join(runtime, "bin/cline")
		}
		if got := runProbe(t, bin, values[stage], updates, "--version"); got != "3.0.60" {
			t.Fatalf("%s version = %q", stage, got)
		}
		if got := runProbe(t, bin, values[stage], updates, "--help"); !strings.Contains(got, "--tui") || !strings.Contains(got, "--auto-approve <boolean>") {
			t.Fatalf("%s help = %q", stage, got)
		}
		if got, err := os.Readlink(bin); err != nil || got != target {
			t.Fatalf("%s launcher target = %q, %v", stage, got, err)
		}
		if _, err := os.Stat(updates); !os.IsNotExist(err) {
			t.Fatalf("%s probe launched the updater: %v", stage, err)
		}
	}
	t.Run("unprotected-probe-can-pass-while-removing-launcher", func(t *testing.T) {
		bin := makePackage(t, filepath.Join(root, "unprotected"))
		if got := runProbe(t, bin, "", updates, "--version"); got != "3.0.60" {
			t.Fatalf("version = %q", got)
		}
		if _, err := os.Stat(updates); err != nil {
			t.Fatalf("unprotected probe did not launch the updater: %v", err)
		}
		if _, err := os.Lstat(bin); !os.IsNotExist(err) {
			t.Fatalf("unprotected fixture retained its launcher: %v", err)
		}
	})
}
