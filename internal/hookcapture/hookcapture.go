// Package hookcapture accepts structured provider identity events. It owns the
// HTTP envelope and bearer-token hashing; the control database owns durable
// correlation and the atomic identity transition.
//
// The rule the package serves: any well-formed identity event arriving on a
// grant fenced to the live generation records that generation's current native
// session id. The pane holding a generation's credential is the authority on
// which conversation it is running, and whatever it last said is true.
package hookcapture

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/noisefloor-oss/deck/internal/provider/nativeid"
)

var (
	// ErrUnknownToken deliberately combines unknown, expired, and revoked
	// grants so the endpoint does not disclose which bearer hashes exist.
	ErrUnknownToken = errors.New("hookcapture: unknown or expired capture token")
	// ErrConflict covers an event that is not well formed for its grant, or
	// one whose grant no longer fences the generation it would write.
	ErrConflict = errors.New("hookcapture: capture event conflicts with its grant")
)

type eventConflict struct{ reason string }

func (e *eventConflict) Error() string { return ErrConflict.Error() }
func (e *eventConflict) Unwrap() error { return ErrConflict }

func conflict(reason string) error { return &eventConflict{reason: reason} }

// ConflictReason returns a stable, content-free validation category suitable
// for lifecycle diagnostics. It never includes hook payload values.
func ConflictReason(err error) string {
	var eventErr *eventConflict
	if errors.As(err, &eventErr) {
		return eventErr.reason
	}
	return "event"
}

const maxEventBytes = 1 << 13

// The two shapes a grant is minted for: a launch that starts a fresh
// conversation, and one that asks the provider for an exact recorded one.
// They decide what the grant expects at mint time; they are not a filter on
// what an event may report afterwards.
const (
	ExpectedSourceStartup = "startup"
	ExpectedSourceResume  = "resume"
)

// GrantState is the durable terminal fact used to serialize hook acceptance
// against timeout or cleanup. A caller may clean a provisional successor only
// after it wins the pending-to-revoked transition; consumed means acceptance
// won and the candidate must be verified instead.
type GrantState string

const (
	GrantPending  GrantState = "pending"
	GrantConsumed GrantState = "consumed"
	GrantExpired  GrantState = "expired"
	GrantRevoked  GrantState = "revoked"
)

// Event is the documented structured startup envelope emitted by supported
// native hooks/plugins. Extra provider fields are ignored by JSON decoding;
// terminal output, transcripts, and provider state are never inputs here.
type Event struct {
	HookEventName string `json:"hook_event_name"`
	SessionID     string `json:"session_id"`
	CWD           string `json:"cwd"`
	Source        string `json:"source"`
	// TranscriptPath is the conversation's exact ledger file, reported by
	// Claude's SessionStart payload
	// (<store-root>/projects/<cwd-slug>/<session-uuid>.jsonl). Optional: a
	// hook that omits it degrades store pinning only. When present on a
	// claude event it must derive a store root for the event's own session
	// id, or the event is malformed.
	TranscriptPath string `json:"transcript_path"`
	// PID is the operating-system process id of the process whose hook is
	// reporting, when the hook knows it. It is a process fact, not provider
	// output: the capture store uses it to keep a child process from
	// speaking for the pane — an omp subagent inherits the pane's capture
	// token and fires its own session_start — and ignores it when a hook
	// does not report one.
	PID      string `json:"pid"`
	Sequence int64  `json:"sequence"`
}

// Grant contains the non-secret correlation persisted before spawn. TokenHash
// is a lowercase SHA-256 digest; the raw bearer is returned only to the launch
// caller and never reaches the database.
type Grant struct {
	TokenHash        string
	SessionID        string
	Provider         string
	Generation       int
	IdentityMarker   string
	CanonicalWorkDir string
	ExpectedSource   string
	// ExpectedNativeIDHash is present only for exact resume. The opaque ID
	// itself remains owned by the session row and provider argv.
	ExpectedNativeIDHash string
	ExpiresAt            time.Time
}

// Credential is the short-lived launch material for one grant. Token is
// injected into the exact provider launch and never persisted; TokenHash is
// safe to bind to the durable grant and provisional-successor slot.
type Credential struct {
	Token     string
	TokenHash string
}

// Store is the durable half of capture. Implementations must consume a valid
// event, publish its native id, and append the capture audit in one transaction.
type Store interface {
	CreateIdentityCapture(context.Context, Grant) error
	AcceptIdentityCapture(context.Context, string, Event, time.Time) error
	RevokeIdentityCapture(context.Context, string, int, string, time.Time) (GrantState, error)
	IdentityCapturePending(context.Context, string, int, string, time.Time) (bool, error)
	ExpireIdentityCaptures(context.Context, time.Time) error
}

// Service is restart-neutral: all mutable grant state belongs to Store.
type Service struct {
	store Store
	ttl   time.Duration
	now   func() time.Time
}

func NewService(store Store, ttl time.Duration) (*Service, error) {
	if store == nil || ttl <= 0 {
		return nil, errors.New("hookcapture: durable store and positive ttl are required")
	}
	return &Service{store: store, ttl: ttl, now: time.Now}, nil
}

// RegisterFenced commits a hash-only grant before its launch side effect.
func (s *Service) RegisterFenced(ctx context.Context, sessionID, provider, workDir string,
	generation int, marker string) (string, error) {
	credential, grant, err := s.prepareGrant(sessionID, provider, workDir,
		generation, marker, ExpectedSourceStartup, "")
	if err != nil {
		return "", err
	}
	if err := s.store.CreateIdentityCapture(ctx, grant); err != nil {
		return "", err
	}
	return credential.Token, nil
}

// PrepareResumeGrant creates launch-only bearer material and a hash-only grant
// for insertion in the same transaction as a provisional successor slot.
// It deliberately does not persist either object by itself.
func (s *Service) PrepareResumeGrant(sessionID, provider, workDir string,
	generation int, marker, expectedNativeID string) (Credential, Grant, error) {
	return s.prepareGrant(sessionID, provider, workDir, generation, marker,
		ExpectedSourceResume, expectedNativeID)
}

// PrepareLaunchGrant creates the launch-shaped counterpart for a recovery
// generation that starts a FRESH conversation: there is no prior thread to
// attest, so the grant expects the startup source and no exact identity.
// Like PrepareResumeGrant, it persists nothing by itself.
func (s *Service) PrepareLaunchGrant(sessionID, provider, workDir string,
	generation int, marker string) (Credential, Grant, error) {
	return s.prepareGrant(sessionID, provider, workDir, generation, marker,
		ExpectedSourceStartup, "")
}

func (s *Service) prepareGrant(sessionID, provider, workDir string, generation int,
	marker, expectedSource, expectedNativeID string) (Credential, Grant, error) {
	if sessionID == "" || provider == "" || workDir == "" || generation < 1 || marker == "" {
		return Credential{}, Grant{}, errors.New("hookcapture: incomplete capture grant")
	}
	canonicalWorkDir, err := canonicalDirectory(workDir)
	if err != nil {
		return Credential{}, Grant{}, errors.New("hookcapture: capture work directory is not canonicalizable")
	}
	if expectedSource == ExpectedSourceStartup {
		if expectedNativeID != "" {
			return Credential{}, Grant{}, errors.New("hookcapture: startup grant cannot expect native identity")
		}
	} else if expectedSource != ExpectedSourceResume || !validNativeID(provider, expectedNativeID) {
		return Credential{}, Grant{}, errors.New("hookcapture: invalid exact-resume identity")
	}
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return Credential{}, Grant{}, fmt.Errorf("hookcapture: generate bearer: %w", err)
	}
	credential := Credential{Token: hex.EncodeToString(raw)}
	credential.TokenHash = hashToken(credential.Token)
	now := s.now()
	grant := Grant{
		TokenHash: credential.TokenHash, SessionID: sessionID, Provider: provider,
		Generation: generation, IdentityMarker: marker, CanonicalWorkDir: canonicalWorkDir,
		ExpectedSource: expectedSource, ExpiresAt: now.Add(s.ttl),
	}
	if expectedNativeID != "" {
		grant.ExpectedNativeIDHash = NativeIDHash(expectedNativeID)
	}
	return credential, grant, nil
}

func (s *Service) RevokeFenced(ctx context.Context, sessionID string, generation int,
	marker string) error {
	_, err := s.store.RevokeIdentityCapture(ctx, sessionID, generation, marker, s.now())
	if errors.Is(err, ErrUnknownToken) {
		// Revocation is idempotent. Assigned-identity providers never create a
		// grant, and closed grants may already have aged out of durable storage.
		return nil
	}
	return err
}

func (s *Service) RevokeFencedState(ctx context.Context, sessionID string, generation int,
	marker string) (GrantState, error) {
	return s.store.RevokeIdentityCapture(ctx, sessionID, generation, marker, s.now())
}

func (s *Service) Pending(ctx context.Context, sessionID string, generation int,
	marker string) (bool, error) {
	return s.store.IdentityCapturePending(ctx, sessionID, generation, marker, s.now())
}

func (s *Service) Sweep(ctx context.Context) error {
	return s.store.ExpireIdentityCaptures(ctx, s.now())
}

// Handle serves the token-authenticated hook route outside browser auth. It
// never writes a request body, token, cwd, or provider payload to logs.
func (s *Service) Handle() http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		token := req.Header.Get("X-Noisedeck-Hook-Token")
		if token == "" {
			http.Error(w, "missing hook token", http.StatusBadRequest)
			return
		}
		var ev Event
		dec := json.NewDecoder(http.MaxBytesReader(w, req.Body, maxEventBytes))
		if err := dec.Decode(&ev); err != nil {
			http.Error(w, "unreadable hook event", http.StatusBadRequest)
			return
		}
		var trailing any
		if err := dec.Decode(&trailing); err != io.EOF {
			http.Error(w, "hook event must be a single JSON value", http.StatusBadRequest)
			return
		}
		err := s.store.AcceptIdentityCapture(req.Context(), hashToken(token), ev, s.now())
		switch {
		case err == nil:
			w.WriteHeader(http.StatusNoContent)
		case errors.Is(err, ErrUnknownToken):
			http.Error(w, "capture token is unavailable", http.StatusGone)
		case errors.Is(err, ErrConflict):
			http.Error(w, "capture event conflicts with its grant", http.StatusConflict)
		default:
			http.Error(w, "identity could not be recorded", http.StatusInternalServerError)
		}
	}
}

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// NativeIDHash returns the non-secret correlation digest stored for an exact
// resume expectation. Provider native IDs remain opaque; callers compare only
// byte identity and never interpret their contents beyond provider validation.
func NativeIDHash(id string) string {
	sum := sha256.Sum256([]byte(id))
	return hex.EncodeToString(sum[:])
}

// ValidateEvent checks that an event is well formed enough to be believed: a
// session id of the provider's known shape, the working directory the grant
// was minted for, and — where the payload carries one — a transcript naming
// the event's own conversation. These are sanity, and they catch a
// misconfigured hook posting somebody else's payload.
//
// They deliberately do not judge WHICH conversation the pane reports, nor how
// it got there. Deck runs CLIs it does not control; a rule about which of a
// provider's hook names or entry sources may carry a new id models the
// provider's internals, and breaks on the provider's schedule. The caller
// supplies the canonical cwd stored with the grant; the value is never
// included in an error or audit detail.
func ValidateEvent(provider, canonicalWorkDir string, ev Event) error {
	if !validNativeID(provider, ev.SessionID) {
		return conflict("native_identity_shape")
	}
	if ev.TranscriptPath != "" && provider == "claude" {
		if _, err := ClaudeStoreRoot(ev.TranscriptPath, ev.SessionID); err != nil {
			return conflict("transcript_path")
		}
	}
	// Devin's documented SessionStart payload omits cwd; its documented
	// DEVIN_PROJECT_DIR environment is local to the hook command and does not
	// cross the capture endpoint. The pre-spawn bearer grant remains bound to
	// the canonical project directory. Other providers must attest cwd in the
	// structured event itself.
	if provider != "devin" && !samePath(ev.CWD, canonicalWorkDir) {
		return conflict("working_directory")
	}
	return nil
}

// ClaudeStoreRoot derives the store root from a SessionStart transcript path.
// The measured ledger layout is
// <store-root>/projects/<cwd-slug>/<session-uuid>.jsonl, so the root is the
// directory containing projects/ — the CLAUDE_CONFIG_DIR-equivalent the
// conversation actually lives under. The path must be absolute, must name the
// event's own session id, and must sit exactly one slug directory below
// projects/: anything else is not the documented ledger shape and derives
// nothing.
func ClaudeStoreRoot(transcriptPath, sessionID string) (string, error) {
	if !filepath.IsAbs(transcriptPath) {
		return "", errors.New("hookcapture: transcript path is not absolute")
	}
	cleaned := filepath.Clean(transcriptPath)
	if filepath.Base(cleaned) != sessionID+".jsonl" {
		return "", errors.New("hookcapture: transcript path does not name the event's session")
	}
	slugDir := filepath.Dir(cleaned)
	projectsDir := filepath.Dir(slugDir)
	root := filepath.Dir(projectsDir)
	if filepath.Base(projectsDir) != "projects" || slugDir == projectsDir || root == projectsDir ||
		root == "/" || root == "." {
		return "", errors.New("hookcapture: transcript path is not inside a projects/ store")
	}
	return root, nil
}

// AcceptedEventHash defines replay equivalence independently of JSON key order
// or whitespace. Fields are length-framed to avoid concatenation ambiguity.
//
// It covers the FACT an event carries — which conversation, in which ledger —
// and not the hook name or entry source that carried it. Two deliveries
// naming the same conversation say the same thing however the provider
// labelled them, so a duplicate is a no-op; an event naming the same session
// in a different ledger is a different fact, not a replay.
func AcceptedEventHash(provider, canonicalWorkDir string, ev Event) string {
	h := sha256.New()
	hashField(h, provider)
	hashField(h, canonicalWorkDir)
	hashField(h, ev.SessionID)
	hashField(h, ev.TranscriptPath)
	return hex.EncodeToString(h.Sum(nil))
}

func hashField(h hash.Hash, value string) {
	var length [8]byte
	binary.BigEndian.PutUint64(length[:], uint64(len(value)))
	_, _ = h.Write(length[:])
	_, _ = h.Write([]byte(value))
}

func validNativeID(provider, id string) bool {
	switch provider {
	case "claude", "command-code", "codex":
		return nativeid.IsCanonicalUUID(id)
	case "omp":
		return nativeid.IsOmpUUIDv7(id)
	case "pi":
		return nativeid.IsPiUUIDv7(id)
	case "kimi":
		return nativeid.IsKimiSession(id)
	case "opencode", "kilo":
		return nativeid.IsOpenCodeSession(id)
	case "devin":
		return nativeid.IsDevinSession(id)
	default:
		return false
	}
}

func samePath(a, b string) bool {
	left, err := canonicalDirectory(a)
	if err != nil {
		return false
	}
	right, err := canonicalDirectory(b)
	return err == nil && left == right
}

func canonicalDirectory(path string) (string, error) {
	if !filepath.IsAbs(path) {
		return "", errors.New("relative work directory")
	}
	resolved, err := filepath.EvalSymlinks(filepath.Clean(strings.TrimRight(path, "/")))
	if err != nil {
		return "", err
	}
	info, err := os.Stat(resolved)
	if err != nil || !info.IsDir() {
		return "", errors.New("work directory is unavailable")
	}
	return filepath.Clean(resolved), nil
}
