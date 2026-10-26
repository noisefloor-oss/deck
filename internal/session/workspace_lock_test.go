package session

import (
	"context"
	"database/sql"
	"errors"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/noisefloor-oss/deck/internal/inputgate"
	"github.com/noisefloor-oss/deck/internal/lease"
	"github.com/noisefloor-oss/deck/internal/provider"
	"github.com/noisefloor-oss/deck/internal/tmux"
)

func TestWorkspaceLockRefusesDeadlineThatOmitsRuntimeObservation(t *testing.T) {
	mgr, repo, tc, project := newTestEnv(t, sleeperDescriptor())
	ctx := context.Background()
	s, err := mgr.Create(ctx, CreateSpec{ProjectID: project, Provider: "claude", WorkDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	mgr.lifecycleHooks = &lifecycleTestHooks{stopSignaled: func() { t.Fatal("refused lock signaled a session") }}
	oldBudget := workspaceLockGracefulStop + workspaceLockForceStop +
		workspaceLockStopsPerSession*workspaceLockWritesPerStop*stopDetachedWriteTimeout
	shortCtx, cancel := context.WithTimeout(ctx, oldBudget+time.Second)
	defer cancel()
	if err := mgr.PrepareWorkspaceLock(shortCtx); !errors.Is(err, ErrWorkspaceLockRefused) {
		t.Fatalf("deadline between the old and new budgets was not refused: %v", err)
	}
	release, err := mgr.gate.Admit()
	if err != nil {
		t.Fatalf("clean refusal disabled admission: %v", err)
	}
	release()
	got, err := repo.Get(ctx, s.ID)
	if err != nil || got.StopRequestedAt != nil || workspaceStopped(got) {
		t.Fatalf("refused lock committed a stop marker: %+v %v", got, err)
	}
	if info, err := tc.PaneInfoByID(ctx, *s.TmuxPaneID); err != nil || info.Dead {
		t.Fatalf("refused lock stopped a process: %+v %v", info, err)
	}
}

func TestWorkspaceLockWedgedTmuxObservationIsBoundedWithoutFabricatingDeath(t *testing.T) {
	mgr, repo, tc, project := newTestEnv(t, sleeperDescriptor())
	ctx, cancel := context.WithTimeout(context.Background(), WorkspaceLockTimeout)
	defer cancel()
	s, err := mgr.Create(ctx, CreateSpec{ProjectID: project, Provider: "claude", WorkDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	out, err := exec.CommandContext(ctx, "tmux", "-S", tc.SocketPath(),
		"display-message", "-p", "#{pid}").Output()
	if err != nil {
		t.Fatalf("read private tmux server pid: %v", err)
	}
	serverPID, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		t.Fatalf("parse private tmux server pid: %v", err)
	}
	t.Cleanup(func() { _ = syscall.Kill(serverPID, syscall.SIGCONT) })
	mgr.lifecycleHooks = &lifecycleTestHooks{workspaceLockMarked: func() {
		applied, err := repo.MarkInterrupted(ctx, s.ID, "test fixture loses the controller while its process stays live")
		if err != nil || !applied {
			t.Fatalf("mark terminal session mid-lock: applied=%v err=%v", applied, err)
		}
		if err := syscall.Kill(serverPID, syscall.SIGSTOP); err != nil {
			t.Fatalf("wedge private tmux server: %v", err)
		}
	}}
	started := time.Now()
	lockErr := mgr.PrepareWorkspaceLock(ctx)
	elapsed := time.Since(started)
	if err := syscall.Kill(serverPID, syscall.SIGCONT); err != nil {
		t.Fatalf("resume private tmux server: %v", err)
	}
	got, err := repo.Get(ctx, s.ID)
	if err != nil || got.Status != StatusInterrupted || got.ExitCode != nil || !workspaceStopped(got) {
		t.Fatalf("wedged observation fabricated exit evidence: %+v %v", got, err)
	}
	info, err := tc.PaneInfoByID(ctx, *s.TmuxPaneID)
	if err != nil || info.Dead || info.PID != *s.PanePID {
		t.Fatalf("wedged observation killed or misidentified the live process: %+v %v", info, err)
	}
	if !errors.Is(lockErr, ErrNotRunning) {
		t.Fatalf("wedged observation replaced the original stop error: %v", lockErr)
	}
	if elapsed < workspaceLockObserveTimeout-time.Second || elapsed > 4*workspaceLockObserveTimeout {
		t.Fatalf("wedged observation took %s, want approximately %s", elapsed, workspaceLockObserveTimeout)
	}
}

func TestWorkspaceLockStopsAllAndUnlockDoesNotAutoResume(t *testing.T) {
	mgr, repo, project, tc, gate := newResumeEnv(t, []string{"-c", "sleep 300; exit 0"})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	var sessions []*Session
	for range 2 {
		s, err := mgr.Create(ctx, CreateSpec{ProjectID: project, Provider: "claude", WorkDir: t.TempDir()})
		if err != nil {
			t.Fatal(err)
		}
		sessions = append(sessions, s)
	}
	// Even a previously lost session must not restart during the next boot.
	lost := createInterrupted(t, mgr, project)
	sessions = append(sessions, lost)
	if err := mgr.PrepareWorkspaceLock(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := gate.Admit(); !errors.Is(err, inputgate.ErrWorkspaceLocking) {
		t.Fatalf("writer admitted: %v", err)
	}
	if _, err := mgr.Create(ctx, CreateSpec{}); !errors.Is(err, inputgate.ErrWorkspaceLocking) {
		t.Fatalf("launch admitted: %v", err)
	}
	for _, s := range sessions {
		got, err := repo.Get(ctx, s.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.StopRequestedAt == nil || !workspaceStopped(got) {
			t.Fatalf("generation may auto-resume: %+v", got)
		}
		info, err := tc.PaneInfoByID(ctx, *s.TmuxPaneID)
		if err != nil || !info.Dead {
			t.Fatalf("process still live: %+v %v", info, err)
		}
	}
	// A new backend after remount gets a fresh admission gate, but it must
	// preserve the intentional stopped generations even beyond stop expiry.
	fresh := NewManager(repo, tc, mgr.providers, "actest-")
	fresh.SetResumeDeps(lease.NewManager(repo.db), inputgate.New(), testWorkDirResolver(t))
	fresh.now = func() time.Time { return time.Now().Add(24 * time.Hour) }
	if err := fresh.AutoResumeSweep(ctx); err != nil {
		t.Fatal(err)
	}
	candidates, err := repo.ListResumeCandidates(ctx)
	if err != nil || len(candidates) != 0 {
		t.Fatalf("post-unlock candidates=%d err=%v", len(candidates), err)
	}
	for _, s := range sessions {
		got, err := repo.Get(ctx, s.ID)
		if err != nil || got.Generation != s.Generation {
			t.Fatalf("lock restarted generation: %+v %v", got, err)
		}
	}
	revived, _, err := fresh.Recover(ctx, sessions[0].ID)
	if err != nil || revived.Generation != sessions[0].Generation+1 || workspaceStopped(revived) {
		t.Fatalf("explicit resume after unlock failed: %+v %v", revived, err)
	}
}

func TestWorkspaceLockInterruptedFirstStopMarksEntireCohort(t *testing.T) {
	desc := sleeperDescriptor()
	desc.graceful = 0
	mgr, repo, tc, project := newTestEnv(t, desc)
	ctx := context.Background()
	var sessions []*Session
	for range 2 {
		s, err := mgr.Create(ctx, CreateSpec{ProjectID: project, Provider: "claude", WorkDir: t.TempDir()})
		if err != nil {
			t.Fatal(err)
		}
		sessions = append(sessions, s)
	}
	stopCtx, cancel := context.WithCancel(ctx)
	var once sync.Once
	mgr.lifecycleHooks = &lifecycleTestHooks{stopSignaled: func() {
		once.Do(func() {
			for _, member := range sessions {
				got, err := repo.Get(ctx, member.ID)
				if err != nil || !workspaceStopped(got) {
					t.Error("first signal preceded complete cohort intent")
				}
			}
			cancel()
		})
	}}
	if err := mgr.PrepareWorkspaceLock(stopCtx); err == nil {
		t.Fatal("interrupted stop reported completion")
	}
	for _, s := range sessions {
		got, err := repo.Get(ctx, s.ID)
		if err != nil {
			t.Fatal(err)
		}
		if !workspaceStopped(got) {
			t.Fatal("unsignaled cohort member was not durably excluded from recovery")
		}
		info, err := tc.PaneInfoByID(ctx, *s.TmuxPaneID)
		if err != nil || info.Dead {
			t.Fatalf("synthetic process should survive the cancelled signal-zero stop: %v", err)
		}
	}
	fresh := NewManager(repo, tc, mgr.providers, "actest-")
	if err := fresh.ConvergeStopAttempts(ctx); err != nil {
		t.Fatal(err)
	}
	for _, s := range sessions {
		if _, err := repo.ExpireStopRequest(ctx, s.ID, time.Now().Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
		got, err := repo.Get(ctx, s.ID)
		if err != nil || !workspaceStopped(got) {
			t.Fatal("ordinary stop expiry rearmed the workspace cohort")
		}
		if _, err := repo.ConvergeLegacyRecoveryResidue(ctx, got); err != nil {
			t.Fatal(err)
		}
		got, err = repo.Get(ctx, s.ID)
		if err != nil || !workspaceStopped(got) {
			t.Fatal("legacy convergence erased workspace stop intent")
		}
		if automaticRecoveryOpportunity(got, desc, true, time.Now()) {
			t.Fatal("legacy convergence rearmed stopped generation")
		}
	}
}

func TestWorkspaceLockStopsThreeSessionsWithinHostBudget(t *testing.T) {
	mgr, repo, tc, project := newTestEnv(t, sleeperDescriptor())
	ctx, cancel := context.WithTimeout(context.Background(), WorkspaceLockTimeout)
	defer cancel()
	var sessions []*Session
	for range 3 {
		s, err := mgr.Create(ctx, CreateSpec{ProjectID: project, Provider: "claude", WorkDir: t.TempDir()})
		if err != nil {
			t.Fatal(err)
		}
		sessions = append(sessions, s)
	}
	if err := mgr.PrepareWorkspaceLock(ctx); err != nil {
		t.Fatalf("ordinary three-session lock refused: %v", err)
	}
	for _, s := range sessions {
		got, err := repo.Get(ctx, s.ID)
		if err != nil || !workspaceStopped(got) {
			t.Fatal("cohort stop was not recorded")
		}
		info, err := tc.PaneInfoByID(ctx, *s.TmuxPaneID)
		if err != nil || !info.Dead {
			t.Fatalf("process remained live: %+v %v", info, err)
		}
	}
}

func TestWorkspaceLockRefusesCohortLargerThanDeadlineBeforeCommitting(t *testing.T) {
	mgr, repo, tc, project := newTestEnv(t, sleeperDescriptor())
	ctx := context.Background()
	var sessions []*Session
	for range 2 {
		s, err := mgr.Create(ctx, CreateSpec{ProjectID: project, Provider: "claude", WorkDir: t.TempDir()})
		if err != nil {
			t.Fatal(err)
		}
		sessions = append(sessions, s)
	}
	mgr.lifecycleHooks = &lifecycleTestHooks{stopSignaled: func() { t.Fatal("refused lock signaled a session") }}
	shortCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	err := mgr.PrepareWorkspaceLock(shortCtx)
	if !errors.Is(err, ErrWorkspaceLockRefused) || !strings.Contains(err.Error(), "2 sessions") {
		t.Fatalf("undersized deadline was not refused with the cohort size: %v", err)
	}
	release, admissionErr := mgr.gate.Admit()
	if admissionErr != nil {
		t.Fatalf("clean refusal disabled input: %v", admissionErr)
	}
	release()
	for _, s := range sessions {
		got, err := repo.Get(ctx, s.ID)
		if err != nil || got.StopRequestedAt != nil || workspaceStopped(got) {
			t.Fatalf("refused lock committed a stop marker: %+v %v", got, err)
		}
		info, err := tc.PaneInfoByID(ctx, *s.TmuxPaneID)
		if err != nil || info.Dead {
			t.Fatalf("refused lock stopped a process: %+v %v", info, err)
		}
	}
}

func TestWorkspaceLockRefusesDeadlineThatOmitsDetachedStopWrites(t *testing.T) {
	mgr, repo, tc, project := newTestEnv(t, sleeperDescriptor())
	ctx := context.Background()
	s, err := mgr.Create(ctx, CreateSpec{ProjectID: project, Provider: "claude", WorkDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	mgr.lifecycleHooks = &lifecycleTestHooks{stopSignaled: func() { t.Fatal("refused lock signaled a session") }}
	// Covers both stop windows but not the audit and finalization writes that
	// each stop may still spend after its window.
	windows := workspaceLockGracefulStop + workspaceLockForceStop
	shortCtx, cancel := context.WithTimeout(ctx, windows+time.Second)
	defer cancel()
	if err := mgr.PrepareWorkspaceLock(shortCtx); !errors.Is(err, ErrWorkspaceLockRefused) {
		t.Fatalf("deadline covering only the stop windows was not refused: %v", err)
	}
	got, err := repo.Get(ctx, s.ID)
	if err != nil || got.StopRequestedAt != nil || workspaceStopped(got) {
		t.Fatalf("refused lock committed a stop marker: %+v %v", got, err)
	}
	if info, err := tc.PaneInfoByID(ctx, *s.TmuxPaneID); err != nil || info.Dead {
		t.Fatalf("refused lock stopped a process: %+v %v", info, err)
	}
}

func TestWorkspaceLockWaitsBeforePersistingOrSignaling(t *testing.T) {
	mgr, repo, _, project := newTestEnv(t, sleeperDescriptor())
	ctx := context.Background()
	s, err := mgr.Create(ctx, CreateSpec{ProjectID: project, Provider: "claude", WorkDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	release, err := mgr.gate.Admit()
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	stopCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	timer := time.AfterFunc(10*time.Millisecond, cancel)
	defer timer.Stop()
	if err := mgr.PrepareWorkspaceLock(stopCtx); !errors.Is(err, context.Canceled) {
		t.Fatalf("did not drain admitted writer: %v", err)
	}
	got, err := repo.Get(ctx, s.ID)
	if err != nil || got.StopRequestedAt != nil {
		t.Fatal("stop crossed an undrained writer")
	}
	if len(auditTypes(t, repo, s.ID)) != 2 {
		t.Fatal("undrained lock signaled the CLI")
	}
}

func TestWorkspaceLockAcceptsObservedExitDuringPreparation(t *testing.T) {
	mgr, repo, tc, project := newTestEnv(t, sleeperDescriptor())
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	var sessions []*Session
	for range 2 {
		s, err := mgr.Create(ctx, CreateSpec{ProjectID: project, Provider: "claude", WorkDir: t.TempDir()})
		if err != nil {
			t.Fatal(err)
		}
		sessions = append(sessions, s)
	}
	mgr.lifecycleHooks = &lifecycleTestHooks{workspaceLockMarked: func() {
		if err := mgr.Stop(ctx, sessions[1].ID, false); err != nil {
			t.Fatal(err)
		}
	}}
	if err := mgr.PrepareWorkspaceLock(ctx); err != nil {
		t.Fatalf("already ended cohort member stranded lock: %v", err)
	}
	for _, s := range sessions {
		got, err := repo.Get(ctx, s.ID)
		if err != nil || !workspaceStopped(got) {
			t.Fatal("cohort intent was lost")
		}
		info, err := tc.PaneInfoByID(ctx, *s.TmuxPaneID)
		if err != nil || !info.Dead {
			t.Fatal("cohort member remains live")
		}
	}
}

func TestWorkspaceLockCohortCommitFailureSendsNoSignals(t *testing.T) {
	mgr, repo, tc, project := newTestEnv(t, sleeperDescriptor())
	ctx := context.Background()
	var sessions []*Session
	for range 2 {
		s, err := mgr.Create(ctx, CreateSpec{ProjectID: project, Provider: "claude", WorkDir: t.TempDir()})
		if err != nil {
			t.Fatal(err)
		}
		sessions = append(sessions, s)
	}
	err := repo.db.WithTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `CREATE TRIGGER reject_workspace_stop_audit
   BEFORE INSERT ON audit_events WHEN NEW.event_type = 'workspace.stop_requested'
   BEGIN SELECT RAISE(ABORT, 'synthetic audit storage failure'); END`)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	mgr.lifecycleHooks = &lifecycleTestHooks{stopSignaled: func() { t.Fatal("signal crossed a failed cohort transaction") }}
	if err := mgr.PrepareWorkspaceLock(ctx); err == nil {
		t.Fatal("failed durable intent was accepted")
	}
	for _, s := range sessions {
		got, err := repo.Get(ctx, s.ID)
		if err != nil || workspaceStopped(got) || got.StopRequestedAt != nil {
			t.Fatal("partial cohort survived transaction rollback")
		}
		info, err := tc.PaneInfoByID(ctx, *s.TmuxPaneID)
		if err != nil || info.Dead {
			t.Fatal("uncommitted stop killed a process")
		}
	}
}

func TestWorkspaceLockDrainsLaunchBeforeTakingCohort(t *testing.T) {
	mgr, repo, tc, project := newTestEnv(t, sleeperDescriptor())
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	spawned := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	tc.SetFinalSpawnFailureForTest(func(string) error { close(spawned); <-release; return nil })
	defer tc.SetFinalSpawnFailureForTest(nil)
	type result struct {
		s   *Session
		err error
	}
	launched := make(chan result, 1)
	workDir := t.TempDir()
	go func() {
		s, err := mgr.Create(ctx, CreateSpec{ProjectID: project, Provider: "claude", WorkDir: workDir})
		launched <- result{s, err}
	}()
	select {
	case <-spawned:
	case <-ctx.Done():
		t.Fatal("launch never reached native precommit boundary")
	}
	lockCtx, stop := context.WithCancel(ctx)
	timer := time.AfterFunc(10*time.Millisecond, stop)
	defer timer.Stop()
	err := mgr.PrepareWorkspaceLock(lockCtx)
	stop()
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("lock crossed a launch in flight: %v", err)
	}
	rows, err := repo.ListActive(ctx)
	if err != nil || len(rows) != 1 || workspaceStopped(rows[0]) {
		t.Fatal("lock recorded an undrained launch")
	}
	releaseOnce.Do(func() { close(release) })
	created := <-launched
	if created.err != nil || created.s == nil {
		t.Fatalf("admitted launch did not settle: %v", created.err)
	}
	if err := mgr.PrepareWorkspaceLock(ctx); err != nil {
		t.Fatal(err)
	}
	got, err := repo.Get(ctx, created.s.ID)
	if err != nil || !workspaceStopped(got) {
		t.Fatal("settled launch escaped the stop cohort")
	}
	info, err := tc.PaneInfoByID(ctx, *got.TmuxPaneID)
	if err != nil || !info.Dead {
		t.Fatal("settled launch remained alive")
	}
}

func TestWorkspaceLockRefusesLiveProvisionalRecovery(t *testing.T) {
	for _, automatic := range []bool{false, true} {
		name := "manual"
		if automatic {
			name = "automatic"
		}
		t.Run(name, func(t *testing.T) {
			mgr, repo, _, project := newAttestedResumeEnv(t)
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			source, err := mgr.Create(ctx, CreateSpec{
				ProjectID: project, Provider: provider.Codex, WorkDir: t.TempDir(),
			})
			if err != nil {
				t.Fatal(err)
			}
			crashAndObserve(t, mgr, source)
			runCtx, cancelRun := context.WithCancel(ctx)
			defer cancelRun()
			spawned := make(chan *tmux.PaneInfo, 1)
			release := make(chan struct{})
			var releaseOnce sync.Once
			defer releaseOnce.Do(func() { close(release) })
			mgr.lifecycleHooks = &lifecycleTestHooks{resumeSpawned: func(info *tmux.PaneInfo) {
				spawned <- info
				<-release
				cancelRun()
			}}
			recovered := make(chan error, 1)
			go func() {
				if automatic {
					recovered <- mgr.AutoResumeSweep(runCtx)
					return
				}
				_, _, err := mgr.Recover(runCtx, source.ID)
				recovered <- err
			}()
			var candidate *tmux.PaneInfo
			select {
			case candidate = <-spawned:
			case <-ctx.Done():
				t.Fatal("recovery never reached its provisional process")
			}
			lockCtx, cancelLock := context.WithTimeout(ctx, 10*time.Millisecond)
			err = mgr.PrepareWorkspaceLock(lockCtx)
			cancelLock()
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("lock crossed recovery in flight: %v", err)
			}
			releaseOnce.Do(func() { close(release) })
			if err := <-recovered; err == nil {
				t.Fatal("cancelled recovery unexpectedly committed")
			}
			mgr.lifecycleHooks = nil
			row, err := repo.Get(ctx, source.ID)
			if err != nil || !terminalRecoverySource(row) || row.Generation != source.Generation+1 {
				t.Fatalf("missing provisional terminal generation: %+v %v", row, err)
			}
			active, err := repo.ListActive(ctx)
			if err != nil || len(active) != 0 {
				t.Fatalf("fixture did not leave the active-row blind spot: %d %v", len(active), err)
			}
			if err := mgr.PrepareWorkspaceLock(ctx); !errors.Is(err, ErrUnverifiable) {
				t.Fatalf("lock accepted an unresolved live successor: %v", err)
			}
			row, err = repo.Get(ctx, source.ID)
			if err != nil || !workspaceStopped(row) || row.StopRequestedAt == nil {
				t.Fatal("uncertain recovery was not durably excluded from automatic resume")
			}
			slot, err := repo.SuccessorSlot(ctx, source.ID)
			if err != nil || slot == nil || slot.CandidateGeneration != row.Generation {
				t.Fatalf("lock discarded provisional identity: %+v %v", slot, err)
			}
			info, err := mgr.tmux.PaneInfoByID(ctx, candidate.PaneID)
			if err != nil || info.Dead || info.PID != candidate.PID {
				t.Fatalf("refusal guessed authority to stop the provisional process: %+v %v", info, err)
			}
			if _, err := mgr.gate.Admit(); !errors.Is(err, inputgate.ErrWorkspaceLocking) {
				t.Fatalf("uncertain lock reopened admission: %v", err)
			}
		})
	}
}

func TestWorkspaceLockIncludesLaunchAdmittedDuringPreflight(t *testing.T) {
	mgr, repo, tc, project := newTestEnv(t, sleeperDescriptor())
	ctx, cancel := context.WithTimeout(context.Background(), WorkspaceLockTimeout)
	defer cancel()
	var admitted *Session
	mgr.lifecycleHooks = &lifecycleTestHooks{workspaceLockPreflight: func() {
		var err error
		admitted, err = mgr.Create(ctx, CreateSpec{ProjectID: project, Provider: "claude", WorkDir: t.TempDir()})
		if err != nil {
			t.Fatal(err)
		}
	}}
	if err := mgr.PrepareWorkspaceLock(ctx); err != nil {
		t.Fatal(err)
	}
	if admitted == nil {
		t.Fatal("preflight did not admit the concurrent launch")
	}
	got, err := repo.Get(ctx, admitted.ID)
	if err != nil || !workspaceStopped(got) {
		t.Fatal("admitted launch lost durable stop intent")
	}
	info, err := tc.PaneInfoByID(ctx, *admitted.TmuxPaneID)
	if err != nil || !info.Dead {
		t.Fatalf("admitted launch remained live: %+v %v", info, err)
	}
}

func TestWorkspaceLockWorkerFailureCancelsQueuedStops(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	started := make(chan string, workspaceLockWorkers+1)
	fail := make(chan struct{})
	failed := errors.New("first stop failed")
	result := make(chan error, 1)
	cohort := make([]*Session, workspaceLockWorkers+1)
	for i := range cohort {
		cohort[i] = &Session{ID: string(rune('a' + i))}
	}
	go func() {
		result <- stopWorkspaceCohort(ctx, cohort, func(ctx context.Context, s *Session) error {
			started <- s.ID
			if s.ID == "a" {
				select {
				case <-fail:
					return failed
				case <-ctx.Done():
					return ctx.Err()
				}
			}
			<-ctx.Done()
			return ctx.Err()
		})
	}()
	for range workspaceLockWorkers {
		select {
		case id := <-started:
			if id == "e" {
				t.Fatal("worker limit exceeded")
			}
		case <-ctx.Done():
			t.Fatal("stop workers never started")
		}
	}
	close(fail)
	select {
	case err := <-result:
		if !errors.Is(err, failed) {
			t.Fatalf("first error lost: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("worker cancellation did not drain")
	}
	if len(started) != 0 {
		t.Fatal("queued stop ran after cancellation")
	}
}
