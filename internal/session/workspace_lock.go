package session

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/noisefloor-oss/deck/internal/audit"
	"github.com/noisefloor-oss/deck/internal/db"
)

// WorkspaceLockTimeout caps a whole lock preparation. The backend never grants
// preparation a later deadline, whatever budget its caller forwards.
const WorkspaceLockTimeout = 90 * time.Second

// WorkspaceLockBudgetHeader carries the caller's remaining preparation budget
// in whole milliseconds.
const WorkspaceLockBudgetHeader = "X-Noisedeck-Lock-Budget"

// ErrWorkspaceLockRefused reports a budget refusal before admission or durable stop state changes.
var ErrWorkspaceLockRefused = errors.New("workspace lock preparation refused")

const (
	workspaceLockWorkers        = 4
	workspaceLockGracefulStop   = 5 * time.Second
	workspaceLockForceStop      = 5 * time.Second
	workspaceLockObserveTimeout = 5 * time.Second

	// Each Stop call can follow its window with two detached bounded writes:
	// the stop_unobserved audit and the attempt finalization.
	workspaceLockStopsPerSession = 2
	workspaceLockWritesPerStop   = 2

	workspaceLockSessionBudget = workspaceLockGracefulStop + workspaceLockForceStop +
		workspaceLockObserveTimeout +
		workspaceLockStopsPerSession*workspaceLockWritesPerStop*stopDetachedWriteTimeout
)

// PrepareWorkspaceLock is the backend half of an explicitly authorized lock.
// It closes admission for this backend lifetime. The native controller must
// subsequently stop the entire workload and verify the encrypted mount closed;
// this method alone never establishes a locked workspace.
func (m *Manager) PrepareWorkspaceLock(ctx context.Context) error {
	cohort, err := m.repo.ListActive(ctx)
	if err != nil {
		return err
	}
	// Stops run in workspaceLockWorkers parallel batches. Refuse an undersized
	// budget before closing admission or recording stop intent.
	if deadline, ok := ctx.Deadline(); ok {
		batches := (len(cohort) + workspaceLockWorkers - 1) / workspaceLockWorkers
		need := time.Duration(batches) * workspaceLockSessionBudget
		if time.Until(deadline) < need {
			return fmt.Errorf("workspace lock: %d sessions need up to %s to stop, more than the time remaining: %w", len(cohort), need, ErrWorkspaceLockRefused)
		}
	}
	if m.lifecycleHooks != nil && m.lifecycleHooks.workspaceLockPreflight != nil {
		m.lifecycleHooks.workspaceLockPreflight()
	}
	if err := m.gate.FenceAndDrain(ctx); err != nil {
		return err
	}
	// Include launches admitted during preflight. Draining can consume time or
	// enlarge the cohort; after fencing, attempt the stops within the remaining
	// deadline and preserve the complete cohort markers if interrupted.
	cohort, err = m.repo.ListActive(ctx)
	if err != nil {
		return err
	}
	// One transaction marks the complete cohort before the first signal. An
	// interrupted stop must not leave its later members eligible for recovery.
	if err := m.repo.recordWorkspaceStop(ctx); err != nil {
		return err
	}
	if m.lifecycleHooks != nil && m.lifecycleHooks.workspaceLockMarked != nil {
		m.lifecycleHooks.workspaceLockMarked()
	}
	// An interrupted recovery can leave a live provisional CLI behind a
	// terminal session row. The ordinary active list cannot prove it stopped.
	slots, err := m.repo.ListSuccessorSlots(ctx)
	if err != nil {
		return err
	}
	if len(slots) != 0 {
		return fmt.Errorf("workspace lock: unresolved recovery: %w", ErrUnverifiable)
	}
	for _, s := range cohort {
		if s.Status != StatusRunning {
			return fmt.Errorf("workspace lock: unsettled session: %w", ErrUnverifiable)
		}
	}
	return stopWorkspaceCohort(ctx, cohort, m.stopWorkspaceSession)
}

func stopWorkspaceCohort(ctx context.Context, cohort []*Session, stop func(context.Context, *Session) error) error {
	stopCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	jobs := make(chan *Session)
	var workers sync.WaitGroup
	var firstError sync.Once
	var stopErr error
	for range min(workspaceLockWorkers, len(cohort)) {
		workers.Go(func() {
			for s := range jobs {
				if stopCtx.Err() != nil {
					continue
				}
				if err := stop(stopCtx, s); err != nil {
					firstError.Do(func() { stopErr = err; cancel() })
				}
			}
		})
	}
send:
	for _, s := range cohort {
		select {
		case jobs <- s:
		case <-stopCtx.Done():
			break send
		}
	}
	close(jobs)
	workers.Wait()
	if stopErr != nil {
		return stopErr
	}
	return ctx.Err()
}

func (m *Manager) stopWorkspaceSession(ctx context.Context, s *Session) error {
	stopCtx, cancel := context.WithTimeout(ctx, workspaceLockGracefulStop)
	err := m.Stop(stopCtx, s.ID, false)
	cancel()
	if errors.Is(err, ErrStopUnobserved) && ctx.Err() == nil {
		// Force re-observes this exact generation before signalling; an uncertain
		// earlier delivery alone never authorizes a signal to a stale process.
		forceCtx, cancel := context.WithTimeout(ctx, workspaceLockForceStop)
		err = m.Stop(forceCtx, s.ID, true)
		cancel()
	}
	if errors.Is(err, ErrNotRunning) && ctx.Err() == nil {
		// A terminal row alone cannot prove the original generation stopped.
		observeCtx, cancel := context.WithTimeout(ctx, workspaceLockObserveTimeout)
		observed := m.ObserveRuntime(observeCtx, s)
		cancel()
		if observed.Process == ProcessDead && (observed.IdentityVerified == nil || *observed.IdentityVerified) {
			return nil
		}
	}
	return err
}

func (r *Repository) recordWorkspaceStop(ctx context.Context) error {
	return r.db.WithTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		now := db.FormatTime(time.Now())
		// The generation marker survives ordinary stop expiry and compatibility
		// cleanup of recovery counters. Explicit Resume advances the generation.
		_, err := tx.ExecContext(ctx, `UPDATE sessions
			SET stop_requested_at = ?, workspace_stop_generation = generation, updated_at = ?
			WHERE removed_at IS NULL`, now, now)
		if err != nil {
			return err
		}
		return db.AppendAudit(ctx, tx, db.AuditEvent{Type: string(audit.WorkspaceStopRequested),
			Details: "workspace locking closed launch and input admission and recorded all session stops"})
	})
}

func workspaceStopped(s *Session) bool {
	return s != nil && s.Generation > 0 && s.WorkspaceStopGeneration == s.Generation
}
