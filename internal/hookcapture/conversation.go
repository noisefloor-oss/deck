package hookcapture

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strconv"
)

// ConversationEvent contains only native lifecycle metadata, never prompts,
// tool arguments, dialog titles, output or transcript content.
type ConversationEvent struct {
	NativeID      string `json:"native_id"`
	PID           string `json:"pid"`
	Sequence      int64  `json:"sequence"`
	TurnID        string `json:"turn_id"`
	State         string `json:"state"`
	EndedSequence int64  `json:"ended_sequence"`
	EndedTurn     string `json:"ended_turn"`
}

var conversationTurn = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

func (e ConversationEvent) Valid() bool {
	pid, err := strconv.ParseInt(e.PID, 10, 32)
	if err != nil || pid <= 0 || !validNativeID("pi", e.NativeID) {
		return false
	}
	if e.Sequence <= 0 || e.Sequence > 9007199254740991 || !conversationTurn.MatchString(e.TurnID) || len(e.NativeID) > 128 || len(e.PID) > 20 {
		return false
	}
	if e.EndedSequence < 0 || e.EndedSequence > e.Sequence || (e.EndedSequence == 0) != (e.EndedTurn == "") || e.EndedSequence > 0 && !conversationTurn.MatchString(e.EndedTurn) {
		return false
	}
	if e.State == "ended" && (e.EndedSequence == 0 || e.EndedTurn != e.TurnID) {
		return false
	}
	switch e.State {
	case "unknown", "working", "needs_input", "ended":
		return true
	}
	return false
}

// ConversationHandler uses the existing per-launch credential, with a separate
// store operation that cannot change the generation's native identity.
func (s *Service) ConversationHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token := r.Header.Get("X-Noisedeck-Hook-Token")
		if token == "" {
			http.Error(w, "missing hook token", http.StatusBadRequest)
			return
		}
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 2048))
		dec.DisallowUnknownFields()
		var event ConversationEvent
		var extra any
		if dec.Decode(&event) != nil || dec.Decode(&extra) != io.EOF || !event.Valid() {
			http.Error(w, "invalid lifecycle event", http.StatusBadRequest)
			return
		}
		store, ok := s.store.(interface {
			AcceptConversationEvent(context.Context, string, ConversationEvent) error
		})
		if !ok {
			http.Error(w, "lifecycle events unavailable", http.StatusServiceUnavailable)
			return
		}
		err := store.AcceptConversationEvent(r.Context(), hashToken(token), event)
		switch {
		case err == nil:
			w.WriteHeader(http.StatusNoContent)
		case errors.Is(err, ErrUnknownToken):
			http.Error(w, "capture token unavailable", http.StatusGone)
		case errors.Is(err, ErrConflict):
			http.Error(w, "lifecycle event refused", http.StatusConflict)
		default:
			http.Error(w, "lifecycle event unavailable", http.StatusInternalServerError)
		}
	}
}
