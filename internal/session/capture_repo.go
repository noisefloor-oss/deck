package session

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/noisefloor-oss/deck/internal/db"
	"github.com/noisefloor-oss/deck/internal/hookcapture"
	providerpkg "github.com/noisefloor-oss/deck/internal/provider"
)

const (
	capturePending  = string(hookcapture.GrantPending)
	captureConsumed = string(hookcapture.GrantConsumed)
	captureExpired  = string(hookcapture.GrantExpired)
	captureRevoked  = string(hookcapture.GrantRevoked)
)

// CreateIdentityCapture persists the bearer hash before tmux can spawn the
// provider. The exact row/generation/marker check prevents a stale launcher
// from minting authority for a successor.
func (r *Repository) CreateIdentityCapture(ctx context.Context, grant hookcapture.Grant) error {
	if !validIdentityCaptureGrant(grant) || grant.ExpectedSource != hookcapture.ExpectedSourceStartup ||
		grant.ExpectedNativeIDHash != "" {
		return ErrIdentityMismatch
	}
	now := time.Now()
	return r.db.WithTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		var one int
		// The target row either has no identity yet (retrieved contract) or
		// still carries the assigned birth identity — its own id
		// (assigned-then-tracked contract). A row already holding a foreign
		// native identity is past capture, and a grant minted for it would be
		// authority for nothing.
		if err := tx.QueryRowContext(ctx, `
			SELECT 1 FROM sessions
			WHERE id = ? AND provider = ? AND removed_at IS NULL
			  AND status = ? AND generation = ? AND identity_marker = ?
			  AND (native_session_id IS NULL OR native_session_id = id)`,
			grant.SessionID, grant.Provider, StatusStarting, grant.Generation,
			grant.IdentityMarker).Scan(&one); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrIdentityMismatch
			}
			return fmt.Errorf("session %s: validate identity capture target: %w", grant.SessionID, err)
		}
		return insertIdentityCaptureTx(ctx, tx, grant, now)
	})
}

func validIdentityCaptureGrant(grant hookcapture.Grant) bool {
	if len(grant.TokenHash) != 64 || grant.SessionID == "" || grant.Provider == "" ||
		grant.Generation < 1 || grant.IdentityMarker == "" ||
		grant.CanonicalWorkDir == "" || grant.ExpiresAt.IsZero() {
		return false
	}
	return (grant.ExpectedSource == hookcapture.ExpectedSourceStartup &&
		grant.ExpectedNativeIDHash == "") ||
		(grant.ExpectedSource == hookcapture.ExpectedSourceResume &&
			len(grant.ExpectedNativeIDHash) == 64)
}

// insertIdentityCaptureTx is shared with successor admission so the resume
// grant and its only provisional candidate appear in one commit.
func insertIdentityCaptureTx(ctx context.Context, tx *sql.Tx, grant hookcapture.Grant,
	now time.Time) error {
	if !validIdentityCaptureGrant(grant) {
		return ErrIdentityMismatch
	}
	_, err := tx.ExecContext(ctx, `
		INSERT INTO provider_identity_capture_grants (
			token_sha256, session_id, provider, generation, identity_marker,
			canonical_work_dir, expected_source, expected_native_id_sha256,
			expires_at, state, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, NULLIF(?, ''), ?, ?, ?, ?)`,
		grant.TokenHash, grant.SessionID, grant.Provider, grant.Generation,
		grant.IdentityMarker, grant.CanonicalWorkDir, grant.ExpectedSource,
		grant.ExpectedNativeIDHash, db.FormatTime(grant.ExpiresAt), capturePending,
		db.FormatTime(now), db.FormatTime(now))
	if err != nil {
		return fmt.Errorf("session %s: persist identity capture grant: %w", grant.SessionID, err)
	}
	return nil
}

// AcceptIdentityCapture records what the pane reported, audits it, and
// consumes or refreshes the grant in one writer transaction.
//
// One rule: any well-formed event arriving on a grant fenced to the live
// generation records that generation's current native session id. The pane
// holding the credential is the authority on which conversation it is
// running, and Deck does not classify how it got there. A response lost
// after commit can replay the same canonical event without another row or
// audit mutation.
func (r *Repository) AcceptIdentityCapture(ctx context.Context, tokenHash string,
	ev hookcapture.Event, now time.Time) error {
	var outcome error
	err := r.db.WithTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		var (
			sessionID, provider, marker, workDir, expiresRaw, state string
			generation                                              int
			acceptedHash, expectedNativeIDHash                      *string
			acceptedSequence                                        int64
		)
		err := tx.QueryRowContext(ctx, `
			SELECT session_id, provider, generation, identity_marker,
				canonical_work_dir, expected_native_id_sha256,
				expires_at, state, accepted_event_sha256, accepted_sequence
			FROM provider_identity_capture_grants WHERE token_sha256 = ?`,
			tokenHash).Scan(&sessionID, &provider, &generation, &marker,
			&workDir, &expectedNativeIDHash, &expiresRaw,
			&state, &acceptedHash, &acceptedSequence)
		if errors.Is(err, sql.ErrNoRows) {
			outcome = hookcapture.ErrUnknownToken
			return nil
		}
		if err != nil {
			return fmt.Errorf("identity capture: read grant: %w", err)
		}
		expiresAt, err := time.Parse(db.TimeFormat, expiresRaw)
		if err != nil {
			return fmt.Errorf("identity capture: parse expiry: %w", err)
		}
		if state != capturePending && state != captureConsumed {
			outcome = hookcapture.ErrUnknownToken
			return nil
		}
		// One rule, enforced wherever the window is read: a pending grant
		// is acceptable while its exact generation is the live running row,
		// and only otherwise until its absolute expiry. Sparing the grant
		// from the sweep while refusing the delivery it was spared for
		// would move the loss record to the first submission and change
		// nothing the user can see.
		if state == capturePending && !now.Before(expiresAt) {
			live, liveErr := generationIsRunningTx(ctx, tx, sessionID, generation, marker)
			if liveErr != nil {
				return liveErr
			}
			if !live {
				if err := expireIdentityCaptureTx(ctx, tx, sessionID, generation, marker, now,
					"provider identity capture expired before delivery"); err != nil {
					return err
				}
				outcome = hookcapture.ErrUnknownToken
				return nil
			}
		}
		expectedHash := ""
		if expectedNativeIDHash != nil {
			expectedHash = *expectedNativeIDHash
		}
		if validationErr := hookcapture.ValidateEvent(provider, workDir, ev); validationErr != nil {
			if err := db.AppendAudit(ctx, tx, db.AuditEvent{
				SessionID: &sessionID, Type: AuditIdentityCaptureRefused,
				Details: fmt.Sprintf("provider=%s generation=%d reason=%s native_id_sha256=%s",
					provider, generation, hookcapture.ConflictReason(validationErr),
					hookcapture.NativeIDHash(ev.SessionID)),
			}); err != nil {
				return err
			}
			outcome = hookcapture.ErrConflict
			return nil
		}

		// A hook that names its reporting process is also believed about
		// WHICH pane it speaks for: the conversation id must come from the
		// process this generation recorded as its own. omp subagents
		// inherit the pane's capture token and fire their own session_start
		// with a fresh conversation id, and rolling the parent row to a
		// child's id left it naming a conversation that never persisted —
		// the next resume was refused for an id the provider never saw.
		// A generation whose owning process is not (yet) recorded is not
		// judged here: a recovery in flight pins its candidate's pid the
		// moment the pane spawns, and before that there is no pane to
		// impersonate.
		if reportingPID := ev.PID; reportingPID != "" {
			ownerPID, ownerErr := generationOwnerPIDTx(ctx, tx, sessionID, generation, marker)
			if ownerErr != nil {
				return ownerErr
			}
			if ownerPID != nil && strconv.Itoa(*ownerPID) != reportingPID {
				if err := db.AppendAudit(ctx, tx, db.AuditEvent{
					SessionID: &sessionID, Type: AuditIdentityCaptureRefused,
					Details: fmt.Sprintf("provider=%s generation=%d reason=reporting_process native_id_sha256=%s",
						provider, generation, hookcapture.NativeIDHash(ev.SessionID)),
				}); err != nil {
					return err
				}
				outcome = hookcapture.ErrConflict
				return nil
			}
		}
		// Replay idempotence: a delivery carrying the fact already recorded on
		// this grant changes nothing, so a lost response costs nothing.
		eventHash := hookcapture.AcceptedEventHash(provider, workDir, ev)
		if acceptedHash != nil && *acceptedHash == eventHash {
			if ev.Sequence > acceptedSequence {
				if _, err := tx.ExecContext(ctx, `UPDATE provider_identity_capture_grants
					SET accepted_sequence = ?, updated_at = ?
					WHERE token_sha256 = ? AND state IN (?, ?)`,
					ev.Sequence, db.FormatTime(now), tokenHash, capturePending, captureConsumed); err != nil {
					return fmt.Errorf("session %s: advance identity capture sequence: %w", sessionID, err)
				}
			}
			return nil
		}
		if ev.Sequence > 0 && acceptedSequence > 0 && ev.Sequence <= acceptedSequence {
			return nil
		}
		// The requested conversation is a fact about the resume this grant
		// was minted for, so it can only be answered by the first report on
		// it. Later reports are the pane moving itself - a clear, a fork,
		// the user picking another conversation - and comparing those
		// against the original request would audit an honest move as a
		// provider that disobeyed.
		requested := expectedHash
		if acceptedHash != nil {
			requested = ""
		}
		return recordReportedIdentityTx(ctx, tx, tokenHash, sessionID, provider,
			generation, marker, requested, eventHash, ev, now, &outcome)
	})
	if err != nil {
		return err
	}
	return outcome
}

// generationOwnerPIDTx reads the process id this generation recorded as its
// own. A recovery in flight owns the candidate pid its slot pinned at
// spawn; before that pin the generation's owner is deliberately
// unverifiable, so a named capture in the window between admission and the
// spawn's recording is judged against nothing — the row still carries the
// previous generation's tuple, and omp's hook does not retry a 4xx, so
// refusing there would permanently lose a legitimate attestation. With no
// flight, a generation whose row is still live — starting or running —
// owns its recorded pane pid; a terminal row's tuple belongs to whichever
// generation last committed it and decides nothing. Nil means
// unverifiable, and the caller declines to judge.
func generationOwnerPIDTx(ctx context.Context, tx *sql.Tx,
	sessionID string, generation int, marker string) (*int, error) {
	var status string
	var panePID, candidatePID sql.NullInt64
	var slots int
	err := tx.QueryRowContext(ctx, `
		SELECT s.status, s.pane_pid,
			(SELECT COUNT(*) FROM session_successor_slots slot
			 WHERE slot.session_id = s.id
			   AND slot.candidate_generation = s.generation),
			(SELECT slot.candidate_pane_pid FROM session_successor_slots slot
			 WHERE slot.session_id = s.id
			   AND slot.candidate_generation = s.generation) AS candidate_pid
		FROM sessions s
		WHERE s.id = ? AND s.generation = ? AND s.identity_marker = ?`,
		sessionID, generation, marker).Scan(&status, &panePID, &slots, &candidatePID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("identity capture: read generation owner: %w", err)
	}
	if slots > 0 {
		if candidatePID.Valid && candidatePID.Int64 > 1 {
			owner := int(candidatePID.Int64)
			return &owner, nil
		}
		return nil, nil
	}
	if (status == string(StatusRunning) || status == string(StatusStarting)) && panePID.Valid && panePID.Int64 > 1 {
		owner := int(panePID.Int64)
		return &owner, nil
	}
	return nil, nil
}

// recordReportedIdentityTx writes the conversation the pane just named onto
// the exact row the grant fences: this session, this generation, this
// identity marker, this provider. That fence is what decides WHICH row an
// event belongs to, and it is the whole of the decision — a grant minted for
// a generation the row has since moved past matches nothing and is refused,
// which is what keeps one user's many panes from writing over each other.
//
// The grant is left consumed and carrying this event's hash, so it stays the
// live generation's tracking channel for as long as that generation runs.
func recordReportedIdentityTx(ctx context.Context, tx *sql.Tx, tokenHash,
	sessionID, provider string, generation int, marker, requestedNativeIDHash,
	eventHash string, ev hookcapture.Event, now time.Time, outcome *error) error {
	var (
		current  *string
		status   string
		revision int64
	)
	err := tx.QueryRowContext(ctx, `
		SELECT native_session_id, status, controller_revision FROM sessions
		WHERE id = ? AND provider = ? AND removed_at IS NULL
		  AND generation = ? AND identity_marker = ?`,
		sessionID, provider, generation, marker).Scan(&current, &status, &revision)
	if errors.Is(err, sql.ErrNoRows) {
		return refuseIdentityCaptureTx(ctx, tx, sessionID, provider, generation,
			"stale_generation", ev, outcome)
	}
	if err != nil {
		return fmt.Errorf("session %s: read fenced identity row: %w", sessionID, err)
	}
	storedNow := db.FormatTime(now)
	moved := current == nil || *current != ev.SessionID
	if moved {
		res, err := tx.ExecContext(ctx, `UPDATE sessions
			SET native_session_id = ?, identity_capture_lost_at = NULL, updated_at = ?
			WHERE id = ? AND provider = ? AND removed_at IS NULL
			  AND generation = ? AND identity_marker = ? AND native_session_id IS ?`,
			ev.SessionID, storedNow, sessionID, provider, generation, marker, current)
		if err != nil {
			return fmt.Errorf("session %s: record reported native identity: %w", sessionID, err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n != 1 {
			// A concurrent writer moved this exact generation on while the
			// event was in flight, so there is nothing left to write. The
			// transaction commits so the outcome is the refusal, not a
			// rollback that would lose its audit.
			return refuseIdentityCaptureTx(ctx, tx, sessionID, provider, generation,
				"stale_generation", ev, outcome)
		}
		// The sessions trigger advances controller_revision for any identity
		// write, which is right for the pane the user is typing into:
		// controller work built on the conversation just replaced is stale.
		// A row still waiting for its successor commit is not the
		// controller's row yet, and advancing it there fails the commit that
		// is about to publish the pane — so its revision is put back.
		if status != string(StatusStarting) && status != string(StatusRunning) {
			if _, err := tx.ExecContext(ctx, `UPDATE sessions
				SET controller_revision = ? WHERE id = ?`, revision, sessionID); err != nil {
				return fmt.Errorf("session %s: hold controller revision: %w", sessionID, err)
			}
		}
	}
	if provider == string(providerpkg.Claude) {
		if err := recordClaudeStoreTx(ctx, tx, sessionID, generation, marker, ev, now); err != nil {
			return err
		}
	}
	res, err := tx.ExecContext(ctx, `
		UPDATE provider_identity_capture_grants
		SET state = ?, accepted_event_sha256 = ?,
			accepted_sequence = MAX(accepted_sequence, ?),
			closed_at = COALESCE(closed_at, ?), updated_at = ?
		WHERE token_sha256 = ? AND state IN (?, ?)`,
		captureConsumed, eventHash, ev.Sequence, storedNow, storedNow, tokenHash,
		capturePending, captureConsumed)
	if err != nil {
		return fmt.Errorf("session %s: consume identity capture grant: %w", sessionID, err)
	}
	if err := mustAffectOne(res, sessionID); err != nil {
		return err
	}
	auditType := AuditIdentityConfirmed
	if moved {
		auditType = AuditIdentityCaptured
	}
	return db.AppendAudit(ctx, tx, db.AuditEvent{
		SessionID: &sessionID, Type: auditType,
		Details: identityAuditDetails(provider, generation, current, ev,
			requestedNativeIDHash),
	})
}

// refuseIdentityCaptureTx records a content-free refusal and reports it to the
// caller without rolling the transaction back, so the user can read why an
// event went nowhere.
func refuseIdentityCaptureTx(ctx context.Context, tx *sql.Tx, sessionID,
	provider string, generation int, reason string, ev hookcapture.Event,
	outcome *error) error {
	if err := db.AppendAudit(ctx, tx, db.AuditEvent{
		SessionID: &sessionID, Type: AuditIdentityCaptureRefused,
		Details: fmt.Sprintf("provider=%s generation=%d reason=%s native_id_sha256=%s",
			provider, generation, reason, hookcapture.NativeIDHash(ev.SessionID)),
	}); err != nil {
		return err
	}
	*outcome = hookcapture.ErrConflict
	return nil
}

// identityAuditDetails states the change plainly: what the row held, what the
// pane reported, and the provider's own word for how it got there — recorded
// as provenance, never read as permission. Native ids stay opaque, so the
// audit names them by digest.
func identityAuditDetails(provider string, generation int, current *string,
	ev hookcapture.Event, requestedNativeIDHash string) string {
	from := "none"
	if current != nil {
		from = hookcapture.NativeIDHash(*current)
	}
	reported := hookcapture.NativeIDHash(ev.SessionID)
	details := fmt.Sprintf(
		"provider=%s generation=%d source=%s from_native_id_sha256=%s to_native_id_sha256=%s",
		provider, generation, ev.Source, from, reported)
	if requestedNativeIDHash != "" && requestedNativeIDHash != reported {
		details += " the provider resumed a different conversation than the one requested"
	}
	return details
}

// recordClaudeStoreTx persists the store root derived from the conversation's
// reported ledger file, beside the identity the event named and in the same
// transaction as the acceptance, so nothing can observe one without the other.
// The write is fenced to the exact generation, marker, and the event's own
// native id; an event without a transcript records nothing and degrades store
// pinning only. Zero matched rows is not an error here: the acceptance that
// carried this event already decided the outcome, and the store record is a
// rider on it, never a second authority.
func recordClaudeStoreTx(ctx context.Context, tx *sql.Tx, sessionID string,
	generation int, marker string, ev hookcapture.Event, now time.Time) error {
	if ev.TranscriptPath == "" {
		return nil
	}
	root, err := hookcapture.ClaudeStoreRoot(ev.TranscriptPath, ev.SessionID)
	if err != nil {
		// ValidateEvent refused malformed transcripts before acceptance, so
		// this is a caller bug, not provider data.
		return fmt.Errorf("session %s: derive claude store root: %w", sessionID, err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE sessions
		SET native_store_root = ?, updated_at = ?
		WHERE id = ? AND provider = ? AND removed_at IS NULL
		  AND generation = ? AND identity_marker = ? AND native_session_id = ?`,
		root, db.FormatTime(now), sessionID,
		providerpkg.Claude, generation, marker, ev.SessionID); err != nil {
		return fmt.Errorf("session %s: record claude store: %w", sessionID, err)
	}
	return nil
}

func (r *Repository) RevokeIdentityCapture(ctx context.Context, sessionID string,
	generation int, marker string, at time.Time) (hookcapture.GrantState, error) {
	state := hookcapture.GrantState("")
	err := r.db.WithTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		var stored string
		if err := tx.QueryRowContext(ctx, `
			SELECT state FROM provider_identity_capture_grants
			WHERE session_id = ? AND generation = ? AND identity_marker = ?`,
			sessionID, generation, marker).Scan(&stored); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return hookcapture.ErrUnknownToken
			}
			return fmt.Errorf("session %s: read identity capture for revoke: %w", sessionID, err)
		}
		state = hookcapture.GrantState(stored)
		if state != hookcapture.GrantPending {
			return nil
		}
		res, err := tx.ExecContext(ctx, `
			UPDATE provider_identity_capture_grants
			SET state = ?, closed_at = ?, updated_at = ?
			WHERE session_id = ? AND generation = ? AND identity_marker = ?
			  AND state = ?`,
			captureRevoked, db.FormatTime(at), db.FormatTime(at), sessionID,
			generation, marker, capturePending)
		if err != nil {
			return fmt.Errorf("session %s: revoke identity capture: %w", sessionID, err)
		}
		if err := mustAffectOne(res, sessionID); err != nil {
			return err
		}
		state = hookcapture.GrantRevoked
		return nil
	})
	return state, err
}

// IdentityCapturePending remains true past expires_at until the durable sweep
// commits the loss record. Recovery therefore fails closed across a transient
// database failure instead of treating an unrecorded expiry as permission.
func (r *Repository) IdentityCapturePending(ctx context.Context, sessionID string,
	generation int, marker string, _ time.Time) (bool, error) {
	var one int
	err := r.db.Reader().QueryRowContext(ctx, `
		SELECT 1 FROM provider_identity_capture_grants
		WHERE session_id = ? AND generation = ? AND identity_marker = ?
		  AND state = ?`, sessionID, generation, marker, capturePending).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("session %s: read identity capture grant: %w", sessionID, err)
	}
	return true, nil
}

// generationIsRunningTx reports whether the grant's exact generation is still
// the session's live running row - the bound a capture grant lives by.
func generationIsRunningTx(ctx context.Context, tx *sql.Tx,
	sessionID string, generation int, marker string) (bool, error) {
	var one int
	err := tx.QueryRowContext(ctx, `
		SELECT 1 FROM sessions
		WHERE id = ? AND generation = ? AND identity_marker = ?
		  AND removed_at IS NULL AND status = ?`,
		sessionID, generation, marker, StatusRunning).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("session %s: read generation liveness: %w", sessionID, err)
	}
	return true, nil
}

func (r *Repository) ExpireIdentityCaptures(ctx context.Context, at time.Time) error {
	return r.db.WithTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		// The ttl is a delivery window for a launch that died or never
		// called back - not a deadline the user can miss. A provider
		// that reports its id at the first submission reports it whenever
		// the user first types, which is legitimately hours after the
		// pane started; expiring that grant strands the session's identity
		// for the rest of a life the pane is still living, and no later
		// submission can repair it. A pending grant is therefore reclaimed
		// only once its exact generation is no longer the live running row,
		// which is the same bound the consumed carve-out below already
		// keeps: a grant lives exactly as long as the generation it
		// belongs to.
		rows, err := tx.QueryContext(ctx, `
			SELECT session_id, generation, identity_marker
			FROM provider_identity_capture_grants
			WHERE state = ? AND expires_at <= ?
			  AND NOT EXISTS (
			    SELECT 1 FROM sessions live
			    WHERE live.id = provider_identity_capture_grants.session_id
			      AND live.generation = provider_identity_capture_grants.generation
			      AND live.identity_marker = provider_identity_capture_grants.identity_marker
			      AND live.removed_at IS NULL AND live.status = ?
			  )
			ORDER BY expires_at`, capturePending, db.FormatTime(at), StatusRunning)
		if err != nil {
			return fmt.Errorf("identity capture: list due grants: %w", err)
		}
		defer func() { _ = rows.Close() }()
		type target struct {
			id, marker string
			generation int
		}
		var targets []target
		for rows.Next() {
			var item target
			if err := rows.Scan(&item.id, &item.generation, &item.marker); err != nil {
				return err
			}
			targets = append(targets, item)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		if err := rows.Close(); err != nil {
			return err
		}
		for _, item := range targets {
			if err := expireIdentityCaptureTx(ctx, tx, item.id, item.generation,
				item.marker, at, "provider identity capture expired before delivery"); err != nil {
				return err
			}
		}
		// A consumed grant whose exact generation is still the live running
		// row is that generation's tracking channel: Claude's SessionStart
		// rolls and Codex's Stop rotations post to it for the pane's whole
		// life. Deleting it at expiry would silently sever identity tracking
		// mid-session, so it is reclaimed only once the generation is no
		// longer the running one.
		_, err = tx.ExecContext(ctx, `
			DELETE FROM provider_identity_capture_grants
			WHERE state <> ? AND expires_at <= ?
			  AND NOT EXISTS (
			    SELECT 1 FROM session_successor_slots slot
			    WHERE slot.capture_token_sha256 = provider_identity_capture_grants.token_sha256
			  )
			  AND NOT (state = ? AND EXISTS (
			    SELECT 1 FROM sessions live
			    WHERE live.id = provider_identity_capture_grants.session_id
			      AND live.generation = provider_identity_capture_grants.generation
			      AND live.identity_marker = provider_identity_capture_grants.identity_marker
			      AND live.removed_at IS NULL AND live.status = ?
			  ))`, capturePending, db.FormatTime(at), captureConsumed, StatusRunning)
		return err
	})
}

// expireIdentityCaptureTx closes one exact pending generation and records the
// lost capture premise if that same generation still lacks native identity.
// It is also called by positive-death transitions in their existing tx.
func expireIdentityCaptureTx(ctx context.Context, tx *sql.Tx, sessionID string,
	generation int, marker string, at time.Time, reason string) error {
	var expectedSource string
	if err := tx.QueryRowContext(ctx, `
		SELECT expected_source FROM provider_identity_capture_grants
		WHERE session_id = ? AND generation = ? AND identity_marker = ?
		  AND state = ?`, sessionID, generation, marker, capturePending).Scan(&expectedSource); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return fmt.Errorf("session %s: read identity capture for expiry: %w", sessionID, err)
	}
	storedAt := db.FormatTime(at)
	res, err := tx.ExecContext(ctx, `
		UPDATE provider_identity_capture_grants
		SET state = ?, closed_at = ?, updated_at = ?
		WHERE session_id = ? AND generation = ? AND identity_marker = ?
		  AND state = ?`, captureExpired, storedAt, storedAt, sessionID,
		generation, marker, capturePending)
	if err != nil {
		return fmt.Errorf("session %s: expire identity capture grant: %w", sessionID, err)
	}
	n, err := res.RowsAffected()
	if err != nil || n == 0 {
		return err
	}
	if expectedSource == hookcapture.ExpectedSourceResume {
		return nil
	}
	res, err = tx.ExecContext(ctx, `
		UPDATE sessions SET identity_capture_lost_at = ?, updated_at = ?
		WHERE id = ? AND removed_at IS NULL AND generation = ?
		  AND identity_marker = ? AND native_session_id IS NULL
		  AND identity_capture_lost_at IS NULL`,
		storedAt, storedAt, sessionID, generation, marker)
	if err != nil {
		return fmt.Errorf("session %s: mark identity capture lost: %w", sessionID, err)
	}
	n, err = res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return nil
	}
	reason = strings.TrimSpace(reason)
	if reason == "" {
		reason = "provider identity capture closed before delivery"
	}
	return db.AppendAudit(ctx, tx, db.AuditEvent{
		SessionID: &sessionID, Type: AuditIdentityCaptureLost, Details: reason,
	})
}
