package session

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/noisefloor-oss/deck/internal/hookcapture"
	"github.com/noisefloor-oss/deck/internal/inputgate"
	"github.com/noisefloor-oss/deck/internal/lease"
	"github.com/noisefloor-oss/deck/internal/project"
	"github.com/noisefloor-oss/deck/internal/provider"
	"github.com/noisefloor-oss/deck/internal/tmux"
)

// ladderDescriptor is a claude-shaped fake for the user's recover:
// assigned identity, structured-hook tracking, exact-resume attestation — and
// a resume that exits the way a provider answers an identity it does not
// know: immediately by default, or after resumeLife when the test wants the
// pane alive at recovery's first look. Fresh launches sleep and expose their
// capture token.
type ladderDescriptor struct {
	*fakeDescriptor
	tokenFile   string
	resumes     int
	launchSpecs []provider.LaunchSpec
	resumeLife  string
}

func (d *ladderDescriptor) IdentityCaptureMechanisms() []provider.IdentityCaptureMechanism {
	return []provider.IdentityCaptureMechanism{
		provider.IdentityCaptureAssignedAtLaunch,
		provider.IdentityCaptureStructuredNativeHook,
	}
}

func (d *ladderDescriptor) ExactResumeHookSource() string {
	return hookcapture.ExpectedSourceResume
}

func (d *ladderDescriptor) NewSessionArgs(spec provider.LaunchSpec) ([]string, error) {
	d.launchSpecs = append(d.launchSpecs, spec)
	script := `printf '%s' "$NOISEDECK_SESSION_HOOK_TOKEN" > "$1"; sleep 300`
	return []string{d.executable, "-c", script, "launch", d.tokenFile}, nil
}

func (d *ladderDescriptor) ResumeSessionArgs(spec provider.ResumeSpec) ([]string, error) {
	if spec.WorkDir != "" {
		d.resumes++
	}
	if d.resumeLife != "" {
		return []string{d.executable, "-c", "echo 'provider: no such conversation'; sleep " + d.resumeLife + "; exit 1"}, nil
	}
	return []string{d.executable, "-c", "exit 1"}, nil
}

func newLadderEnv(t *testing.T) (*Manager, *Repository, *ladderDescriptor,
	*hookcapture.Service, string, string) {
	t.Helper()
	desc := &ladderDescriptor{
		fakeDescriptor: sleeperDescriptor(),
		tokenFile:      filepath.Join(t.TempDir(), "token"),
	}
	mgr, repo, _, projectID := newTestEnv(t, desc)
	// Recovery resolves its work directory through the project resolver, and
	// the recovery grant binds to THAT canonical directory; the tests post
	// their hook events against it.
	recoveryDir := t.TempDir()
	mgr.SetResumeDeps(lease.NewManager(repo.db), inputgate.New(),
		func(context.Context, string) (string, error) { return recoveryDir, nil })
	captures, err := hookcapture.NewService(repo, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	mgr.SetHookCaptureDeps(captures, "http://127.0.0.1:8837")
	return mgr, repo, desc, captures, projectID, recoveryDir
}

// The user's click asks the provider once and reports its answer. A
// refused resume keeps the recorded identity, keeps the row and its pane,
// advances the generation to the attempt that was made, and launches nothing
// fresh in its place: a new conversation is the card's separate choice.
func TestUserRecoverReportsARefusedResumeAndKeepsTheRecord(t *testing.T) {
	mgr, repo, desc, _, projectID, _ := newLadderEnv(t)
	ctx := context.Background()
	s, err := mgr.Create(ctx, CreateSpec{
		ProjectID: projectID, Provider: provider.Claude, WorkDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	birthID := *s.NativeSessionID
	crashAndObserve(t, mgr, s)
	launchesBefore := len(desc.launchSpecs)

	revived, outcome, err := mgr.Recover(ctx, s.ID)
	if !errors.Is(err, ErrResumeRefusedByProvider) {
		t.Fatalf("Recover = (%v, %q, %v), want the provider's refusal", revived, outcome, err)
	}
	if desc.resumes != 1 || len(desc.launchSpecs) != launchesBefore {
		t.Fatalf("resumes=%d launches=%d, want one resume and no fresh launch",
			desc.resumes, len(desc.launchSpecs)-launchesBefore)
	}
	row := requireKeptIdentity(t, repo, s.ID, birthID)
	if row.Status != StatusExited || row.ResumeAttempts != 1 {
		t.Fatalf("row after refusal = status %s attempts %d, want terminal with the sweep stopped",
			row.Status, row.ResumeAttempts)
	}
	if got := mgr.RecoveryCapabilityFor(row); got != RecoveryCapabilityResume {
		t.Fatalf("recovery capability = %v, want resume still offered", got)
	}
	// Replace in place: one row, one card, never a sibling.
	rows, err := repo.ListByProject(ctx, projectID)
	if err != nil || len(rows) != 1 {
		t.Fatalf("session rows = %d err=%v, want exactly 1", len(rows), err)
	}
}

// claude answers an unknown id in about a second: the pane is alive when
// recovery first looks and gone before the resume grant could be attested.
// The attestation wait notices the death, so the user's click is answered
// with the refusal instead of a commit the reconciler contradicts seconds
// later — and, as above, nothing is forgotten or replaced.
func TestUserRecoverNoticesARefusalDuringAttestation(t *testing.T) {
	mgr, repo, desc, _, projectID, _ := newLadderEnv(t)
	desc.resumeLife = "0.6"
	ctx := context.Background()
	s, err := mgr.Create(ctx, CreateSpec{
		ProjectID: projectID, Provider: provider.Claude, WorkDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	birthID := *s.NativeSessionID
	crashAndObserve(t, mgr, s)

	started := time.Now()
	_, _, err = mgr.Recover(ctx, s.ID)
	if !errors.Is(err, ErrResumeRefusedByProvider) {
		t.Fatalf("Recover = %v, want the refusal noticed during attestation", err)
	}
	if elapsed := time.Since(started); elapsed >= mgr.resumeAttestationBound {
		t.Fatalf("the refusal took %s, the full attestation bound; the pane's death was not watched", elapsed)
	}
	row := requireKeptIdentity(t, repo, s.ID, birthID)
	if row.Generation != s.Generation+1 || row.Status != StatusExited || !row.GenerationResumed ||
		row.ExitCode == nil || *row.ExitCode != 1 || row.ResumeAttempts != 1 {
		t.Fatalf("row = gen %d status %s resumed=%v code=%v attempts=%d, want the refused attempt recorded as the newest generation with the sweep stopped",
			row.Generation, row.Status, row.GenerationResumed, row.ExitCode, row.ResumeAttempts)
	}
	if row.TmuxPaneID == nil {
		t.Fatal("the refused attempt's pane was not recorded")
	}
	if info, err := mgr.tmux.PaneInfoByID(ctx, *row.TmuxPaneID); err != nil || !info.Dead {
		t.Fatalf("the refused attempt's pane is gone (%v): its output is the user's only explanation", err)
	}
	if !containsDetail(auditDetails(t, repo, s.ID, AuditResumeAttempt), refusedResumeAuditDetail) {
		t.Fatal("the refusal was not recorded in the audit trail")
	}
	presented, err := mgr.Present(ctx, row)
	if err != nil {
		t.Fatal(err)
	}
	if presented.PresentationState != PresentationResumeEnded {
		t.Fatalf("presentation = %s, want resume_ended", presented.PresentationState)
	}
}

// The attested-provider nil-identity refusal must not reach the fresh rung: a
// row whose identity was already forgotten (the provider refused it earlier,
// or the row predates identity tracking) relaunches fresh in one request
// instead of answering 409 to a click the card explicitly offers.
func TestUserRecoverFreshRungBypassesAttestedNilIdentityCheck(t *testing.T) {
	mgr, repo, desc, _, projectID, _ := newLadderEnv(t)
	ctx := context.Background()
	s, err := mgr.Create(ctx, CreateSpec{
		ProjectID: projectID, Provider: provider.Claude, WorkDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	crashAndObserve(t, mgr, s)
	if err := repo.db.WithTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`UPDATE sessions SET native_session_id = NULL WHERE id = ?`, s.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}

	forgotten, err := repo.Get(ctx, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := mgr.RecoveryCapabilityFor(forgotten); got != RecoveryCapabilityRelaunch {
		t.Fatalf("forgotten assigned row capability = %v, want relaunch", got)
	}
	revived, outcome, err := mgr.Recover(ctx, s.ID)
	if err != nil {
		t.Fatalf("Recover of a forgotten-identity row: %v", err)
	}
	if outcome != RecoveryOutcomeRelaunched || revived.Status != StatusRunning {
		t.Fatalf("outcome=%q status=%s, want relaunched running", outcome, revived.Status)
	}
	if desc.resumes != 0 {
		t.Fatalf("a forgotten identity was asked again (%d resumes)", desc.resumes)
	}
	if revived.NativeSessionID == nil || *revived.NativeSessionID == s.ID {
		t.Fatalf("fresh identity = %v, want a new single-use uuid", revived.NativeSessionID)
	}
}

// The recover response names what committed; a refusal keeps the {error,
// message} envelope with a specific sentence the card can show, and the row
// it describes is still there to act on afterwards.
func TestRecoverEndpointReportsTheRefusal(t *testing.T) {
	mgr, repo, desc, _, projectID, _ := newLadderEnv(t)
	desc.resumeLife = "0.6"
	h := NewHandler(mgr, repo, project.NewRepository(repo.db), zerolog.Nop())
	mux := http.NewServeMux()
	h.Mount(mux.Handle, func(next http.Handler) http.Handler { return next })
	ctx := context.Background()
	s, err := mgr.Create(ctx, CreateSpec{
		ProjectID: projectID, Provider: provider.Claude, WorkDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	crashAndObserve(t, mgr, s)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost,
		"/api/sessions/"+s.ID+"/recover", nil))
	if rec.Code != http.StatusConflict {
		t.Fatalf("refused recover = %d body %s, want 409", rec.Code, rec.Body.String())
	}
	var refusal struct {
		Error   string `json:"error"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &refusal); err != nil {
		t.Fatalf("decode refusal envelope: %v", err)
	}
	if refusal.Error != "resume_refused" ||
		!strings.Contains(refusal.Message, "did not continue the recorded conversation") ||
		!strings.Contains(refusal.Message, "fresh session") {
		t.Fatalf("refusal envelope = %+v, want resume_refused with the kept-record sentence", refusal)
	}

	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/sessions/"+s.ID, nil))
	var detail struct {
		PresentationState string  `json:"presentation_state"`
		RecoveryRefusal   string  `json:"recovery_refusal"`
		NativeSessionID   *string `json:"native_session_id"`
		Generation        int     `json:"generation"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &detail); err != nil {
		t.Fatalf("decode session: %v", err)
	}
	if detail.PresentationState != string(PresentationResumeEnded) ||
		detail.RecoveryRefusal != string(RecoveryRefusalProvider) ||
		detail.NativeSessionID == nil || *detail.NativeSessionID != *s.NativeSessionID ||
		detail.Generation != s.Generation+1 {
		t.Fatalf("session after refusal = %+v, want resume_ended by the provider on generation %d with the birth identity",
			detail, s.Generation+1)
	}

	// The provider's own words are the user's explanation, and the pane
	// that holds them is served as bytes for the card to show.
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/sessions/"+s.ID+"/screen", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "provider: no such conversation") {
		t.Fatalf("screen = %d %q, want the refused pane's output", rec.Code, rec.Body.String())
	}
}

// A live row's screen belongs to the terminal attachment; a row whose pane is
// gone has nothing retained. Neither is served here.
func TestScreenIsServedOnlyForARetainedDeadPane(t *testing.T) {
	mgr, repo, _, _, projectID, _ := newLadderEnv(t)
	h := NewHandler(mgr, repo, project.NewRepository(repo.db), zerolog.Nop())
	mux := http.NewServeMux()
	h.Mount(mux.Handle, func(next http.Handler) http.Handler { return next })
	ctx := context.Background()
	s, err := mgr.Create(ctx, CreateSpec{
		ProjectID: projectID, Provider: provider.Claude, WorkDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/sessions/"+s.ID+"/screen", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("screen of a running session = %d, want 404", rec.Code)
	}
	crashAndObserve(t, mgr, s)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/sessions/"+s.ID+"/screen", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("screen of a crashed session's retained pane = %d, want 200", rec.Code)
	}
	if err := mgr.tmux.KillSession(ctx, s.TmuxSessionName); err != nil {
		t.Fatalf("remove the retained pane: %v", err)
	}
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/sessions/"+s.ID+"/screen", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("screen after the pane is gone = %d, want 404", rec.Code)
	}
}

// A pane a user reclaims while the attestation wait is watching it is
// not the candidate any more. Its occupant's death must not be read as the
// provider's refusal, must not retire the slot, and must never buy the
// cleanup that would remove the user's pane: the recovery ends with an
// identity mismatch and the reclaimed pane stands.
func TestAttestationWaitRefusesAReclaimedPane(t *testing.T) {
	mgr, repo, desc, _, projectID, _ := newLadderEnv(t)
	desc.resumeLife = "5"
	ctx := context.Background()
	s, err := mgr.Create(ctx, CreateSpec{
		ProjectID: projectID, Provider: provider.Claude, WorkDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	birthID := *s.NativeSessionID
	crashAndObserve(t, mgr, s)

	socket := mgr.tmux.SocketPath()
	reclaimed := make(chan error, 1)
	mgr.lifecycleHooks = &lifecycleTestHooks{resumeSpawned: func(info *tmux.PaneInfo) {
		go func() {
			time.Sleep(500 * time.Millisecond)
			reclaimed <- exec.Command("tmux", "-S", socket, "respawn-pane", "-k",
				"-t", info.PaneID, "--", "sh", "-c", "sleep 0.3; exit 7").Run()
		}()
	}}
	defer func() { mgr.lifecycleHooks = nil }()

	_, _, err = mgr.Recover(ctx, s.ID)
	if !errors.Is(err, ErrIdentityMismatch) || errors.Is(err, ErrResumeRefusedByProvider) {
		t.Fatalf("Recover over a reclaimed pane = %v, want an identity mismatch and no refusal", err)
	}
	if err := <-reclaimed; err != nil {
		t.Fatalf("out-of-band respawn: %v", err)
	}
	row := requireKeptIdentity(t, repo, s.ID, birthID)
	if row.TmuxPaneID == nil {
		t.Fatal("the row lost its pane")
	}
	// The recovery answered before the user's command finished; the
	// pane must still be there when it does, holding that command's exit.
	deadline := time.Now().Add(30 * time.Second)
	for {
		info, err := mgr.tmux.PaneInfoByID(ctx, *row.TmuxPaneID)
		if err != nil {
			t.Fatalf("the user's reclaimed pane was removed: %v", err)
		}
		if info.Dead && info.ExitStatus != nil {
			if *info.ExitStatus != 7 {
				t.Fatalf("reclaimed pane exit = %d, want the user's own 7", *info.ExitStatus)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("reclaimed pane = dead=%v status=%v, want the user's own exit 7 left in place", info.Dead, info.ExitStatus)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if containsDetail(auditDetails(t, repo, s.ID, AuditResumeAttempt), refusedResumeAuditDetail) {
		t.Fatal("a stranger's exit was recorded as the provider's refusal")
	}
}
