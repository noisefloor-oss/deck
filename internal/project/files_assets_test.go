package project

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// writeAsset creates a file under root with the given mtime, making its
// parent folders.
func writeAsset(t *testing.T, root, rel, body string, mtime time.Time) string {
	t.Helper()
	full := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", rel, err)
	}
	if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", rel, err)
	}
	if err := os.Chtimes(full, mtime, mtime); err != nil {
		t.Fatalf("chtimes %s: %v", rel, err)
	}
	return full
}

func scanOrFail(t *testing.T, root string) *AssetListing {
	t.Helper()
	listing, err := ScanAssets(root)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	return listing
}

func locationByID(t *testing.T, listing *AssetListing, id string) AssetLocationListing {
	t.Helper()
	for _, loc := range listing.Locations {
		if loc.ID == id {
			return loc
		}
	}
	t.Fatalf("no location %q in %+v", id, listing.Locations)
	return AssetLocationListing{}
}

func entryPaths(loc AssetLocationListing) []string {
	out := make([]string, 0, len(loc.Entries))
	for _, e := range loc.Entries {
		out = append(out, e.Path)
	}
	return out
}

func versionOf(t *testing.T, listing *AssetListing, path string) string {
	t.Helper()
	for _, loc := range listing.Locations {
		for _, e := range loc.Entries {
			if e.Path == path {
				return e.Version
			}
		}
	}
	t.Fatalf("%s is not listed", path)
	return ""
}

func TestScanAssetsListsEveryLocationEvenWhenMissing(t *testing.T) {
	root := t.TempDir()
	listing := scanOrFail(t, root)
	want := []struct{ id, name, path string }{
		{"assets", "Assets folder", ".deck/assets/"},
		{"build", "Build scratch", ".deck/build/"},
		{"visual-diff", "Visual-diff captures", ".deck/visual-diff/"},
		{"runs", "Run receipts", ".deck/runs/"},
		{"sessions", "Session scratch", ".deck/sessions/"},
	}
	if len(listing.Locations) != len(want) {
		t.Fatalf("locations = %d, want %d", len(listing.Locations), len(want))
	}
	for i, w := range want {
		got := listing.Locations[i]
		if got.ID != w.id || got.Name != w.name || got.Path != w.path || got.Truncated || got.Entries == nil || len(got.Entries) != 0 {
			t.Errorf("location %d = %+v, want %s %q %s with no entries", i, got, w.id, w.name, w.path)
		}
	}
	if _, err := time.Parse(time.RFC3339, listing.Scanned); err != nil {
		t.Errorf("scanned %q is not RFC 3339: %v", listing.Scanned, err)
	}
	if listing.PreviewMaxBytes != MaxAssetPreviewBytes {
		t.Errorf("preview_max_bytes = %d", listing.PreviewMaxBytes)
	}
}

func TestScanAssetsGroupsSortsLocksAndSkipsLinks(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	writeAsset(t, outside, "secret.txt", "secret", time.Unix(5000, 0))
	base := time.Unix(1_000_000, 0)
	writeAsset(t, root, ".deck/assets/old.png", "old", base)
	writeAsset(t, root, ".deck/assets/new.png", "newer", base.Add(time.Hour))
	writeAsset(t, root, ".deck/assets/same-second-later.png", "x", base.Add(500*time.Millisecond))
	writeAsset(t, root, ".deck/build/x/y.txt", "y", base.Add(2*time.Hour))
	if err := os.Chtimes(filepath.Join(root, ".deck/build/x"), base.Add(-time.Hour), base.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	writeAsset(t, root, ".deck/visual-diff/actual/home.png", "png", base)
	writeAsset(t, root, "README.md", "not an asset", base)
	if err := os.Symlink(filepath.Join(outside, "secret.txt"), filepath.Join(root, ".deck/assets/file-link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, ".deck/assets/dir-link")); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(root, ".deck/assets/pipe"), 0o600); err != nil {
		t.Fatal(err)
	}

	listing := scanOrFail(t, root)

	assets := locationByID(t, listing, "assets")
	if got, want := strings.Join(entryPaths(assets), ","), ".deck/assets/new.png,.deck/assets/same-second-later.png,.deck/assets/old.png"; got != want {
		t.Errorf("assets entries = %s, want %s (links and the pipe skipped, newest first)", got, want)
	}
	newest := assets.Entries[0]
	if newest.Name != "new.png" || newest.Kind != "file" || newest.Size != 5 || newest.Locked != nil {
		t.Errorf("new.png entry = %+v", newest)
	}
	if newest.MTime != base.Add(time.Hour).UTC().Format(time.RFC3339) {
		t.Errorf("new.png mtime = %s", newest.MTime)
	}
	parent, err := os.Open(filepath.Join(root, ".deck/assets"))
	if err != nil {
		t.Fatal(err)
	}
	defer parent.Close()
	st, err := statUploadTarget(parent, "new.png")
	if err != nil || st == nil {
		t.Fatalf("stat new.png: %v", err)
	}
	if newest.Version != st.Version {
		t.Errorf("listing version %q differs from the upload-conflict version %q", newest.Version, st.Version)
	}

	build := locationByID(t, listing, "build")
	if got, want := strings.Join(entryPaths(build), ","), ".deck/build/x/y.txt,.deck/build/x"; got != want {
		t.Fatalf("build entries = %s, want %s", got, want)
	}
	file, dir := build.Entries[0], build.Entries[1]
	if file.Name != "x/y.txt" || file.Locked != nil {
		t.Errorf("nested file = %+v", file)
	}
	if dir.Kind != "dir" || dir.Name != "x" || dir.Locked == nil || *dir.Locked != "Folder — view only" {
		t.Errorf("folder entry = %+v", dir)
	}

	vdiff := locationByID(t, listing, "visual-diff")
	if got := strings.Join(entryPaths(vdiff), ","); !strings.Contains(got, ".deck/visual-diff/actual/home.png") {
		t.Errorf("visual-diff entries = %s", got)
	}
	for _, loc := range listing.Locations {
		if loc.Truncated {
			t.Errorf("%s truncated with a handful of entries", loc.ID)
		}
		for _, e := range loc.Entries {
			if strings.Contains(e.Path, "secret") || strings.Contains(e.Path, "README") {
				t.Errorf("listed %s", e.Path)
			}
		}
	}
}

func TestScanAssetsBoundsEntries(t *testing.T) {
	for _, tc := range []struct {
		name      string
		files     int
		listed    int
		truncated bool
	}{
		{"at the bound", MaxAssetEntries, MaxAssetEntries, false},
		{"past the bound", MaxAssetEntries + 1, MaxAssetEntries, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			dir := filepath.Join(root, ".deck", "build")
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			for i := range tc.files {
				if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("f%04d", i)), nil, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			writeAsset(t, root, ".deck/assets/kept.png", "k", time.Unix(1, 0))
			listing := scanOrFail(t, root)
			build := locationByID(t, listing, "build")
			if len(build.Entries) != tc.listed || build.Truncated != tc.truncated {
				t.Fatalf("build: %d entries, truncated %t; want %d, %t", len(build.Entries), build.Truncated, tc.listed, tc.truncated)
			}
			if assets := locationByID(t, listing, "assets"); len(assets.Entries) != 1 || assets.Truncated {
				t.Fatalf("a full location must not bound its neighbour: %+v", assets)
			}
		})
	}
}

func TestScanAssetsBoundsDepth(t *testing.T) {
	chain := func(depth int) string {
		parts := make([]string, depth)
		for i := range parts {
			parts[i] = fmt.Sprintf("d%d", i+1)
		}
		return strings.Join(parts, "/")
	}
	t.Run("an empty folder at the bound is complete", func(t *testing.T) {
		root := t.TempDir()
		if err := os.MkdirAll(filepath.Join(root, ".deck/runs", chain(maxAssetDepth)), 0o755); err != nil {
			t.Fatal(err)
		}
		runs := locationByID(t, scanOrFail(t, root), "runs")
		if len(runs.Entries) != maxAssetDepth || runs.Truncated {
			t.Fatalf("runs: %d entries, truncated %t", len(runs.Entries), runs.Truncated)
		}
	})
	t.Run("content below the bound truncates", func(t *testing.T) {
		root := t.TempDir()
		writeAsset(t, root, ".deck/runs/"+chain(maxAssetDepth)+"/deep.json", "{}", time.Unix(1, 0))
		runs := locationByID(t, scanOrFail(t, root), "runs")
		if len(runs.Entries) != maxAssetDepth || !runs.Truncated {
			t.Fatalf("runs: %d entries, truncated %t", len(runs.Entries), runs.Truncated)
		}
		for _, e := range runs.Entries {
			if strings.HasSuffix(e.Path, "deep.json") {
				t.Fatalf("listed past the depth bound: %s", e.Path)
			}
		}
	})
	t.Run("a file at the bound is listed", func(t *testing.T) {
		root := t.TempDir()
		writeAsset(t, root, ".deck/runs/"+chain(maxAssetDepth-1)+"/at-bound.json", "{}", time.Unix(1, 0))
		runs := locationByID(t, scanOrFail(t, root), "runs")
		if len(runs.Entries) != maxAssetDepth || runs.Truncated {
			t.Fatalf("runs: %d entries, truncated %t", len(runs.Entries), runs.Truncated)
		}
	})
}

func TestScanAssetsNeverFollowsALinkedLocation(t *testing.T) {
	for _, linked := range []string{".deck", ".deck/assets"} {
		t.Run(linked, func(t *testing.T) {
			root := t.TempDir()
			outside := t.TempDir()
			writeAsset(t, outside, "assets/leak.png", "x", time.Unix(1, 0))
			writeAsset(t, outside, "leak.png", "x", time.Unix(1, 0))
			if err := os.MkdirAll(filepath.Dir(filepath.Join(root, linked)), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(outside, filepath.Join(root, linked)); err != nil {
				t.Fatal(err)
			}
			for _, loc := range scanOrFail(t, root).Locations {
				if len(loc.Entries) != 0 {
					t.Fatalf("%s listed through a link: %+v", loc.ID, loc.Entries)
				}
			}
		})
	}
}

func TestAssetsFingerprintFollowsTheScan(t *testing.T) {
	root := t.TempDir()
	file := writeAsset(t, root, ".deck/assets/a.png", "one", time.Unix(1000, 0))
	fingerprint := func() string {
		t.Helper()
		f, err := assetsFingerprint(root)
		if err != nil {
			t.Fatalf("fingerprint: %v", err)
		}
		return f
	}
	last := fingerprint()
	if again := fingerprint(); again != last {
		t.Fatal("an unchanged tree changed its fingerprint")
	}
	steps := []struct {
		name   string
		change func()
	}{
		{"size", func() { writeAsset(t, root, ".deck/assets/a.png", "longer", time.Unix(1000, 0)) }},
		{"mtime", func() {
			if err := os.Chtimes(file, time.Unix(2000, 0), time.Unix(2000, 0)); err != nil {
				t.Fatal(err)
			}
		}},
		{"new file in another location", func() { writeAsset(t, root, ".deck/sessions/s/log.txt", "x", time.Unix(1, 0)) }},
		{"removed file", func() {
			if err := os.Remove(file); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, step := range steps {
		step.change()
		current := fingerprint()
		if current == last {
			t.Fatalf("%s: fingerprint did not change", step.name)
		}
		last = current
	}
	// A path outside the locations is not part of the scan.
	writeAsset(t, root, "README.md", "x", time.Unix(1, 0))
	writeAsset(t, root, ".deck/preview.json", "{}", time.Unix(1, 0))
	if fingerprint() != last {
		t.Fatal("a file outside the agent locations changed the fingerprint")
	}
}

func TestDeleteAssetsOutcomes(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	secret := writeAsset(t, outside, "secret.txt", "secret", time.Unix(1, 0))
	writeAsset(t, root, ".deck/assets/a.png", "a", time.Unix(1000, 0))
	changedAt := time.Date(2026, 10, 18, 14, 6, 0, 0, time.Local)
	changed := writeAsset(t, root, ".deck/assets/changed.png", "c", time.Unix(1000, 0))
	writeAsset(t, root, ".deck/build/x/y.txt", "y", time.Unix(1000, 0))
	if err := os.Mkdir(filepath.Join(root, ".deck/build/empty"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, filepath.Join(root, ".deck/assets/link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, ".deck/assets/linked-dir")); err != nil {
		t.Fatal(err)
	}
	writeAsset(t, root, "README.md", "keep", time.Unix(1, 0))
	writeAsset(t, root, ".deck/preview.json", "{}", time.Unix(1, 0))

	listing := scanOrFail(t, root)
	aVersion := versionOf(t, listing, ".deck/assets/a.png")
	changedVersion := versionOf(t, listing, ".deck/assets/changed.png")
	yVersion := versionOf(t, listing, ".deck/build/x/y.txt")
	dirVersion := versionOf(t, listing, ".deck/build/empty")
	// The file changes after the user selected it.
	if err := os.WriteFile(changed, []byte("rewritten"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(changed, changedAt, changedAt); err != nil {
		t.Fatal(err)
	}

	requests := []AssetDeleteRequest{
		{Path: ".deck/assets/a.png", Version: aVersion},
		{Path: ".deck/assets/a.png", Version: aVersion},
		{Path: ".deck/assets/changed.png", Version: changedVersion},
		{Path: ".deck/build/x/y.txt", Version: "1-not-the-version"},
		{Path: ".deck/build/empty", Version: dirVersion},
		{Path: ".deck/build/x", Version: dirVersion},
		{Path: ".deck/assets/link", Version: "v"},
		{Path: ".deck/assets/linked-dir/secret.txt", Version: "v"},
		{Path: ".deck/assets/never-existed.png", Version: "v"},
		{Path: ".deck/assets/gone/deeper.png", Version: "v"},
		{Path: "README.md", Version: "v"},
		{Path: ".deck/preview.json", Version: "v"},
		{Path: ".deck/assets", Version: "v"},
		{Path: ".deck/assets/", Version: "v"},
		{Path: ".deck/assets/../../README.md", Version: "v"},
		{Path: "/etc/passwd", Version: "v"},
		{Path: "./.deck/build/x/y.txt", Version: yVersion},
	}
	results, err := DeleteAssets(root, requests)
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	want := []struct{ outcome, reason string }{
		{AssetDeleted, ""},
		{AssetSkipped, "Already gone"},
		{AssetSkipped, "Changed on disk after you selected it — modified 14:06"},
		{AssetSkipped, "Changed on disk after you selected it — modified " + time.Unix(1000, 0).Format("15:04")},
		{AssetSkipped, "Not a regular file"},
		{AssetSkipped, "Not a regular file"},
		{AssetSkipped, "Not a regular file"},
		{AssetSkipped, "Outside the agent locations"},
		{AssetSkipped, "Already gone"},
		{AssetSkipped, "Already gone"},
		{AssetSkipped, "Outside the agent locations"},
		{AssetSkipped, "Outside the agent locations"},
		{AssetSkipped, "Outside the agent locations"},
		{AssetSkipped, "Outside the agent locations"},
		{AssetSkipped, "Outside the agent locations"},
		{AssetSkipped, "Outside the agent locations"},
		{AssetDeleted, ""},
	}
	if len(results) != len(want) {
		t.Fatalf("results = %d, want %d", len(results), len(want))
	}
	for i, w := range want {
		got := results[i]
		if got.Path != requests[i].Path || got.Outcome != w.outcome || got.Reason != w.reason {
			t.Errorf("%s: %s %q, want %s %q", requests[i].Path, got.Outcome, got.Reason, w.outcome, w.reason)
		}
	}

	for _, kept := range []string{
		".deck/assets/changed.png", ".deck/build/empty", ".deck/build/x",
		".deck/assets/link", "README.md", ".deck/preview.json",
	} {
		if _, err := os.Lstat(filepath.Join(root, kept)); err != nil {
			t.Errorf("%s was removed: %v", kept, err)
		}
	}
	if _, err := os.Stat(secret); err != nil {
		t.Errorf("the link's target outside the project was removed: %v", err)
	}
	for _, gone := range []string{".deck/assets/a.png", ".deck/build/x/y.txt"} {
		if _, err := os.Lstat(filepath.Join(root, gone)); !os.IsNotExist(err) {
			t.Errorf("%s still exists: %v", gone, err)
		}
	}
}

func TestScanAssetsListsWriteOnlyAndLargeFilesWithoutReading(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores file read permissions")
	}
	root := t.TempDir()
	writeAsset(t, root, ".deck/assets/write-only.txt", "w", time.Unix(1, 0))
	if err := os.Chmod(filepath.Join(root, ".deck/assets/write-only.txt"), 0o200); err != nil {
		t.Fatal(err)
	}
	writeAsset(t, root, ".deck/build/large.bin", "", time.Unix(1, 0))
	large, err := os.OpenFile(filepath.Join(root, ".deck/build/large.bin"), os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := large.Truncate(1 << 40); err != nil {
		t.Fatal(err)
	}
	if err := large.Close(); err != nil {
		t.Fatal(err)
	}
	listing := scanOrFail(t, root)
	version := versionOf(t, listing, ".deck/assets/write-only.txt")
	if got := versionOf(t, listing, ".deck/build/large.bin"); got == "" {
		t.Fatal("the sparse large file disappeared from the metadata listing")
	}
	results, err := DeleteAssets(root, []AssetDeleteRequest{{Path: ".deck/assets/write-only.txt", Version: version}})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].Outcome != AssetDeleted {
		t.Fatalf("write-only delete = %+v", results)
	}
}

func TestDeleteAssetsReportsAPermissionRefusal(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	root := t.TempDir()
	writeAsset(t, root, ".deck/assets/held.png", "h", time.Unix(1, 0))
	version := versionOf(t, scanOrFail(t, root), ".deck/assets/held.png")
	dir := filepath.Join(root, ".deck/assets")
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
	results, err := DeleteAssets(root, []AssetDeleteRequest{{Path: ".deck/assets/held.png", Version: version}})
	if err != nil {
		t.Fatal(err)
	}
	if results[0].Outcome != AssetSkipped || results[0].Reason != "The workspace refused: permission denied" {
		t.Fatalf("result = %+v", results[0])
	}
	if _, err := os.Stat(filepath.Join(dir, "held.png")); err != nil {
		t.Fatalf("held.png removed: %v", err)
	}
}

func serveAssets(t *testing.T, handler func(http.ResponseWriter, *http.Request), id string, req *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	req.SetPathValue("id", id)
	rec := httptest.NewRecorder()
	handler(rec, req)
	return rec
}

func TestAssetRoutesAnswerUnknownProjectFirst(t *testing.T) {
	h, _, _ := uploadHandler(t)
	for name, call := range map[string]func() *httptest.ResponseRecorder{
		"list": func() *httptest.ResponseRecorder {
			return serveAssets(t, h.ListAssets, "missing", httptest.NewRequest(http.MethodGet, "/api/projects/missing/assets", nil))
		},
		"events": func() *httptest.ResponseRecorder {
			return serveAssets(t, h.AssetsEvents, "missing", httptest.NewRequest(http.MethodGet, "/api/projects/missing/assets/events", nil))
		},
		"delete": func() *httptest.ResponseRecorder {
			return serveAssets(t, h.DeleteAssets, "missing", httptest.NewRequest(http.MethodPost, "/api/projects/missing/assets/delete", strings.NewReader(`{"files":[{"path":"x","version":"y"}]}`)))
		},
	} {
		if rec := call(); rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "no such project") {
			t.Errorf("%s: %d %s", name, rec.Code, rec.Body.String())
		}
	}
}

func TestAssetRoutesListAndDeleteOverHTTP(t *testing.T) {
	h, id, dir := uploadHandler(t)
	writeAsset(t, dir, ".deck/assets/a.png", "a", time.Unix(1000, 0))
	writeAsset(t, dir, ".deck/build/x/y.txt", "y", time.Unix(1000, 0))

	rec := serveAssets(t, h.ListAssets, id, httptest.NewRequest(http.MethodGet, "/api/projects/x/assets", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("list: %d %s", rec.Code, rec.Body.String())
	}
	var raw map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	first := raw["locations"].([]any)[0].(map[string]any)["entries"].([]any)[0].(map[string]any)
	if v, ok := first["locked"]; !ok || v != nil {
		t.Fatalf("a deletable file must carry locked: null, got %v", first)
	}
	var listing AssetListing
	if err := json.Unmarshal(rec.Body.Bytes(), &listing); err != nil {
		t.Fatal(err)
	}
	version := versionOf(t, &listing, ".deck/assets/a.png")

	body, _ := json.Marshal(map[string]any{"files": []AssetDeleteRequest{{Path: ".deck/assets/a.png", Version: version}}})
	rec = serveAssets(t, h.DeleteAssets, id, httptest.NewRequest(http.MethodPost, "/api/projects/x/assets/delete", bytes.NewReader(body)))
	if rec.Code != http.StatusOK || rec.Body.String() != `{"results":[{"path":".deck/assets/a.png","outcome":"deleted"}]}`+"\n" {
		t.Fatalf("delete: %d %s", rec.Code, rec.Body.String())
	}
}

func TestAssetDeleteRefusesMalformedRequests(t *testing.T) {
	h, id, _ := uploadHandler(t)
	tooMany := make([]AssetDeleteRequest, MaxAssetDeleteFiles+1)
	for i := range tooMany {
		tooMany[i] = AssetDeleteRequest{Path: ".deck/assets/a", Version: "v"}
	}
	overCap, _ := json.Marshal(map[string]any{"files": tooMany})
	for name, body := range map[string]string{
		"not JSON":        "files",
		"no files":        `{"files":[]}`,
		"missing version": `{"files":[{"path":".deck/assets/a"}]}`,
		"missing path":    `{"files":[{"version":"v"}]}`,
		"over the cap":    string(overCap),
	} {
		rec := serveAssets(t, h.DeleteAssets, id, httptest.NewRequest(http.MethodPost, "/api/projects/x/assets/delete", strings.NewReader(body)))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: %d %s", name, rec.Code, rec.Body.String())
		}
	}
}

func TestAssetsEventsStreamsAScanChange(t *testing.T) {
	h, id, dir := uploadHandler(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.SetPathValue("id", id)
		h.AssetsEvents(w, r)
	}))
	defer server.Close()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/api/projects/x/assets/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("events response = %d %q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	lines := make(chan string, 8)
	go func() {
		reader := bufio.NewReader(resp.Body)
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				close(lines)
				return
			}
			lines <- line
		}
	}()
	next := func() string {
		t.Helper()
		select {
		case line := <-lines:
			return line
		case <-time.After(2 * assetsEventsInterval):
			t.Fatal("timed out waiting for an event line")
			return ""
		}
	}
	if line := next(); line != ": connected\n" {
		t.Fatalf("connected = %q", line)
	}
	next()
	writeAsset(t, dir, ".deck/assets/new.png", "x", time.Unix(1, 0))
	if line := next(); line != "event: assets-changed\n" {
		t.Fatalf("event = %q", line)
	}
}

func TestDeleteAssetsSkipsAFileRewrittenInPlace(t *testing.T) {
	root := t.TempDir()
	file := writeAsset(t, root, ".deck/assets/a.png", "selected", time.Unix(1000, 0))
	version := versionOf(t, scanOrFail(t, root), ".deck/assets/a.png")
	if err := os.WriteFile(file, []byte("rewritten in place"), 0o644); err != nil {
		t.Fatal(err)
	}
	results, err := DeleteAssets(root, []AssetDeleteRequest{{Path: ".deck/assets/a.png", Version: version}})
	if err != nil {
		t.Fatal(err)
	}
	if got := results[0]; got.Outcome != AssetSkipped || !strings.HasPrefix(got.Reason, "Changed on disk after you selected it — modified ") {
		t.Fatalf("result = %+v", got)
	}
	if body, err := os.ReadFile(file); err != nil || string(body) != "rewritten in place" {
		t.Fatalf("the rewritten file was not kept: %q %v", body, err)
	}
}
