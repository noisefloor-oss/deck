package harnessbump

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const readmePinsStart = "<!-- harness-pins:start -->"
const readmePinsEnd = "<!-- harness-pins:end -->"

// planReadmePin skips exports and fixtures without a marked harness table.
func planReadmePin(root string, bump Bump, when time.Time) (*edit, error) {
	abs := filepath.Join(root, "README.md")
	raw, err := os.ReadFile(filepath.Clean(abs))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	body := string(raw)
	start := strings.Index(body, readmePinsStart)
	if start < 0 {
		return nil, nil
	}
	end := strings.Index(body, readmePinsEnd)
	if end <= start || strings.Count(body, readmePinsStart) != 1 || strings.Count(body, readmePinsEnd) != 1 {
		return nil, fmt.Errorf("harnessbump: README pin table markers are invalid")
	}
	pattern := readmePinPattern(bump.Harness.Executable, bump.From)
	table := body[start:end]
	if len(pattern.FindAllStringIndex(table, -1)) != 1 {
		return nil, fmt.Errorf("harnessbump: README pin table must name %s %s exactly once", bump.Harness.Executable, bump.From)
	}
	replacement := fmt.Sprintf("| `%s` | `%s` | %s |", bump.Harness.Executable, bump.To, when.UTC().Format(time.DateOnly))
	table = pattern.ReplaceAllStringFunc(table, func(string) string { return replacement })
	info, err := os.Stat(abs)
	if err != nil {
		return nil, err
	}
	return &edit{path: "README.md", abs: abs, mode: info.Mode().Perm(), content: []byte(body[:start] + table + body[end:]), matched: 1}, nil
}

// The version and date are candidate data, just like the candidate Docker pins.
// Reconstructing them never executes candidate code or trusts other README edits:
// the final comparison still checks the whole reconstructed tree.
func readmeAtHead(ctx context.Context, root, head string) (string, error) {
	names, err := exec.CommandContext(ctx, "git", "-C", root, "ls-tree", "--name-only", head, "--", "README.md").Output()
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(string(names)) == "" {
		return "", nil
	}
	raw, err := exec.CommandContext(ctx, "git", "-C", root, "show", head+":README.md").Output()
	return string(raw), err
}

func readmePinPattern(executable, version string) *regexp.Regexp {
	return regexp.MustCompile("(?m)^\\| `" + regexp.QuoteMeta(executable) + "` \\| `" + regexp.QuoteMeta(version) + "` \\| (\\d{4}-\\d{2}-\\d{2}) \\|")
}
func candidatePinDate(body string, h Harness, version string) (string, error) {
	start, end := strings.Index(body, readmePinsStart), strings.Index(body, readmePinsEnd)
	if start < 0 {
		return "", nil
	}
	if end <= start || strings.Count(body, readmePinsStart) != 1 || strings.Count(body, readmePinsEnd) != 1 {
		return "", fmt.Errorf("invalid candidate README markers")
	}
	rows := readmePinPattern(h.Executable, version).FindAllStringSubmatch(body[start:end], -1)
	if len(rows) != 1 {
		return "", fmt.Errorf("candidate README must name %s %s exactly once", h.Executable, version)
	}
	if _, err := time.Parse(time.DateOnly, rows[0][1]); err != nil {
		return "", err
	}
	return rows[0][1], nil
}
