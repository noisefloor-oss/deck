package project

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"sort"
	"strings"
	"syscall"
	"time"
)

// Agent assets are the files agents write for the user under a
// project's .deck/ directory. This is Deck's convention, and these are its
// only definitions; the client renders what the listing reports and never
// hard-codes a location, a bound, or the preview cap.
//
// The five locations, in the order the listing reports them:
//
//	.deck/assets/       Assets folder         files agents export for the user
//	.deck/build/        Build scratch
//	.deck/visual-diff/  Visual-diff captures
//	.deck/runs/         Run receipts
//	.deck/sessions/     Session scratch
//
// Each location is walked recursively, never through a symbolic link, to
// at most maxAssetDepth levels and MaxAssetEntries listed entries. A
// location that hits either bound is reported truncated, so a client never
// presents a short list as the whole folder. Delete removes only regular
// files inside these locations; folders, links and special files are
// listed or skipped, never removed.
var assetLocations = []assetLocation{
	{ID: "assets", Name: "Assets folder", Dir: "assets"},
	{ID: "build", Name: "Build scratch", Dir: "build"},
	{ID: "visual-diff", Name: "Visual-diff captures", Dir: "visual-diff"},
	{ID: "runs", Name: "Run receipts", Dir: "runs"},
	{ID: "sessions", Name: "Session scratch", Dir: "sessions"},
}

const (
	// assetsDir is the project-relative directory that holds every location.
	assetsDir = ".deck"
	// MaxAssetEntries bounds the entries one location lists.
	MaxAssetEntries = 500
	// maxAssetDepth bounds how far below a location the walk descends; an
	// entry directly inside the location is at depth 1.
	maxAssetDepth = 8
	// maxAssetExamined bounds the names one location's walk looks at,
	// listed or skipped, so a folder of links the listing never shows
	// cannot make one scan unbounded.
	maxAssetExamined = 8 * MaxAssetEntries
	// MaxAssetPreviewBytes is the largest file the client previews inline:
	// the image viewer's own bound, since previews are served by it.
	MaxAssetPreviewBytes = MaxImageViewBytes
	// MaxAssetDeleteFiles bounds one delete request.
	MaxAssetDeleteFiles = 500

	// assetFolderLock is why a folder row is never selectable: delete
	// removes files only.
	assetFolderLock = "Folder — view only"
)

type assetLocation struct {
	ID   string
	Name string
	// Dir is the location's own directory name inside assetsDir.
	Dir string
}

func (l assetLocation) prefix() string {
	return assetsDir + "/" + l.Dir + "/"
}

// AssetEntry is one listed item inside an agent location.
type AssetEntry struct {
	// Path is relative to the project root and slash-separated.
	Path string `json:"path"`
	// Name is the path relative to its location, so a nested capture
	// reads as "actual/home.png".
	Name string `json:"name"`
	// Kind is "file" or "dir".
	Kind  string `json:"kind"`
	Size  int64  `json:"size"`
	MTime string `json:"mtime"`
	// Version is the cheap metadata identity a delete must quote. Content-
	// checked upload consent uses a separate version on its own path.
	Version string `json:"version"`
	// Locked is why the entry cannot be selected for delete, or null when
	// it can.
	Locked *string `json:"locked"`

	mtime time.Time
}

// AssetLocationListing is one location's scan.
type AssetLocationListing struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// Path is the location relative to the project root, with a trailing
	// slash.
	Path string `json:"path"`
	// Truncated reports that the walk stopped at a bound, so some entries
	// in this location are not listed.
	Truncated bool         `json:"truncated"`
	Entries   []AssetEntry `json:"entries"`
}

// AssetListing is the scan of every agent location in a project.
type AssetListing struct {
	Locations []AssetLocationListing `json:"locations"`
	// Scanned is the server time the scan ran, RFC 3339.
	Scanned         string `json:"scanned"`
	PreviewMaxBytes int64  `json:"preview_max_bytes"`
}

// AssetDeleteRequest names one file the user selected, by the version
// the listing reported for it.
type AssetDeleteRequest struct {
	Path    string `json:"path"`
	Version string `json:"version"`
}

// Asset delete outcomes. A file the client sent but heard nothing back
// about is the client's to call unconfirmed; the server only reports what
// it did.
const (
	AssetDeleted = "deleted"
	AssetSkipped = "skipped"
)

// AssetDeleteResult is what happened to one requested file. Reason is set
// for a skip and states what the server observed.
type AssetDeleteResult struct {
	Path    string `json:"path"`
	Outcome string `json:"outcome"`
	Reason  string `json:"reason,omitempty"`
	// err is the raw failure behind a skip, for the log only.
	err error
}

// ScanAssets lists every agent location in a project.
func ScanAssets(projectRoot string) (*AssetListing, error) {
	return scanAssets(projectRoot, time.Now())
}

func scanAssets(projectRoot string, now time.Time) (*AssetListing, error) {
	root, err := openProjectRoot(projectRoot, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()
	top, err := root.Open(".")
	if err != nil {
		return nil, mapRootOpenError(err)
	}
	defer func() { _ = top.Close() }()

	listing := &AssetListing{
		Locations:       make([]AssetLocationListing, 0, len(assetLocations)),
		Scanned:         now.UTC().Format(time.RFC3339),
		PreviewMaxBytes: MaxAssetPreviewBytes,
	}
	deck, err := openDirAt(top, assetsDir)
	if err != nil && !isMissingDir(err) {
		return nil, fmt.Errorf("open %s: %w", assetsDir, err)
	}
	if deck != nil {
		defer func() { _ = deck.Close() }()
	}
	for _, loc := range assetLocations {
		out := AssetLocationListing{ID: loc.ID, Name: loc.Name, Path: loc.prefix(), Entries: []AssetEntry{}}
		if deck != nil {
			if err := scanLocation(deck, loc, &out); err != nil {
				return nil, err
			}
		}
		sort.SliceStable(out.Entries, func(i, j int) bool {
			a, b := out.Entries[i], out.Entries[j]
			if !a.mtime.Equal(b.mtime) {
				return a.mtime.After(b.mtime)
			}
			return a.Path < b.Path
		})
		listing.Locations = append(listing.Locations, out)
	}
	return listing, nil
}

// isMissingDir reports a location that is absent, or whose name holds a
// link or a file instead of a folder. None of these is listable, and none
// is an error: the location is simply empty.
func isMissingDir(err error) bool {
	return errors.Is(err, syscall.ENOENT) || errors.Is(err, syscall.ENOTDIR) || errors.Is(err, syscall.ELOOP)
}

type assetWalk struct {
	loc      assetLocation
	out      *AssetLocationListing
	examined int
}

func scanLocation(deck *os.File, loc assetLocation, out *AssetLocationListing) error {
	dir, err := openDirAt(deck, loc.Dir)
	if err != nil {
		if isMissingDir(err) {
			return nil
		}
		return fmt.Errorf("open %s: %w", loc.prefix(), err)
	}
	defer func() { _ = dir.Close() }()
	walk := &assetWalk{loc: loc, out: out}
	walk.dir(dir, "", 1)
	return nil
}

// dir lists one folder's entries into the walk and descends into its
// subfolders. It returns false once a bound stopped the walk.
func (w *assetWalk) dir(dir *os.File, rel string, depth int) bool {
	for {
		names, err := dir.Readdirnames(64)
		for _, name := range names {
			if w.examined >= maxAssetExamined || len(w.out.Entries) >= MaxAssetEntries {
				w.out.Truncated = true
				return false
			}
			w.examined++
			if !w.entry(dir, rel, name, depth) {
				return false
			}
		}
		if errors.Is(err, io.EOF) || (err == nil && len(names) == 0) {
			return true
		}
		if err != nil {
			// An unreadable folder lists what was read before the failure;
			// the walk goes on with its siblings.
			return true
		}
	}
}

func (w *assetWalk) entry(dir *os.File, rel, name string, depth int) bool {
	st, err := statUploadTarget(dir, name)
	if err != nil || st == nil || (!st.Dir && !st.Regular) {
		// Vanished, unreadable, a link, or a special file: never listed.
		return true
	}
	childRel := path.Join(rel, name)
	entry := AssetEntry{
		Path:    w.loc.prefix() + childRel,
		Name:    childRel,
		Kind:    "file",
		Size:    st.Size,
		MTime:   st.MTime.UTC().Format(time.RFC3339),
		Version: st.Version,
		mtime:   st.MTime,
	}
	if st.Dir {
		lock := assetFolderLock
		entry.Kind = "dir"
		entry.Size = 0
		entry.Locked = &lock
	}
	w.out.Entries = append(w.out.Entries, entry)
	if !st.Dir {
		return true
	}
	child, err := openDirAt(dir, name)
	if err != nil {
		// Replaced by a link or removed since the stat: not walked.
		return true
	}
	defer func() { _ = child.Close() }()
	if depth >= maxAssetDepth {
		// The depth bound hides whatever this folder holds; say so rather
		// than present the listing as complete.
		if names, _ := child.Readdirnames(1); len(names) > 0 {
			w.out.Truncated = true
		}
		return true
	}
	return w.dir(child, childRel, depth+1)
}

// assetLocationOf returns the location a cleaned project-relative path sits
// strictly inside, and the path below it.
func assetLocationOf(clean string) (assetLocation, string, bool) {
	for _, loc := range assetLocations {
		if rest, ok := strings.CutPrefix(clean, loc.prefix()); ok && rest != "" {
			return loc, rest, true
		}
	}
	return assetLocation{}, "", false
}

// DeleteAssets removes each requested regular file that still matches the
// version the user selected, and reports one result per request, in
// request order. Every file is judged on its own: one refusal never stops
// the rest.
func DeleteAssets(projectRoot string, files []AssetDeleteRequest) ([]AssetDeleteResult, error) {
	root, err := openProjectRoot(projectRoot, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()
	top, err := root.Open(".")
	if err != nil {
		return nil, mapRootOpenError(err)
	}
	defer func() { _ = top.Close() }()
	results := make([]AssetDeleteResult, 0, len(files))
	for _, f := range files {
		results = append(results, deleteAsset(top, f))
	}
	return results, nil
}

func skipped(p, reason string, err error) AssetDeleteResult {
	return AssetDeleteResult{Path: p, Outcome: AssetSkipped, Reason: reason, err: err}
}

const (
	assetReasonOutside    = "Outside the agent locations"
	assetReasonNotFile    = "Not a regular file"
	assetReasonGone       = "Already gone"
	assetReasonPermission = "The workspace refused: permission denied"
	assetReasonFailed     = "The workspace could not delete it"
)

func changedReason(mtime time.Time) string {
	return "Changed on disk after you selected it — modified " + mtime.Local().Format("15:04")
}

func deleteAsset(top *os.File, req AssetDeleteRequest) AssetDeleteResult {
	if err := checkRelative(req.Path); err != nil {
		return skipped(req.Path, assetReasonOutside, err)
	}
	clean := cleanRelative(req.Path)
	loc, rest, ok := assetLocationOf(clean)
	if !ok {
		return skipped(req.Path, assetReasonOutside, nil)
	}
	components := append([]string{assetsDir, loc.Dir}, strings.Split(rest, "/")...)
	leaf := components[len(components)-1]

	parent := top
	for _, name := range components[:len(components)-1] {
		next, err := openDirAt(parent, name)
		if err != nil {
			res := parentFailure(parent, name, req.Path, err)
			if parent != top {
				_ = parent.Close()
			}
			return res
		}
		if parent != top {
			_ = parent.Close()
		}
		parent = next
	}
	defer func() { _ = parent.Close() }()

	st, err := statUploadTarget(parent, leaf)
	if err != nil {
		return deleteFailure(req.Path, err)
	}
	if st == nil {
		return skipped(req.Path, assetReasonGone, nil)
	}
	if !st.Regular {
		return skipped(req.Path, assetReasonNotFile, nil)
	}
	if st.Version != req.Version {
		return skipped(req.Path, changedReason(st.MTime), nil)
	}
	if err := unlinkFileAt(parent, leaf); err != nil {
		return deleteFailure(req.Path, err)
	}
	return AssetDeleteResult{Path: req.Path, Outcome: AssetDeleted}
}

// parentFailure explains a folder on the requested path that would not
// open. A link there leads somewhere the locations do not own; a missing
// folder, or a file standing where the folder was, means the file is gone.
func parentFailure(parent *os.File, name, p string, err error) AssetDeleteResult {
	if errors.Is(err, syscall.ELOOP) || errors.Is(err, syscall.ENOTDIR) {
		st, statErr := statUploadTarget(parent, name)
		if statErr == nil && st != nil && !st.Dir && !st.Regular {
			return skipped(p, assetReasonOutside, err)
		}
		return skipped(p, assetReasonGone, err)
	}
	return deleteFailure(p, err)
}

func deleteFailure(p string, err error) AssetDeleteResult {
	switch {
	case errors.Is(err, syscall.EACCES), errors.Is(err, syscall.EPERM):
		return skipped(p, assetReasonPermission, err)
	case errors.Is(err, syscall.ENOENT):
		return skipped(p, assetReasonGone, err)
	case errors.Is(err, syscall.EISDIR):
		return skipped(p, assetReasonNotFile, err)
	default:
		return skipped(p, assetReasonFailed, err)
	}
}

// assetsFingerprint folds a scan into what the change feed compares: each
// entry's path and version (which covers size and mtime), and each
// location's truncation.
func assetsFingerprint(projectRoot string) (string, error) {
	listing, err := ScanAssets(projectRoot)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	for _, loc := range listing.Locations {
		fmt.Fprintf(&b, "%s\x00%t\x00", loc.ID, loc.Truncated)
		for _, e := range loc.Entries {
			fmt.Fprintf(&b, "%s\x00%s\x00", e.Path, e.Version)
		}
	}
	return b.String(), nil
}
