package project

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"path"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// The per-project file endpoints. The registry supplies the
// canonical project path, and the file layer binds all traversal and I/O to
// descriptors acquired beneath that root.

// fileError maps the file-layer errors onto responses. Kept in one place
// so a new caller cannot invent a different status for the same refusal.
func (h *Handler) fileError(w http.ResponseWriter, err error) {
	var conflict *UploadConflict
	if errors.As(err, &conflict) {
		h.uploadConflictResponse(w, conflict)
		return
	}
	switch {
	case errors.Is(err, syscall.EEXIST):
		// The conflicting name vanished before metadata could be read.
		// There is no observed file version on which to offer Replace.
		h.err(w, http.StatusConflict, "conflict", "destination changed while uploading; check the folder and try again")
	case errors.Is(err, ErrOutsideProject):
		h.err(w, http.StatusForbidden, "outside_project", err.Error())
	case errors.Is(err, ErrFileNotFound):
		h.err(w, http.StatusNotFound, "not_found", err.Error())
	case errors.Is(err, ErrNotRegularFile):
		h.err(w, http.StatusBadRequest, "not_a_file", err.Error())
	case errors.Is(err, ErrTooLarge), errors.Is(err, ErrUploadTooLarge):
		h.err(w, http.StatusRequestEntityTooLarge, "too_large", err.Error())
	case errors.Is(err, ErrNotText):
		h.err(w, http.StatusUnsupportedMediaType, "not_text", err.Error())
	case errors.Is(err, ErrNotImage):
		h.err(w, http.StatusUnsupportedMediaType, "not_image", err.Error())
	default:
		h.err(w, http.StatusBadRequest, "bad_request", err.Error())
	}
}

// uploadConflictResponse is the ask, not the refusal: the metadata and the
// opaque version the user needs to choose Replace, rename, or cancel,
// observed at the moment of refusal rather than in a possibly stale
// browser listing.
func (h *Handler) uploadConflictResponse(w http.ResponseWriter, conflict *UploadConflict) {
	reason := "exists"
	if conflict.Dir {
		reason = "directory"
	} else if conflict.NotFile {
		reason = "not_a_file"
	} else if conflict.Changed {
		reason = "changed"
	}
	h.json(w, http.StatusConflict, map[string]any{
		"error":   "conflict",
		"message": conflict.Error(),
		"reason":  reason,
		"existing": map[string]any{
			"name":    conflict.Leaf,
			"dir":     conflict.Dir,
			"size":    conflict.Size,
			"mtime":   conflict.MTime.UTC().Format("2006-01-02T15:04:05Z"),
			"version": conflict.Version,
		},
	})
}

// uploadConsentFromRequest reads explicit replacement consent off the
// query. `replace` alone is not consent: the request must also carry the
// opaque version the server reported for the file being replaced, so a
// replayed or scripted replace cannot destroy something the user never
// saw, and a destination that changed since the approval is re-asked
// rather than overwritten.
func uploadConsentFromRequest(r *http.Request) (*UploadConsent, error) {
	query := r.URL.Query()
	if !truthy(query.Get("replace")) {
		if query.Has("expected_version") {
			return nil, errors.New("expected_version requires replace=1")
		}
		return nil, nil
	}
	version := strings.TrimSpace(query.Get("expected_version"))
	if version == "" || len(version) > 128 {
		return nil, errors.New("replace requires expected_version naming the existing file")
	}
	return &UploadConsent{Version: version}, nil
}

func truthy(v string) bool {
	return v == "1" || v == "true"
}

// Keep the standard library's default form-part limit while streaming bodies.
const maxMultipartParts = 1000

var (
	errUnreadableUpload    = errors.New("unreadable upload")
	errUploadMissingFile   = errors.New("upload is missing its file part")
	errUploadMultipleFiles = errors.New("upload has multiple file parts")
	errUploadTooManyParts  = errors.New("upload has too many multipart parts")
)

type multipartUploadStream struct {
	reader *multipart.Reader
	parts  int
}

func (s *multipartUploadStream) nextPart() (*multipart.Part, error) {
	part, err := s.reader.NextPart()
	if err != nil {
		return nil, err
	}
	s.parts++
	if s.parts > maxMultipartParts {
		return nil, errUploadTooManyParts
	}
	return part, nil
}

func isUploadFilePart(part *multipart.Part) bool {
	return part.FormName() == "file" && part.FileName() != ""
}

func unreadableUpload(err error) error {
	return fmt.Errorf("%w: %w", errUnreadableUpload, err)
}

func discardMultipartPart(part *multipart.Part) error {
	if _, err := io.Copy(io.Discard, part); err != nil {
		return unreadableUpload(err)
	}
	return nil
}

type multipartFilePart struct {
	part *multipart.Part
}

func (p multipartFilePart) Read(buf []byte) (int, error) {
	n, err := p.part.Read(buf)
	if err != nil && !errors.Is(err, io.EOF) {
		return n, unreadableUpload(err)
	}
	return n, err
}

type multipartRemainder struct {
	stream *multipartUploadStream
	done   bool
}

// Read validates trailing parts before reporting EOF, which keeps WriteFile
// from committing the staged file when a second intended file part exists.
func (r *multipartRemainder) Read(buf []byte) (int, error) {
	if len(buf) == 0 {
		return 0, nil
	}
	if r.done {
		return 0, io.EOF
	}
	r.done = true
	for {
		part, err := r.stream.nextPart()
		switch {
		case errors.Is(err, io.EOF):
			return 0, io.EOF
		case err != nil:
			return 0, unreadableUpload(err)
		case isUploadFilePart(part):
			return 0, errUploadMultipleFiles
		}
		if err := discardMultipartPart(part); err != nil {
			return 0, err
		}
	}
}

func multipartUploadSource(r *http.Request) (io.Reader, error) {
	reader, err := r.MultipartReader()
	if err != nil {
		return nil, unreadableUpload(err)
	}
	stream := &multipartUploadStream{reader: reader}
	for {
		part, err := stream.nextPart()
		switch {
		case errors.Is(err, io.EOF):
			return nil, errUploadMissingFile
		case err != nil:
			return nil, unreadableUpload(err)
		case isUploadFilePart(part):
			return io.MultiReader(
				multipartFilePart{part: part},
				&multipartRemainder{stream: stream},
			), nil
		}
		if err := discardMultipartPart(part); err != nil {
			return nil, err
		}
	}
}

func normalizeUploadError(err error) error {
	var maxBytes *http.MaxBytesError
	if errors.As(err, &maxBytes) {
		return fmt.Errorf("%w: request exceeds %d bytes", ErrUploadTooLarge, maxBytes.Limit)
	}
	return err
}

func (h *Handler) uploadError(w http.ResponseWriter, err error) {
	err = normalizeUploadError(err)
	if errors.Is(err, errUnreadableUpload) {
		h.err(w, http.StatusBadRequest, "bad_request", errUnreadableUpload.Error())
		return
	}
	h.fileError(w, err)
}

// projectRoot resolves the {id} route parameter to a registered project's
// canonical path. A file request for an unknown project is a 404 about
// the PROJECT, before any path is looked at.
func (h *Handler) projectRoot(w http.ResponseWriter, r *http.Request) (string, bool) {
	p, err := h.repo.Get(r.Context(), r.PathValue("id"))
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			h.err(w, http.StatusNotFound, "not_found", "no such project")
		} else {
			h.err(w, http.StatusInternalServerError, "internal", "project lookup failed")
		}
		return "", false
	}
	return p.CanonicalPath, true
}

// ListFiles serves one directory inside a project.
func (h *Handler) ListFiles(w http.ResponseWriter, r *http.Request) {
	root, ok := h.projectRoot(w, r)
	if !ok {
		return
	}
	listing, err := ListFiles(root, r.URL.Query().Get("path"))
	if err != nil {
		h.fileError(w, err)
		return
	}
	listing.Head, _ = gitHead(r.Context(), root)
	annotateChanges(r.Context(), root, listing)
	h.json(w, http.StatusOK, listing)
}

// ViewFile serves a text file's contents for the viewer.
func (h *Handler) ViewFile(w http.ResponseWriter, r *http.Request) {
	root, ok := h.projectRoot(w, r)
	if !ok {
		return
	}
	body, err := ReadTextFile(root, r.URL.Query().Get("path"))
	if err != nil {
		h.fileError(w, err)
		return
	}
	h.json(w, http.StatusOK, map[string]string{"content": body})
}

// DiffFile serves the selected file's changes against the last commit.
func (h *Handler) DiffFile(w http.ResponseWriter, r *http.Request) {
	root, ok := h.projectRoot(w, r)
	if !ok {
		return
	}
	body, err := DiffFile(r.Context(), root, r.URL.Query().Get("path"))
	if err != nil {
		h.fileError(w, err)
		return
	}
	h.json(w, http.StatusOK, map[string]string{"content": body})
}

const filesEventsInterval = 2 * time.Second

// FilesEvents keeps one directory listing live. ListFiles owns path
// validation and descriptor-based confinement; the fingerprint is only the
// metadata and change markers the listing already disclosed, so an edit in
// place is visible without treating the directory's own mtime as the source
// of truth. Each tick runs one bounded git status (gitStatusTimeout,
// gitStatusMaxOutput), so a stalled or huge status delays that tick rather
// than piling up work.
func (h *Handler) FilesEvents(w http.ResponseWriter, r *http.Request) {
	root, ok := h.projectRoot(w, r)
	if !ok {
		return
	}
	rel := r.URL.Query().Get("path")
	if _, err := ListFiles(root, rel); err != nil {
		h.fileError(w, err)
		return
	}
	h.streamChanges(w, r, "files-changed", filesEventsInterval, func() (string, error) {
		return filesFingerprint(r.Context(), root, rel)
	})
}

// streamChanges serves one change feed: it baselines fingerprint on
// connect, then names event each time a tick sees a different value. A
// fingerprint that starts failing (the watched folder vanished) is one
// event, not one per tick, until it reads again.
func (h *Handler) streamChanges(w http.ResponseWriter, r *http.Request, event string, interval time.Duration, fingerprint func() (string, error)) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		h.err(w, http.StatusInternalServerError, "internal", "streaming unsupported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	write := func(format string, args ...any) bool {
		if _, err := fmt.Fprintf(w, format, args...); err != nil {
			return false
		}
		flusher.Flush()
		return true
	}
	// The baseline is taken before the client hears "connected", so a
	// change made after that line is never folded into the baseline by a
	// slow first tick.
	last, _ := fingerprint()
	if !write(": connected\n\n") {
		return
	}

	missing := false
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
			current, err := fingerprint()
			if err != nil {
				if !missing {
					missing = true
					if !write("event: %s\ndata: {}\n\n", event) {
						return
					}
				}
				continue
			}
			missing = false
			if current == last {
				continue
			}
			last = current
			if !write("event: %s\ndata: {}\n\n", event) {
				return
			}
		}
	}
}

func filesFingerprint(ctx context.Context, root, rel string) (string, error) {
	listing, err := ListFiles(root, rel)
	if err != nil {
		return "", err
	}
	listing.Head, _ = gitHead(ctx, root)
	annotateChanges(ctx, root, listing)
	var fingerprint strings.Builder
	fingerprint.WriteString(listing.Head)
	for _, entry := range listing.Entries {
		fmt.Fprintf(&fingerprint, "%s\x00%d\x00%s\x00%t\x00", entry.Name, entry.Size, entry.MTime, entry.Dir)
		fmt.Fprintf(&fingerprint, "%s\x00", entry.Change)
	}
	return fingerprint.String(), nil
}

// DownloadFile streams a file.
//
// The response is forced to a download with a generic content type: this
// server and the console share an origin, so letting a browser render an
// uploaded file inline would run whatever HTML or SVG a user stored
// as though the console had served it.
func (h *Handler) DownloadFile(w http.ResponseWriter, r *http.Request) {
	root, ok := h.projectRoot(w, r)
	if !ok {
		return
	}
	rel := r.URL.Query().Get("path")
	f, info, err := OpenFile(root, rel)
	if err != nil {
		h.fileError(w, err)
		return
	}
	defer f.Close()

	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Disposition",
		mime.FormatMediaType("attachment", map[string]string{"filename": path.Base(rel)}))
	http.ServeContent(w, r, "", info.ModTime(), f)
}

// ImageFile streams an image for the viewer to render inline.
//
// Inline display is safe only for formats the browser decodes as pixels,
// so the content type is the sniffed allowlisted one and nosniff stops the
// browser from second-guessing it into HTML or SVG. The file can change
// under the listing, so nothing is cached; the client re-requests when the
// listing's mtime moves.
func (h *Handler) ImageFile(w http.ResponseWriter, r *http.Request) {
	root, ok := h.projectRoot(w, r)
	if !ok {
		return
	}
	rel := r.URL.Query().Get("path")
	f, info, contentType, err := OpenImageFile(root, rel)
	if err != nil {
		h.fileError(w, err)
		return
	}
	defer func() { _ = f.Close() }()

	w.Header().Set("Content-Type", contentType)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Disposition",
		mime.FormatMediaType("inline", map[string]string{"filename": path.Base(rel)}))
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("Content-Length", strconv.FormatInt(info.Size(), 10))
	w.WriteHeader(http.StatusOK)
	// A file that grows mid-read stops at the size already announced; one
	// that shrinks ends early. Either way the browser sees a truncated
	// image and shows it as undecodable.
	_, _ = io.Copy(w, io.LimitReader(f, info.Size()))
}

// UploadFile stores one file inside a project.
//
// The destination is the `path` query parameter — the user's chosen
// name — never the client-supplied multipart filename, which is attacker
// data in a request that already names its target.
func (h *Handler) UploadFile(w http.ResponseWriter, r *http.Request) {
	root, ok := h.projectRoot(w, r)
	if !ok {
		return
	}
	rel := strings.TrimSpace(r.URL.Query().Get("path"))
	if rel == "" {
		h.err(w, http.StatusBadRequest, "bad_request", "path is required")
		return
	}
	consent, err := uploadConsentFromRequest(r)
	if err != nil {
		h.err(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}

	// Bound the complete request independently of the per-file WriteFile bound.
	r.Body = http.MaxBytesReader(w, r.Body, MaxRequestBytes)
	defer r.Body.Close()

	src := io.Reader(r.Body)
	if ct := r.Header.Get("Content-Type"); strings.HasPrefix(ct, "multipart/form-data") {
		var err error
		src, err = multipartUploadSource(r)
		if err != nil {
			h.uploadError(w, err)
			return
		}
	}

	stored, n, err := WriteFile(root, rel, src, consent)
	if err != nil {
		h.uploadError(w, err)
		return
	}
	h.json(w, http.StatusCreated, map[string]any{"path": stored, "size": n})
}

// MountFiles registers the file routes, separately from Mount.
func (h *Handler) MountFiles(handle func(pattern string, hd http.Handler), wrap func(http.Handler) http.Handler) {
	handle("GET /api/projects/{id}/files", wrap(http.HandlerFunc(h.ListFiles)))
	handle("GET /api/projects/{id}/files/events", wrap(http.HandlerFunc(h.FilesEvents)))
	handle("GET /api/projects/{id}/files/view", wrap(http.HandlerFunc(h.ViewFile)))
	handle("GET /api/projects/{id}/files/diff", wrap(http.HandlerFunc(h.DiffFile)))
	handle("GET /api/projects/{id}/files/download", wrap(http.HandlerFunc(h.DownloadFile)))
	handle("GET /api/projects/{id}/files/image", wrap(http.HandlerFunc(h.ImageFile)))
	handle("POST /api/projects/{id}/files", wrap(http.HandlerFunc(h.UploadFile)))
	handle("GET /api/projects/{id}/assets", wrap(http.HandlerFunc(h.ListAssets)))
	handle("GET /api/projects/{id}/assets/events", wrap(http.HandlerFunc(h.AssetsEvents)))
	handle("POST /api/projects/{id}/assets/delete", wrap(http.HandlerFunc(h.DeleteAssets)))
}
