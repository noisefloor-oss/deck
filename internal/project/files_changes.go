package project

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
	"strings"
	"time"
)

const (
	// ChangeModified marks a tracked entry that differs from the last
	// commit, or a folder holding one that does.
	ChangeModified = "modified"
	// ChangeNew marks an untracked file.
	ChangeNew = "new"

	// changeDeleted never reaches a row; it keeps the folders above a
	// removed file marked.
	changeDeleted = "deleted"

	gitStatusTimeout   = 2 * time.Second
	gitStatusMaxOutput = 4 << 20
)

var errGitStatusTooLarge = errors.New("git status output exceeds the limit")

// cappedBuffer fails the write that would pass its limit, which stops the
// command instead of letting a huge status grow without bound.
//
// The buffer is a named field, not embedded: embedding would promote
// ReadFrom, which os/exec's io.Copy prefers over Write and so would skip
// the limit.
type cappedBuffer struct {
	buf      bytes.Buffer
	limit    int
	err      error
	overflow bool
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	if b.buf.Len()+len(p) > b.limit {
		b.overflow = true
		if b.err != nil {
			return 0, b.err
		}
		return 0, errGitStatusTooLarge
	}
	return b.buf.Write(p)
}

// annotateChanges marks the listing's entries from one `git status` of the
// project root. Markers are a convenience: any failure (no git, not a
// repository, a timeout, oversized output) leaves the listing unmarked.
func annotateChanges(ctx context.Context, projectRoot string, listing *FileListing) {
	if listing == nil || len(listing.Entries) == 0 {
		return
	}
	changes, err := gitChanges(ctx, projectRoot)
	if err != nil || len(changes) == 0 {
		return
	}
	for i := range listing.Entries {
		e := &listing.Entries[i]
		if !e.Dir {
			if c := changes[e.Path]; c != changeDeleted {
				e.Change = c
			}
			continue
		}
		// Git reports a changed submodule at the submodule's own path.
		if c, ok := changes[e.Path]; ok && c != changeDeleted {
			e.Change = ChangeModified
			continue
		}
		prefix := e.Path + "/"
		for path := range changes {
			if strings.HasPrefix(path, prefix) {
				e.Change = ChangeModified
				break
			}
		}
	}
}

// gitChanges returns the project-relative paths that differ from the last
// commit. Discovery is ceilinged at the project root's parent, so a project
// nested inside some other repository reports nothing rather than that
// repository's status.
func gitChanges(ctx context.Context, projectRoot string) (map[string]string, error) {
	gitPath, err := exec.LookPath("git")
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, gitStatusTimeout)
	defer cancel()
	out := &cappedBuffer{limit: gitStatusMaxOutput}
	cmd := exec.CommandContext(ctx, gitPath, "--no-optional-locks", "-C", projectRoot,
		"status", "--porcelain=v2", "-z", "--untracked-files=all", "--no-renames")
	cmd.Env = gitEnv([]string{projectRoot})
	cmd.Stdout = out
	if err := cmd.Run(); err != nil {
		return nil, err
	}
	return parseGitStatus(out.buf.Bytes()), nil
}

func gitHead(ctx context.Context, projectRoot string) (string, error) {
	gitPath, err := exec.LookPath("git")
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(ctx, gitStatusTimeout)
	defer cancel()
	return gitOutput(ctx, gitPath, projectRoot, "rev-parse", "HEAD")
}

// parseGitStatus reads `git status --porcelain=v2 -z` output. A deleted
// path is kept as changeDeleted so its folders can be marked. An unmerged
// record is never read as deleted: a modify/delete conflict reports a D
// while the file stays on disk.
func parseGitStatus(raw []byte) map[string]string {
	changes := map[string]string{}
	for _, rec := range strings.Split(string(raw), "\x00") {
		if len(rec) < 3 {
			continue
		}
		switch rec[0] {
		case '?':
			changes[rec[2:]] = ChangeNew
		case '1':
			// 1 XY sub mH mI mW hH hI path
			fields := strings.SplitN(rec, " ", 9)
			if len(fields) < 9 {
				continue
			}
			if strings.Contains(fields[1], "D") {
				changes[fields[8]] = changeDeleted
				continue
			}
			changes[fields[8]] = ChangeModified
		case 'u':
			// u XY sub m1 m2 m3 mW h1 h2 h3 path
			fields := strings.SplitN(rec, " ", 11)
			if len(fields) < 11 {
				continue
			}
			changes[fields[10]] = ChangeModified
		}
	}
	return changes
}
