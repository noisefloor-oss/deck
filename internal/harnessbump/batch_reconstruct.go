package harnessbump

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// reconstructBatch reads candidate pins as data, applies them to trusted main's
// merge base, then compares the entire tree. No candidate code is executed.
func reconstructBatch(ctx context.Context, root string, resolver *Resolver, id, head string) (bool, error) {
	git := func(args ...string) ([]byte, error) {
		return exec.CommandContext(ctx, "git", append([]string{"-C", root}, args...)...).Output()
	}
	if _, err := git("fetch", "--quiet", "origin", head); err != nil {
		return false, err
	}
	raw, err := git("show", head+":Dockerfile")
	if err != nil {
		return false, err
	}
	target := ReadPins(raw)
	wantID, err := BatchID(target)
	if err != nil || wantID != id {
		return false, err
	}
	base, err := git("merge-base", head, "HEAD")
	if err != nil {
		return false, err
	}
	scratch, err := os.MkdirTemp("", "harness-batch-reconstruct-")
	if err != nil {
		return false, err
	}
	defer func() {
		_, _ = git("worktree", "remove", "--force", scratch)
		_ = os.RemoveAll(scratch)
	}()
	if _, err := git("worktree", "add", "--quiet", "--detach", scratch, strings.TrimSpace(string(base))); err != nil {
		return false, err
	}
	readme, err := readmeAtHead(ctx, root, head)
	if err != nil {
		return false, err
	}
	moved := false
	for _, h := range Catalogue {
		from, _, err := Pinned(scratch, h)
		if err != nil {
			return false, err
		}
		to := target[h.VersionArg]
		if from == to {
			continue
		}
		if h.Source == SourceManual {
			return false, nil
		}
		// Ownership is tied to the published snapshot, not a moving latest
		// tag. Settlement separately requires exact-head build/probe evidence.
		digests, err := resolver.Digests(ctx, h, to)
		if err != nil {
			return false, err
		}
		date, err := candidatePinDate(readme, h, to)
		if err != nil {
			return false, nil
		}
		if _, err := Apply(scratch, Bump{Harness: h, From: from, To: to, Digests: digests, PinnedOn: date}); err != nil {
			return false, err
		}
		moved = true
	}
	if !moved {
		return false, nil
	}
	err = exec.CommandContext(ctx, "git", "-C", scratch, "diff", "--quiet", head, "--", ".").Run()
	if err == nil {
		return true, nil
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 1 {
		return false, nil
	}
	return false, fmt.Errorf("batch reconstruction: %w", err)
}
