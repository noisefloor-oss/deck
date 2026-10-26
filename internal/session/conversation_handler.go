package session

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/noisefloor-oss/deck/internal/hookcapture"
)

func (h *Handler) AcknowledgeAttention(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Generation    int    `json:"generation"`
		ContextKey    string `json:"context_key"`
		EndedSequence int64  `json:"ended_sequence"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024))
	dec.DisallowUnknownFields()
	var extra any
	if dec.Decode(&body) != nil || dec.Decode(&extra) != io.EOF {
		h.err(w, http.StatusBadRequest, "invalid", "invalid acknowledgement")
		return
	}
	if err := h.sessions.AcknowledgeConversation(r.Context(), r.PathValue("id"), body.Generation, body.ContextKey, body.EndedSequence); err != nil {
		if errors.Is(err, hookcapture.ErrConflict) {
			h.err(w, http.StatusConflict, "changed", "conversation changed")
		} else {
			h.err(w, http.StatusInternalServerError, "internal", "acknowledgement unavailable")
		}
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
