package session

import (
	"errors"
	"fmt"
	"time"

	"github.com/noisefloor-oss/deck/internal/audit"
)

// Status is the recorded lifecycle state. It is a *record*, not reality:
// displayed status is always derived by reconciling this against observed
// tmux/process state.
type Status string

const (
	// StatusStarting: the row exists but the pane may not; the identity tuple
	// is incomplete by definition.
	StatusStarting Status = "starting"
	// StatusRunning: the pane existed with a complete identity tuple when
	// last written. The schema refuses this status with a partial tuple.
	StatusRunning Status = "running"
	// StatusExited: the exit was OBSERVED and its cause recorded — an exit
	// code or a signal, never both, never neither.
	StatusExited Status = "exited"
	// StatusInterrupted records that Deck cannot attach the exact controller
	// and has no exit evidence. It does not claim an unverifiable predecessor
	// died; the browser projects that case as attachment-unavailable or inactive.
	// Deliberately distinct from exited, whose positive evidence is enforced by
	// the schema.
	StatusInterrupted Status = "interrupted"
	// StatusNeedsAttention is a frozen legacy database value written only by
	// earlier releases. New reconciliation code never writes it: a row whose
	// attachment is lost converges to interrupted or exited instead. The
	// value survives in the schema's CHECK constraint for inert read
	// compatibility with existing databases, and startup convergence moves
	// every legacy row off it before the first response.
	StatusNeedsAttention Status = "needs_attention"
)

// PresentationState is the backend's passive, content-free projection of one
// fresh runtime observation. The browser renders it directly and never infers
// process truth from a recorded lifecycle value.
type PresentationState string

const (
	PresentationRunning               PresentationState = "running"
	PresentationRecovering            PresentationState = "recovering"
	PresentationAttachmentUnavailable PresentationState = "attachment_unavailable"
	PresentationEnded                 PresentationState = "ended"
	PresentationInactive              PresentationState = "inactive"
	// PresentationResumeEnded: the recorded conversation was not continued —
	// the last generation resumed it and ended on its own with a failure
	// status, or its recorded store is gone by name — and no unattended
	// attempt remains. The record, its identity and the pane's output are
	// kept, and the card offers the explicit choices: resume again, or start
	// a fresh session beside it. Ended would hide the row, and a hidden row
	// leaves the user no way to make either choice.
	PresentationResumeEnded PresentationState = "resume_ended"
)

// RecoveryRefusal names, for a resume_ended row, what stands between the
// user and the recorded conversation. It is a passive projection like
// PresentationState: the provider's own words stay in the pane.
type RecoveryRefusal string

const (
	// RecoveryRefusalProvider: the provider was launched with the recorded
	// identity and ended without continuing it.
	RecoveryRefusalProvider RecoveryRefusal = "provider"
	// RecoveryRefusalStoreGone: the store the conversation was recorded in
	// no longer exists on disk, so nothing was launched.
	RecoveryRefusalStoreGone RecoveryRefusal = "store_gone"
)

// RecoveryCapability is the provider descriptor's explicit recovery shape.
// A provider-native id may be used to build argv, but it is never a browser-
// inferred lock or ownership signal. Resume means a resume can be ATTEMPTED:
// an assigned id names a conversation only once the provider has persisted
// one (claude writes nothing before the first submission), and only the
// provider's answer to the launch says whether it exists.
type RecoveryCapability string

const (
	RecoveryCapabilityResume   RecoveryCapability = "resume"
	RecoveryCapabilityRelaunch RecoveryCapability = "relaunch"
	RecoveryCapabilityNone     RecoveryCapability = "none"
)

// RecoveryState reports the durable successor opportunity/candidate state.
type RecoveryState string

const (
	RecoveryUnavailable      RecoveryState = "unavailable"
	RecoveryAutomaticPending RecoveryState = "automatic_pending"
	RecoveryRecovering       RecoveryState = "recovering"
)

// Session mirrors one row of the sessions table. Pointer fields are columns
// that are unknown until the pane exists (the runtime identity tuple)
// or until an exit is observed (evidence).
//
// The JSON tags ARE the API contract (snake_case, matching the project
// registry): the browser client reads these keys verbatim, and an untagged
// struct would silently emit PascalCase the client cannot consume.
type Session struct {
	ID        string `json:"id"`
	ProjectID string `json:"project_id"`
	Provider  string `json:"provider"`
	// CodexLoginHelper is a guidance hint, not authentication status. Each read
	// re-checks the auth metadata path the running generation launched with.
	CodexLoginHelper bool `json:"codex_login_helper,omitempty"`

	// DisplayName is the user's own label for the row. Empty
	// means the UI renders the provider, as before it existed. It is
	// metadata, never identity: nothing reconciles on it.
	DisplayName string `json:"display_name"`

	TmuxSocketPath  string  `json:"tmux_socket_path"`
	TmuxSessionName string  `json:"tmux_session_name"`
	TmuxSessionID   *string `json:"tmux_session_id,omitempty"`
	TmuxPaneID      *string `json:"tmux_pane_id,omitempty"`
	Generation      int     `json:"generation"`
	IdentityMarker  *string `json:"identity_marker,omitempty"`
	PanePID         *int    `json:"pane_pid,omitempty"`
	ProcessGroupID  *int    `json:"process_group_id,omitempty"`
	StartTicks      *int64  `json:"start_ticks,omitempty"`
	// BootEpoch is the host boot (the kernel's boot_id) under which the
	// live tuple members were recorded. Start ticks are boot-relative, so
	// a pid/start-ticks match is meaningful only while the recorded epoch
	// IS the current host boot. Nil on rows predating the column;
	// a nil epoch carries no claim and cannot verify a process identity.
	BootEpoch *string `json:"boot_epoch,omitempty"`
	Color     *string `json:"color,omitempty"`
	// ExpectedExecutable is the per-provider expectation for the kernel's
	// executable path for the pane process.
	// For interpreted providers this is the interpreter and is necessary but
	// NOT sufficient identity.
	ExpectedExecutable *string `json:"expected_executable,omitempty"`

	// NativeSessionID is the conversation the pane last reported it was on,
	// established at launch through a documented CLI contract (assigned for
	// Claude) and moved by the provider's own structured hook. It is never
	// parsed out of provider state. Nil for a provider whose retrieval
	// pipeline has not run.
	NativeSessionID *string `json:"native_session_id,omitempty"`
	// NativeStoreRoot is the store the conversation lives in, derived from
	// the ledger path the provider's own hook reported (the
	// CLAUDE_CONFIG_DIR-equivalent ancestor). It exists so a resume consults
	// that store: a moved or vanished store is a named condition, never a
	// silent wrong-directory lookup. It follows the identity — a move
	// rewrites it, forgetting the identity clears it — and is nil for rows
	// whose hook never reported a transcript. A server-internal fact, not
	// browser contract.
	NativeStoreRoot *string `json:"-"`

	ConversationAttention *ConversationAttention `json:"conversation_attention,omitempty"`
	Status                Status                 `json:"status"`
	// These three fields are derived for API responses from one fresh
	// observation plus the durable recovery-candidate record. They are not
	// database columns.
	PresentationState  PresentationState  `json:"presentation_state"`
	RecoveryRefusal    RecoveryRefusal    `json:"recovery_refusal,omitempty"`
	RecoveryCapability RecoveryCapability `json:"recovery_capability"`
	RecoveryState      RecoveryState      `json:"recovery_state"`

	ExitCode   *int       `json:"exit_code,omitempty"`
	ExitSignal *string    `json:"exit_signal,omitempty"`
	ExitedAt   *time.Time `json:"exited_at,omitempty"`
	RemovedAt  *time.Time `json:"removed_at,omitempty"`

	// Automatic recovery state. New code uses
	// ResumeAttempts as a binary generation opportunity: zero is open and one
	// is durably consumed. The released next-at/series columns remain readable
	// for database compatibility but new transitions only clear them.
	ResumeAttempts int `json:"resume_attempts"`
	// AutomaticRecoveries counts how many times the unattended sweep has
	// relaunched this row, and AutomaticRecoverySeriesAt when that count
	// started. ResumeAttempts bounds ONE generation's attempt; a committed
	// recovery clears it, so without these a provider that starts, commits and
	// exits again is relaunched forever at the reconciliation interval.
	AutomaticRecoveries       int        `json:"automatic_recoveries"`
	AutomaticRecoverySeriesAt *time.Time `json:"-"`
	// GenerationStartedAt is when the CURRENT generation's process was
	// committed, and GenerationResumed records whether that generation was
	// launched by resuming a recorded identity rather than starting fresh.
	// A resumed generation that dies within RefusedResumeWindow is how a
	// provider tells us it does not recognise the id we handed it.
	// GenerationStartedAt is nil for rows written before it existed, which
	// withholds that judgement instead of guessing it.
	GenerationStartedAt *time.Time `json:"-"`
	GenerationResumed   bool       `json:"-"`
	// ResumeNextAt and ResumeSeriesStartedAt are released database fields.
	// They are loaded only so upgrade convergence can clear inherited retry
	// state; they are not part of the session API.
	ResumeNextAt          *time.Time `json:"-"`
	ResumeSeriesStartedAt *time.Time `json:"-"`
	// ResumeProbationUntil is a released compatibility field. Exact-live
	// observation clears any inherited value immediately; new recovery never
	// writes one.
	ResumeProbationUntil *time.Time `json:"-"`
	// StopRequestedAt records that the user asked this session to
	// stop: a following exit is a requested exit, never a death for
	// automatic recovery to undo. The stamp is scoped to the death the
	// stop produces: a session OBSERVED alive past StopAttributionWindow
	// has provably survived the request, and the stamp is cleared so a
	// later genuine crash still recovers.
	StopRequestedAt *time.Time `json:"stop_requested_at,omitempty"`
	// WorkspaceStopGeneration survives ordinary stop expiry. A newly and
	// explicitly resumed generation gets its own automatic recovery policy.
	WorkspaceStopGeneration int `json:"-"`

	// LastOutputAt is tmux's content-free last PTY activity timestamp.
	// Nothing here reads or interprets provider output, and this
	// value never contributes to session lifecycle state.
	LastOutputAt *time.Time `json:"last_output_at,omitempty"`
	// IdentityCaptureLostAt records that a retrieved-identity launch closed its
	// durable capture grant without receiving an ID, either on expiry/positive
	// process death or during the one-time upgrade from the old in-memory
	// contract. A service restart preserves a current grant. The timestamp is
	// the premise a NULL native ID needs before it can mean capture is no longer
	// possible; a conversation may exist that no ID points at.
	IdentityCaptureLostAt *time.Time `json:"identity_capture_lost_at,omitempty"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	// ControllerRevision is an internal compare-and-swap fence. Cosmetic
	// metadata writes do not move it and it is never a browser contract.
	ControllerRevision int64 `json:"-"`
}

// Audit event types appended by this package. Every lifecycle transition
// commits its audit event in the same transaction as the row change;
// the vocabulary is fixed here so consumers can rely on it.
const (
	AuditCreated                 = string(audit.SessionCreated)
	AuditRunning                 = string(audit.SessionRunning)
	AuditLaunchFailed            = string(audit.SessionLaunchFailed)
	AuditStopRequested           = string(audit.SessionStopRequested)
	AuditStopExecuting           = string(audit.SessionStopExecuting)
	AuditStopCompleted           = string(audit.SessionStopCompleted)
	AuditStopSurvived            = string(audit.SessionStopSurvived)
	AuditExited                  = string(audit.SessionExited)
	AuditInterrupted             = string(audit.SessionInterrupted)
	AuditResumed                 = string(audit.SessionResumed)
	AuditResumeAttempt           = string(audit.SessionResumeAttempt)
	AuditResumeProbationCleared  = string(audit.SessionResumeProbationCleared)
	AuditRecoveryLegacyConverged = string(audit.SessionRecoveryLegacyConverged)
	AuditRemoved                 = string(audit.SessionRemoved)
	AuditRemovalExecuting        = string(audit.SessionRemovalExecuting)
	AuditRemovalProcessStopped   = string(audit.SessionRemovalProcessStopped)
	AuditRemovalCompleted        = string(audit.SessionRemovalCompleted)
	AuditRecordForgotten         = string(audit.SessionRecordForgotten)
	AuditRenamed                 = string(audit.SessionRenamed)
	AuditColorChanged            = "session.color_changed"
	AuditIdentityCaptured        = string(audit.SessionIdentityCaptured)
	AuditIdentityConfirmed       = string(audit.SessionIdentityConfirmed)
	AuditIdentityCaptureRefused  = string(audit.SessionIdentityCaptureRefused)
	AuditIdentityCaptureLost     = string(audit.SessionIdentityCaptureLost)
	AuditAttention               = string(audit.SessionAttention)
	// AuditUnknownPane is appended with no session id: it records a pane in
	// our namespace that no recorded session claims. The pane is
	// neither adopted nor killed nor written to; the event is the entire
	// action.
	AuditUnknownPane = string(audit.ReconUnknownPane)
)

// AuditStaleRecordDiscarded distinguishes a row-only stale discard from
// an ordinary removal that stops the recorded session.
const AuditStaleRecordDiscarded = string(audit.SessionStaleRecordDiscarded)

// ErrNotFound reports an unknown session id.
var ErrNotFound = errors.New("session: not found")

// ErrNotRunning reports a stop of a session that is not recorded as live.
var ErrNotRunning = errors.New("session: not running")

// ErrNotResumable reports an automatic recovery whose row is not a lost
// generation or whose descriptor cannot construct documented recovery argv.
var ErrNotResumable = errors.New("session: not resumable")

// ErrRecoveryInProgress reports that another process already admitted the
// row's one candidate. No second side effect is attempted.
var ErrRecoveryInProgress = errors.New("session: recovery already in progress")

// ErrResumeRefusedByProvider marks a recovery whose resumed candidate was
// gone before it committed: the provider was asked to continue the recorded
// identity and did not. The identity stays recorded and the automatic
// opportunity closes in the same pass; the user's card carries the
// refusal with its two explicit ways forward, resume again or a fresh
// session.
var ErrResumeRefusedByProvider = errors.New("session: provider refused the recorded native identity")

// ConversationStoreGoneError reports that the store a conversation's ledger
// was recorded in no longer exists on disk. It is a NAMED condition: the
// alternative is a resume that silently looks in whatever directory the
// current environment resolves and fails as if the conversation never
// existed. It joins ErrResumeRefusedByProvider so the refusal reaches the
// card with the gone store named.
type ConversationStoreGoneError struct{ Path string }

func (e *ConversationStoreGoneError) Error() string {
	return fmt.Sprintf("session: the store this conversation lived in (%s) is gone", e.Path)
}

// RecoveryOutcome names what a user recovery actually did, so the
// response can report it instead of letting a relaunch masquerade as a
// resume. It is derived from the committed generation's own record
// (GenerationResumed), never from intent.
type RecoveryOutcome string

const (
	// RecoveryOutcomeResumed: the recorded conversation was resumed exactly.
	RecoveryOutcomeResumed RecoveryOutcome = "resumed"
	// RecoveryOutcomeRelaunched: a fresh conversation was started in the same
	// session record — the previous thread did not survive and nothing may
	// imply it did.
	RecoveryOutcomeRelaunched RecoveryOutcome = "relaunched"
)

var ErrControllerActionInProgress = errors.New("session: controller action already in progress")

// ErrRecoverySuperseded reports that the source generation changed before a
// candidate could be admitted or committed.
var ErrRecoverySuperseded = errors.New("session: recovery source superseded")

// ErrInvalidRecoveryCandidate reports malformed local admission data.
var ErrInvalidRecoveryCandidate = errors.New("session: invalid recovery candidate")

// ErrIdentityMismatch reports that the observed pane does not match the
// recorded identity tuple. Nothing is signalled on a mismatch: killing a
// process we cannot prove is ours could terminate an unrelated session.
var ErrIdentityMismatch = errors.New("session: observed pane does not match recorded identity")

// ErrProviderNotLaunchable reports a provider whose launch path is not
// implemented yet (retrieved-identity providers await the Phase 2 capture
// pipeline).
var ErrProviderNotLaunchable = errors.New("session: provider launch not available in this phase")

// ErrConfirmationRequired reports that a destructive action requires an
// explicit user confirmation it did not receive.
var ErrConfirmationRequired = errors.New("session: confirmation required")
