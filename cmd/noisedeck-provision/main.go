// noisedeck-provision seeds provider configuration on first boot.
// Provisioning owns provider configuration; Deck never writes it at
// runtime. This tool runs from the image's entrypoint wrapper and guarantees
// the SessionStart hook registration and codex's native hook trust, which is
// how a retrieved-identity session's native id reaches Deck at all.
//
// The TOML work is a real parse + structured edit, not text surgery:
// go-toml round-trips the user's existing keys and tables, and every
// guarantee is expressed against the parsed structure, so spacing,
// indentation, comments, and table order cannot fool it. An existing
// config that needs changes is backed up first (config.toml.noisedeck-bak).
package main

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/pelletier/go-toml/v2"

	"github.com/noisefloor-oss/deck/internal/provider"
)

// codexHooksJSON is the fixed SessionStart/Stop registration. It must stay
// byte-identical to the file the learned trust hashes cover — the
// bootstrap comment in the Dockerfile install step says the same.
//
//go:embed assets/codex-hooks.json
var codexHooksJSON []byte

// codexHooksV1JSON is the exact previously released Deck-owned hook file.
// Recognising only these frozen bytes lets upgrades replace it without
// claiming authority over a user-authored hook configuration.
//
//go:embed assets/codex-hooks-v1.json
var codexHooksV1JSON []byte

// The MemPalace overlay composes its own exact hook groups beside Deck's. Both
// versions are frozen deployment outputs: recognizing the entire byte tuple
// lets provisioning upgrade Deck's group without claiming arbitrary user
// configuration.
//
//go:embed assets/codex-hooks-mempalace.json
var codexHooksMempalaceJSON []byte

//go:embed assets/codex-hooks-mempalace-v1.json
var codexHooksMempalaceV1JSON []byte

//go:embed assets/codex-hooks-mempalace-v2.json
var codexHooksMempalaceV2JSON []byte

// These shared per-entry native hook-trust measurements are recorded in
// internal/provider/codex.go and docs/provider-contracts/codex.yaml. The key
// is the absolute hooks.json path + event + group/hook indices. They qualify
// only exact owned compositions, not full CLI conformance.
const (
	codexSessionStartTrustHash = provider.CodexSessionStartTrustHash
	codexStopTrustHash         = provider.CodexStopTrustHash

	codexMempalaceSessionStartTrustHash = provider.CodexMempalaceSessionStartTrustHash
	codexMempalaceStopTrustHash         = provider.CodexMempalaceStopTrustHash
	codexMempalaceSessionMemoryHash     = provider.CodexMempalaceSessionMemoryHash
	codexMempalaceStopMemoryHash        = provider.CodexMempalaceStopMemoryHash
	codexMempalacePreCompactHash        = provider.CodexMempalacePreCompactHash

	codexMempalaceA2ASessionStartTrustHash = provider.CodexMempalaceA2ASessionStartTrustHash
	codexMempalaceA2AStopTrustHash         = provider.CodexMempalaceA2AStopTrustHash
)

type codexTrustTarget struct {
	event string
	group int
	hook  int
	hash  string
}

func codexMempalaceTrustTargets() []codexTrustTarget {
	return []codexTrustTarget{
		{event: "session_start", group: 0, hook: 0, hash: codexMempalaceSessionStartTrustHash},
		{event: "session_start", group: 1, hook: 0, hash: codexMempalaceSessionMemoryHash},
		{event: "session_start", group: 2, hook: 0, hash: codexMempalaceA2ASessionStartTrustHash},
		{event: "stop", group: 0, hook: 0, hash: codexMempalaceStopTrustHash},
		{event: "stop", group: 1, hook: 0, hash: codexMempalaceStopMemoryHash},
		{event: "stop", group: 2, hook: 0, hash: codexMempalaceA2AStopTrustHash},
		{event: "pre_compact", group: 0, hook: 0, hash: codexMempalacePreCompactHash},
	}
}

// kimiHookTOML is the flat [[hooks]] table Kimi's strict loader permits:
// event/matcher/command/timeout and nothing else.
const kimiHookTOML = `
[[hooks]]
event = "SessionStart"
matcher = "startup"
command = "/usr/local/bin/noisedeck-provider-hook"
timeout = 30
`

func main() {
	home := os.Getenv("HOME")
	if home == "" {
		home = "/home/agent"
	}
	// With arguments this is the per-project subcommand; with none it is the
	// boot pass that seeds every provider's configuration.
	if len(os.Args) > 1 {
		if err := runTrust(home, os.Args[1:], os.Stdout); err != nil {
			fmt.Fprintf(os.Stderr, "noisedeck-provision: %v\n", err)
			os.Exit(1)
		}
		return
	}
	if err := run(home); err != nil {
		// Provisioning failure must not stop the service: the launch
		// gate refuses affected provider launches with remediation while claude
		// keeps working.
		fmt.Fprintf(os.Stderr, "noisedeck-provision: %v\n", err)
		os.Exit(1)
	}
}

func run(home string) error {
	// All providers are attempted regardless of another's failure: a
	// broken codex config must not leave kimi, omp, or opencode unprovisioned.
	return errors.Join(
		provisionCommandCode(home),
		provisionCodex(home),
		provisionDevin(home),
		provisionKimi(home),
		provisionKilo(home),
		provisionOmp(home),
		provisionPi(home),
		provisionOpencode(home),
	)
}

// provisionDevin merges the documented user-level SessionStart hook and the
// two non-interactive repository-policy settings into Devin's persisted JSON
// configuration. Unrelated user configuration is preserved.
func provisionDevin(home string) error {
	dir := filepath.Join(home, ".config", "devin")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	configPath := filepath.Join(dir, "config.json")
	raw, err := os.ReadFile(configPath) // #nosec G304 -- fixed provider config under the selected service home.
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("read %s: %w", configPath, err)
	}
	doc := map[string]any{}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &doc); err != nil {
			return fmt.Errorf("%s does not parse as JSON: %w", configPath, err)
		}
	}
	changed := false
	if doc["attribution"] != false {
		doc["attribution"] = false
		changed = true
	}
	if doc["auto_update"] != false {
		doc["auto_update"] = false
		changed = true
	}
	hooksValue, hasHooks := doc["hooks"]
	hooks, hooksOK := hooksValue.(map[string]any)
	if hasHooks && !hooksOK {
		return fmt.Errorf("%s hooks must be an object", configPath)
	}
	if !hasHooks {
		hooks = map[string]any{}
		doc["hooks"] = hooks
		changed = true
	}
	groupsValue, hasGroups := hooks["SessionStart"]
	groups, groupsOK := groupsValue.([]any)
	if hasGroups && !groupsOK {
		return fmt.Errorf("%s hooks.SessionStart must be an array", configPath)
	}
	found := false
	for _, groupValue := range groups {
		group, _ := groupValue.(map[string]any)
		entries, _ := group["hooks"].([]any)
		for _, entryValue := range entries {
			entry, _ := entryValue.(map[string]any)
			if entry["type"] == "command" && entry["command"] == provider.HookReceiverPath {
				found = true
			}
		}
	}
	if !found {
		groups = append(groups, map[string]any{"hooks": []any{map[string]any{
			"type": "command", "command": provider.HookReceiverPath,
		}}})
		hooks["SessionStart"] = groups
		changed = true
	}
	if !changed {
		return nil
	}
	if len(raw) > 0 {
		if err := os.WriteFile(configPath+".noisedeck-bak", raw, 0o600); err != nil {
			return fmt.Errorf("back up %s: %w", configPath, err)
		}
	}
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return fmt.Errorf("encode %s: %w", configPath, err)
	}
	if err := os.WriteFile(configPath, append(out, '\n'), 0o600); err != nil {
		return fmt.Errorf("write %s: %w", configPath, err)
	}
	return nil
}

// provisionCommandCode registers the documented SessionStart hook without
// replacing unrelated user settings. The entry must name the exact
// receiver: a hook pointing elsewhere never delivers an identity to Deck.
func provisionCommandCode(home string) error {
	dir := filepath.Join(home, ".commandcode")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	settingsPath := filepath.Join(dir, "settings.json")
	raw, err := os.ReadFile(settingsPath) // #nosec G304 -- fixed provider config under the selected service home.
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("read %s: %w", settingsPath, err)
	}
	doc := map[string]any{}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &doc); err != nil {
			return fmt.Errorf("%s does not parse as JSON: %w", settingsPath, err)
		}
	}
	hooksValue, hasHooks := doc["hooks"]
	hooks, hooksOK := hooksValue.(map[string]any)
	if hasHooks && !hooksOK {
		return fmt.Errorf("%s hooks must be an object", settingsPath)
	}
	if !hasHooks {
		hooks = map[string]any{}
		doc["hooks"] = hooks
	}
	groupsValue, hasGroups := hooks["SessionStart"]
	groups, groupsOK := groupsValue.([]any)
	if hasGroups && !groupsOK {
		return fmt.Errorf("%s hooks.SessionStart must be an array", settingsPath)
	}
	for _, groupValue := range groups {
		group, _ := groupValue.(map[string]any)
		entries, _ := group["hooks"].([]any)
		for _, entryValue := range entries {
			entry, _ := entryValue.(map[string]any)
			if entry["type"] == "command" && entry["command"] == provider.HookReceiverPath {
				return nil
			}
		}
	}
	groups = append(groups, map[string]any{"hooks": []any{map[string]any{
		"type": "command", "command": provider.HookReceiverPath,
	}}})
	hooks["SessionStart"] = groups
	if len(raw) > 0 {
		if err := os.WriteFile(settingsPath+".noisedeck-bak", raw, 0o600); err != nil {
			return fmt.Errorf("back up %s: %w", settingsPath, err)
		}
	}
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return fmt.Errorf("encode %s: %w", settingsPath, err)
	}
	if err := os.WriteFile(settingsPath, out, 0o600); err != nil {
		return fmt.Errorf("write %s: %w", settingsPath, err)
	}
	return nil
}

// provisionKilo installs the global plugin that emits Kilo's documented
// session.created identity event into Deck's one-shot capture endpoint.
func provisionKilo(home string) error {
	dir := filepath.Join(home, ".config", "kilo", "plugin")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	pluginPath := filepath.Join(dir, "noisedeck-provider-hook.js")
	existing, err := os.ReadFile(pluginPath) // #nosec G304 -- fixed provider config path below service home.
	switch {
	case err == nil && string(existing) == string(provider.KiloHookPlugin):
		return nil
	case err != nil && !os.IsNotExist(err):
		return fmt.Errorf("read %s: %w", pluginPath, err)
	default:
		if err == nil {
			if backupErr := os.WriteFile(pluginPath+".noisedeck-bak", existing, 0o600); backupErr != nil {
				return fmt.Errorf("back up %s: %w", pluginPath, backupErr)
			}
		}
		if writeErr := os.WriteFile(pluginPath, provider.KiloHookPlugin, 0o600); writeErr != nil {
			return fmt.Errorf("install %s: %w", pluginPath, writeErr)
		}
		return nil
	}
}

// provisionOmp installs the identity capture hook omp needs. Deck injects
// this TypeScript module with --hook on every new launch, and the installed
// file must be byte-identical to provider.OmpHookPlugin. A differing
// file is backed up and replaced so a stale or disabled hook cannot silently
// lose the only native session id omp reports.
func provisionOmp(home string) error {
	dir := filepath.Join(home, ".omp")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	hookPath := filepath.Join(dir, "noisedeck-provider-hook.ts")
	// #nosec G304 -- the path is <service home>/.omp/
	// noisedeck-provider-hook.ts; this tool's whole job is provisioning that file.
	existing, err := os.ReadFile(hookPath)
	switch {
	case err == nil && string(existing) == string(provider.OmpHookPlugin):
		// Already current.
	case err != nil && !os.IsNotExist(err):
		return fmt.Errorf("read %s: %w", hookPath, err)
	default:
		if err == nil {
			if berr := os.WriteFile(hookPath+".noisedeck-bak", existing, 0o600); berr != nil {
				return fmt.Errorf("back up %s: %w", hookPath, berr)
			}
		}
		if err := os.WriteFile(hookPath, provider.OmpHookPlugin, 0o600); err != nil {
			return fmt.Errorf("install %s: %w", hookPath, err)
		}
	}
	return nil
}

// provisionPi installs the identity capture hook pi needs. Deck injects this
// JavaScript module with --extension on every new launch; pi's extension
// discovery scans only <cwd>/.pi/extensions/ and ~/.pi/agent/extensions/, and
// the hook is installed at the ~/.pi config root (neither), so the flag is
// the only loader. The installed file must be byte-identical to
// provider.PiHookPlugin; a differing file is
// backed up and replaced so a stale or disabled hook cannot silently lose
// the only native session id pi reports.
func provisionPi(home string) error {
	dir := filepath.Join(home, ".pi")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	hookPath := filepath.Join(dir, "noisedeck-provider-hook.js")
	// #nosec G304 -- the path is <service home>/.pi/
	// noisedeck-provider-hook.js; this tool's whole job is provisioning that file.
	existing, err := os.ReadFile(hookPath)
	switch {
	case err == nil && string(existing) == string(provider.PiHookPlugin):
		// Already current.
	case err != nil && !os.IsNotExist(err):
		return fmt.Errorf("read %s: %w", hookPath, err)
	default:
		if err == nil {
			if berr := os.WriteFile(hookPath+".noisedeck-bak", existing, 0o600); berr != nil {
				return fmt.Errorf("back up %s: %w", hookPath, berr)
			}
		}
		if err := os.WriteFile(hookPath, provider.PiHookPlugin, 0o600); err != nil {
			return fmt.Errorf("install %s: %w", hookPath, err)
		}
	}
	return nil
}

// provisionOpencode installs the identity plugin opencode needs. opencode
// loads any .js in its plugins directory at startup; the installed file must
// be byte-identical to provider.OpencodeHookPlugin, so a differing file is
// backed up and replaced — leaving it would lose every opencode session's
// only native id.
func provisionOpencode(home string) error {
	dir := filepath.Join(home, ".config", "opencode", "plugins")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	pluginPath := filepath.Join(dir, "noisedeck-provider-hook.js")
	// #nosec G304 -- the path is <service home>/.config/opencode/plugins/
	// noisedeck-provider-hook.js; this tool's whole job is provisioning that file.
	existing, err := os.ReadFile(pluginPath)
	switch {
	case err == nil && string(existing) == string(provider.OpencodeHookPlugin):
		// Already current.
	case err != nil && !os.IsNotExist(err):
		return fmt.Errorf("read %s: %w", pluginPath, err)
	default:
		if err == nil {
			if berr := os.WriteFile(pluginPath+".noisedeck-bak", existing, 0o600); berr != nil {
				return fmt.Errorf("back up %s: %w", pluginPath, berr)
			}
		}
		if err := os.WriteFile(pluginPath, provider.OpencodeHookPlugin, 0o600); err != nil {
			return fmt.Errorf("install %s: %w", pluginPath, err)
		}
	}

	// Update prompts would stall an unattended launch, same hazard as
	// codex's check_for_update_on_startup. Merge the key into the user's
	// existing opencode.json; a file that does not parse is the user's
	// to fix, not ours to rewrite.
	confPath := filepath.Join(home, ".config", "opencode", "opencode.json")
	// #nosec G304 -- sibling of the provisioned plugin, same justification.
	raw, err := os.ReadFile(confPath)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("read %s: %w", confPath, err)
	}
	doc := map[string]any{}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &doc); err != nil {
			return fmt.Errorf("%s does not parse as JSON: %w", confPath, err)
		}
	}
	if v, ok := doc["autoupdate"]; !ok || v != false {
		if len(raw) > 0 {
			if berr := os.WriteFile(confPath+".noisedeck-bak", raw, 0o600); berr != nil {
				return fmt.Errorf("back up %s: %w", confPath, berr)
			}
		}
		doc["autoupdate"] = false
		out, err := json.MarshalIndent(doc, "", "  ")
		if err != nil {
			return fmt.Errorf("encode %s: %w", confPath, err)
		}
		if err := os.WriteFile(confPath, out, 0o600); err != nil {
			return fmt.Errorf("write %s: %w", confPath, err)
		}
	}

	// OpenCode's default theme paints fixed colors. Its documented system
	// theme preserves terminal defaults and ANSI colors, so it follows the
	// xterm palette selected by the user without runtime output parsing.
	tuiPath := filepath.Join(home, ".config", "opencode", "tui.json")
	// #nosec G304 -- sibling of the provisioned plugin, same justification.
	raw, err = os.ReadFile(tuiPath)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("read %s: %w", tuiPath, err)
	}
	tui := map[string]any{}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &tui); err != nil {
			return fmt.Errorf("%s does not parse as JSON: %w", tuiPath, err)
		}
	}
	if theme, ok := tui["theme"]; !ok || theme != "system" {
		if len(raw) > 0 {
			if berr := os.WriteFile(tuiPath+".noisedeck-bak", raw, 0o600); berr != nil {
				return fmt.Errorf("back up %s: %w", tuiPath, berr)
			}
		}
		tui["theme"] = "system"
		out, err := json.MarshalIndent(tui, "", "  ")
		if err != nil {
			return fmt.Errorf("encode %s: %w", tuiPath, err)
		}
		if err := os.WriteFile(tuiPath, out, 0o600); err != nil {
			return fmt.Errorf("write %s: %w", tuiPath, err)
		}
	}
	return nil
}

func provisionCodex(home string) error {
	dir := filepath.Join(home, ".codex")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}

	hooksPath := filepath.Join(dir, "hooks.json")
	var trustTargets []codexTrustTarget
	var hookContractErr error
	// #nosec G304 -- the path is <service home>/.codex/hooks.json; this tool's
	// whole job is provisioning that exact file.
	if existing, err := os.ReadFile(hooksPath); os.IsNotExist(err) {
		if err := os.WriteFile(hooksPath, codexHooksJSON, 0o600); err != nil {
			return fmt.Errorf("install hooks.json: %w", err)
		}
		trustTargets = []codexTrustTarget{
			{event: "session_start", group: 0, hook: 0, hash: codexSessionStartTrustHash},
			{event: "stop", group: 0, hook: 0, hash: codexStopTrustHash},
		}
	} else if err != nil {
		return fmt.Errorf("read hooks.json: %w", err)
	} else {
		var upgraded []byte
		switch string(existing) {
		case string(codexHooksJSON):
			trustTargets = []codexTrustTarget{
				{event: "session_start", group: 0, hook: 0, hash: codexSessionStartTrustHash},
				{event: "stop", group: 0, hook: 0, hash: codexStopTrustHash},
			}
		case string(codexHooksV1JSON):
			upgraded = codexHooksJSON
			trustTargets = []codexTrustTarget{
				{event: "session_start", group: 0, hook: 0, hash: codexSessionStartTrustHash},
				{event: "stop", group: 0, hook: 0, hash: codexStopTrustHash},
			}
		case string(codexHooksMempalaceJSON):
			trustTargets = codexMempalaceTrustTargets()
		case string(codexHooksMempalaceV1JSON):
			upgraded = codexHooksMempalaceJSON
			trustTargets = codexMempalaceTrustTargets()
		case string(codexHooksMempalaceV2JSON):
			upgraded = codexHooksMempalaceJSON
			trustTargets = codexMempalaceTrustTargets()
		default:
			hookContractErr = fmt.Errorf("%s is not an exact provisioning-owned Codex hook composition; preserved unchanged", hooksPath)
		}
		if upgraded != nil {
			if err := os.WriteFile(hooksPath+".noisedeck-bak", existing, 0o600); err != nil {
				return fmt.Errorf("back up hooks.json: %w", err)
			}
			if err := os.WriteFile(hooksPath, upgraded, 0o600); err != nil {
				return fmt.Errorf("upgrade hooks.json: %w", err)
			}
		}
	}

	confPath := filepath.Join(dir, "config.toml")
	// #nosec G304 -- sibling of the provisioned hooks file, same justification.
	raw, err := os.ReadFile(confPath)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	var doc map[string]any
	if len(raw) > 0 {
		if err := toml.Unmarshal(raw, &doc); err != nil {
			// A config that does not parse is the user's to fix.
			// Not ours to rewrite.
			return fmt.Errorf("%s does not parse as TOML: %w", confPath, err)
		}
	} else {
		doc = map[string]any{}
	}

	changed := false

	// Top-level: update prompts would stall an unattended launch. The
	// required value is enforced, not just its presence.
	if v, ok := doc["check_for_update_on_startup"]; !ok || v != false {
		doc["check_for_update_on_startup"] = false
		changed = true
	}

	// [features] hooks = true — codex 0.145.0 keeps hooks behind it.
	features, _ := doc["features"].(map[string]any)
	if features == nil {
		features = map[string]any{}
		doc["features"] = features
	}
	if v, ok := features["hooks"]; !ok || v != true {
		features["hooks"] = true
		changed = true
	}

	// The native hook-trust block is only valid as a TUPLE with the exact
	// hooks.json bytes the hash was learned from (its preimage is
	// unmeasured). Seeding it beside a different hooks.json would either
	// leave codex distrusting the real file or — worse — bless content we
	// never verified. When the file is not ours we seed nothing and report the
	// conflict, leaving the registration for the user to reconcile to one
	// exact provisioning-owned composition.
	if len(trustTargets) > 0 {
		hooksSection, _ := doc["hooks"].(map[string]any)
		if hooksSection == nil {
			hooksSection = map[string]any{}
			doc["hooks"] = hooksSection
		}
		state, _ := hooksSection["state"].(map[string]any)
		if state == nil {
			state = map[string]any{}
			hooksSection["state"] = state
		}
		for _, target := range trustTargets {
			trustKey := fmt.Sprintf("%s:%s:%d:%d", hooksPath, target.event,
				target.group, target.hook)
			block, ok := state[trustKey].(map[string]any)
			if !ok || block["trusted_hash"] != target.hash {
				state[trustKey] = map[string]any{"trusted_hash": target.hash}
				changed = true
			}
		}
	}

	if !changed {
		return hookContractErr
	}
	if len(raw) > 0 {
		if err := os.WriteFile(confPath+".noisedeck-bak", raw, 0o600); err != nil {
			return fmt.Errorf("back up config.toml: %w", err)
		}
	}
	out, err := toml.Marshal(doc)
	if err != nil {
		return fmt.Errorf("encode config.toml: %w", err)
	}
	if err := os.WriteFile(confPath, out, 0o600); err != nil {
		return fmt.Errorf("write config.toml: %w", err)
	}
	return hookContractErr
}

func provisionKimi(home string) error {
	dir := filepath.Join(home, ".kimi-code")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	confPath := filepath.Join(dir, "config.toml")
	// #nosec G304 -- sibling of the provisioned hooks file, same justification.
	raw, err := os.ReadFile(confPath)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if len(raw) > 0 {
		var doc map[string]any
		if err := toml.Unmarshal(raw, &doc); err != nil {
			return fmt.Errorf("%s does not parse as TOML: %w", confPath, err)
		}
		hooks, _ := doc["hooks"].([]any)
		for _, entry := range hooks {
			table, _ := entry.(map[string]any)
			if table["event"] == "SessionStart" {
				if cmd, _ := table["command"].(string); strings.TrimSpace(cmd) != "" {
					return nil // a SessionStart hook already exists
				}
			}
		}
	}
	// Appending a table cannot disturb existing content.
	// #nosec G304 -- sibling of the provisioned hooks file, same justification.
	f, err := os.OpenFile(confPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	if _, err := f.WriteString(kimiHookTOML); err != nil {
		return err
	}
	return nil
}

// Workspace trust, per project.
//
// Claude, Codex and Kimi each stop a launch in a directory they have not been
// trusted for, and each remembers the answer in its own state
// (docs/provider-contracts). A person at the pane answers three modals per
// project; provisioning owns provider configuration, so it can write those
// three records instead — one command per project rather than one modal per
// harness per project.
//
// What this does NOT do is decide. Trusting a directory is the user's
// call, and this only carries a decision they have already made by naming the
// path. It never overwrites a decision already recorded: a project Codex has
// recorded as untrusted stays untrusted, and the run says so.

// trustUsage is printed for any argument shape this does not implement.
const trustUsage = "usage: noisedeck-provision trust <absolute project directory>"

// runTrust seeds the workspace-trust record of every harness whose gate is
// measured, for one project directory. Each harness is attempted whatever the
// others do, like the boot path.
//
// The report lines are best effort: a stream that cannot be written to must
// not turn a completed configuration change into a failure. They are also
// the only place the detail lands — a seeded harness and one that already
// carried a decision both succeed — so the records on disk, not the exit
// code, are what a user reads back when the report is lost.
func runTrust(home string, args []string, out io.Writer) error {
	if len(args) != 2 || args[0] != "trust" {
		return errors.New(trustUsage)
	}
	dir := args[1]
	if !filepath.IsAbs(dir) {
		return fmt.Errorf("%s: %q is not absolute — the CLIs key their record on the working directory a launch passes", trustUsage, dir)
	}
	dir = filepath.Clean(dir)
	info, err := os.Stat(dir)
	if err != nil {
		return fmt.Errorf("project directory %s: %w", dir, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("project %s is not a directory", dir)
	}
	return errors.Join(
		trustClaude(home, dir, out),
		trustCodex(home, dir, out),
		trustKimi(home, dir, out),
		trustDevin(home, dir, out),
	)
}

// trustClaude records the accepted trust dialog in ~/.claude.json.
//
// That file is Claude's live configuration and Claude rewrites it while it
// runs, so the edit is staged in a sibling file and renamed over the original
// only after re-reading it and finding the bytes this edit was built on. That
// is best effort, not a compare-and-swap: a write landing between that
// re-read and the rename is still lost, and there is no lock to take. The
// precondition is the real protection — run this with no Claude session live
// on the same home.
func trustClaude(home, dir string, out io.Writer) error {
	path := filepath.Join(home, ".claude.json")
	// The swap renames a staged file over this path, which would replace a
	// SYMLINK rather than write through it — silently disconnecting the
	// user's managed configuration from the file Claude reads. os.Lstat
	// answers about the link itself.
	if info, err := os.Lstat(path); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%s is a symlink: this edit would replace the link instead of its target — trust the project by starting Claude in it once and accepting the prompt", path)
	} else if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("inspect %s: %w", path, err)
	}
	for range 3 {
		// #nosec G304 -- the fixed provider config under the service home.
		raw, err := os.ReadFile(path)
		if err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("read %s: %w", path, err)
		}
		// A shape this does not recognise is the user's to fix. Writing
		// over it would be this tool deciding what their configuration meant.
		var doc map[string]any
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &doc); err != nil {
				return fmt.Errorf("%s does not parse as JSON: %w", path, err)
			}
			if doc == nil {
				return fmt.Errorf("%s does not hold a JSON object", path)
			}
		} else {
			doc = map[string]any{}
		}
		projects, ok := doc["projects"].(map[string]any)
		if !ok {
			if existing, present := doc["projects"]; present && existing != nil {
				return fmt.Errorf(`%s has a "projects" value that is not an object (%T)`, path, existing)
			}
			projects = map[string]any{}
			doc["projects"] = projects
		}
		entry, ok := projects[dir].(map[string]any)
		if !ok {
			if existing, present := projects[dir]; present && existing != nil {
				return fmt.Errorf("%s has an entry for %s that is not an object (%T)", path, dir, existing)
			}
			entry = map[string]any{}
			projects[dir] = entry
		}
		if accepted, ok := entry["hasTrustDialogAccepted"].(bool); ok && accepted {
			_, _ = fmt.Fprintf(out, "claude: already trusted (%s)\n", path)
			return nil
		}
		entry["hasTrustDialogAccepted"] = true
		encoded, err := json.MarshalIndent(doc, "", "  ")
		if err != nil {
			return fmt.Errorf("encode %s: %w", path, err)
		}
		// A test seam standing in for the racing writer: this is the window
		// the swap narrows, and a test cannot hit it by racing.
		claudeConfigReadHook()
		swapped, err := swapIfUnchanged(path, raw, encoded)
		if err != nil {
			return err
		}
		if swapped {
			_, _ = fmt.Fprintf(out, "claude: trusted %s (%s)\n", dir, path)
			return nil
		}
	}
	return fmt.Errorf("%s changed under this edit three times: a claude session is writing it — stop it and re-run", path)
}

// claudeConfigReadHook runs in the window between reading Claude's live
// config and swapping the edit over it. It does nothing outside tests, which replace it to write
// the file in that window deliberately.
var claudeConfigReadHook = func() {}

// swapIfUnchanged renames a staged sibling file over path, after re-reading
// path and finding want. It reports whether the swap happened; false means
// the file had already moved. The re-read is a check, not a lock: a write
// between it and the rename is lost.
func swapIfUnchanged(path string, want, content []byte) (bool, error) {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".noisedeck-trust-*")
	if err != nil {
		return false, fmt.Errorf("stage %s: %w", path, err)
	}
	staged := tmp.Name()
	defer func() { _ = os.Remove(staged) }()
	if _, err := tmp.Write(content); err != nil {
		_ = tmp.Close()
		return false, fmt.Errorf("stage %s: %w", path, err)
	}
	if err := tmp.Close(); err != nil {
		return false, fmt.Errorf("stage %s: %w", path, err)
	}
	if err := os.Chmod(staged, 0o600); err != nil {
		return false, fmt.Errorf("stage %s: %w", path, err)
	}
	// #nosec G304 -- re-reading the same provider config to confirm it stands.
	current, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return false, fmt.Errorf("re-read %s: %w", path, err)
	}
	if !bytes.Equal(current, want) {
		return false, nil
	}
	if err := os.Rename(staged, path); err != nil {
		return false, fmt.Errorf("write %s: %w", path, err)
	}
	return true, nil
}

// trustCodex records trust_level = "trusted" for the project in Codex's
// config.toml, through the same parse-and-edit path the boot provisioning
// uses. A project that already carries a decision — including an explicit
// untrusted — keeps it.
func trustCodex(home, dir string, out io.Writer) error {
	confPath := filepath.Join(home, ".codex", "config.toml")
	// #nosec G304 -- the fixed provider config under the service home.
	raw, err := os.ReadFile(confPath)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("read %s: %w", confPath, err)
	}
	doc := map[string]any{}
	if len(raw) > 0 {
		if err := toml.Unmarshal(raw, &doc); err != nil {
			return fmt.Errorf("%s does not parse as TOML: %w", confPath, err)
		}
	}
	projects, ok := doc["projects"].(map[string]any)
	if !ok {
		if existing, present := doc["projects"]; present && existing != nil {
			return fmt.Errorf("%s has a [projects] value that is not a table (%T)", confPath, existing)
		}
		projects = map[string]any{}
		doc["projects"] = projects
	}
	entry, ok := projects[dir].(map[string]any)
	if !ok {
		if existing, present := projects[dir]; present && existing != nil {
			return fmt.Errorf("%s has an entry for %s that is not a table (%T)", confPath, dir, existing)
		}
		entry = map[string]any{}
		projects[dir] = entry
	}
	if level, ok := entry["trust_level"].(string); ok && level != "" {
		_, _ = fmt.Fprintf(out, "codex: already decided as %q (%s)\n", level, confPath)
		return nil
	}
	// The project's table is EXTENDED: an entry can carry settings beside the
	// trust decision, and replacing it would drop them.
	entry["trust_level"] = "trusted"
	if err := os.MkdirAll(filepath.Dir(confPath), 0o700); err != nil {
		return err
	}
	if len(raw) > 0 {
		if err := os.WriteFile(confPath+".noisedeck-bak", raw, 0o600); err != nil {
			return fmt.Errorf("back up %s: %w", confPath, err)
		}
	}
	encoded, err := toml.Marshal(doc)
	if err != nil {
		return fmt.Errorf("encode %s: %w", confPath, err)
	}
	if err := os.WriteFile(confPath, encoded, 0o600); err != nil {
		return fmt.Errorf("write %s: %w", confPath, err)
	}
	_, _ = fmt.Fprintf(out, "codex: trusted %s (%s)\n", dir, confPath)
	return nil
}

// trustKimi writes the one file Kimi files this directory's trust under. The
// NAME is load-bearing — Kimi ignores a record filed anywhere else — so it
// comes from the same helper the reader uses.
func trustKimi(home, dir string, out io.Writer) error {
	trustDir := filepath.Join(home, ".kimi-code", "workspace-trust")
	path := filepath.Join(trustDir, provider.KimiWorkspaceTrustFileName(dir))
	// #nosec G304 -- the per-directory trust record this tool writes.
	if raw, err := os.ReadFile(path); err == nil {
		var existing struct {
			Root string `json:"root"`
		}
		if json.Unmarshal(raw, &existing) == nil && existing.Root == dir {
			_, _ = fmt.Fprintf(out, "kimi: already trusted (%s)\n", path)
			return nil
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("read %s: %w", path, err)
	}
	if err := os.MkdirAll(trustDir, 0o700); err != nil {
		return err
	}
	record, err := json.Marshal(map[string]any{
		"root": dir, "trustedAt": time.Now().UnixMilli(),
	})
	if err != nil {
		return fmt.Errorf("encode %s: %w", path, err)
	}
	if err := os.WriteFile(path, record, 0o600); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	_, _ = fmt.Fprintf(out, "kimi: trusted %s (%s)\n", dir, path)
	return nil
}

// trustDevin appends the directory to the one central list Devin keeps,
// creating it with just this entry when Devin has never trusted anything.
// Devin rewrites the list itself when it answers a modal, so the edit
// reads the current bytes and rewrites them whole, like Claude's.
func trustDevin(home, dir string, out io.Writer) error {
	cliDir := filepath.Join(home, ".local", "share", "devin", "cli")
	path := filepath.Join(cliDir, "trusted_workspaces.json")
	// #nosec G304 -- the central trust list this tool writes.
	raw, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("read %s: %w", path, err)
	}
	var doc struct {
		TrustedPaths []string `json:"trusted_paths"`
	}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &doc); err != nil {
			return fmt.Errorf("%s does not parse as Devin's trust list: %w", path, err)
		}
	}
	for _, trusted := range doc.TrustedPaths {
		if trusted == dir {
			_, _ = fmt.Fprintf(out, "devin: already trusted (%s)\n", path)
			return nil
		}
	}
	doc.TrustedPaths = append(doc.TrustedPaths, dir)
	encoded, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return fmt.Errorf("encode %s: %w", path, err)
	}
	if err := os.MkdirAll(cliDir, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", cliDir, err)
	}
	if err := os.WriteFile(path, encoded, 0o600); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	_, _ = fmt.Fprintf(out, "devin: trusted %s (%s)\n", dir, path)
	return nil
}
