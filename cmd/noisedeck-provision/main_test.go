package main

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"

	"github.com/pelletier/go-toml/v2"

	"github.com/noisefloor-oss/deck/internal/provider"
)

func readCodex(t *testing.T, home string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(home, ".codex", "config.toml"))
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	var doc map[string]any
	if err := toml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("seeded config does not parse: %v\n%s", err, raw)
	}
	return doc
}

func codexFeatures(t *testing.T, home string) map[string]any {
	t.Helper()
	features, ok := readCodex(t, home)["features"].(map[string]any)
	if !ok {
		t.Fatal("no [features] table")
	}
	return features
}

func TestProvisionCommandCodePreservesSettingsAndRegistersHook(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, ".commandcode")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "settings.json")
	if err := os.WriteFile(path, []byte(`{"theme":"dark","hooks":{"Stop":[]}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := provisionCommandCode(home); err != nil {
		t.Fatalf("provisionCommandCode: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if doc["theme"] != "dark" {
		t.Fatalf("theme = %v", doc["theme"])
	}
	hooks := doc["hooks"].(map[string]any)
	if _, ok := hooks["Stop"]; !ok {
		t.Fatal("user hook was removed")
	}
	groups := hooks["SessionStart"].([]any)
	entry := groups[0].(map[string]any)["hooks"].([]any)[0].(map[string]any)
	if entry["type"] != "command" || entry["command"] != provider.HookReceiverPath {
		t.Fatalf("hook = %v", entry)
	}
	if _, err := os.Stat(path + ".noisedeck-bak"); err != nil {
		t.Fatalf("backup: %v", err)
	}
	first := string(raw)
	if err := provisionCommandCode(home); err != nil {
		t.Fatalf("second provision: %v", err)
	}
	raw, _ = os.ReadFile(path)
	if string(raw) != first {
		t.Fatal("second provision changed settings")
	}
}

func TestProvisionDevinPreservesConfigAndRegistersHook(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, ".config", "devin")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`{"model":"fast","hooks":{"Stop":[]}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := provisionDevin(home); err != nil {
		t.Fatalf("provisionDevin: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if doc["model"] != "fast" || doc["attribution"] != false || doc["auto_update"] != false {
		t.Fatalf("preserved settings = %v", doc)
	}
	hooks := doc["hooks"].(map[string]any)
	if _, ok := hooks["Stop"]; !ok {
		t.Fatal("user hook was removed")
	}
	groups := hooks["SessionStart"].([]any)
	entry := groups[0].(map[string]any)["hooks"].([]any)[0].(map[string]any)
	if entry["type"] != "command" || entry["command"] != provider.HookReceiverPath {
		t.Fatalf("hook = %v", entry)
	}
	if _, err := os.Stat(path + ".noisedeck-bak"); err != nil {
		t.Fatalf("backup: %v", err)
	}
	first := string(raw)
	if err := provisionDevin(home); err != nil {
		t.Fatalf("second provision: %v", err)
	}
	raw, _ = os.ReadFile(path)
	if string(raw) != first {
		t.Fatal("second provision changed config")
	}
}

func TestProvisionDevinRefusesMalformedConfig(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, ".config", "devin")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`{"hooks":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := provisionDevin(home); err == nil {
		t.Fatal("non-object hooks accepted")
	}
	raw, _ := os.ReadFile(path)
	if string(raw) != `{"hooks":[]}` {
		t.Fatal("malformed user config was rewritten")
	}
}

func TestProvisionCodexFreshHome(t *testing.T) {
	home := t.TempDir()
	if err := run(home); err != nil {
		t.Fatalf("run: %v", err)
	}
	features := codexFeatures(t, home)
	if features["hooks"] != true {
		t.Fatalf("features.hooks = %v", features["hooks"])
	}
	doc := readCodex(t, home)
	if doc["check_for_update_on_startup"] != false {
		t.Fatalf("update flag = %v", doc["check_for_update_on_startup"])
	}
	state := doc["hooks"].(map[string]any)["state"].(map[string]any)
	for event, want := range map[string]string{
		"session_start": codexSessionStartTrustHash,
		"stop":          codexStopTrustHash,
	} {
		key := filepath.Join(home, ".codex", "hooks.json") + ":" + event + ":0:0"
		block, ok := state[key].(map[string]any)
		if !ok || block["trusted_hash"] != want {
			t.Fatalf("trust block for %q = %v", key, state)
		}
	}
	// hooks.json is the embedded bytes.
	got, err := os.ReadFile(filepath.Join(home, ".codex", "hooks.json"))
	if err != nil || string(got) != string(codexHooksJSON) {
		t.Fatalf("hooks.json mismatch: %v", err)
	}
}

func TestCodexOwnedIdentityEntriesShareNativeTrust(t *testing.T) {
	const assetDigest = "78199ecc5032753e353218e31a2a6d8d2fb7daba72220790e4a7b5af7ab08c37"
	if got := fmt.Sprintf("%x", sha256.Sum256(codexHooksJSON)); got != assetDigest {
		t.Fatalf("plain hook bytes changed: %s", got)
	}
	var plain, composed map[string]any
	if err := json.Unmarshal(codexHooksJSON, &plain); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(codexHooksMempalaceJSON, &composed); err != nil {
		t.Fatal(err)
	}
	for _, event := range []string{"SessionStart", "Stop"} {
		plainGroup := plain["hooks"].(map[string]any)[event].([]any)[0]
		composedGroup := composed["hooks"].(map[string]any)[event].([]any)[0]
		if !reflect.DeepEqual(plainGroup, composedGroup) {
			t.Fatalf("%s identity group differs between owned compositions", event)
		}
	}
	if codexSessionStartTrustHash != codexMempalaceSessionStartTrustHash ||
		codexStopTrustHash != codexMempalaceStopTrustHash {
		t.Fatal("identical identity entry tuples must use the same measured native hashes")
	}
}

func TestProvisionCodexPlainProfileUsesAcceptedNativeHashes(t *testing.T) {
	// These literals are native Codex 0.162.1 evidence, independent of the
	// constants under test. The stale pair belonged to the earlier command.
	accepted := map[string]string{
		"session_start": "sha256:1c9347558100c3944c4811f2395489124b2ef14f8e7c752af83774ceb941f6e9",
		"stop":          "sha256:73957a3fd7d0507af5c9168350e26925dea8384830c96217f2c045d87f548b63",
	}
	stale := map[string]string{
		"session_start": "sha256:532f40f94e8a651166783ec911c82eef57dd983aae7d4710bc99f6a14ad1ca9d",
		"stop":          "sha256:5a9574ea20d4a760126604525dc07586ea303b9b15f244b130b5e860b2a74f1b",
	}
	for _, profile := range []struct {
		name  string
		hooks []byte
	}{
		{name: "fresh"},
		{name: "current-plain-stale-trust", hooks: codexHooksJSON},
		{name: "released-plain-upgrade", hooks: codexHooksV1JSON},
	} {
		t.Run(profile.name, func(t *testing.T) {
			home := t.TempDir()
			dir := filepath.Join(home, ".codex")
			hooksPath := filepath.Join(dir, "hooks.json")
			if profile.hooks != nil {
				if err := os.MkdirAll(dir, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(hooksPath, profile.hooks, 0o600); err != nil {
					t.Fatal(err)
				}
				state := map[string]any{"operator-owned": map[string]any{"trusted_hash": "sha256:operator"}}
				for event, hash := range stale {
					state[hooksPath+":"+event+":0:0"] = map[string]any{"trusted_hash": hash}
				}
				raw, err := toml.Marshal(map[string]any{
					"model":    "synthetic-model",
					"features": map[string]any{"hooks": true},
					"hooks":    map[string]any{"state": state},
				})
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, "config.toml"), raw, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if err := provisionCodex(home); err != nil {
				t.Fatal(err)
			}
			doc := readCodex(t, home)
			state := doc["hooks"].(map[string]any)["state"].(map[string]any)
			for event, hash := range accepted {
				block := state[hooksPath+":"+event+":0:0"].(map[string]any)
				if block["trusted_hash"] != hash {
					t.Fatalf("%s native trust = %v, want measured %s", event, block, hash)
				}
			}
			if profile.hooks != nil && (doc["model"] != "synthetic-model" ||
				state["operator-owned"].(map[string]any)["trusted_hash"] != "sha256:operator") {
				t.Fatal("user settings or unrelated trust changed")
			}
			got, err := os.ReadFile(hooksPath)
			if err != nil || string(got) != string(codexHooksJSON) {
				t.Fatalf("current hook bytes not installed: %v", err)
			}
			first, err := os.ReadFile(filepath.Join(dir, "config.toml"))
			if err != nil {
				t.Fatal(err)
			}
			if err := provisionCodex(home); err != nil {
				t.Fatal(err)
			}
			second, err := os.ReadFile(filepath.Join(dir, "config.toml"))
			if err != nil || string(first) != string(second) {
				t.Fatalf("accepted native trust changed on reprovision: %v", err)
			}
		})
	}
}

func TestProvisionCodexDoesNotBlessAlteredPlainHooks(t *testing.T) {
	for name, altered := range map[string]string{
		"different-command":            strings.Replace(string(codexHooksJSON), provider.CodexIdentityHookPath, "/operator/custom-hook", 1),
		"same-entries-different-bytes": string(codexHooksJSON) + "\n",
	} {
		t.Run(name, func(t *testing.T) {
			home := t.TempDir()
			dir := filepath.Join(home, ".codex")
			if err := os.MkdirAll(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			hooksPath := filepath.Join(dir, "hooks.json")
			if err := os.WriteFile(hooksPath, []byte(altered), 0o600); err != nil {
				t.Fatal(err)
			}
			key := hooksPath + ":session_start:0:0"
			existing := "check_for_update_on_startup = false\n[features]\nhooks = true\n[hooks.state.\"" + key + "\"]\ntrusted_hash = \"sha256:operator\"\n"
			if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(existing), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := provisionCodex(home); err == nil || !strings.Contains(err.Error(), "preserved unchanged") {
				t.Fatalf("altered plain hooks error = %v", err)
			}
			got, err := os.ReadFile(hooksPath)
			if err != nil || string(got) != altered {
				t.Fatalf("user hooks changed: %v", err)
			}
			got, err = os.ReadFile(filepath.Join(dir, "config.toml"))
			if err != nil || string(got) != existing {
				t.Fatalf("native trust was rewritten beside unowned hook bytes: %v", err)
			}
		})
	}
}

func TestProvisionCodexExistingConfigPreservesAndRepairs(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, ".codex")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	existing := `model = "gpt-5.6-sol"

# the user's own flag
[features]
  unified_exec = true
  hooks=false

[hooks.state."legacy"]
trusted_hash = "sha256:abc"
check_for_update_on_startup = true
`
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(existing), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := run(home); err != nil {
		t.Fatalf("run: %v", err)
	}
	doc := readCodex(t, home)
	features := doc["features"].(map[string]any)
	if features["hooks"] != true || features["unified_exec"] != true {
		t.Fatalf("features = %v", features)
	}
	// A backup of the user's file exists.
	if _, err := os.Stat(filepath.Join(dir, "config.toml.noisedeck-bak")); err != nil {
		t.Fatalf("no backup: %v", err)
	}
}

func TestProvisionCodexUpgradesOnlyReleasedHookBytes(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, ".codex")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "hooks.json"), codexHooksV1JSON, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := provisionCodex(home); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "hooks.json"))
	if err != nil || string(got) != string(codexHooksJSON) {
		t.Fatalf("upgraded hooks=%q err=%v", got, err)
	}
	doc := readCodex(t, home)
	state := doc["hooks"].(map[string]any)["state"].(map[string]any)
	for _, event := range []string{"session_start", "stop"} {
		key := filepath.Join(dir, "hooks.json") + ":" + event + ":0:0"
		if _, ok := state[key]; !ok {
			t.Fatalf("missing upgraded trust for %s", event)
		}
	}
}

func TestProvisionCodexUpgradesReleasedHookInsideComposedConfig(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, ".codex")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	hooksPath := filepath.Join(dir, "hooks.json")
	if err := os.WriteFile(hooksPath, codexHooksMempalaceV1JSON, 0o600); err != nil {
		t.Fatal(err)
	}

	if err := provisionCodex(home); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(hooksPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != string(codexHooksMempalaceJSON) {
		t.Fatal("released composition was not upgraded to the measured current bytes")
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("upgraded hooks do not parse: %v", err)
	}
	hooks := doc["hooks"].(map[string]any)
	starts := hooks["SessionStart"].([]any)
	if len(starts) != 3 {
		t.Fatalf("SessionStart groups = %d, want Deck, MemPalace, a2a", len(starts))
	}
	deckStart := starts[0].(map[string]any)["hooks"].([]any)[0].(map[string]any)
	if deckStart["command"] != provider.CodexIdentityHookPath {
		t.Fatalf("Deck SessionStart command = %v", deckStart["command"])
	}
	if got := starts[1].(map[string]any)["hooks"].([]any)[0].(map[string]any)["command"]; got != "/opt/mempalace/hooks/mempal-hook.sh session-start" {
		t.Fatalf("foreign SessionStart command = %v", got)
	}
	if got := starts[2].(map[string]any)["hooks"].([]any)[0].(map[string]any)["command"]; got != "/opt/mempalace/hooks/a2a-hook.sh session-start codex" {
		t.Fatalf("a2a SessionStart command = %v", got)
	}
	stops := hooks["Stop"].([]any)
	if len(stops) != 3 {
		t.Fatalf("Stop groups = %d, want Deck, MemPalace, a2a", len(stops))
	}
	if got := stops[0].(map[string]any)["hooks"].([]any)[0].(map[string]any)["command"]; got != provider.CodexIdentityHookPath {
		t.Fatalf("Deck Stop command = %v", got)
	}
	if got := stops[1].(map[string]any)["hooks"].([]any)[0].(map[string]any)["command"]; got != "/opt/mempalace/hooks/mempal-hook.sh stop" {
		t.Fatalf("foreign Stop command = %v", got)
	}
	if got := stops[2].(map[string]any)["hooks"].([]any)[0].(map[string]any)["command"]; got != "/opt/mempalace/hooks/a2a-hook.sh stop codex" {
		t.Fatalf("a2a Stop command = %v", got)
	}
	if _, ok := hooks["PreCompact"]; !ok {
		t.Fatal("foreign PreCompact hook was removed")
	}

	state := readCodex(t, home)["hooks"].(map[string]any)["state"].(map[string]any)
	for key, want := range map[string]string{
		hooksPath + ":session_start:0:0": codexMempalaceSessionStartTrustHash,
		hooksPath + ":session_start:1:0": codexMempalaceSessionMemoryHash,
		hooksPath + ":session_start:2:0": codexMempalaceA2ASessionStartTrustHash,
		hooksPath + ":stop:0:0":          codexMempalaceStopTrustHash,
		hooksPath + ":stop:1:0":          codexMempalaceStopMemoryHash,
		hooksPath + ":stop:2:0":          codexMempalaceA2AStopTrustHash,
		hooksPath + ":pre_compact:0:0":   codexMempalacePreCompactHash,
	} {
		block, ok := state[key].(map[string]any)
		if !ok || block["trusted_hash"] != want {
			t.Fatalf("trust block for %q = %v", key, state)
		}
	}

	first := string(raw)
	if err := provisionCodex(home); err != nil {
		t.Fatalf("second provision: %v", err)
	}
	second, err := os.ReadFile(hooksPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(second) != first {
		t.Fatal("second provision changed composed hooks")
	}
}

func TestProvisionCodexUpgradesTheV2ComposedConfig(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, ".codex")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	hooksPath := filepath.Join(dir, "hooks.json")
	if err := os.WriteFile(hooksPath, codexHooksMempalaceV2JSON, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := provisionCodex(home); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(hooksPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != string(codexHooksMempalaceJSON) {
		t.Fatal("v2 composition was not upgraded to the measured current bytes")
	}
	state := readCodex(t, home)["hooks"].(map[string]any)["state"].(map[string]any)
	for _, key := range []string{hooksPath + ":session_start:2:0", hooksPath + ":stop:2:0"} {
		if _, ok := state[key].(map[string]any); !ok {
			t.Fatalf("missing a2a trust for %s", key)
		}
	}
}

func TestProvisionCodexDoesNotClaimAnAlteredComposition(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, ".codex")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	hooksPath := filepath.Join(dir, "hooks.json")
	altered := strings.Replace(string(codexHooksMempalaceV1JSON),
		`"timeout": 30`, `"timeout": 31`, 1)
	if err := os.WriteFile(hooksPath, []byte(altered), 0o600); err != nil {
		t.Fatal(err)
	}
	err := provisionCodex(home)
	if err == nil || !strings.Contains(err.Error(), "preserved unchanged") {
		t.Fatalf("altered composition error = %v", err)
	}
	got, err := os.ReadFile(hooksPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != altered {
		t.Fatal("altered composition was rewritten")
	}
	doc := readCodex(t, home)
	if hooks, ok := doc["hooks"].(map[string]any); ok {
		if state, ok := hooks["state"].(map[string]any); ok && len(state) != 0 {
			t.Fatalf("trust was seeded for an unmeasured composition: %v", state)
		}
	}
}

func TestProvisionIsIdempotent(t *testing.T) {
	home := t.TempDir()
	if err := run(home); err != nil {
		t.Fatalf("first run: %v", err)
	}
	first, _ := os.ReadFile(filepath.Join(home, ".codex", "config.toml"))
	if err := run(home); err != nil {
		t.Fatalf("second run: %v", err)
	}
	second, _ := os.ReadFile(filepath.Join(home, ".codex", "config.toml"))
	if string(first) != string(second) {
		t.Fatalf("second boot changed the config:\n%s\n---\n%s", first, second)
	}
}

func TestProvisionKimiValidatesHookShapesBeforeWriting(t *testing.T) {
	for name, contents := range map[string]string{
		"scalar":        "hooks = true\n",
		"plain table":   "[hooks]\nevent = \"Stop\"\n",
		"empty array":   "hooks = []\n",
		"inline tables": "hooks = [{event = \"Stop\", command = \"x\"}]\n",
	} {
		t.Run(name, func(t *testing.T) {
			home := t.TempDir()
			path := filepath.Join(home, ".kimi-code", "config.toml")
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := provisionKimi(home); err == nil {
				t.Fatal("incompatible hooks accepted")
			}
			after, err := os.ReadFile(path)
			if err != nil || string(after) != contents {
				t.Fatalf("config changed: %q, %v", after, err)
			}
			if _, err := os.Stat(path + ".noisedeck-bak"); !os.IsNotExist(err) {
				t.Fatalf("refusal created a backup: %v", err)
			}
		})
	}
}

func TestProvisionKimiParsesTheCompleteCandidateBeforeBackup(t *testing.T) {
	for _, header := range []string{"[[ hooks ]]", "[[\"hooks\"]]"} {
		t.Run(header, func(t *testing.T) {
			home := t.TempDir()
			path := filepath.Join(home, ".kimi-code", "config.toml")
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatal(err)
			}
			contents := header + "\nevent = \"Stop\"\ncommand = \"x\"\n"
			if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := provisionKimi(home); err != nil {
				t.Fatalf("valid array-of-tables refused: %v", err)
			}
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var doc map[string]any
			if err := toml.Unmarshal(raw, &doc); err != nil {
				t.Fatalf("extended config does not parse: %v", err)
			}
			if hooks, _ := doc["hooks"].([]any); len(hooks) != 2 {
				t.Fatalf("hooks = %v", hooks)
			}
		})
	}

	home := t.TempDir()
	path := filepath.Join(home, ".kimi-code", "config.toml")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	contents := "# Example: [[hooks]]\nhooks = [{event = \"Stop\", command = \"x\"}]\n"
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := provisionKimi(home); err == nil {
		t.Fatal("misleading comment allowed an invalid append")
	}
	after, err := os.ReadFile(path)
	if err != nil || string(after) != contents {
		t.Fatalf("config changed: %q, %v", after, err)
	}
	if _, err := os.Stat(path + ".noisedeck-bak"); !os.IsNotExist(err) {
		t.Fatalf("refusal created a backup: %v", err)
	}
}

func TestProvisionKimiBacksUpAndExtendsArrayOfTables(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, ".kimi-code", "config.toml")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	contents := "[[hooks]]\nevent = \"Stop\"\ncommand = \"x\"\n"
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := provisionKimi(home); err != nil {
		t.Fatalf("provision: %v", err)
	}
	backup, err := os.ReadFile(path + ".noisedeck-bak")
	if err != nil || string(backup) != contents {
		t.Fatalf("backup = %q, %v", backup, err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := toml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("post-provision TOML: %v", err)
	}
	if hooks, _ := doc["hooks"].([]any); len(hooks) != 2 {
		t.Fatalf("hooks = %v", hooks)
	}
	if err := provisionKimi(home); err != nil {
		t.Fatalf("idempotent provision: %v", err)
	}
}

func TestProvisionKimiAppendsTheFlatHookOnce(t *testing.T) {
	home := t.TempDir()
	if err := run(home); err != nil {
		t.Fatalf("run: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(home, ".kimi-code", "config.toml"))
	if err != nil {
		t.Fatalf("read kimi config: %v", err)
	}
	var doc map[string]any
	if err := toml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("kimi config does not parse: %v", err)
	}
	hooks, _ := doc["hooks"].([]any)
	if len(hooks) != 1 {
		t.Fatalf("kimi hooks = %v", hooks)
	}
	table := hooks[0].(map[string]any)
	if table["event"] != "SessionStart" || table["command"] != "/usr/local/bin/noisedeck-provider-hook" {
		t.Fatalf("kimi hook table = %v", table)
	}
	// Only the permitted keys (Kimi refuses the whole config otherwise).
	for key := range table {
		switch key {
		case "event", "matcher", "command", "timeout":
		default:
			t.Fatalf("kimi hook table carries %q — Kimi refuses extra keys", key)
		}
	}
	if err := run(home); err != nil {
		t.Fatalf("second run: %v", err)
	}
	raw2, _ := os.ReadFile(filepath.Join(home, ".kimi-code", "config.toml"))
	var doc2 map[string]any
	if err := toml.Unmarshal(raw2, &doc2); err != nil {
		t.Fatal(err)
	}
	if n := len(doc2["hooks"].([]any)); n != 1 {
		t.Fatalf("second boot duplicated the hook table (%d)", n)
	}
}

func TestProvisionRefusesInvalidTOML(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, ".codex")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte("[broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := run(home); err == nil {
		t.Fatal("invalid TOML was not an error")
	}
}

func TestProvisionCodexForeignHooksJSONIsPreservedAndReported(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, ".codex")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	// The user's own hooks.json: valid, but NOT the bytes the learned
	// hash covers. Seeding the trust block beside it would bless content
	// nobody verified.
	if err := os.WriteFile(filepath.Join(dir, "hooks.json"),
		[]byte(`{"hooks":{"SessionStart":[{"hooks":[{"type":"command","command":"/opt/other"}]}]}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	err := run(home)
	if err == nil || !strings.Contains(err.Error(), "preserved unchanged") {
		t.Fatalf("foreign hooks error = %v", err)
	}
	doc := readCodex(t, home)
	if hooks, ok := doc["hooks"].(map[string]any); ok {
		if state, ok := hooks["state"].(map[string]any); ok && len(state) > 0 {
			t.Fatalf("trust seeded beside a foreign hooks.json: %v", state)
		}
	}
	// The user's file is untouched.
	raw, _ := os.ReadFile(filepath.Join(dir, "hooks.json"))
	if strings.Contains(string(raw), "noisedeck-provider-hook") {
		t.Fatal("the user's hooks.json was rewritten")
	}
}

func TestProvisionRepairsWrongValues(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, ".codex")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "hooks.json"), codexHooksJSON, 0o600); err != nil {
		t.Fatal(err)
	}
	key := filepath.Join(dir, "hooks.json") + ":session_start:0:0"
	existing := "check_for_update_on_startup = true\n\n[features]\nhooks = true\n\n[hooks.state]\n[hooks.state.\"" + key + "\"]\ntrusted_hash = \"sha256:wrong\"\n"
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(existing), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := run(home); err != nil {
		t.Fatalf("run: %v", err)
	}
	doc := readCodex(t, home)
	if doc["check_for_update_on_startup"] != false {
		t.Fatal("wrong update flag preserved")
	}
	block := doc["hooks"].(map[string]any)["state"].(map[string]any)[key].(map[string]any)
	if block["trusted_hash"] != codexSessionStartTrustHash {
		t.Fatalf("wrong trust hash preserved: %v", block)
	}
}

func opencodePlugin(t *testing.T, home string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(home, ".config", "opencode", "plugins", "noisedeck-provider-hook.js"))
	if err != nil {
		t.Fatalf("read plugin: %v", err)
	}
	return string(raw)
}

func TestProvisionKiloInstallsAndRepairsIdentityPlugin(t *testing.T) {
	home := t.TempDir()
	if err := provisionKilo(home); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home, ".config", "kilo", "plugin", "noisedeck-provider-hook.js")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != string(provider.KiloHookPlugin) {
		t.Fatal("installed Kilo plugin differs from the provisioned bytes")
	}
	if !strings.Contains(string(raw), "MAX_DELIVERY_ATTEMPTS = 3") {
		t.Fatal("installed Kilo plugin lacks bounded capture delivery")
	}
	if err := os.WriteFile(path, []byte("disabled"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := provisionKilo(home); err != nil {
		t.Fatal(err)
	}
	raw, err = os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != string(provider.KiloHookPlugin) {
		t.Fatal("Kilo plugin was not repaired")
	}
	backup, err := os.ReadFile(path + ".noisedeck-bak")
	if err != nil || string(backup) != "disabled" {
		t.Fatalf("backup = %q, err = %v", backup, err)
	}
}

func TestProvisionOpencodeFreshHome(t *testing.T) {
	home := t.TempDir()
	if err := run(home); err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := opencodePlugin(t, home); got != string(provider.OpencodeHookPlugin) {
		t.Fatal("installed plugin is not the canonical asset the gate requires")
	} else if !strings.Contains(got, "MAX_DELIVERY_ATTEMPTS = 3") {
		t.Fatal("installed opencode plugin lacks bounded capture delivery")
	}
	raw, err := os.ReadFile(filepath.Join(home, ".config", "opencode", "opencode.json"))
	if err != nil {
		t.Fatalf("read opencode.json: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("opencode.json does not parse: %v", err)
	}
	if doc["autoupdate"] != false {
		t.Fatalf("autoupdate = %v, want false (an update prompt would stall unattended launches)", doc["autoupdate"])
	}
	tuiRaw, err := os.ReadFile(filepath.Join(home, ".config", "opencode", "tui.json"))
	if err != nil {
		t.Fatalf("read tui.json: %v", err)
	}
	var tui map[string]any
	if err := json.Unmarshal(tuiRaw, &tui); err != nil {
		t.Fatalf("tui.json does not parse: %v", err)
	}
	if tui["theme"] != "system" {
		t.Fatalf("theme = %v, want system", tui["theme"])
	}
}

func TestProvisionOpencodeMergesSystemTheme(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, ".config", "opencode")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "tui.json")
	original := []byte(`{"scroll_speed":5,"theme":"opencode"}`)
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := run(home); err != nil {
		t.Fatalf("run: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if doc["theme"] != "system" || doc["scroll_speed"] != float64(5) {
		t.Fatalf("merged tui config = %#v", doc)
	}
	backup, err := os.ReadFile(path + ".noisedeck-bak")
	if err != nil || string(backup) != string(original) {
		t.Fatalf("backup = %q, %v", backup, err)
	}
}

func TestProvisionOpencodeRepairsTamperedPlugin(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, ".config", "opencode", "plugins")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "noisedeck-provider-hook.js")
	if err := os.WriteFile(path, []byte("// disabled by hand\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := run(home); err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := opencodePlugin(t, home); got != string(provider.OpencodeHookPlugin) {
		t.Fatal("a tampered plugin must be replaced — the gate requires exact content")
	}
	if _, err := os.Stat(path + ".noisedeck-bak"); err != nil {
		t.Fatal("the replaced file must be backed up first")
	}
}

func TestProvisionOpencodeIsIdempotent(t *testing.T) {
	home := t.TempDir()
	if err := run(home); err != nil {
		t.Fatalf("first run: %v", err)
	}
	if err := run(home); err != nil {
		t.Fatalf("second run: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, ".config", "opencode", "plugins", "noisedeck-provider-hook.js.noisedeck-bak")); !os.IsNotExist(err) {
		t.Fatal("a second run must not back up or rewrite an already-current plugin")
	}
}

func ompHook(t *testing.T, home string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(home, ".omp", "noisedeck-provider-hook.ts"))
	if err != nil {
		t.Fatalf("read omp hook: %v", err)
	}
	return raw
}

func TestProvisionOmpFreshHome(t *testing.T) {
	home := t.TempDir()
	if err := run(home); err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := ompHook(t, home); string(got) != string(provider.OmpHookPlugin) {
		t.Fatal("installed omp hook is not the canonical asset the gate requires")
	} else if strings.Contains(string(got), "writeFileSync") ||
		!strings.Contains(string(got), "MAX_DELIVERY_ATTEMPTS = 3") {
		t.Fatal("installed omp hook must deliver boundedly without payload staging")
	}
	dirInfo, err := os.Stat(filepath.Join(home, ".omp"))
	if err != nil {
		t.Fatalf("stat omp directory: %v", err)
	}
	if got := dirInfo.Mode().Perm(); got != 0o700 {
		t.Errorf("omp directory mode = %04o, want 0700", got)
	}
	hookInfo, err := os.Stat(filepath.Join(home, ".omp", "noisedeck-provider-hook.ts"))
	if err != nil {
		t.Fatalf("stat omp hook: %v", err)
	}
	if got := hookInfo.Mode().Perm(); got != 0o600 {
		t.Errorf("omp hook mode = %04o, want 0600", got)
	}
	stat, ok := hookInfo.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatal("omp hook stat has no Unix ownership")
	}
	if stat.Uid != uint32(os.Geteuid()) || stat.Gid != uint32(os.Getegid()) {
		t.Errorf("omp hook owner = %d:%d, want runtime process %d:%d",
			stat.Uid, stat.Gid, os.Geteuid(), os.Getegid())
	}
}

func TestProvisionOmpRepairsTamperedHook(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, ".omp")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "noisedeck-provider-hook.ts")
	tampered := []byte("// disabled by hand\n")
	if err := os.WriteFile(path, tampered, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := run(home); err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := ompHook(t, home); string(got) != string(provider.OmpHookPlugin) {
		t.Fatal("a tampered omp hook must be replaced — the gate requires exact content")
	}
	backup, err := os.ReadFile(path + ".noisedeck-bak")
	if err != nil {
		t.Fatalf("read replaced hook backup: %v", err)
	}
	if string(backup) != string(tampered) {
		t.Fatalf("backup = %q, want original tampered bytes", backup)
	}
}

func TestProvisionOmpIsIdempotent(t *testing.T) {
	home := t.TempDir()
	if err := run(home); err != nil {
		t.Fatalf("first run: %v", err)
	}
	if err := run(home); err != nil {
		t.Fatalf("second run: %v", err)
	}
	backup := filepath.Join(home, ".omp", "noisedeck-provider-hook.ts.noisedeck-bak")
	if _, err := os.Stat(backup); !os.IsNotExist(err) {
		t.Fatal("a second run must not back up or rewrite an already-current omp hook")
	}
}

func piHook(t *testing.T, home string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(home, ".pi", "noisedeck-provider-hook.js"))
	if err != nil {
		t.Fatalf("read pi hook: %v", err)
	}
	return raw
}

func TestProvisionPiFreshHome(t *testing.T) {
	home := t.TempDir()
	if err := run(home); err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := piHook(t, home); string(got) != string(provider.PiHookPlugin) {
		t.Fatal("installed pi hook is not the canonical asset the gate requires")
	} else if strings.Contains(string(got), "writeFileSync") ||
		!strings.Contains(string(got), "MAX_DELIVERY_ATTEMPTS = 3") {
		t.Fatal("installed pi hook must deliver boundedly without payload staging")
	}
	dirInfo, err := os.Stat(filepath.Join(home, ".pi"))
	if err != nil {
		t.Fatalf("stat pi directory: %v", err)
	}
	if got := dirInfo.Mode().Perm(); got != 0o700 {
		t.Errorf("pi directory mode = %04o, want 0700", got)
	}
	hookInfo, err := os.Stat(filepath.Join(home, ".pi", "noisedeck-provider-hook.js"))
	if err != nil {
		t.Fatalf("stat pi hook: %v", err)
	}
	if got := hookInfo.Mode().Perm(); got != 0o600 {
		t.Errorf("pi hook mode = %04o, want 0600", got)
	}
}

func TestProvisionPiRepairsTamperedHook(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, ".pi")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "noisedeck-provider-hook.js")
	tampered := []byte("// disabled by hand\n")
	if err := os.WriteFile(path, tampered, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := run(home); err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := piHook(t, home); string(got) != string(provider.PiHookPlugin) {
		t.Fatal("a tampered pi hook must be replaced — the gate requires exact content")
	}
	backup, err := os.ReadFile(path + ".noisedeck-bak")
	if err != nil {
		t.Fatalf("read replaced hook backup: %v", err)
	}
	if string(backup) != string(tampered) {
		t.Fatalf("backup = %q, want original tampered bytes", backup)
	}
}

func TestProvisionPiIsIdempotent(t *testing.T) {
	home := t.TempDir()
	if err := run(home); err != nil {
		t.Fatalf("first run: %v", err)
	}
	if err := run(home); err != nil {
		t.Fatalf("second run: %v", err)
	}
	backup := filepath.Join(home, ".pi", "noisedeck-provider-hook.js.noisedeck-bak")
	if _, err := os.Stat(backup); !os.IsNotExist(err) {
		t.Fatal("a second run must not back up or rewrite an already-current pi hook")
	}
}
