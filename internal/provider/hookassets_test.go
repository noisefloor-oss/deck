package provider

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func TestCodexProviderContractParses(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "docs", "provider-contracts", "codex.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("codex provider contract is not valid YAML: %v", err)
	}
}

func TestCodexPlainTrustContractMatchesCompiledHashes(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "docs", "provider-contracts", "codex.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Identity struct {
			Hook struct {
				Measurement struct {
					Compositions []struct {
						Name    string `yaml:"name"`
						Targets []struct {
							KeySuffix   string `yaml:"key_suffix"`
							TrustedHash string `yaml:"trusted_hash"`
						} `yaml:"targets"`
					} `yaml:"compositions"`
				} `yaml:"native_trust_measurement"`
			} `yaml:"hook"`
		} `yaml:"identity"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"session_start:0:0": CodexSessionStartTrustHash,
		"stop:0:0":          CodexStopTrustHash,
	}
	plainCount := 0
	for _, composition := range doc.Identity.Hook.Measurement.Compositions {
		if composition.Name != "deck-only" {
			continue
		}
		plainCount++
		if len(composition.Targets) != 2 {
			t.Fatalf("plain trust target count = %d, want 2", len(composition.Targets))
		}
		for _, target := range composition.Targets {
			hash, ok := want[target.KeySuffix]
			if !ok || target.TrustedHash != hash {
				t.Fatalf("plain trust target %q = %q, want compiled %q", target.KeySuffix, target.TrustedHash, hash)
			}
			delete(want, target.KeySuffix)
		}
	}
	if plainCount != 1 || len(want) != 0 {
		t.Fatalf("plain trust compositions = %d, unmatched targets = %v", plainCount, want)
	}
}

// Provisioning installs the embedded asset byte for byte, so a hollowed-out
// opencode-hook.js would be installed and capture nothing. Two pins guard
// that, and BOTH must be updated deliberately together — a string scan alone
// was measured to pass a no-op plugin whose comments carried the tokens.
func TestOpencodeHookAssetCarriesTheCaptureMechanism(t *testing.T) {
	asset := string(OpencodeHookPlugin)
	for _, want := range []string{
		`event?.type !== "session.created"`, // the event check in CODE, not a comment
		"NOISEDECK_SESSION_HOOK_URL",        // capture endpoint from the launch env
		"NOISEDECK_SESSION_HOOK_TOKEN",      // the launch-only credential
		"X-Noisedeck-Hook-Token",            // the header hookcapture requires
		"session_id",                        // the payload's identity field
		`source: "startup",`,                // accept() refuses non-startup events
		"cwd: directory,",                   // accept() correlates on cwd
		"fetch(url",                         // the POST actually targets the endpoint
		"const body = JSON.stringify",       // retry attempts reuse one serialized body
		"await deliver(url, token, body)",   // the event waits only for bounded delivery
	} {
		if !strings.Contains(asset, want) {
			t.Errorf("opencode-hook.js is missing %q — provisioning would install a broken plugin", want)
		}
	}
}

// The hard pin: ANY change to the asset fails here, forcing the change to be
// made deliberately rather than incidentally. Update the sum with
// `sha256sum internal/provider/assets/opencode-hook.js` when the plugin
// deliberately changes.
func TestOpencodeHookAssetChecksum(t *testing.T) {
	const want = "3cfb825eb1386e42892bc136093a37b5f963dcc0d5645a773cc1ea940cb5ff75"
	if got := fmt.Sprintf("%x", sha256.Sum256(OpencodeHookPlugin)); got != want {
		t.Fatalf("opencode-hook.js sha256 = %s, want %s (deliberate plugin change? update both)", got, want)
	}
}

// omp reports its id only through this hook at launch, so a hollowed-out
// omp-hook.ts would leave every omp session unidentifiable and unresumable.
// The tokens below pin the capture mechanism in CODE, not in comments.
func TestOmpHookAssetCarriesTheCaptureMechanism(t *testing.T) {
	asset := string(OmpHookPlugin)
	for _, want := range []string{
		`pi.on("session_start"`,             // the event subscription in CODE
		`ctx?.sessionManager?.getSessionId`, // the id source the notes measured
		`ctx?.sessionManager?.getCwd`,       // cwd correlation
		"NOISEDECK_SESSION_HOOK_URL",        // capture endpoint from the launch env
		"NOISEDECK_SESSION_HOOK_TOKEN",      // the launch-only credential
		"X-Noisedeck-Hook-Token",            // the header hookcapture requires
		`hook_event_name: "SessionStart"`,   // the payload shape the receiver POSTs
		"session_id",                        // the payload's identity field
		`source: "startup",`,                // events ride the startup-shaped capture channel
		"pid: String(process.pid)",          // the reporting process, so subagents are rejected
		`pi.on("session_switch"`,            // in-pane /resume reports the conversation it lands on
		`hook_event_name: "SessionSwitch"`,  // the switch delivery names itself
		"const body = JSON.stringify",       // retry attempts reuse one serialized body
		"await deliver(url, token, body)",   // delivery stays in the hook process
	} {
		if !strings.Contains(asset, want) {
			t.Errorf("omp-hook.ts is missing %q — provisioning would install a broken hook", want)
		}
	}
	for _, forbidden := range []string{"writeFileSync", "unlinkSync", "tmpdir", "pi.exec", "node:fs"} {
		if strings.Contains(asset, forbidden) {
			t.Errorf("omp-hook.ts contains %q — capture payloads must not be staged or passed to a child process", forbidden)
		}
	}
}

func TestInProcessHookAssetsBoundCaptureDelivery(t *testing.T) {
	assets := map[string][]byte{
		"omp":      OmpHookPlugin,
		"pi":       PiHookPlugin,
		"opencode": OpencodeHookPlugin,
		"kilo":     KiloHookPlugin,
	}
	for name, raw := range assets {
		t.Run(name, func(t *testing.T) {
			asset := string(raw)
			for _, want := range []string{
				"NOISEDECK_SESSION_HOOK_SOCKET",
				"socketPath,",
				"request.end(options.body)",
				"const DELIVERY_DEADLINE_MS = 8000",
				"const ATTEMPT_TIMEOUT_MS = 2500",
				"const MAX_DELIVERY_ATTEMPTS = 3",
				"const deadline = Date.now() + DELIVERY_DEADLINE_MS",
				"const remaining = deadline - Date.now()",
				`redirect: "manual"`,
				"AbortSignal.timeout(Math.min(ATTEMPT_TIMEOUT_MS, remaining))",
				"response.status >= 200 && response.status < 300",
				"response.status >= 400 && response.status < 500",
				"response.status < 500 || response.status >= 600",
				"setTimeout(resolve, pause)",
			} {
				if !strings.Contains(asset, want) {
					t.Errorf("capture asset is missing bounded-delivery mechanism %q", want)
				}
			}
			if strings.Contains(asset, "console.") {
				t.Error("capture asset must not log payload, token, or response status")
			}
		})
	}
}

// The hard pin: ANY change to the asset fails here, forcing the change to be
// made deliberately rather than incidentally. Update the sum with
// `sha256sum internal/provider/assets/omp-hook.ts` when the hook deliberately
// changes.
func TestOmpHookAssetChecksum(t *testing.T) {
	const want = "2b5e1ac48d91d71b945a1390312fbbeafc12fc2b5bcaf3ecae7b0ae1ef037a96"
	if got := fmt.Sprintf("%x", sha256.Sum256(OmpHookPlugin)); got != want {
		t.Fatalf("omp-hook.ts sha256 = %s, want %s (deliberate hook change? update both)", got, want)
	}
}

// pi reports its id only through this hook at launch, so a hollowed-out
// pi-hook.js would leave every pi session unidentifiable and unresumable.
// The tokens below pin the capture mechanism in CODE, not in comments.
func TestPiHookAssetCarriesTheCaptureMechanism(t *testing.T) {
	asset := string(PiHookPlugin)
	for _, want := range []string{
		`pi.on("session_start"`,              // the event subscription in CODE
		`ctx?.sessionManager?.getSessionId`,  // the id source the notes measured
		`ctx?.sessionManager?.getCwd`,        // cwd correlation
		"NOISEDECK_SESSION_HOOK_URL",         // capture endpoint from the launch env
		"NOISEDECK_SESSION_HOOK_TOKEN",       // the launch-only credential
		"X-Noisedeck-Hook-Token",             // the header hookcapture requires
		`hook_event_name: "SessionStart"`,    // the payload shape the receiver POSTs
		"session_id",                         // the payload's identity field
		`source: event.reason ?? "startup",`, // initial launch attestation
		"const body = JSON.stringify",        // retry attempts reuse one serialized body
		"await deliver(url, token, body)",    // delivery stays in the hook process
	} {
		if !strings.Contains(asset, want) {
			t.Errorf("pi-hook.js is missing %q — provisioning would install a broken hook", want)
		}
	}
	for _, forbidden := range []string{"writeFileSync", "unlinkSync", "tmpdir", "pi.exec", "node:fs"} {
		if strings.Contains(asset, forbidden) {
			t.Errorf("pi-hook.js contains %q — capture payloads must not be staged or passed to a child process", forbidden)
		}
	}
}

// The hard pin: ANY change to the asset fails here, forcing the change to be
// made deliberately rather than incidentally. Update the sum with
// `sha256sum internal/provider/assets/pi-hook.js` when the hook deliberately
// changes.
func TestPiHookAssetChecksum(t *testing.T) {
	const want = "a2dbc914a8977e8eff6d1317af8a6d2af0426c8d3359fd40675969f4346586c4"
	if got := fmt.Sprintf("%x", sha256.Sum256(PiHookPlugin)); got != want {
		t.Fatalf("pi-hook.js sha256 = %s, want %s (deliberate hook change? update both)", got, want)
	}
}

// Execute each real exported hook against isolated Unix and loopback servers.
// Static pins cannot prove that socket failures keep tokens away from TCP.
func TestInProcessHookUnixTransportRuntime(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("Node.js unavailable for hook transport fixture")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, node, "--test", filepath.Join("assets", "hook-transport.test.mjs"))
	cmd.Env = []string{"PATH=" + os.Getenv("PATH")}
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("hook runtime fixture failed: %v\n%s", err, output)
	}
}

func TestKiloHookAssetChecksum(t *testing.T) {
	const want = "7fbba3547216dab7c7797957c3b25c4da99cd4e30257943cd3106b6e3f0ed91d"
	if got := fmt.Sprintf("%x", sha256.Sum256(KiloHookPlugin)); got != want {
		t.Fatalf("kilo-hook.js sha256 = %s, want %s (deliberate hook change? update pin)", got, want)
	}
}
