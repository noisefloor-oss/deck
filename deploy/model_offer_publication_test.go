package deploy

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The publication path of the model offer script, exercised end to end
// against a stateful gh mock: no moving-base write, byte-exact source
// validation, and convergence from any interruption point without
// overwriting a foreign edit.

const mockGH = `#!/bin/sh
state="$MOCK_STATE"; log="$state/calls"
cmd="$1"
note() { printf '%s\n' "$*" >>"$log"; }
sha_of() { printf '%s' "$1" | sha256sum | cut -d' ' -f1; }

# Real gh applies --jq client side; the mock pipes through jq the same way.
jq=""; method=GET; ref=""; content=""; branch=""; sha=""; endpoint=""
sub=""; shift
while [ $# -gt 0 ]; do
  case "$1" in
    -X) method="$2"; shift 2 ;;
    --jq) jq="$2"; shift 2 ;;
    -f) case "$2" in ref=*) ref="${2#ref=}";; sha=*) sha="${2#sha=}";; content=*) content="${2#content=}";; branch=*) branch="${2#branch=}";; esac; shift 2 ;;
    repos/*) endpoint="$1"; shift ;;
    list|create) sub="$1"; shift ;;
    *) shift ;;
  esac
done
emit() {
  if [ -n "$jq" ]; then printf '%s' "$1" | jq -r "$jq"; else printf '%s\n' "$1"; fi
}

case "$cmd" in
  pr)
    case "$sub" in
      list) emit "[]" ;;
      create) note "pr-create" ;;
      *) exit 3 ;;
    esac ;;
  api)
    case "$endpoint" in
      */git/refs)
        [ "$method" = POST ] || exit 4
        name="${ref#refs/heads/}"
        refkey=$(echo "$name" | tr / _)
        [ -f "$state/refs/$refkey" ] && exit 1
        mkdir -p "$state/refs"; printf '%s' "$sha" >"$state/refs/$refkey"; note "refs-post $name $sha"; exit 0 ;;
      */git/ref/heads/*)
        name="${endpoint#*/git/ref/heads/}"
        refkey=$(echo "$name" | tr / _)
        [ -f "$state/refs/$refkey" ] || exit 1
        [ "$jq" = .object.sha ] && { cat "$state/refs/$refkey"; echo; exit 0; }
        exit 0 ;;
      */contents/*)
        path="${endpoint#*/contents/}"; path="${path%%\?*}"
        case "$endpoint" in *\?ref=*) ref="${endpoint##*\?ref=}" ;; esac
        if [ "$method" = PUT ]; then
          if [ "${MOCK_FAIL_FIRST_PUT:-}" = 1 ] && [ ! -f "$state/put-any" ]; then
            : >"$state/put-any"; exit 1
          fi
          mkdir -p "$state/blobs/$branch"; printf '%s' "$content" | base64 -d >"$state/blobs/$branch/$(echo "$path" | tr / _)"; note "contents-put $branch $path"; exit 0
        fi
        blob="$state/blobs/$ref/$(echo "$path" | tr / _)"
        # A branch created at mainsha inherits its tree: model git's
        # blob inheritance for refs with no stored override.
        if [ ! -f "$blob" ] && [ "$ref" != "mainsha" ]; then
          blob="$state/blobs/mainsha/$(echo "$path" | tr / _)"
        fi
        [ -f "$blob" ] || exit 1
        if [ -z "$jq" ]; then
          printf '{"content":"%s","sha":"%s"}\n' "$(base64 -w0 "$blob")" "$(sha_of "$(cat "$blob")")"
          exit 0
        fi
        case "$jq" in
          .content) base64 -w0 "$blob"; echo ;;
          .sha) sha_of "$(cat "$blob")" ;;
        esac ;;
      *) exit 4 ;;
    esac ;;
  *) exit 5 ;;
esac
`

type offerFixture struct {
	root, state, bin string
}

// newOfferFixture builds a workspace repo, a stateful gh mock, and a plan
// whose single file moves a pin literal.
func newOfferFixture(t *testing.T) offerFixture {
	t.Helper()
	fx := offerFixture{}
	fx.root = t.TempDir()
	fx.state = t.TempDir()
	fx.bin = t.TempDir()

	sdlc := "The primary reviewer is Codex on `gpt-6-sol` at high reasoning effort.\n"
	if err := os.WriteFile(filepath.Join(fx.root, "SDLC.md"), []byte(sdlc), 0o644); err != nil {
		t.Fatal(err)
	}
	next := "The primary reviewer is Codex on `gpt-6.2-sol` at high reasoning effort.\n"
	sourceSum := sha256.Sum256([]byte(sdlc))
	plan := `{"id":"abc123","statuses":[{"role":"primary","harness":"codex","pinned":"gpt-6-sol","latest":"gpt-6.2-sol"}],"offers":[{"role":"primary","harness":"codex","from":"gpt-6-sol","to":"gpt-6.2-sol"}],"files":{"SDLC.md":"` +
		base64.StdEncoding.EncodeToString([]byte(next)) + `"},"sources":{"SDLC.md":"` + hex.EncodeToString(sourceSum[:]) + `"}}`
	if err := os.WriteFile(filepath.Join(fx.state, "plan.json"), []byte(plan), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fx.bin, "gh"), []byte(mockGH), 0o755); err != nil {
		t.Fatal(err)
	}
	// The script refuses to run without all three CLIs on PATH; a CI runner
	// has none. Discovery is replaced by the prepared plan, so inert stubs
	// satisfy the guard.
	for _, cli := range []string{"codex", "claude", "kimi"} {
		if err := os.WriteFile(filepath.Join(fx.bin, cli), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// main carries exactly the workspace's bytes at the base commit.
	if err := os.MkdirAll(filepath.Join(fx.state, "blobs", "mainsha"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fx.state, "blobs", "mainsha", "SDLC.md"), []byte(sdlc), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(fx.state, "refs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fx.state, "refs", "main"), []byte("mainsha"), 0o644); err != nil {
		t.Fatal(err)
	}
	return fx
}

func (fx offerFixture) run(t *testing.T) (string, error) {
	t.Helper()
	return runScript(t, filepath.Join("..", "scripts", "model-offer", "model-offer.sh"),
		map[string]string{
			"PATH":                       fx.bin + ":" + os.Getenv("PATH"),
			"MOCK_STATE":                 fx.state,
			"NOISEDECK_MODEL_OFFER_PLAN": filepath.Join(fx.state, "plan.json"),
		},
		"--repo", fx.root, "--slug", "example-owner/deck")
}

func (fx offerFixture) calls() string {
	raw, err := os.ReadFile(filepath.Join(fx.state, "calls"))
	if err != nil {
		return ""
	}
	return string(raw)
}

func runScript(t *testing.T, path string, env map[string]string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command("bash", append([]string{path}, args...)...)
	cmd.Env = os.Environ()
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func TestModelOfferPublicationRefusesAMovedBase(t *testing.T) {
	fx := newOfferFixture(t)
	// main moved after the plan was built: its SDLC.md differs by content.
	if err := os.WriteFile(filepath.Join(fx.state, "blobs", "mainsha", "SDLC.md"),
		[]byte("The primary reviewer is Codex on `gpt-6.1-sol` at high reasoning effort.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := fx.run(t)
	if err == nil || !strings.Contains(out, "does not match the plan's source bytes") {
		t.Fatalf("a moved base published anyway: err=%v out=%q", err, out)
	}
	if strings.Contains(fx.calls(), "refs-post") {
		t.Fatal("validation must abort before the branch is created")
	}
}

func TestModelOfferPublicationComparesExactBytes(t *testing.T) {
	fx := newOfferFixture(t)
	// One trailing newline of difference — invisible to command
	// substitution, decisive for hashes.
	if err := os.WriteFile(filepath.Join(fx.state, "blobs", "mainsha", "SDLC.md"),
		[]byte("The primary reviewer is Codex on `gpt-6-sol` at high reasoning effort.\n\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := fx.run(t)
	if err == nil || !strings.Contains(out, "does not match the plan's source bytes") {
		t.Fatalf("a trailing-newline difference published anyway: err=%v out=%q", err, out)
	}
}

func TestModelOfferPublicationCompletesAndResumes(t *testing.T) {
	fx := newOfferFixture(t)
	out, err := fx.run(t)
	if err != nil {
		t.Fatalf("clean publication failed: %v\n%s", err, out)
	}
	for _, call := range []string{"refs-post model-bump/reviewers-abc123 mainsha", "contents-put model-bump/reviewers-abc123 SDLC.md", "pr-create"} {
		if !strings.Contains(fx.calls(), call) {
			t.Fatalf("missing call %q; log:\n%s", call, fx.calls())
		}
	}
}

func TestModelOfferPublicationResumesAfterPartialWrite(t *testing.T) {
	fx := newOfferFixture(t)
	branch := "model-bump/reviewers-abc123"
	// An interrupted run created the branch and wrote the planned bytes
	// already; the daily retry must only create the PR.
	if err := os.WriteFile(filepath.Join(fx.state, "refs", strings.ReplaceAll(branch, "/", "_")), []byte("mainsha"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(fx.state, "blobs", branch), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fx.state, "blobs", branch, "SDLC.md"),
		[]byte("The primary reviewer is Codex on `gpt-6.2-sol` at high reasoning effort.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := fx.run(t)
	if err != nil {
		t.Fatalf("resume failed: %v\n%s", err, out)
	}
	calls := fx.calls()
	if strings.Contains(calls, "contents-put") {
		t.Fatalf("resume rewrote an already-written file; log:\n%s", calls)
	}
	if !strings.Contains(calls, "pr-create") {
		t.Fatalf("resume never created the PR; log:\n%s", calls)
	}
}

func TestModelOfferPublicationRefusesForeignBranchEdits(t *testing.T) {
	fx := newOfferFixture(t)
	branch := "model-bump/reviewers-abc123"
	if err := os.WriteFile(filepath.Join(fx.state, "refs", strings.ReplaceAll(branch, "/", "_")), []byte("mainsha"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(fx.state, "blobs", branch), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fx.state, "blobs", branch, "SDLC.md"),
		[]byte("someone else edited this file\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := fx.run(t)
	if err == nil || !strings.Contains(out, "neither the plan's source bytes nor the planned bytes") {
		t.Fatalf("a foreign edit was overwritten: err=%v out=%q", err, out)
	}
	if strings.Contains(fx.calls(), "contents-put") {
		t.Fatal("the foreign edit must not be replaced")
	}
}

func TestModelOfferPublicationRefusesAStaleReplayedPlan(t *testing.T) {
	fx := newOfferFixture(t)
	// main and the checkout both advanced past the bytes the plan was
	// built from; matching them to each other must not be enough.
	if err := os.WriteFile(filepath.Join(fx.state, "blobs", "mainsha", "SDLC.md"),
		[]byte("The primary reviewer is Codex on `gpt-6-sol` at high reasoning effort.\nan unrelated later addition.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fx.root, "SDLC.md"),
		[]byte("The primary reviewer is Codex on `gpt-6-sol` at high reasoning effort.\nan unrelated later addition.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := fx.run(t)
	if err == nil || !strings.Contains(out, "does not match the plan's source bytes") {
		t.Fatalf("a stale replayed plan published anyway: err=%v out=%q", err, out)
	}
	if strings.Contains(fx.calls(), "refs-post") {
		t.Fatal("the stale plan must be refused before any write")
	}
}

func TestModelOfferPublicationStopsOnAFailedWrite(t *testing.T) {
	fx := newOfferFixture(t)
	run := exec.Command("bash", filepath.Join("..", "scripts", "model-offer", "model-offer.sh"),
		"--repo", fx.root, "--slug", "example-owner/deck")
	run.Env = append(os.Environ(),
		"PATH="+fx.bin+":"+os.Getenv("PATH"),
		"MOCK_STATE="+fx.state,
		"NOISEDECK_MODEL_OFFER_PLAN="+filepath.Join(fx.state, "plan.json"),
		"MOCK_FAIL_FIRST_PUT=1",
	)
	out, err := run.CombinedOutput()
	if err == nil {
		t.Fatalf("a failed write still exited zero: %s", out)
	}
	if strings.Contains(string(out), "pr-create") || strings.Contains(fx.calls(), "pr-create") {
		t.Fatal("an incomplete publication must not create the PR")
	}
}
