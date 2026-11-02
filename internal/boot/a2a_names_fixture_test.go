package boot

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/noisefloor-oss/deck/internal/config"
	"github.com/noisefloor-oss/deck/internal/db"
	"github.com/noisefloor-oss/deck/internal/provider"
	"github.com/rs/zerolog"
)

// Opt-in acceptance over the real backend, native credential-free Kimi,
// built browser UI, and standalone a2a CLI. Each run owns its database, home,
// socket, keys, and in-memory relay; the relay refuses chat writes.
// This verifies live process identity, never provider authentication/readiness.
// Build web/dist first, then set NOISEDECK_A2A_NAMES_FIXTURE=1.
const a2aNamesFixtureGate = "NOISEDECK_A2A_NAMES_FIXTURE"

func TestA2ASessionNamesPropagateThroughLiveKimi(t *testing.T) {
	if os.Getenv(a2aNamesFixtureGate) != "1" {
		t.Skipf("set %s=1 to run the name-propagation fixture (needs tmux, python3, and the real kimi CLI)", a2aNamesFixtureGate)
	}
	for _, bin := range []string{"tmux"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s not installed", bin)
		}
	}
	python3, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not installed")
	}
	kimi := "/usr/local/bin/kimi"
	if info, err := os.Stat(kimi); err != nil || info.IsDir() || info.Mode()&0o111 == 0 {
		t.Skipf("the real kimi CLI is not installed at %s", kimi)
	}
	node, err := exec.LookPath("node")
	if err != nil || os.Getenv("NOISE_BROWSER_WS") == "" {
		t.Fatal("this acceptance fixture requires node and the browser sidecar")
	}
	repo, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	sourceA2A := os.Getenv("A2A_PROBE_CLIENT")
	if sourceA2A == "" {
		sourceA2A = "/usr/local/bin/a2a"
	}
	if _, err := os.Stat(sourceA2A); err != nil {
		t.Skipf("standalone a2a client missing: %v", err)
	}
	relayScript := filepath.Join(repo, "deploy", "a2a", "relay_fixture.py")
	if _, err := os.Stat(relayScript); err != nil {
		t.Skipf("relay fixture missing: %v", err)
	}

	python3, _ = filepath.Abs(python3)
	root := t.TempDir()
	home := filepath.Join(root, "home")
	kimiHome := filepath.Join(home, ".kimi-code")
	a2aState := filepath.Join(root, "a2a-state")
	a2aKeys := filepath.Join(root, "a2a-keys")
	fixtureBin := filepath.Join(root, "bin")
	for _, dir := range []string{home, kimiHome, a2aState, a2aKeys, fixtureBin} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	shim := filepath.Join(fixtureBin, "a2a")
	if err := os.WriteFile(shim, []byte("#!/bin/sh\nexec "+python3+" "+sourceA2A+" \"$@\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	kimiHookConfig := filepath.Join(kimiHome, "config.toml")
	if err := os.WriteFile(kimiHookConfig, []byte(
		"[[hooks]]\nevent = \"SessionStart\"\ncommand = \""+provider.HookReceiverPath+"\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	relayCtx, relayCancel := context.WithCancel(context.Background())
	defer relayCancel()
	relay := exec.CommandContext(relayCtx, python3, relayScript, "--port", "0")
	relay.Env = []string{"PATH=/usr/local/bin:/usr/bin:/bin", "HOME=" + home}
	relay.Stderr = io.Discard
	relayStdout, err := relay.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := relay.Start(); err != nil {
		t.Fatalf("start relay fixture: %v", err)
	}
	t.Cleanup(func() {
		_ = relay.Process.Kill()
		_, _ = relay.Process.Wait()
	})
	ready := make(chan string, 1)
	go func() {
		var line string
		if _, err := fmt.Fscanf(relayStdout, "READY %s\n", &line); err == nil {
			ready <- line
		}
		close(ready)
	}()
	var relayURL string
	select {
	case line, ok := <-ready:
		if !ok || line == "" {
			t.Fatal("relay fixture printed no ready line")
		}
		relayURL = "http://127.0.0.1:" + line + "/mcp"
		t.Logf("relay fixture listening on %s", relayURL)
		go func() { _, _ = io.Copy(io.Discard, relayStdout) }()
	case <-time.After(10 * time.Second):
		t.Fatal("relay fixture never became ready")
	}

	socketDir, err := os.MkdirTemp("/tmp", "a2a-names-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(socketDir) })
	tmuxSocket := filepath.Join(socketDir, "ac.sock")

	stream := "a2a-names-fixture-" + filepath.Base(root)
	t.Setenv("HOME", home)
	t.Setenv("KIMI_CODE_HOME", kimiHome)
	t.Setenv("PATH", fixtureBin+":/usr/local/bin:/usr/bin:/bin")
	t.Setenv("A2A_MCP_URL", relayURL)
	t.Setenv("A2A_MCP_TOKEN", "")
	t.Setenv("A2A_HANDLE", "")
	t.Setenv("A2A_STREAM", stream)
	t.Setenv("A2A_STATE_DIR", a2aState)
	t.Setenv("A2A_KEY_DIR", a2aKeys)
	t.Setenv("A2A_TMUX_SOCKETS", tmuxSocket)
	t.Setenv("A2A_NO_COURIER", "1")

	fingerprint := runA2AClient(t, shim, "key", "init")
	if !strings.Contains(fingerprint, "-") {
		t.Fatalf("key init printed %q, want a fingerprint in dash groups", fingerprint)
	}

	cfg := testConfig(t)
	cfg.Auth.Mode = "none"
	cfg.Tmux.SocketPath = tmuxSocket
	cfg.Projects.AllowedRoots = []string{root}
	cfg.Providers.Kimi = config.Provider{
		Executable:   kimi,
		HookConfig:   kimiHookConfig,
		HookReceiver: provider.HookReceiverPath,
	}
	cfg.Chats = config.Chats{Executable: shim}

	uiListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = uiListener.Close() })
	uiOrigin := "http://" + uiListener.Addr().String()
	cfg.Server.AllowedOrigins = append(cfg.Server.AllowedOrigins, uiOrigin)

	stopFixtureTmuxServer(t, cfg.Tmux.SocketPath)
	var bootLog bytes.Buffer
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("backend log:\n%s", bootLog.String())
		}
	})
	h := upOrFailLogging(t, cfg, &bootLog)
	base, stopBackend := startServer(t, h.Run, h.Server.BoundAddr)
	_ = serveA2ANamesUI(t, uiListener, base, filepath.Join(repo, "web", "dist"))

	projectID := seedA2ANamesProject(t, h, filepath.Join(root, "project"))
	label := "primer watch"

	sessionA := createKimiSession(t, base, projectID)
	sessionB := createKimiSession(t, base, projectID)
	t.Setenv("NOISEDECK_SESSION_ID", sessionA.ID)
	t.Setenv("NOISEDECK_PROJECT_ID", projectID)
	projectName := "a2a names fixture"

	var contract []string
	remember := func(stage, sessionID, handle string, row rosterRow, wantLabel bool, label string) {
		t.Helper()
		if row == nil {
			contract = append(contract, fmt.Sprintf("%s: the source a2a roster has no row for deck session %s", stage, sessionID))
			return
		}
		contract = append(contract, routingHandleProblems(stage, row, sessionID, handle)...)
		if row["project_id"] != projectID || row["project_name"] != projectName {
			contract = append(contract, fmt.Sprintf("%s: session %s has wrong project metadata: %v", stage, sessionID, row))
		}
		var registered struct {
			ProjectID   string `json:"project_id"`
			ProjectName string `json:"project_name"`
		}
		raw, err := os.ReadFile(filepath.Join(a2aState, "session-label-"+sessionID+".json"))
		if err != nil || json.Unmarshal(raw, &registered) != nil || registered.ProjectID != projectID || registered.ProjectName != projectName {
			contract = append(contract, fmt.Sprintf("%s: session %s has missing or stale local project registration", stage, sessionID))
		}
		if wantLabel && !rowCarriesLabel(row, label) {
			contract = append(contract, fmt.Sprintf("%s: session %s does not carry its label %q beside the routing handle: %v", stage, sessionID, label, row))
		}
		if !wantLabel && rowCarriesLabel(row, label) {
			contract = append(contract, fmt.Sprintf("%s: session %s still carries label %q after the label was unset: %v", stage, sessionID, label, row))
		}
	}
	wantA, wantB := "kimi-"+strings.ToLower(sessionA.ID[:4]), "kimi-"+strings.ToLower(sessionB.ID[:4])
	if wantA == wantB {
		t.Fatalf("fixture session ids share a routing handle prefix: %s", wantA)
	}

	rows := awaitRosterRows(t, shim, 20*time.Second, sessionA.ID, sessionB.ID)
	remember("launch synchronization", sessionA.ID, wantA, rosterRowForSession(rows, sessionA.ID), false, label)
	remember("launch synchronization", sessionB.ID, wantB, rosterRowForSession(rows, sessionB.ID), false, label)

	if _, err := os.Stat(filepath.Join(repo, "web", "dist", "index.html")); err != nil {
		t.Fatal("build web/dist before running the panel acceptance fixture")
	}
	runNamesProbe(t, node, filepath.Join(repo, "web"), uiOrigin, projectID, sessionA.ID, sessionB.ID, label)

	if got := sessionDisplayName(t, base, sessionA.ID); got != label {
		t.Fatalf("session %s display name = %q, want %q", sessionA.ID, got, label)
	}
	rows = awaitRosterRows(t, shim, 10*time.Second, sessionA.ID, sessionB.ID)
	remember("panel rename", sessionA.ID, wantA, rosterRowForSession(rows, sessionA.ID), true, label)
	if !rosterBlockHasLabel(runA2AClient(t, shim, "who"), wantA, label) {
		t.Fatal("a2a who text did not associate the panel label with its routing handle")
	}

	renameSession(t, base, sessionB.ID, label)
	renameSession(t, base, sessionA.ID, "")

	rows = awaitRosterRows(t, shim, 10*time.Second, sessionA.ID, sessionB.ID)
	remember("rename synchronization (label unset)", sessionA.ID, wantA, rosterRowForSession(rows, sessionA.ID), false, label)
	remember("rename synchronization (duplicate label)", sessionB.ID, wantB, rosterRowForSession(rows, sessionB.ID), true, label)

	assertChatsReadPath(t, base, sessionA.ID, wantA)
	assertChatsReadPath(t, base, sessionB.ID, wantB)

	projectName = "Renamed A2A project"
	req, err := http.NewRequest(http.MethodPost, base+"/api/projects/"+projectID+"/rename", strings.NewReader(`{"name":"Renamed A2A project"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("rename fixture project = %d", resp.StatusCode)
	}
	rows = awaitRosterRows(t, shim, 10*time.Second, sessionA.ID, sessionB.ID)
	remember("project rename synchronization", sessionA.ID, wantA, rosterRowForSession(rows, sessionA.ID), false, label)
	remember("project rename synchronization", sessionB.ID, wantB, rosterRowForSession(rows, sessionB.ID), true, label)

	stopBackend()
	h.Close()
	if err := os.Remove(filepath.Join(a2aState, "session-label-"+sessionB.ID+".json")); err != nil {
		t.Fatalf("remove only fixture label metadata before restoration: %v", err)
	}
	restored := upOrFailLogging(t, cfg, &bootLog)
	restoredBase := serve(t, restored)
	if got := sessionDisplayName(t, restoredBase, sessionB.ID); got != label {
		t.Fatalf("after backend restart, session %s display name = %q, want %q", sessionB.ID, got, label)
	}
	if got := sessionDisplayName(t, restoredBase, sessionA.ID); got != "" {
		t.Fatalf("after backend restart, session %s display name = %q, want empty", sessionA.ID, got)
	}
	if _, err := restored.Sessions.Get(context.Background(), sessionB.ID); err != nil {
		t.Fatalf("restored backend cannot read session %s: %v", sessionB.ID, err)
	}
	rows = awaitRosterRows(t, shim, 10*time.Second, sessionA.ID, sessionB.ID)
	remember("restore synchronization", sessionA.ID, wantA, rosterRowForSession(rows, sessionA.ID), false, label)
	remember("restore synchronization", sessionB.ID, wantB, rosterRowForSession(rows, sessionB.ID), true, label)

	for _, gap := range contract {
		t.Errorf("name-propagation contract: %s", gap)
	}
	if len(contract) == 0 {
		t.Log("the source a2a roster carried every label and stable project ID, through session and project rename, duplicate, unset, and backend restart")
	}
}

func upOrFailLogging(t *testing.T, cfg *config.Config, log io.Writer) *Handles {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	h, err := Up(context.Background(), cfg, zerolog.New(zerolog.SyncWriter(log)))
	if err != nil {
		t.Fatalf("Up: %v", err)
	}
	t.Cleanup(h.Close)
	return h
}

func runA2AClient(t *testing.T, shim string, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, shim, args...)
	cmd.Env = os.Environ()
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("a2a %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

type rosterRow map[string]any

func (r rosterRow) handle() string {
	if v, ok := r["handle"].(string); ok {
		return v
	}
	return ""
}

func rosterRows(t *testing.T, shim string) map[string]rosterRow {
	t.Helper()
	runA2AClient(t, shim, "courier", "--once")
	raw := runA2AClient(t, shim, "who", "--json")
	var rows map[string]rosterRow
	if err := json.Unmarshal([]byte(raw), &rows); err != nil {
		t.Fatalf("a2a who --json: %v\n%s", err, raw)
	}
	return rows
}

func rosterHandles(rows map[string]rosterRow) []string {
	handles := make([]string, 0, len(rows))
	for handle := range rows {
		handles = append(handles, handle)
	}
	return handles
}

func rosterRowForSession(rows map[string]rosterRow, sessionID string) rosterRow {
	for _, row := range rows {
		if deck, ok := row["deck_session"].(string); ok && deck == sessionID {
			return row
		}
	}
	return nil
}

func awaitRosterRows(t *testing.T, shim string, bound time.Duration, sessionIDs ...string) map[string]rosterRow {
	t.Helper()
	deadline := time.Now().Add(bound)
	var rows map[string]rosterRow
	for {
		rows = rosterRows(t, shim)
		complete := true
		for _, id := range sessionIDs {
			if rosterRowForSession(rows, id) == nil {
				complete = false
			}
		}
		if complete || time.Now().After(deadline) {
			if !complete {
				t.Logf("roster incomplete after %s; handles so far: %v", bound, rosterHandles(rows))
			}
			return rows
		}
		time.Sleep(500 * time.Millisecond)
	}
}

func routingHandleProblems(stage string, row rosterRow, sessionID, want string) []string {
	if got := row.handle(); got != want {
		return []string{fmt.Sprintf("%s: session %s routed under handle %q, want the derived %q (display labels never become routing handles): %v", stage, sessionID, got, want, row)}
	}
	if state, _ := row["state"].(string); state == "unverified" {
		return []string{fmt.Sprintf("%s: session %s presence is unverified (unsigned or foreign); a courier could act on it: %v", stage, sessionID, row)}
	}
	return nil
}

func rosterBlockHasLabel(text, handle, label string) bool {
	quoted, _ := json.Marshal(label)
	for _, block := range strings.Split(text, "\n\n") {
		lines := strings.Split(block, "\n")
		if lines[0] != handle && lines[0] != handle+" (you)" {
			continue
		}
		for _, line := range lines[1:] {
			if strings.TrimSpace(line) == "Name: "+string(quoted) || strings.HasPrefix(strings.TrimSpace(line), "Name: "+string(quoted)+"  |  ") {
				return true
			}
		}
	}
	return false
}

func TestRosterLabelStaysInItsHandleBlock(t *testing.T) {
	text := "Reachable agents\n\nfirst\n  Name: \"Other\"\n\nsecond\n  Name: \"Target\"  |  Harness: kimi\n\n"
	if rosterBlockHasLabel(text, "first", "Target") || !rosterBlockHasLabel(text, "second", "Target") {
		t.Fatal("roster labels must belong to the selected handle's block")
	}
}

func rowCarriesLabel(row rosterRow, label string) bool {
	return row["display_name"] == label
}

func seedA2ANamesProject(t *testing.T, h *Handles, workDir string) string {
	t.Helper()
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		t.Fatal(err)
	}
	const projectID = "a2a-names-project"
	err := h.DB.WithTx(context.Background(), func(ctx context.Context, tx *sql.Tx) error {
		const profileID = "a2a00000000000000000000000000641"
		now := db.FormatTime(time.Now())
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO git_identity_profiles (id, name, email, revision, created_at, updated_at)
			VALUES (?, 'A2A Names Fixture', 'a2a-names-fixture@example.invalid', 1, ?, ?)`, profileID, now, now); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE git_identity_catalog SET default_profile_id = ?, revision = 1, updated_at = ?
			WHERE singleton = 1`, profileID, now); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO projects (
				id, name, canonical_path, git_author_name, git_author_email,
				git_identity_profile_id, git_identity_profile_revision, git_identity_setup_revision,
				created_at, last_used_at
			) VALUES (?, 'a2a names fixture', ?, 'A2A Names Fixture', 'a2a-names-fixture@example.invalid', ?, 1, 1, ?, ?)`,
			projectID, workDir, profileID, now, now); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `
			INSERT INTO project_git_identity_snapshots (
				project_id, revision, profile_id, profile_revision, name, email, created_at
			) VALUES (?, 1, ?, 1, 'A2A Names Fixture', 'a2a-names-fixture@example.invalid', ?)`, projectID, profileID, now)
		return err
	})
	if err != nil {
		t.Fatalf("seed fixture project: %v", err)
	}
	return projectID
}

type a2aNamesSession struct {
	ID     string `json:"id"`
	Status string `json:"status"`
}

func createKimiSession(t *testing.T, base, projectID string) a2aNamesSession {
	t.Helper()
	body := `{"provider":"kimi","cols":100,"rows":30}`
	req, err := http.NewRequest(http.MethodPost, base+"/api/projects/"+projectID+"/sessions", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("create kimi session: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create kimi session = %d (%s)", resp.StatusCode, raw)
	}
	var created a2aNamesSession
	if err := json.Unmarshal(raw, &created); err != nil {
		t.Fatalf("decode created session: %v (%s)", err, raw)
	}
	deadline := time.Now().Add(90 * time.Second)
	var panePID int
	for {
		var session struct {
			a2aNamesSession
			PanePID *int `json:"pane_pid"`
		}
		if code, body := apiGet(t, base+"/api/sessions/"+created.ID); code != http.StatusOK {
			t.Fatalf("session %s status = %d (%s)", created.ID, code, body)
		} else if err := json.Unmarshal([]byte(body), &session); err != nil {
			t.Fatalf("decode session %s: %v (%s)", created.ID, err, body)
		} else if session.Status == "running" && session.PanePID != nil {
			panePID = *session.PanePID
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("kimi session %s never reached running (status=%q)", created.ID, session.Status)
		}
		time.Sleep(250 * time.Millisecond)
	}
	assertKimiProcessFacts(t, created.ID, panePID)
	return created
}

func assertKimiProcessFacts(t *testing.T, sessionID string, pid int) {
	t.Helper()
	rawEnv, err := os.ReadFile(fmt.Sprintf("/proc/%d/environ", pid))
	if err != nil {
		t.Fatal(err)
	}
	env := make(map[string]string)
	for _, entry := range strings.Split(string(rawEnv), "\x00") {
		key, value, ok := strings.Cut(entry, "=")
		if ok {
			env[key] = value
		}
	}
	if env["HOME"] != os.Getenv("HOME") || env["KIMI_CODE_HOME"] != os.Getenv("KIMI_CODE_HOME") || env["NOISEDECK_SESSION_ID"] != sessionID {
		t.Fatal("native process did not receive the isolated home and exact Deck session identity")
	}
	for _, key := range []string{"KIMI_API_KEY", "MOONSHOT_API_KEY", "OPENAI_API_KEY", "ANTHROPIC_API_KEY", "GH_TOKEN", "GITHUB_TOKEN", "A2A_MCP_TOKEN"} {
		if env[key] != "" {
			t.Fatalf("native fixture process inherited credential variable %s", key)
		}
	}
	exe, err := os.Readlink(fmt.Sprintf("/proc/%d/exe", pid))
	if err != nil {
		t.Fatalf("session %s pane pid %d has no kernel executable: %v", sessionID, pid, err)
	}
	if !strings.Contains(exe, "node") {
		t.Fatalf("session %s pane runs %q, want Kimi's node interpreter", sessionID, exe)
	}
	cmdline, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if err != nil {
		t.Fatalf("session %s pane pid %d argv unreadable: %v", sessionID, pid, err)
	}
	if !strings.Contains(strings.ReplaceAll(string(cmdline), "\x00", " "), "kimi") {
		t.Fatalf("session %s pane argv does not name kimi: %q", sessionID, cmdline)
	}
	t.Logf("session %s: live kimi process pid %d (%s)", sessionID, pid, exe)
}

func apiGet(t *testing.T, url string) (int, string) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

func renameSession(t *testing.T, base, sessionID, name string) {
	t.Helper()
	body := fmt.Sprintf(`{"name":%q}`, name)
	req, err := http.NewRequest(http.MethodPost, base+"/api/sessions/"+sessionID+"/rename", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("rename session %s to %q: %v", sessionID, name, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("rename session %s to %q = %d (%s)", sessionID, name, resp.StatusCode, raw)
	}
}

func sessionDisplayName(t *testing.T, base, sessionID string) string {
	t.Helper()
	code, body := apiGet(t, base+"/api/sessions/"+sessionID)
	if code != http.StatusOK {
		t.Fatalf("session %s = %d (%s)", sessionID, code, body)
	}
	var session struct {
		DisplayName string `json:"display_name"`
	}
	if err := json.Unmarshal([]byte(body), &session); err != nil {
		t.Fatalf("decode session %s: %v (%s)", sessionID, err, body)
	}
	return session.DisplayName
}

func assertChatsReadPath(t *testing.T, base, sessionID, wantHandle string) {
	t.Helper()
	code, body := apiGet(t, base+"/api/sessions/"+sessionID+"/chats")
	if code != http.StatusOK {
		t.Fatalf("chats read for %s = %d (%s)", sessionID, code, body)
	}
	var ledger struct {
		Agent string `json:"agent"`
	}
	if err := json.Unmarshal([]byte(body), &ledger); err != nil {
		t.Fatalf("decode chats for %s: %v (%s)", sessionID, err, body)
	}
	if ledger.Agent != wantHandle {
		t.Fatalf("chats read for %s names agent %q, want the routing handle %q", sessionID, ledger.Agent, wantHandle)
	}
}

func serveA2ANamesUI(t *testing.T, listener net.Listener, backend, distDir string) string {
	t.Helper()
	target, err := url.Parse(backend)
	if err != nil {
		t.Fatal(err)
	}
	proxy := httputil.NewSingleHostReverseProxy(target)
	files := http.FileServer(http.Dir(distDir))
	mux := http.NewServeMux()
	mux.Handle("/api/", proxy)
	mux.Handle("/auth/", proxy)
	mux.Handle("/", files)
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close() })
	return "http://" + listener.Addr().String()
}

func runNamesProbe(t *testing.T, node, webDir, origin, projectID, sessionA, sessionB, label string) {
	t.Helper()
	ready, err := json.Marshal(struct {
		Origin     string `json:"origin"`
		ProjectID  string `json:"project_id"`
		SessionA   string `json:"session_a"`
		SessionB   string `json:"session_b"`
		Label      string `json:"label"`
		Provider   string `json:"provider"`
		TimeoutSec int    `json:"timeout_sec"`
	}{origin, projectID, sessionA, sessionB, label, "kimi", 180})
	if err != nil {
		t.Fatal(err)
	}
	readyPath := filepath.Join(t.TempDir(), "a2a-names-probe.json")
	if err := os.WriteFile(readyPath, ready, 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, node, "tools/a2a-names-probe.mjs", readyPath)
	cmd.Dir = webDir
	cmd.Env = os.Environ()
	out, err := cmd.CombinedOutput()
	t.Logf("a2a-names probe:\n%s", out)
	if err != nil {
		t.Fatalf("the panel rename probe failed: %v", err)
	}
}

func stopFixtureTmuxServer(t *testing.T, socketPath string) {
	t.Helper()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = exec.CommandContext(ctx, "tmux", "-S", socketPath, "kill-server").Run()
	})
}
