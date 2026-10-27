package reconciler

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/noisefloor-oss/deck/internal/db"
	"github.com/noisefloor-oss/deck/internal/inputgate"
	"github.com/noisefloor-oss/deck/internal/lease"
	"github.com/noisefloor-oss/deck/internal/provider"
	"github.com/noisefloor-oss/deck/internal/session"
	"github.com/noisefloor-oss/deck/internal/tmux"
)

// Integration tests against a REAL tmux server on a private per-test socket
// and a real SQLite database. Sessions are launched through the real
// session.Manager; only the provider is faked.

type fakeDescriptor struct {
	unavailable error
	args        []string
	resumeArgs  []string
}

func (f *fakeDescriptor) Name() provider.Name { return provider.Name("claude") }
func (f *fakeDescriptor) Executable() string  { return "/bin/sh" }
func (f *fakeDescriptor) IdentityContract() provider.IdentityContract {
	return provider.IdentityAssigned
}
func (f *fakeDescriptor) Availability() error                   { return f.unavailable }
func (f *fakeDescriptor) Environment() []string                 { return nil }
func (f *fakeDescriptor) PermittedEnvironment() []string        { return []string{"PATH", "HOME"} }
func (f *fakeDescriptor) RequiredConfigPaths() []string         { return nil }
func (f *fakeDescriptor) Injection() provider.InjectionContract { return provider.InjectionContract{} }
func (f *fakeDescriptor) GracefulStopSignal() syscall.Signal    { return syscall.SIGTERM }
func (f *fakeDescriptor) ForceStopSignal() syscall.Signal       { return syscall.SIGKILL }
func (f *fakeDescriptor) NewSessionArgs(provider.LaunchSpec) ([]string, error) {
	if f.args != nil {
		return f.args, nil
	}
	return []string{"/bin/sh", "-c", "sleep 300; exit 0"}, nil
}

func (f *fakeDescriptor) ResumeSessionArgs(provider.ResumeSpec) ([]string, error) {
	if f.resumeArgs != nil {
		return f.resumeArgs, nil
	}
	return nil, errors.New("fake: no resume")
}
func (f *fakeDescriptor) ExecutableIdentity() provider.ExecutableIdentity {
	return provider.ExecutableIdentity{ExpectedProcExe: "/bin/sh"}
}

type env struct {
	db        *db.DB
	mgr       *session.Manager
	repo      *session.Repository
	tmux      *tmux.Client
	rec       *Reconciler
	projectID string
}

func newEnv(t *testing.T) *env { return newEnvWith(t, &fakeDescriptor{}) }

func newEnvWith(t *testing.T, desc provider.Descriptor) *env {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	d, err := db.Open(context.Background(), db.Config{
		Path: filepath.Join(t.TempDir(), "control.db"),
	})
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })

	const projectID = "prj-test"
	err = d.WithTx(context.Background(), func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			INSERT INTO projects (id, name, canonical_path, created_at, last_used_at)
			VALUES (?, 'test', ?, ?, ?)`,
			projectID, t.TempDir(), db.FormatTime(time.Now()), db.FormatTime(time.Now()))
		return err
	})
	if err != nil {
		t.Fatalf("insert project: %v", err)
	}

	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	tc, err := tmux.NewClient(tmux.Config{
		SocketPath: filepath.Join(dir, "ac.sock"),
		Timeout:    10 * time.Second,
	})
	if err != nil {
		t.Fatalf("tmux.NewClient: %v", err)
	}
	t.Cleanup(func() { _ = tc.KillServer(context.Background()) })

	repo := session.NewRepository(d)
	mgr := session.NewManager(repo, tc,
		map[provider.Name]provider.Descriptor{provider.Name("claude"): desc}, "acrec-")
	return &env{
		db:        d,
		mgr:       mgr,
		repo:      repo,
		tmux:      tc,
		rec:       New(repo, tc, 15*time.Second),
		projectID: projectID,
	}
}

func (e *env) launch(t *testing.T) *session.Session {
	t.Helper()
	s, err := e.mgr.Create(context.Background(), session.CreateSpec{
		ProjectID: e.projectID, Provider: "claude", WorkDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if s.Status != session.StatusRunning {
		t.Fatalf("launch status = %s, want running", s.Status)
	}
	return s
}

func TestSocketMismatchCannotClaimCurrentPane(t *testing.T) {
	e := newEnv(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	s := e.launch(t)
	before, err := e.tmux.PaneInfoByID(ctx, *s.TmuxPaneID)
	if err != nil {
		t.Fatalf("observe pane: %v", err)
	}
	if err := e.db.WithTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE sessions SET tmux_socket_path = ? WHERE id = ?`,
			e.tmux.SocketPath()+".foreign", s.ID)
		return err
	}); err != nil {
		t.Fatalf("seed socket mismatch: %v", err)
	}
	report, err := e.rec.RunOnce(ctx)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	got, err := e.repo.Get(ctx, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != session.StatusInterrupted || report.UnknownPanes != 1 {
		t.Fatalf("socket-mismatched row = status %q report %+v", got.Status, report)
	}
	after, err := e.tmux.PaneInfoByID(ctx, before.PaneID)
	if err != nil || after.Dead || after.PID != before.PID ||
		after.StartTicks != before.StartTicks {
		t.Fatalf("socket-mismatched pane was affected: before=%+v after=%+v err=%v",
			before, after, err)
	}
}

func (e *env) waitDead(t *testing.T, paneID string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		info, err := e.tmux.PaneInfoByID(context.Background(), paneID)
		if err != nil {
			t.Fatalf("observe: %v", err)
		}
		if info.Dead {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("pane never died")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// respawnAsRecorded replaces a pane's process out of band while leaving its
// RECORDED identity observable. The spawn guard refuses a respawn
// whose generation or marker equals the pane's current ones, so the harness
// respawns with fresh ones and then restores the recorded pane options
// (generation, marker) via the tmux set-option path. The launch marker,
// stamped in the session environment at Create, already reads as recorded.
// The result is the shape the production paths must classify: the marker,
// generation, and launch marker survive while the pid and start ticks move.
func (e *env) respawnAsRecorded(t *testing.T, s *session.Session, argv []string) {
	t.Helper()
	ctx := context.Background()
	if s.PanePID != nil {
		info, err := e.tmux.PaneInfoByID(ctx, *s.TmuxPaneID)
		if err != nil {
			t.Fatalf("observe: %v", err)
		}
		if !info.Dead {
			if err := syscall.Kill(*s.PanePID, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
				t.Fatalf("out-of-band kill: %v", err)
			}
		}
	}
	e.waitDead(t, *s.TmuxPaneID)
	_, err := e.tmux.RespawnPane(ctx, *s.TmuxPaneID, tmux.SessionSpec{
		Argv:       argv,
		WorkDir:    t.TempDir(),
		Generation: s.Generation + 1,
		Marker:     fmt.Sprintf("fresh-%d", time.Now().UnixNano()),
	})
	if err != nil {
		t.Fatalf("out-of-band respawn: %v", err)
	}
	if err := e.tmux.SetPaneOption(ctx, *s.TmuxPaneID, tmux.OptGeneration, strconv.Itoa(s.Generation)); err != nil {
		t.Fatalf("restore recorded generation: %v", err)
	}
	if err := e.tmux.SetPaneOption(ctx, *s.TmuxPaneID, tmux.OptMarker, *s.IdentityMarker); err != nil {
		t.Fatalf("restore recorded marker: %v", err)
	}
}

func TestHealthyPassChangesNothing(t *testing.T) {
	e := newEnv(t)
	s := e.launch(t)

	rep, err := e.rec.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if rep.Discrepancies != 0 {
		t.Fatalf("discrepancies = %d on a healthy pass: %+v", rep.Discrepancies, rep)
	}
	got, _ := e.repo.Get(context.Background(), s.ID)
	if got.Status != session.StatusRunning {
		t.Fatalf("status changed to %s on a healthy pass", got.Status)
	}
	if e.rec.LastPass().IsZero() {
		t.Fatal("LastPass not recorded")
	}
}

func TestRestartedReconcilerClearsLegacyRecoveryResidueOnce(t *testing.T) {
	e := newEnv(t)
	s := e.launch(t)
	deadline := time.Now().Add(30 * time.Second)
	if err := e.db.WithTx(context.Background(), func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			UPDATE sessions SET resume_attempts = 3, resume_next_at = ?,
				resume_series_started_at = ?, resume_probation_until = ?
			WHERE id = ?`,
			db.FormatTime(time.Now()), db.FormatTime(time.Now().Add(-time.Minute)),
			db.FormatTime(deadline), s.ID)
		return err
	}); err != nil {
		t.Fatalf("seed probation: %v", err)
	}

	restarted := New(e.repo, e.tmux, 15*time.Second)
	restarted.now = func() time.Time { return deadline.Add(-time.Second) }
	rep, err := restarted.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("RunOnce after restart: %v", err)
	}
	if rep.LegacyRecoveryConverged != 1 {
		t.Fatalf("LegacyRecoveryConverged = %d, want 1", rep.LegacyRecoveryConverged)
	}
	got, err := e.repo.Get(context.Background(), s.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.ResumeAttempts != 0 || got.ResumeNextAt != nil ||
		got.ResumeSeriesStartedAt != nil || got.ResumeProbationUntil != nil {
		t.Fatalf("probation did not reset the series: attempts=%d next=%v series=%v probation=%v",
			got.ResumeAttempts, got.ResumeNextAt, got.ResumeSeriesStartedAt, got.ResumeProbationUntil)
	}
	var audits int
	if err := e.db.Reader().QueryRow(`SELECT COUNT(*) FROM audit_events WHERE session_id = ? AND event_type = ?`,
		s.ID, session.AuditRecoveryLegacyConverged).Scan(&audits); err != nil {
		t.Fatalf("count probation audits: %v", err)
	}
	if audits != 1 {
		t.Fatalf("probation audits = %d, want 1", audits)
	}
	rep, err = restarted.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("second RunOnce: %v", err)
	}
	if rep.LegacyRecoveryConverged != 0 {
		t.Fatalf("second LegacyRecoveryConverged = %d, want 0", rep.LegacyRecoveryConverged)
	}
}

func TestLegacyRecoveryResidueConvergenceFencesBootEpoch(t *testing.T) {
	e := newEnv(t)
	s := e.launch(t)
	deadline := time.Now().Add(time.Hour)
	if err := e.db.WithTx(context.Background(), func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			UPDATE sessions SET resume_attempts = 3, resume_next_at = ?,
				resume_series_started_at = ?, resume_probation_until = ?
			WHERE id = ?`,
			db.FormatTime(time.Now()), db.FormatTime(time.Now().Add(-time.Minute)),
			db.FormatTime(deadline), s.ID)
		return err
	}); err != nil {
		t.Fatalf("seed legacy recovery state: %v", err)
	}
	s, err := e.repo.Get(context.Background(), s.ID)
	if err != nil {
		t.Fatalf("get seeded row: %v", err)
	}
	if err := e.db.WithTx(context.Background(), func(ctx context.Context, tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `UPDATE sessions SET boot_epoch = ? WHERE id = ?`,
			"different-boot", s.ID); err != nil {
			return err
		}
		// Hold the independent revision fence equal so this test specifically
		// proves that boot identity is part of the convergence compare-and-swap.
		_, err := tx.ExecContext(ctx, `UPDATE sessions SET controller_revision = ? WHERE id = ?`,
			s.ControllerRevision, s.ID)
		return err
	}); err != nil {
		t.Fatalf("change boot epoch: %v", err)
	}

	applied, err := e.repo.ConvergeLegacyRecoveryResidue(context.Background(), s)
	if err != nil {
		t.Fatalf("converge stale observation: %v", err)
	}
	if applied {
		t.Fatal("legacy recovery state converged across a changed boot epoch")
	}
	got, err := e.repo.Get(context.Background(), s.ID)
	if err != nil {
		t.Fatalf("get changed row: %v", err)
	}
	if got.ResumeAttempts != 3 || got.ResumeProbationUntil == nil {
		t.Fatalf("stale convergence changed retry residue: attempts=%d probation=%v",
			got.ResumeAttempts, got.ResumeProbationUntil)
	}
}

// A user stop requested during probation must survive the probation
// commit: ConvergeLegacyRecoveryResidue refuses while the stamp stands, the CLI
// surviving the stop lets ExpireStopRequest clear it, and only then does a
// later pass commit the series.
func TestUserStopDuringProbationBlocksCommit(t *testing.T) {
	e := newEnv(t)
	s := e.launch(t)
	deadline := time.Now().Add(30 * time.Second)
	stopAt := deadline.Add(-time.Second)
	if err := e.db.WithTx(context.Background(), func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			UPDATE sessions SET resume_attempts = 3, resume_next_at = ?,
				resume_series_started_at = ?, resume_probation_until = ?,
				stop_requested_at = ?
			WHERE id = ?`,
			db.FormatTime(time.Now()), db.FormatTime(time.Now().Add(-time.Minute)),
			db.FormatTime(deadline), db.FormatTime(stopAt), s.ID)
		return err
	}); err != nil {
		t.Fatalf("seed probation with stop: %v", err)
	}

	r := New(e.repo, e.tmux, 15*time.Second)
	r.now = func() time.Time { return deadline.Add(time.Second) }
	rep, err := r.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("RunOnce with pending stop: %v", err)
	}
	if rep.LegacyRecoveryConverged != 0 {
		t.Fatalf("LegacyRecoveryConverged = %d, want 0 — a pending stop must refuse the commit", rep.LegacyRecoveryConverged)
	}
	got, err := e.repo.Get(context.Background(), s.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.ResumeAttempts != 3 || got.ResumeProbationUntil == nil || got.StopRequestedAt == nil {
		t.Fatalf("commit ran over a pending stop: attempts=%d probation=%v stop=%v",
			got.ResumeAttempts, got.ResumeProbationUntil, got.StopRequestedAt)
	}

	// The CLI proves it survived the stop; the stamp expires and the next
	// pass commits the same probation it refused before.
	r.now = func() time.Time { return stopAt.Add(2 * session.StopAttributionWindow) }
	if _, err := r.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce expiring stop: %v", err)
	}
	rep, err = r.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("RunOnce after stop expiry: %v", err)
	}
	if rep.LegacyRecoveryConverged != 1 {
		t.Fatalf("LegacyRecoveryConverged after stop expiry = %d, want 1", rep.LegacyRecoveryConverged)
	}
	got, err = e.repo.Get(context.Background(), s.ID)
	if err != nil {
		t.Fatalf("get after commit: %v", err)
	}
	if got.ResumeAttempts != 0 || got.ResumeProbationUntil != nil || got.StopRequestedAt != nil {
		t.Fatalf("post-commit state: attempts=%d probation=%v stop=%v",
			got.ResumeAttempts, got.ResumeProbationUntil, got.StopRequestedAt)
	}
}

// A dead pane's preserved evidence is recorded as exited.
func TestDeadPaneExitRecorded(t *testing.T) {
	e := newEnv(t)
	s := e.launch(t)

	if err := syscall.Kill(*s.PanePID, syscall.SIGKILL); err != nil {
		t.Fatalf("kill: %v", err)
	}
	e.waitDead(t, *s.TmuxPaneID)

	rep, err := e.rec.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if rep.ExitsRecorded != 1 {
		t.Fatalf("ExitsRecorded = %d, want 1: %+v", rep.ExitsRecorded, rep)
	}
	got, _ := e.repo.Get(context.Background(), s.ID)
	if got.Status != session.StatusExited || got.ExitSignal == nil || *got.ExitSignal != "9" {
		t.Fatalf("status=%s signal=%v, want exited signal 9", got.Status, got.ExitSignal)
	}
}

func TestDeadPaneWithoutExitEvidenceIsInterrupted(t *testing.T) {
	e := newEnv(t)
	s := e.launch(t)

	rep := &Report{}
	if err := e.rec.recordDeadPane(context.Background(), s, &tmux.PaneInfo{Dead: true}, rep); err != nil {
		t.Fatalf("recordDeadPane: %v", err)
	}
	if rep.Interrupted != 1 {
		t.Fatalf("report = %+v, want one interrupted", rep)
	}
	got, err := e.repo.Get(context.Background(), s.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != session.StatusInterrupted {
		t.Fatalf("status = %s, want interrupted", got.Status)
	}
	if got.ExitCode != nil || got.ExitSignal != nil {
		t.Fatalf("interrupted session fabricated exit evidence: code=%v signal=%v",
			got.ExitCode, got.ExitSignal)
	}
}

// A pass can retain generation N while another observation records its death
// and recovery publishes generation N+1. None of the old pass's conclusions
// may be applied to the replacement, even though both snapshots say running.
func TestStalePassCannotSettleRecoveredGeneration(t *testing.T) {
	e := newEnvWith(t, &fakeDescriptor{
		resumeArgs: []string{"/bin/sh", "-c", "sleep 300; exit 0"},
	})
	workDir := t.TempDir()
	e.mgr.SetResumeDeps(lease.NewManager(e.db), inputgate.New(),
		func(context.Context, string) (string, error) { return workDir, nil })
	stale := e.launch(t)
	ctx := context.Background()

	if err := syscall.Kill(-*stale.ProcessGroupID, syscall.SIGKILL); err != nil {
		t.Fatalf("crash: %v", err)
	}
	e.waitDead(t, *stale.TmuxPaneID)
	dead, err := e.tmux.PaneInfoByID(ctx, *stale.TmuxPaneID)
	if err != nil {
		t.Fatalf("observe dead pane: %v", err)
	}
	if err := e.rec.recordDeadPane(ctx, stale, dead, &Report{}); err != nil {
		t.Fatalf("record death: %v", err)
	}
	if err := e.mgr.AutoResumeSweep(ctx); err != nil {
		t.Fatalf("recover: %v", err)
	}

	recovered, err := e.repo.Get(ctx, stale.ID)
	if err != nil {
		t.Fatalf("get recovered: %v", err)
	}
	if recovered.Status != session.StatusRunning || recovered.Generation <= stale.Generation {
		t.Fatalf("recovery = %s gen %d, want running generation past %d",
			recovered.Status, recovered.Generation, stale.Generation)
	}

	oldExit := 9
	rep := &Report{}
	if err := e.rec.recordDeadPane(ctx, stale, &tmux.PaneInfo{
		Dead: true, ExitStatus: &oldExit,
	}, rep); err != nil {
		t.Fatalf("record stale dead pane: %v", err)
	}
	if err := e.rec.reconcileAbsent(ctx, []*session.Session{stale}, nil, rep); err != nil {
		t.Fatalf("reconcile stale absence: %v", err)
	}
	if rep.ExitsRecorded != 0 || rep.Interrupted != 0 {
		t.Fatalf("stale pass reported a transition: %+v", rep)
	}

	got, err := e.repo.Get(ctx, stale.ID)
	if err != nil {
		t.Fatalf("get after stale pass: %v", err)
	}
	if got.Status != session.StatusRunning || got.Generation != recovered.Generation {
		t.Fatalf("stale pass overwrote recovery: got %s gen %d, want running gen %d",
			got.Status, got.Generation, recovered.Generation)
	}

	t.Cleanup(func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = e.mgr.Stop(stopCtx, stale.ID, true)
	})
}

// Pane gone entirely — running becomes interrupted, never a fabricated
// exit.
func TestGonePaneInterrupted(t *testing.T) {
	e := newEnv(t)
	s := e.launch(t)

	if err := e.tmux.KillSession(context.Background(), s.TmuxSessionName); err != nil {
		t.Fatalf("out-of-band kill-session: %v", err)
	}

	rep, err := e.rec.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if rep.Interrupted != 1 {
		t.Fatalf("Interrupted = %d, want 1: %+v", rep.Interrupted, rep)
	}
	got, _ := e.repo.Get(context.Background(), s.ID)
	if got.Status != session.StatusInterrupted {
		t.Fatalf("status = %s, want interrupted", got.Status)
	}
	if got.ExitCode != nil || got.ExitSignal != nil {
		t.Fatal("interrupted must carry no fabricated exit evidence")
	}
}

// An out-of-band respawn keeps the marker but moves pid and start
// ticks; the active session records the lost attachment as interrupted and
// the imposter is not touched.
func TestRespawnedImposterInterrupted(t *testing.T) {
	e := newEnv(t)
	s := e.launch(t)
	ctx := context.Background()

	e.respawnAsRecorded(t, s, []string{"/bin/sh", "-c", "sleep 300; exit 0"})

	rep, err := e.rec.RunOnce(ctx)
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if rep.Interrupted != 1 {
		t.Fatalf("Interrupted = %d, want 1: %+v", rep.Interrupted, rep)
	}
	got, _ := e.repo.Get(ctx, s.ID)
	if got.Status != session.StatusInterrupted {
		t.Fatalf("status = %s, want interrupted", got.Status)
	}
	info, err := e.tmux.PaneInfoByID(ctx, *s.TmuxPaneID)
	if err != nil || info.Dead {
		t.Fatalf("imposter was touched (info=%+v err=%v)", info, err)
	}
}

// A pane in the namespace no session claims is audited, counted, and left
// strictly alone.
func TestUnknownPaneAuditedNotKilled(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()

	info, err := e.tmux.CreateSession(ctx, tmux.SessionSpec{
		Name:       "acrec-intruder",
		WorkDir:    t.TempDir(),
		Argv:       []string{"/bin/sh", "-c", "sleep 300; exit 0"},
		Generation: 7,
		Marker:     "not-recorded-anywhere",
	})
	if err != nil {
		t.Fatalf("intruder create: %v", err)
	}

	rep, err := e.rec.RunOnce(ctx)
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if rep.UnknownPanes != 1 {
		t.Fatalf("UnknownPanes = %d, want 1: %+v", rep.UnknownPanes, rep)
	}
	after, err := e.tmux.PaneInfoByID(ctx, info.PaneID)
	if err != nil || after.Dead {
		t.Fatalf("unknown pane was touched (info=%+v err=%v)", after, err)
	}
	var n int
	if err := e.db.Reader().QueryRow(
		`SELECT COUNT(*) FROM audit_events WHERE event_type = ? AND session_id IS NULL`,
		session.AuditUnknownPane).Scan(&n); err != nil {
		t.Fatalf("count orphan audits: %v", err)
	}
	if n != 1 {
		t.Fatalf("orphan audit events = %d, want 1", n)
	}
}

// Server gone AND the recorded process gone: the row resolves to
// interrupted — no pane, no process, no fabricated evidence.
func TestServerDownGoneProcessInterrupted(t *testing.T) {
	e := newEnv(t)
	s := e.launch(t)
	ctx := context.Background()

	if err := e.tmux.KillServer(ctx); err != nil {
		t.Fatalf("kill server: %v", err)
	}
	// kill-server HUPs the pane process; the default-disposition shell dies.
	deadline := time.Now().Add(30 * time.Second)
	for syscall.Kill(*s.PanePID, 0) == nil {
		if time.Now().After(deadline) {
			t.Fatal("pane process survived kill-server; test expects the default HUP disposition")
		}
		time.Sleep(50 * time.Millisecond)
	}

	rep, err := e.rec.RunOnce(ctx)
	if err != nil {
		t.Fatalf("RunOnce with no server: %v", err)
	}
	if !rep.ServerDown || rep.Interrupted != 1 {
		t.Fatalf("report = %+v, want ServerDown with 1 interrupted", rep)
	}
	got, _ := e.repo.Get(ctx, s.ID)
	if got.Status != session.StatusInterrupted {
		t.Fatalf("status = %s, want interrupted", got.Status)
	}
}

// Server gone but the RECORDED process survives (HUP-immune): a live
// process without an attachable pane — parked, never signalled.
func TestServerDownLiveProcessInterruptedUnsignalled(t *testing.T) {
	e := newEnvWith(t, &fakeDescriptor{
		args: []string{"/bin/sh", "-c", "trap '' HUP; while :; do sleep 1; done"},
	})
	s := e.launch(t)
	ctx := context.Background()

	if err := e.tmux.KillServer(ctx); err != nil {
		t.Fatalf("kill server: %v", err)
	}
	time.Sleep(200 * time.Millisecond) // let the HUP land and be ignored
	if syscall.Kill(*s.PanePID, 0) != nil {
		t.Fatal("HUP-immune process died; cannot exercise the orphan path")
	}
	t.Cleanup(func() { _ = syscall.Kill(*s.PanePID, syscall.SIGKILL) })

	rep, err := e.rec.RunOnce(ctx)
	if err != nil {
		t.Fatalf("RunOnce with no server: %v", err)
	}
	if rep.OrphanProcesses != 1 {
		t.Fatalf("report = %+v, want 1 orphan process", rep)
	}
	got, _ := e.repo.Get(ctx, s.ID)
	if got.Status != session.StatusInterrupted {
		t.Fatalf("status = %s, want interrupted", got.Status)
	}
	if syscall.Kill(*s.PanePID, 0) != nil {
		t.Fatal("orphan process was signalled")
	}
}

// A running session whose recorded boot epoch predates the current boot
// (the recorded process proven gone across a restart) is recovered, not
// parked with a dead-end error.
func TestReconcileStaleBootEpochRecoversNotParks(t *testing.T) {
	if _, err := tmux.BootID(); err != nil {
		t.Skipf("host boot epoch unavailable on this platform: %v", err)
	}
	e := newEnv(t)
	s := e.launch(t)
	ctx := context.Background()

	// Force a stale boot epoch so the recorded process predates this boot.
	const stale = "00000000-0000-0000-0000-000000000000"
	if err := e.db.WithTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE sessions SET boot_epoch = ? WHERE id = ?`, stale, s.ID)
		return err
	}); err != nil {
		t.Fatalf("set stale boot epoch: %v", err)
	}
	if err := e.tmux.KillSession(ctx, s.TmuxSessionName); err != nil {
		t.Fatalf("kill tmux session: %v", err)
	}

	rep, err := e.rec.RunOnce(ctx)
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	got, _ := e.repo.Get(ctx, s.ID)
	if got.Status != session.StatusInterrupted {
		t.Fatalf("status = %s, want interrupted (recover, not park)", got.Status)
	}
	if rep.Interrupted != 1 {
		t.Fatalf("Interrupted = %d, want 1 (stale epoch recovers)", rep.Interrupted)
	}
}

// A starting row that never committed identity becomes interrupted after one
// interval, consuming its (non-existent) auto opportunity; a fresh one is
// left to finish launching.
func TestStaleStartingInterrupted(t *testing.T) {
	e := newEnv(t)
	e.rec = New(e.repo, e.tmux, 500*time.Millisecond)
	e.rec.startingGrace = 500 * time.Millisecond
	ctx := context.Background()

	stale := &session.Session{
		ID: "stale-start", ProjectID: e.projectID, Provider: "claude",
		TmuxSocketPath: e.tmux.SocketPath(), TmuxSessionName: "acrec-stale", Generation: 1,
	}
	if err := e.repo.Create(ctx, stale); err != nil {
		t.Fatalf("create stale: %v", err)
	}
	time.Sleep(600 * time.Millisecond)

	fresh := &session.Session{
		ID: "fresh-start", ProjectID: e.projectID, Provider: "claude",
		TmuxSocketPath: e.tmux.SocketPath(), TmuxSessionName: "acrec-fresh", Generation: 1,
	}
	if err := e.repo.Create(ctx, fresh); err != nil {
		t.Fatalf("create fresh: %v", err)
	}

	rep, err := e.rec.RunOnce(ctx)
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if rep.StaleStarting != 1 {
		t.Fatalf("StaleStarting = %d, want 1: %+v", rep.StaleStarting, rep)
	}
	gotStale, _ := e.repo.Get(ctx, "stale-start")
	if gotStale.Status != session.StatusInterrupted {
		t.Fatalf("stale status = %s, want interrupted", gotStale.Status)
	}
	if gotStale.ResumeAttempts != 1 {
		t.Fatalf("stale attempts = %d, want 1 (no auto opportunity for unverified generation)",
			gotStale.ResumeAttempts)
	}
	gotFresh, _ := e.repo.Get(ctx, "fresh-start")
	if gotFresh.Status != session.StatusStarting {
		t.Fatalf("fresh status = %s, want starting untouched", gotFresh.Status)
	}
}

func TestStartingPaneCannotPublishAfterReconciliationRetiresLaunch(t *testing.T) {
	e := newEnv(t)
	e.rec = New(e.repo, e.tmux, 100*time.Millisecond)
	e.rec.startingGrace = 100 * time.Millisecond
	ctx := context.Background()
	marker := "starting-marker-before-running"
	s := &session.Session{
		ID: "starting-pane", ProjectID: e.projectID, Provider: "claude",
		TmuxSocketPath: e.tmux.SocketPath(), TmuxSessionName: "acrec-starting-pane",
		Generation: 1, IdentityMarker: &marker,
	}
	if err := e.repo.Create(ctx, s); err != nil {
		t.Fatalf("create starting row: %v", err)
	}
	info, err := e.tmux.CreateSession(ctx, tmux.SessionSpec{
		Name: s.TmuxSessionName, WorkDir: t.TempDir(),
		Argv:       []string{"/bin/sh", "-c", "sleep 300; exit 0"},
		Generation: s.Generation, Marker: marker,
	})
	if err != nil || info.Dead {
		t.Fatalf("spawn starting pane: info=%+v err=%v", info, err)
	}
	t.Cleanup(func() { _ = e.tmux.KillSession(context.Background(), s.TmuxSessionName) })
	time.Sleep(150 * time.Millisecond)

	rep, err := e.rec.RunOnce(ctx)
	if err != nil {
		t.Fatalf("reconcile starting pane: %v", err)
	}
	if rep.StaleStarting != 1 || rep.Interrupted != 1 {
		t.Fatalf("starting-pane report = %+v, want one bounded interruption", rep)
	}
	retired, err := e.repo.Get(ctx, s.ID)
	if err != nil || retired.Status != session.StatusInterrupted || retired.ResumeAttempts != 1 {
		t.Fatalf("retired launch = %+v err=%v, want interrupted attempts=1", retired, err)
	}

	bootEpoch, err := tmux.BootID()
	if err != nil {
		t.Fatalf("boot epoch: %v", err)
	}
	expected := "/bin/sh"
	s.TmuxSessionID = &info.SessionID
	s.TmuxPaneID = &info.PaneID
	s.PanePID = &info.PID
	s.ProcessGroupID = &info.ProcessGroupID
	s.StartTicks = &info.StartTicks
	s.ExpectedExecutable = &expected
	s.BootEpoch = &bootEpoch
	if err := e.repo.MarkRunning(ctx, s); err == nil {
		t.Fatal("late launch publication overwrote the reconciled starting row")
	}
	stillRetired, err := e.repo.Get(ctx, s.ID)
	if err != nil || stillRetired.Status != session.StatusInterrupted ||
		stillRetired.Generation != 1 || stillRetired.ResumeAttempts != 1 {
		t.Fatalf("late publication changed row = %+v err=%v", stillRetired, err)
	}
	observed, err := e.tmux.PaneInfoByID(ctx, info.PaneID)
	if err != nil || observed.Dead {
		t.Fatalf("reconciliation touched uncommitted runtime: dead=%v err=%v", observed.Dead, err)
	}
}

// A terminal row whose pane is alive again (out-of-band respawn of the
// corpse) must not be silently resurrected; whatever lives there now is
// recorded as an unknown pane and the row keeps its history.
func TestTerminalRowWithLivePaneNotResurrected(t *testing.T) {
	e := newEnv(t)
	s := e.launch(t)
	ctx := context.Background()

	stopCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := e.mgr.Stop(stopCtx, s.ID, true); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	e.respawnAsRecorded(t, s, []string{"/bin/sh", "-c", "sleep 300; exit 0"})

	rep, err := e.rec.RunOnce(ctx)
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if rep.UnknownPanes != 1 {
		t.Fatalf("UnknownPanes = %d, want 1 (the resurrected corpse): %+v", rep.UnknownPanes, rep)
	}
	got, _ := e.repo.Get(ctx, s.ID)
	if got.Status != session.StatusExited {
		t.Fatalf("status = %s; terminal history must not be rewritten", got.Status)
	}
}

// A zombie still has a /proc entry with matching start ticks but is not
// alive; treating it as an orphan process would park the session forever
// over a corpse awaiting reaping.
func TestProcAliveRejectsZombie(t *testing.T) {
	cmd := exec.Command("/bin/sh", "-c", "exit 0")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start child: %v", err)
	}
	pid := cmd.Process.Pid

	// Capture the live start ticks before the child exits... or after: a
	// zombie's stat keeps its start ticks, so reading them from the zombie
	// itself both drives the probe and proves the point.
	deadline := time.Now().Add(30 * time.Second)
	var ticks int64
	for {
		raw, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
		if err != nil {
			t.Fatalf("read child stat: %v", err)
		}
		s := string(raw)
		fields := strings.Fields(s[strings.LastIndexByte(s, ')')+1:])
		if len(fields) >= 20 && fields[0] == "Z" {
			var perr error
			ticks, perr = strconv.ParseInt(fields[19], 10, 64)
			if perr != nil {
				t.Fatalf("parse ticks: %v", perr)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("child never became a zombie")
		}
		time.Sleep(10 * time.Millisecond)
	}

	if alive, _ := tmux.ProcAlive(pid, ticks); alive {
		t.Fatal("procAlive reported a zombie as alive")
	}
	_ = cmd.Wait() // reap
}

// A parked imposter and a lingering unknown pane must not churn new audit
// events on every subsequent pass.
func TestRepeatedPassesDoNotChurnAudits(t *testing.T) {
	e := newEnv(t)
	s := e.launch(t)
	ctx := context.Background()

	// One imposter (respawned corpse under an active row) ...
	e.respawnAsRecorded(t, s, []string{"/bin/sh", "-c", "sleep 300; exit 0"})
	// ... and one unknown pane.
	if _, err := e.tmux.CreateSession(ctx, tmux.SessionSpec{
		Name: "acrec-intruder", WorkDir: t.TempDir(),
		Argv:       []string{"/bin/sh", "-c", "sleep 300; exit 0"},
		Generation: 3, Marker: "never-recorded",
	}); err != nil {
		t.Fatalf("intruder: %v", err)
	}

	countAudits := func() int {
		t.Helper()
		var n int
		if err := e.db.Reader().QueryRow(`SELECT COUNT(*) FROM audit_events`).Scan(&n); err != nil {
			t.Fatalf("count audits: %v", err)
		}
		return n
	}

	if _, err := e.rec.RunOnce(ctx); err != nil {
		t.Fatalf("first pass: %v", err)
	}
	after1 := countAudits()
	for i := 0; i < 3; i++ {
		if _, err := e.rec.RunOnce(ctx); err != nil {
			t.Fatalf("pass %d: %v", i+2, err)
		}
	}
	if after4 := countAudits(); after4 != after1 {
		t.Fatalf("audit events grew %d -> %d across settled passes — churn", after1, after4)
	}
}

// A failed audit write must not mark the condition as recorded: the next
// pass with a working context must still write it.
func TestFailedOrphanAuditIsRetried(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()

	canceled, cancel := context.WithCancel(ctx)
	cancel()
	keys := map[string]bool{}
	if err := e.rec.auditOrphanOnce(canceled, keys, "k1", "first attempt"); err == nil {
		t.Fatal("write on a canceled context should fail")
	}
	if err := e.rec.auditOrphanOnce(ctx, keys, "k1", "retry"); err != nil {
		t.Fatalf("retry: %v", err)
	}
	var n int
	if err := e.db.Reader().QueryRow(
		`SELECT COUNT(*) FROM audit_events WHERE event_type = ?`,
		session.AuditUnknownPane).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Fatalf("audit rows = %d, want 1 (the successful retry)", n)
	}
}

// A condition that disappears and later recurs is a NEW occurrence and is
// audited again; the dedup map shrinks with the pane population.
func TestDepartedConditionReauditsOnRecurrence(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()

	spec := tmux.SessionSpec{
		Name: "acrec-intruder", WorkDir: t.TempDir(),
		Argv:       []string{"/bin/sh", "-c", "sleep 300; exit 0"},
		Generation: 3, Marker: "never-recorded",
	}
	if _, err := e.tmux.CreateSession(ctx, spec); err != nil {
		t.Fatalf("intruder: %v", err)
	}
	if _, err := e.rec.RunOnce(ctx); err != nil {
		t.Fatalf("pass 1: %v", err)
	}
	if err := e.tmux.KillSession(ctx, spec.Name); err != nil {
		t.Fatalf("remove intruder: %v", err)
	}
	if _, err := e.rec.RunOnce(ctx); err != nil {
		t.Fatalf("pass 2 (prunes): %v", err)
	}
	if _, err := e.tmux.CreateSession(ctx, spec); err != nil {
		t.Fatalf("intruder returns: %v", err)
	}
	if _, err := e.rec.RunOnce(ctx); err != nil {
		t.Fatalf("pass 3: %v", err)
	}

	var n int
	if err := e.db.Reader().QueryRow(
		`SELECT COUNT(*) FROM audit_events WHERE event_type = ?`,
		session.AuditUnknownPane).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 2 {
		t.Fatalf("audit rows = %d, want 2 (original + recurrence)", n)
	}
}

func TestRunLoopHonorsContext(t *testing.T) {
	e := newEnv(t)
	e.rec = New(e.repo, e.tmux, 30*time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { e.rec.Run(ctx); close(done) }()
	t.Cleanup(cancel)
	deadline := time.Now().Add(30 * time.Second)
	for e.rec.LastPass().IsZero() {
		if time.Now().After(deadline) {
			t.Fatal("Run never completed a pass")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("Run did not stop on context cancellation")
	}
}

// The reconciler path: a stop request the CLI survived is cleared
// by the periodic pass itself — the healthy observation past the
// attribution window is the proof — with the stop_survived audit event,
// so a later crash gets its bounded automatic resume.
func TestReconcilerClearsSurvivedStopRequest(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	s := e.launch(t)
	t.Cleanup(func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = e.mgr.Stop(stopCtx, s.ID, true)
	})

	// A stop request older than the attribution window, on a session the
	// pass will observe alive.
	stale := db.FormatTime(time.Now().Add(-session.StopAttributionWindow - time.Minute))
	if err := e.db.WithTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`UPDATE sessions SET stop_requested_at = ? WHERE id = ?`, stale, s.ID)
		return err
	}); err != nil {
		t.Fatalf("stamp stale stop request: %v", err)
	}

	if _, err := e.rec.RunOnce(ctx); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	got, err := e.repo.Get(ctx, s.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status != session.StatusRunning {
		t.Fatalf("status = %s, want running", got.Status)
	}
	if got.StopRequestedAt != nil {
		t.Fatalf("survived stop request not cleared by the pass: %v", got.StopRequestedAt)
	}
	var events int
	if err := e.db.Reader().QueryRowContext(ctx,
		`SELECT count(*) FROM audit_events WHERE session_id = ? AND event_type = 'session.stop_survived'`,
		s.ID).Scan(&events); err != nil {
		t.Fatalf("count audit: %v", err)
	}
	if events != 1 {
		t.Fatalf("stop_survived audit events = %d, want 1", events)
	}

	// A FRESH stop request must not be cleared by the next pass: within
	// the window, an alive observation proves nothing about survival yet.
	if err := e.db.WithTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`UPDATE sessions SET stop_requested_at = ? WHERE id = ?`,
			db.FormatTime(time.Now()), s.ID)
		return err
	}); err != nil {
		t.Fatalf("stamp fresh stop request: %v", err)
	}
	if _, err := e.rec.RunOnce(ctx); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	fresh, err := e.repo.Get(ctx, s.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if fresh.StopRequestedAt == nil {
		t.Fatal("a fresh stop request was cleared inside the attribution window")
	}
}
