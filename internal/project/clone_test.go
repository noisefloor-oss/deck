package project

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
)

// fakeGit stands in for git clone: it prints progress the way git does
// (carriage-return rewrites), creates the destination with a partial file,
// then obeys a marker beside the destination — REFUSE fails before a byte
// is written, the way an unreachable or missing remote leaves the
// destination empty, AUTH fails like a refused key, STALL waits to be
// cancelled, and otherwise it finishes the clone.
func fakeGit(t *testing.T) string {
	t.Helper()
	script := filepath.Join(t.TempDir(), "git")
	body := `#!/bin/sh
# args: clone --progress -- URL .  (git clones into its working directory,
# which the runner holds open by descriptor)
dest="$(pwd -P)"
if [ -f "$(dirname "$dest")/REFUSE" ]; then
  printf 'fatal: repository %s not found\n' "$4" >&2
  exit 128
fi
mkdir -p "$dest/.git"
# What git would see: the environment it was handed, and a helper child
# that inherits its pipes the way ssh or git-remote-https would.
env > "$dest/.git/env.txt"
sleep 60 &
echo $! > "$dest/.git/helper.pid"
printf 'Cloning into %s...\n' "$dest" >&2
printf 'remote: Enumerating objects: 3, done.\n' >&2
printf 'Receiving objects:  34%% (1/3), 4.20 MiB | 1.20 MiB/s\r' >&2
printf 'partial' > "$dest/.git/pack.tmp"
if [ -f "$(dirname "$dest")/AUTH" ]; then
  # The last diagnostic carries no terminator, as git's often does not.
  printf 'git@github.com: Permission denied (publickey).\nfatal: Could not read from remote repository.' >&2
  exit 128
fi
if [ -f "$(dirname "$dest")/STALL" ]; then
  sleep 30
  exit 1
fi
printf 'Receiving objects: 100%% (3/3), 12.0 MiB | 1.20 MiB/s, done.\r\n' >&2
printf 'Resolving deltas: 100%% (1/1), done.\n' >&2
rm -f "$dest/.git/pack.tmp"
printf 'clone' > "$dest/README.md"
exit 0
`
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return script
}

func testCloner(t *testing.T) (*Cloner, string, *[]CloneJob) {
	t.Helper()
	root := mkdir(t, filepath.Join(t.TempDir(), "root"))
	registered := &[]CloneJob{}
	c := NewCloner([]string{root}, func(_ context.Context, job CloneJob, _ CloneIdentity) (*Project, error) {
		*registered = append(*registered, job)
		return &Project{ID: "proj-" + job.Name, Name: job.Name, CanonicalPath: job.Destination}, nil
	}, zerolog.Nop())
	c.git = fakeGit(t)
	t.Cleanup(c.Shutdown)
	return c, root, registered
}

func waitFor(t *testing.T, c *Cloner, id string, status CloneStatus) *CloneJob {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		job, err := c.Get(id)
		if err != nil {
			t.Fatal(err)
		}
		if job.Status == status {
			return job
		}
		time.Sleep(20 * time.Millisecond)
	}
	job, _ := c.Get(id)
	t.Fatalf("job never reached %s: %+v", status, job)
	return nil
}

func TestCloneFinishesAndRegistersTheProject(t *testing.T) {
	c, root, registered := testCloner(t)
	job, err := c.Start(CloneRequest{URL: "https://github.com/fable/deck.git", Destination: filepath.Join(root, "deck")})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if job.Status != CloneRunning || job.Name != "deck" {
		t.Fatalf("started: %+v", job)
	}
	done := waitFor(t, c, job.ID, CloneDone)
	if done.Percent != 100 || done.ProjectID != "proj-deck" || done.Failure != "" {
		t.Fatalf("done: %+v", done)
	}
	if len(*registered) != 1 || (*registered)[0].Destination != filepath.Join(root, "deck") {
		t.Fatalf("registered: %+v", *registered)
	}
	if _, err := os.Stat(filepath.Join(root, "deck", "README.md")); err != nil {
		t.Fatalf("the clone is on disk: %v", err)
	}
}

func TestCloneReportsGitsProgressAsPrinted(t *testing.T) {
	c, root, _ := testCloner(t)
	if err := os.WriteFile(filepath.Join(root, "STALL"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	job, err := c.Start(CloneRequest{URL: "https://github.com/fable/deck.git", Destination: filepath.Join(root, "deck")})
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		got, _ := c.Get(job.ID)
		if got.Percent == 34 && strings.HasPrefix(got.Line, "Receiving objects:  34%") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("progress never arrived: %+v", got)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestCancelKeepsThePartialCloneAndResumeStartsAgain(t *testing.T) {
	c, root, registered := testCloner(t)
	stall := filepath.Join(root, "STALL")
	if err := os.WriteFile(stall, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	job, err := c.Start(CloneRequest{URL: "git@github.com:fable/deck.git", Destination: filepath.Join(root, "deck")})
	if err != nil {
		t.Fatal(err)
	}
	waitForPartial(t, filepath.Join(root, "deck", ".git", "pack.tmp"))
	helper := readHelperPID(t, filepath.Join(root, "deck", ".git", "helper.pid"))
	if alive, err := cloneHelperRunning(helper); err != nil || !alive {
		t.Fatalf("git helper was not alive before cancellation: %v", err)
	}
	stopped, err := c.Cancel(job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stopped.Status != CloneStopped || stopped.PartialBytes == 0 {
		t.Fatalf("cancel keeps what is on disk and says how much: %+v", stopped)
	}
	// Cancellation must stop the helper that inherited git's pipes.
	if processAlive(t, helper) {
		t.Fatalf("git's helper (pid %d) survived the cancel", helper)
	}
	if _, err := os.Stat(filepath.Join(root, "deck", ".git", "pack.tmp")); err != nil {
		t.Fatalf("nothing was deleted: %v", err)
	}
	if _, err := c.Start(CloneRequest{URL: "git@github.com:fable/deck.git", Destination: filepath.Join(root, "deck")}); !errors.Is(err, ErrCloneDestNotEmpty) {
		t.Fatalf("a partial clone occupies its destination: %v", err)
	}
	// Resuming starts the fetch again: the partial directory is cleared
	// and the clone lands whole.
	if err := os.Remove(stall); err != nil {
		t.Fatal(err)
	}
	resumed, err := c.Resume(job.ID)
	if err != nil || resumed.Status != CloneRunning || resumed.ID != job.ID {
		t.Fatalf("resume: %+v %v", resumed, err)
	}
	done := waitFor(t, c, job.ID, CloneDone)
	if done.ProjectID == "" || len(*registered) != 1 {
		t.Fatalf("resumed clone registers once: %+v %d", done, len(*registered))
	}
	if _, err := os.Stat(filepath.Join(root, "deck", ".git", "pack.tmp")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the partial file is gone after a whole clone: %v", err)
	}
}

func TestARefusedKeyIsNamedAndDiscardRemovesThePartial(t *testing.T) {
	c, root, registered := testCloner(t)
	if err := os.WriteFile(filepath.Join(root, "AUTH"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	job, err := c.Start(CloneRequest{URL: "git@github.com:fable/private.git", Destination: filepath.Join(root, "private")})
	if err != nil {
		t.Fatal(err)
	}
	failed := waitFor(t, c, job.ID, CloneFailed)
	if failed.Failure != CloneAuthRefused || !strings.Contains(failed.Stderr, "Permission denied (publickey)") || !strings.HasSuffix(failed.Stderr, "fatal: Could not read from remote repository.") {
		t.Fatalf("failure: %+v", failed)
	}
	if len(*registered) != 0 {
		t.Fatalf("a failed clone registers nothing")
	}
	if err := c.Discard(job.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "private")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("discard removes the partial directory: %v", err)
	}
	if _, err := c.Get(job.ID); !errors.Is(err, ErrCloneNotFound) {
		t.Fatalf("discard forgets the job: %v", err)
	}
}

func TestRetryingAFailedCloneAtItsEmptyDestinationRetiresTheOldJob(t *testing.T) {
	c, root, registered := testCloner(t)
	refuse := filepath.Join(root, "REFUSE")
	if err := os.WriteFile(refuse, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(root, "private")
	first, err := c.Start(CloneRequest{URL: "git@github.com:fable/private.git", Destination: dest})
	if err != nil {
		t.Fatal(err)
	}
	failed := waitFor(t, c, first.ID, CloneFailed)
	if failed.Failure != CloneRefused {
		t.Fatalf("an early failure is a refusal: %+v", failed)
	}
	entries, err := os.ReadDir(dest)
	if err != nil || len(entries) != 0 {
		t.Fatalf("an early failure leaves the destination empty: %v %d", err, len(entries))
	}
	// The retry the dialog offers: a new clone at the failed job's own
	// empty destination. The old job is retired exactly as a discard would
	// retire it, never orphaned with its anchor unlinked beneath it.
	if err := os.Remove(refuse); err != nil {
		t.Fatal(err)
	}
	retry, err := c.Start(CloneRequest{URL: "git@github.com:fable/private.git", Destination: dest})
	if err != nil {
		t.Fatalf("retry at the failed clone's own empty destination: %v", err)
	}
	if retry.ID == first.ID {
		t.Fatalf("a retry is a new job, not the failed one: %+v", retry)
	}
	if _, err := c.Get(first.ID); !errors.Is(err, ErrCloneNotFound) {
		t.Fatalf("the retired job is forgotten: %v", err)
	}
	done := waitFor(t, c, retry.ID, CloneDone)
	if done.ProjectID == "" || len(*registered) != 1 {
		t.Fatalf("the retried clone registers once: %+v %d", done, len(*registered))
	}
	if _, err := os.Stat(filepath.Join(dest, "README.md")); err != nil {
		t.Fatalf("the retried clone is on disk: %v", err)
	}
}

func TestARetryNeverDeletesWhatLandedInTheEmptyDestination(t *testing.T) {
	c, root, _ := testCloner(t)
	refuse := filepath.Join(root, "REFUSE")
	if err := os.WriteFile(refuse, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(root, "private")
	first, err := c.Start(CloneRequest{URL: "git@github.com:fable/private.git", Destination: dest})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, c, first.ID, CloneFailed)
	// Work lands in the failed clone's empty destination between the
	// retry's emptiness check and the retired directory's removal. A retry
	// replaces an empty directory only: the work stays, the old job keeps
	// its claim, and the start is refused the way any occupied
	// destination is.
	c.beforeRemove = func(job CloneJob) {
		if job.ID == first.ID {
			if err := os.WriteFile(filepath.Join(dest, "operator-work.txt"), []byte("keep"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, err := c.Start(CloneRequest{URL: "git@github.com:fable/private.git", Destination: dest}); !errors.Is(err, ErrCloneDestNotEmpty) {
		t.Fatalf("a destination that gained work refuses the retry: %v", err)
	}
	c.beforeRemove = nil
	if got, err := os.ReadFile(filepath.Join(dest, "operator-work.txt")); err != nil || string(got) != "keep" {
		t.Fatalf("the work is untouched: %v %q", err, got)
	}
	if _, err := c.Get(first.ID); err != nil {
		t.Fatalf("the old job keeps its claim: %v", err)
	}
	if err := c.Discard(first.ID); err != nil {
		t.Fatalf("the old job is still discardable: %v", err)
	}
	if _, err := os.Stat(dest); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("an explicit discard removes what a retry would not: %v", err)
	}
}

func TestARetryNeverDeletesAFileSwappedOntoTheTomb(t *testing.T) {
	c, root, _ := testCloner(t)
	refuse := filepath.Join(root, "REFUSE")
	if err := os.WriteFile(refuse, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(root, "private")
	first, err := c.Start(CloneRequest{URL: "git@github.com:fable/private.git", Destination: dest})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, c, first.ID, CloneFailed)
	// The retired directory is swapped off its tomb name and a file takes
	// the name. The removal is a directory-only rmdir: the file is never
	// deleted, the start fails, and the old job keeps its claim.
	tombed := ""
	var swapErr error
	c.afterTombCheck = func(tomb string) {
		tombed = tomb
		if err := os.Rename(tomb, tomb+"-moved"); err != nil {
			swapErr = err
			return
		}
		swapErr = os.WriteFile(tomb, []byte("keep"), 0o644)
	}
	if _, err := c.Start(CloneRequest{URL: "git@github.com:fable/private.git", Destination: dest}); !errors.Is(err, ErrInvalidDir) {
		t.Fatalf("a file swapped onto the tomb refuses the retry: %v", err)
	}
	c.afterTombCheck = nil
	if swapErr != nil {
		t.Fatal(swapErr)
	}
	if got, err := os.ReadFile(tombed); err != nil || string(got) != "keep" {
		t.Fatalf("the swapped file is untouched: %v %q", err, got)
	}
	if entries, err := os.ReadDir(tombed + "-moved"); err != nil || len(entries) != 0 {
		t.Fatalf("the retired directory stands where the swap moved it, empty: %v %d", err, len(entries))
	}
	if _, err := c.Get(first.ID); err != nil {
		t.Fatalf("the old job keeps its claim: %v", err)
	}
}

func TestCloneRefusesADestinationOutsideTheRootsOrOccupied(t *testing.T) {
	c, root, _ := testCloner(t)
	if _, err := c.Start(CloneRequest{URL: "https://x/y", Destination: filepath.Join(t.TempDir(), "elsewhere")}); !errors.Is(err, ErrOutsideRoots) {
		t.Fatalf("outside: %v", err)
	}
	mkdir(t, filepath.Join(root, "taken", "child"))
	if _, err := c.Start(CloneRequest{URL: "https://x/y", Destination: filepath.Join(root, "taken")}); !errors.Is(err, ErrCloneDestNotEmpty) {
		t.Fatalf("occupied: %v", err)
	}
	if _, err := c.Start(CloneRequest{URL: "https://x/y", Destination: filepath.Join(root, "missing", "deep")}); !errors.Is(err, ErrInvalidDir) {
		t.Fatalf("missing parent: %v", err)
	}
	if _, err := c.Start(CloneRequest{URL: "-oProxyCommand=true", Destination: filepath.Join(root, "x")}); !errors.Is(err, ErrCloneURLInvalid) {
		t.Fatalf("an option is not a URL: %v", err)
	}
}

// Two starts for one absent destination: the second waits on the lock,
// then finds the first running and is refused without ever publishing —
// the first job's directory is untouched.
func TestConcurrentStartsForOneDestinationPublishOnce(t *testing.T) {
	c, root, _ := testCloner(t)
	if err := os.WriteFile(filepath.Join(root, "STALL"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(root, "deck")
	second := make(chan error, 1)
	// The handshake: from inside the first start's acquisition, under the
	// lock and before anything is published, launch the second start and
	// wait until it has reached the lock. Only then does the first go on.
	// Without the serialization the second would not wait, both would
	// acquire, and the second would not be the one refused.
	atLock := make(chan struct{})
	var once sync.Once
	c.beforeLock = func(req CloneRequest) {
		if req.Name == "second" {
			close(atLock)
		}
	}
	c.beforeAcquire = func(req CloneRequest) {
		if req.Name != "" {
			return
		}
		once.Do(func() {
			go func() {
				_, err := c.Start(CloneRequest{URL: "https://github.com/fable/deck.git", Destination: dest, Name: "second"})
				second <- err
			}()
			<-atLock
			select {
			case err := <-second:
				t.Errorf("the second start ran while the first held the lock: %v", err)
			case <-time.After(100 * time.Millisecond):
			}
		})
	}
	first, err := c.Start(CloneRequest{URL: "https://github.com/fable/deck.git", Destination: dest})
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	if err := <-second; !errors.Is(err, ErrCloneInProgress) {
		t.Fatalf("second: %v", err)
	}
	waitForPartial(t, filepath.Join(dest, ".git", "pack.tmp"))
	got, _ := c.Get(first.ID)
	if got.Status != CloneRunning {
		t.Fatalf("first job: %+v", got)
	}
	if len(c.List()) != 1 {
		t.Fatalf("one job")
	}
	if tombs, _ := filepath.Glob(filepath.Join(root, ".clone-*")); len(tombs) != 0 {
		t.Fatalf("a refused start left names behind: %v", tombs)
	}
}

func TestValidateCloneURLRefusesInlineCredentials(t *testing.T) {
	for _, bad := range []string{"https://user:token@github.com/x/y.git", "https://token@github.com/x/y.git", "ssh://git:pw@github.com/x/y", "git:pw@github.com:x/y.git", "https://@github.com/x/y"} {
		if err := ValidateCloneURL(bad); !errors.Is(err, ErrCloneURLInvalid) {
			t.Errorf("%q should be refused: %v", bad, err)
		}
	}
	for _, ok := range []string{"ssh://git@github.com/x/y", "git@github.com:x/y.git"} {
		if err := ValidateCloneURL(ok); err != nil {
			t.Errorf("%q is an address, not a credential: %v", ok, err)
		}
	}
}

func TestValidateCloneURLAdmitsOnlyRemotes(t *testing.T) {
	for _, ok := range []string{"https://github.com/fable/deck.git", "ssh://git@github.com/fable/deck", "git@github.com:fable/deck.git", "git://example.org/repo"} {
		if err := ValidateCloneURL(ok); err != nil {
			t.Errorf("%q should be accepted: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "/workspace/deck", "file:///workspace/deck", "../deck", "-oProxyCommand=x", "fable/deck", "https://a b"} {
		if err := ValidateCloneURL(bad); !errors.Is(err, ErrCloneURLInvalid) {
			t.Errorf("%q should be refused: %v", bad, err)
		}
	}
}

func TestClassifyCloneFailure(t *testing.T) {
	cases := map[string]CloneFailure{
		"fatal: Authentication failed for 'https://github.com/x/y/'":                        CloneAuthRefused,
		"remote: Repository not found.\nfatal: repository not found":                        CloneRefused,
		"fatal: destination path 'x' already exists and is not an empty directory.":         CloneDestNotEmpty,
		"fetch-pack: unexpected disconnect while reading sideband packet\nfatal: early EOF": CloneNetworkDied,
		"fatal: unable to access: Could not resolve host: github.com":                       CloneNetworkDied,
	}
	for stderr, want := range cases {
		if got := classifyCloneFailure(stderr); got != want {
			t.Errorf("%q → %s, want %s", stderr, got, want)
		}
	}
}

// A hostile in-root actor renames the destination away and puts a link to
// another empty in-root directory in its place, between the check and the
// clone. The bytes must still land in the directory that was checked.
func TestSwappedDestinationNameCannotRedirectTheClone(t *testing.T) {
	c, root, _ := testCloner(t)
	decoy := mkdir(t, filepath.Join(root, "decoy"))
	dest := filepath.Join(root, "deck")
	c.beforeLaunch = func(job CloneJob) {
		if err := os.Rename(dest, filepath.Join(root, "moved-away")); err != nil {
			t.Error(err)
		}
		if err := os.Symlink(decoy, dest); err != nil {
			t.Error(err)
		}
	}
	job, err := c.Start(CloneRequest{URL: "https://github.com/fable/deck.git", Destination: dest})
	if err != nil {
		t.Fatal(err)
	}
	done := waitFor(t, c, job.ID, CloneDone)
	if done.Status != CloneDone {
		t.Fatalf("done: %+v", done)
	}
	if _, err := os.Stat(filepath.Join(root, "moved-away", "README.md")); err != nil {
		t.Fatalf("the clone landed in the directory that was checked: %v", err)
	}
	if entries, _ := os.ReadDir(decoy); len(entries) != 0 {
		t.Fatalf("the decoy received %d entries", len(entries))
	}
}

// Between the stop and the discard the destination name is pointed at
// another in-root directory holding someone's files. Discard must clear
// only the clone's own directory and leave the substitute untouched.
func TestSwappedDestinationNameCannotRedirectTheDelete(t *testing.T) {
	c, root, _ := testCloner(t)
	if err := os.WriteFile(filepath.Join(root, "STALL"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(root, "deck")
	job, err := c.Start(CloneRequest{URL: "git@github.com:fable/deck.git", Destination: dest})
	if err != nil {
		t.Fatal(err)
	}
	waitForPartial(t, filepath.Join(dest, ".git", "pack.tmp"))
	if _, err := c.Cancel(job.ID); err != nil {
		t.Fatal(err)
	}
	// An EMPTY substitute: the one case a plain rmdir at the name would
	// have deleted, so this is what the identity check exists to refuse.
	victim := mkdir(t, filepath.Join(root, "victim"))
	c.beforeRemove = func(job CloneJob) {
		if err := os.Rename(dest, filepath.Join(root, "moved-away")); err != nil {
			t.Error(err)
		}
		if err := os.Rename(victim, dest); err != nil {
			t.Error(err)
		}
	}
	err = c.Discard(job.ID)
	if err == nil {
		t.Fatalf("discard succeeded against a substituted name")
	}
	if info, err := os.Lstat(dest); err != nil || !info.IsDir() {
		t.Fatalf("the substituted directory was removed or not put back: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "moved-away", ".git", "pack.tmp")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the clone's own directory was cleared through its descriptor: %v", err)
	}
	if leftovers, _ := filepath.Glob(filepath.Join(root, ".clone-tomb-*")); len(leftovers) != 0 {
		t.Fatalf("a tomb name was left behind: %v", leftovers)
	}
}

// Between creating the directory and holding it, the freshly created
// directory is renamed away and an empty one put under its name. What is
// held must be what was created, or the start fails.
func TestASubstitutedDirectoryDuringCreationIsRefused(t *testing.T) {
	c, root, _ := testCloner(t)
	c.afterCreate = func(tmp string) {
		if err := os.Rename(tmp, filepath.Join(root, "stolen")); err != nil {
			t.Error(err)
		}
		mkdir(t, tmp)
	}
	_, err := c.Start(CloneRequest{URL: "https://github.com/fable/deck.git", Destination: filepath.Join(root, "deck")})
	if !errors.Is(err, ErrInvalidDir) {
		t.Fatalf("a directory that changed while being created is refused: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "deck")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("nothing was published under the destination's name: %v", err)
	}
}

// After the created directory is held and checked, the random name is
// swapped for another empty directory before the rename publishes it. What
// lands under the destination is compared with what is held; the
// substitute is taken back off the name and the start fails, so no job is
// registered against a directory git would not write into.
func TestASubstitutedDirectoryAfterTheHoldIsNotPublished(t *testing.T) {
	c, root, _ := testCloner(t)
	c.afterHold = func(tmp string) {
		if err := os.Rename(tmp, filepath.Join(root, "stolen")); err != nil {
			t.Error(err)
		}
		mkdir(t, tmp)
	}
	_, err := c.Start(CloneRequest{URL: "https://github.com/fable/deck.git", Destination: filepath.Join(root, "deck")})
	if !errors.Is(err, ErrInvalidDir) {
		t.Fatalf("a directory that changed while being published is refused: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "deck")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the substitute was not left published under the destination: %v", err)
	}
	if len(c.List()) != 0 {
		t.Fatalf("no job was registered")
	}
}

// After the tomb has been checked, an empty directory is swapped onto the
// tomb name before the rmdir. The clone's own directory was already
// cleared through its descriptor, and the most the swap can lose is the
// empty directory the actor placed under a random name only this call
// uses; anything with contents survives, because rmdir refuses it.
func TestASwapOntoTheTombLosesAtMostAnEmptyDirectory(t *testing.T) {
	c, root, _ := testCloner(t)
	if err := os.WriteFile(filepath.Join(root, "STALL"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(root, "deck")
	job, err := c.Start(CloneRequest{URL: "git@github.com:fable/deck.git", Destination: dest})
	if err != nil {
		t.Fatal(err)
	}
	waitForPartial(t, filepath.Join(dest, ".git", "pack.tmp"))
	if _, err := c.Cancel(job.ID); err != nil {
		t.Fatal(err)
	}
	full := mkdir(t, filepath.Join(root, "full"))
	if err := os.WriteFile(filepath.Join(full, "precious"), []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	c.afterTombCheck = func(tomb string) {
		if err := os.Rename(tomb, filepath.Join(root, "ours-moved")); err != nil {
			t.Error(err)
		}
		if err := os.Rename(full, tomb); err != nil {
			t.Error(err)
		}
	}
	if err := c.Discard(job.ID); err == nil {
		t.Fatalf("an rmdir onto a directory with contents cannot succeed")
	}
	if _, err := os.Stat(filepath.Join(root, "ours-moved")); err != nil {
		t.Fatalf("the clone's own directory is where the actor moved it, cleared: %v", err)
	}
	if entries, _ := os.ReadDir(filepath.Join(root, "ours-moved")); len(entries) != 0 {
		t.Fatalf("the clone's directory was cleared through its descriptor before the name was touched")
	}
	tombs, _ := filepath.Glob(filepath.Join(root, ".clone-tomb-*"))
	if len(tombs) != 1 {
		t.Fatalf("the substituted directory survives under the tomb name: %v", tombs)
	}
	if _, err := os.Stat(filepath.Join(tombs[0], "precious")); err != nil {
		t.Fatalf("the substitute kept its contents: %v", err)
	}
}

// A destination that already exists is taken only while empty, and taken
// atomically: a file, a link, or contents at the name refuse the clone.
func TestPublishOntoAnExistingNameIsAtomic(t *testing.T) {
	c, root, _ := testCloner(t)
	empty := mkdir(t, filepath.Join(root, "empty"))
	job, err := c.Start(CloneRequest{URL: "https://github.com/fable/deck.git", Destination: empty})
	if err != nil {
		t.Fatalf("an empty directory is a fine destination: %v", err)
	}
	waitFor(t, c, job.ID, CloneDone)
	if err := os.Symlink(root, filepath.Join(root, "linked")); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Start(CloneRequest{URL: "https://github.com/fable/deck.git", Destination: filepath.Join(root, "linked")}); !errors.Is(err, ErrInvalidDir) {
		t.Fatalf("a link at the name is refused: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "afile"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Start(CloneRequest{URL: "https://github.com/fable/deck.git", Destination: filepath.Join(root, "afile")}); !errors.Is(err, ErrInvalidDir) {
		t.Fatalf("a file at the name is refused: %v", err)
	}
}

func TestCloneNeverAsksAQuestion(t *testing.T) {
	c, root, _ := testCloner(t)
	job, err := c.Start(CloneRequest{URL: "https://github.com/fable/deck.git", Destination: filepath.Join(root, "deck")})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, c, job.ID, CloneDone)
	env, err := os.ReadFile(filepath.Join(root, "deck", ".git", "env.txt"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"GIT_TERMINAL_PROMPT=0\n", "GIT_SSH_COMMAND=ssh -o BatchMode=yes\n", "GIT_ASKPASS=\n"} {
		if !strings.Contains(string(env), want) {
			t.Errorf("git ran without %q in its environment", strings.TrimSpace(want))
		}
	}
}

func readHelperPID(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if body, err := os.ReadFile(path); err == nil && strings.TrimSpace(string(body)) != "" {
			pid, err := strconv.Atoi(strings.TrimSpace(string(body)))
			if err == nil {
				return pid
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("helper pid never appeared at %s", path)
	return 0
}

// processAlive reports whether pid still exists; a killed helper may linger
// as a zombie for a moment, so a live process is one that is not a zombie.
func processAlive(t *testing.T, pid int) bool {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		alive, err := cloneHelperRunning(pid)
		if err != nil {
			t.Fatalf("observe git helper process: %v", err)
		}
		if !alive {
			return false
		}
		time.Sleep(20 * time.Millisecond)
	}
	return true
}

func waitForPartial(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("partial clone never appeared at %s", path)
}
