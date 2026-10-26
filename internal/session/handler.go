package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/rs/zerolog"

	"github.com/noisefloor-oss/deck/internal/project"
	"github.com/noisefloor-oss/deck/internal/provider"
)

// Handler exposes the user-owned session lifecycle over HTTP.
type Handler struct {
	manager  *Manager
	sessions *Repository
	projects *project.Repository
	log      zerolog.Logger
}

// NewHandler wires the lifecycle endpoints.
func NewHandler(m *Manager, sessions *Repository, projects *project.Repository, log zerolog.Logger) *Handler {
	return &Handler{manager: m, sessions: sessions, projects: projects, log: log}
}

// Mount registers the routes. handle and wrap are the server's registrar
// and the auth middleware, exactly as the terminal handler takes them.
func (h *Handler) Mount(handle func(pattern string, hd http.Handler), wrap func(http.Handler) http.Handler) {
	handle("POST /api/sessions/{id}/attention/seen", wrap(http.HandlerFunc(h.AcknowledgeAttention)))
	handle("GET /api/projects/{id}/sessions", wrap(http.HandlerFunc(h.List)))
	handle("GET /api/session-activity/events", wrap(http.HandlerFunc(h.ActivityEvents)))
	handle("GET /api/harness-usage", wrap(http.HandlerFunc(h.Usage)))
	handle("POST /api/projects/{id}/sessions", wrap(http.HandlerFunc(h.Create)))
	handle("GET /api/sessions/{id}", wrap(http.HandlerFunc(h.Get)))
	handle("POST /api/sessions/{id}/rename", wrap(http.HandlerFunc(h.Rename)))
	handle("POST /api/sessions/{id}/color", wrap(http.HandlerFunc(h.SetColor)))
	handle("POST /api/sessions/{id}/stop", wrap(http.HandlerFunc(h.Stop)))
	handle("POST /api/sessions/{id}/recover", wrap(http.HandlerFunc(h.Recover)))
	handle("GET /api/sessions/{id}/screen", wrap(http.HandlerFunc(h.Screen)))
	handle("DELETE /api/sessions/{id}", wrap(http.HandlerFunc(h.Remove)))
}

const activityEventPoll = 250 * time.Millisecond

// ActivityEvents publishes only tmux-owned timestamps and session identity.
// It never attaches to a pane, acquires a controller lease, or receives PTY
// bytes. The first snapshot establishes a baseline so reconnect/restart cannot
// replay old activity as a fresh spinner.
func (h *Handler) ActivityEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		h.err(w, http.StatusInternalServerError, "streaming_unavailable", "activity stream unavailable")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(": connected\n\n"))
	flusher.Flush()

	last := make(map[string]time.Time)
	baseline, err := h.manager.ObservedActivity(r.Context())
	if err != nil {
		return
	}
	for _, activity := range baseline {
		last[activity.SessionID] = activity.At
	}
	ticker := time.NewTicker(activityEventPoll)
	defer ticker.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
			activities, err := h.manager.ObservedActivity(r.Context())
			if err != nil {
				return
			}
			live := make(map[string]struct{}, len(activities))
			for _, activity := range activities {
				live[activity.SessionID] = struct{}{}
				if previous, exists := last[activity.SessionID]; exists && !activity.At.After(previous) {
					continue
				}
				last[activity.SessionID] = activity.At
				payload, err := json.Marshal(activity)
				if err != nil {
					return
				}
				if _, err := w.Write([]byte("event: output\ndata: " + string(payload) + "\n\n")); err != nil {
					return
				}
				flusher.Flush()
			}
			for id := range last {
				if _, exists := live[id]; !exists {
					delete(last, id)
				}
			}
		}
	}
}

// List returns a project's sessions, newest first, all statuses.
func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	projectID := r.PathValue("id")
	if _, err := h.projects.Get(r.Context(), projectID); err != nil {
		if errors.Is(err, project.ErrNotFound) {
			h.err(w, http.StatusNotFound, "not_found", "project not found")
			return
		}
		h.log.Error().Err(err).Msg("project read failed")
		h.err(w, http.StatusInternalServerError, "internal", "could not read project")
		return
	}
	sessions, err := h.sessions.ListByProject(r.Context(), projectID)
	if err != nil {
		h.err(w, http.StatusInternalServerError, "internal", "could not list sessions")
		return
	}
	attention, err := h.sessions.ConversationAttention(r.Context(), projectID)
	if err != nil {
		h.err(w, http.StatusInternalServerError, "internal", "could not read conversation attention")
		return
	}
	// The rows are reconciled against the panes before presentation.
	// Observation uncertainty is projected explicitly instead of turning a
	// passive list into a dead-end error.
	for i, s := range sessions {
		sessions[i], err = h.manager.Present(r.Context(), s)
		if err != nil {
			h.log.Error().Err(err).Msg("could not reconcile session state")
			h.err(w, http.StatusInternalServerError, "internal", "could not reconcile session state")
			return
		}
	}
	attentionByID := make(map[string]ConversationAttention, len(attention))
	for _, fact := range attention {
		attentionByID[fact.SessionID] = fact
	}
	for _, s := range sessions {
		fact, ok := attentionByID[s.ID]
		if !ok || fact.Generation != s.Generation || s.IdentityMarker == nil || s.NativeSessionID == nil || conversationKey(*s.IdentityMarker, *s.NativeSessionID) != fact.ContextKey {
			continue
		}
		if s.PresentationState != PresentationRunning {
			fact.State = "unknown"
		}
		s.ConversationAttention = &fact
	}

	h.json(w, http.StatusOK, sessions)
}

// Usage reports how often each harness has been launched and when it
// was last used, for the whole installation and — with ?project=<id> — for
// one project, so a picker can order harnesses by what the user actually
// launches instead of by a static list. One request answers both halves
// because a picker ranks on both.
//
// Only harnesses this image can launch appear: history from an image variant
// that shipped a harness this one does not have would offer the user a
// row they cannot use. A scope with no history is an empty list, and an
// unknown project answers with that same empty list rather than a refusal,
// so it is indistinguishable from a project with no surviving launchable
// history.
func (h *Handler) Usage(w http.ResponseWriter, r *http.Request) {
	global, err := h.sessions.UsageByProvider(r.Context(), "")
	if err != nil {
		h.log.Error().Err(err).Msg("harness usage read failed")
		h.err(w, http.StatusInternalServerError, "internal", "could not read harness usage")
		return
	}
	scoped := []HarnessUsage{}
	if projectID := r.URL.Query().Get("project"); projectID != "" {
		usage, err := h.sessions.UsageByProvider(r.Context(), projectID)
		if err != nil {
			h.log.Error().Err(err).Msg("harness usage read failed")
			h.err(w, http.StatusInternalServerError, "internal", "could not read harness usage")
			return
		}
		scoped = h.launchable(usage)
	}
	h.json(w, http.StatusOK, struct {
		Global  []HarnessUsage `json:"global"`
		Project []HarnessUsage `json:"project"`
	}{Global: h.launchable(global), Project: scoped})
}

// launchable drops history for harnesses this image cannot launch, against
// the same registry that admits a create request.
func (h *Handler) launchable(usage []HarnessUsage) []HarnessUsage {
	out := []HarnessUsage{}
	for _, one := range usage {
		if _, ok := h.manager.providers[provider.Name(one.Provider)]; !ok {
			continue
		}
		out = append(out, one)
	}
	return out
}

// Create launches a provider CLI in a project (only documented
// providers). The project must be active and the provider launchable
// today — a valid provider whose identity capture has not landed is a
// conflict with an honest reason, not a silent fallback.
func (h *Handler) Create(w http.ResponseWriter, r *http.Request) {
	projectID := r.PathValue("id")
	var req struct {
		Provider string `json:"provider"`
		// The creating client's terminal size; optional. Decoded wide and
		// bounded the way the attach endpoint bounds its dimensions — a size
		// outside the range is ignored rather than rejected, because creation
		// must not fail over a cosmetic hint, and a uint16 field would turn
		// 65536 or -1 into a 400 before the bounds could speak.
		Cols int64 `json:"cols,omitempty"`
		Rows int64 `json:"rows,omitempty"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10)).Decode(&req); err != nil {
		h.err(w, http.StatusBadRequest, "bad_request", "unreadable create request")
		return
	}
	// Admission is the registry itself. A hand-written list of every provider
	// the project has ever supported would admit, on an image that ships two
	// harnesses, fourteen requests that cannot be launched — and the message
	// would name fourteen harnesses the user does not have. The names in
	// the refusal are the ones this image actually offers.
	if _, ok := h.manager.providers[provider.Name(req.Provider)]; !ok {
		h.err(w, http.StatusBadRequest, "unsupported_provider",
			"provider must be one of "+strings.Join(provider.AvailableNames(h.manager.providers), ", "))
		return
	}
	p, err := h.projects.Get(r.Context(), projectID)
	if err != nil {
		if errors.Is(err, project.ErrNotFound) {
			h.err(w, http.StatusNotFound, "not_found", "project not found")
			return
		}
		h.log.Error().Err(err).Msg("project read failed")
		h.err(w, http.StatusInternalServerError, "internal", "could not read project")
		return
	}
	if p.GitAuthorName == "" || p.GitAuthorEmail == "" ||
		p.GitIdentityProfileID == nil || p.GitIdentityProfileRevision == nil ||
		p.GitIdentitySetupRevision < 1 {
		h.err(w, http.StatusConflict, "git_identity_required", project.ErrGitIdentityRequired.Error())
		return
	}
	var cols, rows uint16
	if req.Cols >= 20 && req.Cols <= 500 && req.Rows >= 5 && req.Rows <= 300 {
		// #nosec G115 -- both values are range-checked to 20..500 x 5..300
		// on the line above; the narrowing cannot overflow.
		cols, rows = uint16(req.Cols), uint16(req.Rows)
	}
	s, err := h.manager.Create(r.Context(), CreateSpec{
		ProjectID: projectID,
		Provider:  provider.Name(req.Provider),
		WorkDir:   p.CanonicalPath,
		Cols:      cols,
		Rows:      rows,
	})
	if err != nil {
		switch {
		case errors.Is(err, ErrProviderNotLaunchable), errors.Is(err, provider.ErrHookNotProvisioned):
			h.err(w, http.StatusConflict, "provider_not_launchable", err.Error())
		case errors.Is(err, ErrProjectClosing):
			h.err(w, http.StatusConflict, "project_closing", "This project's task lane is being closed, so it cannot start new sessions.")
		default:
			h.log.Error().Err(err).Str("project", projectID).Msg("session create failed")
			h.err(w, http.StatusInternalServerError, "internal", "could not create session")
		}
		return
	}
	s, err = h.manager.Present(r.Context(), s)
	if err != nil {
		h.log.Error().Err(err).Str("session", s.ID).Msg("session presentation failed")
		h.err(w, http.StatusInternalServerError, "internal", "could not read session")
		return
	}
	h.json(w, http.StatusCreated, s)
}

// Get returns one session, reconciled against its pane first:
// the status the caller reads is derived, never the raw row. Read paths
// are honest and nonblocking — observation uncertainty surfaces as
// interrupted metadata, never a dead-end error.
func (h *Handler) Get(w http.ResponseWriter, r *http.Request) {
	s, err := h.sessions.Get(r.Context(), r.PathValue("id"))
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			h.err(w, http.StatusNotFound, "not_found", "session not found")
			return
		}
		h.log.Error().Err(err).Msg("session read failed")
		h.err(w, http.StatusInternalServerError, "internal", "could not read session")
		return
	}
	s, err = h.manager.Present(r.Context(), s)
	if err != nil {
		h.log.Error().Err(err).Msg("session read failed")
		h.err(w, http.StatusInternalServerError, "internal", "could not read session")
		return
	}
	h.json(w, http.StatusOK, s)
}

// Rename records the user's own label for a session. An
// empty name clears the label; the row then renders its provider again.
// The name is display metadata only — never identity.
func (h *Handler) Rename(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10)).Decode(&req); err != nil {
		h.err(w, http.StatusBadRequest, "bad_request", "unreadable rename request")
		return
	}
	name := strings.TrimSpace(req.Name)
	if len(name) > 100 {
		h.err(w, http.StatusBadRequest, "bad_request", "display name over 100 characters")
		return
	}
	if err := h.manager.Rename(r.Context(), id, name); err != nil {
		switch {
		case errors.Is(err, ErrNotFound):
			h.err(w, http.StatusNotFound, "not_found", "session not found")
		default:
			h.err(w, http.StatusInternalServerError, "internal", err.Error())
		}
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// Remove retires a row at the user's request. An exact verified current
// controller is stopped first; absent, foreign, historical, or unverifiable
// runtime is left untouched and removal is record-only.

// SetColor sets the user-assigned color tag for a session.
func (h *Handler) SetColor(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req struct {
		Color *string `json:"color"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10)).Decode(&req); err != nil {
		h.err(w, http.StatusBadRequest, "bad_request", "unreadable color request")
		return
	}
	// Validate against the tag palette the sidebar's swatch strip actually
	// sends (SESSION_COLOR_TAGS in web/src/components/sidebar.ts): the
	// eleven mark hues plus the four neutrals. The legacy family keys
	// (red, amber, green, blue, magenta, gray) stay accepted so a client
	// mid-upgrade and rows already stored keep working; the web resolves
	// them to their canonical keys. A strip key missing from this map
	// answers 400 on every pick of that swatch, so the two lists move
	// together.
	if req.Color != nil {
		allowed := map[string]bool{
			"crimson": true, "vermilion": true, "gold": true, "lime": true,
			"emerald": true, "teal": true, "cerulean": true, "sapphire": true,
			"indigo": true, "violet": true, "orchid": true, "gray": true,
			"pearl": true, "silver": true, "graphite": true, "ink": true,
			"red": true, "amber": true, "green": true, "blue": true, "magenta": true,
		}
		c := strings.TrimSpace(*req.Color)
		if c == "" {
			req.Color = nil
		} else if !allowed[c] {
			h.err(w, http.StatusBadRequest, "bad_request", "unknown color: "+c)
			return
		} else {
			req.Color = &c
		}
	}
	if err := h.sessions.SetColor(r.Context(), id, req.Color); err != nil {
		switch {
		case errors.Is(err, ErrNotFound):
			h.err(w, http.StatusNotFound, "not_found", "session not found")
		default:
			h.err(w, http.StatusInternalServerError, "internal", err.Error())
		}
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
func (h *Handler) Remove(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	err := h.manager.Remove(r.Context(), id)
	if err != nil {
		switch {
		case errors.Is(err, ErrNotFound):
			h.err(w, http.StatusNotFound, "not_found", "session not found")
		default:
			h.log.Error().Err(err).Str("session", id).Msg("session removal failed")
			h.err(w, http.StatusInternalServerError, "internal", "could not remove session")
		}
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// Recover revives a dead session in the pane it already occupies. It is the
// user's answer to an automatic attempt that did not succeed: no new
// session, no new card, the same row carrying its previous output. The
// response's outcome field names what committed — resumed, or relaunched
// for a row that carried no identity to resume — so the browser never
// presents a relaunch as a survival. A resume the provider does not continue
// is refused here, never replaced by a fresh conversation: that is the
// card's separate Start fresh session choice.
//
// A refusal answers with a SPECIFIC sentence and its joined cause in the log:
// a silent generic 409 is a dead button in front of the user and a blank
// line in front of whoever debugs it.
func (h *Handler) Recover(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	session, outcome, err := h.manager.Recover(r.Context(), id)
	if err != nil {
		switch {
		case errors.Is(err, ErrNotFound):
			h.err(w, http.StatusNotFound, "not_found", "session not found")
		case errors.Is(err, ErrRecoveryInProgress):
			h.log.Warn().Err(err).Str("session", id).Msg("session recovery refused")
			h.err(w, http.StatusConflict, "recovery_in_progress",
				"a recovery attempt for this session is already running")
		case errors.Is(err, ErrProjectClosing):
			h.err(w, http.StatusConflict, "project_closing", "This project's task lane is being closed, so its sessions cannot be recovered.")
		case errors.Is(err, ErrResumeRefusedByProvider):
			// The provider did not continue the recorded conversation. The
			// record is kept and the sentence says so; the joined cause
			// belongs in the log.
			h.log.Warn().Err(err).Str("session", id).Msg("session recovery refused")
			h.err(w, http.StatusConflict, "resume_refused", recoverRefusalSentence(err))
		case errors.Is(err, ErrNotResumable):
			h.log.Warn().Err(err).Str("session", id).Msg("session recovery refused")
			h.err(w, http.StatusConflict, "not_resumable", recoverRefusalSentence(err))
		default:
			h.log.Error().Err(err).Str("session", id).Msg("session recovery failed")
			h.err(w, http.StatusInternalServerError, "internal", "could not recover session")
		}
		return
	}
	h.json(w, http.StatusOK, struct {
		*Session
		// Outcome names what this request actually did: "resumed" (the
		// recorded conversation continued) or "relaunched" (a fresh
		// conversation in the same session record).
		Outcome RecoveryOutcome `json:"outcome"`
	}{session, outcome})
}

// Screen serves the last screen of a terminal session's dead pane as plain
// text, so the recovery card can show the provider's own words about why it
// stopped — the only explanation Deck has, since it never interprets them.
// The bytes are transported, never read: nothing here or in the browser
// derives state from them. A live pane is served by the terminal attachment,
// never here, and a pane that does not verify as this row's is not served at
// all.
func (h *Handler) Screen(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s, err := h.sessions.Get(r.Context(), id)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			h.err(w, http.StatusNotFound, "not_found", "session not found")
			return
		}
		h.log.Error().Err(err).Msg("session read failed")
		h.err(w, http.StatusInternalServerError, "internal", "could not read session")
		return
	}
	screen, err := h.manager.DeadPaneScreen(r.Context(), s)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			h.err(w, http.StatusNotFound, "no_screen",
				"this session's last screen is not retained")
			return
		}
		h.log.Error().Err(err).Str("session", id).Msg("session screen read failed")
		h.err(w, http.StatusInternalServerError, "internal", "could not read the session's screen")
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(screen))
}

// recoverRefusalSentence maps a refused recovery's error class to the exact
// sentence the card shows. Every sentence states a fact the user can act
// on; none of them repeats provider output.
func recoverRefusalSentence(err error) string {
	var storeGone *ConversationStoreGoneError
	if errors.As(err, &storeGone) {
		return fmt.Sprintf("the store this conversation lived in (%s) is gone; its record is kept, so restore the store and resume, or start a fresh session", storeGone.Path)
	}
	switch recoveryErrorClass(err) {
	case "hook_not_provisioned":
		return "the provider's identity hook is not provisioned; run provisioning, then resume"
	case "provider_unavailable", "provider_not_launchable":
		return "the provider CLI is not installed or not runnable on this host"
	case "provider_refused_identity":
		return "the provider did not continue the recorded conversation; its record and output are kept, so resume again once the cause shown in the pane is fixed, or start a fresh session"
	case "deadline":
		return "recovery timed out before the provider could be observed"
	default:
		return "this session could not be revived; its provider refused or left nothing to resume"
	}
}

// Stop signals the session's process group per its provider's contract.
// A force stop is destructive enough to require explicit
// confirmation; the observed exit is recorded, never assumed.
func (h *Handler) Stop(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req struct {
		Force     bool `json:"force"`
		Confirmed bool `json:"confirmed"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10)).Decode(&req); err != nil {
		h.err(w, http.StatusBadRequest, "bad_request", "unreadable stop request")
		return
	}
	if req.Force && !req.Confirmed {
		h.err(w, http.StatusBadRequest, "confirmation_required",
			"a force stop requires confirming you inspected the session first")
		return
	}
	err := h.manager.Stop(r.Context(), id, req.Force)
	if err != nil {
		switch {
		case errors.Is(err, ErrNotFound):
			h.err(w, http.StatusNotFound, "not_found", "session not found")
		case errors.Is(err, ErrNotRunning):
			h.err(w, http.StatusConflict, "not_running", err.Error())
		case errors.Is(err, ErrIdentityMismatch):
			h.err(w, http.StatusConflict, "identity_mismatch", err.Error())
		case errors.Is(err, ErrStopUnobserved):
			// The honest 200 stands on the stop_unobserved audit record.
			// recordUnobserved joins the audit failure into the
			// error when that record could not be written — and a 200
			// without the record would claim one that does not exist.
			if errors.Is(err, errStopUnobservedAudit) {
				h.log.Error().Err(err).Str("session", id).Msg("stop_unobserved audit could not be recorded")
				h.err(w, http.StatusInternalServerError, "internal",
					"the stop was signalled but its audit record could not be written")
				return
			}
			// The signal was delivered and the audit says so; the row
			// honestly stays running. Return the state, not a verdict.
			// The stop consumed the request's deadline waiting for the
			// exit, so the read happens on a fresh bounded context — an
			// expired one would 500 the honest answer.
			rctx, rcancel := context.WithTimeout(context.WithoutCancel(r.Context()), 5*time.Second)
			defer rcancel()
			s, gerr := h.sessions.Get(rctx, id)
			if gerr != nil {
				h.err(w, http.StatusInternalServerError, "internal", "could not read session")
				return
			}
			observed, oerr := h.manager.Present(rctx, s)
			if oerr != nil {
				h.err(w, http.StatusInternalServerError, "internal", "could not reconcile session state")
				return
			}
			h.json(w, http.StatusOK, observed)
		default:
			h.log.Error().Err(err).Str("session", id).Msg("stop failed")
			h.err(w, http.StatusInternalServerError, "internal", "stop failed")
		}
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) json(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (h *Handler) err(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": code, "message": message})
}
