package project

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDiffFileShowsLastCommitChangesAndUntrackedLines(t *testing.T) {
	root := t.TempDir()
	gitRun(t, root, "init", "--quiet", "--initial-branch=main")
	putFile(t, root+"/README.md", "one\ntwo\n")
	gitRun(t, root, "add", ".")
	gitRun(t, root, "commit", "--quiet", "-m", "init")
	putFile(t, root+"/README.md", "one\nchanged\n")
	gitRun(t, root, "add", "README.md")
	putFile(t, root+"/fresh.txt", "new\nlines\n")

	modified, err := DiffFile(context.Background(), root, "README.md")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"-two", "+changed", " one"} {
		if !strings.Contains(modified, want) {
			t.Errorf("modified diff missing %q:\n%s", want, modified)
		}
	}
	added, err := DiffFile(context.Background(), root, "fresh.txt")
	if err != nil {
		t.Fatal(err)
	}
	const wantAdded = "--- /dev/null\n+++ b/fresh.txt\n@@ -0,0 +1,2 @@\n+new\n+lines\n"
	if added != wantAdded {
		t.Errorf("untracked diff = %q, want %q", added, wantAdded)
	}
}

func TestDiffFileCannotReadAnotherWorktree(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	gitRun(t, root, "init", "--quiet", "--initial-branch=main")
	putFile(t, root+"/b.txt", "root\n")
	gitRun(t, root, "add", ".")
	gitRun(t, root, "commit", "--quiet", "-m", "init")
	putFile(t, root+"/b.txt", "root-changed\n")
	putFile(t, outside+"/b.txt", "outside-secret\n")
	gitRun(t, root, "config", "core.worktree", outside)
	got, err := DiffFile(context.Background(), root, "b.txt")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "root-changed") || strings.Contains(got, "outside-secret") {
		t.Fatalf("worktree override redirected the diff:\n%s", got)
	}
}

func TestDiffFileNeverRunsTextconv(t *testing.T) {
	root := t.TempDir()
	marker := filepath.Join(t.TempDir(), "textconv-ran")
	gitRun(t, root, "init", "--quiet", "--initial-branch=main")
	putFile(t, root+"/a.txt", "before\n")
	putFile(t, root+"/.gitattributes", "*.txt diff=blocked\n")
	gitRun(t, root, "add", ".")
	gitRun(t, root, "commit", "--quiet", "-m", "init")
	putFile(t, root+"/a.txt", "after\n")
	gitRun(t, root, "config", "diff.blocked.textconv", "touch "+marker)
	if _, err := DiffFile(context.Background(), root, "a.txt"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("textconv executed: %v", err)
	}
}

func TestDiffFileTreatsFilenameAsLiteral(t *testing.T) {
	root := t.TempDir()
	gitRun(t, root, "init", "--quiet", "--initial-branch=main")
	putFile(t, root+"/*", "wild\n")
	putFile(t, root+"/other.txt", "other\n")
	gitRun(t, root, "add", ".")
	gitRun(t, root, "commit", "--quiet", "-m", "init")
	putFile(t, root+"/*", "literal\n")
	putFile(t, root+"/other.txt", "other-file\n")
	got, err := DiffFile(context.Background(), root, "*")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "literal") || strings.Contains(got, "other-file") {
		t.Fatalf("wildcard pathspec widened the diff:\n%s", got)
	}
}

func TestDiffFileBoundsExpandedUntrackedOutput(t *testing.T) {
	root := t.TempDir()
	gitRun(t, root, "init", "--quiet", "--initial-branch=main")
	putFile(t, root+"/large.txt", strings.Repeat("\n", maxTextViewBytes))
	if _, err := DiffFile(context.Background(), root, "large.txt"); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("untracked overflow = %v, want ErrTooLarge", err)
	}
}

func TestDiffFileBoundsTrackedOutput(t *testing.T) {
	root := t.TempDir()
	gitRun(t, root, "init", "--quiet", "--initial-branch=main")
	putFile(t, root+"/large.txt", "base\n")
	gitRun(t, root, "add", ".")
	gitRun(t, root, "commit", "--quiet", "-m", "init")
	putFile(t, root+"/large.txt", strings.Repeat("x\n", maxTextViewBytes))
	if _, err := DiffFile(context.Background(), root, "large.txt"); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("tracked overflow = %v, want ErrTooLarge", err)
	}
}

func TestDiffFileSupportsLinkedWorktree(t *testing.T) {
	root := t.TempDir()
	gitRun(t, root, "init", "--quiet", "--initial-branch=main")
	putFile(t, root+"/a.txt", "before\n")
	gitRun(t, root, "add", ".")
	gitRun(t, root, "commit", "--quiet", "-m", "init")
	linked := filepath.Join(t.TempDir(), "linked")
	gitRun(t, root, "worktree", "add", "--detach", linked, "HEAD")
	putFile(t, linked+"/a.txt", "after\n")
	got, err := DiffFile(context.Background(), linked, "a.txt")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "+after") {
		t.Fatalf("linked worktree diff = %q", got)
	}
}

func TestFilesFingerprintChangesWithHEAD(t *testing.T) {
	root := t.TempDir()
	gitRun(t, root, "init", "--quiet", "--initial-branch=main")
	putFile(t, root+"/docs/a.txt", "base\n")
	gitRun(t, root, "add", ".")
	gitRun(t, root, "commit", "--quiet", "-m", "init")
	putFile(t, root+"/docs/a.txt", "staged\n")
	gitRun(t, root, "add", "docs/a.txt")
	putFile(t, root+"/docs/a.txt", "unstaged\n")
	before, err := filesFingerprint(context.Background(), root, "docs")
	if err != nil {
		t.Fatal(err)
	}
	beforeListing, err := ListFiles(root, "docs")
	if err != nil {
		t.Fatal(err)
	}
	gitRun(t, root, "commit", "--quiet", "-m", "partial")
	after, err := filesFingerprint(context.Background(), root, "docs")
	if err != nil {
		t.Fatal(err)
	}
	afterListing, err := ListFiles(root, "docs")
	if err != nil {
		t.Fatal(err)
	}
	annotateChanges(context.Background(), root, beforeListing)
	annotateChanges(context.Background(), root, afterListing)
	beforeEntry := beforeListing.Entries[0]
	afterEntry := afterListing.Entries[0]
	if beforeEntry.Change != ChangeModified || afterEntry.Change != ChangeModified {
		t.Fatalf("the file did not stay modified: %+v then %+v", beforeEntry, afterEntry)
	}
	if beforeEntry.Size != afterEntry.Size || beforeEntry.MTime != afterEntry.MTime || beforeEntry.Change != afterEntry.Change {
		t.Fatalf("file metadata changed during the partial commit: %+v then %+v", beforeEntry, afterEntry)
	}
	if before == after {
		t.Fatal("a partial commit did not change the file-event fingerprint")
	}
}

func TestDiffFileRejectsOutsidePath(t *testing.T) {
	root := t.TempDir()
	if _, err := DiffFile(context.Background(), root, "../outside"); err == nil {
		t.Fatal("outside path accepted")
	}
}
