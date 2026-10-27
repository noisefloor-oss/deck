package project

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
)

// DiffFile returns a unified diff against the last commit, including staged
// and unstaged changes. An untracked file has no committed side, so every
// line is an addition. Output is bounded like the text viewer.
func DiffFile(ctx context.Context, projectRoot, rel string) (string, error) {
	if err := checkRelative(rel); err != nil {
		return "", err
	}
	rel = cleanRelative(rel)
	changes, err := gitChanges(ctx, projectRoot)
	if err != nil {
		return "", err
	}
	if changes[rel] == ChangeNew {
		body, err := ReadTextFile(projectRoot, rel)
		if err != nil {
			return "", err
		}
		var diff strings.Builder
		fmt.Fprintf(&diff, "--- /dev/null\n+++ b/%s\n", rel)
		lines := []string{}
		if body != "" {
			lines = strings.Split(body, "\n")
			if lines[len(lines)-1] == "" {
				lines = lines[:len(lines)-1]
			}
		}
		fmt.Fprintf(&diff, "@@ -0,0 +1,%d @@\n", len(lines))
		for index, line := range lines {
			diff.WriteByte('+')
			diff.WriteString(line)
			if index < len(lines)-1 || strings.HasSuffix(body, "\n") {
				diff.WriteByte('\n')
			}
		}
		if diff.Len() > maxTextViewBytes {
			return "", ErrTooLarge
		}
		return diff.String(), nil
	}
	gitPath, err := exec.LookPath("git")
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(ctx, gitStatusTimeout)
	defer cancel()
	gitDirCmd := exec.CommandContext(ctx, gitPath, "--literal-pathspecs", "--no-optional-locks", "-C", projectRoot,
		"-c", "core.worktree="+projectRoot, "rev-parse", "--git-common-dir")
	gitDirCmd.Env = append(gitEnv([]string{projectRoot}), "GIT_WORK_TREE="+projectRoot)
	gitDirBytes, err := gitDirCmd.Output()
	if err != nil {
		return "", fmt.Errorf("git directory: %w", err)
	}
	gitDir := strings.TrimSpace(string(gitDirBytes))
	if !filepath.IsAbs(gitDir) {
		gitDir = filepath.Join(projectRoot, gitDir)
	}
	out := &cappedBuffer{limit: maxTextViewBytes, err: ErrTooLarge}
	cmd := exec.CommandContext(ctx, gitPath, "--literal-pathspecs", "--no-optional-locks", "-C", projectRoot,
		"-c", "core.worktree="+projectRoot, "-c", "core.objects="+filepath.Join(gitDir, "objects"),
		"diff", "--no-ext-diff", "--no-textconv", "--no-color", "--unified=3", "HEAD", "--", rel)
	cmd.Env = append(gitEnv([]string{projectRoot}), "GIT_WORK_TREE="+projectRoot,
		"GIT_OBJECT_DIRECTORY="+filepath.Join(gitDir, "objects"))
	cmd.Stdout = out
	err = cmd.Run()
	if out.overflow {
		return "", ErrTooLarge
	}
	if err != nil {
		return "", fmt.Errorf("git diff: %w", err)
	}
	return out.buf.String(), nil
}
