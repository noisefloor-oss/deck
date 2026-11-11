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
	return reconstruct(root, func(ctx context.Context, h Harness, version string, _ map[string]string) (map[string]string, error) {
		return resolver.Digests(ctx, h, version)
	})
}

// ReconstructRecorded checks generated edits using the recorded pins as data.
// Consumers must also require trusted producer evidence for this exact head;
// the producer itself uses Reconstruct to verify the upstream artifact bytes.
func ReconstructRecorded(root string) Deterministic {
	return reconstruct(root, func(_ context.Context, h Harness, _ string, pins map[string]string) (map[string]string, error) {
		digests, err := candidateDigests(pins, h)
		if err != nil {
			return nil, errCandidatePins
		}
		return digests, nil
	})
}

var errCandidatePins = errors.New("harnessbump: invalid recorded candidate pins")

// artifactDigests uses errCandidatePins for a candidate refusal; upstream
// failures remain operational errors.
type artifactDigests func(context.Context, Harness, string, map[string]string) (map[string]string, error)

func reconstruct(root string, digestsFor artifactDigests) Deterministic {
	return func(ctx context.Context, h Harness, version, head string) (bool, error) {
		if h.Name == "all" {
			return reconstructBatch(ctx, root, version, head, digestsFor)
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
		raw, err := git(root, "show", head+":Dockerfile")
		if err != nil {
			return false, err
		}
		digests, err := digestsFor(ctx, h, version, ReadPins([]byte(raw)))
		if errors.Is(err, errCandidatePins) {
			return false, nil
		}
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

// candidateDigests reads pins as data; only exact-head producer evidence can
// establish that their bytes passed the download, image and probe gates.
func candidateDigests(pins map[string]string, h Harness) (map[string]string, error) {
	digests := make(map[string]string, len(h.Artifacts))
	for _, a := range h.Artifacts {
		d := pins[a.SHAArg]
		if !hexDigest.MatchString(d) {
			return nil, fmt.Errorf("harnessbump: candidate %s is not sha256 hex", a.SHAArg)
		}
		digests[a.SHAArg] = d
	}
	return digests, nil
}
