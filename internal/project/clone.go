package project

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/rs/zerolog"
)

// A clone is a job on the server, not a step in a dialog: it outlives the
// dialog that started it, keeps its place in the rail while it runs, and
// when it stops part-way the half-cloned directory is a state the app holds
// rather than a mess it hides. Only a clone is a job; every other way of
// adding a project is instant.

type CloneStatus string

const (
	CloneRunning CloneStatus = "running"
	CloneStopped CloneStatus = "stopped"
	CloneFailed  CloneStatus = "failed"
	CloneDone    CloneStatus = "done"
)

// CloneFailure names why a clone did not finish, in the terms the dialog
// states. Bad and private are ONE failure: GitHub answers 404 for a
// repository you cannot see, deliberately, so guessing which it was would be
// the app inventing a fact.
type CloneFailure string

const (
	CloneRefused      CloneFailure = "bad_or_private"
	CloneAuthRefused  CloneFailure = "auth"
	CloneDestNotEmpty CloneFailure = "dest_not_empty"
	CloneNetworkDied  CloneFailure = "network"
)

var (
	ErrCloneNotFound     = errors.New("clone job not found")
	ErrCloneInProgress   = errors.New("a clone is already running into that directory")
	ErrCloneNotStopped   = errors.New("the clone is not stopped")
	ErrCloneURLInvalid   = errors.New("not a repository URL")
	ErrCloneDestNotEmpty = errors.New("destination is not empty")
)

// CloneJob is the job as the API reports it.
type CloneJob struct {
	GitHubAccount      string       `json:"github_account,omitempty"`
	GitHubRepositoryID int64        `json:"github_repository_id,omitempty"`
	ID                 string       `json:"id"`
	URL                string       `json:"url"`
	Destination        string       `json:"destination"`
	Name               string       `json:"name"`
	Status             CloneStatus  `json:"status"`
	Failure            CloneFailure `json:"failure,omitempty"`
	// Percent and Line are git's own progress, as it printed them: the
	// dialog and the rail draw exactly what git said.
	Percent int    `json:"percent"`
	Line    string `json:"line,omitempty"`
	// Stderr is the tail of what git said when the clone stopped or failed.
	Stderr string `json:"stderr,omitempty"`
	// PartialBytes is what the half-cloned directory holds on disk once the
	// clone has stopped or failed.
	PartialBytes int64     `json:"partial_bytes"`
	ProjectID    string    `json:"project_id,omitempty"`
	StartedAt    time.Time `json:"started_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// CloneIdentity is the Git identity the finished project registers with:
// a reusable profile at an exact revision, or an explicit author.
type CloneIdentity struct {
	ProfileID        string
	ExpectedRevision int64
	Name             string
	Email            string
}

// CloneRequest starts one clone.
type CloneRequest struct {
	GitHubAccount      string
	GitHubRepositoryID int64
	URL                string
	Destination        string
	Name               string
	Identity           CloneIdentity
}

// CloneRegistrar registers the finished clone as a project.
type CloneRegistrar func(ctx context.Context, job CloneJob, identity CloneIdentity) (*Project, error)

// Cloner runs clone jobs. Jobs live in memory for the process's life: a
// restart forgets them, and a half-cloned directory left behind is then what
// inspection reports it as — a destination that is not empty.
type Cloner struct {
	mu       sync.Mutex
	jobs     map[string]*cloneRun
	roots    []string
	register CloneRegistrar
	log      zerolog.Logger
	// git is the executable the jobs run. Tests point it at a script that
	// prints progress and fails or stops on cue.
	git   string
	now   func() time.Time
	newID func() (string, error)
	wg    sync.WaitGroup
	// beforeLaunch and beforeRemove run between a destination's
	// acquisition and its use; tests swap the name under the job there.
	beforeLaunch   func(CloneJob)
	beforeRemove   func(CloneJob)
	afterCreate    func(tmpPath string)
	afterHold      func(tmpPath string)
	afterTombCheck func(tombPath string)
	beforeAcquire  func(CloneRequest)
	beforeLock     func(CloneRequest)
}

// retract takes a directory this start published back off the
// destination when the start is refused after publication, through the
// same tomb protocol a discard uses, so a refused start leaves nothing
// behind and touches nothing it did not create.
func (c *Cloner) retract(a *anchor) {
	if a.leaf == nil {
		a.close()
		return
	}
	run := &cloneRun{job: CloneJob{Destination: filepath.Join(a.base, a.rel)}, anchor: *a}
	if err := c.removePartial(run); err != nil {
		c.log.Warn().Err(err).Str("destination", run.job.Destination).Msg("a refused clone start left its directory")
	}
	a.leaf = run.anchor.leaf
	a.close()
}

type cloneRun struct {
	job      CloneJob
	identity CloneIdentity
	anchor   anchor
	cancel   context.CancelFunc
	// stderr keeps the tail of git's output; the last progress line is
	// reported live and the rest only when the clone stops or fails.
	stderr []string
}

func NewCloner(allowedRoots []string, register CloneRegistrar, log zerolog.Logger) *Cloner {
	git, err := exec.LookPath("git")
	if err != nil {
		git = "git"
	}
	return &Cloner{
		jobs:     map[string]*cloneRun{},
		roots:    allowedRoots,
		register: register,
		log:      log,
		git:      git,
		now:      time.Now,
		newID:    newID,
	}
}

var remoteURL = regexp.MustCompile(`^(https?://|ssh://|git://)[^\s]+$|^[\w.-]+@[\w.-]+:[^\s]+$`)

// ValidateCloneURL admits only a remote repository: a scheme'd URL or an
// scp-style ssh address. A local path or file:// would let a clone read any
// directory the server can, which is not what the field offers.
func ValidateCloneURL(url string) error {
	u := strings.TrimSpace(url)
	if u == "" || strings.HasPrefix(u, "-") || !remoteURL.MatchString(u) {
		return fmt.Errorf("%w: %q", ErrCloneURLInvalid, url)
	}
	// A secret never enters argv or job state: a URL that carries
	// credentials inline is refused outright. Over http(s) any userinfo is
	// a credential; over ssh a bare user name is the address (git@host)
	// and only a password after it is refused.
	if scheme, rest, ok := strings.Cut(u, "://"); ok {
		host := rest
		if at := strings.LastIndex(rest, "@"); at >= 0 && !strings.Contains(rest[:at], "/") {
			userinfo := rest[:at]
			host = rest[at+1:]
			if strings.HasPrefix(scheme, "http") || strings.Contains(userinfo, ":") {
				return fmt.Errorf("%w: credentials do not belong in the URL", ErrCloneURLInvalid)
			}
		}
		if host == "" {
			return fmt.Errorf("%w: %q", ErrCloneURLInvalid, url)
		}
	} else if at := strings.Index(u, "@"); at >= 0 && strings.Contains(u[:at], ":") {
		return fmt.Errorf("%w: credentials do not belong in the URL", ErrCloneURLInvalid)
	}
	return nil
}

// Start begins a clone. The destination must lie inside an allowed root and
// be absent or an empty directory; the URL is the caller's to validate,
// because the boundary between remote and local belongs at the API.
func (c *Cloner) Start(req CloneRequest) (*CloneJob, error) {
	if strings.TrimSpace(req.URL) == "" || strings.HasPrefix(strings.TrimSpace(req.URL), "-") {
		return nil, fmt.Errorf("%w: %q", ErrCloneURLInvalid, req.URL)
	}
	// Acquisition, the duplicate check and registration happen under one
	// lock: two starts for one destination cannot both publish, and the
	// one that loses never touches what the winner holds. The seam before
	// the lock lets a test prove a second start is waiting on it.
	if c.beforeLock != nil {
		c.beforeLock(req)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.beforeAcquire != nil {
		c.beforeAcquire(req)
	}
	dest, a, err := c.resolveDestination(req.Destination)
	if err != nil {
		return nil, err
	}
	// A live job's destination is taken even while it is still empty — git
	// has not written yet. Completed jobs release it. A stopped or failed
	// job still holds its directory for Resume or Discard: a new start over
	// one with content fails below at publishLeaf, and over an empty one
	// retires the old job through an empty-only removal. Retrying a failed
	// clone's own empty destination is how the dialog retries a failure,
	// and it must never unlink the old job's anchor beneath it — nor delete
	// anything that arrived in the directory since the emptiness check.
	for id, run := range c.jobs {
		if run.job.Destination != dest {
			continue
		}
		switch run.job.Status {
		case CloneRunning:
			a.close()
			return nil, fmt.Errorf("%w: %s", ErrCloneInProgress, dest)
		case CloneDone:
			continue
		}
		entries, err := run.anchor.leafEntries()
		if err != nil {
			a.close()
			return nil, err
		}
		if len(entries) != 0 {
			continue // the partial occupies its destination; publishLeaf refuses it
		}
		if err := c.removeEmpty(run); err != nil {
			a.close()
			return nil, err
		}
		run.anchor.close()
		delete(c.jobs, id)
	}
	if err := c.publishLeaf(&a); err != nil {
		a.close()
		return nil, err
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		name = filepath.Base(dest)
	}
	id, err := c.newID()
	if err != nil {
		c.retract(&a)
		return nil, err
	}
	now := c.now()
	run := &cloneRun{
		job: CloneJob{
			ID: id, URL: strings.TrimSpace(req.URL), Destination: dest, Name: name,
			GitHubAccount: req.GitHubAccount, GitHubRepositoryID: req.GitHubRepositoryID,
			Status: CloneRunning, StartedAt: now, UpdatedAt: now,
		},
		identity: req.Identity,
		anchor:   a,
	}
	c.jobs[id] = run
	c.launch(run)
	job := run.job
	return &job, nil
}

// anchor is a clone destination held by descriptor rather than by name:
// the allowed root it lies under, opened component by component so a
// replaced ancestor is refused rather than followed, and the destination's
// path relative to it. Every write and delete goes through the root, which
// refuses any symlink that would leave it.
type anchor struct {
	root *os.Root
	base string
	rel  string
	// leaf is the destination directory itself, held open from the moment
	// it was created or checked: git runs in it and its contents are
	// removed through it, so a name swapped under the job changes nothing
	// about where the bytes go or what is deleted.
	leaf *os.File
}

func (a anchor) close() {
	if a.leaf != nil {
		_ = a.leaf.Close()
	}
	if a.root != nil {
		_ = a.root.Close()
	}
}

// leafEntries lists what the held directory holds, through a root bound
// to the descriptor: a fresh open of the same inode by its kernel-owned
// link, never by name.
func (a anchor) leafEntries() ([]string, error) {
	if a.leaf == nil {
		return nil, fmt.Errorf("%w: no destination held", ErrInvalidDir)
	}
	inside, err := rootAtDescriptor(a.leaf)
	if err != nil {
		return nil, err
	}
	defer func() { _ = inside.Close() }()
	dir, err := inside.Open(".")
	if err != nil {
		return nil, err
	}
	defer func() { _ = dir.Close() }()
	return dir.Readdirnames(-1)
}

// clearLeaf removes everything inside the held directory, through it.
func (a anchor) clearLeaf() error {
	names, err := a.leafEntries()
	if err != nil {
		return err
	}
	inside, err := rootAtDescriptor(a.leaf)
	if err != nil {
		return err
	}
	defer func() { _ = inside.Close() }()
	for _, name := range names {
		if err := inside.RemoveAll(name); err != nil {
			return err
		}
	}
	return nil
}

// resolveDestination confines the leaf by name and opens its root by
// descriptor; publishLeaf then creates and holds the directory itself.
func (c *Cloner) resolveDestination(path string) (string, anchor, error) {
	if !filepath.IsAbs(path) {
		return "", anchor{}, fmt.Errorf("%w: clone destination must be absolute", ErrInvalidDir)
	}
	roots, err := canonicalRoots(c.roots)
	if err != nil {
		return "", anchor{}, err
	}
	clean := filepath.Clean(path)
	parent, err := Canonicalize(filepath.Dir(clean))
	if err != nil {
		return "", anchor{}, fmt.Errorf("%w: the directory above %s does not exist", ErrInvalidDir, clean)
	}
	dest := filepath.Join(parent, filepath.Base(clean))
	base := ""
	for _, root := range roots {
		if UnderRoot(dest, root) && UnderRoot(parent, root) && filepath.Clean(dest) != root {
			base = root
			break
		}
	}
	if base == "" {
		return "", anchor{}, fmt.Errorf("%w: %s", ErrOutsideRoots, dest)
	}
	a, err := c.anchorUnder(base, dest)
	if err != nil {
		return "", anchor{}, err
	}
	// Create the leaf through the root. A leaf that exists must be an empty
	// directory and not a link; the root refuses a link that leaves it, and
	// one that stays inside is refused here, so the clone lands where the
	// name says.
	return dest, a, nil
}

// publishLeaf makes the directory that is held the directory that is
// named, with no gap between the two: the directory is created under a
// random name nobody else is looking for, opened and checked against
// what was created, and then renamed onto the destination in one step —
// which succeeds only onto nothing or an empty directory, and fails onto
// a link, a file, or anything with contents. What git will write into is
// therefore exactly what was created here.
func (c *Cloner) publishLeaf(a *anchor) error {
	id, err := c.newID()
	if err != nil {
		return err
	}
	tmp := filepath.Join(filepath.Dir(a.rel), ".clone-"+id)
	if err := a.root.Mkdir(tmp, 0o750); err != nil {
		return fmt.Errorf("%w: cannot create %s: %w", ErrInvalidDir, filepath.Join(a.base, a.rel), err)
	}
	made, err := a.root.Lstat(tmp)
	if err != nil {
		_ = a.root.Remove(tmp)
		return fmt.Errorf("%w: %w", ErrInvalidDir, err)
	}
	if c.afterCreate != nil {
		c.afterCreate(filepath.Join(a.base, tmp))
	}
	leaf, err := a.root.OpenFile(tmp, os.O_RDONLY|leafOpenFlags, 0)
	if err != nil {
		_ = a.root.Remove(tmp)
		return fmt.Errorf("%w: %w", ErrInvalidDir, err)
	}
	held, err := leaf.Stat()
	if err != nil || !os.SameFile(made, held) {
		_ = leaf.Close()
		_ = a.root.Remove(tmp)
		return fmt.Errorf("%w: %s changed while being created", ErrInvalidDir, filepath.Join(a.base, a.rel))
	}
	if c.afterHold != nil {
		c.afterHold(filepath.Join(a.base, tmp))
	}
	// The rename never replaces: an existing empty directory is taken off
	// the name first with a remove that fails on anything with contents,
	// and the rename is retried once. Anything placed at the name between
	// the two makes the rename fail, and the clone is refused.
	err = a.root.Rename(tmp, a.rel)
	if errors.Is(err, os.ErrExist) {
		switch rerr := a.root.Remove(a.rel); {
		case rerr == nil:
			err = a.root.Rename(tmp, a.rel)
		case errors.Is(rerr, syscall.ENOTEMPTY) || errors.Is(rerr, syscall.EEXIST):
			err = fmt.Errorf("%w: %s", ErrCloneDestNotEmpty, filepath.Join(a.base, a.rel))
		default:
			err = fmt.Errorf("%w: %s is not an empty directory: %w", ErrInvalidDir, filepath.Join(a.base, a.rel), rerr)
		}
	}
	if err != nil {
		_ = leaf.Close()
		_ = a.root.Remove(tmp)
		if errors.Is(err, ErrCloneDestNotEmpty) || errors.Is(err, ErrInvalidDir) {
			return err
		}
		return fmt.Errorf("%w: %s is not an empty directory: %w", ErrInvalidDir, filepath.Join(a.base, a.rel), err)
	}
	// What now stands under the name must be what is held. A directory
	// swapped onto the random name between the hold and the rename would
	// have been published in its place; it is taken back off the name and
	// the start fails, so the job is never registered against it.
	published, err := a.root.Lstat(a.rel)
	if err != nil || !os.SameFile(published, held) {
		_ = leaf.Close()
		if rerr := a.root.Rename(a.rel, tmp); rerr != nil {
			c.log.Error().Err(rerr).Str("destination", filepath.Join(a.base, a.rel)).Msg("could not take a substituted directory back off the destination")
		}
		return fmt.Errorf("%w: %s changed while being published", ErrInvalidDir, filepath.Join(a.base, a.rel))
	}
	if a.leaf != nil {
		_ = a.leaf.Close()
	}
	a.leaf = leaf
	return nil
}

// anchorUnder opens the allowed root by descriptor and names dest inside it.
func (c *Cloner) anchorUnder(base, dest string) (anchor, error) {
	rel, err := filepath.Rel(base, dest)
	if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
		return anchor{}, fmt.Errorf("%w: %s", ErrOutsideRoots, dest)
	}
	root, err := openProjectRoot(base, nil)
	if err != nil {
		return anchor{}, fmt.Errorf("%w: %s: %w", ErrInvalidDir, base, err)
	}
	return anchor{root: root, base: base, rel: rel}, nil
}

// launch runs git under the job's own context. Callers hold c.mu.
func (c *Cloner) launch(run *cloneRun) {
	ctx, cancel := context.WithCancel(context.Background())
	run.cancel = cancel
	run.stderr = nil
	run.job.Status = CloneRunning
	run.job.Failure = ""
	run.job.Percent = 0
	run.job.Line = ""
	run.job.Stderr = ""
	run.job.PartialBytes = 0
	run.job.UpdatedAt = c.now()
	c.wg.Add(1)
	go c.execute(ctx, run)
}

func (c *Cloner) execute(ctx context.Context, run *cloneRun) {
	defer c.wg.Done()
	// #nosec G204 -- the URL is validated at the API and passed after "--";
	// the destination was canonicalized and confined to an allowed root.
	// git clones into its working directory, and that directory is the
	// leaf held since it was created — not the path, which a name swapped
	// after validation could redirect. The hook is the tests' seam for the
	// swap itself.
	if c.beforeLaunch != nil {
		c.beforeLaunch(run.job)
	}
	if run.anchor.leaf == nil {
		c.finish(ctx, run, fmt.Errorf("%w: %s: no destination held", ErrInvalidDir, run.job.Destination))
		return
	}
	cmd, err := newCloneCommand(ctx, c.git, run.job.URL, run.job.Destination, run.anchor.leaf)
	if err != nil {
		c.finish(ctx, run, err)
		return
	}
	// git spawns helpers (ssh, git-remote-https, pack readers) that inherit
	// its pipes; a cancel kills the whole group, or a helper would hold the
	// progress pipe open and the job would never report stopped.
	killProcessGroup(cmd)
	cmd.WaitDelay = 2 * time.Second
	// A clone never asks a question nobody is there to answer: a refused
	// key or a private repository fails instead of hanging on a prompt.
	cmd.Env = append(os.Environ(),
		"GIT_TERMINAL_PROMPT=0",
		"GIT_SSH_COMMAND=ssh -o BatchMode=yes",
		"GIT_ASKPASS=",
	)
	// git's stderr goes through a writer of our own rather than a pipe we
	// read ourselves: Wait closes a pipe the moment git exits and drops
	// whatever was not yet read, so the last lines — the ones that say why
	// a clone failed — could be lost on a slow machine. A writer is drained
	// by Wait before it returns, and WaitDelay bounds a helper that holds
	// the stream open after git is gone.
	progress := &progressWriter{cloner: c, run: run}
	cmd.Stderr = progress
	if err := cmd.Start(); err != nil {
		c.finish(ctx, run, err)
		return
	}
	err = cmd.Wait()
	// Wait has stopped the copy into the writer, so the fragment after
	// the last terminator — git's final diagnostic often has none — is
	// recorded here without racing a live write.
	progress.flush()
	if errors.Is(err, exec.ErrWaitDelay) {
		// git itself exited cleanly; a helper it left behind (ssh, a pack
		// reader) held the stream open past the bound. The clone's verdict
		// is git's exit status, and every line git wrote has been drained.
		err = nil
	}
	c.finish(ctx, run, err)
}

// progressWriter feeds git's stderr, as it arrives, to the same line
// parser a reader would: git rewrites its progress line in place with
// carriage returns, and a line is complete at either terminator.
type progressWriter struct {
	cloner *Cloner
	run    *cloneRun
	line   strings.Builder
}

func (w *progressWriter) Write(p []byte) (int, error) {
	for _, b := range p {
		if b == '\r' || b == '\n' {
			w.cloner.recordLine(w.run, w.line.String())
			w.line.Reset()
			continue
		}
		w.line.WriteByte(b)
	}
	return len(p), nil
}

func (w *progressWriter) flush() {
	if w.line.Len() == 0 {
		return
	}
	w.cloner.recordLine(w.run, w.line.String())
	w.line.Reset()
}

var (
	receiving = regexp.MustCompile(`^(Receiving objects|Resolving deltas|Compressing objects|Counting objects|Updating files):\s+(\d+)%`)
)

// recordLine takes one line of git's stderr. The percent and the line are
// reported as git printed them; the tail is kept for the moment the clone
// stops.
func (c *Cloner) recordLine(run *cloneRun, raw string) {
	text := strings.TrimSpace(raw)
	if text == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if m := receiving.FindStringSubmatch(text); m != nil {
		if pct, err := strconv.Atoi(m[2]); err == nil {
			run.job.Percent = pct
		}
		run.job.Line = text
	} else {
		run.stderr = append(run.stderr, text)
		if len(run.stderr) > 40 {
			run.stderr = run.stderr[len(run.stderr)-40:]
		}
	}
	run.job.UpdatedAt = c.now()
}

func (c *Cloner) finish(ctx context.Context, run *cloneRun, waitErr error) {
	c.mu.Lock()
	tail := strings.Join(run.stderr, "\n")
	job := run.job
	identity := run.identity
	c.mu.Unlock()

	switch {
	case waitErr == nil:
		project, err := c.register(context.Background(), job, identity)
		c.mu.Lock()
		defer c.mu.Unlock()
		if err != nil {
			// The bytes arrived and the registration did not: the clone is
			// on disk and whole, which is a failure the user can act on
			// with the directory itself, so it is stated, not hidden.
			run.job.Status = CloneFailed
			run.job.Failure = CloneRefused
			run.job.Stderr = err.Error()
			run.job.PartialBytes = run.anchor.bytes()
			run.job.UpdatedAt = c.now()
			c.log.Warn().Err(err).Str("destination", run.job.Destination).Msg("clone finished but the project did not register")
			return
		}
		run.job.Status = CloneDone
		run.job.Percent = 100
		run.job.ProjectID = project.ID
		run.job.UpdatedAt = c.now()
	case ctx.Err() != nil:
		// Cancelled: the partial directory stays, as a state the user
		// can resume or discard, never deleted silently.
		c.mu.Lock()
		defer c.mu.Unlock()
		run.job.Status = CloneStopped
		run.job.Stderr = tail
		run.job.PartialBytes = run.anchor.bytes()
		run.job.UpdatedAt = c.now()
	default:
		c.mu.Lock()
		defer c.mu.Unlock()
		run.job.Status = CloneFailed
		run.job.Failure = classifyCloneFailure(tail)
		run.job.Stderr = tail
		run.job.PartialBytes = run.anchor.bytes()
		run.job.UpdatedAt = c.now()
	}
}

// classifyCloneFailure reads git's own words. Anything it cannot name is a
// refusal — the honest answer when the remote said no and did not say why.
func classifyCloneFailure(stderr string) CloneFailure {
	s := strings.ToLower(stderr)
	switch {
	case strings.Contains(s, "permission denied (publickey)"),
		strings.Contains(s, "authentication failed"),
		strings.Contains(s, "could not read username"),
		strings.Contains(s, "could not read password"),
		strings.Contains(s, "host key verification failed"):
		return CloneAuthRefused
	case strings.Contains(s, "already exists and is not an empty directory"):
		return CloneDestNotEmpty
	case strings.Contains(s, "unexpected disconnect"),
		strings.Contains(s, "early eof"),
		strings.Contains(s, "could not resolve host"),
		strings.Contains(s, "connection reset"),
		strings.Contains(s, "connection timed out"),
		strings.Contains(s, "operation timed out"),
		strings.Contains(s, "the remote end hung up unexpectedly"),
		strings.Contains(s, "failed to connect"):
		return CloneNetworkDied
	default:
		return CloneRefused
	}
}

// Cancel stops a running clone and keeps what is on disk.
func (c *Cloner) Cancel(id string) (*CloneJob, error) {
	c.mu.Lock()
	run, ok := c.jobs[id]
	if !ok {
		c.mu.Unlock()
		return nil, ErrCloneNotFound
	}
	if run.job.Status != CloneRunning {
		job := run.job
		c.mu.Unlock()
		return &job, nil
	}
	cancel := run.cancel
	c.mu.Unlock()
	cancel()
	c.awaitStatus(id, CloneRunning)
	return c.Get(id)
}

// Resume starts the clone again. git cannot pick a clone up part-way, so
// resuming clears the partial directory first — the dialog says as much:
// resuming starts the fetch again.
func (c *Cloner) Resume(id string) (*CloneJob, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	run, ok := c.jobs[id]
	if !ok {
		return nil, ErrCloneNotFound
	}
	if run.job.Status != CloneStopped && run.job.Status != CloneFailed {
		return nil, fmt.Errorf("%w: %s", ErrCloneNotStopped, run.job.Status)
	}
	if err := c.removePartial(run); err != nil {
		return nil, err
	}
	// The leaf is created, held and published again exactly as the first
	// was, so the resumed clone is anchored the same way.
	if err := c.publishLeaf(&run.anchor); err != nil {
		return nil, err
	}
	c.launch(run)
	job := run.job
	return &job, nil
}

// Discard removes a stopped or failed clone's directory and forgets the job.
func (c *Cloner) Discard(id string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	run, ok := c.jobs[id]
	if !ok {
		return ErrCloneNotFound
	}
	if run.job.Status == CloneRunning {
		return fmt.Errorf("%w: cancel it first", ErrCloneNotStopped)
	}
	if run.job.Status != CloneDone {
		if err := c.removePartial(run); err != nil {
			return err
		}
	}
	run.anchor.close()
	delete(c.jobs, id)
	return nil
}

// removePartial deletes a clone's directory, and only that, through the
// anchor the job was started with: a delete is the one operation this
// package must never get wrong, so it never goes by name. The root refuses
// a link that leaves it, and the leaf itself is refused when it has become
// a link.
func (c *Cloner) removePartial(run *cloneRun) error {
	a := run.anchor
	if a.root == nil || a.leaf == nil {
		return fmt.Errorf("%w: %s", ErrOutsideRoots, run.job.Destination)
	}
	if c.beforeRemove != nil {
		c.beforeRemove(run.job)
	}
	// The contents go through the held directory, whatever the name says
	// now. The name itself is taken off the destination in one step — a
	// rename onto a random tomb name nobody else is looking for — and only
	// then is what arrived there compared with what is held: the clone's
	// own directory is removed, anything else is put back where it was and
	// the discard fails. Nothing is ever deleted at the destination's name.
	if err := a.clearLeaf(); err != nil {
		return fmt.Errorf("%w: cannot clear %s: %w", ErrInvalidDir, run.job.Destination, err)
	}
	id, err := c.newID()
	if err != nil {
		return err
	}
	tomb := filepath.Join(filepath.Dir(a.rel), ".clone-tomb-"+id)
	switch err := a.root.Rename(a.rel, tomb); {
	case errors.Is(err, os.ErrNotExist):
		// Nothing at the name: the directory was already taken away.
		_ = a.leaf.Close()
		run.anchor.leaf = nil
		return nil
	case err != nil:
		return fmt.Errorf("%w: cannot remove %s: %w", ErrInvalidDir, run.job.Destination, err)
	}
	arrived, err := a.root.Lstat(tomb)
	held, herr := a.leaf.Stat()
	if err != nil || herr != nil || !os.SameFile(arrived, held) {
		// Someone else's directory was under the clone's name. It goes back.
		if rerr := a.root.Rename(tomb, a.rel); rerr != nil {
			c.log.Error().Err(rerr).Str("tomb", tomb).Msg("could not restore a substituted directory")
		}
		return fmt.Errorf("%w: %s no longer names the clone's directory", ErrInvalidDir, run.job.Destination)
	}
	if c.afterTombCheck != nil {
		c.afterTombCheck(filepath.Join(a.base, tomb))
	}
	// The one operation left by name is an rmdir of a random name that
	// exists for this call only, and rmdir refuses anything with contents
	// or that is not a directory: the most an actor swapping onto it can
	// lose is an empty directory they themselves placed under a name only
	// this call uses.
	if err := removeDir(a.root, tomb); err != nil {
		return fmt.Errorf("%w: cannot remove %s: %w", ErrInvalidDir, run.job.Destination, err)
	}
	_ = a.leaf.Close()
	run.anchor.leaf = nil
	return nil
}

// removeEmpty retires a stopped or failed job whose held directory is
// empty, for a new start over its destination. Unlike a discard, a retry
// is authorized to replace an empty directory only, so nothing is
// cleared: the name moves to a tomb and rmdir refuses anything that
// arrived since the caller's emptiness check, in which case the name is
// put back with its contents and the old job keeps its claim.
func (c *Cloner) removeEmpty(run *cloneRun) error {
	a := run.anchor
	if a.root == nil || a.leaf == nil {
		return fmt.Errorf("%w: %s", ErrOutsideRoots, run.job.Destination)
	}
	if c.beforeRemove != nil {
		c.beforeRemove(run.job)
	}
	id, err := c.newID()
	if err != nil {
		return err
	}
	tomb := filepath.Join(filepath.Dir(a.rel), ".clone-tomb-"+id)
	switch err := a.root.Rename(a.rel, tomb); {
	case errors.Is(err, os.ErrNotExist):
		// Nothing at the name: the directory was already taken away.
		_ = a.leaf.Close()
		run.anchor.leaf = nil
		return nil
	case err != nil:
		return fmt.Errorf("%w: cannot remove %s: %w", ErrInvalidDir, run.job.Destination, err)
	}
	arrived, err := a.root.Lstat(tomb)
	held, herr := a.leaf.Stat()
	if err != nil || herr != nil || !os.SameFile(arrived, held) {
		// Someone else's directory was under the clone's name. It goes back.
		if rerr := a.root.Rename(tomb, a.rel); rerr != nil {
			c.log.Error().Err(rerr).Str("tomb", tomb).Msg("could not restore a substituted directory")
		}
		return fmt.Errorf("%w: %s no longer names the clone's directory", ErrInvalidDir, run.job.Destination)
	}
	if c.afterTombCheck != nil {
		c.afterTombCheck(filepath.Join(a.base, tomb))
	}
	// rmdir refuses a directory with contents and anything that is not a
	// directory: what arrived since the emptiness check goes back with the
	// name, and a file or link swapped onto the tomb is never removed.
	switch err := removeDir(a.root, tomb); {
	case err == nil:
	case errors.Is(err, syscall.ENOTEMPTY) || errors.Is(err, syscall.EEXIST):
		if rerr := a.root.Rename(tomb, a.rel); rerr != nil {
			c.log.Error().Err(rerr).Str("tomb", tomb).Msg("could not restore a directory that would not remove")
			return fmt.Errorf("%w: cannot restore %s: %w", ErrInvalidDir, run.job.Destination, rerr)
		}
		return fmt.Errorf("%w: %s", ErrCloneDestNotEmpty, run.job.Destination)
	default:
		return fmt.Errorf("%w: %s changed while being retired", ErrInvalidDir, run.job.Destination)
	}
	_ = a.leaf.Close()
	run.anchor.leaf = nil
	return nil
}

func (c *Cloner) Get(id string) (*CloneJob, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	run, ok := c.jobs[id]
	if !ok {
		return nil, ErrCloneNotFound
	}
	job := run.job
	return &job, nil
}

// List reports every job the process knows, oldest first.
func (c *Cloner) List() []CloneJob {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]CloneJob, 0, len(c.jobs))
	for _, run := range c.jobs {
		out = append(out, run.job)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].StartedAt.Before(out[j].StartedAt) })
	return out
}

// Shutdown stops every running clone and waits for git to exit. What is on
// disk stays: a restart will find it where inspection reports it.
func (c *Cloner) Shutdown() {
	c.mu.Lock()
	for _, run := range c.jobs {
		if run.job.Status == CloneRunning && run.cancel != nil {
			run.cancel()
		}
	}
	c.mu.Unlock()
	c.wg.Wait()
}

// awaitStatus blocks briefly until the job leaves status, so a cancel
// answers with the stopped job rather than a running one.
func (c *Cloner) awaitStatus(id string, leaving CloneStatus) {
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		c.mu.Lock()
		run, ok := c.jobs[id]
		done := !ok || run.job.Status != leaving
		c.mu.Unlock()
		if done {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// bytes is what the destination holds, read through the root.
func (a anchor) bytes() int64 {
	if a.leaf == nil {
		return 0
	}
	inside, err := rootAtDescriptor(a.leaf)
	if err != nil {
		return 0
	}
	defer func() { _ = inside.Close() }()
	var total int64
	_ = fs.WalkDir(inside.FS(), ".", func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			// A subtree that cannot be read is left out of the count and
			// the walk moves on; the number is a readout, not an audit.
			return fs.SkipDir
		}
		if d.IsDir() {
			return nil
		}
		if info, err := d.Info(); err == nil {
			total += info.Size()
		}
		return nil
	})
	return total
}
