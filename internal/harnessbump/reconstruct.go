package harnessbump

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Reconstruct returns a Deterministic that rebuilds the bump the job makes
// from its recorded target version applied to the head's own base on
// main, in a scratch worktree of the repository at root, and compares the
// trees. The base is the head's merge base with main, so a main that
// moved since the bump does not make the job's own bump look like
// someone else's. git's credentials come from the environment the caller
// sets up; nothing is passed here.
func Reconstruct(root string, resolver *Resolver) Deterministic {
	return func(ctx context.Context, h Harness, version, head string) (bool, error) {
		if h.Name == "all" {
			return reconstructBatch(ctx, root, resolver, version, head)
		}
		if h.Source == SourceManual {
			// No index, no job-made bump: the pin moves by hand.
			return false, nil
		}
		git := func(dir string, args ...string) (string, error) {
			cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
			out, err := cmd.Output()
			return strings.TrimSpace(string(out)), err
		}
		if _, err := git(root, "fetch", "--quiet", "origin", head); err != nil {
			return false, fmt.Errorf("harnessbump: reconstruct: fetch %s: %w", head, err)
		}
		base, err := git(root, "merge-base", head, "HEAD")
		if err != nil {
			return false, fmt.Errorf("harnessbump: reconstruct: merge base of %s: %w", head, err)
		}
		scratch, err := os.MkdirTemp("", "harness-bump-reconstruct-")
		if err != nil {
			return false, err
		}
		defer func() {
			_ = exec.Command("git", "-C", root, "worktree", "remove", "--force", scratch).Run()
			_ = os.RemoveAll(scratch)
		}()
		if _, err := git(root, "worktree", "add", "--quiet", "--detach", scratch, base); err != nil {
			return false, fmt.Errorf("harnessbump: reconstruct: worktree at %s: %w", base, err)
		}
		from, _, err := Pinned(scratch, h)
		if err != nil {
			return false, err
		}
		if from == version {
			// The base already ships this version: nothing the job would
			// have done, so nothing the head could be.
			return false, nil
		}
		digests, err := resolver.Digests(ctx, h, version)
		if err != nil {
			return false, err
		}
		readme, err := readmeAtHead(ctx, root, head)
		if err != nil {
			return false, err
		}
		date, err := candidatePinDate(readme, h, version)
		if err != nil {
			return false, nil
		}
		if _, err := Apply(scratch, Bump{Harness: h, From: from, To: version, Digests: digests, PinnedOn: date}); err != nil {
			return false, err
		}
		cmd := exec.CommandContext(ctx, "git", "-C", scratch, "diff", "--quiet", head, "--", ".")
		err = cmd.Run()
		if err == nil {
			return true, nil
		}
		var exit *exec.ExitError
		if errors.As(err, &exit) && exit.ExitCode() == 1 {
			return false, nil
		}
		return false, fmt.Errorf("harnessbump: reconstruct: diff: %w", err)
	}
}

// ReconstructRoot resolves the repository root the command runs from.
func ReconstructRoot(root string) (string, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(filepath.Join(abs, "Dockerfile")); err != nil {
		return "", fmt.Errorf("harnessbump: %s is not the repository root", abs)
	}
	return abs, nil
}
