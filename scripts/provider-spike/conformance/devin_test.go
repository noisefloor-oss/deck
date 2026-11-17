//go:build linux

package conformance

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/noisefloor-oss/deck/internal/provider/nativeid"
	"github.com/noisefloor-oss/deck/internal/tmux"
)

const realDevinEnv = "NOISEDECK_REAL_DEVIN"

type devinHookEvent struct {
	HookEventName string `json:"hook_event_name"`
	SessionID     string `json:"session_id"`
	Source        string `json:"source"`
}

type devinHarness struct {
	t          *testing.T
	dir        string
	project    string
	executable string
	// version is the exact CLI version this run measures: the contract's
	// version_tested by default, or the candidate a bump names.
	version  string
	client   *tmux.Client
	listener net.Listener
	events   chan devinHookEvent
	errMu    sync.Mutex
	hookErr  error
	panes    []*tmux.PaneInfo
}

// TestDevinProviderContract is an opt-in black-box contract test for the real
// authenticated CLI. Identity evidence comes only from Devin's documented
// SessionStart and UserPromptSubmit hooks. The test never captures or parses terminal output,
// provider state, session lists, transcripts, or provider logs.
func TestDevinProviderContract(t *testing.T) {
	if os.Getenv(realDevinEnv) != "1" {
		t.Skip("real Devin contract probe is opt-in")
	}
	h := newDevinHarness(t)
	ctx := context.Background()

	fresh := h.launch(ctx, "devin-fresh", 1, nil)
	startup := h.waitEvent(30 * time.Second)
	h.requireEvent(startup, "startup", "")
	// SessionStart precedes the TUI accepting input. A bounded startup settle
	// keeps this black-box probe from racing the documented lifecycle event;
	// product code never sends an automatic prompt.
	time.Sleep(60 * time.Second)

	start := filepath.Join(h.dir, "active-start")
	end := filepath.Join(h.dir, "active-end")
	queued := filepath.Join(h.dir, "active-queued")
	// The first turn must stay active across the paste settle, the Enter,
	// and the prompt hook's own latency, or the mid-turn assertion below
	// measures a post-turn submission instead of queuing.
	firstCommand := "sh -c " + shellQuote("printf x > "+shellQuote(start)+"; sleep 25; printf x > "+shellQuote(end))
	// The queued command records its own ordering evidence: it appends x
	// only when the first command's end sentinel already exists, and the
	// distinct byte E when it ran early. The assertion below then needs no
	// racy cross-file polling to prove the queue held until the turn ended.
	queuedCommand := "sh -c " + shellQuote(
		"if [ -f "+shellQuote(end)+" ]; then printf x >> "+shellQuote(queued)+"; else printf E >> "+shellQuote(queued)+"; fi")
	h.submit(fresh.PaneID, "Run exactly this shell command and wait for it to finish:\n"+firstCommand+"\nDo not run any other command.")
	firstPrompt := h.waitEvent(30 * time.Second)
	h.requirePromptEvent(firstPrompt, startup.SessionID)
	h.waitFile(start, 120*time.Second)
	if fileExists(end) {
		t.Fatal("active-turn sentinel finished before the second submission")
	}
	h.submit(fresh.PaneID, "After the current command finishes, run exactly this shell command once:\n"+queuedCommand)
	// 3000.5.20 and 3000.6.14 accept input during the active turn and queue
	// it behind the running turn (3000.4.25 rejected it outright). The queued
	// message must belong to the same attested session and must execute
	// exactly once, after the active command has finished.
	// 3000.13.4 fires UserPromptSubmit when it accepts a mid-turn message;
	// the command still has to run exactly once after the active turn ends.
	// Require the event while the first command is still active; an event
	// that arrives only after completion is the older at-dequeue contract.
	// The sentinels independently prove execution order.
	early, ok := h.waitOptionalEvent(10 * time.Second)
	if !ok {
		t.Fatal("UserPromptSubmit did not fire while the first command was active")
	}
	h.requirePromptEvent(early, startup.SessionID)
	if fileExists(end) {
		t.Fatal("the first turn ended inside the quiet window; queuing was not exercised")
	}
	h.waitFile(end, 60*time.Second)
	h.waitFile(queued, 120*time.Second)
	time.Sleep(20 * time.Second)
	data, err := os.ReadFile(queued)
	if err != nil || string(data) != "x" {
		t.Fatalf("queued input did not execute exactly once after the turn ended: bytes=%d early=%t",
			len(data), strings.ContainsRune(string(data), 'E'))
	}
	h.stop(fresh)

	resumed := h.launch(ctx, "devin-resume", 2, []string{"--resume", startup.SessionID})
	if resumeEvent, ok := h.waitOptionalEvent(15 * time.Second); ok {
		t.Fatalf("3000.13.4 unexpectedly emitted resume SessionStart: source=%q", resumeEvent.Source)
	}
	h.submit(resumed.PaneID, "Reply with exactly: NOISEDECK-RESUME-OK.")
	resumePrompt := h.waitEvent(30 * time.Second)
	h.requirePromptEvent(resumePrompt, startup.SessionID)
	h.stop(resumed)

	unknownID := differentDevinID(startup.SessionID)
	unknown := h.launch(ctx, "devin-unknown", 3, []string{"--resume", unknownID})
	fallback := h.waitEvent(30 * time.Second)
	h.requireEvent(fallback, "startup", "")
	if fallback.SessionID == startup.SessionID || fallback.SessionID == unknownID {
		t.Fatal("unknown-id fallback did not allocate a distinct fresh identity")
	}
	h.stop(unknown)

	t.Logf(`devin_contract={"version":%q,"hook_identity":true,"exact_resume":true,"unknown_id_fallback":"startup_new_id","bracketed_paste":true,"multiline":true,"active_turn":"queued","prompt_hook_timing":"at_accept","sigterm":"signal_15","terminal_output_parsing":false}`, h.version)
}

func newDevinHarness(t *testing.T) *devinHarness {
	t.Helper()
	requireCommand(t, "git")
	requireCommand(t, "python3")
	requireCommand(t, "tmux")
	executable := os.Getenv("NOISEDECK_DEVIN_EXECUTABLE")
	if executable == "" {
		executable = "/usr/local/bin/devin"
	}
	if !filepath.IsAbs(executable) {
		t.Fatal("Devin executable must be absolute")
	}
	// The version this run measures is the contract's, so the probe and the
	// contract cannot drift: a bump names its candidate through the
	// environment, and a pass moves version_tested — never the reverse.
	wantVersion := os.Getenv(devinExpectedVersionEnv)
	if wantVersion == "" {
		wantVersion = devinContractVersion(t)
	}
	version, err := exec.Command(executable, "--version").Output()
	// The whole shape, not a token: "devin <version> (<build>)" and nothing
	// else, with the version compared exactly so 3000.5.2 cannot satisfy
	// 3000.5.20. The build is what the contract's build_tested records, so a
	// report without one is not a report this run can be attributed to.
	if err != nil || !devinVersionLine(string(version), wantVersion) {
		t.Fatalf("Devin executable reports %q, not \"devin %s (<build>)\"",
			strings.TrimSpace(string(version)), wantVersion)
	}
	dir, err := os.MkdirTemp(devinFixtureRoot(t), ".dv-")
	if err != nil {
		t.Fatal("create private Devin fixture")
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal("secure private Devin fixture")
	}
	h := &devinHarness{
		t: t, dir: dir, project: filepath.Join(dir, "project"),
		executable: executable, version: wantVersion, events: make(chan devinHookEvent, 8),
	}
	t.Cleanup(h.cleanup)
	if err := os.MkdirAll(filepath.Join(h.project, ".devin"), 0o700); err != nil {
		t.Fatal("create private Devin project")
	}
	if out, err := exec.Command("git", "init", "--quiet", h.project).CombinedOutput(); err != nil {
		t.Fatalf("initialize private Devin project: output_bytes=%d", len(out))
	}
	socket := filepath.Join(dir, "hook.sock")
	h.listener, err = net.Listen("unix", socket)
	if err != nil {
		t.Fatal("open private Devin hook sink")
	}
	if err := os.Chmod(socket, 0o600); err != nil {
		t.Fatal("secure private Devin hook sink")
	}
	go h.serveHooks()
	hookScript := filepath.Join(dir, "hook.py")
	if err := os.WriteFile(hookScript, []byte(devinHookProgram), 0o700); err != nil {
		t.Fatal("write private Devin hook receiver")
	}
	hookEntry := []any{map[string]any{
		"hooks": []any{map[string]any{"type": "command", "command": "python3 " + shellQuote(hookScript) + " " + shellQuote(socket)}},
	}}
	hooks := map[string]any{"SessionStart": hookEntry, "UserPromptSubmit": hookEntry}
	raw, err := json.MarshalIndent(hooks, "", "  ")
	if err != nil {
		t.Fatal("marshal private Devin hook config")
	}
	if err := os.WriteFile(filepath.Join(h.project, ".devin", "hooks.v1.json"), raw, 0o600); err != nil {
		t.Fatal("write private Devin hook config")
	}
	h.client, err = tmux.NewClient(tmux.Config{SocketPath: filepath.Join(dir, "tmux.sock")})
	if err != nil {
		t.Fatal("construct private Devin tmux client")
	}
	return h
}

func (h *devinHarness) launch(ctx context.Context, name string, generation int, prefix []string) *tmux.PaneInfo {
	h.t.Helper()
	args := []string{h.executable}
	args = append(args, prefix...)
	args = append(args, "--respect-workspace-trust", "false", "--permission-mode", "dangerous")
	pane, err := h.client.CreateSession(ctx, tmux.SessionSpec{
		Name: name, WorkDir: h.project, Argv: args,
		Generation: generation, Marker: name + "-marker",
	})
	if err != nil {
		h.t.Fatalf("launch real Devin in private tmux namespace: %v", sanitizedErr(err))
	}
	h.panes = append(h.panes, pane)
	return pane
}

func (h *devinHarness) submit(paneID, body string) {
	h.t.Helper()
	if err := h.client.SendBracketedPaste(context.Background(), paneID, body); err != nil {
		h.t.Fatalf("send bracketed Devin input: %v", sanitizedErr(err))
	}
	// The 3000.5.20 TUI swallowed an Enter that landed while it was still
	// processing a bracketed paste: the body stayed in the composer and the
	// prompt never submitted. A bounded settle keeps the paste and the
	// submission as two inputs the way a human's are; this probe always
	// settles, so it measures that the settled sequence submits, never that
	// an immediate Enter still fails.
	time.Sleep(time.Second)
	if err := h.client.SendKey(context.Background(), paneID, "Enter"); err != nil {
		h.t.Fatalf("submit Devin input: %v", sanitizedErr(err))
	}
}

func (h *devinHarness) waitEvent(timeout time.Duration) devinHookEvent {
	h.t.Helper()
	select {
	case event := <-h.events:
		return event
	case <-time.After(timeout):
		h.errMu.Lock()
		err := h.hookErr
		h.errMu.Unlock()
		h.t.Fatalf("documented SessionStart hook timed out: receiver_error=%t", err != nil)
		return devinHookEvent{}
	}
}

func (h *devinHarness) waitOptionalEvent(timeout time.Duration) (devinHookEvent, bool) {
	select {
	case event := <-h.events:
		return event, true
	case <-time.After(timeout):
		return devinHookEvent{}, false
	}
}

func (h *devinHarness) requireEvent(event devinHookEvent, source, exactID string) {
	h.t.Helper()
	if event.HookEventName != "SessionStart" || event.Source != source || !nativeid.IsDevinSession(event.SessionID) {
		h.t.Fatalf("invalid documented SessionStart event: event=%t source=%q expected_source=%q id_shape=%t",
			event.HookEventName == "SessionStart", event.Source, source, nativeid.IsDevinSession(event.SessionID))
	}
	if exactID != "" && event.SessionID != exactID {
		h.t.Fatal("exact resume hook did not attest the requested native identity")
	}
}

func (h *devinHarness) requirePromptEvent(event devinHookEvent, exactID string) {
	h.t.Helper()
	if event.HookEventName != "UserPromptSubmit" || event.SessionID != exactID {
		h.t.Fatalf("invalid documented UserPromptSubmit event: event=%t exact_identity=%t",
			event.HookEventName == "UserPromptSubmit", event.SessionID == exactID)
	}
}

func (h *devinHarness) serveHooks() {
	for {
		conn, err := h.listener.Accept()
		if err != nil {
			if !errors.Is(err, net.ErrClosed) {
				h.recordHookErr(err)
			}
			return
		}
		var event devinHookEvent
		err = json.NewDecoder(bufio.NewReader(conn)).Decode(&event)
		_ = conn.Close()
		if err != nil {
			h.recordHookErr(err)
			continue
		}
		h.events <- event
	}
}

func (h *devinHarness) recordHookErr(err error) {
	h.errMu.Lock()
	if h.hookErr == nil {
		h.hookErr = err
	}
	h.errMu.Unlock()
}

func (h *devinHarness) waitFile(path string, timeout time.Duration) {
	h.t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if fileExists(path) {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	h.t.Fatal("timed out waiting for a synthetic provider-spike marker")
}

func (h *devinHarness) stop(spawn *tmux.PaneInfo) {
	h.t.Helper()
	info, err := h.client.PaneInfoByID(context.Background(), spawn.PaneID)
	if err != nil {
		h.t.Fatalf("observe private Devin pane before stop: %v", sanitizedErr(err))
	}
	if info.Dead {
		return
	}
	if info.PID != spawn.PID || info.StartTicks != spawn.StartTicks || info.ProcessGroupID != info.PID || info.PID <= 1 {
		h.t.Fatal("refusing to signal a changed or non-private Devin process group")
	}
	if err := syscall.Kill(-info.ProcessGroupID, syscall.SIGTERM); err != nil {
		h.t.Fatalf("stop exact private Devin process group: %v", err)
	}
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		observed, observeErr := h.client.PaneInfoByID(context.Background(), spawn.PaneID)
		if observeErr == nil && observed.Dead {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	h.t.Fatal("private Devin pane did not stop after SIGTERM")
}

func (h *devinHarness) cleanup() {
	if h.listener != nil {
		_ = h.listener.Close()
	}
	if h.client != nil {
		for _, spawn := range h.panes {
			info, err := h.client.PaneInfoByID(context.Background(), spawn.PaneID)
			if err == nil && !info.Dead && info.PID == spawn.PID && info.StartTicks == spawn.StartTicks &&
				info.ProcessGroupID == info.PID && info.PID > 1 {
				_ = syscall.Kill(-info.ProcessGroupID, syscall.SIGKILL)
			}
		}
		_ = h.client.KillServer(context.Background())
	}
	if strings.HasPrefix(filepath.Base(h.dir), ".dv-") {
		_ = os.RemoveAll(h.dir)
	}
}

func differentDevinID(id string) string {
	if id[0] == 'a' {
		return "b" + id[1:]
	}
	return "a" + id[1:]
}

const devinHookProgram = `#!/usr/bin/env python3
import json
import socket
import sys

payload = json.load(sys.stdin)
record = {
    "hook_event_name": payload.get("hook_event_name"),
    "session_id": payload.get("session_id"),
    "source": payload.get("source") if isinstance(payload.get("source"), str) else "",
}
with socket.socket(socket.AF_UNIX, socket.SOCK_STREAM) as sink:
    sink.connect(sys.argv[1])
    sink.sendall((json.dumps(record, separators=(",", ":")) + "\n").encode("utf-8"))
`

// devinExpectedVersionEnv names the candidate version a bump is validating.
// Unset, the run measures the version the contract already records.
const devinExpectedVersionEnv = "NOISEDECK_DEVIN_EXPECTED_VERSION"

// devinContractVersion reads version_tested from the provider contract, the
// one place the measured version lives.
func devinContractVersion(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(repoRoot(t), "docs", "provider-contracts", "devin.yaml"))
	if err != nil {
		t.Fatalf("read the Devin contract: %v", err)
	}
	var contract struct {
		VersionTested string `yaml:"version_tested"`
	}
	if err := yaml.Unmarshal(raw, &contract); err != nil || contract.VersionTested == "" {
		t.Fatal("the Devin contract records no version_tested")
	}
	return contract.VersionTested
}

// devinFixtureRoot is where the private fixture lives: inside the checkout
// under a gitignored directory, so a run from a worktree never writes beside
// another lane's, and an aborted run leaves nothing outside the repository.
func devinFixtureRoot(t *testing.T) string {
	t.Helper()
	root := filepath.Join(repoRoot(t), ".conformance")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatalf("create the conformance fixture root: %v", err)
	}
	return root
}

// repoRoot is the checkout this test file belongs to, three levels up from
// scripts/provider-spike/conformance.
func repoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate the conformance package")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", "..", ".."))
}

// devinVersionLine reports whether a --version line is exactly
// "devin <want> (<build>)" with a non-empty build: the version compared as a
// whole token, nothing before, nothing after.
func devinVersionLine(line, want string) bool {
	fields := strings.Fields(line)
	if len(fields) != 3 || fields[0] != "devin" || fields[1] != want {
		return false
	}
	build := fields[2]
	return len(build) > 2 && strings.HasPrefix(build, "(") && strings.HasSuffix(build, ")")
}
