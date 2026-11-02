package project

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Inspection answers the add-project field from the filesystem inside the
// allowed roots. What was typed decides the shape below the field: a path
// that does not exist is created, a directory is recognised for what it is,
// and a URL is cloned. Nothing here touches the network — the first fetch is
// the clone itself.
type Inspection struct {
	// Kind is one of the Kind* constants.
	Kind string `json:"kind"`
	// Path is the directory the input names: the path itself, or a clone's
	// destination. Canonical when it exists, cleaned when it does not.
	Path   string `json:"path"`
	Exists bool   `json:"exists"`
	// Empty distinguishes an existing empty clone destination from one whose
	// files or hidden directories are omitted from the display entries.
	Empty bool `json:"empty,omitempty"`
	// Remote and Head describe a repository root or linked worktree, in the
	// host form you would recognise (github.com/owner/repo) and the
	// one-line summary that tells two checkouts of one repository apart.
	Remote string `json:"remote,omitempty"`
	Head   string `json:"head,omitempty"`
	// Parent is the repository this directory belongs to without being its
	// root: a worktree's main repository, or the repository enclosing a plain
	// or absent path, or a clone destination. Its store is the one that
	// governs here, which is what the boundary warning is about.
	Parent *ParentRepository `json:"parent,omitempty"`
	// StorePath is the task engine's store the project would use, exactly as
	// the engine resolves it: under the repository's git common directory,
	// which is why a worktree cannot have its own. Empty when nothing would
	// own a store — a plain directory is not a repository.
	StorePath string `json:"store_path,omitempty"`
	// Entries are what an existing directory holds: ".git" first when there
	// is one, then its visible subdirectories.
	Entries []string `json:"entries"`
	// CloneURL is the URL git would be handed for a clone.
	CloneURL string `json:"clone_url,omitempty"`
	// SameClone reports that the destination already holds a clone of this
	// remote, so nothing needs fetching.
	SameClone bool `json:"same_clone,omitempty"`
}

// ParentRepository names an enclosing repository: the registered project's
// name when the sidebar has it, otherwise its directory name.
type ParentRepository struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

const (
	KindAbsent     = "absent"
	KindPlain      = "plain"
	KindRepository = "repository"
	KindWorktree   = "worktree"
	KindClone      = "clone"
)

// storeRelPath is where the task engine keeps a repository's store, relative
// to the git common directory.
const storeRelPath = "noise-tasks/tasks.sqlite"

var (
	schemedURL   = regexp.MustCompile(`^(https?://|git@|ssh://)`)
	hostedURL    = regexp.MustCompile(`(?i)^[\w.-]+\.[a-z]{2,}/`)
	shorthandURL = regexp.MustCompile(`^[\w.-]+/[\w.-]+$`)
	slugUnsafe   = regexp.MustCompile(`[^A-Za-z0-9._-]`)
)

// Inspect classifies input. base is the container a clone lands in; empty
// means the first allowed root. nameAt resolves a registered project's name
// for a repository path, or "" when the sidebar does not have it.
func Inspect(ctx context.Context, input, base string, allowedRoots []string, nameAt func(path string) string) (*Inspection, error) {
	v := strings.TrimSpace(input)
	if v == "" {
		return nil, fmt.Errorf("%w: nothing to inspect", ErrInvalidDir)
	}
	roots, err := canonicalRoots(allowedRoots)
	if err != nil {
		return nil, err
	}
	if len(roots) == 0 {
		return nil, fmt.Errorf("%w: no project root is configured", ErrInvalidDir)
	}
	if nameAt == nil {
		nameAt = func(string) string { return "" }
	}
	git, err := exec.LookPath("git")
	if err != nil {
		return nil, fmt.Errorf("resolve Git executable: %w", err)
	}
	in := inspector{ctx: ctx, git: git, roots: roots, nameAt: nameAt}

	// Scheme'd and host-qualified forms are unambiguous. The bare owner/repo
	// shorthand has the shape of a relative path, so it yields to a directory
	// that actually exists; when neither exists the shorthand wins, because a
	// path that does not exist is already served by typing it absolutely.
	isURL := schemedURL.MatchString(v) || hostedURL.MatchString(v)
	if !isURL && shorthandURL.MatchString(v) && !strings.HasPrefix(v, "/") {
		isURL = !isDir(filepath.Join(roots[0], strings.TrimRight(v, "/")), roots)
	}
	if isURL {
		return in.clone(v, base)
	}
	return in.path(v)
}

type inspector struct {
	ctx    context.Context
	git    string
	roots  []string
	nameAt func(string) string
}

func (in inspector) path(v string) (*Inspection, error) {
	path := strings.TrimRight(v, "/")
	if !filepath.IsAbs(path) {
		path = filepath.Join(in.roots[0], path)
	}
	path = filepath.Clean(path)
	exists, err := resolveDir(&path, in.roots)
	if err != nil {
		return nil, err
	}
	if !exists {
		// An absent path is confined by what exists above it: the nearest
		// existing ancestor is resolved through any symlink and checked
		// against the roots before Git looks at it, so a link inside a root
		// cannot lend an outside repository's facts to a path that appears
		// to sit inside. The absent tail is re-rooted under the resolved
		// ancestor, so what is reported is where the directory would be.
		ancestor := nearestExisting(path, in.roots)
		if ancestor == "" {
			return nil, fmt.Errorf("%w: %s", ErrInvalidDir, path)
		}
		tail, err := filepath.Rel(ancestor, path)
		if err != nil {
			return nil, fmt.Errorf("%w: %s", ErrInvalidDir, path)
		}
		resolved, err := Canonicalize(ancestor)
		if err != nil {
			return nil, err
		}
		path = filepath.Join(resolved, tail)
	}
	if !underAnyRoot(path, in.roots) {
		return nil, fmt.Errorf("%w: %s", ErrOutsideRoots, path)
	}
	out := &Inspection{Path: path, Exists: exists, Entries: []string{}}
	if exists {
		out.Entries = in.entries(path)
		// describeRepository needs remote/head before it knows whether git
		// named an outside directory, so it reads entries first and the
		// git facts after; a failed confinement leaves only the entries.
		top, ok := in.toplevel(path)
		switch {
		case ok && top == path:
			in.describeRepository(out, path)
			return out, nil
		case ok:
			out.Kind = KindPlain
			in.setParent(out, top)
			return out, nil
		default:
			out.Kind = KindPlain
			return out, nil
		}
	}
	out.Kind = KindAbsent
	if top, ok := in.toplevel(nearestExisting(path, in.roots)); ok {
		in.setParent(out, top)
		return out, nil
	}
	out.StorePath = filepath.Join(path, ".git", storeRelPath)
	return out, nil
}

func (in inspector) clone(v, base string) (*Inspection, error) {
	container := in.roots[0]
	if strings.TrimSpace(base) != "" {
		canonical, err := Canonicalize(base)
		if err != nil {
			return nil, err
		}
		if !underAnyRoot(canonical, in.roots) {
			return nil, fmt.Errorf("%w: %s", ErrOutsideRoots, canonical)
		}
		if !isDir(canonical, in.roots) {
			return nil, fmt.Errorf("%w: %s", ErrInvalidDir, canonical)
		}
		container = canonical
	}
	trimmed := strings.TrimRight(v, "/")
	slug := slugUnsafe.ReplaceAllString(strings.TrimSuffix(filepath.Base(trimmed), ".git"), "-")
	if slug == "" || slug == "." {
		return nil, fmt.Errorf("%w: the repository URL has no name to clone into", ErrInvalidDir)
	}
	dest := filepath.Join(container, slug)
	exists, err := resolveDir(&dest, in.roots)
	if err != nil {
		return nil, err
	}
	// The container is confined, but a link inside it named after the slug
	// can resolve anywhere; the destination is checked as resolved before
	// anything looks at it.
	if !underAnyRoot(dest, in.roots) {
		return nil, fmt.Errorf("%w: %s", ErrOutsideRoots, dest)
	}
	out := &Inspection{Kind: KindClone, Path: dest, Exists: exists, Entries: []string{}, CloneURL: cloneURL(trimmed)}
	if exists {
		out.Entries = in.entries(dest)
		if children, err := allowedReadDir(dest, in.roots); err == nil {
			out.Empty = len(children) == 0
		}
		if top, ok := in.toplevel(dest); ok && top == dest {
			// The typed form may be shorter than the stored remote
			// (fable/deck against github.com/fable/deck), so the remote must
			// contain what was typed, never the other way round.
			remote := in.remote(dest)
			if remote != "" && strings.Contains(remote, hostForm(trimmed)) {
				out.SameClone = true
				out.Remote = remote
				out.StorePath = in.storePath(dest)
				return out, nil
			}
		}
	}
	// A destination inside a repository resolves to that repository's store,
	// exactly as a worktree does. Walk from the container, never from the
	// destination: a destination that is itself a repository is a collision,
	// not a nesting.
	if top, ok := in.toplevel(nearestExisting(container, in.roots)); ok {
		in.setParent(out, top)
		return out, nil
	}
	out.StorePath = filepath.Join(dest, ".git", storeRelPath)
	return out, nil
}

// describeRepository fills a repository root's facts, telling a linked
// worktree from a main repository by whether its git directory is its own.
func (in inspector) describeRepository(out *Inspection, path string) {
	out.Remote = in.remote(path)
	out.Head = in.head(path)
	out.StorePath = in.storePath(path)
	gitDir, _ := in.run(path, "rev-parse", "--path-format=absolute", "--git-dir")
	common, _ := in.run(path, "rev-parse", "--path-format=absolute", "--git-common-dir")
	for _, d := range []string{gitDir, common} {
		if resolved, err := Canonicalize(d); err != nil || !underAnyRoot(resolved, in.roots) {
			// git named a directory outside the roots after all; the facts
			// read so far came from inside, and none of what follows may.
			*out = Inspection{Kind: KindPlain, Path: path, Exists: true, Entries: out.Entries}
			return
		}
	}
	if gitDir != "" && common != "" && filepath.Clean(gitDir) != filepath.Clean(common) {
		out.Kind = KindWorktree
		main := filepath.Dir(filepath.Clean(common))
		out.Parent = &ParentRepository{Name: in.name(main), Path: main}
		return
	}
	out.Kind = KindRepository
}

func (in inspector) setParent(out *Inspection, top string) {
	out.Parent = &ParentRepository{Name: in.name(top), Path: top}
	out.StorePath = in.storePath(top)
}

func (in inspector) name(path string) string {
	if name := in.nameAt(path); name != "" {
		return name
	}
	return filepath.Base(path)
}

// storePath is the engine's, not ours: it resolves under the repository's
// git common directory, so a worktree reports its main repository's store.
func (in inspector) storePath(repo string) string {
	common, err := in.run(repo, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil || common == "" {
		return filepath.Join(repo, ".git", storeRelPath)
	}
	return filepath.Join(filepath.Clean(common), storeRelPath)
}

// toplevel reports the repository root that contains dir, when one does.
func (in inspector) toplevel(dir string) (string, bool) {
	if dir == "" {
		return "", false
	}
	top, err := in.run(dir, "rev-parse", "--show-toplevel")
	if err != nil || top == "" {
		return "", false
	}
	canonical, err := Canonicalize(top)
	if err != nil || !underAnyRoot(canonical, in.roots) {
		return "", false
	}
	return canonical, true
}

func (in inspector) remote(repo string) string {
	url, err := in.run(repo, "config", "--get", "remote.origin.url")
	if err != nil {
		return ""
	}
	return hostForm(url)
}

// head is the one line that tells two checkouts of one repository apart:
// branch, short commit, its age, and whether the working tree is clean.
func (in inspector) head(repo string) string {
	branch, err := in.run(repo, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil || branch == "" {
		branch = "HEAD"
	}
	if branch == "HEAD" {
		branch = "detached"
	}
	short, err := in.run(repo, "rev-parse", "--short", "HEAD")
	if err != nil || short == "" {
		return branch + " · no commits yet"
	}
	parts := []string{branch, short}
	if stamp, err := in.run(repo, "log", "-1", "--format=%ct"); err == nil {
		if unix, err := strconv.ParseInt(stamp, 10, 64); err == nil {
			parts = append(parts, sinceLabel(time.Since(time.Unix(unix, 0)))+" ago")
		}
	}
	status, err := in.run(repo, "status", "--porcelain")
	switch {
	case err != nil:
	case status == "":
		parts = append(parts, "working tree clean")
	default:
		n := strings.Count(status, "\n") + 1
		if n == 1 {
			parts = append(parts, "1 file modified")
		} else {
			parts = append(parts, fmt.Sprintf("%d files modified", n))
		}
	}
	return strings.Join(parts, " · ")
}

// entries lists what a directory holds the way the dialog states it: .git
// first when present, then the visible subdirectories by name.
func (in inspector) entries(dir string) []string {
	out := []string{}
	children, err := allowedReadDir(dir, in.roots)
	if err != nil {
		return out
	}
	names := make([]string, 0, len(children))
	for _, child := range children {
		name := child.Name()
		if name == ".git" {
			out = append(out, ".git")
			continue
		}
		if strings.HasPrefix(name, ".") || !child.IsDir() {
			continue
		}
		names = append(names, name)
	}
	sort.Strings(names)
	return append(out, names...)
}

// run invokes git with discovery ceilinged at the allowed roots, so a
// repository above a root is never found, and only after gitConfined has
// checked that the .git git would read inside the root points nowhere
// outside it. The two together mean git never opens a directory outside
// the roots on this package's behalf.
func (in inspector) run(dir string, args ...string) (string, error) {
	if !in.gitConfined(dir) {
		return "", fmt.Errorf("%w: %s", ErrOutsideRoots, dir)
	}
	argv := append([]string{"-C", dir}, args...)
	cmd := exec.CommandContext(in.ctx, in.git, argv...)
	cmd.Env = gitEnv(in.roots)
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func (in inspector) gitConfined(dir string) bool {
	return gitDirConfined(dir, in.roots)
}

// gitDirConfined walks up from dir to its root looking for the .git entry
// git would use, and resolves what that entry names — a gitdir file, a
// linked worktree's commondir, or a symlinked directory — before git reads
// it. A .git that points outside every root is a repository Deck may not
// read, and the directory is then treated as holding no repository at all.
// Every git invocation this package makes on a path you chose goes
// through this first; the discovery ceiling stops the upward walk, and this
// stops an explicit pointer.
func gitDirConfined(dir string, roots []string) bool {
	if !underAnyRoot(dir, roots) {
		return false
	}
	for p := dir; ; p = filepath.Dir(p) {
		if !underAnyRoot(p, roots) {
			return true
		}
		entry := filepath.Join(p, ".git")
		root, rel, err := openAllowedPath(entry, roots)
		if err != nil {
			return false
		}
		info, err := root.Lstat(rel)
		_ = root.Close()
		if err == nil {
			return gitEntryConfined(entry, info, roots)
		}
		if !errors.Is(err, os.ErrNotExist) {
			return false
		}
		if filepath.Dir(p) == p {
			return true
		}
	}
}

func gitEntryConfined(entry string, info os.FileInfo, roots []string) bool {
	gitDir := entry
	if !info.IsDir() {
		root, rel, err := openAllowedPath(entry, roots)
		if err != nil {
			return false
		}
		target, err := root.Readlink(rel)
		_ = root.Close()
		if info.Mode()&os.ModeSymlink != 0 && err == nil {
			gitDir = target
		} else {
			body, err := allowedReadFile(entry, roots)
			if err != nil {
				return false
			}
			line := strings.TrimSpace(string(body))
			if !strings.HasPrefix(line, "gitdir:") {
				return false
			}
			gitDir = strings.TrimSpace(strings.TrimPrefix(line, "gitdir:"))
		}
		if !filepath.IsAbs(gitDir) {
			gitDir = filepath.Join(filepath.Dir(entry), gitDir)
		}
	}
	resolved, err := Canonicalize(gitDir)
	if err != nil || !underAnyRoot(resolved, roots) {
		return false
	}
	// A linked worktree's git directory names the main repository's in a
	// commondir file; that is where the store lives, so it is confined too.
	if body, err := allowedReadFile(filepath.Join(resolved, "commondir"), roots); err == nil {
		common := strings.TrimSpace(string(body))
		if !filepath.IsAbs(common) {
			common = filepath.Join(resolved, common)
		}
		resolvedCommon, err := Canonicalize(common)
		if err != nil || !underAnyRoot(resolvedCommon, roots) {
			return false
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return false
	}
	return true
}

// resolveDir canonicalizes an existing directory in place and reports
// whether it exists. A path that names a file is an error, not an absence.
func resolveDir(path *string, roots []string) (bool, error) {
	canonical, err := Canonicalize(*path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !underAnyRoot(canonical, roots) {
		return false, fmt.Errorf("%w: %s", ErrOutsideRoots, canonical)
	}
	info, err := allowedStat(canonical, roots)
	if err != nil || !info.IsDir() {
		return false, fmt.Errorf("%w: %s", ErrInvalidDir, canonical)
	}
	*path = canonical
	return true, nil
}

func isDir(path string, roots []string) bool {
	canonical, err := Canonicalize(path)
	if err != nil {
		return false
	}
	info, err := allowedStat(canonical, roots)
	return err == nil && info.IsDir()
}

// nearestExisting climbs from an absent path to the first ancestor that
// exists, so an absent path can still be placed inside or outside a
// repository.
func nearestExisting(path string, roots []string) string {
	for p := path; ; p = filepath.Dir(p) {
		if canonical, err := Canonicalize(p); err == nil {
			if !underAnyRoot(canonical, roots) {
				return p
			}
			if info, err := allowedStat(canonical, roots); err == nil && info.IsDir() {
				return p
			}
		}
		if filepath.Dir(p) == p {
			return ""
		}
	}
}

// hostForm reduces any way of writing a remote to github.com/owner/repo:
// scheme and .git dropped, the scp-style colon read as a slash.
func hostForm(url string) string {
	s := strings.TrimSpace(url)
	s = schemedURL.ReplaceAllString(s, "")
	s = strings.TrimSuffix(strings.TrimRight(s, "/"), ".git")
	if at := strings.Index(s, ":"); at > 0 && !strings.Contains(s[:at], "/") {
		s = s[:at] + "/" + s[at+1:]
	}
	return s
}

// cloneURL is what git is handed: a scheme'd URL as typed, a host-qualified
// one over https, and the bare shorthand on GitHub.
func cloneURL(v string) string {
	switch {
	case schemedURL.MatchString(v):
		return v
	case hostedURL.MatchString(v):
		return "https://" + v
	default:
		return "https://github.com/" + v
	}
}

func sinceLabel(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "< 1m"
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}
