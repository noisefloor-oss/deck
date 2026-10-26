package modelbump

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"
)

// unrecognizedModel is the structured marker Claude Code prints when a
// --model value is not in its local catalog. Measured against the pinned
// CLI 2.1.296: the marker is emitted BEFORE any credential is consulted,
// but on an authenticated host the CLI still proceeds to send /v1/messages
// requests for the unknown model — the marker is a warning, not a stop.
// The membership probe therefore runs with a scratch HOME (no
// credentials) and a dead loopback endpoint (no egress): the marker still
// appears for an unknown model, while a known model gets only as far as
// the credential or connection failure. It is a stable stderr tag, not
// prose to scrape.
const unrecognizedModel = "claude-code:unrecognized_model"

// CodexCatalog lists every model the codex CLI's bundled catalog marks
// visible. It is an offline command: no credential, no network, one JSON
// document — the same document the image's pinned CLI ships, so the
// catalog the lane sees is the catalog reviewers have.
func CodexCatalog(ctx context.Context, execPath string) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	raw, err := exec.CommandContext(ctx, execPath, "debug", "models", "--bundled").Output()
	if err != nil {
		return nil, fmt.Errorf("modelbump: %s debug models --bundled: %w", execPath, err)
	}
	var doc struct {
		Models []struct {
			Slug       string `json:"slug"`
			Visibility string `json:"visibility"`
		} `json:"models"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("modelbump: decoding codex model catalog: %w", err)
	}
	var slugs []string
	for _, m := range doc.Models {
		if m.Slug != "" && m.Visibility == "list" {
			slugs = append(slugs, m.Slug)
		}
	}
	if len(slugs) == 0 {
		return nil, fmt.Errorf("modelbump: codex catalog lists no visible models; refusing to offer anything")
	}
	return slugs, nil
}

// KimiCatalog lists the configured Kimi Code aliases without starting a
// session or making a model call. Only managed Kimi Code aliases participate
// in the k-version tier-family comparison; other configured providers can use
// different naming grammars.
func KimiCatalog(ctx context.Context, execPath string) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	raw, err := exec.CommandContext(ctx, execPath, "provider", "list", "--json").Output()
	if err != nil {
		return nil, fmt.Errorf("modelbump: %s provider list --json: %w", execPath, err)
	}
	var doc struct {
		Models map[string]json.RawMessage `json:"models"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("modelbump: decoding kimi model catalog: %w", err)
	}
	var slugs []string
	for alias := range doc.Models {
		if strings.HasPrefix(alias, "kimi-code/") {
			slugs = append(slugs, alias)
		}
	}
	if len(slugs) == 0 {
		return nil, fmt.Errorf("modelbump: kimi catalog lists no managed models; refusing to offer anything")
	}
	sort.Strings(slugs)
	return slugs, nil
}

// downstreamFailure lists the output signatures the pinned CLI
// measurably reaches ONLY after catalog validation passes: with a
// scratch HOME a known model prints "Not logged in · Please run /login"
// (measured on 2.1.296, exit 0), and with credentials or a reachable
// endpoint the run instead fails on the connection or the credential.
// Anything else — argument rejection, startup crashes, settings errors —
// never validated the catalog and classifies Ambiguous.
var downstreamFailure = [][]byte{
	[]byte("Not logged in"),
	[]byte("Please run /login"),
	[]byte("ECONNREFUSED"),
	[]byte("fetch failed"),
	[]byte("ENOTFOUND"),
	[]byte("ETIMEDOUT"),
	[]byte("api key"),
	[]byte("API key"),
	[]byte("Unauthorized"),
	[]byte("authentication"),
}

// ClaudeMember probes whether one candidate model is in the claude CLI's
// local catalog, without credentials and without network egress. The
// probe runs `claude --model <candidate> -p ping` under a scratch HOME
// and ANTHROPIC_BASE_URL pointed at a dead loopback port:
//
//   - an unlaunchable executable is a setup failure and returns an
//     error, failing the whole plan;
//   - the unrecognized-model marker means the local catalog rejected the
//     candidate: Absent, a local fact;
//   - output matching a measured downstream-failure signature means the
//     CLI ran past validation into the credential or connection failure
//     only a known model reaches: Present, and no model call was spent;
//   - anything else — silent exits, argument rejection, startup or
//     settings errors, timeouts, signal death — cannot rank candidates:
//     Ambiguous, and the lane then withholds the whole role rather than
//     offer a farther candidate while a nearer one may exist.
func ClaudeMember(ctx context.Context, execPath, model string) (Presence, error) {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	scratch, err := os.MkdirTemp("", "modelbump-claude-")
	if err != nil {
		return Ambiguous, err
	}
	defer func() { _ = os.RemoveAll(scratch) }()
	cmd := exec.CommandContext(ctx, execPath, "--model", model, "-p", "ping")
	// The scratch HOME removes the caller's credentials; the dead
	// loopback endpoint keeps even a known model from reaching the API.
	// PATH stays because the CLI may shell out to helpers on startup.
	cmd.Env = []string{
		"HOME=" + scratch,
		"PATH=" + envOr("PATH", "/usr/local/bin:/usr/bin:/bin"),
		"TERM=dumb",
		"ANTHROPIC_BASE_URL=http://127.0.0.1:9",
		"ANTHROPIC_API_KEY=",
		"ANTHROPIC_AUTH_TOKEN=",
		"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1",
	}
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	runErr := cmd.Run()
	var launchErr *exec.Error
	var pathErr *fs.PathError
	if errors.As(runErr, &launchErr) || errors.As(runErr, &pathErr) {
		// The probe binary never started: PATH-lookup failure or
		// fork/exec failure. That is a setup problem, not a verdict.
		return Ambiguous, fmt.Errorf("modelbump: claude probe cannot launch %s: %w", execPath, runErr)
	}
	if ctx.Err() == context.DeadlineExceeded {
		return Ambiguous, nil
	}
	if bytes.Contains(out.Bytes(), []byte(unrecognizedModel)) {
		return Absent, nil
	}
	for _, signature := range downstreamFailure {
		if bytes.Contains(out.Bytes(), signature) {
			return Present, nil
		}
	}
	return Ambiguous, nil
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
