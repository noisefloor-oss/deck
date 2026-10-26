// Package chats serves the console's read of the agent chat ledger.
// It runs the a2a client's ledger export for a session's agent
// and hands the result to the browser. It writes nothing to the Logstream,
// reads no terminal, and names no source session.
package chats

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog"

	"github.com/noisefloor-oss/deck/internal/session"
)

// Variables rather than constants so the tests can drive the timeout,
// the overflow limit and the heartbeat without waiting them out.
var (
	ledgerReadTimeout = 10 * time.Second
	ledgerOutputLimit = 8 << 20
	eventsPoll        = 2 * time.Second
	eventsHeartbeat   = 15 * time.Second
	statusTimeout     = 5 * time.Second
	statusTTL         = 15 * time.Second
)

// harnessNames maps Deck's provider ids to the harness names a2a derives a
// handle from (deploy/a2a/a2a.py, HARNESS_COMMS).
var harnessNames = map[string]string{
	"claude":   "claude",
	"codex":    "codex",
	"kimi":     "kimi",
	"opencode": "opencode",
	"pi":       "pi",
	"omp":      "omp",
}

// Sessions is the one read the handler needs from the session store.
type Sessions interface {
	Get(ctx context.Context, id string) (*session.Session, error)
}

// Handler serves GET /api/sessions/{id}/chats and its change stream.
type Handler struct {
	sessions   Sessions
	executable string
	log        zerolog.Logger

	statusMu        sync.Mutex
	statusAt        time.Time
	statusAvailable bool
}

func NewHandler(sessions Sessions, executable string, log zerolog.Logger) *Handler {
	return &Handler{sessions: sessions, executable: executable, log: log}
}

func (h *Handler) Mount(handle func(pattern string, hd http.Handler), wrap func(http.Handler) http.Handler) {
	handle("GET /api/sessions/{id}/chats", wrap(http.HandlerFunc(h.Ledger)))
	handle("GET /api/sessions/{id}/chats/events", wrap(http.HandlerFunc(h.Events)))
	handle("GET /api/chats", wrap(http.HandlerFunc(h.Index)))
	handle("GET /api/chats/status", wrap(http.HandlerFunc(h.Status)))
}

// Status reports whether agent chat works on this install: the a2a client
// is present and its service answers one authenticated read. The read costs
// the same however much history exists. The answer is cached briefly so a
// page that asks on every load does not start a child each time.
func (h *Handler) Status(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	h.json(w, http.StatusOK, map[string]bool{"available": h.available()})
}

func (h *Handler) available() bool {
	h.statusMu.Lock()
	defer h.statusMu.Unlock()
	if !h.statusAt.IsZero() && time.Since(h.statusAt) < statusTTL {
		return h.statusAvailable
	}
	_, err := h.runWithin(context.Background(), statusTimeout, "ledger", "--probe")
	h.statusAvailable = err == nil
	h.statusAt = time.Now()
	return h.statusAvailable
}

// Index is the content-free list of every chat on this palace: ids,
// states, members and row counts, never a body. The sidebar marks the
// sessions whose agents are in a live chat from it.
func (h *Handler) Index(w http.ResponseWriter, r *http.Request) {
	index, err := h.readIndex(r.Context())
	if err != nil {
		h.log.Error().Err(err).Msg("chat index read failed")
		h.err(w, http.StatusBadGateway, "ledger_unavailable", "the chat ledger could not be read")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(index)
}

// IndexEntry is the allowlist: the only fields of an index entry the
// console will ever serve. Anything else the child prints is dropped here,
// so the content-free boundary is mechanized, not promised.
type IndexEntry struct {
	ID      string   `json:"id"`
	State   string   `json:"state"`
	Members []string `json:"members"`
	Left    []string `json:"left"`
	Last    *string  `json:"last"`
	LastRow *string  `json:"last_row"`
	Rows    int      `json:"rows"`
}

type Index struct {
	Chats    []IndexEntry      `json:"chats"`
	Sessions map[string]string `json:"sessions"`
}

func (h *Handler) readIndex(parent context.Context) ([]byte, error) {
	raw, err := h.run(parent, "ledger", "--index", "--compact")
	if err != nil {
		return nil, err
	}
	var shape struct {
		Chats    []IndexEntry      `json:"chats"`
		Sessions map[string]string `json:"sessions"`
	}
	if err := decodeOneJSON(raw, &shape); err != nil || shape.Chats == nil {
		return nil, errors.New("chats: a2a ledger index returned an unsupported shape")
	}
	index := Index{Chats: make([]IndexEntry, 0, len(shape.Chats)), Sessions: map[string]string{}}
	for _, entry := range shape.Chats {
		if entry.ID == "" || entry.State == "" {
			return nil, errors.New("chats: a2a ledger index returned an unsupported shape")
		}
		if entry.Members == nil {
			entry.Members = []string{}
		}
		if entry.Left == nil {
			entry.Left = []string{}
		}
		index.Chats = append(index.Chats, entry)
	}
	// Sessions maps a Deck session id to its agent's effective handle, as
	// presence resolved it — the same content-free class as the member
	// handles, and empty from an a2a that predates the field, which the
	// browser reads as "derive the default".
	for sessionID, handle := range shape.Sessions {
		if sessionID == "" || handle == "" {
			return nil, errors.New("chats: a2a ledger index returned an unsupported shape")
		}
		index.Sessions[sessionID] = handle
	}
	return json.Marshal(index)
}

// Harness is the a2a harness name of a session's provider, or false for
// a provider a2a does not know (a shell has no agent and so no chats).
func Harness(s *session.Session) (string, bool) {
	harness, ok := harnessNames[s.Provider]
	if !ok || len(s.ID) < 4 {
		return "", false
	}
	return harness, true
}

// Handle is the default a2a handle of a session's agent: the harness name
// and the first four characters of the session id, exactly as the client
// derives it inside the session. The rule and the ledger export the
// console reads are frozen as the a2a consumer contract (docs/CONTRACT.md
// in the a2a repository). The effective handle can differ when the agent
// chose a name, so the ledger is read by session, and a2a resolves the
// name through the presence room (handle_for_session in a2a.py).
func Handle(s *session.Session) (string, bool) {
	harness, ok := Harness(s)
	if !ok {
		return "", false
	}
	return harness + "-" + strings.ToLower(s.ID[:4]), true
}

// Ledger is the response body: the export as a2a wrote it, plus the handle
// it was read for so the browser can tell "this agent" from the others.
type Ledger struct {
	Agent string          `json:"agent"`
	Chats json.RawMessage `json:"chats"`
}

func (h *Handler) Ledger(w http.ResponseWriter, r *http.Request) {
	s, ok := h.sessionFor(w, r)
	if !ok {
		return
	}
	ledger, err := h.read(r.Context(), s)
	if err != nil {
		h.log.Error().Err(err).Msg("chat ledger read failed")
		h.err(w, http.StatusBadGateway, "ledger_unavailable", "the chat ledger could not be read")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	h.json(w, http.StatusOK, ledger)
}

// Events streams `chats-changed` whenever the ledger for this session's
// agent differs from the last one sent, and a comment heartbeat otherwise.
// The browser re-reads the ledger on each event; the stream carries no
// chat content.
func (h *Handler) Events(w http.ResponseWriter, r *http.Request) {
	s, ok := h.sessionFor(w, r)
	if !ok {
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		h.err(w, http.StatusInternalServerError, "streaming_unavailable", "streaming is not supported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, ": connected\n\n")
	flusher.Flush()

	var last string
	var sequence uint64
	poll := time.NewTicker(eventsPoll)
	defer poll.Stop()
	heartbeat := time.NewTicker(eventsHeartbeat)
	defer heartbeat.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-heartbeat.C:
			if _, err := io.WriteString(w, ": heartbeat\n\n"); err != nil {
				return
			}
			flusher.Flush()
		case <-poll.C:
			ledger, err := h.read(r.Context(), s)
			if err != nil {
				continue // the next poll may succeed; the browser keeps what it has
			}
			digest := fingerprint(ledger.Chats)
			if digest == last {
				continue
			}
			last = digest
			sequence++
			if _, err := fmt.Fprintf(w, "event: chats-changed\ndata: {\"sequence\":%d}\n\n", sequence); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

func (h *Handler) sessionFor(w http.ResponseWriter, r *http.Request) (*session.Session, bool) {
	s, err := h.sessions.Get(r.Context(), r.PathValue("id"))
	if err != nil {
		if errors.Is(err, session.ErrNotFound) {
			h.err(w, http.StatusNotFound, "not_found", "session not found")
			return nil, false
		}
		h.log.Error().Err(err).Msg("session read failed")
		h.err(w, http.StatusInternalServerError, "internal", "could not read session")
		return nil, false
	}
	if _, ok := Harness(s); !ok {
		h.err(w, http.StatusNotFound, "no_agent_handle", "this session's harness has no chat handle")
		return nil, false
	}
	return s, true
}

// read runs `a2a ledger --for <handle> --compact` and validates the shape
// before trusting it. The child gets the server's environment minus any
// handle override, and A2A_NO_COURIER so a read never starts a courier.
func (h *Handler) run(parent context.Context, args ...string) ([]byte, error) {
	return h.runWithin(parent, ledgerReadTimeout, args...)
}

func (h *Handler) runWithin(parent context.Context, timeout time.Duration, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, h.executable, args...)
	// A killed child can leave a grandchild holding the pipe; WaitDelay
	// bounds how long the read waits for it after the timeout fires.
	cmd.WaitDelay = time.Second
	cmd.Env = append(withoutEnvironment(os.Environ(), "A2A_HANDLE"), "A2A_NO_COURIER=1")
	stdout := boundedBuffer{limit: ledgerOutputLimit}
	var stderr boundedBuffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("chats: ledger read canceled: %w", ctx.Err())
		}
		return nil, errors.New("chats: a2a ledger exited unsuccessfully")
	}
	if stdout.overflow || stderr.overflow {
		return nil, errors.New("chats: a2a ledger output exceeded limit")
	}
	return stdout.buf.Bytes(), nil
}

// read runs `a2a ledger --for-session <id> --harness <name> --compact`:
// a2a resolves the agent's effective handle for the session, chosen name
// or default, and the response names it.
func (h *Handler) read(parent context.Context, s *session.Session) (*Ledger, error) {
	harness, _ := Harness(s)
	raw, err := h.run(parent, "ledger", "--for-session", s.ID, "--harness", harness, "--compact")
	if err != nil {
		return nil, err
	}
	var shape struct {
		Agent string          `json:"agent"`
		Chats json.RawMessage `json:"chats"`
	}
	if err := decodeOneJSON(raw, &shape); err != nil {
		return nil, errors.New("chats: a2a ledger returned invalid JSON")
	}
	if shape.Agent == "" || len(shape.Chats) == 0 || shape.Chats[0] != '[' {
		return nil, errors.New("chats: a2a ledger returned an unsupported shape")
	}
	return &Ledger{Agent: shape.Agent, Chats: shape.Chats}, nil
}

func fingerprint(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:8])
}

func withoutEnvironment(env []string, names ...string) []string {
	kept := env[:0:0]
	for _, entry := range env {
		drop := false
		for _, name := range names {
			if strings.HasPrefix(entry, name+"=") {
				drop = true
				break
			}
		}
		if !drop {
			kept = append(kept, entry)
		}
	}
	return kept
}

type boundedBuffer struct {
	buf      bytes.Buffer
	overflow bool
	limit    int
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	limit := b.limit
	if limit == 0 {
		limit = 64 << 10
	}
	remaining := limit - b.buf.Len()
	if remaining > 0 {
		if remaining > len(p) {
			remaining = len(p)
		}
		b.buf.Write(p[:remaining])
	}
	if remaining < len(p) {
		b.overflow = true
	}
	return len(p), nil
}

func decodeOneJSON(raw []byte, out any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	if err := dec.Decode(out); err != nil {
		return err
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("trailing data")
	}
	return nil
}

func (h *Handler) json(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (h *Handler) err(w http.ResponseWriter, status int, code, message string) {
	h.json(w, status, map[string]string{"error": code, "message": message})
}
