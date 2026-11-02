package project

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// inspectRoot lays out the shapes the add-project field has to tell apart:
// a repository with a remote, a linked worktree of it, a plain directory,
// a plain directory inside the repository, and a clone already on disk.
func inspectRoot(t *testing.T) string {
	t.Helper()
	root := mkdir(t, filepath.Join(t.TempDir(), "root"))
	repo := mkdir(t, filepath.Join(root, "deck"))
	testGit(t, repo, "init", "--quiet", "--initial-branch=main")
	testGit(t, repo, "config", "user.name", "Test Author")
	testGit(t, repo, "config", "user.email", "test@example.invalid")
	testGit(t, repo, "remote", "add", "origin", "git@github.com:fable/deck.git")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("deck\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mkdir(t, filepath.Join(repo, "src"))
	testGit(t, repo, "add", ".")
	testGit(t, repo, "commit", "--quiet", "-m", "first")
	mkdir(t, filepath.Join(root, "tmp"))
	testGit(t, repo, "worktree", "add", "--quiet", filepath.Join(root, "tmp", "vd-gate2"), "-b", "feat/gate2")
	mkdir(t, filepath.Join(root, "plain", "one"))
	mkdir(t, filepath.Join(root, "plain", "two"))
	return root
}

func inspect(t *testing.T, root, input, base string) *Inspection {
	t.Helper()
	out, err := Inspect(context.Background(), input, base, []string{root}, func(path string) string {
		if filepath.Base(path) == "deck" {
			return "Deck"
		}
		return ""
	})
	if err != nil {
		t.Fatalf("inspect %q: %v", input, err)
	}
	return out
}

func TestInspectAbsentPathResolvesUnderTheRootWithItsOwnStore(t *testing.T) {
	root := inspectRoot(t)
	out := inspect(t, root, "brand-new-thing", "")
	if out.Kind != KindAbsent || out.Exists || out.Path != filepath.Join(root, "brand-new-thing") {
		t.Fatalf("absent: %+v", out)
	}
	if out.Parent != nil || out.StorePath != filepath.Join(root, "brand-new-thing", ".git", storeRelPath) {
		t.Fatalf("absent store: %+v", out)
	}
}

func TestInspectPlainDirectoryListsWhatIsThereAndOwnsNoStore(t *testing.T) {
	root := inspectRoot(t)
	out := inspect(t, root, filepath.Join(root, "plain"), "")
	if out.Kind != KindPlain || !out.Exists || out.Parent != nil {
		t.Fatalf("plain: %+v", out)
	}
	if strings.Join(out.Entries, ",") != "one,two" || out.StorePath != "" {
		t.Fatalf("plain entries/store: %+v", out)
	}
}

func TestInspectRepositoryRootIsRecognised(t *testing.T) {
	root := inspectRoot(t)
	out := inspect(t, root, "deck", "")
	if out.Kind != KindRepository || out.Parent != nil {
		t.Fatalf("repository: %+v", out)
	}
	if out.Remote != "github.com/fable/deck" {
		t.Fatalf("remote = %q", out.Remote)
	}
	if !strings.HasPrefix(out.Head, "main · ") || !strings.HasSuffix(out.Head, " · working tree clean") {
		t.Fatalf("head = %q", out.Head)
	}
	if out.StorePath != filepath.Join(root, "deck", ".git", storeRelPath) {
		t.Fatalf("store = %q", out.StorePath)
	}
	if out.Entries[0] != ".git" || out.Entries[1] != "src" {
		t.Fatalf("entries = %v", out.Entries)
	}
}

func TestInspectWorktreeNamesItsMainRepositoryAndShareItsStore(t *testing.T) {
	root := inspectRoot(t)
	out := inspect(t, root, filepath.Join(root, "tmp", "vd-gate2"), "")
	if out.Kind != KindWorktree || out.Parent == nil {
		t.Fatalf("worktree: %+v", out)
	}
	if out.Parent.Path != filepath.Join(root, "deck") || out.Parent.Name != "Deck" {
		t.Fatalf("parent = %+v", out.Parent)
	}
	if out.StorePath != filepath.Join(root, "deck", ".git", storeRelPath) {
		t.Fatalf("a worktree cannot have its own store: %q", out.StorePath)
	}
	if !strings.HasPrefix(out.Head, "feat/gate2 · ") {
		t.Fatalf("head = %q", out.Head)
	}
}

func TestInspectInsideARepositoryBindsToItsParent(t *testing.T) {
	root := inspectRoot(t)
	existing := inspect(t, root, filepath.Join(root, "deck", "src"), "")
	if existing.Kind != KindPlain || existing.Parent == nil || existing.Parent.Path != filepath.Join(root, "deck") {
		t.Fatalf("plain inside: %+v", existing)
	}
	absent := inspect(t, root, filepath.Join(root, "deck", "not-yet"), "")
	if absent.Kind != KindAbsent || absent.Parent == nil || absent.Parent.Path != filepath.Join(root, "deck") {
		t.Fatalf("absent inside: %+v", absent)
	}
	// The recorded tie: a nonexistent owner/repo and a nonexistent relative
	// path are the same string, and the shorthand wins because an absent
	// path is already served by typing it absolutely.
	if tie := inspect(t, root, "deck/not-yet", ""); tie.Kind != KindClone {
		t.Fatalf("shorthand tie resolves toward clone: %+v", tie)
	}
	for _, out := range []*Inspection{existing, absent} {
		if out.StorePath != filepath.Join(root, "deck", ".git", storeRelPath) {
			t.Fatalf("the parent's store governs: %q", out.StorePath)
		}
	}
}

func TestInspectShorthandClonesUnlessTheRelativePathExists(t *testing.T) {
	root := inspectRoot(t)
	clone := inspect(t, root, "fable/genuinely-new", "")
	if clone.Kind != KindClone || clone.Exists || clone.Path != filepath.Join(root, "genuinely-new") {
		t.Fatalf("shorthand clone: %+v", clone)
	}
	if clone.CloneURL != "https://github.com/fable/genuinely-new" || clone.StorePath != filepath.Join(root, "genuinely-new", ".git", storeRelPath) {
		t.Fatalf("shorthand clone url/store: %+v", clone)
	}
	shadowed := inspect(t, root, "tmp/vd-gate2", "")
	if shadowed.Kind != KindWorktree {
		t.Fatalf("a relative path that exists is a path, not a clone: %+v", shadowed)
	}
}

func TestInspectCloneDestinationStates(t *testing.T) {
	root := inspectRoot(t)
	same := inspect(t, root, "https://github.com/fable/deck.git", "")
	if same.Kind != KindClone || !same.SameClone || same.Path != filepath.Join(root, "deck") {
		t.Fatalf("same clone: %+v", same)
	}
	short := inspect(t, root, "fable/deck", "")
	if !short.SameClone {
		t.Fatalf("the typed form may be shorter than the stored remote: %+v", short)
	}
	other := inspect(t, root, "github.com/other/deck", "")
	if other.SameClone || !other.Exists || other.Entries[0] != ".git" {
		t.Fatalf("a different remote at the destination is a collision: %+v", other)
	}
	if other.CloneURL != "https://github.com/other/deck" {
		t.Fatalf("hosted clone url = %q", other.CloneURL)
	}
	nested := inspect(t, root, "fable/tasks-engine", filepath.Join(root, "deck"))
	if nested.Kind != KindClone || nested.Parent == nil || nested.Parent.Path != filepath.Join(root, "deck") {
		t.Fatalf("a destination inside a repository is nested: %+v", nested)
	}
	if nested.StorePath != filepath.Join(root, "deck", ".git", storeRelPath) {
		t.Fatalf("nested store = %q", nested.StorePath)
	}
}

func TestInspectCloneDestinationReportsAuthoritativeEmptiness(t *testing.T) {
	root := inspectRoot(t)
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, path string)
	}{
		{name: "empty"},
		{name: "file", setup: func(t *testing.T, path string) {
			t.Helper()
			if err := os.WriteFile(filepath.Join(path, "README.md"), []byte("occupied\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "hidden directory", setup: func(t *testing.T, path string) {
			t.Helper()
			mkdir(t, filepath.Join(path, ".cache"))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := mkdir(t, filepath.Join(root, "empty-retry"))
			if tc.setup != nil {
				tc.setup(t, path)
			}
			out := inspect(t, root, "fable/empty-retry", "")
			if out.Empty != (tc.name == "empty") {
				t.Fatalf("empty = %v for %s destination; entries = %v", out.Empty, tc.name, out.Entries)
			}
			if err := os.RemoveAll(path); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestInspectAbsentChildOfAnInRootSymlinkStaysConfined(t *testing.T) {
	root := inspectRoot(t)
	outside := mkdir(t, filepath.Join(t.TempDir(), "outside"))
	testGit(t, outside, "init", "--quiet", "--initial-branch=main")
	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	// Absolute inputs: a bare owner/repo shape would take the clone branch.
	for _, input := range []string{filepath.Join(root, "link", "new-child"), filepath.Join(root, "link", "deeper", "still")} {
		_, err := Inspect(context.Background(), input, "", []string{root}, nil)
		if !errors.Is(err, ErrOutsideRoots) {
			t.Fatalf("%s: an absent path below an outside link is outside: %v", input, err)
		}
	}
	// The link itself resolves outside too, and an absent path whose
	// resolved ancestor is inside the root reports its resolved location.
	if _, err := Inspect(context.Background(), "link", "", []string{root}, nil); !errors.Is(err, ErrOutsideRoots) {
		t.Fatalf("the link resolves outside: %v", err)
	}
	if err := os.Symlink(filepath.Join(root, "plain"), filepath.Join(root, "alias")); err != nil {
		t.Fatal(err)
	}
	out := inspect(t, root, filepath.Join(root, "alias", "new-child"), "")
	if out.Kind != KindAbsent || out.Path != filepath.Join(root, "plain", "new-child") {
		t.Fatalf("an absent child of an in-root link is reported where it would land: %+v", out)
	}
}

func TestInspectCloneDestinationBehindAnEscapingLinkIsRefused(t *testing.T) {
	root := inspectRoot(t)
	outside := mkdir(t, filepath.Join(t.TempDir(), "outside"))
	testGit(t, outside, "init", "--quiet", "--initial-branch=main")
	testGit(t, outside, "remote", "add", "origin", "https://example.invalid/escaped.git")
	if err := os.Symlink(outside, filepath.Join(root, "escaped")); err != nil {
		t.Fatal(err)
	}
	for _, input := range []string{"https://example.invalid/escaped", "https://example.invalid/escaped.git", "example.invalid/x/escaped"} {
		_, err := Inspect(context.Background(), input, "", []string{root}, nil)
		if !errors.Is(err, ErrOutsideRoots) {
			t.Fatalf("%s: a destination that resolves outside is refused before inspection: %v", input, err)
		}
	}
}

func TestInspectNeverReadsARepositoryOutsideTheRoots(t *testing.T) {
	root := inspectRoot(t)
	outside := mkdir(t, filepath.Join(t.TempDir(), "outside"))
	testGit(t, outside, "init", "--quiet", "--initial-branch=main")
	testGit(t, outside, "config", "user.name", "Outside Author")
	testGit(t, outside, "config", "user.email", "outside@example.invalid")
	testGit(t, outside, "remote", "add", "origin", "https://example.invalid/outside.git")
	if err := os.WriteFile(filepath.Join(outside, "f"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	testGit(t, outside, "add", ".")
	testGit(t, outside, "commit", "--quiet", "-m", "outside")
	// A linked worktree inside the root whose main repository is outside.
	linked := filepath.Join(root, "linked")
	testGit(t, outside, "worktree", "add", "--quiet", linked, "-b", "feat/linked")
	out := inspect(t, root, linked, "")
	if out.Kind != KindPlain || out.Parent != nil || out.StorePath != "" || out.Remote != "" || out.Head != "" {
		t.Fatalf("an outside repository lends no facts: %+v", out)
	}
	if out.Entries[0] != ".git" {
		t.Fatalf("what is on disk is still listed: %v", out.Entries)
	}
	// A .git file inside the root naming a gitdir outside it.
	pointer := mkdir(t, filepath.Join(root, "pointer"))
	if err := os.WriteFile(filepath.Join(pointer, ".git"), []byte("gitdir: "+filepath.Join(outside, ".git")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out = inspect(t, root, pointer, "")
	if out.Kind != KindPlain || out.Parent != nil || out.StorePath != "" {
		t.Fatalf("a pointer to an outside gitdir lends no facts: %+v", out)
	}
	// A root that itself sits inside an outside repository: nothing under
	// it is reported as inside that repository.
	inner := mkdir(t, filepath.Join(outside, "inner"))
	mkdir(t, filepath.Join(inner, "child"))
	out2, err := Inspect(context.Background(), filepath.Join(inner, "child"), "", []string{inner}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if out2.Kind != KindPlain || out2.Parent != nil {
		t.Fatalf("discovery stops at the root: %+v", out2)
	}
}

func TestInspectRefusesOutsideTheRoots(t *testing.T) {
	root := inspectRoot(t)
	_, err := Inspect(context.Background(), t.TempDir(), "", []string{root}, nil)
	if !errors.Is(err, ErrOutsideRoots) {
		t.Fatalf("outside roots: %v", err)
	}
	_, err = Inspect(context.Background(), "fable/x", t.TempDir(), []string{root}, nil)
	if !errors.Is(err, ErrOutsideRoots) {
		t.Fatalf("clone base outside roots: %v", err)
	}
}

func TestHostFormAndCloneURL(t *testing.T) {
	cases := map[string]string{
		"git@github.com:fable/deck.git":     "github.com/fable/deck",
		"https://github.com/fable/deck.git": "github.com/fable/deck",
		"ssh://git@github.com/fable/deck":   "git@github.com/fable/deck",
		"github.com/fable/deck/":            "github.com/fable/deck",
	}
	for in, want := range cases {
		if got := hostForm(in); got != want {
			t.Errorf("hostForm(%q) = %q, want %q", in, got, want)
		}
	}
	if got := cloneURL("git@github.com:fable/deck.git"); got != "git@github.com:fable/deck.git" {
		t.Errorf("scheme'd url kept: %q", got)
	}
}
