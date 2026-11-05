package session

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/noisefloor-oss/deck/internal/db"
	"github.com/noisefloor-oss/deck/internal/hookcapture"
	"github.com/noisefloor-oss/deck/internal/provider"
)

const capturedCodexID = "12345678-1234-4abc-8abc-123456789abc"

func postCapture(t *testing.T, service *hookcapture.Service, token, body string) int {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/hooks/capture", strings.NewReader(body))
	req.Header.Set("X-Noisedeck-Hook-Token", token)
	rec := httptest.NewRecorder()
	service.Handle().ServeHTTP(rec, req)
	return rec.Code
}

func captureBody(id, source, cwd string) string {
	return `{"hook_event_name":"SessionStart","session_id":"` + id +
		`","cwd":"` + cwd + `","source":"` + source + `"}`
}

func startingCaptureFixture(t *testing.T) (*Repository, *Session, string) {
	t.Helper()
	mgr, repo, _, projectID := newTestEnv(t, sleeperDescriptor())
	ctx := context.Background()
	s, err := mgr.Create(ctx, CreateSpec{
		ProjectID: projectID, Provider: provider.Claude, WorkDir: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.db.WithTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE sessions
			SET provider = ?, native_session_id = NULL, status = ?,
				exit_code = NULL, exit_signal = NULL, exited_at = NULL,
				identity_capture_lost_at = NULL
			WHERE id = ?`, provider.Codex, StatusStarting, s.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	s, err = repo.Get(ctx, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	return repo, s, t.TempDir()
}

func TestDurableCaptureSurvivesServiceRestartAndReplaysIdempotently(t *testing.T) {
	repo, row, workDir := startingCaptureFixture(t)
	ctx := context.Background()
	first, err := hookcapture.NewService(repo, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	token, err := first.RegisterFenced(ctx, row.ID, row.Provider, workDir,
		row.Generation, *row.IdentityMarker)
	if err != nil {
		t.Fatal(err)
	}
	var persisted string
	if err := repo.db.Reader().QueryRow(`SELECT token_sha256
		FROM provider_identity_capture_grants WHERE session_id = ?`, row.ID).Scan(&persisted); err != nil {
		t.Fatal(err)
	}
	if persisted == token || len(persisted) != 64 {
		t.Fatalf("persisted bearer = %q, want only a 64-byte digest", persisted)
	}

	// A reconstructed envelope has no in-memory correlation to inherit; the
	// same database grant is nevertheless accepted.
	restarted, err := hookcapture.NewService(repo, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	body := captureBody(capturedCodexID, hookcapture.ExpectedSourceStartup, workDir)
	if got := postCapture(t, restarted, token, body); got != http.StatusNoContent {
		t.Fatalf("capture after service restart = %d, want 204", got)
	}
	if got := postCapture(t, restarted, token, body); got != http.StatusNoContent {
		t.Fatalf("lost-response replay = %d, want 204", got)
	}
	stored, err := repo.Get(ctx, row.ID)
	if err != nil || stored.NativeSessionID == nil || *stored.NativeSessionID != capturedCodexID {
		t.Fatalf("captured identity = %v err=%v", stored.NativeSessionID, err)
	}
	if got := countAuditType(t, repo, row.ID, AuditIdentityCaptured); got != 1 {
		t.Fatalf("identity-captured audits = %d, want 1: the replay was not a no-op", got)
	}
}

// The pane holding this generation's credential is the authority on which
// conversation it is running. A later event naming a different one is the
// pane saying it moved, whatever hook name or entry source carried it, and
// the row follows with the change audited old to new.
func TestAPaneReportingADifferentIdMovesTheRow(t *testing.T) {
	const movedID = "22345678-1234-4abc-8abc-123456789abc"
	for _, tc := range []struct{ name, hookEvent, source string }{
		{name: "fork", hookEvent: "SessionStart", source: "fork"},
		{name: "clear", hookEvent: "SessionStart", source: "clear"},
		{name: "compact", hookEvent: "SessionStart", source: "compact"},
		{name: "in-pane switch", hookEvent: "SessionSwitch", source: hookcapture.ExpectedSourceStartup},
		{name: "startup", hookEvent: "SessionStart", source: hookcapture.ExpectedSourceStartup},
		{name: "codex current", hookEvent: "Stop", source: "current"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo, row, workDir := startingCaptureFixture(t)
			ctx := context.Background()
			service, err := hookcapture.NewService(repo, time.Hour)
			if err != nil {
				t.Fatal(err)
			}
			token, err := service.RegisterFenced(ctx, row.ID, row.Provider, workDir,
				row.Generation, *row.IdentityMarker)
			if err != nil {
				t.Fatal(err)
			}
			if got := postCapture(t, service, token, captureBody(capturedCodexID,
				hookcapture.ExpectedSourceStartup, workDir)); got != http.StatusNoContent {
				t.Fatalf("first capture = %d, want 204", got)
			}
			moved := `{"hook_event_name":"` + tc.hookEvent + `","session_id":"` + movedID +
				`","cwd":"` + workDir + `","source":"` + tc.source + `"}`
			if got := postCapture(t, service, token, moved); got != http.StatusNoContent {
				t.Fatalf("move report = %d, want 204", got)
			}
			stored, err := repo.Get(ctx, row.ID)
			if err != nil || stored.NativeSessionID == nil || *stored.NativeSessionID != movedID {
				t.Fatalf("identity = %v err=%v, want the reported %s",
					stored.NativeSessionID, err, movedID)
			}
			details := auditDetails(t, repo, row.ID, AuditIdentityCaptured)
			if len(details) != 2 {
				t.Fatalf("identity-captured audits = %d, want the launch plus the move", len(details))
			}
			want := "from_native_id_sha256=" + hookcapture.NativeIDHash(capturedCodexID) +
				" to_native_id_sha256=" + hookcapture.NativeIDHash(movedID)
			if !strings.Contains(details[1], want) {
				t.Fatalf("move audit = %q, want it to state %q", details[1], want)
			}
			if strings.Contains(details[1], movedID) || strings.Contains(details[1], capturedCodexID) {
				t.Fatalf("move audit leaked a raw native id: %q", details[1])
			}
		})
	}
}

func TestCodexNativeResumeSelectionConsumesFreshLaunchGrant(t *testing.T) {
	repo, row, workDir := startingCaptureFixture(t)
	ctx := context.Background()
	service, err := hookcapture.NewService(repo, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	token, err := service.RegisterFenced(ctx, row.ID, row.Provider, workDir,
		row.Generation, *row.IdentityMarker)
	if err != nil {
		t.Fatal(err)
	}

	body := captureBody(capturedCodexID, hookcapture.ExpectedSourceResume, workDir)
	if got := postCapture(t, service, token, body); got != http.StatusNoContent {
		t.Fatalf("Codex native /resume capture = %d, want 204", got)
	}
	stored, err := repo.Get(ctx, row.ID)
	if err != nil || stored.NativeSessionID == nil || *stored.NativeSessionID != capturedCodexID {
		t.Fatalf("captured resumed identity = %v err=%v", stored.NativeSessionID, err)
	}
	if got := countAuditType(t, repo, row.ID, AuditIdentityCaptured); got != 1 {
		t.Fatalf("identity-captured audits = %d, want 1", got)
	}
}

// Codex reports its live conversation on the Stop hook, and the user's
// in-CLI switch lands there as a new id on the same long-lived grant.
func TestCodexStopHookMovesTheLiveIdentity(t *testing.T) {
	repo, row, workDir := startingCaptureFixture(t)
	ctx := context.Background()
	service, err := hookcapture.NewService(repo, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	token, err := service.RegisterFenced(ctx, row.ID, row.Provider, workDir,
		row.Generation, *row.IdentityMarker)
	if err != nil {
		t.Fatal(err)
	}
	first := captureBody(capturedCodexID, hookcapture.ExpectedSourceStartup, workDir)
	if got := postCapture(t, service, token, first); got != http.StatusNoContent {
		t.Fatalf("initial capture=%d, want 204", got)
	}
	if err := repo.db.WithTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE sessions SET status = ? WHERE id = ?`,
			StatusRunning, row.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	resumedID := "62345678-1234-4abc-8abc-123456789abc"
	resumed := `{"hook_event_name":"Stop","session_id":"` + resumedID +
		`","cwd":"` + workDir + `","source":"current"}`
	if got := postCapture(t, service, token, resumed); got != http.StatusNoContent {
		t.Fatalf("resumed registration=%d, want 204", got)
	}
	if got := postCapture(t, service, token, resumed); got != http.StatusNoContent {
		t.Fatalf("resumed replay=%d, want 204", got)
	}
	stored, err := repo.Get(ctx, row.ID)
	if err != nil || stored.NativeSessionID == nil || *stored.NativeSessionID != resumedID {
		t.Fatalf("rotated identity=%v err=%v", stored.NativeSessionID, err)
	}
	if got := countAuditType(t, repo, row.ID, AuditIdentityCaptured); got != 2 {
		t.Fatalf("identity audits=%d, want initial plus rotation", got)
	}
}

func TestCaptureRefusalRecordsContentFreeReason(t *testing.T) {
	repo, row, workDir := startingCaptureFixture(t)
	ctx := context.Background()
	service, err := hookcapture.NewService(repo, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	token, err := service.RegisterFenced(ctx, row.ID, row.Provider, workDir,
		row.Generation, *row.IdentityMarker)
	if err != nil {
		t.Fatal(err)
	}
	foreignDir := t.TempDir()
	body := captureBody(capturedCodexID, hookcapture.ExpectedSourceStartup, foreignDir)
	if got := postCapture(t, service, token, body); got != http.StatusConflict {
		t.Fatalf("mismatched capture = %d, want 409", got)
	}
	var details string
	if err := repo.db.Reader().QueryRow(`SELECT details FROM audit_events
		WHERE session_id = ? AND event_type = ?`, row.ID, AuditIdentityCaptureRefused).
		Scan(&details); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(details, "reason=working_directory") ||
		strings.Contains(details, workDir) || strings.Contains(details, foreignDir) ||
		strings.Contains(details, capturedCodexID) {
		t.Fatalf("refusal diagnostic is not content-free: %q", details)
	}
}

func TestDevinStartupCapturePersistsDocumentedPayloadWithoutCWD(t *testing.T) {
	repo, row, workDir := startingCaptureFixture(t)
	ctx := context.Background()
	if err := repo.db.WithTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE sessions SET provider = ? WHERE id = ?`,
			provider.Devin, row.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	row, err := repo.Get(ctx, row.ID)
	if err != nil {
		t.Fatal(err)
	}
	service, err := hookcapture.NewService(repo, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	token, err := service.RegisterFenced(ctx, row.ID, row.Provider, workDir,
		row.Generation, *row.IdentityMarker)
	if err != nil {
		t.Fatal(err)
	}
	body := captureBody("sample-session", hookcapture.ExpectedSourceStartup, "")
	if got := postCapture(t, service, token, body); got != http.StatusNoContent {
		t.Fatalf("Devin startup capture = %d, want 204", got)
	}
	stored, err := repo.Get(ctx, row.ID)
	if err != nil || stored.NativeSessionID == nil || *stored.NativeSessionID != "sample-session" {
		t.Fatalf("captured Devin identity = %v err=%v", stored.NativeSessionID, err)
	}
}

func TestCaptureConsumeRollsBackAsOneTransaction(t *testing.T) {
	repo, row, workDir := startingCaptureFixture(t)
	ctx := context.Background()
	service, err := hookcapture.NewService(repo, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	token, err := service.RegisterFenced(ctx, row.ID, row.Provider, workDir,
		row.Generation, *row.IdentityMarker)
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.db.WithTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `CREATE TRIGGER fail_capture_audit
			BEFORE INSERT ON audit_events
			WHEN NEW.event_type = 'session.identity_captured'
			BEGIN SELECT RAISE(ABORT, 'injected capture audit failure'); END`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	body := captureBody(capturedCodexID, hookcapture.ExpectedSourceStartup, workDir)
	if got := postCapture(t, service, token, body); got != http.StatusInternalServerError {
		t.Fatalf("faulted capture = %d, want 500", got)
	}
	var nativeID *string
	var state string
	if err := repo.db.Reader().QueryRow(`SELECT s.native_session_id, grant.state
		FROM sessions s JOIN provider_identity_capture_grants grant ON grant.session_id = s.id
		WHERE s.id = ?`, row.ID).Scan(&nativeID, &state); err != nil {
		t.Fatal(err)
	}
	if nativeID != nil || state != capturePending || countAuditType(t, repo, row.ID, AuditIdentityCaptured) != 0 {
		t.Fatalf("partial transaction survived: id=%v state=%q", nativeID, state)
	}
	if err := repo.db.WithTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `DROP TRIGGER fail_capture_audit`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if got := postCapture(t, service, token, body); got != http.StatusNoContent {
		t.Fatalf("retry after rollback = %d, want 204", got)
	}
}

func TestCaptureAtExpiryClosesBeforeAcceptance(t *testing.T) {
	repo, row, workDir := startingCaptureFixture(t)
	ctx := context.Background()
	service, err := hookcapture.NewService(repo, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.RegisterFenced(ctx, row.ID, row.Provider, workDir,
		row.Generation, *row.IdentityMarker); err != nil {
		t.Fatal(err)
	}
	var tokenHash, expiresRaw string
	if err := repo.db.Reader().QueryRow(`SELECT token_sha256, expires_at
		FROM provider_identity_capture_grants WHERE session_id = ?`, row.ID).
		Scan(&tokenHash, &expiresRaw); err != nil {
		t.Fatal(err)
	}
	expiresAt, err := time.Parse(db.TimeFormat, expiresRaw)
	if err != nil {
		t.Fatal(err)
	}
	err = repo.AcceptIdentityCapture(ctx, tokenHash, hookcapture.Event{
		HookEventName: "SessionStart", SessionID: capturedCodexID,
		CWD: workDir, Source: hookcapture.ExpectedSourceStartup,
	}, expiresAt)
	if !errors.Is(err, hookcapture.ErrUnknownToken) {
		t.Fatalf("capture at absolute expiry = %v, want unavailable", err)
	}
	stored, err := repo.Get(ctx, row.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.NativeSessionID != nil || stored.IdentityCaptureLostAt == nil {
		t.Fatalf("capture at expiry left id=%v loss=%v", stored.NativeSessionID,
			stored.IdentityCaptureLostAt)
	}
}

func resumeGrantFixture(t *testing.T, marker string) (*Repository, *Session,
	*hookcapture.Service, hookcapture.Credential, string) {
	t.Helper()
	mgr, repo, _, projectID := newTestEnv(t, sleeperDescriptor())
	ctx := context.Background()
	source := createInterrupted(t, mgr, projectID)
	if err := repo.db.WithTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE sessions SET provider = ? WHERE id = ?`,
			provider.Codex, source.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	source, err := repo.Get(ctx, source.ID)
	if err != nil {
		t.Fatal(err)
	}
	service, err := hookcapture.NewService(repo, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	workDir := t.TempDir()
	credential, grant, err := service.PrepareResumeGrant(source.ID, source.Provider,
		workDir, source.Generation+1, marker, *source.NativeSessionID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.BeginSuccessorSlot(ctx, source, marker, &grant, ""); err != nil {
		t.Fatal(err)
	}
	return repo, source, service, credential, workDir
}

// The provider reporting back the conversation Deck asked for changes
// nothing, and a replay of that report changes nothing either.
func TestExactResumeReportRecordsAndReplaysAsANoOp(t *testing.T) {
	repo, source, service, credential, workDir := resumeGrantFixture(t, "resume-marker")
	ctx := context.Background()
	exact := captureBody(*source.NativeSessionID, hookcapture.ExpectedSourceResume, workDir)
	if got := postCapture(t, service, credential.Token, exact); got != http.StatusNoContent {
		t.Fatalf("exact resume report = %d, want 204", got)
	}
	if got := postCapture(t, service, credential.Token, exact); got != http.StatusNoContent {
		t.Fatalf("exact report replay = %d, want 204", got)
	}
	if got := countAuditType(t, repo, source.ID, AuditIdentityConfirmed); got != 1 {
		t.Fatalf("identity-confirmed audits = %d, want 1: the replay was not a no-op", got)
	}
	slot, err := repo.SuccessorSlot(ctx, source.ID)
	if err != nil || slot == nil || slot.CaptureState == nil ||
		*slot.CaptureState != hookcapture.GrantConsumed {
		t.Fatalf("capture state = %+v err=%v", slot, err)
	}
	stored, _ := repo.Get(ctx, source.ID)
	if stored.NativeSessionID == nil || *stored.NativeSessionID != *source.NativeSessionID {
		t.Fatalf("exact report rewrote opaque identity: %v", stored.NativeSessionID)
	}
	if committed, err := repo.CommitSuccessorRunning(ctx, slot, stored); err != nil || !committed {
		t.Fatalf("successor commit = %v err=%v, want committed", committed, err)
	}
}

// Deck launched --resume X and the provider came up on Y. Refusing that would
// leave the row naming a conversation the pane is not on, so the next restart
// resumes the wrong one and the user's work looks lost. Record what the
// pane said and audit the discrepancy; the successor still commits, because a
// live pane is running whatever a callback reports.
func TestResumeLandingElsewhereIsRecordedAndAudited(t *testing.T) {
	repo, source, service, credential, workDir := resumeGrantFixture(t, "resume-marker")
	ctx := context.Background()
	if got := postCapture(t, service, credential.Token,
		captureBody(capturedCodexID, hookcapture.ExpectedSourceResume, workDir)); got != http.StatusNoContent {
		t.Fatalf("resume landing elsewhere = %d, want 204 recorded", got)
	}
	stored, err := repo.Get(ctx, source.ID)
	if err != nil || stored.NativeSessionID == nil || *stored.NativeSessionID != capturedCodexID {
		t.Fatalf("identity = %v err=%v, want the conversation the pane reported",
			stored.NativeSessionID, err)
	}
	details := auditDetails(t, repo, source.ID, AuditIdentityCaptured)
	if len(details) != 1 ||
		!strings.Contains(details[0], "the provider resumed a different conversation than the one requested") {
		t.Fatalf("audits = %q, want the discrepancy stated", details)
	}
	if !strings.Contains(details[0], "from_native_id_sha256="+
		hookcapture.NativeIDHash(*source.NativeSessionID)) {
		t.Fatalf("audit = %q, want the conversation Deck asked for named", details[0])
	}
	slot, err := repo.SuccessorSlot(ctx, source.ID)
	if err != nil || slot == nil {
		t.Fatalf("successor slot = %+v err=%v", slot, err)
	}
	if committed, err := repo.CommitSuccessorRunning(ctx, slot, stored); err != nil || !committed {
		t.Fatalf("successor commit = %v err=%v: recording the move broke the launch",
			committed, err)
	}

	// The requested conversation is a fact about the resume, so only the
	// report that answers it can diverge from it. A later move on the same
	// pane - a clear, a fork, the user picking another conversation - is
	// the pane doing something ordinary, and calling that a provider that
	// resumed the wrong conversation would put a false sentence in the log.
	const movedOnCodexID = "7c9e2f14-3b8a-4d55-9f61-2a0e5b7c1d38"
	if got := postCapture(t, service, credential.Token,
		captureBody(movedOnCodexID, hookcapture.ExpectedSourceResume, workDir)); got != http.StatusNoContent {
		t.Fatalf("later move on the same grant = %d, want 204 recorded", got)
	}
	moved := auditDetails(t, repo, source.ID, AuditIdentityCaptured)
	if len(moved) != 2 {
		t.Fatalf("audits = %d, want the later move recorded too: %q", len(moved), moved)
	}
	if strings.Contains(moved[1], "resumed a different conversation") {
		t.Fatalf("audit = %q, want an ordinary move stated as a move", moved[1])
	}
}

// Attestation decides what is KNOWN about a successor, never whether a running
// process may be recorded as running. A provider that resumed and stayed
// silent — a slow hook, a hook removed upstream — presents an identical live
// pane to one that reported in, and the user keeps it either way.
func TestUnattestedSuccessorStillCommits(t *testing.T) {
	mgr, repo, _, projectID := newTestEnv(t, sleeperDescriptor())
	ctx := context.Background()
	source := createInterrupted(t, mgr, projectID)
	if err := repo.db.WithTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE sessions SET provider = ? WHERE id = ?`,
			provider.Codex, source.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	source, _ = repo.Get(ctx, source.ID)
	service, _ := hookcapture.NewService(repo, time.Hour)
	_, grant, err := service.PrepareResumeGrant(source.ID, source.Provider,
		t.TempDir(), source.Generation+1, "silent-marker", *source.NativeSessionID)
	if err != nil {
		t.Fatal(err)
	}
	slot, err := repo.BeginSuccessorSlot(ctx, source, "silent-marker", &grant, "")
	if err != nil {
		t.Fatal(err)
	}
	candidate, _ := repo.Get(ctx, source.ID)
	committed, err := repo.CommitSuccessorRunning(ctx, slot, candidate)
	if err != nil || !committed {
		t.Fatalf("silent successor commit = %v err=%v, want committed", committed, err)
	}
	stored, err := repo.Get(ctx, source.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != StatusRunning {
		t.Fatalf("status = %s, want running", stored.Status)
	}
}

// The ttl is a delivery window for a launch that died, not a deadline the
// user can miss. A provider that reports its id at the first submission
// reports it whenever the user first types, so a live pane's pending
// grant must outlive the window; expiring it stranded 42 real sessions
// across six providers, each exactly one hour after it started.
func TestCaptureExpirySparesALivePane(t *testing.T) {
	repo, row, workDir := startingCaptureFixture(t)
	ctx := context.Background()
	service, _ := hookcapture.NewService(repo, time.Hour)
	if _, err := service.RegisterFenced(ctx, row.ID, row.Provider, workDir,
		row.Generation, *row.IdentityMarker); err != nil {
		t.Fatal(err)
	}
	if err := repo.db.WithTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE sessions SET status = ? WHERE id = ?`,
			StatusRunning, row.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := repo.ExpireIdentityCaptures(ctx, time.Now().Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	stored, _ := repo.Get(ctx, row.ID)
	if stored.IdentityCaptureLostAt != nil {
		t.Fatal("a running pane's pending capture was expired; its first submission can no longer report an id")
	}
	pending, err := repo.IdentityCapturePending(ctx, row.ID, row.Generation, *row.IdentityMarker, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if !pending {
		t.Fatal("the live pane's grant is gone, so a late hook has nothing to post to")
	}

	// Surviving the sweep is worth nothing unless the delivery it was
	// spared for is accepted: the user's first submission is the hook,
	// and it lands long past the window.
	var tokenHash, expiresRaw string
	if err := repo.db.Reader().QueryRow(`SELECT token_sha256, expires_at
		FROM provider_identity_capture_grants WHERE session_id = ?`, row.ID).
		Scan(&tokenHash, &expiresRaw); err != nil {
		t.Fatal(err)
	}
	expiresAt, err := time.Parse(db.TimeFormat, expiresRaw)
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.AcceptIdentityCapture(ctx, tokenHash, hookcapture.Event{
		HookEventName: "SessionStart", SessionID: capturedCodexID,
		CWD: workDir, Source: hookcapture.ExpectedSourceStartup,
	}, expiresAt.Add(time.Hour)); err != nil {
		t.Fatalf("late capture on a live pane = %v, want accepted", err)
	}
	delivered, _ := repo.Get(ctx, row.ID)
	if delivered.NativeSessionID == nil || *delivered.NativeSessionID != capturedCodexID {
		t.Fatalf("late capture recorded id %v, want %s", delivered.NativeSessionID, capturedCodexID)
	}

}

// The bound is the generation, not the clock: a grant whose pane is no longer
// the live running row is reclaimed on the ordinary sweep, so the exception
// cannot become an unbounded token.
func TestCaptureExpiryReclaimsAStoppedPane(t *testing.T) {
	repo, row, workDir := startingCaptureFixture(t)
	ctx := context.Background()
	service, _ := hookcapture.NewService(repo, time.Hour)
	if _, err := service.RegisterFenced(ctx, row.ID, row.Provider, workDir,
		row.Generation, *row.IdentityMarker); err != nil {
		t.Fatal(err)
	}
	if err := repo.db.WithTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE sessions
			SET status = ?, exit_code = 0, exited_at = ? WHERE id = ?`,
			StatusExited, db.FormatTime(time.Now()), row.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := repo.ExpireIdentityCaptures(ctx, time.Now().Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	stopped, _ := repo.Get(ctx, row.ID)
	if stopped.IdentityCaptureLostAt == nil {
		t.Fatal("a stopped pane's pending capture was not reclaimed")
	}
}

func TestCaptureExpiryMarksOnlyStartupIdentityLost(t *testing.T) {
	repo, row, workDir := startingCaptureFixture(t)
	ctx := context.Background()
	service, _ := hookcapture.NewService(repo, time.Hour)
	if _, err := service.RegisterFenced(ctx, row.ID, row.Provider, workDir,
		row.Generation, *row.IdentityMarker); err != nil {
		t.Fatal(err)
	}
	if err := repo.ExpireIdentityCaptures(ctx, time.Now().Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	stored, _ := repo.Get(ctx, row.ID)
	if stored.IdentityCaptureLostAt == nil {
		t.Fatal("startup expiry did not record capture loss")
	}

	mgr, resumeRepo, _, projectID := newTestEnv(t, sleeperDescriptor())
	resumeSource := createInterrupted(t, mgr, projectID)
	if err := resumeRepo.db.WithTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE sessions SET provider = ? WHERE id = ?`,
			provider.Codex, resumeSource.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	resumeSource, _ = resumeRepo.Get(ctx, resumeSource.ID)
	resumeService, _ := hookcapture.NewService(resumeRepo, time.Hour)
	_, grant, err := resumeService.PrepareResumeGrant(resumeSource.ID,
		resumeSource.Provider, t.TempDir(), resumeSource.Generation+1,
		"expiring-resume", *resumeSource.NativeSessionID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := resumeRepo.BeginSuccessorSlot(ctx, resumeSource,
		"expiring-resume", &grant, ""); err != nil {
		t.Fatal(err)
	}
	if err := resumeRepo.ExpireIdentityCaptures(ctx, time.Now().Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	resumeStored, _ := resumeRepo.Get(ctx, resumeSource.ID)
	if resumeStored.IdentityCaptureLostAt != nil {
		t.Fatal("resume-attestation expiry poisoned the already captured source identity")
	}
}

func TestOlderSequencedCaptureCannotRollBackNewerIdentity(t *testing.T) {
	const movedID = "22345678-1234-4abc-8abc-123456789abc"
	repo, row, workDir := startingCaptureFixture(t)
	ctx := context.Background()
	service, err := hookcapture.NewService(repo, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	token, err := service.RegisterFenced(ctx, row.ID, row.Provider, workDir,
		row.Generation, *row.IdentityMarker)
	if err != nil {
		t.Fatal(err)
	}
	event := func(id string, sequence int64) string {
		return fmt.Sprintf(`{"hook_event_name":"SessionSwitch","session_id":%q,"cwd":%q,"source":"startup","sequence":%d}`,
			id, workDir, sequence)
	}
	if got := postCapture(t, service, token, event(capturedCodexID, 1)); got != http.StatusNoContent {
		t.Fatalf("first capture = %d, want 204", got)
	}
	if got := postCapture(t, service, token, event(movedID, 3)); got != http.StatusNoContent {
		t.Fatalf("newer capture = %d, want 204", got)
	}
	afterNewer, err := repo.Get(ctx, row.ID)
	if err != nil {
		t.Fatal(err)
	}
	revisionAfterNewer := afterNewer.ControllerRevision
	if got := postCapture(t, service, token, event(capturedCodexID, 2)); got != http.StatusNoContent {
		t.Fatalf("delayed older capture = %d, want an idempotent 204", got)
	}
	stored, err := repo.Get(ctx, row.ID)
	if err != nil || stored.NativeSessionID == nil || *stored.NativeSessionID != movedID {
		t.Fatalf("identity rolled back to %v, want newest %s", stored.NativeSessionID, movedID)
	}
	if stored.ControllerRevision != revisionAfterNewer {
		t.Fatalf("controller revision advanced on delayed duplicate: %d -> %d", revisionAfterNewer, stored.ControllerRevision)
	}
	if got := countAuditType(t, repo, row.ID, AuditIdentityCaptured); got != 2 {
		t.Fatalf("identity-captured audits = %d, want only the first and newer identities", got)
	}
	var acceptedSequence int64
	if err := repo.db.Reader().QueryRow(`SELECT accepted_sequence
		FROM provider_identity_capture_grants WHERE session_id = ?`, row.ID).Scan(&acceptedSequence); err != nil {
		t.Fatal(err)
	}
	if acceptedSequence != 3 {
		t.Fatalf("accepted sequence = %d, want 3", acceptedSequence)
	}
}
