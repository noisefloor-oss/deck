package project

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

// This endpoint takes a user-supplied path and turns it into a
// filesystem operation, so the escape tests are the load-bearing ones: a
// traversal, an absolute path, or a symlink planted inside the project
// must all resolve to a refusal rather than to a file on the host.

func projectWithFiles(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "brand"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	for path, body := range map[string]string{
		"README.md":          "# project\n",
		"brand/wordmark.svg": "<svg/>\n",
		"brand/notes.txt":    "some notes\n",
	} {
		if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(path)), []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}
	return root
}

func TestListFilesStaysInsideTheProject(t *testing.T) {
	root := projectWithFiles(t)
	listing, err := ListFiles(root, "")
	if err != nil {
		t.Fatalf("list root: %v", err)
	}
	if !listing.AtRoot || listing.Parent != "" {
		t.Fatalf("root listing reports a parent: %+v", listing)
	}
	// Directories sort first, then case-insensitively by name.
	if listing.Entries[0].Name != "brand" || !listing.Entries[0].Dir {
		t.Fatalf("directories do not sort first: %+v", listing.Entries)
	}
	// No entry leaks the container's absolute layout.
	for _, e := range listing.Entries {
		if strings.HasPrefix(e.Path, "/") || strings.Contains(e.Path, root) {
			t.Fatalf("entry leaks an absolute path: %+v", e)
		}
	}

	sub, err := ListFiles(root, "brand")
	if err != nil {
		t.Fatalf("list subdir: %v", err)
	}
	if sub.AtRoot || sub.Path != "brand" || sub.Parent != "" {
		t.Fatalf("subdirectory listing is wrong: %+v", sub)
	}
}

func TestListFilesReportsItsEntryBound(t *testing.T) {
	root := t.TempDir()
	for i := 0; i <= maxEntries; i++ {
		name := filepath.Join(root, fmt.Sprintf("entry-%04d", i))
		if err := os.WriteFile(name, nil, 0o644); err != nil {
			t.Fatalf("write entry %d: %v", i, err)
		}
	}
	listing, err := ListFiles(root, "")
	if err != nil {
		t.Fatalf("list bounded directory: %v", err)
	}
	if !listing.Truncated || len(listing.Entries) != maxEntries {
		t.Fatalf("bounded listing has %d entries, truncated=%v", len(listing.Entries), listing.Truncated)
	}
}

// fakeDirReader serves canned batches the way a directory under churn does:
// the bounded read comes back short while entries remain.
type fakeDirReader struct{ batches [][]os.DirEntry }

func (f *fakeDirReader) ReadDir(int) ([]os.DirEntry, error) {
	if len(f.batches) == 0 {
		return nil, io.EOF
	}
	b := f.batches[0]
	f.batches = f.batches[1:]
	return b, nil
}

func TestReadBoundedDirProbesPastAShortRead(t *testing.T) {
	short := make([]os.DirEntry, maxEntries-1)
	more := []os.DirEntry{nil}

	_, truncated, err := readBoundedDir(&fakeDirReader{batches: [][]os.DirEntry{short, more}})
	if err != nil || !truncated {
		t.Fatalf("short read with entries remaining: truncated=%v err=%v", truncated, err)
	}
	names, truncated, err := readBoundedDir(&fakeDirReader{batches: [][]os.DirEntry{short}})
	if err != nil || truncated || len(names) != len(short) {
		t.Fatalf("exhausted directory: %d names truncated=%v err=%v", len(names), truncated, err)
	}
}

// Every shape of escape resolves to a refusal. The traversal cases matter
// even though filepath.Clean would flatten them, because the guard must
// not depend on the caller having cleaned anything.
func TestFileAccessRefusesEveryEscape(t *testing.T) {
	root := projectWithFiles(t)
	outside := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(outside, []byte("not yours\n"), 0o600); err != nil {
		t.Fatalf("write outside: %v", err)
	}
	// A symlink planted INSIDE the project pointing out of it: the case
	// that a string-prefix check would pass and Canonicalize catches.
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	if err := os.Symlink(filepath.Dir(outside), filepath.Join(root, "escapedir")); err != nil {
		t.Fatalf("symlink dir: %v", err)
	}

	for _, rel := range []string{
		"../secret.txt",
		"../../etc/passwd",
		"brand/../../escape-me",
		"/etc/passwd",
		"escape",               // symlinked file
		"escapedir/secret.txt", // through a symlinked directory
	} {
		t.Run(rel, func(t *testing.T) {
			if _, err := ListFiles(root, rel); !errors.Is(err, ErrOutsideProject) {
				t.Errorf("ListFiles(%q) = %v, want ErrOutsideProject", rel, err)
			}
			if _, _, err := OpenFile(root, rel); !errors.Is(err, ErrOutsideProject) {
				t.Errorf("OpenFile(%q) = %v, want ErrOutsideProject", rel, err)
			}
			if _, err := ReadTextFile(root, rel); !errors.Is(err, ErrOutsideProject) {
				t.Errorf("ReadTextFile(%q) = %v, want ErrOutsideProject", rel, err)
			}
		})
	}

	// The same for writes, which are worse: a successful escape here
	// CREATES a file outside the project.
	for _, rel := range []string{"../planted.txt", "/tmp/planted.txt", "escapedir/planted.txt"} {
		if _, _, err := WriteFile(root, rel, strings.NewReader("x"), nil); !errors.Is(err, ErrOutsideProject) {
			t.Errorf("WriteFile(%q) = %v, want ErrOutsideProject", rel, err)
		}
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(outside), "planted.txt")); err == nil {
		t.Fatal("a refused upload still created a file outside the project")
	}
}

func TestFileAccessFollowsOnlySafeRelativeSymlinks(t *testing.T) {
	root := projectWithFiles(t)
	if err := os.Symlink("brand", filepath.Join(root, "assets")); err != nil {
		t.Fatalf("relative symlink: %v", err)
	}

	listing, err := ListFiles(root, "assets")
	if err != nil {
		t.Fatalf("list relative symlink: %v", err)
	}
	if listing.Path != "assets" || len(listing.Entries) != 2 {
		t.Fatalf("relative symlink listing = %+v", listing)
	}
	body, err := ReadTextFile(root, "assets/notes.txt")
	if err != nil || body != "some notes\n" {
		t.Fatalf("read relative symlink = %q, %v", body, err)
	}
	stored, _, err := WriteFile(root, "assets/new.txt", strings.NewReader("new\n"), nil)
	if err != nil {
		t.Fatalf("write relative symlink: %v", err)
	}
	if stored != "assets/new.txt" {
		t.Fatalf("stored path = %q", stored)
	}
	if body, err := os.ReadFile(filepath.Join(root, "brand", "new.txt")); err != nil || string(body) != "new\n" {
		t.Fatalf("relative symlink target = %q, %v", body, err)
	}

	if err := os.Symlink(filepath.Join(root, "brand"), filepath.Join(root, "absolute-assets")); err != nil {
		t.Fatalf("absolute symlink: %v", err)
	}
	if _, err := ReadTextFile(root, "absolute-assets/notes.txt"); !errors.Is(err, ErrOutsideProject) {
		t.Fatalf("absolute symlink read = %v, want ErrOutsideProject", err)
	}
}

func TestFileAccessPinsDescriptorsAcrossDirectorySwaps(t *testing.T) {
	for _, swapRoot := range []bool{false, true} {
		swapName := "descendant"
		if swapRoot {
			swapName = "project-root"
		}
		for _, operation := range []string{"list", "read", "write"} {
			t.Run(swapName+"/"+operation, func(t *testing.T) {
				root, insideDir, outsideDir, swap := swapFixture(t, swapRoot)
				swapped := false
				afterOpen := func() {
					if swapped {
						t.Fatal("swap hook ran more than once")
					}
					swapped = true
					swap()
				}
				hooks := fileAccessHooks{}
				if swapRoot {
					hooks.rootAnchored = afterOpen
				} else if operation == "write" {
					hooks.parentOpened = afterOpen
				} else {
					hooks.targetOpened = afterOpen
				}

				switch operation {
				case "list":
					listing, err := listFiles(root, "slot", hooks)
					if err != nil {
						t.Fatalf("list after swap: %v", err)
					}
					if !entryNamed(listing, "inside-only.txt") || entryNamed(listing, "outside-only.txt") {
						t.Fatalf("listing crossed the descriptor boundary: %+v", listing.Entries)
					}
				case "read":
					body, err := readTextFile(root, "slot/value.txt", hooks)
					if err != nil {
						t.Fatalf("read after swap: %v", err)
					}
					if body != "inside\n" {
						t.Fatalf("read crossed the descriptor boundary: %q", body)
					}
				case "write":
					stored, _, err := writeFile(root, "slot/upload.txt", strings.NewReader("uploaded\n"), nil, hooks)
					if err != nil {
						t.Fatalf("write after swap: %v", err)
					}
					if stored != "slot/upload.txt" {
						t.Fatalf("stored path = %q", stored)
					}
					body, err := os.ReadFile(filepath.Join(insideDir(), "upload.txt"))
					if err != nil || string(body) != "uploaded\n" {
						t.Fatalf("anchored upload = %q, %v", body, err)
					}
					if _, err := os.Stat(filepath.Join(outsideDir, "upload.txt")); !errors.Is(err, os.ErrNotExist) {
						t.Fatalf("outside upload exists or cannot be checked: %v", err)
					}
				default:
					t.Fatalf("unknown operation %q", operation)
				}
				if !swapped {
					t.Fatal("operation did not reach the adversarial swap")
				}
			})
		}
	}
}

func swapFixture(t *testing.T, swapRoot bool) (root string, insideDir func() string, outsideDir string, swap func()) {
	t.Helper()
	container := t.TempDir()
	root = filepath.Join(container, "project")
	inside := filepath.Join(root, "slot")
	outsideRoot := filepath.Join(t.TempDir(), "outside")
	outsideDir = filepath.Join(outsideRoot, "slot")
	for _, dir := range []string{inside, outsideDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	for path, body := range map[string]string{
		filepath.Join(inside, "value.txt"):            "inside\n",
		filepath.Join(inside, "inside-only.txt"):      "inside\n",
		filepath.Join(outsideDir, "value.txt"):        "outside\n",
		filepath.Join(outsideDir, "outside-only.txt"): "outside\n",
	} {
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}

	target := inside
	replacement := outsideDir
	saved := inside + ".anchored"
	insideDir = func() string { return saved }
	if swapRoot {
		target = root
		replacement = outsideRoot
		saved = root + ".anchored"
		insideDir = func() string { return filepath.Join(saved, "slot") }
	}
	swap = func() {
		if err := os.Rename(target, saved); err != nil {
			t.Fatalf("rename swap target: %v", err)
		}
		if err := os.Symlink(replacement, target); err != nil {
			t.Fatalf("install outside symlink: %v", err)
		}
	}
	return root, insideDir, outsideDir, swap
}

func entryNamed(listing *FileListing, name string) bool {
	for _, entry := range listing.Entries {
		if entry.Name == name {
			return true
		}
	}
	return false
}

// An absolute path must be REFUSED, not reinterpreted. filepath.Join
// silently strips the leading separator, so "/etc/passwd" would otherwise
// resolve to <project>/etc/passwd — inside the root, so every boundary
// check passes, and the user is served a different file than the one
// they named without being told.
func TestAbsolutePathsAreRefusedNotReinterpreted(t *testing.T) {
	root := projectWithFiles(t)
	if err := os.MkdirAll(filepath.Join(root, "etc"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	decoy := filepath.Join(root, "etc", "passwd")
	if err := os.WriteFile(decoy, []byte("decoy\n"), 0o644); err != nil {
		t.Fatalf("write decoy: %v", err)
	}
	if _, err := ReadTextFile(root, "/etc/passwd"); !errors.Is(err, ErrOutsideProject) {
		t.Fatalf("reading /etc/passwd = %v, want ErrOutsideProject; it must not resolve to the decoy", err)
	}
	if _, err := ListFiles(root, "/etc"); !errors.Is(err, ErrOutsideProject) {
		t.Fatalf("listing /etc = %v, want ErrOutsideProject", err)
	}
	if _, _, err := WriteFile(root, "/etc/planted", strings.NewReader("x"), nil); !errors.Is(err, ErrOutsideProject) {
		t.Fatalf("writing /etc/planted = %v, want ErrOutsideProject", err)
	}
}

func TestReadTextFileRefusesBinaryAndOversize(t *testing.T) {
	root := projectWithFiles(t)
	body, err := ReadTextFile(root, "README.md")
	if err != nil || body != "# project\n" {
		t.Fatalf("read text = %q, %v", body, err)
	}

	// Invalid UTF-8 is refused rather than returned with replacement
	// characters, which would misreport the user's file as corrupt.
	if err := os.WriteFile(filepath.Join(root, "logo.bin"), []byte{0xff, 0xfe, 0x00, 0x01}, 0o644); err != nil {
		t.Fatalf("write binary: %v", err)
	}
	if _, err := ReadTextFile(root, "logo.bin"); !errors.Is(err, ErrNotText) {
		t.Fatalf("binary read = %v, want ErrNotText", err)
	}

	big := filepath.Join(root, "big.txt")
	if err := os.WriteFile(big, make([]byte, maxTextViewBytes+1), 0o644); err != nil {
		t.Fatalf("write big: %v", err)
	}
	if _, err := ReadTextFile(root, "big.txt"); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("oversize read = %v, want ErrTooLarge", err)
	}
}

func TestReadRefusesDirectoriesAndDevices(t *testing.T) {
	root := projectWithFiles(t)
	if _, _, err := OpenFile(root, "brand"); !errors.Is(err, ErrNotRegularFile) {
		t.Fatalf("opening a directory = %v, want ErrNotRegularFile", err)
	}
}

func TestWriteFileCommitsAtomicallyAndBoundsSize(t *testing.T) {
	root := projectWithFiles(t)
	rel, n, err := WriteFile(root, "brand/new.svg", strings.NewReader("<svg>new</svg>"), nil)
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	if rel != "brand/new.svg" || n != 14 {
		t.Fatalf("upload reported %q, %d bytes", rel, n)
	}
	body, err := os.ReadFile(filepath.Join(root, "brand", "new.svg"))
	if err != nil || string(body) != "<svg>new</svg>" {
		t.Fatalf("stored %q, %v", body, err)
	}

	// One byte past the bound is refused, and nothing is left behind —
	// neither the named file nor the staging file it was written through.
	oversize := strings.NewReader(strings.Repeat("x", MaxUploadBytes+1))
	if _, _, err := WriteFile(root, "brand/huge.bin", oversize, nil); !errors.Is(err, ErrUploadTooLarge) {
		t.Fatalf("oversize upload = %v, want ErrUploadTooLarge", err)
	}
	if _, err := os.Stat(filepath.Join(root, "brand", "huge.bin")); err == nil {
		t.Fatal("a refused upload left the file in place")
	}
	entries, err := os.ReadDir(filepath.Join(root, "brand"))
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".upload-") {
			t.Fatalf("a refused upload left staging file %q behind", e.Name())
		}
	}
}

// An existing destination is refused, never silently replaced: without
// consent naming the file the user approved destroying, the original
// keeps its bytes and the upload leaves nothing behind.
func TestWriteFileRefusesExistingDestinationWithoutConsent(t *testing.T) {
	root := projectWithFiles(t)
	_, _, err := WriteFile(root, "README.md", strings.NewReader("clobber\n"), nil)
	var conflict *UploadConflict
	if !errors.As(err, &conflict) || conflict.Dir || conflict.Changed || conflict.NotFile {
		t.Fatalf("upload over existing = %v, want unchanged conflict", err)
	}
	if conflict.Leaf != "README.md" || conflict.Size != int64(len("# project\n")) {
		t.Fatalf("conflict describes %q size %d, want README.md with the on-disk size", conflict.Leaf, conflict.Size)
	}
	if conflict.MTime.IsZero() {
		t.Fatal("conflict carries no modification time")
	}
	if conflict.Version == "" {
		t.Fatal("conflict carries no version to approve replacement with")
	}
	body, err := os.ReadFile(filepath.Join(root, "README.md"))
	if err != nil || string(body) != "# project\n" {
		t.Fatalf("refused upload changed the original: %q, %v", body, err)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".upload-") {
			t.Fatalf("refused upload left staging file %q behind", e.Name())
		}
	}
}

// Consent naming the file that actually holds the name replaces it: the
// rename is what makes the swap atomic, so a reader either sees the old
// bytes or the new ones and never a half-written file.
func TestWriteFileReplacesWithMatchingConsentWithoutTruncating(t *testing.T) {
	root := projectWithFiles(t)
	_, _, err := WriteFile(root, "README.md", strings.NewReader("first attempt\n"), nil)
	var refused *UploadConflict
	if !errors.As(err, &refused) {
		t.Fatalf("first upload = %v, want conflict", err)
	}
	rel, n, err := WriteFile(root, "README.md", strings.NewReader("replaced\n"),
		&UploadConsent{Version: refused.Version})
	if err != nil {
		t.Fatalf("replace: %v", err)
	}
	if rel != "README.md" || n != 9 {
		t.Fatalf("replace reported %q, %d bytes", rel, n)
	}
	body, err := os.ReadFile(filepath.Join(root, "README.md"))
	if err != nil || string(body) != "replaced\n" {
		t.Fatalf("replaced content = %q, %v", body, err)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".upload-") {
			t.Fatalf("replace left staging file %q behind", e.Name())
		}
	}
}

// Consent that no longer matches the destination — because the file grew,
// was rewritten, or was replaced by another writer — is refused with the
// metadata now observed, so the user can be asked again with current
// facts. The second consent, taken from that refusal, replaces cleanly:
// reconfirmation is a re-ask, not a dead end.
func TestWriteFileReconfirmsChangedTarget(t *testing.T) {
	root := projectWithFiles(t)
	_, _, err := WriteFile(root, "README.md", strings.NewReader("x"), nil)
	var first *UploadConflict
	if !errors.As(err, &first) {
		t.Fatalf("first upload = %v, want conflict", err)
	}

	// Rewrite the file in place after the approval was observed.
	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("# project, edited\n"), 0o644); err != nil {
		t.Fatalf("edit target: %v", err)
	}
	_, _, err = WriteFile(root, "README.md", strings.NewReader("clobber\n"),
		&UploadConsent{Version: first.Version})
	var changed *UploadConflict
	if !errors.As(err, &changed) || !changed.Changed {
		t.Fatalf("stale consent = %v, want changed conflict", err)
	}
	body, err := os.ReadFile(filepath.Join(root, "README.md"))
	if err != nil || string(body) != "# project, edited\n" {
		t.Fatalf("stale consent changed the target: %q, %v", body, err)
	}

	_, _, err = WriteFile(root, "README.md", strings.NewReader("replaced\n"),
		&UploadConsent{Version: changed.Version})
	if err != nil {
		t.Fatalf("reconfirmed replace: %v", err)
	}
	body, err = os.ReadFile(filepath.Join(root, "README.md"))
	if err != nil || string(body) != "replaced\n" {
		t.Fatalf("reconfirmed content = %q, %v", body, err)
	}
}

// A directory holding the name is never replaceable, consent or not: the
// refusal says so, and the directory is untouched.
func TestWriteFileRefusesDirectoryTargetEvenWithConsent(t *testing.T) {
	root := projectWithFiles(t)
	target := filepath.Join(root, "brand")
	info, err := os.Stat(target)
	if err != nil {
		t.Fatalf("stat target: %v", err)
	}
	_, _, werr := WriteFile(root, "brand", strings.NewReader("not a folder\n"), nil)
	var conflict *UploadConflict
	if !errors.As(werr, &conflict) || !conflict.Dir {
		t.Fatalf("upload over directory = %v, want directory conflict", werr)
	}
	consent := &UploadConsent{Version: conflict.Version}
	if _, _, werr = WriteFile(root, "brand", strings.NewReader("not a folder\n"), consent); !errors.As(werr, &conflict) || !conflict.Dir {
		t.Fatalf("consented upload over directory = %v, want directory conflict", werr)
	}
	after, err := os.Stat(target)
	if err != nil || !after.IsDir() || !after.ModTime().Equal(info.ModTime()) {
		t.Fatalf("directory target was disturbed: %+v, %v", after, err)
	}
}

// A symlink holding the name is reported as itself — the link's own size
// and mtime, never the target's — and is not replaceable even with
// consent: only a regular file is. The link and the file it points at
// both keep what they had.
func TestWriteFileConflictReportsSymlinkNotTargetAndNeverReplacesIt(t *testing.T) {
	root := projectWithFiles(t)
	outside := filepath.Join(t.TempDir(), "secret.txt")
	const secret = "outside the project\n"
	if err := os.WriteFile(outside, []byte(secret), 0o600); err != nil {
		t.Fatalf("write outside target: %v", err)
	}
	// The name must be free for the link to take it: the fixture's regular
	// README.md is what the link displaces in the listing.
	if err := os.Remove(filepath.Join(root, "README.md")); err != nil {
		t.Fatalf("clear fixture name: %v", err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "README.md")); err != nil {
		t.Fatalf("plant symlink: %v", err)
	}
	_, _, err := WriteFile(root, "README.md", strings.NewReader("clobber\n"), nil)
	var conflict *UploadConflict
	if !errors.As(err, &conflict) || conflict.Dir {
		t.Fatalf("upload over symlink = %v, want conflict", err)
	}
	if !conflict.NotFile {
		t.Fatalf("symlink conflict does not say the name is not a regular file: %+v", conflict)
	}
	if conflict.Size != int64(len(outside)) {
		t.Fatalf("conflict size = %d, want the link text's own length %d", conflict.Size, len(outside))
	}
	if _, _, err = WriteFile(root, "README.md", strings.NewReader("replaced\n"),
		&UploadConsent{Version: conflict.Version}); !errors.As(err, &conflict) || !conflict.NotFile {
		t.Fatalf("consented replace over symlink = %v, want not-a-file refusal", err)
	}
	if link, lerr := os.Readlink(filepath.Join(root, "README.md")); lerr != nil || link != outside {
		t.Fatalf("symlink was disturbed: %q, %v", link, lerr)
	}
	kept, err := os.ReadFile(outside)
	if err != nil || string(kept) != secret {
		t.Fatalf("symlink target was disturbed: %q, %v", kept, err)
	}
}

// An approved target that vanished cannot be destroyed by the replacement,
// so the upload lands on the free name rather than failing or asking
// again.
func TestWriteFileLandsWhenApprovedTargetVanished(t *testing.T) {
	root := projectWithFiles(t)
	_, _, err := WriteFile(root, "README.md", strings.NewReader("x"), nil)
	var first *UploadConflict
	if !errors.As(err, &first) {
		t.Fatalf("first upload = %v, want conflict", err)
	}
	if err := os.Remove(filepath.Join(root, "README.md")); err != nil {
		t.Fatalf("remove target: %v", err)
	}
	rel, _, err := WriteFile(root, "README.md", strings.NewReader("landed\n"),
		&UploadConsent{Version: first.Version})
	if err != nil {
		t.Fatalf("upload over vanished target: %v", err)
	}
	if rel != "README.md" {
		t.Fatalf("stored path = %q", rel)
	}
	body, err := os.ReadFile(filepath.Join(root, "README.md"))
	if err != nil || string(body) != "landed\n" {
		t.Fatalf("stored content = %q, %v", body, err)
	}
}

func TestWriteFileReasksWhenVanishedApprovedTargetIsTakenAgain(t *testing.T) {
	root := projectWithFiles(t)
	_, _, err := WriteFile(root, "README.md", strings.NewReader("new"), nil)
	var first *UploadConflict
	if !errors.As(err, &first) {
		t.Fatalf("initial upload = %v, want conflict", err)
	}
	path := filepath.Join(root, "README.md")
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	_, _, err = writeFile(root, "README.md", strings.NewReader("new"), &UploadConsent{Version: first.Version}, fileAccessHooks{
		targetMissing: func() {
			if err := os.WriteFile(path, []byte("competing file"), 0o600); err != nil {
				t.Fatal(err)
			}
		},
	})
	var conflict *UploadConflict
	if !errors.As(err, &conflict) || !conflict.Changed || conflict.Version == "" || conflict.Version == first.Version {
		t.Fatalf("competing creator = %v, want fresh changed conflict", err)
	}
	content, err := os.ReadFile(path)
	if err != nil || string(content) != "competing file" {
		t.Fatalf("competing file lost: %q, %v", content, err)
	}
}

// Two files of the same name in one batch are two sequential commits: the
// first lands, the second meets it on disk and is refused like any other
// existing destination.
func TestWriteFileSameNameTwiceConflictsOnDisk(t *testing.T) {
	root := t.TempDir()
	if _, _, err := WriteFile(root, "notes.txt", strings.NewReader("first\n"), nil); err != nil {
		t.Fatalf("first upload: %v", err)
	}
	_, _, err := WriteFile(root, "notes.txt", strings.NewReader("second\n"), nil)
	var conflict *UploadConflict
	if !errors.As(err, &conflict) || conflict.Changed || conflict.Dir {
		t.Fatalf("second same-name upload = %v, want plain conflict", err)
	}
	body, err := os.ReadFile(filepath.Join(root, "notes.txt"))
	if err != nil || string(body) != "first\n" {
		t.Fatalf("second upload won the name: %q, %v", body, err)
	}
}

// A different file planted under the name with the same size and mtime
// must invalidate consent. The version folds inode and ctime, so approval
// for the original is refused, the planted file keeps its bytes, and a
// consent taken from that refusal replaces cleanly.
func TestWriteFileRefusesSameSizeSameMTimeReplacement(t *testing.T) {
	root := projectWithFiles(t)
	readme := filepath.Join(root, "README.md")
	before, err := os.Stat(readme)
	if err != nil {
		t.Fatalf("stat target: %v", err)
	}
	_, _, err = WriteFile(root, "README.md", strings.NewReader("x"), nil)
	var approved *UploadConflict
	if !errors.As(err, &approved) {
		t.Fatalf("first upload = %v, want conflict", err)
	}

	const planted = "hijacked!\n"
	if len(planted) != len("# project\n") {
		t.Fatalf("fixture lengths differ: %d vs %d", len(planted), len("# project\n"))
	}
	if err := os.WriteFile(readme, []byte(planted), 0o644); err != nil {
		t.Fatalf("plant replacement: %v", err)
	}
	if err := os.Chtimes(readme, before.ModTime(), before.ModTime()); err != nil {
		t.Fatalf("restore mtime: %v", err)
	}
	stat, err := os.Stat(readme)
	if err != nil || stat.Size() != before.Size() || !stat.ModTime().Equal(before.ModTime()) {
		t.Fatalf("planted file is not metadata-identical: %+v, %v", stat, err)
	}

	_, _, err = WriteFile(root, "README.md", strings.NewReader("clobber\n"),
		&UploadConsent{Version: approved.Version})
	var changed *UploadConflict
	if !errors.As(err, &changed) || !changed.Changed {
		t.Fatalf("consent approved before the same-size swap = %v, want changed conflict", err)
	}
	body, err := os.ReadFile(readme)
	if err != nil || string(body) != planted {
		t.Fatalf("the writer that took the name was destroyed: %q, %v", body, err)
	}

	if _, _, err = WriteFile(root, "README.md", strings.NewReader("replaced\n"),
		&UploadConsent{Version: changed.Version}); err != nil {
		t.Fatalf("reconfirmed replace: %v", err)
	}
	if body, err = os.ReadFile(readme); err != nil || string(body) != "replaced\n" {
		t.Fatalf("reconfirmed content = %q, %v", body, err)
	}
}

// An in-place rewrite keeps the inode; when it also keeps the size and
// the mtime is restored exactly, only ctime still moved. The version
// folds ctime at nanosecond resolution, so the stale approval is still
// refused — the rewrite is not mistaken for the file the user saw.
func TestWriteFileRefusesInPlaceRewriteRestoringSizeAndMTime(t *testing.T) {
	root := projectWithFiles(t)
	readme := filepath.Join(root, "README.md")
	before, err := os.Stat(readme)
	if err != nil {
		t.Fatalf("stat target: %v", err)
	}
	_, _, err = WriteFile(root, "README.md", strings.NewReader("x"), nil)
	var approved *UploadConflict
	if !errors.As(err, &approved) {
		t.Fatalf("first upload = %v, want conflict", err)
	}

	const rewritten = "REWROTE!!\n"
	if len(rewritten) != len("# project\n") {
		t.Fatalf("fixture lengths differ: %d vs %d", len(rewritten), len("# project\n"))
	}
	f, err := os.OpenFile(readme, os.O_WRONLY, 0)
	if err != nil {
		t.Fatalf("open target: %v", err)
	}
	if _, err := f.WriteAt([]byte(rewritten), 0); err != nil {
		f.Close()
		t.Fatalf("rewrite in place: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close target: %v", err)
	}
	if err := os.Chtimes(readme, before.ModTime(), before.ModTime()); err != nil {
		t.Fatalf("restore mtime: %v", err)
	}
	after, err := os.Stat(readme)
	if err != nil || after.Size() != before.Size() || !after.ModTime().Equal(before.ModTime()) {
		t.Fatalf("rewritten file is not metadata-identical: %+v, %v", after, err)
	}
	if !os.SameFile(before, after) {
		t.Fatal("the rewrite did not keep the inode; the test is not exercising the in-place case")
	}

	_, _, err = WriteFile(root, "README.md", strings.NewReader("clobber\n"),
		&UploadConsent{Version: approved.Version})
	var changed *UploadConflict
	if !errors.As(err, &changed) || !changed.Changed {
		t.Fatalf("consent approved before the in-place rewrite = %v, want changed conflict", err)
	}
	if body, err := os.ReadFile(readme); err != nil || string(body) != rewritten {
		t.Fatalf("the in-place writer was destroyed: %q, %v", body, err)
	}
}

func TestUploadContentDigestRejectsFIFOBeforeReading(t *testing.T) {
	root := projectWithFiles(t)
	const leaf = "replacement.fifo"
	if err := unix.Mkfifo(root+"/"+leaf, 0o600); err != nil {
		t.Fatalf("create FIFO: %v", err)
	}
	parent, err := os.Open(root)
	if err != nil {
		t.Fatalf("open project: %v", err)
	}
	defer parent.Close()
	digest, opened, err := uploadContentDigest(parent, leaf, &unix.Stat_t{Mode: unix.S_IFREG})
	if err != nil {
		t.Fatalf("digest swapped FIFO: %v", err)
	}
	if opened.Mode&unix.S_IFMT == unix.S_IFREG {
		t.Fatal("the opened FIFO was treated as a regular file")
	}
	if digest != "" {
		t.Fatal("the FIFO was read despite being nonregular")
	}
}

func TestUploadConsentVersionUsesActualFileBytes(t *testing.T) {
	root := projectWithFiles(t)
	const leaf = "README.md"
	parent, err := os.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer parent.Close()
	st, err := lstatUploadTarget(parent, leaf)
	if err != nil {
		t.Fatal(err)
	}
	first, opened, err := uploadContentDigest(parent, leaf, st)
	if err != nil {
		t.Fatal(err)
	}
	repeated, _, err := uploadContentDigest(parent, leaf, st)
	if err != nil {
		t.Fatal(err)
	}
	if first != repeated {
		t.Fatal("unchanged bytes produced different content digests")
	}
	if err := os.WriteFile(filepath.Join(root, leaf), []byte("changed"), 0o644); err != nil {
		t.Fatal(err)
	}
	second, _, err := uploadContentDigest(parent, leaf, st)
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatal("different bytes produced the same content digest")
	}
	if uploadVersion(opened, first) == uploadVersion(opened, second) {
		t.Fatal("identical metadata produced the same consent version for different bytes")
	}
}

func TestWriteFileWriteOnlyTargetKeepsConsentFlow(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores file read permissions")
	}
	root := projectWithFiles(t)
	if err := os.Chmod(filepath.Join(root, "README.md"), 0o200); err != nil {
		t.Fatal(err)
	}
	_, _, err := WriteFile(root, "README.md", strings.NewReader("replacement\n"), nil)
	var conflict *UploadConflict
	if !errors.As(err, &conflict) {
		t.Fatalf("write-only target = %v, want a consent conflict", err)
	}
	if _, _, err := WriteFile(root, "README.md", strings.NewReader("replacement\n"),
		&UploadConsent{Version: conflict.Version}); err != nil {
		t.Fatalf("consented replacement of write-only target: %v", err)
	}
	body, err := os.ReadFile(filepath.Join(root, "README.md"))
	if err != nil || string(body) != "replacement\n" {
		t.Fatalf("replacement = %q, %v", body, err)
	}
}

func TestUploadVersionIncludesContentDigest(t *testing.T) {
	st := unix.Stat_t{}
	first := uploadVersion(&st, "first-content")
	second := uploadVersion(&st, "changed-content")
	if first == second {
		t.Fatal("identical metadata produced the same version for changed bytes")
	}
	if first != uploadVersion(&st, "first-content") {
		t.Fatal("identical metadata and content did not produce a stable version")
	}
}

// The version is deterministic for an unchanged file — so an approval is
// not invalidated by nothing — and differs between files. Reading does
// not count: atime is excluded on purpose, because reading a file must
// not invalidate an approval taken for it.
func TestUploadConflictVersionIsStableAndDistinguishesTargets(t *testing.T) {
	root := projectWithFiles(t)
	refuse := func(rel string) *UploadConflict {
		_, _, err := WriteFile(root, rel, strings.NewReader("x"), nil)
		var conflict *UploadConflict
		if !errors.As(err, &conflict) {
			t.Fatalf("upload over %s = %v, want conflict", rel, err)
		}
		return conflict
	}
	first := refuse("README.md")
	if _, err := os.ReadFile(filepath.Join(root, "README.md")); err != nil {
		t.Fatalf("read target: %v", err)
	}
	if again := refuse("README.md"); again.Version != first.Version {
		t.Fatalf("unchanged, unread-modified file changed version: %q then %q", first.Version, again.Version)
	}
	if other := refuse("brand/wordmark.svg"); other.Version == first.Version {
		t.Fatalf("different file shares version %q", other.Version)
	}
}

// A special file holding the name is never replaceable either: the
// conflict says the name is not a regular file, and consent naming it is
// refused without disturbing the device.
func TestWriteFileRefusesSpecialFileTargetEvenWithConsent(t *testing.T) {
	root := projectWithFiles(t)
	fifo := filepath.Join(root, "pipe")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatalf("mkfifo: %v", err)
	}
	_, _, err := WriteFile(root, "pipe", strings.NewReader("x"), nil)
	var conflict *UploadConflict
	if !errors.As(err, &conflict) || !conflict.NotFile {
		t.Fatalf("upload over fifo = %v, want not-a-file conflict", err)
	}
	if _, _, err = WriteFile(root, "pipe", strings.NewReader("x"),
		&UploadConsent{Version: conflict.Version}); !errors.As(err, &conflict) || !conflict.NotFile {
		t.Fatalf("consented upload over fifo = %v, want not-a-file conflict", err)
	}
	if info, lerr := os.Lstat(fifo); lerr != nil || info.Mode()&os.ModeNamedPipe == 0 {
		t.Fatalf("fifo was disturbed: %+v, %v", info, lerr)
	}
}
