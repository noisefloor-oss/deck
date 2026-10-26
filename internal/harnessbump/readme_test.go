package harnessbump

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestReadmePinDateChangesOnlyWithTheChosenHarness(t *testing.T) {
	root := t.TempDir()
	body := readmePinsStart + "\n| `pi` | `1.4.0` | 2026-10-22 | Live | Helper | Unknown |\n| `codex` | `0.162.1` | 2026-10-10 | Live | Helper | Unknown |\n" + readmePinsEnd + "\n"
	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte(body), 0644); err != nil {
		t.Fatal(err)
	}
	bump := Bump{Harness: Harness{Executable: "pi"}, From: "1.4.0", To: "1.4.1"}
	when := time.Date(2026, 10, 24, 1, 0, 0, 0, time.FixedZone("fixture", 2*60*60))
	planned, err := planReadmePin(root, bump, when)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(planned.content), "| `pi` | `1.4.1` | 2026-10-23 | Live | Helper | Unknown |") {
		t.Fatalf("updated row: %s", planned.content)
	}
	if !strings.Contains(string(planned.content), "| `codex` | `0.162.1` | 2026-10-10 |") {
		t.Fatal("unrelated pin date moved")
	}
	before, err := os.ReadFile(filepath.Join(root, "README.md"))
	if err != nil || string(before) != body {
		t.Fatal("planning changed the file before validation completed")
	}
}

func TestApplyRefusesAStaleReadmeWithoutChangingPins(t *testing.T) {
	root := fixtureRepo(t)
	raw, err := os.ReadFile(filepath.Join(root, "Dockerfile"))
	if err != nil {
		t.Fatal(err)
	}
	body := readmePinsStart + "\n| `omp` | `wrong-version` | 2026-01-01 | Manual | None | Unknown |\n" + readmePinsEnd
	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte(body), 0644); err != nil {
		t.Fatal(err)
	}
	var harness Harness
	for _, h := range Catalogue {
		if h.Name == "omp" {
			harness = h
		}
	}
	if _, err := Apply(root, Bump{Harness: harness, From: "18.1.2", To: "18.2.0"}); err == nil {
		t.Fatal("stale table was silently left behind")
	}
	after, err := os.ReadFile(filepath.Join(root, "Dockerfile"))
	if err != nil || string(after) != string(raw) {
		t.Fatal("failed table validation changed the Dockerfile")
	}
}

func readmeFixture(t *testing.T, root string) {
	t.Helper()
	body := readmePinsStart + "\n| `omp` | `18.1.2` | 2000-01-01 | Live | None | Unknown |\n| `crush` | `0.92.0` | 2000-01-02 | Manual | None | Unknown |\n" + readmePinsEnd + "\n"
	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte(body), 0644); err != nil {
		t.Fatal(err)
	}
}
func TestApplyWritesRecordedReadmeDate(t *testing.T) {
	root := fixtureRepo(t)
	readmeFixture(t, root)
	h, _ := Lookup("omp")
	changed, err := Apply(root, Bump{Harness: h, From: "18.1.2", To: "18.2.0", PinnedOn: "2001-12-31"})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, p := range changed {
		if p == "README.md" {
			found = true
		}
	}
	if !found {
		t.Fatal("Apply did not include README")
	}
	assertContains(t, filepath.Join(root, "README.md"), "| `omp` | `18.2.0` | 2001-12-31 |")
	assertContains(t, filepath.Join(root, "README.md"), "| `crush` | `0.92.0` | 2000-01-02 |")
	before, _ := os.ReadFile(filepath.Join(root, "Dockerfile"))
	if _, err := Apply(root, Bump{Harness: h, From: "18.2.0", To: "18.3.0", PinnedOn: "2001-02-31"}); err == nil {
		t.Fatal("invalid candidate date accepted")
	}
	after, _ := os.ReadFile(filepath.Join(root, "Dockerfile"))
	if string(before) != string(after) {
		t.Fatal("invalid date changed pins")
	}
}

func TestReadmeDateReconstructsSingleBumpAcrossDays(t *testing.T) {
	root, resolver := batchFixture(t)
	readmeFixture(t, root)
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", root}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=Fixture", "GIT_COMMITTER_NAME=Fixture", "GIT_AUTHOR_EMAIL=fixture@example.invalid", "GIT_COMMITTER_EMAIL=fixture@example.invalid")
		raw, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git: %v %s", err, raw)
		}
		return strings.TrimSpace(string(raw))
	}
	git("init", "-b", "main")
	git("add", ".")
	git("commit", "-m", "base")
	git("remote", "add", "origin", root)
	git("checkout", "-b", "bump")
	h, _ := Lookup("omp")
	if _, err := Apply(root, Bump{Harness: h, From: "18.1.2", To: "19.0.0", PinnedOn: "2001-12-31"}); err != nil {
		t.Fatal(err)
	}
	git("add", ".")
	git("commit", "-m", "candidate")
	head := git("rev-parse", "HEAD")
	git("checkout", "main")
	prove := Reconstruct(root, resolver)
	if ok, err := prove(context.Background(), h, "19.0.0", head); err != nil || !ok {
		t.Fatalf("recorded date not deterministic: %v %v", ok, err)
	}
	git("checkout", "bump")
	f := filepath.Join(root, "README.md")
	body, _ := os.ReadFile(f)
	os.WriteFile(f, append(body, []byte("unrelated prose edit\n")...), 0644)
	git("add", ".")
	git("commit", "-m", "extra README edit")
	modified := git("rev-parse", "HEAD")
	git("checkout", "main")
	if ok, err := prove(context.Background(), h, "19.0.0", modified); err != nil || ok {
		t.Fatalf("unrelated README edit admitted: %v %v", ok, err)
	}
}
func TestMalformedCandidateReadmeIsNotDeterministicWithRealGit(t *testing.T) {
	for _, kind := range []string{"single", "batch"} {
		for _, malformed := range []string{"marker", "row", "date"} {
			t.Run(kind+"/"+malformed, func(t *testing.T) {
				root, resolver := batchFixture(t)
				readmeFixture(t, root)
				git := func(args ...string) string {
					t.Helper()
					cmd := exec.Command("git", append([]string{"-C", root}, args...)...)
					cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=Fixture", "GIT_COMMITTER_NAME=Fixture", "GIT_AUTHOR_EMAIL=fixture@example.invalid", "GIT_COMMITTER_EMAIL=fixture@example.invalid")
					raw, err := cmd.CombinedOutput()
					if err != nil {
						t.Fatalf("git %v: %v\n%s", args, err, raw)
					}
					return strings.TrimSpace(string(raw))
				}
				git("init", "-b", "main")
				git("add", ".")
				git("commit", "-m", "base")
				git("remote", "add", "origin", root)
				git("checkout", "-b", "candidate")
				h, _ := Lookup("omp")
				version := "19.0.0"
				if kind == "single" {
					if _, err := Apply(root, Bump{Harness: h, From: "18.1.2", To: version, PinnedOn: "2001-12-31"}); err != nil {
						t.Fatal(err)
					}
				} else {
					result, err := ApplyBatch(context.Background(), root, resolver)
					if err != nil {
						t.Fatal(err)
					}
					recordHistoricalFixtureDates(t, root, "omp", "crush")
					h, version = Harness{Name: "all"}, result.ID
				}
				git("add", ".")
				git("commit", "-m", "candidate")
				path := filepath.Join(root, "README.md")
				raw, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				text := string(raw)
				switch malformed {
				case "marker":
					text = strings.Replace(text, readmePinsEnd, readmePinsEnd+"\n"+readmePinsEnd, 1)
				case "row":
					text = strings.Replace(text, "| `omp` | `19.0.0` |", "| `omp` | `18.1.2` |", 1)
				case "date":
					text = strings.Replace(text, "| `omp` | `19.0.0` | 2001-12-31 |", "| `omp` | `19.0.0` | 2001-02-31 |", 1)
				}
				if text == string(raw) {
					t.Fatal("fixture did not malform the candidate README")
				}
				if err := os.WriteFile(path, []byte(text), 0644); err != nil {
					t.Fatal(err)
				}
				git("add", "README.md")
				git("commit", "-m", "malformed candidate README")
				head := git("rev-parse", "HEAD")
				git("checkout", "main")
				if ok, err := Reconstruct(root, resolver)(context.Background(), h, version, head); err != nil || ok {
					t.Fatalf("malformed candidate README: deterministic = %v, err = %v", ok, err)
				}
			})
		}
	}
}

func TestReadmeAdmittedBySingleAndBatchProvenance(t *testing.T) {
	for _, name := range []string{"omp", "all"} {
		version := "19.0.0"
		if name == "all" {
			version = strings.Repeat("a", 64)
		}
		pr := pullRequest{Title: "chore(provider): pin " + name + " " + version, HeadRefName: BranchPrefix + name + "-" + version, BaseRefName: "main"}
		pr.Author.Login = "app/github-actions"
		for _, path := range []string{"Dockerfile", "README.md"} {
			pr.Files = append(pr.Files, struct {
				Path string `json:"path"`
			}{path})
		}
		if err := provenance(pr, map[string]bool{"app/github-actions": true}); err != nil {
			t.Fatal(err)
		}
	}
}

func recordHistoricalFixtureDates(t *testing.T, root string, names ...string) {
	t.Helper()
	path := filepath.Join(root, "README.md")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	for _, name := range names {
		h, err := Lookup(name)
		if err != nil {
			t.Fatal(err)
		}
		version, _, err := Pinned(root, h)
		if err != nil {
			t.Fatal(err)
		}
		date, err := candidatePinDate(text, h, version)
		if err != nil {
			t.Fatal(err)
		}
		old := fmt.Sprintf("| `%s` | `%s` | %s |", h.Executable, version, date)
		newRow := fmt.Sprintf("| `%s` | `%s` | 2001-12-31 |", h.Executable, version)
		text = strings.Replace(text, old, newRow, 1)
	}
	if err := os.WriteFile(path, []byte(text), 0644); err != nil {
		t.Fatal(err)
	}
}
