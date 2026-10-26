package main

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/pelletier/go-toml/v2"

	"github.com/noisefloor-oss/deck/internal/provider"
)

// trustFixture is one service home and one project directory to trust.
func trustFixture(t *testing.T) (home, project string) {
	t.Helper()
	return t.TempDir(), t.TempDir()
}

// readTrust answers the same question the readiness diagnostic does, so these
// tests assert the seeded records against the reader that consumes them
// rather than against their own idea of the format.
func readTrust(t *testing.T, home, project string, name provider.Name) provider.WorkspaceTrustState {
	t.Helper()
	return provider.TrustHomes{Home: home}.WorkspaceTrust(name, project).State
}

func TestTrustSeedsEveryMeasuredHarness(t *testing.T) {
	home, project := trustFixture(t)
	var out strings.Builder
	if err := runTrust(home, []string{"trust", project}, &out); err != nil {
		t.Fatalf("runTrust: %v", err)
	}
	for _, name := range []provider.Name{provider.Claude, provider.Codex, provider.Kimi, provider.Devin} {
		if got := readTrust(t, home, project, name); got != provider.WorkspaceTrustPrepared {
			t.Errorf("%s reads state %d after seeding", name, got)
		}
		if !strings.Contains(out.String(), string(name)+":") {
			t.Errorf("the run does not say what it did for %s: %s", name, out.String())
		}
	}
	// The record path is in the output: a user who wants to see or undo
	// the decision needs the file, not the verb.
	if !strings.Contains(out.String(), provider.KimiWorkspaceTrustFileName(project)) {
		t.Errorf("the run does not name kimi's record: %s", out.String())
	}
}

// Running it twice is a no-op that says so, and writes nothing new: the
// second pass must not restate a decision as if it had just made it.
func TestTrustIsIdempotent(t *testing.T) {
	home, project := trustFixture(t)
	if err := runTrust(home, []string{"trust", project}, io.Discard); err != nil {
		t.Fatalf("first run: %v", err)
	}
	before := map[string][]byte{}
	for _, path := range []string{
		filepath.Join(home, ".claude.json"),
		filepath.Join(home, ".codex", "config.toml"),
		filepath.Join(home, ".kimi-code", "workspace-trust", provider.KimiWorkspaceTrustFileName(project)),
		filepath.Join(home, ".local", "share", "devin", "cli", "trusted_workspaces.json"),
	} {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		before[path] = raw
	}
	var out strings.Builder
	if err := runTrust(home, []string{"trust", project}, &out); err != nil {
		t.Fatalf("second run: %v", err)
	}
	for path, want := range before {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if string(raw) != string(want) {
			t.Errorf("%s was rewritten by an idempotent run", path)
		}
	}
	if !strings.Contains(out.String(), "already trusted") {
		t.Errorf("the second run does not say the decision already stood: %s", out.String())
	}
}

// The user's other configuration is theirs. Seeding one project must not
// disturb another project's entry, or anything else in the file.
func TestTrustPreservesEveryOtherSetting(t *testing.T) {
	home, project := trustFixture(t)
	claude := `{"numStartups":41,"projects":{` +
		`"/elsewhere":{"hasTrustDialogAccepted":true,"allowedTools":["Bash"]},` +
		`"` + project + `":{"allowedTools":["Read"],"history":[{"display":"x"}]}}}`
	if err := os.WriteFile(filepath.Join(home, ".claude.json"), []byte(claude), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(home, ".codex"), 0o700); err != nil {
		t.Fatal(err)
	}
	codex := "model = 'gpt-6'\n\n[features]\nhooks = true\n\n[projects]\n[projects.'/elsewhere']\ntrust_level = 'trusted'\n"
	if err := os.WriteFile(filepath.Join(home, ".codex", "config.toml"), []byte(codex), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := runTrust(home, []string{"trust", project}, io.Discard); err != nil {
		t.Fatalf("runTrust: %v", err)
	}
	var claudeDoc struct {
		NumStartups int `json:"numStartups"`
		Projects    map[string]struct {
			HasTrustDialogAccepted bool     `json:"hasTrustDialogAccepted"`
			AllowedTools           []string `json:"allowedTools"`
		} `json:"projects"`
	}
	raw, err := os.ReadFile(filepath.Join(home, ".claude.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &claudeDoc); err != nil {
		t.Fatal(err)
	}
	if claudeDoc.NumStartups != 41 {
		t.Errorf("claude: an unrelated top-level setting was lost")
	}
	if other := claudeDoc.Projects["/elsewhere"]; !other.HasTrustDialogAccepted || len(other.AllowedTools) != 1 {
		t.Errorf("claude: another project's entry was rewritten: %+v", other)
	}
	// The seeded project's OWN entry keeps everything else it carried: the
	// edit adds a key, it does not replace a user's project record.
	if seeded := claudeDoc.Projects[project]; !seeded.HasTrustDialogAccepted || len(seeded.AllowedTools) != 1 {
		t.Errorf("claude: the project's existing entry was replaced: %+v", seeded)
	}
	var codexDoc struct {
		Model    string `toml:"model"`
		Features struct {
			Hooks bool `toml:"hooks"`
		} `toml:"features"`
		Projects map[string]struct {
			TrustLevel string `toml:"trust_level"`
		} `toml:"projects"`
	}
	raw, err = os.ReadFile(filepath.Join(home, ".codex", "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if err := toml.Unmarshal(raw, &codexDoc); err != nil {
		t.Fatal(err)
	}
	if codexDoc.Model != "gpt-6" || !codexDoc.Features.Hooks {
		t.Errorf("codex: unrelated settings were lost: %+v", codexDoc)
	}
	if codexDoc.Projects["/elsewhere"].TrustLevel != "trusted" {
		t.Errorf("codex: another project's decision was rewritten")
	}
	if codexDoc.Projects[project].TrustLevel != "trusted" {
		t.Errorf("codex: the named project was not trusted")
	}
	// The user's file is backed up before it is edited, like every other
	// config this tool touches.
	if _, err := os.Stat(filepath.Join(home, ".codex", "config.toml.noisedeck-bak")); err != nil {
		t.Errorf("codex: no backup beside the edited config: %v", err)
	}
}

// A decision already recorded is the user's, including a refusal. Codex
// records untrusted as a decision it acts on, so overwriting it would reverse
// an answer the user gave.
func TestTrustNeverOverwritesARecordedDecision(t *testing.T) {
	home, project := trustFixture(t)
	if err := os.MkdirAll(filepath.Join(home, ".codex"), 0o700); err != nil {
		t.Fatal(err)
	}
	codex := "[projects]\n[projects.'" + project + "']\ntrust_level = 'untrusted'\n"
	if err := os.WriteFile(filepath.Join(home, ".codex", "config.toml"), []byte(codex), 0o600); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	if err := runTrust(home, []string{"trust", project}, &out); err != nil {
		t.Fatalf("runTrust: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(home, ".codex", "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "untrusted") {
		t.Errorf("codex: a recorded refusal was overwritten: %s", raw)
	}
	if !strings.Contains(out.String(), `already decided as "untrusted"`) {
		t.Errorf("the run does not report the standing decision: %s", out.String())
	}
}

// Claude's config is live: it rewrites the file while it runs. A change this
// SEES — one landing before the final re-read — is refused rather than
// overwritten, with the remedy. The window between that re-read and the
// rename stays open, which is why the documented precondition is no live
// Claude session.
func TestTrustRefusesToClobberAClaudeConfigThatMovedUnderIt(t *testing.T) {
	home, project := trustFixture(t)
	path := filepath.Join(home, ".claude.json")
	if err := os.WriteFile(path, []byte(`{"numStartups":1,"projects":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	writes := 0
	claudeConfigReadHook = func() {
		writes++
		if err := os.WriteFile(path, []byte(`{"numStartups":`+strconv.Itoa(writes+1)+`,"projects":{}}`), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { claudeConfigReadHook = func() {} })

	err := trustClaude(home, project, io.Discard)
	if err == nil {
		t.Fatal("an edit landed on a config that moved under it")
	}
	if !strings.Contains(err.Error(), "re-run") {
		t.Errorf("the failure does not tell the user what to do: %v", err)
	}
	if writes != 3 {
		t.Errorf("the edit was attempted %d times, want three before giving up", writes)
	}
	// The other writer's last value stands: the edit did not land at all.
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != `{"numStartups":4,"projects":{}}` {
		t.Errorf("the racing writer's config was overwritten: %s", raw)
	}
}

// And when the file holds still, the edit lands on exactly those bytes.
func TestTrustEditsAClaudeConfigThatHoldsStill(t *testing.T) {
	home, project := trustFixture(t)
	path := filepath.Join(home, ".claude.json")
	if err := os.WriteFile(path, []byte(`{"numStartups":1,"projects":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := trustClaude(home, project, io.Discard); err != nil {
		t.Fatalf("trustClaude: %v", err)
	}
	if got := readTrust(t, home, project, provider.Claude); got != provider.WorkspaceTrustPrepared {
		t.Errorf("claude reads state %d after seeding", got)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	// The swap renames a staged file over the original, so the mode is the
	// staged one: provider configuration stays owner-only.
	if info.Mode().Perm() != 0o600 {
		t.Errorf("the swapped config is mode %v", info.Mode().Perm())
	}
}

// The argument shape is part of the contract: a relative path names a
// directory that depends on where the command was run, and these records key
// on the absolute directory a launch passes.
func TestTrustRefusesAnArgumentItCannotAct(t *testing.T) {
	home, project := trustFixture(t)
	// A relative path that EXISTS from here, so only its shape refuses it.
	t.Chdir(filepath.Dir(project))
	for _, args := range [][]string{
		{},
		{"trust"},
		{"trust", filepath.Base(project)},
		{"trust", project, "extra"},
		{"seed", project},
		{"trust", filepath.Join(project, "missing")},
	} {
		if err := runTrust(home, args, io.Discard); err == nil {
			t.Errorf("%v was accepted", args)
		}
	}
	// A file is not a project directory.
	file := filepath.Join(project, "file")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := runTrust(home, []string{"trust", file}, io.Discard); err == nil {
		t.Error("a file was accepted as a project directory")
	}
}

// A project's entry can carry settings beside the trust decision. Both files
// are EXTENDED, never replaced: dropping a user's per-project settings
// to record one key would be this tool losing their configuration.
func TestTrustExtendsTheNamedProjectsOwnEntry(t *testing.T) {
	home, project := trustFixture(t)
	if err := os.WriteFile(filepath.Join(home, ".claude.json"),
		[]byte(`{"projects":{"`+project+`":{"allowedTools":["Read"],"lastCost":1.5}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(home, ".codex"), 0o700); err != nil {
		t.Fatal(err)
	}
	codex := "[projects]\n[projects.'" + project + "']\nsandbox_mode = 'workspace-write'\n"
	if err := os.WriteFile(filepath.Join(home, ".codex", "config.toml"), []byte(codex), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := runTrust(home, []string{"trust", project}, io.Discard); err != nil {
		t.Fatalf("runTrust: %v", err)
	}
	var claudeDoc struct {
		Projects map[string]struct {
			HasTrustDialogAccepted bool     `json:"hasTrustDialogAccepted"`
			AllowedTools           []string `json:"allowedTools"`
			LastCost               float64  `json:"lastCost"`
		} `json:"projects"`
	}
	raw, err := os.ReadFile(filepath.Join(home, ".claude.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &claudeDoc); err != nil {
		t.Fatal(err)
	}
	entry := claudeDoc.Projects[project]
	if !entry.HasTrustDialogAccepted || len(entry.AllowedTools) != 1 || entry.LastCost != 1.5 {
		t.Errorf("claude: the entry was replaced rather than extended: %+v", entry)
	}
	var codexDoc struct {
		Projects map[string]struct {
			TrustLevel  string `toml:"trust_level"`
			SandboxMode string `toml:"sandbox_mode"`
		} `toml:"projects"`
	}
	raw, err = os.ReadFile(filepath.Join(home, ".codex", "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if err := toml.Unmarshal(raw, &codexDoc); err != nil {
		t.Fatal(err)
	}
	if got := codexDoc.Projects[project]; got.TrustLevel != "trusted" || got.SandboxMode != "workspace-write" {
		t.Errorf("codex: the table was replaced rather than extended: %+v", got)
	}
}

// A shape this tool does not recognise is the user's to fix. Every one of
// these refuses with the file named, and leaves the bytes exactly as they
// were — rewriting them would be this tool deciding what they meant.
func TestTrustRefusesAConfigItDoesNotRecognise(t *testing.T) {
	for _, tc := range []struct {
		name, file, content, want string
	}{
		{"claude null", ".claude.json", "null", "does not hold a JSON object"},
		{"claude scalar root", ".claude.json", `"nope"`, "does not parse as JSON"},
		{"claude projects list", ".claude.json", `{"projects":[]}`, "not an object"},
		{"claude entry string", ".claude.json", `{"projects":{"PROJECT":"trusted"}}`, "not an object"},
		{"claude unparseable", ".claude.json", "{not json", "does not parse as JSON"},
		{"codex projects scalar", ".codex/config.toml", "projects = 1\n", "not a table"},
		{"codex entry scalar", ".codex/config.toml", "[projects]\nPROJECT = 'trusted'\n", "not a table"},
		{"codex unparseable", ".codex/config.toml", "[projects\n", "does not parse as TOML"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home, project := trustFixture(t)
			path := filepath.Join(home, tc.file)
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatal(err)
			}
			content := strings.ReplaceAll(tc.content, "PROJECT", project)
			if strings.HasSuffix(tc.file, ".toml") {
				content = strings.ReplaceAll(content, project, "'"+project+"'")
			}
			if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			err := runTrust(home, []string{"trust", project}, io.Discard)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want one naming %q", err, tc.want)
			}
			if !strings.Contains(err.Error(), path) {
				t.Errorf("the refusal does not name the file: %v", err)
			}
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if string(raw) != content {
				t.Errorf("the unrecognised config was rewritten: %s", raw)
			}
		})
	}
}

// Renaming over a symlink replaces the LINK, which would disconnect Claude
// from the configuration the user manages elsewhere.
func TestTrustRefusesToReplaceASymlinkedClaudeConfig(t *testing.T) {
	for _, target := range []string{"real.json", "missing.json"} {
		t.Run(target, func(t *testing.T) {
			home, project := trustFixture(t)
			path := filepath.Join(home, ".claude.json")
			managed := filepath.Join(home, target)
			if target == "real.json" {
				if err := os.WriteFile(managed, []byte(`{"projects":{}}`), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Symlink(managed, path); err != nil {
				t.Fatal(err)
			}
			err := trustClaude(home, project, io.Discard)
			if err == nil || !strings.Contains(err.Error(), "symlink") {
				t.Fatalf("error = %v, want a refusal naming the symlink", err)
			}
			info, statErr := os.Lstat(path)
			if statErr != nil {
				t.Fatal(statErr)
			}
			if info.Mode()&os.ModeSymlink == 0 {
				t.Error("the symlink was replaced by a regular file")
			}
		})
	}
}
