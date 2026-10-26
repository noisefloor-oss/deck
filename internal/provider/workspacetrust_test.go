package provider

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// The fixtures are the shapes measured on the real CLIs (2026-09-06): the
// claude config's projects map, codex's [projects.'<path>'] table, and one
// kimi file per directory named for a digest of its path.
func trustHome(t *testing.T) TrustHomes {
	t.Helper()
	home := t.TempDir()
	claude := map[string]any{"projects": map[string]any{
		"/workspace/deck":  map[string]any{"hasTrustDialogAccepted": true, "allowedTools": []string{}},
		"/workspace/other": map[string]any{"hasTrustDialogAccepted": false},
		"/workspace/never": map[string]any{"allowedTools": []string{}},
	}}
	raw, err := json.Marshal(claude)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".claude.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(home, ".codex"), 0o700); err != nil {
		t.Fatal(err)
	}
	codex := "model = 'gpt-6'\n\n[projects]\n[projects.'/workspace/deck']\ntrust_level = 'trusted'\n\n" +
		"[projects.'/workspace/other']\ntrust_level = 'untrusted'\n"
	if err := os.WriteFile(filepath.Join(home, ".codex", "config.toml"), []byte(codex), 0o600); err != nil {
		t.Fatal(err)
	}
	trustDir := filepath.Join(home, ".kimi-code", "workspace-trust")
	if err := os.MkdirAll(trustDir, 0o700); err != nil {
		t.Fatal(err)
	}
	record, err := json.Marshal(map[string]any{"root": "/workspace/deck", "trustedAt": 1786310744131})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(trustDir, KimiWorkspaceTrustFileName("/workspace/deck")), record, 0o600); err != nil {
		t.Fatal(err)
	}
	devinDir := filepath.Join(home, ".local", "share", "devin", "cli")
	if err := os.MkdirAll(devinDir, 0o700); err != nil {
		t.Fatal(err)
	}
	devin, err := json.Marshal(map[string]any{"trusted_paths": []string{"/workspace/deck"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(devinDir, "trusted_workspaces.json"), devin, 0o600); err != nil {
		t.Fatal(err)
	}
	return TrustHomes{Home: home}
}

func TestWorkspaceTrustReadsEachProvidersOwnRecord(t *testing.T) {
	homes := trustHome(t)
	for _, name := range []Name{Claude, Codex, Kimi, Devin} {
		if got := homes.WorkspaceTrust(name, "/workspace/deck"); got.State != WorkspaceTrustPrepared {
			t.Errorf("%s: prepared project read state %d (%s)", name, got.State, got.Reason)
		}
		// A project the record does not mention at all: the modal is what a
		// launch there meets.
		if got := homes.WorkspaceTrust(name, "/workspace/fresh"); got.State != WorkspaceTrustAbsent {
			t.Errorf("%s: unrecorded project read state %d (%s)", name, got.State, got.Reason)
		}
		if got := homes.WorkspaceTrust(name, "/workspace/deck"); got.RecordPath == "" {
			t.Errorf("%s: a result with no record path names nothing a user can act on", name)
		}
	}
}

// Devin inherits: a subdirectory of a listed path is covered, which is
// what a launch there measured (no modal, straight into the REPL on
// 3000.11.205). Kimi does not, so the same probe distinguishes them.
func TestDevinTrustCarriesToSubdirectories(t *testing.T) {
	homes := trustHome(t)
	if got := homes.WorkspaceTrust(Devin, "/workspace/deck/internal"); got.State != WorkspaceTrustPrepared {
		t.Errorf("devin subdirectory of a trusted path read %d (%s), want prepared", got.State, got.Reason)
	}
	if got := homes.WorkspaceTrust(Kimi, "/workspace/deck/internal"); got.State != WorkspaceTrustAbsent {
		t.Errorf("kimi subdirectory of a trusted path read %d (%s), want absent", got.State, got.Reason)
	}
	if got := homes.WorkspaceTrust(Devin, "/workspace/deckevil"); got.State != WorkspaceTrustAbsent {
		t.Errorf("a path sharing a prefix but not an ancestor read %d (%s), want absent", got.State, got.Reason)
	}
}

// A record listing the root covers every path, as the inheritance rule
// already says: excluding it would let a seeded "/" read back absent.
func TestDevinTrustRootCoversEveryPath(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, ".local", "share", "devin", "cli")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(map[string]any{"trusted_paths": []string{"/"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "trusted_workspaces.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	homes := TrustHomes{Home: home}
	for _, path := range []string{"/workspace/deck", "/home/x/y"} {
		if got := homes.WorkspaceTrust(Devin, path); got.State != WorkspaceTrustPrepared {
			t.Errorf("root-listed record read %d for %s (%s)", got.State, path, got.Reason)
		}
	}
}

// A recorded refusal is not the same thing in both CLIs: Claude asks again
// when its entry says the dialog was not accepted, while Codex takes an
// untrusted project as decided and starts without a prompt. Reading Codex's
// refusal as a missing answer would report a gate it does not have.
func TestWorkspaceTrustReadsARecordedRefusalAsEachCLIDoes(t *testing.T) {
	homes := trustHome(t)
	if got := homes.WorkspaceTrust(Claude, "/workspace/other"); got.State != WorkspaceTrustAbsent {
		t.Errorf("claude: hasTrustDialogAccepted=false read state %d", got.State)
	}
	if got := homes.WorkspaceTrust(Codex, "/workspace/other"); got.State != WorkspaceTrustPrepared {
		t.Errorf("codex: trust_level=untrusted read state %d", got.State)
	}
	// An entry that carries no verdict at all is not an acceptance either.
	if got := homes.WorkspaceTrust(Claude, "/workspace/never"); got.State != WorkspaceTrustAbsent {
		t.Errorf("claude: entry without the key read state %d", got.State)
	}
}

// Kimi looks a record up by name, and the name is a digest of the path, so a
// record whose root says otherwise is a stale file or a collision — never
// this project's answer.
func TestKimiTrustRecordMustNameItsOwnProject(t *testing.T) {
	homes := trustHome(t)
	path := filepath.Join(homes.Home, ".kimi-code", "workspace-trust", KimiWorkspaceTrustFileName("/workspace/fresh"))
	if err := os.WriteFile(path, []byte(`{"root":"/workspace/deck","trustedAt":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := homes.WorkspaceTrust(Kimi, "/workspace/fresh"); got.State != WorkspaceTrustAbsent {
		t.Errorf("a record naming another root read state %d", got.State)
	}
}

// The name Kimi files a record under is load-bearing: a record under any
// other name is invisible to it (measured — renaming one brought the modal
// back), so provisioning and this reader must agree on the exact string.
func TestKimiTrustFileNameIsThePathDigest(t *testing.T) {
	if got := KimiWorkspaceTrustFileName("/workspace/deck"); got != "wd_deck_b6318286a380" {
		t.Errorf("kimi trust file name = %q, want the name the real CLI reads", got)
	}
	if got := KimiWorkspaceTrustFileName("/workspace/landing"); got != "wd_landing_af0d418a6f30" {
		t.Errorf("kimi trust file name = %q", got)
	}
}

// Every uncertainty is Unknown, because a gate reported from one would be a
// gate this reader invented. A harness with no measured gate, a provider that has
// never written a config, a relocated claude config dir, and an unparseable
// file are all uncertainties.
func TestWorkspaceTrustIsUnknownRatherThanAGuess(t *testing.T) {
	empty := TrustHomes{Home: t.TempDir()}
	for _, name := range []Name{Claude, Codex} {
		if got := empty.WorkspaceTrust(name, "/workspace/deck"); got.State != WorkspaceTrustUnknown {
			t.Errorf("%s: a missing config read state %d", name, got.State)
		}
	}
	if got := empty.WorkspaceTrust(Kimi, "/workspace/deck"); got.State != WorkspaceTrustUnknown {
		t.Errorf("kimi: a missing home read state %d", got.State)
	}
	for _, name := range []Name{Opencode, Shell, Mimo} {
		if got := trustHome(t).WorkspaceTrust(name, "/workspace/fresh"); got.State != WorkspaceTrustUnknown {
			t.Errorf("%s has no measured gate but read state %d", name, got.State)
		}
	}
	relocated := trustHome(t)
	relocated.ClaudeConfigDir = "/somewhere/else"
	if got := relocated.WorkspaceTrust(Claude, "/workspace/fresh"); got.State != WorkspaceTrustUnknown {
		t.Errorf("a relocated claude config read state %d", got.State)
	}
	broken := trustHome(t)
	if err := os.WriteFile(filepath.Join(broken.Home, ".claude.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := broken.WorkspaceTrust(Claude, "/workspace/deck"); got.State != WorkspaceTrustUnknown {
		t.Errorf("an unparseable claude config read state %d", got.State)
	}
	if got := trustHome(t).WorkspaceTrust(Claude, "  "); got.State != WorkspaceTrustUnknown {
		t.Errorf("an empty project path read state %d", got.State)
	}
}

// The homes come from the environment a launch inherits, and each provider's
// own override has to be honoured or the reader answers about a config the
// launch will not use.
func TestWorkspaceTrustHonoursTheProviderHomeOverrides(t *testing.T) {
	fixture := trustHome(t)
	relocated := TrustHomes{
		Home:         t.TempDir(),
		CodexHome:    filepath.Join(fixture.Home, ".codex"),
		KimiCodeHome: filepath.Join(fixture.Home, ".kimi-code"),
	}
	if got := relocated.WorkspaceTrust(Codex, "/workspace/deck"); got.State != WorkspaceTrustPrepared {
		t.Errorf("CODEX_HOME was not read: state %d (%s)", got.State, got.Reason)
	}
	if got := relocated.WorkspaceTrust(Kimi, "/workspace/deck"); got.State != WorkspaceTrustPrepared {
		t.Errorf("KIMI_CODE_HOME was not read: state %d (%s)", got.State, got.Reason)
	}
}

// Claude and Codex answer a directory from a decision recorded further up:
// a fresh directory under a trusted parent, and a subdirectory of a trusted
// repository, both launched with no modal (measured 2026-09-06). Reading the
// exact key alone would report gates that are not there.
func TestWorkspaceTrustFollowsAnInheritedDecision(t *testing.T) {
	homes := trustHome(t)
	for _, name := range []Name{Claude, Codex} {
		if got := homes.WorkspaceTrust(name, "/workspace/deck/internal/session"); got.State != WorkspaceTrustPrepared {
			t.Errorf("%s: a directory under a trusted project read state %d", name, got.State)
		}
		// Inheritance is downward only: a sibling of a trusted project is not
		// covered by it, and neither is its parent.
		if got := homes.WorkspaceTrust(name, "/workspace/deck-fork"); got.State != WorkspaceTrustAbsent {
			t.Errorf("%s: a sibling of a trusted project read state %d", name, got.State)
		}
		if got := homes.WorkspaceTrust(name, "/workspace"); got.State != WorkspaceTrustAbsent {
			t.Errorf("%s: the parent of a trusted project read state %d", name, got.State)
		}
	}
}

// Kimi does not inherit: a subdirectory of a trusted project met the modal
// (measured). Its record is one file per exact directory, so the ancestor
// rule must not leak into it.
func TestKimiTrustDoesNotInherit(t *testing.T) {
	homes := trustHome(t)
	if got := homes.WorkspaceTrust(Kimi, "/workspace/deck/internal"); got.State != WorkspaceTrustAbsent {
		t.Errorf("a subdirectory of a trusted project read state %d", got.State)
	}
}

// A record's key is compared as a path, not as text: a trailing slash or a
// doubled separator names the same directory the launch will pass.
func TestWorkspaceTrustComparesRecordKeysAsPaths(t *testing.T) {
	home := t.TempDir()
	claude := `{"projects":{"/workspace/deck/":{"hasTrustDialogAccepted":true}}}`
	if err := os.WriteFile(filepath.Join(home, ".claude.json"), []byte(claude), 0o600); err != nil {
		t.Fatal(err)
	}
	homes := TrustHomes{Home: home}
	if got := homes.WorkspaceTrust(Claude, "/workspace/deck"); got.State != WorkspaceTrustPrepared {
		t.Errorf("a key with a trailing slash read state %d", got.State)
	}
	if got := homes.WorkspaceTrust(Claude, "/workspace//deck/sub"); got.State != WorkspaceTrustPrepared {
		t.Errorf("a doubled separator read state %d", got.State)
	}
	if err := os.MkdirAll(filepath.Join(home, ".codex"), 0o700); err != nil {
		t.Fatal(err)
	}
	codex := "[projects]\n[projects.'/workspace/deck/']\ntrust_level = 'trusted'\n"
	if err := os.WriteFile(filepath.Join(home, ".codex", "config.toml"), []byte(codex), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := homes.WorkspaceTrust(Codex, "/workspace/deck"); got.State != WorkspaceTrustPrepared {
		t.Errorf("codex: a key with a trailing slash read state %d", got.State)
	}
}

// The launch passes the environment's bytes, so this must read them: a home
// with a trailing space is a different directory, and answering from the
// trimmed one would report a gate for a project the launch's own config
// trusts.
func TestWorkspaceTrustDoesNotTrimProviderHomes(t *testing.T) {
	root := t.TempDir()
	spaced := filepath.Join(root, "codex ")
	if err := os.MkdirAll(spaced, 0o700); err != nil {
		t.Fatal(err)
	}
	trusted := "[projects]\n[projects.'/workspace/deck']\ntrust_level = 'trusted'\n"
	if err := os.WriteFile(filepath.Join(spaced, "config.toml"), []byte(trusted), 0o600); err != nil {
		t.Fatal(err)
	}
	// The untrimmed home is the one the launch reads.
	if got := (TrustHomes{Home: root, CodexHome: spaced}).WorkspaceTrust(Codex, "/workspace/deck"); got.State != WorkspaceTrustPrepared {
		t.Errorf("the home with a trailing space read state %d (%s)", got.State, got.Reason)
	}
	// And the trimmed one names a directory that does not exist here, which
	// is an uncertainty rather than an absence.
	untrimmed := filepath.Join(root, "codex")
	if got := (TrustHomes{Home: root, CodexHome: untrimmed}).WorkspaceTrust(Codex, "/workspace/deck"); got.State != WorkspaceTrustUnknown {
		t.Errorf("a home that does not exist read state %d", got.State)
	}
}

// A relative home resolves against the process's working directory, and the
// launch's is the project — two different directories. The fixture puts a
// REAL config at the relative path from this process's cwd, so a reader that
// resolved it here would answer prepared from a config the launch would never
// open.
func TestWorkspaceTrustRefusesToGuessARelativeHome(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	if err := os.MkdirAll(filepath.Join(root, "relative", "codex"), 0o700); err != nil {
		t.Fatal(err)
	}
	trusted := "[projects]\n[projects.'/workspace/deck']\ntrust_level = 'trusted'\n"
	if err := os.WriteFile(filepath.Join(root, "relative", "codex", "config.toml"), []byte(trusted), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "relative", "home"), 0o700); err != nil {
		t.Fatal(err)
	}
	claude := `{"projects":{"/workspace/deck":{"hasTrustDialogAccepted":true}}}`
	if err := os.WriteFile(filepath.Join(root, "relative", "home", ".claude.json"), []byte(claude), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := (TrustHomes{Home: "relative/home"}).WorkspaceTrust(Claude, "/workspace/deck"); got.State != WorkspaceTrustUnknown {
		t.Errorf("a relative HOME read state %d", got.State)
	}
	if got := (TrustHomes{Home: root, CodexHome: "relative/codex"}).WorkspaceTrust(Codex, "/workspace/deck"); got.State != WorkspaceTrustUnknown {
		t.Errorf("a relative CODEX_HOME read state %d", got.State)
	}
	dir := filepath.Join(root, "relative", "kimi", "workspace-trust")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	record, err := json.Marshal(map[string]any{"root": "/workspace/deck", "trustedAt": 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, KimiWorkspaceTrustFileName("/workspace/deck")), record, 0o600); err != nil {
		t.Fatal(err)
	}
	if got := (TrustHomes{Home: root, KimiCodeHome: "relative/kimi"}).WorkspaceTrust(Kimi, "/workspace/deck"); got.State != WorkspaceTrustUnknown {
		t.Errorf("a relative KIMI_CODE_HOME read state %d", got.State)
	}
}

// A project path that is not the absolute directory a launch passes cannot be
// looked up in any of these records.
func TestWorkspaceTrustRequiresAnAbsoluteProjectPath(t *testing.T) {
	homes := trustHome(t)
	for _, path := range []string{"", "  ", "workspace/deck", "./deck"} {
		for _, name := range []Name{Claude, Codex, Kimi} {
			if got := homes.WorkspaceTrust(name, path); got.State != WorkspaceTrustUnknown {
				t.Errorf("%s with %q read state %d", name, path, got.State)
			}
		}
	}
}

// A linked worktree lives wherever it was created, and Codex carries its main
// checkout's decision to it (measured: a worktree of a trusted repository
// launched clean from a directory that is no descendant of it). The lexical
// ancestors alone would report a gate that is not there.
func TestWorkspaceTrustFollowsALinkedWorktreeToItsCheckout(t *testing.T) {
	root := t.TempDir()
	checkout := filepath.Join(root, "main")
	worktree := filepath.Join(root, "linked")
	gitDir := filepath.Join(checkout, ".git", "worktrees", "linked")
	for _, dir := range []string{gitDir, worktree} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(worktree, ".git"), []byte("gitdir: "+gitDir+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(gitDir, "commondir"), []byte("../..\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".codex"), 0o700); err != nil {
		t.Fatal(err)
	}
	codex := "[projects]\n[projects.'" + checkout + "']\ntrust_level = 'trusted'\n"
	if err := os.WriteFile(filepath.Join(home, ".codex", "config.toml"), []byte(codex), 0o600); err != nil {
		t.Fatal(err)
	}
	claude := `{"projects":{"` + checkout + `":{"hasTrustDialogAccepted":true}}}`
	if err := os.WriteFile(filepath.Join(home, ".claude.json"), []byte(claude), 0o600); err != nil {
		t.Fatal(err)
	}
	homes := TrustHomes{Home: home}
	for _, name := range []Name{Codex, Claude} {
		if got := homes.WorkspaceTrust(name, worktree); got.State != WorkspaceTrustPrepared {
			t.Errorf("%s: a linked worktree of a trusted checkout read state %d", name, got.State)
		}
		if got := homes.WorkspaceTrust(name, filepath.Join(worktree, "sub")); got.State != WorkspaceTrustPrepared {
			t.Errorf("%s: a directory inside that worktree read state %d", name, got.State)
		}
	}
	// A worktree whose checkout carries no decision is still absent: the
	// resolution adds candidates, it does not trust them. The configs here
	// are readable and name a DIFFERENT project, so both readers run.
	bare := t.TempDir()
	if err := os.MkdirAll(filepath.Join(bare, ".codex"), 0o700); err != nil {
		t.Fatal(err)
	}
	elsewhere := "[projects]\n[projects.'/somewhere/else']\ntrust_level = 'trusted'\n"
	if err := os.WriteFile(filepath.Join(bare, ".codex", "config.toml"), []byte(elsewhere), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bare, ".claude.json"),
		[]byte(`{"projects":{"/somewhere/else":{"hasTrustDialogAccepted":true}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, name := range []Name{Codex, Claude} {
		if got := (TrustHomes{Home: bare}).WorkspaceTrust(name, worktree); got.State != WorkspaceTrustAbsent {
			t.Errorf("%s: a worktree of an undecided checkout read state %d", name, got.State)
		}
		if got := (TrustHomes{Home: bare}).WorkspaceTrust(name, filepath.Join(worktree, "sub")); got.State != WorkspaceTrustAbsent {
			t.Errorf("%s: inside a worktree of an undecided checkout read state %d", name, got.State)
		}
	}
}

// A .git file that does not name a resolvable worktree adds no candidates,
// rather than a directory guessed from a partial parse.
func TestWorkspaceTrustIgnoresAnUnresolvableWorktreeLink(t *testing.T) {
	home := trustHome(t)
	root := t.TempDir()
	for _, content := range []string{"not a gitdir line", "gitdir: /nonexistent/path", ""} {
		dir := filepath.Join(root, "wt")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, ".git"), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		if got := home.WorkspaceTrust(Claude, dir); got.State != WorkspaceTrustAbsent {
			t.Errorf("%q: read state %d", content, got.State)
		}
	}
}

// The homes come from the environment a launch inherits, so the reading of
// that environment is part of the contract: a value is used as it stands.
func TestTrustHomesFromEnvironmentReadsTheBytesAsTheyAre(t *testing.T) {
	// The fixture home TRUSTS the project, so a reader that treated the
	// whitespace CLAUDE_CONFIG_DIR below as unset would fall back to it and
	// answer prepared for a config the launch would not read.
	fixture := trustHome(t)
	t.Setenv("HOME", fixture.Home)
	t.Setenv("CODEX_HOME", " /srv/codex")
	t.Setenv("KIMI_CODE_HOME", "/srv/kimi\t")
	t.Setenv("CLAUDE_CONFIG_DIR", "  ")
	homes := TrustHomesFromEnvironment()
	if homes.Home != fixture.Home || homes.CodexHome != " /srv/codex" ||
		homes.KimiCodeHome != "/srv/kimi\t" || homes.ClaudeConfigDir != "  " {
		t.Fatalf("environment was rewritten: %+v", homes)
	}
	// A CLAUDE_CONFIG_DIR of whitespace is still a set value, and where the
	// relocated record lives is unmeasured.
	if got := homes.WorkspaceTrust(Claude, "/workspace/deck"); got.State != WorkspaceTrustUnknown {
		t.Errorf("a whitespace CLAUDE_CONFIG_DIR read state %d", got.State)
	}
}
