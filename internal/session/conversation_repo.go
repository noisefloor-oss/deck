package session

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"strconv"

	"github.com/noisefloor-oss/deck/internal/hookcapture"
)

// ConversationAttention records a native loop boundary, not task success.
type ConversationAttention struct {
	SessionID     string `json:"session_id"`
	Generation    int    `json:"generation"`
	ContextKey    string `json:"context_key"`
	State         string `json:"state"`
	Sequence      int64  `json:"sequence"`
	EndedSequence int64  `json:"ended_sequence"`
	SeenSequence  int64  `json:"seen_sequence"`
}

func conversationKey(marker, native string) string {
	sum := sha256.Sum256([]byte(marker + "\x00" + native))
	return hex.EncodeToString(sum[:])
}

func (r *Repository) AcceptConversationEvent(ctx context.Context, tokenHash string, event hookcapture.ConversationEvent) error {
	if !event.Valid() {
		return hookcapture.ErrConflict
	}
	return r.db.WithTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		var id, marker, native, provider, status, grantState string
		var generation, pid int
		err := tx.QueryRowContext(ctx, `SELECT s.id,s.generation,s.identity_marker,COALESCE(s.native_session_id,''),s.provider,s.status,COALESCE(s.pane_pid,0),g.state
   FROM provider_identity_capture_grants g JOIN sessions s ON s.id=g.session_id
   WHERE g.token_sha256=? AND g.provider=s.provider AND s.removed_at IS NULL AND g.generation=s.generation AND g.identity_marker=s.identity_marker`, tokenHash).Scan(&id, &generation, &marker, &native, &provider, &status, &pid, &grantState)
		if errors.Is(err, sql.ErrNoRows) {
			return hookcapture.ErrUnknownToken
		}
		if err != nil {
			return err
		}
		if provider != "pi" || status != string(StatusRunning) || grantState != "consumed" || native != event.NativeID || event.PID != strconv.Itoa(pid) {
			return hookcapture.ErrConflict
		}
		var previous int64
		var oldGen int
		var oldMarker, oldNative, lastEnded string
		var ended, seen int64
		err = tx.QueryRowContext(ctx, `SELECT generation,identity_marker,native_id,sequence,last_ended_turn,ended_sequence,seen_sequence FROM conversation_attention WHERE session_id=?`, id).Scan(&oldGen, &oldMarker, &oldNative, &previous, &lastEnded, &ended, &seen)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if oldGen != generation || oldMarker != marker || oldNative != native {
			previous = 0
			ended = 0
			seen = 0
			lastEnded = ""
		}
		completionAdvanced := event.EndedSequence > ended && event.EndedTurn != lastEnded
		if completionAdvanced {
			ended = event.EndedSequence
			lastEnded = event.EndedTurn
		}
		if event.Sequence <= previous {
			// A pre-reload delivery can carry an unseen boundary even after a
			// newer stream has reported its current state. Merge the boundary
			// without rolling that newer state back.
			if completionAdvanced {
				_, err = tx.ExecContext(ctx, `UPDATE conversation_attention SET ended_sequence=?,last_ended_turn=? WHERE session_id=?`, ended, lastEnded, id)
			}
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO conversation_attention(session_id,generation,identity_marker,native_id,sequence,state,turn_id,last_ended_turn,ended_sequence,seen_sequence)
   VALUES(?,?,?,?,?,?,?,?,?,?) ON CONFLICT(session_id) DO UPDATE SET generation=excluded.generation,identity_marker=excluded.identity_marker,native_id=excluded.native_id,
   sequence=excluded.sequence,state=excluded.state,turn_id=excluded.turn_id,last_ended_turn=excluded.last_ended_turn,ended_sequence=excluded.ended_sequence,seen_sequence=excluded.seen_sequence`, id, generation, marker, native, event.Sequence, event.State, event.TurnID, lastEnded, ended, seen)
		return err
	})
}

func (r *Repository) ConversationAttention(ctx context.Context, projectID string) ([]ConversationAttention, error) {
	rows, err := r.db.Reader().QueryContext(ctx, `SELECT a.session_id,a.generation,a.identity_marker,a.native_id,a.state,a.sequence,a.ended_sequence,a.seen_sequence
  FROM conversation_attention a JOIN sessions s ON s.id=a.session_id AND s.generation=a.generation AND s.identity_marker=a.identity_marker AND s.native_session_id=a.native_id WHERE s.removed_at IS NULL AND (?='' OR s.project_id=?)`, projectID, projectID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	result := []ConversationAttention{}
	for rows.Next() {
		var a ConversationAttention
		var marker, native string
		if err := rows.Scan(&a.SessionID, &a.Generation, &marker, &native, &a.State, &a.Sequence, &a.EndedSequence, &a.SeenSequence); err != nil {
			return nil, err
		}
		a.ContextKey = conversationKey(marker, native)
		result = append(result, a)
	}
	return result, rows.Err()
}

func (r *Repository) AcknowledgeConversation(ctx context.Context, id string, generation int, key string, ended int64) error {
	if ended <= 0 || ended > 9007199254740991 {
		return hookcapture.ErrConflict
	}
	return r.db.WithTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		var marker, native string
		var currentEnd int64
		err := tx.QueryRowContext(ctx, `SELECT a.identity_marker,a.native_id,a.ended_sequence FROM conversation_attention a JOIN sessions s ON s.id=a.session_id AND s.generation=a.generation AND s.identity_marker=a.identity_marker AND s.native_session_id=a.native_id WHERE a.session_id=? AND a.generation=? AND s.removed_at IS NULL`, id, generation).Scan(&marker, &native, &currentEnd)
		if errors.Is(err, sql.ErrNoRows) {
			return hookcapture.ErrConflict
		}
		if err != nil {
			return err
		}
		if key != conversationKey(marker, native) || ended > currentEnd {
			return hookcapture.ErrConflict
		}
		_, err = tx.ExecContext(ctx, `UPDATE conversation_attention SET seen_sequence=MAX(seen_sequence,?) WHERE session_id=?`, ended, id)
		return err
	})
}
