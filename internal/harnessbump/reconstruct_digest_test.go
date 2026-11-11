package harnessbump

import (
	"context"
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestRecordedReconstructionKeepsProducerDigestChecks(t *testing.T) {
	for _, batch := range []bool{false, true} {
		t.Run(fmt.Sprintf("batch=%v", batch), func(t *testing.T) {
			root := t.TempDir()
			artifact := []byte("published fixture archive")
			sum := sha256.Sum256(artifact)
			digest := fmt.Sprintf("%x", sum)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(artifact) }))
			t.Cleanup(server.Close)
			h := Harness{Name: "fixture", Executable: "fixture", VersionArg: "FIXTURE_VERSION", Source: SourceNPM,
				Artifacts: []Artifact{{SHAArg: "FIXTURE_SHA256_AMD64", URL: func(string) string { return server.URL + "/archive" }}},
				Repeats:   []Repeat{{Path: "repeat.txt", Format: "fixture/%s", Required: true}}}
			saved := Catalogue
			Catalogue = []Harness{h}
			t.Cleanup(func() { Catalogue = saved })
			write := func(name, body string) {
				t.Helper()
				if err := os.WriteFile(filepath.Join(root, name), []byte(body), 0600); err != nil {
					t.Fatal(err)
				}
			}
			write("Dockerfile", "ARG FIXTURE_VERSION=1.0.0\nARG FIXTURE_SHA256_AMD64="+strings.Repeat("a", 64)+"\n")
			write("repeat.txt", "fixture/1.0.0\n")
			write("README.md", readmePinsStart+"\n| \x60fixture\x60 | \x601.0.0\x60 | 2000-01-01 | Manual | None | Unknown |\n"+readmePinsEnd+"\n")
			git := func(args ...string) string {
				t.Helper()
				c := exec.Command("git", append([]string{"-C", root}, args...)...)
				c.Env = append(os.Environ(), "GIT_AUTHOR_NAME=Fixture", "GIT_COMMITTER_NAME=Fixture", "GIT_AUTHOR_EMAIL=fixture@example.invalid", "GIT_COMMITTER_EMAIL=fixture@example.invalid")
				b, err := c.CombinedOutput()
				if err != nil {
					t.Fatalf("git %v: %v\n%s", args, err, b)
				}
				return strings.TrimSpace(string(b))
			}
			git("init", "-b", "main")
			git("add", ".")
			git("commit", "-m", "base")
			git("remote", "add", "origin", root)
			git("checkout", "-b", "candidate")
			if _, err := Apply(root, Bump{Harness: h, From: "1.0.0", To: "2.0.0", Digests: map[string]string{"FIXTURE_SHA256_AMD64": digest}, PinnedOn: "2000-01-02"}); err != nil {
				t.Fatal(err)
			}
			git("add", ".")
			git("commit", "-m", "candidate")
			head := git("rev-parse", "HEAD")
			git("checkout", "main")
			identity := "2.0.0"
			proofHarness := h
			if batch {
				var err error
				identity, err = BatchID(map[string]string{"FIXTURE_VERSION": "2.0.0"})
				if err != nil {
					t.Fatal(err)
				}
				proofHarness = Harness{Name: "all"}
			}
			ctx := context.Background()
			producer := Reconstruct(root, &Resolver{HTTP: server.Client()})
			recorded := ReconstructRecorded(root)
			if ok, err := producer(ctx, proofHarness, identity, head); err != nil || !ok {
				t.Fatalf("producer rejected the measured digest and historical pin date: %v, %v", ok, err)
			}
			noEvidence := func(context.Context, Harness, string, string) (bool, error) { return false, nil }
			checkNoEvidence := func(candidate string) {
				t.Helper()
				pr := pullRequest{Title: "chore(provider): pin " + proofHarness.Name + " " + identity, HeadRefOid: candidate}
				reason, err := jobHeadProof(ctx, pr, recorded, noEvidence)
				if err != nil || !strings.Contains(reason, "no bump run") {
					t.Fatalf("pins bypassed producer evidence: %q, %v", reason, err)
				}
			}
			checkNoEvidence(head)
			git("checkout", "candidate")
			body, err := os.ReadFile(filepath.Join(root, "Dockerfile"))
			if err != nil {
				t.Fatal(err)
			}
			write("Dockerfile", strings.Replace(string(body), digest, strings.Repeat("b", 64), 1))
			git("add", ".")
			git("commit", "-m", "alter digest")
			altered := git("rev-parse", "HEAD")
			git("checkout", "main")
			if ok, err := producer(ctx, proofHarness, identity, altered); err != nil || ok {
				t.Fatalf("producer vouched for an orphan with an altered digest: %v, %v", ok, err)
			}
			if ok, err := recorded(ctx, proofHarness, identity, altered); err != nil || !ok {
				t.Fatalf("recorded pins unexpectedly claimed upstream verification: %v, %v", ok, err)
			}
			checkNoEvidence(altered)
			git("checkout", "-b", "malformed", head)
			body, err = os.ReadFile(filepath.Join(root, "Dockerfile"))
			if err != nil {
				t.Fatal(err)
			}
			write("Dockerfile", strings.Replace(string(body), digest, "not-a-digest", 1))
			git("add", ".")
			git("commit", "-m", "malformed digest")
			malformed := git("rev-parse", "HEAD")
			git("checkout", "main")
			if ok, err := recorded(ctx, proofHarness, identity, malformed); err != nil || ok {
				t.Fatalf("malformed recorded candidate was not refused: %v, %v", ok, err)
			}
			server.Close()
			if ok, err := recorded(ctx, proofHarness, identity, head); err != nil || !ok {
				t.Fatalf("recorded candidate depends on live archive availability: %v, %v", ok, err)
			}
			if ok, err := producer(ctx, proofHarness, identity, head); err == nil || ok {
				t.Fatalf("producer ignored an unavailable archive: %v, %v", ok, err)
			}
			git("checkout", "-b", "extra", head)
			write("unrelated.txt", "not generated\n")
			git("add", ".")
			git("commit", "-m", "extra")
			extra := git("rev-parse", "HEAD")
			git("checkout", "main")
			if ok, err := recorded(ctx, proofHarness, identity, extra); err != nil || ok {
				t.Fatalf("extra edit admitted: %v, %v", ok, err)
			}
		})
	}
}

func TestCandidateDigestsRejectMissingOrMalformedPins(t *testing.T) {
	h := Harness{Artifacts: []Artifact{{SHAArg: "FIXTURE_SHA256"}}}
	for _, pin := range []string{"", "not-a-digest", strings.Repeat("a", 63), strings.Repeat("A", 64)} {
		if _, err := candidateDigests(map[string]string{"FIXTURE_SHA256": pin}, h); err == nil {
			t.Fatalf("invalid digest accepted: %q", pin)
		}
	}
}
