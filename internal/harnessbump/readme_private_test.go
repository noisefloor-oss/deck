package harnessbump

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPublicReadmeHarnessPinsMatchTheImage(t *testing.T) {
	root := filepath.Join("..", "..")
	raw, err := os.ReadFile(filepath.Join(root, "Dockerfile"))
	if err != nil {
		t.Fatal(err)
	}
	pins := ReadPins(raw)
	body, err := os.ReadFile(filepath.Join(root, "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	start, end := strings.Index(string(body), readmePinsStart), strings.Index(string(body), readmePinsEnd)
	if start < 0 || end <= start {
		t.Fatal("README pin matrix missing")
	}
	table := string(body)[start:end]
	for _, h := range Catalogue {
		pattern := readmePinPattern(h.Executable, pins[h.VersionArg])
		rows := pattern.FindAllStringSubmatch(table, -1)
		if len(rows) != 1 {
			t.Errorf("%s: expected one row at pinned version %s", h.Name, pins[h.VersionArg])
			continue
		}
		if _, err := time.Parse(time.DateOnly, rows[0][1]); err != nil {
			t.Errorf("%s: invalid UTC pin date", h.Name)
		}
	}
}
