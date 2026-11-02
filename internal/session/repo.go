package session

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/noisefloor-oss/deck/internal/db"
)

// Repository persists sessions. Every state transition performs its row
// change and appends its audit event inside one transaction — that pairing,
// not AppendAudit alone, keeps lifecycle changes and their audit events atomic.
type Repository struct {
	db *db.DB
}

// FleetCounts is the recorded lifecycle summary used by diagnostics. These
// are database facts, not claims that every active row is healthy; the
// reconciler metrics and per-session observation remain separate.
type FleetCounts struct {
	Active int `json:"active_sessions"`
}

// NewRepository wraps the control database.
func NewRepository(d *db.DB) *Repository { return &Repository{db: d} }

const sessionColumns = `id, project_id, provider, display_name, tmux_socket_path,
	tmux_session_name, tmux_session_id, tmux_pane_id, generation,
	identity_marker, pane_pid, process_group_id, process_start_ticks,
	expected_executable, native_session_id, status, exit_code, exit_signal,
	exited_at, created_at, updated_at, removed_at, resume_attempts,
	resume_next_at, resume_series_started_at, resume_probation_until,
	stop_requested_at, last_output_at, identity_capture_lost_at, boot_epoch, color,
	controller_revision, automatic_recoveries, automatic_recovery_series_at,
	generation_started_at, generation_resumed, native_store_root, workspace_stop_generation`

// Create inserts a new session in `starting` and appends AuditCreated.
func (r *Repository) Create(ctx context.Context, s *Session) error {
	now := time.Now()
	s.CreatedAt, s.UpdatedAt = now, now
	s.Status = StatusStarting
	return r.db.WithTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO sessions (id, project_id, provider, display_name, tmux_socket_path,
				tmux_session_name, generation, identity_marker, native_session_id, status,
				created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			s.ID, s.ProjectID, s.Provider, s.DisplayName, s.TmuxSocketPath,
			s.TmuxSessionName, s.Generation, s.IdentityMarker, s.NativeSessionID, s.Status,
			db.FormatTime(s.CreatedAt), db.FormatTime(s.UpdatedAt),
		); err != nil {
			return fmt.Errorf("session: insert %s: %w", s.ID, err)
		}
		return db.AppendAudit(ctx, tx, db.AuditEvent{
			SessionID: &s.ID,
			Type:      AuditCreated,
			Details:   fmt.Sprintf("provider=%s project=%s tmux=%s", s.Provider, s.ProjectID, s.TmuxSessionName),
		})
	})
}

// MarkRunning records the complete observed identity tuple and the running
// status in one write. The schema refuses `running` with a partial tuple,
// so a caller that failed to observe everything fails here rather
// than recording an unverifiable claim of liveness.
func (r *Repository) MarkRunning(ctx context.Context, s *Session) error {
	s.Status = StatusRunning
	s.UpdatedAt = time.Now()
	return r.db.WithTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `
			UPDATE sessions SET tmux_session_id = ?, tmux_pane_id = ?,
				generation = ?, identity_marker = ?, pane_pid = ?,
				process_group_id = ?, process_start_ticks = ?,
				expected_executable = ?, boot_epoch = ?, status = ?, updated_at = ?
			WHERE id = ? AND removed_at IS NULL AND status = ?
			  AND tmux_socket_path = ? AND tmux_session_name = ?
			  AND generation = ? AND identity_marker IS ?`,
			s.TmuxSessionID, s.TmuxPaneID, s.Generation, s.IdentityMarker,
			s.PanePID, s.ProcessGroupID, s.StartTicks, s.ExpectedExecutable,
			s.BootEpoch, s.Status, db.FormatTime(s.UpdatedAt), s.ID,
			StatusStarting, s.TmuxSocketPath, s.TmuxSessionName,
			s.Generation, s.IdentityMarker,
		)
		if err != nil {
			return fmt.Errorf("session: mark running %s: %w", s.ID, err)
		}
		if err := mustAffectOne(res, s.ID); err != nil {
			return err
		}
		return db.AppendAudit(ctx, tx, db.AuditEvent{
			SessionID: &s.ID,
			Type:      AuditRunning,
			Details: fmt.Sprintf("pane=%s pid=%d pgid=%d gen=%d",
				deref(s.TmuxPaneID), derefInt(s.PanePID), derefInt(s.ProcessGroupID), s.Generation),
		})
	})
}

// RestoreRunningObserved reactivates a detached row only when it still names
// the exact live runtime tuple a fresh observation verified. It is not pane
// adoption: every recorded identity member remains unchanged. Exit fields are
// cleared because a live row may not carry terminal evidence.
func (r *Repository) RestoreRunningObserved(ctx context.Context, s *Session) (bool, error) {
	applied := false
	err := r.db.WithTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `
			UPDATE sessions SET status = ?, exit_code = NULL, exit_signal = NULL,
				exited_at = NULL, resume_probation_until = NULL, updated_at = ?
			WHERE id = ? AND removed_at IS NULL AND status = ?
			  AND tmux_socket_path = ? AND tmux_session_id IS ?
			  AND tmux_pane_id IS ? AND generation = ?
			  AND identity_marker IS ? AND pane_pid IS ?
			  AND process_group_id IS ? AND process_start_ticks IS ?
			  AND expected_executable IS ? AND boot_epoch IS ?
			  AND NOT EXISTS (
				SELECT 1 FROM session_successor_slots slot
				WHERE slot.session_id = sessions.id
			  )`,
			StatusRunning, db.FormatTime(time.Now()), s.ID, s.Status,
			s.TmuxSocketPath, s.TmuxSessionID, s.TmuxPaneID, s.Generation,
			s.IdentityMarker, s.PanePID, s.ProcessGroupID, s.StartTicks,
			s.ExpectedExecutable, s.BootEpoch,
		)
		if err != nil {
			return fmt.Errorf("session: restore observed running %s: %w", s.ID, err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n == 0 {
			return nil
		}
		applied = true
		return db.AppendAudit(ctx, tx, db.AuditEvent{
			SessionID: &s.ID,
			Type:      AuditRunning,
			Details:   "detached session matched its complete recorded runtime identity",
		})
	})
	return applied, err
}

// RestoreLegacyRunningObserved attaches an exact live legacy row and clears
// retry-series residue written by earlier releases. The now-verified
// generation must receive a fresh automatic opportunity if it is later lost.
func (r *Repository) RestoreLegacyRunningObserved(ctx context.Context, s *Session) (bool, error) {
	applied := false
	err := r.db.WithTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `
			UPDATE sessions SET status = ?, exit_code = NULL, exit_signal = NULL,
				exited_at = NULL, resume_attempts = 0, resume_next_at = NULL,
				resume_series_started_at = NULL, resume_probation_until = NULL,
				updated_at = ?
			WHERE id = ? AND removed_at IS NULL AND status = ?
			  AND controller_revision = ?
			  AND tmux_socket_path = ? AND tmux_session_id IS ?
			  AND tmux_pane_id IS ? AND generation = ?
			  AND identity_marker IS ? AND pane_pid IS ?
			  AND process_group_id IS ? AND process_start_ticks IS ?
			  AND expected_executable IS ? AND boot_epoch IS ?
			  AND NOT EXISTS (
				SELECT 1 FROM session_successor_slots slot
				WHERE slot.session_id = sessions.id
			  )`,
			StatusRunning, db.FormatTime(time.Now()), s.ID, StatusNeedsAttention,
			s.ControllerRevision, s.TmuxSocketPath, s.TmuxSessionID, s.TmuxPaneID,
			s.Generation, s.IdentityMarker, s.PanePID, s.ProcessGroupID,
			s.StartTicks, s.ExpectedExecutable, s.BootEpoch)
		if err != nil {
			return fmt.Errorf("session: restore legacy running %s: %w", s.ID, err)
		}
		n, err := res.RowsAffected()
		if err != nil || n == 0 {
			return err
		}
		applied = true
		return db.AppendAudit(ctx, tx, db.AuditEvent{
			SessionID: &s.ID, Type: AuditRunning,
			Details: "legacy row attached to its exact live controller",
		})
	})
	return applied, err
}

// ConvergeLegacyRecoveryResidue clears retry/probation residue written by an
// earlier release once the exact current controller is observed live. New
// recovery commits re-arm their verified generation atomically and never
// write these released compatibility columns.
func (r *Repository) ConvergeLegacyRecoveryResidue(ctx context.Context, s *Session) (bool, error) {
	applied := false
	updatedAt := time.Now()
	err := r.db.WithTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		applied = false
		res, err := tx.ExecContext(ctx, `
			UPDATE sessions SET resume_attempts = 0, resume_next_at = NULL,
				resume_series_started_at = NULL, resume_probation_until = NULL,
				updated_at = ?
			WHERE id = ? AND removed_at IS NULL AND status = ?
			  AND generation = ? AND identity_marker IS ?
			  AND tmux_session_id IS ? AND tmux_pane_id IS ?
			  AND native_session_id IS ? AND pane_pid IS ?
			  AND process_group_id IS ? AND process_start_ticks IS ?
			  AND expected_executable IS ? AND boot_epoch IS ?
			  AND controller_revision = ? AND resume_attempts = ?
			  AND resume_probation_until IS ? AND stop_requested_at IS NULL`,
			db.FormatTime(updatedAt), s.ID,
			StatusRunning, s.Generation, s.IdentityMarker,
			s.TmuxSessionID, s.TmuxPaneID,
			s.NativeSessionID, s.PanePID, s.ProcessGroupID, s.StartTicks,
			s.ExpectedExecutable, s.BootEpoch, s.ControllerRevision, s.ResumeAttempts,
			optionalStoredTime(s.ResumeProbationUntil),
		)
		if err != nil {
			return fmt.Errorf("session: converge legacy recovery residue %s: %w", s.ID, err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n == 0 {
			return nil // residue already gone, or the controller tuple changed
		}
		applied = true
		return db.AppendAudit(ctx, tx, db.AuditEvent{
			SessionID: &s.ID,
			Type:      AuditRecoveryLegacyConverged,
			Details: fmt.Sprintf("legacy recovery residue cleared: gen=%d pane=%s pid=%d",
				s.Generation, deref(s.TmuxPaneID), derefInt(s.PanePID)),
		})
	})
	if applied && err == nil {
		s.ResumeAttempts = 0
		s.ResumeNextAt = nil
		s.ResumeSeriesStartedAt = nil
		s.ResumeProbationUntil = nil
		s.UpdatedAt = updatedAt
	}
	return applied, err
}

func optionalStoredTime(t *time.Time) any {
	if t == nil {
		return nil
	}
	return db.FormatTime(*t)
}

// RecordExitObserved records exit evidence only while the row still names the
// exact runtime generation that produced it. A stale observer must not apply
// an old pane's death to a replacement generation that recovery has already
// published.
func (r *Repository) RecordExitObserved(ctx context.Context, s *Session, code *int, signal *string, observedAt time.Time) (bool, error) {
	if (code == nil) == (signal == nil) {
		return false, fmt.Errorf("session: exit evidence for %s must be exactly one of code or signal", s.ID)
	}
	applied := false
	err := r.db.WithTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `
			UPDATE sessions SET status = ?, exit_code = ?, exit_signal = ?,
				exited_at = ?, resume_probation_until = NULL, updated_at = ?
			WHERE id = ? AND removed_at IS NULL AND status = ?
			  AND tmux_socket_path = ? AND tmux_session_id IS ?
			  AND tmux_pane_id IS ? AND generation = ?
			  AND identity_marker IS ? AND pane_pid IS ?
			  AND process_group_id IS ? AND process_start_ticks IS ?
			  AND expected_executable IS ? AND boot_epoch IS ?`,
			StatusExited, code, signal,
			db.FormatTime(observedAt), db.FormatTime(time.Now()), s.ID, s.Status,
			s.TmuxSocketPath, s.TmuxSessionID, s.TmuxPaneID, s.Generation,
			s.IdentityMarker, s.PanePID, s.ProcessGroupID, s.StartTicks,
			s.ExpectedExecutable, s.BootEpoch,
		)
		if err != nil {
			return fmt.Errorf("session: record observed exit %s: %w", s.ID, err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n == 0 {
			return nil
		}
		applied = true
		// Decided here rather than at a caller so that EVERY observer applies
		// it. The reconciler's cadence pass is almost always the first to see
		// a resume that failed on the way up — the process is gone seconds
		// after commit — and once it records the exit the row is terminal and
		// the read path never looks again. A refusal noticed only on the read
		// path is a refusal never noticed at all.
		if refusedResume(s, code, signal, observedAt) {
			if err := closeRecoveryOpportunityTx(ctx, tx, s, refusedResumeAuditDetail); err != nil {
				return err
			}
		}
		if s.IdentityMarker != nil {
			if err := expireIdentityCaptureTx(ctx, tx, s.ID, s.Generation,
				*s.IdentityMarker, observedAt,
				"provider process ended before identity capture"); err != nil {
				return err
			}
		}
		detail := "signal=" + deref(signal)
		if code != nil {
			detail = fmt.Sprintf("code=%d", *code)
		}
		return db.AppendAudit(ctx, tx, db.AuditEvent{
			SessionID: &s.ID, Type: AuditExited, Details: detail,
		})
	})
	return applied, err
}

// RecordLegacyExitObserved converts exact legacy death evidence while
// explicitly replacing retry-series residue. A qualifying lost generation is
// re-armed once; clean, stopped, and otherwise ineligible deaths stay closed.
func (r *Repository) RecordLegacyExitObserved(ctx context.Context, s *Session, code *int,
	signal *string, observedAt time.Time, allowAutomatic bool) (bool, error) {
	if (code == nil) == (signal == nil) {
		return false, fmt.Errorf("session: exit evidence for %s must be exactly one of code or signal", s.ID)
	}
	attempts := 1
	if allowAutomatic {
		attempts = 0
	}
	applied := false
	err := r.db.WithTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `
			UPDATE sessions SET status = ?, exit_code = ?, exit_signal = ?,
				exited_at = ?, resume_attempts = ?, resume_next_at = NULL,
				resume_series_started_at = NULL, resume_probation_until = NULL,
				updated_at = ?
			WHERE id = ? AND removed_at IS NULL AND status = ?
			  AND controller_revision = ?
			  AND tmux_socket_path = ? AND tmux_session_id IS ?
			  AND tmux_pane_id IS ? AND generation = ?
			  AND identity_marker IS ? AND pane_pid IS ?
			  AND process_group_id IS ? AND process_start_ticks IS ?
			  AND expected_executable IS ? AND boot_epoch IS ?`,
			StatusExited, code, signal, db.FormatTime(observedAt), attempts,
			db.FormatTime(time.Now()), s.ID, StatusNeedsAttention,
			s.ControllerRevision, s.TmuxSocketPath, s.TmuxSessionID, s.TmuxPaneID,
			s.Generation, s.IdentityMarker, s.PanePID, s.ProcessGroupID,
			s.StartTicks, s.ExpectedExecutable, s.BootEpoch)
		if err != nil {
			return fmt.Errorf("session: record legacy observed exit %s: %w", s.ID, err)
		}
		n, err := res.RowsAffected()
		if err != nil || n == 0 {
			return err
		}
		applied = true
		// Decided here rather than at a caller so that EVERY observer applies
		// it. The reconciler's cadence pass is almost always the first to see
		// a resume that failed on the way up — the process is gone seconds
		// after commit — and once it records the exit the row is terminal and
		// the read path never looks again. A refusal noticed only on the read
		// path is a refusal never noticed at all.
		if refusedResume(s, code, signal, observedAt) {
			if err := closeRecoveryOpportunityTx(ctx, tx, s, refusedResumeAuditDetail); err != nil {
				return err
			}
		}
		if s.IdentityMarker != nil {
			if err := expireIdentityCaptureTx(ctx, tx, s.ID, s.Generation,
				*s.IdentityMarker, observedAt,
				"provider process ended before identity capture"); err != nil {
				return err
			}
		}
		detail := "signal=" + deref(signal)
		if code != nil {
			detail = fmt.Sprintf("code=%d", *code)
		}
		return db.AppendAudit(ctx, tx, db.AuditEvent{
			SessionID: &s.ID, Type: AuditExited, Details: detail,
		})
	})
	return applied, err
}

// RecordEarlyExit persists a launch whose pane was already dead when first
// observed: the durable identity members (session id, pane id, marker) plus
// the exit evidence, in one transaction. The live-only tuple members stay
// NULL — the process they described is gone, and the schema permits that for
// every status except `running`.
func (r *Repository) RecordEarlyExit(ctx context.Context, s *Session, code *int, signal *string, observedAt time.Time) error {
	if (code == nil) == (signal == nil) {
		return fmt.Errorf("session: exit evidence for %s must be exactly one of code or signal", s.ID)
	}
	s.Status = StatusExited
	s.ExitCode, s.ExitSignal = code, signal
	s.ExitedAt = &observedAt
	s.ResumeAttempts = 1
	s.UpdatedAt = time.Now()
	return r.db.WithTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `
			UPDATE sessions SET tmux_session_id = ?, tmux_pane_id = ?,
				identity_marker = ?, status = ?, exit_code = ?,
				exit_signal = ?, exited_at = ?, resume_attempts = 1,
				updated_at = ?
			WHERE id = ? AND removed_at IS NULL AND status = ?
			  AND tmux_socket_path = ? AND tmux_session_name = ?
			  AND tmux_session_id IS NULL AND tmux_pane_id IS NULL
			  AND generation = ? AND identity_marker IS ?`,
			s.TmuxSessionID, s.TmuxPaneID, s.IdentityMarker, s.Status,
			s.ExitCode, s.ExitSignal, db.FormatTime(observedAt),
			db.FormatTime(s.UpdatedAt), s.ID, StatusStarting,
			s.TmuxSocketPath, s.TmuxSessionName, s.Generation, s.IdentityMarker,
		)
		if err != nil {
			return fmt.Errorf("session: record early exit %s: %w", s.ID, err)
		}
		if err := mustAffectOne(res, s.ID); err != nil {
			return err
		}
		if s.IdentityMarker != nil {
			if err := expireIdentityCaptureTx(ctx, tx, s.ID, s.Generation,
				*s.IdentityMarker, observedAt,
				"provider process ended during launch before identity capture"); err != nil {
				return err
			}
		}
		detail := "exited during launch: signal=" + deref(signal)
		if code != nil {
			detail = fmt.Sprintf("exited during launch: code=%d", *code)
		}
		return db.AppendAudit(ctx, tx, db.AuditEvent{
			SessionID: &s.ID, Type: AuditExited, Details: detail,
		})
	})
}

// RecordEarlyInterrupt persists a launch whose pane was already dead when
// first observed but preserved no exit evidence: the durable identity
// members plus interrupted status and resume_attempts = 1 (a generation
// that never reached verified-live commitment consumes its non-existent
// auto opportunity).
func (r *Repository) RecordEarlyInterrupt(ctx context.Context, s *Session, reason string) error {
	s.Status = StatusInterrupted
	s.UpdatedAt = time.Now()
	return r.db.WithTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `
			UPDATE sessions SET tmux_session_id = ?, tmux_pane_id = ?,
				identity_marker = ?, status = ?, resume_attempts = 1,
				updated_at = ?
			WHERE id = ? AND removed_at IS NULL AND status = ?
			  AND tmux_socket_path = ? AND tmux_session_name = ?
			  AND tmux_session_id IS NULL AND tmux_pane_id IS NULL
			  AND generation = ? AND identity_marker IS ?`,
			s.TmuxSessionID, s.TmuxPaneID, s.IdentityMarker, s.Status,
			db.FormatTime(s.UpdatedAt), s.ID, StatusStarting,
			s.TmuxSocketPath, s.TmuxSessionName, s.Generation, s.IdentityMarker,
		)
		if err != nil {
			return fmt.Errorf("session: record early interrupt %s: %w", s.ID, err)
		}
		if err := mustAffectOne(res, s.ID); err != nil {
			return err
		}
		if s.IdentityMarker != nil {
			if err := expireIdentityCaptureTx(ctx, tx, s.ID, s.Generation,
				*s.IdentityMarker, s.UpdatedAt,
				"provider process ended during launch before identity capture"); err != nil {
				return err
			}
		}
		return db.AppendAudit(ctx, tx, db.AuditEvent{
			SessionID: &s.ID, Type: AuditInterrupted, Details: reason,
		})
	})
}

// refusedResumeAuditDetail is the audit line for a resumed generation that
// ended on the way up. It states what was decided and what was kept; the
// provider's own words stay in the pane where it printed them.
const refusedResumeAuditDetail = "the resumed generation ended on the way up; automatic recovery stops and the recorded conversation is kept for an explicit resume or a fresh session"

// StopAttributionWindow bounds how long a stop request stays attributable
// to a death. A graceful signal either ends the CLI within this window or
// the CLI has provably survived it as an ongoing process; without the bound,
// one survived stop would silently disable automatic recovery for the rest
// of the session's life. Two minutes is generous against every measured
// provider shutdown (docs/provider-contracts: SIGTERM is honoured promptly)
// while short enough that a session crashing hours later is unmistakably a
// new death.
const StopAttributionWindow = 2 * time.Minute

// ExpireStopRequest clears a stop request that the CLI provably survived:
// the caller OBSERVED the session alive, and the request is older than
// StopAttributionWindow. CAS-guarded on the row still being live with the
// old stamp — a fresh stop request or a death that races this write wins,
// and nothing (including the audit event) is recorded for the loser. The
// clearing re-arms automatic recovery for the session's next death and is
// audited as its own fact.
func (r *Repository) ExpireStopRequest(ctx context.Context, id string, requestedBefore time.Time) (bool, error) {
	applied := false
	err := r.db.WithTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		applied = false
		res, err := tx.ExecContext(ctx, `
			UPDATE sessions SET stop_requested_at = NULL, updated_at = ?
			WHERE id = ? AND removed_at IS NULL
			  AND stop_requested_at IS NOT NULL AND stop_requested_at <= ?
			  AND status IN (?, ?)`,
			db.FormatTime(time.Now()), id, db.FormatTime(requestedBefore),
			StatusStarting, StatusRunning)
		if err != nil {
			return fmt.Errorf("session %s: expire stop request: %w", id, err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n == 0 {
			return nil
		}
		applied = true
		return db.AppendAudit(ctx, tx, db.AuditEvent{
			SessionID: &id,
			Type:      AuditStopSurvived,
			Details: fmt.Sprintf("stop request expired: the CLI survived the signal past %s; automatic recovery re-armed",
				StopAttributionWindow),
		})
	})
	return applied, err
}

// ListResumeCandidates returns dead sessions the automatic recovery sweep
// must consider: exited or interrupted, not removed, not the
// result of a user stop request, and not a clean exit. A normal CLI
// exit preserves the evidence instead of undoing the
// user's own /exit. Provider and identity policy (identity-none
// exclusion, missing-identity parking, capture-loss parking) is the
// sweep's, not the query's: it needs the descriptor table this layer
// does not have.
func (r *Repository) ListResumeCandidates(ctx context.Context) ([]*Session, error) {
	rows, err := r.db.Reader().QueryContext(ctx,
		`SELECT `+sessionColumns+` FROM sessions
		 WHERE status IN (?, ?) AND removed_at IS NULL
		   AND stop_requested_at IS NULL
		   AND workspace_stop_generation != generation
		   AND resume_attempts = 0
		   AND NOT EXISTS (
		     SELECT 1 FROM session_successor_slots slot
		     WHERE slot.session_id = sessions.id
		   )
		   AND NOT (status = ? AND COALESCE(exit_code, -1) = 0)
		 -- Newest first: whatever a bounded pass cannot reach must be the
		 -- oldest rows, never the sessions the user just lost, and a
		 -- same-millisecond creation tie falls to the younger rowid.
		 ORDER BY created_at DESC, rowid DESC`,
		StatusExited, StatusInterrupted, StatusExited)
	if err != nil {
		return nil, fmt.Errorf("session: list resume candidates: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []*Session
	for rows.Next() {
		s, err := scanSessionRows(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("session: list resume candidates: %w", err)
	}
	return out, nil
}

// AppendEvent records an audit-only event (for example a removal
// escalation, which must be recorded even though the row does not change
// yet).
func (r *Repository) AppendEvent(ctx context.Context, id, auditType, details string) error {
	return r.db.WithTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		return db.AppendAudit(ctx, tx, db.AuditEvent{
			SessionID: &id, Type: auditType, Details: details,
		})
	})
}

// Get loads one session.
func (r *Repository) Get(ctx context.Context, id string) (*Session, error) {
	row := r.db.Reader().QueryRowContext(ctx,
		`SELECT `+sessionColumns+` FROM sessions WHERE id = ?`, id)
	return scanSession(row)
}

// GetByMarker loads the session that recorded this identity marker. Markers
// are per-launch UUIDs, so unlike names, pane ids, and pids they are never
// recycled — which is what makes them the join key between an observed pane
// and a recorded session.
func (r *Repository) GetByMarker(ctx context.Context, marker string) (*Session, error) {
	row := r.db.Reader().QueryRowContext(ctx,
		`SELECT `+sessionColumns+` FROM sessions WHERE identity_marker = ?`, marker)
	return scanSession(row)
}

// ListActive returns every session recorded as starting or running — the
// rows whose claims reconciliation must test against observed reality.
func (r *Repository) ListActive(ctx context.Context) ([]*Session, error) {
	rows, err := r.db.Reader().QueryContext(ctx,
		`SELECT `+sessionColumns+` FROM sessions
		 WHERE status IN (?, ?) AND removed_at IS NULL ORDER BY created_at`,
		StatusStarting, StatusRunning)
	if err != nil {
		return nil, fmt.Errorf("session: list active: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []*Session
	for rows.Next() {
		s, err := scanSessionRows(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("session: list active: %w", err)
	}
	return out, nil
}

// ListTemporaryOwners returns the durable generation facts needed to retain
// provider temporary directories. Liveness is established by the caller from
// the recorded process tuple; lifecycle status alone is never treated as
// process reality.
func (r *Repository) ListTemporaryOwners(ctx context.Context) ([]*Session, error) {
	rows, err := r.db.Reader().QueryContext(ctx,
		`SELECT `+sessionColumns+` FROM sessions
		 WHERE identity_marker IS NOT NULL ORDER BY created_at`)
	if err != nil {
		return nil, fmt.Errorf("session: list temporary owners: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []*Session
	for rows.Next() {
		s, err := scanSessionRows(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("session: list temporary owners: %w", err)
	}
	return out, nil
}

// ListLegacyNeedsAttention is upgrade input only. No product path writes this
// frozen released status, and boot convergence drains it before readiness.
func (r *Repository) ListLegacyNeedsAttention(ctx context.Context) ([]*Session, error) {
	rows, err := r.db.Reader().QueryContext(ctx,
		`SELECT `+sessionColumns+` FROM sessions
		 WHERE status = ? AND removed_at IS NULL ORDER BY created_at`,
		StatusNeedsAttention)
	if err != nil {
		return nil, fmt.Errorf("session: list detached: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []*Session
	for rows.Next() {
		s, err := scanSessionRows(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("session: list detached rows: %w", err)
	}
	return out, nil
}

// ConvergeLegacyInterrupted replaces one exact legacy row with passive
// interrupted metadata. A complete, positively lost committed generation
// receives one open automatic opportunity; uncertainty or incomplete legacy
// identity closes it without asserting an exit.
func (r *Repository) ConvergeLegacyInterrupted(ctx context.Context, s *Session,
	allowAutomatic bool, detail string) (bool, error) {
	// An incomplete tuple has no verified generation and therefore no
	// automatic opportunity to consume. Keep its counter at zero so the
	// complete-tuple classifier agrees that the row is inert. A stopped
	// complete generation stays explicitly consumed.
	attempts := 0
	if s.StopRequestedAt != nil || (!allowAutomatic && completeVerifiedGeneration(s)) {
		attempts = 1
	}
	applied := false
	err := r.db.WithTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `
			UPDATE sessions SET status = ?, resume_attempts = ?,
				resume_next_at = NULL, resume_series_started_at = NULL,
				resume_probation_until = NULL, updated_at = ?
			WHERE id = ? AND removed_at IS NULL AND status = ?
			  AND controller_revision = ?`,
			StatusInterrupted, attempts, db.FormatTime(time.Now()),
			s.ID, StatusNeedsAttention, s.ControllerRevision)
		if err != nil {
			return fmt.Errorf("session %s: converge legacy row: %w", s.ID, err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n == 0 {
			return nil
		}
		applied = true
		return db.AppendAudit(ctx, tx, db.AuditEvent{
			SessionID: &s.ID, Type: AuditInterrupted, Details: detail,
		})
	})
	return applied, err
}

// Counts reports active rows in one snapshot. Removed sessions are excluded
// because they are no longer part of the user-visible fleet.
func (r *Repository) Counts(ctx context.Context) (FleetCounts, error) {
	var counts FleetCounts
	err := r.db.Reader().QueryRowContext(ctx, `
		SELECT COALESCE(SUM(CASE WHEN status IN (?, ?) THEN 1 ELSE 0 END), 0)
		FROM sessions WHERE removed_at IS NULL`,
		StatusStarting, StatusRunning,
	).Scan(&counts.Active)
	if err != nil {
		return FleetCounts{}, fmt.Errorf("session: count fleet state: %w", err)
	}
	return counts, nil
}

// HarnessUsage is one harness's recorded launch history: how many sessions
// were started with it and when the most recent one was created. It counts
// rows, which is what was launched — never a claim about what is alive now,
// which stays the reconciler's.
type HarnessUsage struct {
	Provider   string    `json:"provider"`
	Launches   int       `json:"launches"`
	LastUsedAt time.Time `json:"last_used_at"`
}

// UsageByProvider aggregates recorded launches per harness, oldest history
// included: an ended or interrupted session is still a launch the user
// made, and only a removed row leaves the count. An empty projectID reads
// every project; otherwise idx_sessions_project serves the project predicate
// and the grouping is built on top of it. A scope with no history is an
// empty result, not an error — a project nobody has launched in is an
// ordinary state.
func (r *Repository) UsageByProvider(ctx context.Context, projectID string) ([]HarnessUsage, error) {
	query := `SELECT provider, COUNT(*), MAX(created_at) FROM sessions
		WHERE removed_at IS NULL`
	args := []any{}
	if projectID != "" {
		query += ` AND project_id = ?`
		args = append(args, projectID)
	}
	query += ` GROUP BY provider ORDER BY provider`
	rows, err := r.db.Reader().QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("session: usage by provider: %w", err)
	}
	defer func() { _ = rows.Close() }()
	usage := []HarnessUsage{}
	for rows.Next() {
		var one HarnessUsage
		var lastUsed string
		if err := rows.Scan(&one.Provider, &one.Launches, &lastUsed); err != nil {
			return nil, fmt.Errorf("session: usage by provider: %w", err)
		}
		if one.LastUsedAt, err = time.Parse(db.TimeFormat, lastUsed); err != nil {
			return nil, fmt.Errorf("session: usage %s: last used: %w", one.Provider, err)
		}
		usage = append(usage, one)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("session: usage by provider: %w", err)
	}
	return usage, nil
}

// ListByProject returns every session of one project, newest first, in
// ALL statuses: the sidebar groups sessions by project and must show the
// dead alongside the live (their statuses are the reconciler's).
func (r *Repository) ListByProject(ctx context.Context, projectID string) ([]*Session, error) {
	rows, err := r.db.Reader().QueryContext(ctx,
		`SELECT `+sessionColumns+` FROM sessions
		 WHERE project_id = ? AND removed_at IS NULL ORDER BY created_at DESC`, projectID)
	if err != nil {
		return nil, fmt.Errorf("session: list by project: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []*Session
	for rows.Next() {
		s, err := scanSessionRows(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("session: list by project: %w", err)
	}
	return out, nil
}

// MarkInterruptedObserved records an evidence-free death only for the exact
// runtime generation whose absence or dead pane the caller observed.
func (r *Repository) MarkInterruptedObserved(ctx context.Context, s *Session, reason string) (bool, error) {
	return r.transitionObserved(ctx, s, StatusInterrupted, AuditInterrupted, reason, false)
}

// MarkDeadInterruptedObserved is the positive-death form used only when tmux
// returned the exact fenced pane as dead but carried no exit status/signal.
// It closes that generation's pending capture in the lifecycle transaction.
func (r *Repository) MarkDeadInterruptedObserved(ctx context.Context, s *Session, reason string) (bool, error) {
	return r.transitionObserved(ctx, s, StatusInterrupted, AuditInterrupted, reason, true)
}

// MarkInterruptedFromStarting transitions a starting row that never reached
// verified-live commitment to interrupted. Because nothing was verified lost,
// the automatic opportunity is consumed (resume_attempts = 1): a row whose
// generation never reached running never creates one. The CAS
// matches the exact starting identity so a concurrent MarkRunning or launch
// failure refuses the write.
func (r *Repository) MarkInterruptedFromStarting(ctx context.Context, s *Session, reason string) (bool, error) {
	applied := false
	err := r.db.WithTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `
			UPDATE sessions SET status = ?, resume_attempts = 1,
				resume_probation_until = NULL, resume_next_at = NULL,
				updated_at = ?
			WHERE id = ? AND removed_at IS NULL AND status = ?
			  AND tmux_socket_path = ? AND tmux_session_id IS ?
			  AND tmux_pane_id IS ? AND generation = ?
			  AND identity_marker IS ?`,
			StatusInterrupted, db.FormatTime(time.Now()), s.ID, StatusStarting,
			s.TmuxSocketPath, s.TmuxSessionID, s.TmuxPaneID, s.Generation,
			s.IdentityMarker,
		)
		if err != nil {
			return fmt.Errorf("session: interrupt from starting %s: %w", s.ID, err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n == 0 {
			return nil
		}
		applied = true
		return db.AppendAudit(ctx, tx, db.AuditEvent{
			SessionID: &s.ID, Type: AuditInterrupted, Details: reason,
		})
	})
	return applied, err
}

// AppendOrphanEvent records an audit event bound to no session: a pane in
// the managed namespace that no recorded session claims.
func (r *Repository) AppendOrphanEvent(ctx context.Context, details string) error {
	return r.db.WithTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		return db.AppendAudit(ctx, tx, db.AuditEvent{
			Type: AuditUnknownPane, Details: details,
		})
	})
}

func (r *Repository) transitionObserved(ctx context.Context, s *Session, to Status,
	auditType, details string, closeCapture bool) (bool, error) {
	applied := false
	err := r.db.WithTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `
			UPDATE sessions SET status = ?, resume_probation_until = NULL, updated_at = ?
			WHERE id = ? AND removed_at IS NULL AND status = ?
			  AND tmux_socket_path = ? AND tmux_session_id IS ?
			  AND tmux_pane_id IS ? AND generation = ?
			  AND identity_marker IS ? AND pane_pid IS ?
			  AND process_group_id IS ? AND process_start_ticks IS ?
			  AND expected_executable IS ? AND boot_epoch IS ?`,
			to, db.FormatTime(time.Now()), s.ID, s.Status,
			s.TmuxSocketPath, s.TmuxSessionID, s.TmuxPaneID, s.Generation,
			s.IdentityMarker, s.PanePID, s.ProcessGroupID, s.StartTicks,
			s.ExpectedExecutable, s.BootEpoch,
		)
		if err != nil {
			return fmt.Errorf("session: transition observed %s to %s: %w", s.ID, to, err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n == 0 {
			return nil
		}
		applied = true
		if closeCapture && s.IdentityMarker != nil {
			if err := expireIdentityCaptureTx(ctx, tx, s.ID, s.Generation,
				*s.IdentityMarker, time.Now(),
				"provider process ended before identity capture"); err != nil {
				return err
			}
		}
		return db.AppendAudit(ctx, tx, db.AuditEvent{
			SessionID: &s.ID, Type: auditType, Details: details,
		})
	})
	return applied, err
}

func mustAffectOne(res sql.Result, id string) error {
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("session %s: %w", id, ErrNotFound)
	}
	return nil
}

// scanner covers *sql.Row and *sql.Rows.
type scanner interface{ Scan(dest ...any) error }

// SetDisplayName records the user's own label for a session.
// An empty name clears it — the row renders its provider again. The
// rename and its audit event commit in one transaction; a
// removed session has nothing left to name.
func (r *Repository) SetDisplayName(ctx context.Context, id, name string) error {
	if len(name) > 100 {
		return fmt.Errorf("session %s: display name over 100 characters", id)
	}
	return r.db.WithTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`UPDATE sessions SET display_name = ?, updated_at = ?
			 WHERE id = ? AND removed_at IS NULL`,
			name, db.FormatTime(time.Now()), id)
		if err != nil {
			return fmt.Errorf("session %s: rename: %w", id, err)
		}
		if err := mustAffectOne(res, id); err != nil {
			return err
		}
		return db.AppendAudit(ctx, tx, db.AuditEvent{
			SessionID: &id,
			Type:      AuditRenamed,
			Details:   fmt.Sprintf("display_name=%q", name),
		})
	})
}

// SetColor sets the user-assigned color tag for a session.
func (r *Repository) SetColor(ctx context.Context, id string, color *string) error {
	return r.db.WithTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`UPDATE sessions SET color = ?, updated_at = ?
			 WHERE id = ? AND removed_at IS NULL`,
			color, db.FormatTime(time.Now()), id)
		if err != nil {
			return fmt.Errorf("session %s: set color: %w", id, err)
		}
		if err := mustAffectOne(res, id); err != nil {
			return err
		}
		tag := "none"
		if color != nil {
			tag = *color
		}
		return db.AppendAudit(ctx, tx, db.AuditEvent{
			SessionID: &id,
			Type:      AuditColorChanged,
			Details:   fmt.Sprintf("color=%s", tag),
		})
	})
}

func scanSession(row *sql.Row) (*Session, error) { return scanFrom(row) }

func scanSessionRows(rows *sql.Rows) (*Session, error) { return scanFrom(rows) }

func scanFrom(row scanner) (*Session, error) {
	var s Session
	var createdAt, updatedAt string
	var exitedAt, removedAt, resumeNextAt, seriesStartedAt, probationUntil, stopRequestedAt *string
	var lastOutputAt *string
	var captureLostAt *string
	var recoverySeriesAt *string
	var generationStartedAt *string
	err := row.Scan(&s.ID, &s.ProjectID, &s.Provider, &s.DisplayName, &s.TmuxSocketPath,
		&s.TmuxSessionName, &s.TmuxSessionID, &s.TmuxPaneID, &s.Generation,
		&s.IdentityMarker, &s.PanePID, &s.ProcessGroupID, &s.StartTicks,
		&s.ExpectedExecutable, &s.NativeSessionID, &s.Status, &s.ExitCode,
		&s.ExitSignal, &exitedAt, &createdAt, &updatedAt, &removedAt,
		&s.ResumeAttempts, &resumeNextAt, &seriesStartedAt, &probationUntil,
		&stopRequestedAt, &lastOutputAt,
		&captureLostAt, &s.BootEpoch, &s.Color, &s.ControllerRevision,
		&s.AutomaticRecoveries, &recoverySeriesAt,
		&generationStartedAt, &s.GenerationResumed,
		&s.NativeStoreRoot, &s.WorkspaceStopGeneration)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("session: scan: %w", err)
	}
	if s.CreatedAt, err = time.Parse(db.TimeFormat, createdAt); err != nil {
		return nil, fmt.Errorf("session %s: created_at: %w", s.ID, err)
	}
	if s.UpdatedAt, err = time.Parse(db.TimeFormat, updatedAt); err != nil {
		return nil, fmt.Errorf("session %s: updated_at: %w", s.ID, err)
	}
	for _, col := range []struct {
		name string
		raw  *string
		dst  **time.Time
	}{
		{"exited_at", exitedAt, &s.ExitedAt},
		{"removed_at", removedAt, &s.RemovedAt},
		{"resume_next_at", resumeNextAt, &s.ResumeNextAt},
		{"resume_series_started_at", seriesStartedAt, &s.ResumeSeriesStartedAt},
		{"resume_probation_until", probationUntil, &s.ResumeProbationUntil},
		{"stop_requested_at", stopRequestedAt, &s.StopRequestedAt},
		{"generation_started_at", generationStartedAt, &s.GenerationStartedAt},
		{"last_output_at", lastOutputAt, &s.LastOutputAt},
		{"identity_capture_lost_at", captureLostAt, &s.IdentityCaptureLostAt},
	} {
		if col.raw == nil {
			continue
		}
		t, err := time.Parse(db.TimeFormat, *col.raw)
		if err != nil {
			return nil, fmt.Errorf("session %s: %s: %w", s.ID, col.name, err)
		}
		*col.dst = &t
	}
	return &s, nil
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func derefInt(i *int) int {
	if i == nil {
		return 0
	}
	return *i
}

func derefInt64(i *int64) int64 {
	if i == nil {
		return 0
	}
	return *i
}

// RecordOutput stamps when tmux observed PTY activity. It writes ONLY that
// column: this is presentation, so it must not touch updated_at (a
// reconciliation-state stamp), bump a generation, or append an audit
// event. A REMOVED row is left alone: its pane and scrollback were
// destroyed, so a fresh stamp would outlive what it describes.
//
// The caller writes only when the tmux stamp moved forward.
func (r *Repository) RecordOutput(ctx context.Context, id string, at time.Time) error {
	// WithTx is the only write path. This carries no audit
	// event because it is not a state transition — it records when the
	// provider last wrote, which the audit log has no claim to make.
	return r.db.WithTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `
			UPDATE sessions SET last_output_at = ?
			WHERE id = ? AND removed_at IS NULL`,
			db.FormatTime(at), id); err != nil {
			return fmt.Errorf("session %s: record output: %w", id, err)
		}
		return nil
	})
}
