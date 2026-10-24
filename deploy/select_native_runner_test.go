package deploy

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestSelectNativeRunner(t *testing.T) {
	tests := []struct {
		name    string
		backend string
		want    []string
	}{
		{
			name: "automatic self-hosted runner",
			want: []string{`backend=self-hosted`, `runner=["self-hosted","Linux","X64"]`},
		},
		{
			name:    "legacy automatic runner uses self-hosted",
			backend: "auto",
			want:    []string{`backend=self-hosted`, `runner=["self-hosted","Linux","X64"]`},
		},
		{
			name: "explicit GitHub override", backend: "github",
			want: []string{`backend=github`, `runner="ubuntu-24.04"`},
		},
		{
			name: "explicit self-hosted", backend: "self-hosted",
			want: []string{`backend=self-hosted`, `runner=["self-hosted","Linux","X64"]`},
		},
		{
			name:    "explicit Blacksmith override",
			backend: "blacksmith",
			want:    []string{`backend=blacksmith`, `runner="blacksmith-4vcpu-ubuntu-2404"`},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			script := filepath.Join("select-native-runner.sh")
			cmd := exec.Command("sh", script)
			cmd.Env = append(os.Environ(),
				"NOISEDECK_NATIVE_BACKEND="+test.backend,
			)
			output, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("select native runner: %v\n%s", err, output)
			}
			for _, want := range test.want {
				if !strings.Contains(string(output), want+"\n") {
					t.Errorf("output %q does not contain %q", output, want)
				}
			}
		})
	}
}

func TestSelectNativeRunnerRejectsInvalidBackend(t *testing.T) {
	for _, backend := range []string{"m1", "somewhere"} {
		t.Run(backend, func(t *testing.T) {
			cmd := exec.Command("sh", "select-native-runner.sh")
			cmd.Env = append(os.Environ(),
				"NOISEDECK_NATIVE_BACKEND="+backend,
			)
			output, err := cmd.CombinedOutput()
			if err == nil {
				t.Fatalf("invalid backend succeeded: %s", output)
			}
			if !strings.Contains(string(output), "must be auto, self-hosted, github, or blacksmith") {
				t.Fatalf("unexpected diagnostic: %s", output)
			}
		})
	}
}
