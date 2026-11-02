package boot

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/rs/zerolog"

	"github.com/noisefloor-oss/deck/internal/auth"
	"github.com/noisefloor-oss/deck/internal/config"
	"github.com/noisefloor-oss/deck/internal/db"
	"github.com/noisefloor-oss/deck/internal/hookcapture"
	"github.com/noisefloor-oss/deck/internal/provider"
	"github.com/noisefloor-oss/deck/internal/session"
	"github.com/noisefloor-oss/deck/internal/terminal"
	"github.com/noisefloor-oss/deck/internal/tmux"
)

func TestPasswordBootCreatesPrivateSetupLinkWithoutLoggingToken(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	cfg := testConfig(t)
	cfg.Auth.Mode = "password"
	cfg.Auth.SessionSecret = ""
	cfg.Auth.PasswordHash = ""
	var first, output bytes.Buffer
	h, err := Up(context.Background(), cfg, zerolog.New(&first), WithSetupOutput(&output))
	if err != nil {
		t.Fatalf("first Up: %v", err)
	}
	h.Close()
	statePath := filepath.Join(filepath.Dir(cfg.Database.Path), "auth-state.json")
	info, err := os.Stat(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("auth state mode = %04o", info.Mode().Perm())
	}
	if cfg.Auth.PasswordHash != "" || len(cfg.Auth.SessionSecret) != 32 {
		t.Fatal("boot did not install durable state into effective auth config")
	}
	if got, err := auth.ReadInstallationMode(auth.InstallationModePath(cfg.Database.Path)); err != nil || got != "password" {
		t.Fatalf("fresh installation mode = %q, %v", got, err)
	}
	link, err := os.ReadFile(auth.SetupURLPath(statePath))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), strings.TrimSpace(string(link))) || strings.Count(output.String(), "/setup#token=") != 1 {
		t.Fatal("startup did not print the setup link once")
	}
	if strings.Contains(first.String(), "#token=") {
		t.Fatal("setup link leaked to structured logs")
	}
	output.Reset()
	var second bytes.Buffer
	h, err = Up(context.Background(), cfg, zerolog.New(&second), WithSetupOutput(&output))
	if err != nil {
		t.Fatalf("restart Up: %v", err)
	}
	h.Close()
	if strings.Contains(second.String(), "#token=") {
		t.Fatal("restart leaked setup link to logs")
	}
	if strings.Count(output.String(), "/setup#token=") != 1 {
		t.Fatal("restart did not provide a valid link")
	}

}

func TestMismatchedReceiptRefusesBeforePasswordBootstrapOrMigration(t *testing.T) {
	cfg := testConfig(t)
	cfg.Auth.Mode = "password"
	cfg.Auth.SessionSecret = ""
	cfg.Auth.PasswordHash = ""
	modePath := auth.InstallationModePath(cfg.Database.Path)
	if err := auth.AdoptInstallationMode(modePath, "oidc", true); err != nil {
		t.Fatal(err)
	}

	if _, err := Up(context.Background(), cfg, zerolog.Nop()); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("mismatched receipt startup error = %v", err)
	}
	if _, err := os.Stat(cfg.Database.Path); !os.IsNotExist(err) {
		t.Fatalf("mismatched receipt startup created database: %v", err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(cfg.Database.Path), "auth-state.json")); !os.IsNotExist(err) {
		t.Fatalf("mismatched receipt startup created password state: %v", err)
	}
}

func TestPreReceiptInstallationRefusesBeforePasswordBootstrapOrMigration(t *testing.T) {
	cfg := testConfig(t)
	cfg.Auth.Mode = "password"
	cfg.Auth.SessionSecret = ""
	cfg.Auth.PasswordHash = ""
	legacy := []byte("last-release-pre-receipt-database")
	if err := os.MkdirAll(filepath.Dir(cfg.Database.Path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg.Database.Path, legacy, 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := Up(context.Background(), cfg, zerolog.Nop()); err == nil || !strings.Contains(err.Error(), "auth-mode adopt") {
		t.Fatalf("pre-receipt startup error = %v", err)
	}
	if got, err := os.ReadFile(cfg.Database.Path); err != nil || !bytes.Equal(got, legacy) {
		t.Fatalf("database changed before auth fence: %q, %v", got, err)
	}
	for _, name := range []string{"auth-state.json", "auth-mode.json"} {
		if _, err := os.Stat(filepath.Join(filepath.Dir(cfg.Database.Path), name)); !os.IsNotExist(err) {
			t.Fatalf("startup refusal created %s: %v", name, err)
		}
	}
}

// Everything here is real: real flock, real SQLite, real tmux socket
// directory, real HTTP socket, with every namespace a per-test temp dir.
// Only providers are unconfigured — provider absence must never block
// boot, which one test asserts directly.

func testBootEpoch(t *testing.T) *string {
	t.Helper()
	id, err := tmux.BootID()
	if err != nil {
		t.Skipf("host boot epoch unavailable: %v", err)
	}
	return &id
}

func testConfig(t *testing.T) *config.Config {
	t.Helper()
	dir := t.TempDir()
	cfg := config.Defaults()
	cfg.Server.Bind = "127.0.0.1:0"
	cfg.Database.Path = filepath.Join(dir, "run", "control.db")
	cfg.Database.LockPath = filepath.Join(dir, "run", "backend.lock")
	cfg.Tmux.SocketPath = filepath.Join(dir, "tmux", "ac.sock")
	cfg.Projects.AllowedRoots = []string{dir}
	cfg.Auth.SessionSecret = "test-secret"
	cfg.Reconciler.Interval = config.Duration(200 * time.Millisecond)
	// Individual tests opt providers in explicitly. Defaults describe the
	// packaged image, while this suite runs against host-installed binaries.
	cfg.Providers = config.Providers{}
	// Tests that need a2a opt in with their isolated source-client wrapper.
	cfg.Chats.Executable = filepath.Join(dir, "no-a2a")
	return &cfg
}

func TestBuildProvidersRegistersPi(t *testing.T) {
	cfg := config.Defaults()
	cfg.Providers = config.Providers{Pi: config.Provider{
		Executable: "/usr/local/bin/pi",
		HookConfig: "/home/agent/.pi/noisedeck-provider-hook.js",
	}}
	got := buildProviders(&cfg, zerolog.Nop())
	d, ok := got["pi"]
	if !ok {
		t.Fatalf("providers = %v, want pi", got)
	}
	if d.Name() != "pi" || d.IdentityContract().String() != "retrieved-by-command" {
		t.Fatalf("pi descriptor = %s/%s", d.Name(), d.IdentityContract())
	}
}

func TestBuildProvidersRegistersCommandCode(t *testing.T) {
	cfg := config.Defaults()
	cfg.Providers = config.Providers{CommandCode: config.Provider{
		Executable:   "/usr/local/bin/command-code",
		HookConfig:   "/home/agent/.commandcode/settings.json",
		HookReceiver: provider.HookReceiverPath,
	}}
	got := buildProviders(&cfg, zerolog.Nop())
	d, ok := got[provider.CommandCode]
	if !ok {
		t.Fatalf("providers = %v, want command-code", got)
	}
	if d.IdentityContract() != provider.IdentityRetrieved {
		t.Fatalf("identity = %s", d.IdentityContract())
	}
}

func TestBuildProvidersRegistersDevinResumeAttestor(t *testing.T) {
	cfg := config.Defaults()
	cfg.Providers = config.Providers{Devin: config.Provider{
		Executable:   "/usr/local/bin/devin",
		HookConfig:   "/home/agent/.config/devin/config.json",
		HookReceiver: provider.HookReceiverPath,
	}}
	got := buildProviders(&cfg, zerolog.Nop())
	d, ok := got[provider.Devin]
	if !ok {
		t.Fatalf("providers = %v, want devin", got)
	}
	attestor, ok := d.(provider.ResumeIdentityAttestationDescriptor)
	if !ok || attestor.ExactResumeHookSource() != "resume" {
		t.Fatalf("devin resume attestor = %T", d)
	}
}

func TestBuildProvidersRegistersQwenFreshRelaunch(t *testing.T) {
	cfg := config.Defaults()
	cfg.Providers = config.Providers{Qwen: config.Provider{
		Executable: "/usr/local/bin/qwen",
	}}
	got := buildProviders(&cfg, zerolog.Nop())
	d, ok := got[provider.Qwen]
	if !ok {
		t.Fatalf("providers = %v, want qwen", got)
	}
	if d.IdentityContract() != provider.IdentityFreshRelaunch {
		t.Fatalf("qwen contract = %s", d.IdentityContract())
	}
	// No authenticated run has observed Qwen Code's SessionStart hook or a
	// resume, so qwen reaches neither the identity-hook launch gate nor the
	// recovery attestor; an unmeasured source would be production identity
	// authority.
	if _, ok := d.(provider.ResumeIdentityAttestationDescriptor); ok {
		t.Fatal("qwen registered a resume attestation no authenticated run measured")
	}
	if _, err := d.ResumeSessionArgs(provider.ResumeSpec{NativeSessionID: "x"}); err == nil {
		t.Fatal("qwen recovery may build a resume argv nobody measured")
	}
}

func TestBuildProvidersRegistersMimoAsFreshRelaunch(t *testing.T) {
	cfg := config.Defaults()
	cfg.Providers = config.Providers{Mimo: config.Provider{Executable: "/usr/local/bin/mimo"}}
	got := buildProviders(&cfg, zerolog.Nop())
	d, ok := got[provider.Mimo]
	if !ok {
		t.Fatalf("providers = %v, want mimo", got)
	}
	if d.IdentityContract() != provider.IdentityFreshRelaunch {
		t.Fatalf("mimo identity = %s", d.IdentityContract())
	}
}

func TestBuildProvidersRegistersQoderAsFreshRelaunch(t *testing.T) {
	cfg := config.Defaults()
	cfg.Providers = config.Providers{Qoder: config.Provider{Executable: "/usr/local/bin/qoder"}}
	got := buildProviders(&cfg, zerolog.Nop())
	d, ok := got[provider.Qoder]
	if !ok {
		t.Fatalf("providers = %v, want qoder", got)
	}
	if d.IdentityContract() != provider.IdentityFreshRelaunch {
		t.Fatalf("qoder identity = %s", d.IdentityContract())
	}
	// No authenticated run has observed a Qoder resume, so qoder reaches
	// neither the identity-hook launch gate nor the recovery attestor; an
	// unmeasured source would be production identity authority.
	if _, ok := d.(provider.ResumeIdentityAttestationDescriptor); ok {
		t.Fatal("qoder registered a resume attestation no authenticated run measured")
	}
	if _, err := d.ResumeSessionArgs(provider.ResumeSpec{NativeSessionID: "x"}); err == nil {
		t.Fatal("qoder recovery may build a resume argv nobody measured")
	}
}

func TestBuildProvidersRegistersCursorAsFreshRelaunch(t *testing.T) {
	cfg := config.Defaults()
	cfg.Providers = config.Providers{Cursor: config.Provider{Executable: "/usr/local/bin/cursor-agent"}}
	got := buildProviders(&cfg, zerolog.Nop())
	d, ok := got[provider.Cursor]
	if !ok {
		t.Fatalf("providers = %v, want cursor", got)
	}
	if d.IdentityContract() != provider.IdentityFreshRelaunch {
		t.Fatalf("cursor identity = %s", d.IdentityContract())
	}
	// No authenticated run has observed a Cursor resume, so cursor reaches
	// neither the identity-hook launch gate nor the recovery attestor; an
	// unmeasured source would be production identity authority.
	if _, ok := d.(provider.ResumeIdentityAttestationDescriptor); ok {
		t.Fatal("cursor registered a resume attestation no authenticated run measured")
	}
	if _, err := d.ResumeSessionArgs(provider.ResumeSpec{NativeSessionID: "x"}); err == nil {
		t.Fatal("cursor recovery may build a resume argv nobody measured")
	}
}

func TestBuildProvidersRegistersGrokAsFreshRelaunch(t *testing.T) {
	cfg := config.Defaults()
	cfg.Providers = config.Providers{Grok: config.Provider{Executable: "/usr/local/bin/grok"}}
	got := buildProviders(&cfg, zerolog.Nop())
	d, ok := got[provider.Grok]
	if !ok {
		t.Fatalf("providers = %v, want grok", got)
	}
	if d.IdentityContract() != provider.IdentityFreshRelaunch {
		t.Fatalf("grok identity = %s", d.IdentityContract())
	}
	// No authenticated run has observed a Grok resume, so grok reaches
	// neither the identity-hook launch gate nor the recovery attestor; an
	// unmeasured source would be production identity authority.
	if _, ok := d.(provider.ResumeIdentityAttestationDescriptor); ok {
		t.Fatal("grok registered a resume attestation no authenticated run measured")
	}
	if _, err := d.ResumeSessionArgs(provider.ResumeSpec{NativeSessionID: "x"}); err == nil {
		t.Fatal("grok recovery may build a resume argv nobody measured")
	}
}

func TestBuildProvidersRegistersKilo(t *testing.T) {
	cfg := config.Defaults()
	cfg.Providers = config.Providers{Kilo: config.Provider{
		Executable: "/usr/local/bin/kilo", HookConfig: "/home/agent/.config/kilo/plugin/noisedeck-provider-hook.js",
	}}
	got := buildProviders(&cfg, zerolog.Nop())
	d, ok := got[provider.Kilo]
	if !ok {
		t.Fatalf("providers = %v, want Kilo", got)
	}
	if d.IdentityContract() != provider.IdentityRetrieved {
		t.Fatalf("Kilo contract = %s", d.IdentityContract())
	}
}

func upOrFail(t *testing.T, cfg *config.Config) *Handles {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	h, err := Up(context.Background(), cfg, zerolog.Nop())
	if err != nil {
		t.Fatalf("Up: %v", err)
	}
	t.Cleanup(h.Close)
	return h
}

// A restart reconstructs the stateless hook receiver without destroying a
// pending durable grant held by an already-running provider process.
func TestUpPreservesPendingIdentityCapture(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	cfg := testConfig(t)
	providerCLI := filepath.Join(t.TempDir(), "codex")
	providerScript := `#!/bin/sh
case "$1" in
  --version) echo 'codex-cli 0.999.0' ;;
  --help)
    printf '%s\n' 'Options:' '      --dangerously-bypass-approvals-and-sandbox  Skip prompts'
    ;;
  resume)
    [ "$2" = '--help' ] || exit 2
    printf '%s\n' 'Usage: codex resume [OPTIONS] [SESSION_ID] [PROMPT]' 'Options:' '      --dangerously-bypass-approvals-and-sandbox  Skip prompts'
    ;;
  *) exit 2 ;;
esac
`
	if err := os.WriteFile(providerCLI, []byte(providerScript), 0o700); err != nil {
		t.Fatalf("write provider fixture: %v", err)
	}
	hookDir := t.TempDir()
	hookConfig := filepath.Join(hookDir, "hooks.json")
	if err := os.WriteFile(hookConfig,
		[]byte(`{"hooks":{"SessionStart":[{"hooks":[{"type":"command","command":"/usr/local/bin/noisedeck-provider-hook"}]}]}}`),
		0o600); err != nil {
		t.Fatalf("write hook fixture: %v", err)
	}
	if err := os.WriteFile(filepath.Join(hookDir, "config.toml"),
		[]byte("[features]\nhooks = true\n"), 0o600); err != nil {
		t.Fatalf("write Codex config fixture: %v", err)
	}
	cfg.Providers.Codex = config.Provider{
		Executable: providerCLI, HookConfig: hookConfig, HookReceiver: provider.HookReceiverPath,
	}
	if err := os.MkdirAll(filepath.Dir(cfg.Database.Path), 0o700); err != nil {
		t.Fatalf("create database directory: %v", err)
	}
	database, err := db.Open(context.Background(), db.Config{Path: cfg.Database.Path})
	if err != nil {
		t.Fatalf("open upgrade fixture: %v", err)
	}
	repo := session.NewRepository(database)
	ctx := context.Background()
	now := db.FormatTime(time.Now())
	const projectID = "capture-order-project"
	if err := database.WithTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			INSERT INTO projects (id, name, canonical_path, git_author_name, git_author_email, created_at, last_used_at)
			VALUES (?, 'capture order', ?, 'Test Author', 'test@example.invalid', ?, ?)`,
			projectID, cfg.Projects.AllowedRoots[0], now, now)
		return err
	}); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	marker := "persisted-codex-marker"
	row := &session.Session{
		ID: "persisted-detached-codex", ProjectID: projectID, Provider: string(provider.Codex),
		TmuxSocketPath: cfg.Tmux.SocketPath, TmuxSessionName: "missing-codex-pane", Generation: 1,
		IdentityMarker: &marker,
	}
	if err := repo.Create(ctx, row); err != nil {
		t.Fatalf("seed session: %v", err)
	}
	captures, err := hookcapture.NewService(repo, time.Hour)
	if err != nil {
		t.Fatalf("create durable capture service: %v", err)
	}
	if _, err := captures.RegisterFenced(ctx, row.ID, row.Provider,
		cfg.Projects.AllowedRoots[0], row.Generation, marker); err != nil {
		t.Fatalf("persist capture grant: %v", err)
	}
	sessionID, paneID := "$999", "%999"
	pid, processGroupID := 999999, 999999
	startTicks := int64(123456)
	expectedExecutable := "/usr/local/bin/node"
	row.TmuxSessionID, row.TmuxPaneID = &sessionID, &paneID
	row.PanePID, row.ProcessGroupID, row.StartTicks = &pid, &processGroupID, &startTicks
	row.BootEpoch = testBootEpoch(t)
	row.ExpectedExecutable = &expectedExecutable
	if err := repo.MarkRunning(ctx, row); err != nil {
		t.Fatalf("seed recorded runtime: %v", err)
	}
	seedLegacyAttention(t, database, row.ID)

	foreignTmux, err := tmux.NewClient(tmux.Config{SocketPath: cfg.Tmux.SocketPath})
	if err != nil {
		t.Fatalf("create foreign-pane client: %v", err)
	}
	foreign, err := foreignTmux.CreateSession(ctx, tmux.SessionSpec{
		Name: "foreign-live-pane", WorkDir: cfg.Projects.AllowedRoots[0],
		Argv:       []string{"/bin/sh", "-c", "sleep 300; exit 0"},
		Generation: 77, Marker: "foreign-live-marker",
	})
	if err != nil || foreign.Dead {
		t.Fatalf("create foreign live pane: info=%+v err=%v", foreign, err)
	}
	t.Cleanup(func() { _ = foreignTmux.KillServer(context.Background()) })
	recordedMarker := "legacy-reused-marker"
	reused := &session.Session{
		ID: "persisted-reused-live-pane", ProjectID: projectID, Provider: string(provider.Codex),
		TmuxSocketPath: cfg.Tmux.SocketPath, TmuxSessionName: foreign.SessionName, Generation: 1,
		IdentityMarker: &recordedMarker,
	}
	if err := repo.Create(ctx, reused); err != nil {
		t.Fatalf("seed reused-pane session: %v", err)
	}
	reused.TmuxSessionID, reused.TmuxPaneID = &foreign.SessionID, &foreign.PaneID
	reused.PanePID, reused.ProcessGroupID, reused.StartTicks =
		&foreign.PID, &foreign.ProcessGroupID, &foreign.StartTicks
	reused.BootEpoch = testBootEpoch(t)
	reused.ExpectedExecutable = &foreign.Executable
	if err := repo.MarkRunning(ctx, reused); err != nil {
		t.Fatalf("seed reused-pane runtime: %v", err)
	}
	seedLegacyAttention(t, database, reused.ID)

	exact, err := foreignTmux.CreateSession(ctx, tmux.SessionSpec{
		Name: "legacy-exact-live-pane", WorkDir: cfg.Projects.AllowedRoots[0],
		Argv:       []string{"/bin/sh", "-c", "sleep 300; exit 0"},
		Generation: 1, Marker: "legacy-exact-live-marker",
	})
	if err != nil || exact.Dead {
		t.Fatalf("create exact legacy pane: info=%+v err=%v", exact, err)
	}
	exactMarker := exact.Marker
	exactRow := &session.Session{
		ID: "persisted-exact-live-pane", ProjectID: projectID, Provider: string(provider.Codex),
		TmuxSocketPath: cfg.Tmux.SocketPath, TmuxSessionName: exact.SessionName, Generation: 1,
		IdentityMarker: &exactMarker,
	}
	if err := repo.Create(ctx, exactRow); err != nil {
		t.Fatalf("seed exact-live session: %v", err)
	}
	exactRow.TmuxSessionID, exactRow.TmuxPaneID = &exact.SessionID, &exact.PaneID
	exactRow.PanePID, exactRow.ProcessGroupID, exactRow.StartTicks =
		&exact.PID, &exact.ProcessGroupID, &exact.StartTicks
	exactRow.BootEpoch = testBootEpoch(t)
	exactRow.ExpectedExecutable = &exact.Executable
	if err := repo.MarkRunning(ctx, exactRow); err != nil {
		t.Fatalf("seed exact-live runtime: %v", err)
	}
	if err := database.WithTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			UPDATE sessions SET resume_attempts = 7, resume_next_at = ?,
				resume_series_started_at = ?, resume_probation_until = ?
			WHERE id = ?`, now, now, db.FormatTime(time.Now().Add(time.Hour)), exactRow.ID)
		return err
	}); err != nil {
		t.Fatalf("seed exact-live retry residue: %v", err)
	}
	seedLegacyAttention(t, database, exactRow.ID)
	if err := database.Close(); err != nil {
		t.Fatalf("close fixture database: %v", err)
	}
	if err := auth.AdoptInstallationMode(auth.InstallationModePath(cfg.Database.Path), cfg.Auth.Mode, true); err != nil {
		t.Fatalf("seed installation mode: %v", err)
	}

	h := upOrFail(t, cfg)
	got, err := h.Sessions.Get(ctx, row.ID)
	if err != nil {
		t.Fatalf("get upgraded row: %v", err)
	}
	// Legacy needs_attention rows converge to interrupted before readiness.
	// Restart preserves the grant; it neither invents a capture loss nor
	// turns this never-verified generation into a recovery opportunity.
	if got.Status != session.StatusInterrupted {
		t.Fatalf("upgraded row status=%s, want interrupted (converged)", got.Status)
	}
	if got.IdentityCaptureLostAt != nil || got.NativeSessionID != nil {
		t.Fatalf("upgraded row capture_lost=%v native=%v",
			got.IdentityCaptureLostAt, got.NativeSessionID)
	}
	pending, err := h.Sessions.IdentityCapturePending(ctx, row.ID, row.Generation,
		marker, time.Now())
	if err != nil || !pending {
		t.Fatalf("pending durable grant after Up = %v, err=%v", pending, err)
	}
	var captureLosses, attentionEvents int
	if err := h.DB.Reader().QueryRowContext(ctx,
		`SELECT count(*) FROM audit_events WHERE session_id = ? AND event_type = ?`,
		row.ID, session.AuditIdentityCaptureLost).Scan(&captureLosses); err != nil {
		t.Fatalf("count capture-loss audit: %v", err)
	}
	if err := h.DB.Reader().QueryRowContext(ctx,
		`SELECT count(*) FROM audit_events WHERE session_id = ? AND event_type = ?`,
		row.ID, session.AuditAttention).Scan(&attentionEvents); err != nil {
		t.Fatalf("count attention audit: %v", err)
	}
	if captureLosses != 0 {
		t.Fatalf("capture-loss audits=%d, want 0", captureLosses)
	}
	// No new needs_attention events: convergence writes interrupted, not attention.
	if attentionEvents != 1 {
		t.Fatalf("attention events=%d, want 1 (only the original legacy mark)", attentionEvents)
	}

	reusedGot, err := h.Sessions.Get(ctx, reused.ID)
	if err != nil {
		t.Fatalf("get reused-pane upgrade row: %v", err)
	}
	if reusedGot.Status != session.StatusInterrupted || reusedGot.ResumeAttempts != 1 {
		t.Fatalf("reused-pane row = status %s attempts %d, want interrupted/1",
			reusedGot.Status, reusedGot.ResumeAttempts)
	}
	foreignAfter, err := foreignTmux.PaneInfoByID(ctx, foreign.PaneID)
	if err != nil || foreignAfter.Dead || foreignAfter.PID != foreign.PID ||
		foreignAfter.StartTicks != foreign.StartTicks || foreignAfter.Marker != foreign.Marker {
		t.Fatalf("legacy convergence touched foreign pane: before=%+v after=%+v err=%v",
			foreign, foreignAfter, err)
	}
	for _, eventType := range []string{session.AuditStopRequested, session.AuditRemoved, session.AuditResumed} {
		var count int
		if err := h.DB.Reader().QueryRowContext(ctx,
			`SELECT count(*) FROM audit_events WHERE session_id = ? AND event_type = ?`,
			reused.ID, eventType).Scan(&count); err != nil {
			t.Fatalf("count %s audits: %v", eventType, err)
		}
		if count != 0 {
			t.Fatalf("reused-pane row recorded %d %s effects", count, eventType)
		}
	}

	exactGot, err := h.Sessions.Get(ctx, exactRow.ID)
	if err != nil {
		t.Fatalf("get exact-live upgrade row: %v", err)
	}
	if exactGot.Status != session.StatusRunning || exactGot.ResumeAttempts != 0 ||
		exactGot.ResumeProbationUntil != nil || exactGot.Generation != exactRow.Generation {
		t.Fatalf("exact-live legacy convergence = status %s attempts %d probation %v generation %d",
			exactGot.Status, exactGot.ResumeAttempts, exactGot.ResumeProbationUntil, exactGot.Generation)
	}
	exactAfter, err := foreignTmux.PaneInfoByID(ctx, exact.PaneID)
	if err != nil || exactAfter.Dead || exactAfter.PID != exact.PID ||
		exactAfter.StartTicks != exact.StartTicks {
		t.Fatalf("exact-live legacy convergence replaced controller: before=%+v after=%+v err=%v",
			exact, exactAfter, err)
	}
	if err := syscall.Kill(-exact.ProcessGroupID, syscall.SIGKILL); err != nil {
		t.Fatalf("lose verified legacy generation: %v", err)
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		dead, observeErr := foreignTmux.PaneInfoByID(ctx, exact.PaneID)
		if observeErr != nil {
			t.Fatalf("observe lost legacy pane: %v", observeErr)
		}
		if dead.Dead {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("lost legacy pane did not become dead")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, err := h.Manager.Present(ctx, exactGot); err != nil {
		t.Fatalf("observe lost verified legacy generation: %v", err)
	}
	if err := h.Manager.AutoResumeSweep(ctx); err != nil {
		t.Fatalf("consume legacy generation automatic opportunity: %v", err)
	}
	lost, err := h.Sessions.Get(ctx, exactRow.ID)
	if err != nil || lost.ResumeAttempts != 1 {
		t.Fatalf("lost verified legacy generation = %+v err=%v, want one consumed opportunity", lost, err)
	}
	var legacyAttemptWrites int
	if err := h.DB.Reader().QueryRowContext(ctx, `
		SELECT count(*) FROM session_recovery_attempts
		WHERE session_id = ? AND source_generation = ? AND kind = 'automatic'`,
		exactRow.ID, exactRow.Generation).Scan(&legacyAttemptWrites); err != nil {
		t.Fatalf("count writes to released recovery table: %v", err)
	}
	if legacyAttemptWrites != 0 {
		t.Fatalf("writes to released recovery table = %d, want 0", legacyAttemptWrites)
	}
}

// serve runs the booted backend and returns its live base URL.
func serve(t *testing.T, h *Handles) string {
	t.Helper()
	base, _ := startServer(t, h.Run, h.Server.BoundAddr)
	return base
}

func serveServer(t *testing.T, h *Handles) string {
	t.Helper()
	base, _ := startServer(t, h.Server.Run, h.Server.BoundAddr)
	return base
}

func startServer(t *testing.T, run func(context.Context) error,
	boundAddr func() string) (string, func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- run(ctx) }()
	var stopOnce sync.Once
	stop := func() {
		stopOnce.Do(func() {
			cancel()
			select {
			case err := <-done:
				if err != nil {
					t.Errorf("backend stopped with error: %v", err)
				}
			case <-time.After(5 * time.Second):
				t.Error("backend did not stop")
			}
		})
	}
	t.Cleanup(stop)
	deadline := time.Now().Add(30 * time.Second)
	for boundAddr() == "" {
		if time.Now().After(deadline) {
			t.Fatal("server never bound")
		}
		time.Sleep(10 * time.Millisecond)
	}
	return "http://" + boundAddr(), stop
}

func get(t *testing.T, url string) (int, string) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func authenticatedGet(t *testing.T, url string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	req.Header.Set("X-Forwarded-User", "operator@example.test")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// The whole point: a fresh boot on an empty namespace comes up ready, with
// readyz green over a real socket.
func TestBootBecomesReady(t *testing.T) {
	h := upOrFail(t, testConfig(t))
	base := serve(t, h)

	code, _ := get(t, base+"/livez")
	if code != http.StatusOK {
		t.Fatalf("livez = %d", code)
	}
	code, body := get(t, base+"/readyz")
	if code != http.StatusOK {
		t.Fatalf("readyz = %d (%s)", code, body)
	}
}

func TestBootWithProvisionedOmpBecomesReady(t *testing.T) {
	cfg := testConfig(t)
	dir := t.TempDir()
	executable := filepath.Join(dir, "omp")
	fakeOmp := `#!/bin/sh
case "$1" in
  --version)
    printf '%s\n' '17.2.12'
    ;;
  --help)
    printf '%s\n' \
      '  --model=<value>  model' \
      '  --thinking=<value>  thinking' \
      '  --hook=<value>  hook' \
      '  --resume=<value>  resume' \
      '  --auto-approve  permissions'
    ;;
esac
`
	if err := os.WriteFile(executable, []byte(fakeOmp), 0o700); err != nil {
		t.Fatalf("write fake omp: %v", err)
	}
	hookPath := filepath.Join(dir, "noisedeck-provider-hook.ts")
	if err := os.WriteFile(hookPath, provider.OmpHookPlugin, 0o600); err != nil {
		t.Fatalf("write provisioned omp hook: %v", err)
	}
	cfg.Providers.Omp = config.Provider{Executable: executable, HookConfig: hookPath}

	h := upOrFail(t, cfg)
	base := serve(t, h)
	if code, body := get(t, base+"/readyz"); code != http.StatusOK {
		t.Fatalf("readyz with packaged omp = %d (%s)", code, body)
	}
}

// A second backend against the same configuration must refuse at the lock,
// before touching anything.
func TestSecondBackendRefused(t *testing.T) {
	cfg := testConfig(t)
	_ = upOrFail(t, cfg)

	_, err := Up(context.Background(), cfg, zerolog.Nop())
	if err == nil || !strings.Contains(err.Error(), "held") {
		t.Fatalf("second boot: err = %v, want lock refusal", err)
	}
}

// Container-restart resolution: a previous life left a `running` row whose
// process and pane no longer exist. Boot's first reconciliation pass must
// resolve it to interrupted before readiness — no session may stay running
// with its tmux session absent.
func TestBootResolvesPreviousLife(t *testing.T) {
	cfg := testConfig(t)
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}

	// Simulate the previous life directly in the store: full identity
	// tuple, long-dead pid, no pane anywhere.
	d, err := db.Open(context.Background(), db.Config{Path: cfg.Database.Path})
	if err != nil {
		t.Fatalf("pre-seed open: %v", err)
	}
	repo := session.NewRepository(d)
	ctx := context.Background()
	err = d.WithTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			INSERT INTO projects (id, name, canonical_path, git_author_name, git_author_email, created_at, last_used_at)
			VALUES ('prj', 'test', ?, 'Test Author', 'test@example.invalid', ?, ?)`,
			cfg.Projects.AllowedRoots[0], db.FormatTime(time.Now()), db.FormatTime(time.Now()))
		return err
	})
	if err != nil {
		t.Fatalf("seed project: %v", err)
	}
	marker := "ghost-marker"
	s := &session.Session{
		ID: "ghost", ProjectID: "prj", Provider: "claude",
		TmuxSocketPath: cfg.Tmux.SocketPath, TmuxSessionName: "noisedeck-ghost", Generation: 1,
		IdentityMarker: &marker,
	}
	if err := repo.Create(ctx, s); err != nil {
		t.Fatalf("seed session: %v", err)
	}
	sid, pane := "$99", "%99"
	pid, pgid := 999999, 999999
	ticks := int64(123456)
	exe := "/bin/claude-of-a-past-boot"
	s.TmuxSessionID, s.TmuxPaneID = &sid, &pane
	s.PanePID, s.ProcessGroupID, s.StartTicks = &pid, &pgid, &ticks
	s.BootEpoch = testBootEpoch(t)
	s.ExpectedExecutable = &exe
	if err := repo.MarkRunning(ctx, s); err != nil {
		t.Fatalf("seed running: %v", err)
	}
	_ = d.Close()
	if err := auth.AdoptInstallationMode(auth.InstallationModePath(cfg.Database.Path), cfg.Auth.Mode, true); err != nil {
		t.Fatalf("seed installation mode: %v", err)
	}

	h := upOrFail(t, cfg)
	got, err := h.Sessions.Get(ctx, "ghost")
	if err != nil {
		t.Fatalf("Get ghost: %v", err)
	}
	if got.Status != session.StatusInterrupted {
		t.Fatalf("previous life resolved to %s, want interrupted", got.Status)
	}
	if got.ExitCode != nil || got.ExitSignal != nil {
		t.Fatal("interruption must carry no fabricated exit evidence")
	}
}

// An independently configured lock path in a fresh directory must boot: the
// lock only DEFAULTS beside the database.
func TestBootCreatesSeparateLockDir(t *testing.T) {
	cfg := testConfig(t)
	cfg.Database.LockPath = filepath.Join(t.TempDir(), "elsewhere", "backend.lock")
	h := upOrFail(t, cfg)
	if h.Lock == nil {
		t.Fatal("lock not held")
	}
}

// Provider absence is logged unavailability, never a boot failure.
func TestBootWithoutProvidersStillReady(t *testing.T) {
	cfg := testConfig(t)
	cfg.Providers.Claude.Executable = ""
	cfg.Providers.Codex.Executable = ""
	cfg.Providers.Kimi.Executable = ""
	h := upOrFail(t, cfg)
	if len(h.Providers) != 0 {
		t.Fatalf("providers = %d, want none", len(h.Providers))
	}
	base := serve(t, h)
	if code, body := get(t, base+"/readyz"); code != http.StatusOK {
		t.Fatalf("readyz without providers = %d (%s)", code, body)
	}
}

// Loosening the tmux socket directory's permissions after boot must fail
// readiness on the next poll.
func TestReadyzFailsOnInsecureTmuxDir(t *testing.T) {
	cfg := testConfig(t)
	h := upOrFail(t, cfg)
	base := serve(t, h)

	if code, _ := get(t, base+"/readyz"); code != http.StatusOK {
		t.Fatalf("precondition: readyz should be 200")
	}
	if err := os.Chmod(filepath.Dir(cfg.Tmux.SocketPath), 0o755); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	code, body := get(t, base+"/readyz")
	if code != http.StatusServiceUnavailable || !strings.Contains(body, "tmux-security") {
		t.Fatalf("readyz after chmod = %d (%s), want 503 naming tmux-security", code, body)
	}
}

func TestReadyzFailsAfterBackendLockIsReleased(t *testing.T) {
	h := upOrFail(t, testConfig(t))
	base := serve(t, h)
	if err := h.Lock.Release(); err != nil {
		t.Fatalf("release lock: %v", err)
	}
	code, body := get(t, base+"/readyz")
	if code != http.StatusServiceUnavailable || !strings.Contains(body, "backend-lock") {
		t.Fatalf("readyz after lock release = %d (%s), want 503 naming backend-lock", code, body)
	}
}

func TestReadyzNamesDatabaseTmuxAndReconcilerFailures(t *testing.T) {
	t.Run("database", func(t *testing.T) {
		h := upOrFail(t, testConfig(t))
		base := serve(t, h)
		if err := h.DB.Close(); err != nil {
			t.Fatalf("close database: %v", err)
		}
		code, body := get(t, base+"/readyz")
		if code != http.StatusServiceUnavailable || !strings.Contains(body, "database") {
			t.Fatalf("readyz after database close = %d (%s), want 503 naming database", code, body)
		}
	})

	t.Run("tmux access", func(t *testing.T) {
		h := upOrFail(t, testConfig(t))
		base := serve(t, h)
		t.Setenv("PATH", t.TempDir())
		code, body := get(t, base+"/readyz")
		if code != http.StatusServiceUnavailable || !strings.Contains(body, "tmux-access") {
			t.Fatalf("readyz with inaccessible tmux = %d (%s), want 503 naming tmux-access", code, body)
		}
	})

	t.Run("reconciler recency", func(t *testing.T) {
		cfg := testConfig(t)
		cfg.Reconciler.Interval = config.Duration(20 * time.Millisecond)
		h := upOrFail(t, cfg)
		base := serveServer(t, h)
		deadline := time.Now().Add(time.Second)
		for {
			code, body := get(t, base+"/readyz")
			if code == http.StatusServiceUnavailable && strings.Contains(body, "reconciler-recency") {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("readyz with stale reconciler = %d (%s), want 503 naming reconciler-recency", code, body)
			}
			time.Sleep(10 * time.Millisecond)
		}
	})
}

func TestDiagnosticsReportRecordedObservedAndFleetState(t *testing.T) {
	cfg := testConfig(t)
	cfg.Auth.Mode = "trusted_proxy"
	cfg.Auth.TrustedProxy.Addresses = []string{"127.0.0.1"}
	h := upOrFail(t, cfg)

	ctx := context.Background()
	now := db.FormatTime(time.Now())
	if err := h.DB.WithTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			INSERT INTO projects (id, name, canonical_path, git_author_name, git_author_email, created_at, last_used_at)
			VALUES ('diag-project', 'diagnostics', ?, 'Test Author', 'test@example.invalid', ?, ?)`,
			cfg.Projects.AllowedRoots[0], now, now)
		return err
	}); err != nil {
		t.Fatalf("seed diagnostic project: %v", err)
	}

	info, err := h.Tmux.CreateSession(ctx, tmux.SessionSpec{
		Name: "diag-live", WorkDir: cfg.Projects.AllowedRoots[0],
		Argv: []string{"/bin/sh"}, Generation: 1, Marker: "diag-marker",
	})
	if err != nil {
		t.Fatalf("create diagnostic pane: %v", err)
	}
	t.Cleanup(func() { _ = h.Tmux.KillServer(context.Background()) })
	live := &session.Session{
		ID: "diag-live", ProjectID: "diag-project", Provider: "shell",
		TmuxSocketPath: cfg.Tmux.SocketPath, TmuxSessionName: "diag-live", Generation: 1,
		NativeSessionID: ptr("native-diag-live"), IdentityMarker: ptr("diag-marker"),
	}
	if err := h.Sessions.Create(ctx, live); err != nil {
		t.Fatalf("create live row: %v", err)
	}
	live.TmuxSessionID, live.TmuxPaneID = &info.SessionID, &info.PaneID
	live.PanePID, live.ProcessGroupID, live.StartTicks = &info.PID, &info.ProcessGroupID, &info.StartTicks
	live.BootEpoch = testBootEpoch(t)
	live.ExpectedExecutable = &info.Executable
	if err := h.Sessions.MarkRunning(ctx, live); err != nil {
		t.Fatalf("mark live row running: %v", err)
	}

	attention := &session.Session{
		ID: "diag-attention", ProjectID: "diag-project", Provider: "shell",
		TmuxSocketPath: cfg.Tmux.SocketPath, TmuxSessionName: "diag-attention", Generation: 1,
	}
	if err := h.Sessions.Create(ctx, attention); err != nil {
		t.Fatalf("create attention row: %v", err)
	}
	seedLegacyAttention(t, h.DB, attention.ID)
	seededLease, err := h.Leases.Acquire(ctx, live.ID, "diagnostic-controller", false)
	if err != nil {
		t.Fatalf("seed controller lease: %v", err)
	}

	newRow := func(id string) *session.Session {
		return &session.Session{
			ID: id, ProjectID: "diag-project", Provider: "shell",
			TmuxSocketPath: cfg.Tmux.SocketPath, TmuxSessionName: id, Generation: 1,
		}
	}
	starting := newRow("diag-starting")
	if err := h.Sessions.Create(ctx, starting); err != nil {
		t.Fatalf("create starting row: %v", err)
	}
	exited := newRow("diag-exited")
	if err := h.Sessions.Create(ctx, exited); err != nil {
		t.Fatalf("create exited row: %v", err)
	}
	exitCode := 0
	if _, err := h.Sessions.RecordExitObserved(ctx, exited, &exitCode, nil, time.Now()); err != nil {
		t.Fatalf("record exited row: %v", err)
	}
	removed := newRow("diag-removed")
	if err := h.Sessions.Create(ctx, removed); err != nil {
		t.Fatalf("create removed row: %v", err)
	}
	if err := h.Manager.Remove(ctx, removed.ID); err != nil {
		t.Fatalf("remove count fixture: %v", err)
	}

	base := serveServer(t, h)
	recordedLive, err := h.Sessions.Get(ctx, live.ID)
	if err != nil {
		t.Fatalf("reload live row: %v", err)
	}
	if code, _ := get(t, base+"/api/sessions/"+live.ID+"/status"); code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated session status = %d, want 401", code)
	}
	code, body := authenticatedGet(t, base+"/api/sessions/"+live.ID+"/status")
	if code != http.StatusOK {
		t.Fatalf("session status = %d (%s)", code, body)
	}
	var status struct {
		Recorded struct {
			ID                 string     `json:"id"`
			Status             string     `json:"status"`
			Generation         int        `json:"generation"`
			NativeSessionID    *string    `json:"native_session_id"`
			ExitCode           *int       `json:"exit_code"`
			ExitSignal         *string    `json:"exit_signal"`
			ExitedAt           *time.Time `json:"exited_at"`
			CreatedAt          time.Time  `json:"created_at"`
			UpdatedAt          time.Time  `json:"updated_at"`
			PresentationState  string     `json:"presentation_state"`
			RecoveryCapability string     `json:"recovery_capability"`
			RecoveryState      string     `json:"recovery_state"`
		} `json:"recorded"`
		Observed struct {
			PaneState         string `json:"pane_state"`
			ProcessState      string `json:"process_state"`
			PaneExists        *bool  `json:"pane_exists"`
			PaneAlive         *bool  `json:"pane_alive"`
			PanePID           *int   `json:"pane_pid"`
			ProcessGroupID    *int   `json:"process_group_id"`
			ProcessStartTicks *int64 `json:"process_start_ticks"`
			IdentityVerified  *bool  `json:"identity_verified"`
		} `json:"observed"`
		Reconciliation struct {
			LastRun          time.Time `json:"last_run"`
			DiscrepancyCount int64     `json:"discrepancy_count"`
			HasDiscrepancy   *bool     `json:"has_discrepancy"`
		} `json:"reconciliation"`
		Connection struct {
			AttachmentActive bool       `json:"attachment_active"`
			AttachmentCount  int        `json:"attachment_count"`
			ControllerActive bool       `json:"controller_active"`
			LeaseState       string     `json:"lease_state"`
			LeaseExpiry      *time.Time `json:"lease_expiry"`
			LastWriteResult  string     `json:"last_write_result"`
		} `json:"connection"`
	}
	if err := json.Unmarshal([]byte(body), &status); err != nil {
		t.Fatalf("decode session status: %v (%s)", err, body)
	}
	if status.Recorded.ID != recordedLive.ID || status.Recorded.Status != "running" ||
		status.Recorded.Generation != recordedLive.Generation || status.Recorded.NativeSessionID == nil ||
		*status.Recorded.NativeSessionID != *recordedLive.NativeSessionID || status.Recorded.ExitCode != nil ||
		status.Recorded.ExitSignal != nil || status.Recorded.ExitedAt != nil ||
		status.Recorded.PresentationState != "running" ||
		status.Recorded.RecoveryCapability != "none" ||
		status.Recorded.RecoveryState != "unavailable" ||
		!status.Recorded.CreatedAt.Equal(recordedLive.CreatedAt) ||
		!status.Recorded.UpdatedAt.Equal(recordedLive.UpdatedAt) {
		t.Fatalf("unexpected recorded projection: %+v, row=%+v", status.Recorded, recordedLive)
	}
	if status.Observed.PaneState != "alive" || status.Observed.ProcessState != "alive" ||
		status.Observed.PaneExists == nil || !*status.Observed.PaneExists ||
		status.Observed.PaneAlive == nil || !*status.Observed.PaneAlive ||
		status.Observed.PanePID == nil || *status.Observed.PanePID != info.PID ||
		status.Observed.ProcessGroupID == nil || *status.Observed.ProcessGroupID != info.ProcessGroupID ||
		status.Observed.ProcessStartTicks == nil || *status.Observed.ProcessStartTicks != info.StartTicks ||
		status.Observed.IdentityVerified == nil || !*status.Observed.IdentityVerified ||
		status.Reconciliation.LastRun.IsZero() || status.Reconciliation.DiscrepancyCount != 0 ||
		status.Reconciliation.HasDiscrepancy == nil || *status.Reconciliation.HasDiscrepancy {
		t.Fatalf("unexpected status projection: %+v", status)
	}
	leaseExpiryMatches := false
	if status.Connection.LeaseExpiry != nil {
		delta := status.Connection.LeaseExpiry.Sub(seededLease.ExpiresAt)
		leaseExpiryMatches = delta > -time.Millisecond && delta < time.Millisecond
	}
	if status.Connection.AttachmentActive || status.Connection.AttachmentCount != 0 ||
		status.Connection.ControllerActive || status.Connection.LeaseState != "held" ||
		!leaseExpiryMatches ||
		status.Connection.LastWriteResult != "none" {
		t.Fatalf("lease was presented as a live attachment: %+v", status.Connection)
	}
	assertNoRetiredDiagnosticFields(t, body)

	if code, _ := get(t, base+"/api/metrics"); code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated metrics = %d, want 401", code)
	}
	code, body = authenticatedGet(t, base+"/api/metrics")
	if code != http.StatusOK {
		t.Fatalf("metrics = %d (%s)", code, body)
	}
	var metrics struct {
		ReconcilerLastRun         time.Time `json:"reconciler_last_run"`
		ReconcilerInterval        string    `json:"reconciler_interval"`
		DiscrepancyCount          int64     `json:"discrepancy_count"`
		ActiveSessions            int       `json:"active_sessions"`
		AutomaticRecoveryPending  int       `json:"automatic_recovery_pending"`
		AutomaticRecoveryInFlight int       `json:"automatic_recovery_in_flight"`
	}
	if err := json.Unmarshal([]byte(body), &metrics); err != nil {
		t.Fatalf("decode metrics: %v (%s)", err, body)
	}
	if metrics.ReconcilerLastRun.IsZero() ||
		metrics.ReconcilerInterval != cfg.Reconciler.Interval.Std().String() ||
		metrics.DiscrepancyCount != 0 || metrics.ActiveSessions != 2 ||
		metrics.AutomaticRecoveryPending != 0 || metrics.AutomaticRecoveryInFlight != 0 {
		t.Fatalf("fleet metrics = %+v", metrics)
	}
	assertNoRetiredDiagnosticFields(t, body)

	liveExitCode := 23
	liveExitedAt := time.Now().UTC().Truncate(time.Millisecond)
	if _, err := h.Sessions.RecordExitObserved(ctx, live, &liveExitCode, nil, liveExitedAt); err != nil {
		t.Fatalf("record live discrepancy: %v", err)
	}
	code, body = authenticatedGet(t, base+"/api/sessions/"+live.ID+"/status")
	if code != http.StatusOK {
		t.Fatalf("discrepant session status = %d (%s)", code, body)
	}
	var discrepant sessionStatusDiagnostic
	if err := json.Unmarshal([]byte(body), &discrepant); err != nil {
		t.Fatalf("decode discrepant session: %v (%s)", err, body)
	}
	if discrepant.Recorded.Status != "exited" || discrepant.Recorded.ExitCode == nil ||
		*discrepant.Recorded.ExitCode != liveExitCode || discrepant.Recorded.ExitSignal != nil ||
		discrepant.Recorded.ExitedAt == nil || !discrepant.Recorded.ExitedAt.Equal(liveExitedAt) ||
		discrepant.Reconciliation.HasDiscrepancy == nil || !*discrepant.Reconciliation.HasDiscrepancy {
		t.Fatalf("discrepancy projection = %+v", discrepant)
	}
	assertNoRetiredDiagnosticFields(t, body)

	if _, err := h.Tmux.CreateSession(ctx, tmux.SessionSpec{
		Name: "diag-orphan", WorkDir: cfg.Projects.AllowedRoots[0],
		Argv: []string{"/bin/sh"}, Generation: 1, Marker: "orphan-marker",
	}); err != nil {
		t.Fatalf("create orphan pane: %v", err)
	}
	if _, err := h.Reconciler.RunOnce(ctx); err != nil {
		t.Fatalf("first discrepancy pass: %v", err)
	}
	firstMetrics := diagnosticMetrics(t, base)
	if !firstMetrics.ReconcilerLastRun.After(metrics.ReconcilerLastRun) ||
		firstMetrics.DiscrepancyCount <= metrics.DiscrepancyCount {
		t.Fatalf("first cumulative metrics = %+v, initial=%+v", firstMetrics, metrics)
	}
	if _, err := h.Reconciler.RunOnce(ctx); err != nil {
		t.Fatalf("second discrepancy pass: %v", err)
	}
	secondMetrics := diagnosticMetrics(t, base)
	if !secondMetrics.ReconcilerLastRun.After(firstMetrics.ReconcilerLastRun) ||
		secondMetrics.DiscrepancyCount <= firstMetrics.DiscrepancyCount {
		t.Fatalf("second cumulative metrics = %+v, first=%+v", secondMetrics, firstMetrics)
	}

	if code, _ := authenticatedGet(t, base+"/api/sessions/missing/status"); code != http.StatusNotFound {
		t.Fatalf("missing session status = %d, want 404", code)
	}
}

func TestStatusReconnectAndRestartClearPerAttachmentWriteEvidence(t *testing.T) {
	cfg := testConfig(t)
	cfg.Auth.Mode = "trusted_proxy"
	cfg.Auth.TrustedProxy.Addresses = []string{"127.0.0.1"}
	cfg.Server.AllowedOrigins = []string{"http://console.test"}
	h := upOrFail(t, cfg)

	ctx := context.Background()
	now := db.FormatTime(time.Now())
	if err := h.DB.WithTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			INSERT INTO projects (id, name, canonical_path, git_author_name, git_author_email, created_at, last_used_at)
			VALUES ('reconnect-project', 'reconnect', ?, 'Test Author', 'test@example.invalid', ?, ?)`,
			cfg.Projects.AllowedRoots[0], now, now)
		return err
	}); err != nil {
		t.Fatalf("seed reconnect project: %v", err)
	}
	info, err := h.Tmux.CreateSession(ctx, tmux.SessionSpec{
		Name: "diag-reconnect", WorkDir: cfg.Projects.AllowedRoots[0],
		Argv: []string{"/bin/sh"}, Generation: 1, Marker: "reconnect-marker",
	})
	if err != nil {
		t.Fatalf("create reconnect pane: %v", err)
	}
	t.Cleanup(func() { _ = h.Tmux.KillServer(context.Background()) })
	s := &session.Session{
		ID: "diag-reconnect", ProjectID: "reconnect-project", Provider: "shell",
		TmuxSocketPath: cfg.Tmux.SocketPath, TmuxSessionName: "diag-reconnect", Generation: 1,
		IdentityMarker: ptr("reconnect-marker"),
	}
	if err := h.Sessions.Create(ctx, s); err != nil {
		t.Fatalf("create reconnect row: %v", err)
	}
	s.TmuxSessionID, s.TmuxPaneID = &info.SessionID, &info.PaneID
	s.PanePID, s.ProcessGroupID, s.StartTicks = &info.PID, &info.ProcessGroupID, &info.StartTicks
	s.BootEpoch = testBootEpoch(t)
	s.ExpectedExecutable = &info.Executable
	if err := h.Sessions.MarkRunning(ctx, s); err != nil {
		t.Fatalf("mark reconnect row running: %v", err)
	}

	base, stop := startServer(t, h.Run, h.Server.BoundAddr)
	c1, token1 := dialDiagnosticTerminal(t, base, s.ID, "browser-a", cfg.Server.AllowedOrigins[0])
	c2, token2 := dialDiagnosticTerminal(t, base, s.ID, "browser-a", cfg.Server.AllowedOrigins[0])
	if token2 != token1 {
		t.Fatalf("same-controller reconnect token = %d, want %d", token2, token1)
	}

	sendDiagnosticInput(t, c1, token1, s.Generation, "echo old-overlap\n")
	awaitDiagnosticWrite(t, c1, terminal.WriteSent)
	connection := diagnosticConnection(t, base, s.ID)
	if connection.AttachmentCount != 2 || connection.LastWriteResult != "none" {
		t.Fatalf("old attachment outcome leaked through status: %+v", connection)
	}

	sendDiagnosticInput(t, c2, token2, s.Generation, "echo new-overlap\n")
	awaitDiagnosticWrite(t, c2, terminal.WriteSent)
	connection = diagnosticConnection(t, base, s.ID)
	if connection.LastWriteResult != terminal.WriteSent {
		t.Fatalf("current attachment outcome = %+v, want sent", connection)
	}

	if err := c1.Close(); err != nil {
		t.Fatalf("close old attachment: %v", err)
	}
	if err := c2.Close(); err != nil {
		t.Fatalf("close current attachment: %v", err)
	}
	deadline := time.Now().Add(30 * time.Second)
	for h.Terminal.ConnectionSnapshot(s.ID).AttachmentCount != 0 {
		if time.Now().After(deadline) {
			t.Fatal("attachments did not drain before service restart")
		}
		time.Sleep(10 * time.Millisecond)
	}
	stop()
	h.Close()

	restarted := upOrFail(t, cfg)
	restartedBase := serve(t, restarted)
	connection = diagnosticConnection(t, restartedBase, s.ID)
	if connection.AttachmentCount != 0 || connection.LastWriteResult != "none" {
		t.Fatalf("per-attachment write evidence survived service restart: %+v", connection)
	}
}

func dialDiagnosticTerminal(t *testing.T, base, sessionID, controllerID, origin string) (*websocket.Conn, int64) {
	t.Helper()
	u, err := url.Parse(base)
	if err != nil {
		t.Fatalf("parse base URL: %v", err)
	}
	u.Scheme = "ws"
	u.Path = "/api/sessions/" + sessionID + "/terminal"
	q := u.Query()
	q.Set("controller", controllerID)
	q.Set("cols", "80")
	q.Set("rows", "24")
	u.RawQuery = q.Encode()
	conn, _, err := websocket.DefaultDialer.Dial(u.String(), http.Header{
		"Origin":           []string{origin},
		"X-Forwarded-User": []string{"operator@example.test"},
	})
	if err != nil {
		t.Fatalf("dial terminal: %v", err)
	}
	for {
		var frame terminal.ServerFrame
		if err := conn.ReadJSON(&frame); err != nil {
			t.Fatalf("read attached frame: %v", err)
		}
		if frame.Type == terminal.TypeAttached {
			return conn, frame.Token
		}
	}
}

func sendDiagnosticInput(t *testing.T, conn *websocket.Conn, token int64, generation int, input string) {
	t.Helper()
	err := conn.WriteJSON(terminal.ClientFrame{
		Type: terminal.TypeInput, Token: token, Generation: generation,
		Data: base64.StdEncoding.EncodeToString([]byte(input)),
	})
	if err != nil {
		t.Fatalf("write terminal input: %v", err)
	}
}

func awaitDiagnosticWrite(t *testing.T, conn *websocket.Conn, want string) {
	t.Helper()
	if err := conn.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatalf("set terminal deadline: %v", err)
	}
	for {
		var frame terminal.ServerFrame
		if err := conn.ReadJSON(&frame); err != nil {
			t.Fatalf("read write result: %v", err)
		}
		if frame.Type == terminal.TypeWriteStatus {
			if frame.Code != want {
				t.Fatalf("write result = %s, want %s", frame.Code, want)
			}
			return
		}
	}
}

func diagnosticConnection(t *testing.T, base, sessionID string) struct {
	AttachmentCount int    `json:"attachment_count"`
	LastWriteResult string `json:"last_write_result"`
} {
	t.Helper()
	code, body := authenticatedGet(t, base+"/api/sessions/"+sessionID+"/status")
	if code != http.StatusOK {
		t.Fatalf("session status = %d (%s)", code, body)
	}
	var response struct {
		Connection struct {
			AttachmentCount int    `json:"attachment_count"`
			LastWriteResult string `json:"last_write_result"`
		} `json:"connection"`
	}
	if err := json.Unmarshal([]byte(body), &response); err != nil {
		t.Fatalf("decode session status: %v (%s)", err, body)
	}
	return response.Connection
}

type sessionStatusDiagnostic struct {
	Recorded struct {
		Status     string     `json:"status"`
		ExitCode   *int       `json:"exit_code"`
		ExitSignal *string    `json:"exit_signal"`
		ExitedAt   *time.Time `json:"exited_at"`
	} `json:"recorded"`
	Reconciliation struct {
		HasDiscrepancy *bool `json:"has_discrepancy"`
	} `json:"reconciliation"`
}

type metricsDiagnostic struct {
	ReconcilerLastRun         time.Time `json:"reconciler_last_run"`
	ReconcilerInterval        string    `json:"reconciler_interval"`
	DiscrepancyCount          int64     `json:"discrepancy_count"`
	ActiveSessions            int       `json:"active_sessions"`
	AutomaticRecoveryPending  int       `json:"automatic_recovery_pending"`
	AutomaticRecoveryInFlight int       `json:"automatic_recovery_in_flight"`
}

func diagnosticMetrics(t *testing.T, base string) metricsDiagnostic {
	t.Helper()
	code, body := authenticatedGet(t, base+"/api/metrics")
	if code != http.StatusOK {
		t.Fatalf("metrics = %d (%s)", code, body)
	}
	var metrics metricsDiagnostic
	if err := json.Unmarshal([]byte(body), &metrics); err != nil {
		t.Fatalf("decode metrics: %v (%s)", err, body)
	}
	assertNoRetiredDiagnosticFields(t, body)
	return metrics
}

func assertNoRetiredDiagnosticFields(t *testing.T, body string) {
	t.Helper()
	var payload any
	if err := json.Unmarshal([]byte(body), &payload); err != nil {
		t.Fatalf("decode diagnostic response for field audit: %v (%s)", err, body)
	}
	forbidden := []string{
		"composer", "outbox", "queue", "held", "released", "ambiguous", "oldest", "input",
	}
	var walk func(any)
	walk = func(value any) {
		switch value := value.(type) {
		case map[string]any:
			for key, child := range value {
				lower := strings.ToLower(key)
				for _, token := range forbidden {
					if strings.Contains(lower, token) {
						t.Fatalf("diagnostic response contains retired %q field %q: %s", token, key, body)
					}
				}
				walk(child)
			}
		case []any:
			for _, child := range value {
				walk(child)
			}
		}
	}
	walk(payload)
}

// seedLegacyAttention writes the released representation directly. Product
// code has no attention writer; upgrade tests alone may create this historical
// input so boot and first-read convergence remain mechanically covered.
func seedLegacyAttention(t *testing.T, database *db.DB, sessionID string) {
	t.Helper()
	if err := database.WithTx(context.Background(), func(ctx context.Context, tx *sql.Tx) error {
		now := db.FormatTime(time.Now())
		res, err := tx.ExecContext(ctx,
			`UPDATE sessions SET status = ?, updated_at = ? WHERE id = ? AND removed_at IS NULL`,
			session.StatusNeedsAttention, now, sessionID)
		if err != nil {
			return err
		}
		if n, err := res.RowsAffected(); err != nil || n != 1 {
			return fmt.Errorf("seed legacy attention row: affected=%d: %w", n, err)
		}
		return db.AppendAudit(ctx, tx, db.AuditEvent{
			SessionID: &sessionID, Type: session.AuditAttention,
			Details: "historical legacy fixture",
		})
	}); err != nil {
		t.Fatalf("seed legacy attention: %v", err)
	}
}

func ptr(s string) *string { return &s }
