package modelbump

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// fakeCLI writes a shell script masquerading as a provider executable.
func fakeCLI(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "claude")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// The probe's classifier must demand evidence the CLI ran: a marker-free
// result is only Present when the process actually executed and reached
// its own reporting. These run real executables through the adapter.
func TestClaudeMemberDemandsRunEvidence(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name string
		body string
		want Presence
	}{
		{"marker means absent", "echo '[claude-code:unrecognized_model] {\"model\":\"x\"}' >&2\nexit 1\n", Absent},
		{"measured login gate means present", "printf 'Not logged in · Please run /login\n'\nexit 0\n", Present},
		{"connection refusal means present", "echo 'fetch failed: ECONNREFUSED 127.0.0.1:9' >&2\nexit 1\n", Present},
		{"silent success is ambiguous", "echo done\nexit 0\n", Ambiguous},
		{"argument rejection is ambiguous", "echo 'usage: claude [options]' >&2\nexit 2\n", Ambiguous},
		{"/bin/false-style silent failure is ambiguous", "exit 1\n", Ambiguous},
		{"signal death is ambiguous", "kill -9 $$\n", Ambiguous},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ClaudeMember(ctx, fakeCLI(t, tc.body), "claude-sonnet-5-6")
			if err != nil {
				t.Fatalf("ClaudeMember: %v", err)
			}
			if got != tc.want {
				t.Fatalf("presence = %v, want %v", got, tc.want)
			}
		})
	}
	t.Run("unlaunchable executable fails the plan", func(t *testing.T) {
		if _, err := ClaudeMember(ctx, filepath.Join(t.TempDir(), "missing"), "claude-sonnet-5-6"); err == nil {
			t.Fatal("a missing CLI executable must be an error, not a presence verdict")
		}
	})
	t.Run("timeout is ambiguous", func(t *testing.T) {
		probeCtx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		defer cancel()
		got, err := ClaudeMember(probeCtx, fakeCLI(t, "sleep 1\n"), "claude-sonnet-5-6")
		if err != nil || got != Ambiguous {
			t.Fatalf("presence = %v, err = %v; want Ambiguous", got, err)
		}
	})
}

func TestKimiCatalogReadsManagedAliases(t *testing.T) {
	cli := fakeCLI(t, `[ "$1 $2 $3" = "provider list --json" ] || exit 9
printf '%s\n' '{"models":{"zeta/other":{},"kimi-code/k3.2-pro":{},"kimi-code/k3":{},"kimi-code/k3.1-pro":{}}}'
`)
	got, err := KimiCatalog(context.Background(), cli)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"kimi-code/k3", "kimi-code/k3.1-pro", "kimi-code/k3.2-pro"}
	if len(got) != len(want) {
		t.Fatalf("catalog = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("catalog = %v, want %v", got, want)
		}
	}
}

func TestKimiCatalogFailsClosed(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{
		{"malformed JSON", "printf 'not json\\n'\n"},
		{"no managed models", "printf '%s\\n' '{\"models\":{\"zeta/other\":{}}}'\n"},
		{"CLI failure", "exit 1\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := KimiCatalog(context.Background(), fakeCLI(t, tc.body)); err == nil {
				t.Fatal("KimiCatalog accepted an unusable catalog")
			}
		})
	}
}
