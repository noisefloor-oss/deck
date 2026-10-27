package session

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/noisefloor-oss/deck/internal/inputgate"
	"github.com/noisefloor-oss/deck/internal/lease"
	"github.com/noisefloor-oss/deck/internal/provider"
)

// resumeRefusingDescriptor resumes into an immediate exit, the way a provider
// behaves when it does not know the identity it was handed, and starts
// normally when launched fresh.
type resumeRefusingDescriptor struct {
	*fakeDescriptor
	resumes  int
	launches int
}

func (d *resumeRefusingDescriptor) NewSessionArgs(spec provider.LaunchSpec) ([]string, error) {
	d.launches++
	return d.fakeDescriptor.NewSessionArgs(spec)
}

// Recovery always names a work directory; the capability probe never does,
// and a probe is not a question put to the provider.
func (d *resumeRefusingDescriptor) ResumeSessionArgs(spec provider.ResumeSpec) ([]string, error) {
	if spec.WorkDir != "" {
		d.resumes++
	}
	return []string{d.executable, "-c", "exit 1"}, nil
}

// A provider that refuses the recorded identity is not asked again by the
// unattended sweep — retrying the identical resume argv answers the same way
// at every reconciliation interval — but nothing is forgotten and nothing is
// replaced: the identity stays on the row, the user's explicit resume may
// ask again, and a fresh conversation is never launched in the refused one's
// place.
func TestRefusedResumeKeepsTheIdentityAndStopsAsking(t *testing.T) {
	base := sleeperDescriptor()
	base.contract = provider.IdentityAssigned
	desc := &resumeRefusingDescriptor{fakeDescriptor: base}
	mgr, repo, _, projectID := newTestEnv(t, desc)
	mgr.SetResumeDeps(lease.NewManager(repo.db), inputgate.New(), testWorkDirResolver(t))

	ctx := context.Background()
	s, err := mgr.Create(ctx, CreateSpec{
		ProjectID: projectID, Provider: provider.Claude, WorkDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	crashAndObserve(t, mgr, s)
	source, err := repo.Get(ctx, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if source.NativeSessionID == nil {
		t.Fatal("an assigned-identity session recorded no identity to refuse")
	}
	birthID := *source.NativeSessionID

	// The unattended sweep resumes once; the provider refuses.
	if _, err := mgr.recover(ctx, s.ID, "", false); !errors.Is(err, ErrResumeRefusedByProvider) {
		t.Fatalf("automatic resume into an immediate exit = %v, want the refusal", err)
	}
	// An exit this fast races tmux's read of the process table: when the
	// tuple was read, the refused attempt is committed as the row's newest
	// generation with its exit; when it was not, the slot retires and the
	// row keeps the crash's evidence. Either way the row is terminal.
	afterRefusal := requireKeptIdentity(t, repo, s.ID, birthID)
	if afterRefusal.Status != StatusExited {
		t.Fatalf("row after refusal = status %s, want exited", afterRefusal.Status)
	}
	if afterRefusal.GenerationResumed && (afterRefusal.ExitCode == nil || *afterRefusal.ExitCode != 1) {
		t.Fatalf("committed refused attempt carries exit %v, want the provider's exit 1", afterRefusal.ExitCode)
	}
	if afterRefusal.ResumeAttempts != 1 {
		t.Fatalf("resume_attempts = %d after a refusal, want the opportunity spent", afterRefusal.ResumeAttempts)
	}
	if automaticRecoveryOpportunity(afterRefusal, desc, true, mgr.now()) {
		t.Fatal("the sweep would ask the provider the same question again")
	}
	if err := mgr.AutoResumeSweep(ctx); err != nil {
		t.Fatalf("sweep after a refusal: %v", err)
	}
	if desc.resumes != 1 {
		t.Fatalf("resume argv built %d times; the refused identity was asked again unattended", desc.resumes)
	}
	if got := mgr.RecoveryCapabilityFor(afterRefusal); got != RecoveryCapabilityResume {
		t.Fatalf("recovery capability = %v, want resume still offered", got)
	}

	// The user's explicit resume asks again and is refused again; still
	// no fresh conversation, still the same identity, still one row.
	if _, _, err := mgr.Recover(ctx, s.ID); !errors.Is(err, ErrResumeRefusedByProvider) {
		t.Fatalf("user Recover = %v, want the refusal", err)
	}
	requireKeptIdentity(t, repo, s.ID, birthID)
	if desc.resumes != 2 || desc.launches != 1 {
		t.Fatalf("resumes=%d launches=%d, want a second resume and no fresh launch",
			desc.resumes, desc.launches)
	}
	rows, err := repo.ListByProject(ctx, projectID)
	if err != nil || len(rows) != 1 {
		t.Fatalf("session rows = %d err=%v, want exactly 1", len(rows), err)
	}
}

func requireKeptIdentity(t *testing.T, repo *Repository, id, want string) *Session {
	t.Helper()
	row, err := repo.Get(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if row.NativeSessionID == nil || *row.NativeSessionID != want {
		t.Fatalf("recorded identity = %v, want %s kept", row.NativeSessionID, want)
	}
	return row
}

func containsDetail(details []string, want string) bool {
	for _, detail := range details {
		if detail == want {
			return true
		}
	}
	return false
}

// A retrieved-identity provider whose capture never arrived still has a
// working CLI and a pane of its own, so recovery starts a new conversation in
// that pane. Refusing to launch anything is the worse of the two outcomes: it
// leaves a card the user cannot act on at all, attached to a harness that
// runs fine from a shell. A restart stranded 23 sessions this way.
//
// The fixture builds the row rather than the capture path, because the row is
// what recovery reads: a retrieved-identity provider, terminal, no identity.
func TestRetrievedProviderWithNoCapturedIdentityRelaunchesInPlace(t *testing.T) {
	desc := sleeperDescriptor()
	mgr, repo, _, projectID := newTestEnv(t, desc)
	mgr.SetResumeDeps(lease.NewManager(repo.db), inputgate.New(), testWorkDirResolver(t))

	ctx := context.Background()
	s, err := mgr.Create(ctx, CreateSpec{
		ProjectID: projectID, Provider: provider.Claude, WorkDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	crashAndObserve(t, mgr, s)

	// The capture expired before delivery, which is how these rows are born.
	desc.contract = provider.IdentityRetrieved
	if err := repo.db.WithTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`UPDATE sessions SET native_session_id = NULL WHERE id = ?`, s.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	source, err := repo.Get(ctx, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if source.NativeSessionID != nil {
		t.Fatal("fixture kept an identity the provider never delivered")
	}

	if got := mgr.RecoveryCapabilityFor(source); got != RecoveryCapabilityRelaunch {
		t.Fatalf("recovery capability = %v, want relaunch: the user was offered nothing", got)
	}
	revived, _, err := mgr.Recover(ctx, s.ID)
	if err != nil {
		t.Fatalf("relaunch without a captured identity: %v", err)
	}
	if revived.Status != StatusRunning {
		t.Fatalf("status = %s, want running", revived.Status)
	}
	// Replace in place: one row, one card, never a sibling.
	rows, err := repo.ListByProject(ctx, projectID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("session rows = %d, want 1: recovery created a second card", len(rows))
	}
}

// lateRefusingDescriptor resumes into a process that lives long enough to be
// committed as the running successor and THEN exits, which is how omp behaves:
// it starts, discovers it does not know the id, prints its complaint and is
// gone in about four seconds. The first-observation check cannot see this —
// the pane is alive when recovery looks at it.
type lateRefusingDescriptor struct {
	*fakeDescriptor
	resumes int
}

func (d *lateRefusingDescriptor) ResumeSessionArgs(spec provider.ResumeSpec) ([]string, error) {
	if spec.WorkDir != "" {
		d.resumes++
	}
	return []string{d.executable, "-c", "sleep 1; exit 1"}, nil
}

// A resume the provider rejects on the way UP is judged even though the
// process lived long enough to commit. omp announces its session id at process
// launch but only writes the session to disk once the conversation has
// content, and claude writes nothing before the first submission, so a
// session launched and never typed into leaves an id the provider will never
// recognise, and every unattended replay of that id is answered the same way
// after a few seconds of life. The judgement closes the unattended
// opportunity and nothing else: the identity stays, and the row is presented
// for the user to decide.
func TestResumeThatDiesOnTheWayUpKeepsTheIdentityAndIsPresented(t *testing.T) {
	base := sleeperDescriptor()
	base.contract = provider.IdentityAssigned
	desc := &lateRefusingDescriptor{fakeDescriptor: base}
	mgr, repo, _, projectID := newTestEnv(t, desc)
	mgr.SetResumeDeps(lease.NewManager(repo.db), inputgate.New(), testWorkDirResolver(t))

	ctx := context.Background()
	s, err := mgr.Create(ctx, CreateSpec{
		ProjectID: projectID, Provider: provider.Claude, WorkDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	birthID := *s.NativeSessionID
	crashAndObserve(t, mgr, s)

	// The resume commits: the pane is alive when recovery observes it, so the
	// refusal is invisible to the first-observation check.
	revived, err := mgr.recover(ctx, s.ID, "", false)
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	if revived.Status != StatusRunning {
		t.Fatalf("status = %s, want the refused resume to commit first", revived.Status)
	}

	waitForPaneExit(t, mgr, revived)
	if _, err := mgr.Observe(ctx, revived); err != nil {
		t.Fatalf("observe the refused resume: %v", err)
	}
	row := requireKeptIdentity(t, repo, s.ID, birthID)
	if row.ResumeAttempts != 1 || automaticRecoveryOpportunity(row, desc, true, mgr.now()) {
		t.Fatalf("resume_attempts = %d; the sweep would ask the provider again", row.ResumeAttempts)
	}
	if desc.resumes != 1 {
		t.Fatalf("resume argv built %d times, want exactly 1", desc.resumes)
	}
	if !containsDetail(auditDetails(t, repo, s.ID, AuditResumeAttempt), refusedResumeAuditDetail) {
		t.Fatal("the refusal was not recorded in the audit trail")
	}
	if got := mgr.RecoveryCapabilityFor(row); got != RecoveryCapabilityResume {
		t.Fatalf("recovery capability = %v, want resume still offered", got)
	}
	// The card can act on the row: it is presented, not hidden as ended.
	presented, err := mgr.Present(ctx, row)
	if err != nil {
		t.Fatal(err)
	}
	if presented.PresentationState != PresentationResumeEnded {
		t.Fatalf("presentation = %s, want resume_ended", presented.PresentationState)
	}
}

func waitForPaneExit(t *testing.T, mgr *Manager, s *Session) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	deadline := time.Now().Add(20 * time.Second)
	for {
		// A read that lands while the process is on its way out fails
		// transiently — /proc/<pid>/exe is already gone. That is the state
		// being waited for, not a reason to stop waiting.
		if info, err := mgr.tmux.PaneInfoByID(ctx, *s.TmuxPaneID); err == nil && info.Dead {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the refused resume never exited")
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// The refusal window judges only a resumed generation that FAILED fast. Each
// exemption here is a conversation that would otherwise be discarded.
func TestSurvivingResumeKeepsItsIdentity(t *testing.T) {
	now := time.Now()
	id := "01a00e45-5dce-7000-a1e2-6b3ef003dfe9"
	failed := 1
	clean := 0
	sig := "9"
	fresh := now.Add(-time.Second)
	base := func() *Session {
		started := fresh
		return &Session{
			GenerationResumed:   true,
			GenerationStartedAt: &started,
			NativeSessionID:     &id,
		}
	}

	if !refusedResume(base(), &failed, nil, now) {
		t.Fatal("a resume that failed inside the window was not called a refusal")
	}
	if !refusedResume(base(), nil, &sig, now) {
		t.Fatal("a resume killed by a signal inside the window was not a refusal")
	}

	// Outlived the window: the provider accepted the id and worked.
	old := base()
	outlived := now.Add(-RefusedResumeWindow - time.Second)
	old.GenerationStartedAt = &outlived
	if refusedResume(old, &failed, nil, now) {
		t.Fatal("a resume that outlived the window was called a refusal")
	}

	// Exited cleanly: the user read an answer and typed /exit. Their
	// conversation must still be there next time.
	if refusedResume(base(), &clean, nil, now) {
		t.Fatal("a clean exit was called a refusal")
	}

	// The user asked it to stop; how fast it obeyed says nothing about the id.
	stopped := base()
	stopped.StopRequestedAt = &now
	if refusedResume(stopped, &failed, nil, now) {
		t.Fatal("a user-requested stop was called a refusal")
	}

	// A generation Deck launched fresh never carried an identity to refuse.
	launched := base()
	launched.GenerationResumed = false
	if refusedResume(launched, &failed, nil, now) {
		t.Fatal("a fresh launch was judged as a refused resume")
	}

	// Rows written before the column existed have no lifetime to measure.
	unknown := base()
	unknown.GenerationStartedAt = nil
	if refusedResume(unknown, &failed, nil, now) {
		t.Fatal("an unknown generation start was read as a short life")
	}
}

// The judgement must be applied by whichever observer records the exit, not
// only by the read path. The reconciler's cadence pass is almost always first:
// the refused process is gone seconds after commit, and once the reconciler
// has recorded that exit the row is terminal and Observe returns early without
// looking. A check that lives only on the read path never runs in the loop it
// was written to end.
func TestReconcilerPathAlsoClosesARefusedResume(t *testing.T) {
	base := sleeperDescriptor()
	base.contract = provider.IdentityAssigned
	desc := &lateRefusingDescriptor{fakeDescriptor: base}
	mgr, repo, _, projectID := newTestEnv(t, desc)
	mgr.SetResumeDeps(lease.NewManager(repo.db), inputgate.New(), testWorkDirResolver(t))

	ctx := context.Background()
	s, err := mgr.Create(ctx, CreateSpec{
		ProjectID: projectID, Provider: provider.Claude, WorkDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	birthID := *s.NativeSessionID
	crashAndObserve(t, mgr, s)

	revived, err := mgr.recover(ctx, s.ID, "", false)
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	waitForPaneExit(t, mgr, revived)

	// Exactly what the reconciler does when its pass finds the dead pane: it
	// records the exit through the repository, never through Observe.
	code := 1
	applied, err := repo.RecordExitObserved(ctx, revived, &code, nil, time.Now())
	if err != nil {
		t.Fatalf("record exit: %v", err)
	}
	if !applied {
		t.Fatal("the exit was not recorded, so the refusal could not be judged")
	}

	row := requireKeptIdentity(t, repo, s.ID, birthID)
	if row.ResumeAttempts != 1 {
		t.Fatalf("resume_attempts = %d after a reconciler-observed refusal, want 1", row.ResumeAttempts)
	}
	if !containsDetail(auditDetails(t, repo, s.ID, AuditResumeAttempt), refusedResumeAuditDetail) {
		t.Fatal("the reconciler-observed refusal was not recorded in the audit trail")
	}
}

// The card reaches only the rows the user must decide about: a resumed
// generation that failed or vanished with no unattended attempt left, or a
// conversation whose recorded store is gone. A clean exit, a requested stop,
// a fresh generation's death, and a row the sweep is about to pick up stay
// off it.
func TestRecoveryRefusalForUserShapes(t *testing.T) {
	id := "01a00e45-5dce-7000-a1e2-6b3ef003dfe9"
	one, zero := 1, 0
	now := time.Now()
	desc := sleeperDescriptor()
	row := func(mutate func(s *Session)) *Session {
		s := &Session{Status: StatusExited, GenerationResumed: true, NativeSessionID: &id, ExitCode: &one}
		mutate(s)
		return s
	}
	cases := []struct {
		name string
		s    *Session
		want RecoveryRefusal
	}{
		{"resumed generation exited with a failure", row(func(*Session) {}), RecoveryRefusalProvider},
		{"resumed generation killed by a signal", row(func(s *Session) { sig := "9"; s.ExitCode, s.ExitSignal = nil, &sig }), RecoveryRefusalProvider},
		{"resumed generation gone without evidence", row(func(s *Session) { s.Status, s.ExitCode = StatusInterrupted, nil }), RecoveryRefusalProvider},
		{"clean exit is the user leaving", row(func(s *Session) { s.ExitCode = &zero }), ""},
		{"a requested stop is the user's", row(func(s *Session) { s.StopRequestedAt = &now }), ""},
		{"a fresh generation's death is not a refusal", row(func(s *Session) { s.GenerationResumed = false }), ""},
		{"a row the sweep will pick up presents as ended until it does", row(func(s *Session) { s.RecoveryState = RecoveryAutomaticPending }), ""},
		{"a live row has nothing to refuse", row(func(s *Session) { s.Status = StatusRunning }), ""},
		{"no identity, nothing to resume", row(func(s *Session) { s.NativeSessionID = nil }), ""},
	}
	for _, tc := range cases {
		got, refused := recoveryRefusalForOperator(tc.s, desc, true)
		if refused != (tc.want != "") || got != tc.want {
			t.Errorf("%s: refusal = %q (%v), want %q", tc.name, got, refused, tc.want)
		}
	}
}
