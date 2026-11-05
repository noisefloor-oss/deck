//go:build linux

package workspaceruntime

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"testing"

	"github.com/noisefloor-oss/deck/internal/sealedstate"
)

func TestSwapPolicyControlsNativeAdmissionAndLaunchEnvironment(t *testing.T) {
	for _, policy := range []sealedstate.SwapPolicy{"", sealedstate.SwapPersonal, sealedstate.SwapParanoid} {
		for _, encrypted := range []bool{false, true} {
			t.Run(string(policy)+map[bool]string{false: "/plain", true: "/encrypted"}[encrypted], func(t *testing.T) {
				f := newFixture(t)
				f.c.cfg.SwapPolicy = policy
				f.facts.SwapFacts = sealedstate.Facts{Swaps: []string{"/dev/dm-1"}, SwapDevice: func(string) string { return "253:1" }, DMUUID: func(string) string {
					if encrypted {
						return "CRYPT-PLAIN-synthetic"
					}
					return "LVM-synthetic"
				}}
				_, err := f.c.Initialize(t.Context(), "init", make([]byte, 32))
				wantPass := policy.Effective() == sealedstate.SwapPersonal || encrypted
				if wantPass && err != nil {
					t.Fatal(err)
				}
				if !wantPass && (!errors.Is(err, ErrUnsafeStorage) || len(f.mutations()) != 0) {
					t.Fatal("unsafe swap caused native effects", err)
				}
				if !slices.Contains(f.c.environment(), "NOISEDECK_SEALED_SWAP_POLICY="+string(policy.Effective())) {
					t.Fatal("launch lost policy")
				}
				if wantPass {
					f.facts.SwapFacts.Swaps = append(f.facts.SwapFacts.Swaps, "/dev/zram0")
					f.facts.SwapFacts.SwapDevice = func(path string) string {
						if path == "/dev/dm-1" {
							return "253:1"
						}
						return ""
					}
					_, err = f.c.Observe(t.Context())
					if policy == sealedstate.SwapParanoid && err == nil {
						t.Fatal("new unencrypted swap did not invalidate observation")
					}
				}
			})
		}
	}
}

func TestSwapPolicyIsInstallationOwned(t *testing.T) {
	f := newFixture(t)
	if err := f.c.Close(); err != nil {
		t.Fatal(err)
	}
	f.c = nil
	f.cfg.SwapPolicy = "typo"
	if c, err := open(f.cfg, f, func() (kernelFacts, error) { return f.facts, nil }); !errors.Is(err, ErrConfiguration) {
		if c != nil {
			_ = c.Close()
		}
		t.Fatal("unknown policy accepted", err)
	}
}

func TestSwapPolicyReopensLegacyInstallation(t *testing.T) {
	f := newFixture(t)
	f.cfg.WorkspaceGateway = true
	// Freeze the serialized configuration before SwapPolicy existed. Building
	// this from Config would hide incompatible field ordering or JSON tags.
	legacy := fmt.Sprintf(`{"InstanceID":%q,"OwnerUID":%d,"CipherDir":%q,"MountPoint":%q,"ControlDir":%q,"HomeDir":%q,"RuntimeDir":%q,"GocryptfsPath":%q,"FusermountPath":%q,"PodmanPath":%q,"SealedComposePath":%q,"ComposeEnvFile":%q,"ComposeProject":%q,"AppContainer":%q,"WorkspaceGateway":true}`,
		f.cfg.InstanceID, f.cfg.OwnerUID, f.cfg.CipherDir, f.cfg.MountPoint,
		f.cfg.ControlDir, f.cfg.HomeDir, f.cfg.RuntimeDir, f.cfg.GocryptfsPath,
		f.cfg.FusermountPath, f.cfg.PodmanPath, f.cfg.SealedComposePath,
		f.cfg.ComposeEnvFile, f.cfg.ComposeProject, f.cfg.AppContainer)
	digest := sha256.Sum256([]byte(legacy))
	if _, err := f.c.db.Exec(`UPDATE installation SET fingerprint=? WHERE id=1`, hex.EncodeToString(digest[:])); err != nil {
		t.Fatal(err)
	}
	if err := f.c.Close(); err != nil {
		t.Fatal(err)
	}
	f.c = nil
	f.open()
	if err := f.c.Close(); err != nil {
		t.Fatal(err)
	}
	f.c = nil
	for _, policy := range []sealedstate.SwapPolicy{sealedstate.SwapPersonal, sealedstate.SwapParanoid} {
		f.cfg.SwapPolicy = policy
		c, err := open(f.cfg, f, func() (kernelFacts, error) { return f.facts, nil })
		if c != nil {
			_ = c.Close()
		}
		if !errors.Is(err, ErrConfiguration) {
			t.Fatalf("adding explicit %s policy to legacy journal: %v", policy, err)
		}
	}
	if len(f.calls) != 0 {
		t.Fatal("legacy configuration reopen dispatched a native command")
	}
}

func TestSwapPolicyCannotChangePinnedInstallation(t *testing.T) {
	for _, original := range []sealedstate.SwapPolicy{sealedstate.SwapPersonal, sealedstate.SwapParanoid} {
		t.Run(string(original), func(t *testing.T) {
			f := newFixture(t)
			f.cfg.SwapPolicy = original
			raw, err := json.Marshal(f.cfg)
			if err != nil {
				t.Fatal(err)
			}
			digest := sha256.Sum256(raw)
			if _, err := f.c.db.Exec(`UPDATE installation SET fingerprint=? WHERE id=1`, hex.EncodeToString(digest[:])); err != nil {
				t.Fatal(err)
			}
			if err := f.c.Close(); err != nil {
				t.Fatal(err)
			}
			f.c = nil
			for _, next := range []sealedstate.SwapPolicy{"", sealedstate.SwapPersonal, sealedstate.SwapParanoid} {
				f.cfg.SwapPolicy = next
				c, err := open(f.cfg, f, func() (kernelFacts, error) { return f.facts, nil })
				if c != nil {
					_ = c.Close()
				}
				if next == original && err != nil {
					t.Fatalf("unchanged policy refused: %v", err)
				}
				if next != original && !errors.Is(err, ErrConfiguration) {
					t.Fatalf("changing %s policy to %q: %v", original, next, err)
				}
			}
			if len(f.calls) != 0 {
				t.Fatal("policy change dispatched a native command")
			}
		})
	}
}
