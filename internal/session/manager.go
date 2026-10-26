package session

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/noisefloor-oss/deck/internal/audit"
	"github.com/noisefloor-oss/deck/internal/hookcapture"
	"github.com/noisefloor-oss/deck/internal/inputgate"
	"github.com/noisefloor-oss/deck/internal/project"
	"github.com/noisefloor-oss/deck/internal/provider"
	"github.com/noisefloor-oss/deck/internal/tmux"
)

// AuditStopUnobserved records that a stop signal was delivered but no exit
// was observed before the caller's deadline. The session stays `running`
// because claiming anything else would be unverified; the user can force
// or look at the terminal.
const AuditStopUnobserved = string(audit.SessionStopUnobserved)

// stopDetachedWriteTimeout bounds each write a stop makes after its own
// context may have expired.
const stopDetachedWriteTimeout = 5 * time.Second

// ErrStopUnobserved is returned with that audit event.
var ErrStopUnobserved = errors.New("session: stop signalled but exit not observed before deadline")

// errStopUnobservedAudit marks the distinct failure to persist the evidence
// that makes an unobserved stop safe to report as an honest 200 response.
var errStopUnobservedAudit = errors.New("session: stop-unobserved audit failed")

// Manager owns session lifecycle: launching a provider CLI as the pane
// process of a dedicated tmux session, recording its runtime identity,
// stopping it, and recording observed exits.
//
// Boundaries, all deliberate:
//
//   - The CLI is launched directly as the pane command with remain-on-exit;
//     input can never fall through to a shell, and a dead pane and
//     its exit evidence are PRESERVED — nothing here calls kill-session.
//   - "Stop" signals the pane's process group per the provider's contract
//     and records the requested action and observed exit. Escaped
//     tool subprocesses are OUTSIDE that boundary and may outlive the stop
//     (docs/provider-contracts/) — there is no /proc-walking compensation.
//   - Displayed status is derived by reconciliation, never read
//     straight from these rows.
//   - WorkDir allowlisting is the project layer's job; callers
//     hand this manager an already-authorized directory.
type Manager struct {
	repo      *Repository
	tmux      *tmux.Client
	providers map[provider.Name]provider.Descriptor
	prefix    string
	// leases and workDir are automatic resume's collaborators. gate is
	// initialized for lifecycle serialization and replaced by SetResumeDeps
	// with the gate shared by terminal input.
	leases LeaseInvalidator
	gate   *inputgate.Gate
	// fence stops admission into a project that is being closed.
	fence   *projectFence
	workDir WorkDirResolver
	// captures is the durable retrieved-identity hook pipeline; its bearer
	// hashes survive backend restart in SQLite.
	captures       *hookcapture.Service
	captureURLBase string
	captureSocket  string
	// settings supplies the console-wide launch settings, wired by
	// SetSettings. Nil means "no settings source": launches then use the
	// safe defaults, which is what the session tests exercise.
	settings               SkipPermissionsReader
	resumeAttestationBound time.Duration
	// resumeSweepBound budgets ONE automatic-recovery pass, not the whole
	// backlog: a candidate that cannot finish inside what remains is left
	// for the next reconciliation pass, and a pass whose budget expires
	// mid-flight unwinds the admission itself (see AutoResumeSweep). Tests
	// shrink it to exercise the truncation.
	resumeSweepBound time.Duration
	// These fields are populated only for the default-off capacity-routing experiment.
	// The capability is an in-memory accident boundary for the shared HTTP
	// listener, not a user identity or a confinement claim.
	unconfinedControlURL        string
	unconfinedControlCapability string
	tempRoot                    string
	harnessLoginEnv             func(context.Context, provider.Descriptor) []string
	codexLaunches               sync.Map
	projectPreviewSocket        string
	displayNameMu               sync.Mutex
	registerDisplayName         func(context.Context, RosterRegistration)
	projectName                 func(context.Context, string) (string, error)

	now   func() time.Time
	newID func() (string, error)
	// procGroup is the final /proc identity resolver used immediately before
	// process-group signals. Tests replace it to exercise group-reuse races.
	procGroup func(int, int64) (int, error)
	// lifecycleHooks is nil outside same-package concurrency tests.
	lifecycleHooks *lifecycleTestHooks
}

type lifecycleTestHooks struct {
	workspaceLockPreflight func()
	workspaceLockMarked    func()
	resumeGated            func()
	resumeSpawned          func(*tmux.PaneInfo)
	resumeCommitted        func()
	removeGated            func()
	removeExecuting        func()
	removeBeforeSignal     func()
	removeProcessStopped   func()
	removePaneKilled       func()
	stopSignaled           func()
}

// SetProjectPreviewSocket supplies only the nonsecret metadata socket location
// to new sessions. Existing sessions can use the default or --socket.
func (m *Manager) SetProjectPreviewSocket(path string) { m.projectPreviewSocket = path }

func (m *Manager) projectPreviewEnvironment() []string {
	if m.projectPreviewSocket == "" {
		return nil
	}
	return []string{"NOISEDECK_PROJECT_PREVIEWS_METADATA_SOCKET=" + m.projectPreviewSocket}
}

// SetHarnessLoginEnvironment installs native browser openers only at launch.
func (m *Manager) SetHarnessLoginEnvironment(resolve func(context.Context, provider.Descriptor) []string) {
	m.harnessLoginEnv = resolve
}

func (m *Manager) harnessLoginEnvironment(ctx context.Context, desc provider.Descriptor) []string {
	if m.harnessLoginEnv == nil {
		return nil
	}
	return m.harnessLoginEnv(ctx, desc)
}

// SetHookCaptureDeps wires the durable retrieved-identity capture pipeline and
// this service's own loopback hook URL.
func (m *Manager) SetHookCaptureDeps(captures *hookcapture.Service, urlBase string) {
	m.captures = captures
	m.captureURLBase = strings.TrimRight(urlBase, "/")
}

// SetHookCaptureSocket selects the same private listener used by the gateway.
// The hook keeps its per-generation credential; the socket is only transport.
func (m *Manager) SetHookCaptureSocket(path string) { m.captureSocket = path }

func (m *Manager) revokeCaptureFenced(ctx context.Context, id string, generation int,
	marker string) error {
	if m.captures == nil {
		return nil
	}
	return m.captures.RevokeFenced(ctx, id, generation, marker)
}

// NewManager wires the lifecycle manager. prefix namespaces tmux session
// names (config tmux.session_prefix).
func NewManager(repo *Repository, t *tmux.Client, providers map[provider.Name]provider.Descriptor, prefix string) *Manager {
	return &Manager{
		repo:      repo,
		tmux:      t,
		providers: providers,
		prefix:    prefix,
		gate:      inputgate.New(),
		fence:     newProjectFence(),
		now:       time.Now,
		newID:     newUUIDv4,
		procGroup: tmux.ProcGroup,
		// Long enough for claude to answer a resume it refuses — about one
		// second on a quiet host, several under load — so the user's
		// click is answered with the refusal instead of a commit the
		// reconciler contradicts. A resume the provider accepts attests
		// well inside this and never waits it out; only a silent hook pays
		// the full bound before its pane commits anyway.
		resumeAttestationBound: 10 * time.Second,
		// Sized so one boot pass recovers a typical backlog of
		// quick candidates plus a couple of attested ones; whatever does
		// not fit is resumed by the reconciler's next passes, never dropped.
		resumeSweepBound: 30 * time.Second,
	}
}

// SkipPermissionsReader is the slice of the settings repository launch
// needs. An interface, not the concrete type, so the session package
// keeps no dependency on settings storage.
type SkipPermissionsReader interface {
	SkipPermissions(ctx context.Context) (bool, error)
}

// SetSettings wires the console settings source. Called once at boot;
// leaving it unset means every launch uses the safe defaults.
func (m *Manager) SetSettings(s SkipPermissionsReader) { m.settings = s }

// SetUnconfinedCapacityRouting exposes the loopback experiment command to
// sessions launched after the feature was explicitly enabled.
func (m *Manager) SetUnconfinedCapacityRouting(controlURL, capability string) {
	m.unconfinedControlURL = strings.TrimRight(controlURL, "/")
	m.unconfinedControlCapability = capability
}

// CreateSpec is the input to Create.
type CreateSpec struct {
	ProjectID   string
	Provider    provider.Name
	WorkDir     string
	DisplayName string
	// Cols and Rows are the creating client's terminal size. Zero falls back
	// to the tmux default; a real size lays the session's headless output out
	// for the screen that asked for it instead of an 80-column nobody.
	Cols uint16
	Rows uint16
}

// Create launches a new provider session and records it. The row is written
// in `starting` before tmux is touched, so a crash mid-launch leaves an
// auditable record rather than an orphan pane with no row.
//
// The identity contract drives the launch shape: assigned (Claude) embeds our
// id in argv, retrieved providers wire the one-shot identity capture,
// identity-none (shell) has no identity to assign or capture, and a
// fresh-relaunch-only provider intentionally records no native identity.
func (m *Manager) Create(ctx context.Context, spec CreateSpec) (*Session, error) {
	return m.create(ctx, spec, nil)
}

// CreateDelegated launches the one fixed initial-prompt shape permitted by
// the explicit delegated-launch contract. The ordinary Create API has no
// prompt parameter.
func (m *Manager) CreateDelegated(ctx context.Context, spec CreateSpec,
	delegated provider.DelegatedLaunch) (*Session, error) {
	return m.create(ctx, spec, &delegated)
}

func (m *Manager) create(ctx context.Context, spec CreateSpec,
	delegated *provider.DelegatedLaunch) (*Session, error) {
	done, err := m.gate.Admit()
	if err != nil {
		return nil, err
	}
	defer done()
	endCreate, err := m.fence.admit(spec.ProjectID)
	if err != nil {
		return nil, err
	}
	defer endCreate()
	desc, ok := m.providers[spec.Provider]
	if !ok {
		return nil, fmt.Errorf("session: provider %s is not available in this deployment: %w",
			spec.Provider, ErrProviderNotLaunchable)
	}
	if err := desc.Availability(); err != nil {
		return nil, fmt.Errorf("session: provider %s unavailable: %w: %w",
			desc.Name(), ErrProviderNotLaunchable, err)
	}
	// The configured CLI must exist before tmux tries to exec it: a missing
	// installation refuses with an actionable provider_not_launchable error
	// (install the CLI or set its *_EXECUTABLE) instead of a generic 500.
	if err := executableRunnable(desc.Executable()); err != nil {
		return nil, fmt.Errorf("session: provider %s not launchable: %w: %w",
			desc.Name(), ErrProviderNotLaunchable, err)
	}
	contract := desc.IdentityContract()
	// Hook provisioning is a DIAGNOSTIC, never a launch precondition. It is
	// measured against provider CLIs Deck does not own, whose config
	// schemas change upstream on their own schedule, and a stale measurement
	// here would refuse a CLI that runs correctly in a terminal. A hook that
	// cannot deliver costs auto-resume and nothing else, which is a degraded
	// convenience, not a broken session.

	id, err := m.newID()
	if err != nil {
		return nil, fmt.Errorf("session: generate id: %w", err)
	}
	marker, err := m.newID()
	if err != nil {
		return nil, fmt.Errorf("session: generate marker: %w", err)
	}
	gate := m.gate.Session(id)
	gate.Lock()
	defer gate.Unlock()

	// NewSessionArgs returns the COMPLETE argv, argv[0] being the executable
	// (descriptor contract). Prepending Executable() again would hand the CLI
	// its own path as a positional argument and kill every real launch. Only
	// the assigned contract embeds our id: a retrieved-identity provider
	// allocates its own, and an identity-none provider has none —
	// passing one is an error at the descriptor.
	launchSpec := provider.LaunchSpec{WorkDir: spec.WorkDir}
	if contract == provider.IdentityAssigned {
		launchSpec.SessionID = id
	}
	// A provider whose hook registration is Deck-provisioned gets the settings
	// file inside this launch's generation directory (generation 1 for a
	// create). The path is named now so argv can carry it; the document is
	// written once the directory exists, before the spawn. Without a
	// temporary root there is nowhere Deck owns to put it, and the launch
	// proceeds with identity tracking degraded rather than refused.
	launchSpec.HookSettingsPath = m.provisionedHookSettingsPath(desc, id, 1, marker)
	// The console setting is read at LAUNCH, not cached at boot: the
	// user's latest choice applies to the next session without a
	// restart. A read failure REFUSES the launch rather than guessing a
	// mode: launching with the flag when the user may not have asked
	// removes safety prompts, and launching without it when they did ask
	// silently ignores the setting. The store lives in the same database
	// this launch is about to write to, so a read failure here means the
	// create would fail moments later anyway.
	if m.settings != nil {
		skip, err := m.settings.SkipPermissions(ctx)
		if err != nil {
			return nil, fmt.Errorf("session: read skip-permissions setting: %w", err)
		}
		launchSpec.SkipPermissions = skip
	}
	var argv []string
	if delegated == nil {
		argv, err = desc.NewSessionArgs(launchSpec)
	} else {
		delegating, ok := desc.(provider.DelegationDescriptor)
		if !ok {
			return nil, fmt.Errorf("session: provider %s has no measured delegated launch contract: %w",
				desc.Name(), ErrProviderNotLaunchable)
		}
		argv, err = delegating.NewDelegatedSessionArgs(launchSpec, *delegated)
	}
	if err != nil {
		return nil, fmt.Errorf("session: %s launch args: %w", desc.Name(), err)
	}
	if len(argv) == 0 {
		return nil, fmt.Errorf("session: %s returned an empty argv", desc.Name())
	}

	expected, err := expectedExecutable(desc, argv[0])
	if err != nil {
		return nil, fmt.Errorf("session: %s expected executable: %w", desc.Name(), err)
	}
	bootEpoch, err := currentBootEpoch()
	if err != nil {
		return nil, err
	}

	s := &Session{
		ID:              id,
		ProjectID:       spec.ProjectID,
		Provider:        string(desc.Name()),
		TmuxSocketPath:  m.tmux.SocketPath(),
		TmuxSessionName: m.prefix + id[:8],
		Generation:      1,
		IdentityMarker:  &marker,
		DisplayName:     spec.DisplayName,
	}
	if contract == provider.IdentityAssigned {
		// Assigned contract: the id we embed IS the native id.
		s.NativeSessionID = &id
	}
	if err := m.repo.Create(ctx, s); err != nil {
		return nil, err
	}

	term, err := m.tmux.DefaultTerminal(ctx)
	if err != nil {
		reason := fmt.Sprintf("read default-terminal: %v", err)
		if _, aerr := m.repo.MarkInterruptedFromStarting(ctx, s, reason); aerr != nil {
			return nil, errors.Join(err, aerr)
		}
		return nil, fmt.Errorf("session %s: %s", s.ID, reason)
	}

	extraEnv := append(sessionIdentityEnv(spec.ProjectID, id), m.unconfinedControlEnv(spec.ProjectID, id)...)
	extraEnv = append(extraEnv, m.projectPreviewEnvironment()...)
	extraEnv = append(extraEnv, m.harnessLoginEnvironment(ctx, desc)...)
	assignedHookTracking := contract == provider.IdentityAssigned &&
		declaresStructuredHookCapture(desc)
	if contract == provider.IdentityRetrieved || assignedHookTracking {
		// The durable identity capture: registered last before
		// launch. It is revoked only after the exact launch is proven gone;
		// an uncertain or replaced runtime keeps its fenced credential until
		// expiry. The token is the correlation and the credential — the hook
		// presents it in a header, never a URL the request log would record.
		// For an assigned-then-tracked provider the same grant is
		// the channel that follows the provider's own id rolls.
		if m.captures == nil || m.captureURLBase == "" {
			if contract == provider.IdentityRetrieved {
				return nil, fmt.Errorf("session: %s identity capture is not wired: %w",
					desc.Name(), ErrProviderNotLaunchable)
			}
			// An assigned identity works without the capture pipeline: the
			// launch proceeds and native_session_id stays the birth identity.
		} else {
			token, captureErr := m.captures.RegisterFenced(ctx, id, string(desc.Name()),
				spec.WorkDir, s.Generation, marker)
			if captureErr != nil {
				reason := "identity capture grant could not be persisted before launch"
				_, rowErr := m.repo.MarkInterruptedFromStarting(ctx, s, reason)
				return nil, errors.Join(captureErr, rowErr)
			}
			extraEnv = append(extraEnv,
				"NOISEDECK_SESSION_HOOK_URL="+m.captureURLBase+"/api/hooks/capture",
				"NOISEDECK_SESSION_HOOK_TOKEN="+token)
			if m.captureSocket != "" {
				extraEnv = append(extraEnv, "NOISEDECK_SESSION_HOOK_SOCKET="+m.captureSocket)
			}
		}
	}
	tempDir, tempErr := m.prepareGenerationTemp(s.ID, s.Generation, marker)
	if tempErr != nil {
		revokeErr := m.revokeCaptureFenced(ctx, s.ID, s.Generation, marker)
		reason := "provider temporary directory could not be prepared before launch"
		_, rowErr := m.repo.MarkInterruptedFromStarting(ctx, s, reason)
		return nil, errors.Join(tempErr, revokeErr, rowErr)
	}
	if tempDir != "" {
		extraEnv = append(extraEnv, "TMPDIR="+tempDir)
	}
	extraEnv = append(extraEnv, hookDiagnosticsEnvironment(tempDir)...)
	if hookErr := writeProvisionedHookSettings(desc, launchSpec.HookSettingsPath); hookErr != nil {
		revokeErr := m.revokeCaptureFenced(ctx, s.ID, s.Generation, marker)
		tempErr := m.removeGenerationTemp(s.ID, s.Generation, marker)
		reason := "provider hook settings could not be provisioned before launch"
		_, rowErr := m.repo.MarkInterruptedFromStarting(ctx, s, reason)
		return nil, errors.Join(hookErr, revokeErr, tempErr, rowErr)
	}

	wrappedArgv := launchArgv(desc, argv, term, extraEnv)
	loginHelper := delegated == nil && desc.Name() == provider.Codex &&
		m.recordCodexLaunch(s.ID, s.Generation, wrappedArgv[:len(wrappedArgv)-len(argv)], spec.WorkDir)
	info, err := m.tmux.CreateSession(ctx, tmux.SessionSpec{
		Name:       s.TmuxSessionName,
		WorkDir:    spec.WorkDir,
		Argv:       wrappedArgv,
		Generation: s.Generation,
		Marker:     marker,
		Cols:       spec.Cols,
		Rows:       spec.Rows,
	})
	if err != nil {
		var revokeErr error
		var tempErr error
		if errors.Is(err, tmux.ErrPartialSpawnCleaned) {
			revokeErr = m.revokeCaptureFenced(ctx, s.ID, s.Generation, marker)
			tempErr = m.removeGenerationTemp(s.ID, s.Generation, marker)
		}
		reason := fmt.Sprintf("tmux launch failed: %v", err)
		if _, aerr := m.repo.MarkInterruptedFromStarting(ctx, s, reason); aerr != nil {
			return nil, errors.Join(err, revokeErr, tempErr, aerr)
		}
		return nil, errors.Join(fmt.Errorf("session %s: launch: %w", s.ID, err), revokeErr, tempErr)
	}
	if !info.MatchesSpawnOrigin() {
		return nil, m.rejectInitialSpawnReplacement(ctx, s, info)
	}

	// The pane must be provably ours before it is recorded as running.
	if info.Marker != marker || info.Generation != s.Generation {
		reason := fmt.Sprintf("pane markers did not round-trip: got gen=%d marker=%q", info.Generation, info.Marker)
		if _, aerr := m.repo.MarkInterruptedFromStarting(ctx, s, reason); aerr != nil {
			return nil, aerr
		}
		return nil, fmt.Errorf("session %s: %s", s.ID, reason)
	}

	// A pane that already died — a bad executable, an instant crash — must
	// not be recorded as running: the live tuple members are gone with the
	// process, and `running` with fabricated members is exactly what
	// chk_sessions_running_identity exists to refuse. Record the preserved
	// evidence instead. (A pane dying just after this observation is the
	// reconciler's to catch.)
	if info.Dead {
		if err := m.recordEarlyExit(ctx, s, marker, info); err != nil {
			return nil, err
		}
		return m.repo.Get(ctx, s.ID)
	}

	// A live pane can attach before its native ID arrives. The capture grant
	// stays active so the provider's hook can make it resumable afterward.
	if info.PID <= 1 || info.ProcessGroupID <= 1 || info.StartTicks <= 0 {
		return nil, m.rejectInitialLaunch(ctx, s,
			"initial pane process identity was incomplete",
			ErrIdentityMismatch)
	}
	// The observed executable is RECORDED, not required to match a measured
	// path. The pid, start ticks, generation, and marker already pin this row
	// to the exact process this launch created; the kernel path proves nothing
	// on top of that and is the one field a user legitimately changes
	// underneath us — a node upgrade, a runtime moved under a version manager,
	// a CLI that starts bundling its own interpreter. Refusing on it would
	// reject a process Deck had just successfully spawned.
	if info.Executable != "" {
		expected = info.Executable
	}

	s.TmuxSessionID = &info.SessionID
	s.TmuxPaneID = &info.PaneID
	s.IdentityMarker = &marker
	s.PanePID = &info.PID
	s.ProcessGroupID = &info.ProcessGroupID
	s.StartTicks = &info.StartTicks
	s.BootEpoch = &bootEpoch
	s.ExpectedExecutable = &expected
	if err := m.repo.MarkRunning(ctx, s); err != nil {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 8*time.Second)
		defer cancel()
		stopErr := m.terminateRecordedProcess(cleanupCtx, s, AuditLaunchFailed,
			"launch cleanup", nil)
		if stopErr != nil {
			return nil, errors.Join(err, stopErr)
		}
		applied, killErr := m.tmux.KillPaneIfIdentity(cleanupCtx, tmux.PaneIdentity{
			SessionID: info.SessionID, PaneID: info.PaneID, PanePID: info.PID,
			Generation: info.Generation, Marker: info.Marker,
			LaunchMarker: info.LaunchMarker,
		})
		if errors.Is(killErr, tmux.ErrNoSession) {
			killErr = nil
		} else if killErr == nil && !applied {
			killErr = ErrIdentityMismatch
		}
		if killErr == nil {
			killErr = m.revokeCaptureFenced(cleanupCtx, s.ID, s.Generation, marker)
		}
		return nil, errors.Join(err, killErr)
	}
	m.syncDisplayName(ctx, s.ID)
	created, err := m.repo.Get(ctx, s.ID)
	if err == nil {
		created.CodexLoginHelper = loginHelper
	}
	return created, err
}

// rejectInitialSpawnReplacement retires only Deck's incomplete row. The pane
// no longer has the physical identity returned by new-session, so neither its
// process nor its launch token can be safely revoked or cleaned up here.
//
// The reason names the members that disagree. Nothing observed later can
// reconstruct the launch-time tuple, so the persisted reason is the only
// evidence this refusal leaves, and a pid that moved, a process group that
// never settled, and a start time belonging to a recycled pid each call for a
// different answer.
func (m *Manager) rejectInitialSpawnReplacement(ctx context.Context, s *Session, info *tmux.PaneInfo) error {
	return m.rejectInitialLaunch(ctx, s,
		fmt.Sprintf("initial pane physical identity changed before verified-live commitment "+
			"(%s)", spawnOriginDivergence(info)),
		ErrIdentityMismatch)
}

// spawnOriginDivergence names the members that disagree, and says so in the
// order MatchesSpawnOrigin tests them. A dead pane keeps only its leader pid,
// so the live members are not reported for one.
func spawnOriginDivergence(info *tmux.PaneInfo) string {
	if info == nil {
		return "no pane observation"
	}
	parts := make([]string, 0, 3)
	if info.PID != info.SpawnPID {
		parts = append(parts, fmt.Sprintf("pid %d != spawn pid %d", info.PID, info.SpawnPID))
	}
	if !info.Dead {
		if info.ProcessGroupID != info.SpawnProcessGroupID {
			parts = append(parts, fmt.Sprintf("pgid %d != spawn pgid %d",
				info.ProcessGroupID, info.SpawnProcessGroupID))
		}
		if info.StartTicks != info.SpawnStartTicks {
			parts = append(parts, fmt.Sprintf("start ticks %d != spawn start ticks %d",
				info.StartTicks, info.SpawnStartTicks))
		}
	}
	if len(parts) == 0 {
		// Every member agrees, so the tuple was never complete enough to
		// verify: an unread identity, not a replaced one.
		return fmt.Sprintf("identity incomplete: pid %d pgid %d start ticks %d dead=%t",
			info.PID, info.ProcessGroupID, info.StartTicks, info.Dead)
	}
	return strings.Join(parts, ", ")
}

func (m *Manager) rejectInitialLaunch(ctx context.Context, s *Session, reason string,
	cause error) error {
	if _, err := m.repo.MarkInterruptedFromStarting(ctx, s, reason); err != nil {
		return errors.Join(cause, err)
	}
	return fmt.Errorf("session %s: %s: %w", s.ID, reason, cause)
}

// recordEarlyExit persists a launch whose pane was already dead when first
// observed: the durable identity members plus the preserved exit evidence.
func (m *Manager) recordEarlyExit(ctx context.Context, s *Session, marker string, info *tmux.PaneInfo) error {
	s.TmuxSessionID = &info.SessionID
	s.TmuxPaneID = &info.PaneID
	s.IdentityMarker = &marker
	if info.ExitStatus == nil && info.ExitSignal == nil {
		reason := "pane dead at launch with no recoverable exit evidence"
		if err := m.repo.RecordEarlyInterrupt(ctx, s, reason); err != nil {
			return err
		}
		return nil
	}
	code, sig := evidence(info)
	if err := m.repo.RecordEarlyExit(ctx, s, code, sig, m.now()); err != nil {
		return err
	}
	return nil
}

// launchArgv wraps the provider argv so the pane process starts with exactly
// the allowlisted environment: variables named by PermittedEnvironment,
// the project-relative assets folder, the browser-sidecar endpoint, provider
// additions, and per-launch capture values. Nothing else crosses from the service environment.
//
// env(1) is not a shell: fixed executable, fixed argv, nothing parsed,
// and it execs the provider in place so the pane pid and start
// time are the provider's own. The wrapper also strips what tmux itself
// injects (TMUX, TMUX_PANE); the Phase 0 contracts were measured with those
// present, so the acceptance suite revalidates real CLIs under
// this launch shape.
//
// TERM is special-cased: it is never carried from the service — the
// service's TERM describes the service's terminal (often `dumb` under a
// supervisor), which would strip a TUI of color and cursor-control capabilities.
// The pane's TERM is tmux's default-terminal, supplied by the caller, even
// when a descriptor lists TERM as permitted.
func launchArgv(desc provider.Descriptor, argv []string, term string, extraEnv []string) []string {
	wrapped := []string{"/usr/bin/env", "-i", "TERM=" + term, "NOISEDECK_ASSETS_DIR=" + project.AssetsFolder}
	overrides := make(map[string]struct{}, len(extraEnv))
	for _, assignment := range extraEnv {
		if name, _, ok := strings.Cut(assignment, "="); ok {
			overrides[name] = struct{}{}
		}
	}
	for _, name := range desc.PermittedEnvironment() {
		if name == "TERM" {
			continue
		}
		if _, overridden := overrides[name]; overridden {
			continue
		}
		if v, ok := os.LookupEnv(name); ok {
			wrapped = append(wrapped, name+"="+v)
		}
	}
	if browserWS, ok := os.LookupEnv("NOISE_BROWSER_WS"); ok {
		wrapped = append(wrapped, "NOISE_BROWSER_WS="+browserWS)
	}
	for _, assignment := range desc.Environment() {
		name, _, ok := strings.Cut(assignment, "=")
		if ok {
			if _, overridden := overrides[name]; overridden {
				continue
			}
		}
		wrapped = append(wrapped, assignment)
	}
	wrapped = append(wrapped, extraEnv...)
	return append(wrapped, argv...)
}

// sessionIdentityEnv names the session to the harness it launches. The
// ids are not secrets; they let the a2a client derive the same agent
// handle the console derives, on every launch, not only under
// the capacity-routing experiment.
func sessionIdentityEnv(projectID, sessionID string) []string {
	return []string{
		"NOISEDECK_PROJECT_ID=" + projectID,
		"NOISEDECK_SESSION_ID=" + sessionID,
	}
}

func (m *Manager) unconfinedControlEnv(projectID, sessionID string) []string {
	if m.unconfinedControlURL == "" {
		return nil
	}
	return []string{
		"NOISEDECK_UNCONFINED_CAPACITY_ROUTING=1",
		"NOISEDECK_CONTROL_URL=" + m.unconfinedControlURL,
		"NOISEDECK_EXPERIMENT_CAPABILITY=" + m.unconfinedControlCapability,
		"NOISEDECK_PROJECT_ID=" + projectID,
		"NOISEDECK_SESSION_ID=" + sessionID,
	}
}

// declaresStructuredHookCapture reports whether a descriptor declares the
// structured-native-hook identity capture mechanism. Only the provider's own
// declaration counts; callers never invent a capture path.
func declaresStructuredHookCapture(desc provider.Descriptor) bool {
	captures, ok := desc.(provider.IdentityCaptureDescriptor)
	if !ok {
		return false
	}
	for _, mechanism := range captures.IdentityCaptureMechanisms() {
		if mechanism == provider.IdentityCaptureStructuredNativeHook {
			return true
		}
	}
	return false
}

// provisionedHookSettingsPath names the per-generation hook settings file for
// a provider whose hook registration Deck injects at launch. Empty when the
// provider does not use one or no temporary root is configured — the launch
// then proceeds without the hook rather than refusing, matching the policy
// that a hook that cannot deliver costs tracking and nothing else.
func (m *Manager) provisionedHookSettingsPath(desc provider.Descriptor, sessionID string,
	generation int, marker string) string {
	provisioned, ok := desc.(provider.ProvisionedHookSettingsDescriptor)
	if !ok || m.tempRoot == "" {
		return ""
	}
	return filepath.Join(m.tempRoot, generationTempName(sessionID, generation, marker),
		provisioned.HookSettingsFileName())
}

// writeProvisionedHookSettings writes the provider's hook settings document at
// the already-named path inside the prepared generation directory. The file is
// owner-only like everything else under the temporary root; its content is the
// descriptor's exact document, never user configuration.
func writeProvisionedHookSettings(desc provider.Descriptor, path string) error {
	if path == "" {
		return nil
	}
	provisioned, ok := desc.(provider.ProvisionedHookSettingsDescriptor)
	if !ok {
		return nil
	}
	if err := os.WriteFile(path, provisioned.HookSettingsDocument(), 0o600); err != nil {
		return fmt.Errorf("%w: write provisioned hook settings: %w",
			provider.ErrHookNotProvisioned, err)
	}
	return nil
}

// expectedExecutable derives the kernel executable-path expectation at
// launch. For a native provider the launch path is a stable symlink to a
// version-stamped binary, so the expectation must be resolved NOW —
// configuring it would make the first provider upgrade look like an identity
// mismatch (docs/provider-contracts/claude.yaml). For an interpreted provider
// the kernel executable is the interpreter, as the descriptor states.
func expectedExecutable(desc provider.Descriptor, launchPath string) (string, error) {
	ident := desc.ExecutableIdentity()
	if ident.Interpreted {
		return ident.ExpectedProcExe, nil
	}
	resolved, err := filepath.EvalSymlinks(launchPath)
	if err != nil {
		return "", err
	}
	return resolved, nil
}

// executableRunnable reports whether the provider's configured CLI exists and
// is runnable. Descriptors hold absolute executable paths, so a stat failure
// means the CLI is not installed; the caller surfaces it as an actionable
// provider_not_launchable refusal instead of a generic 500 after launch.
func executableRunnable(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("executable %s not found: %w", path, err)
	}
	if info.IsDir() {
		return fmt.Errorf("executable %s is a directory", path)
	}
	if info.Mode()&0o111 == 0 {
		return fmt.Errorf("executable %s is not runnable", path)
	}
	return nil
}

// evidence maps a dead pane's fields to the exclusive code-or-signal shape.
// tmux reports exactly one depending on whether the CLI handled the signal
// itself (measured: Claude handles SIGTERM and exits 143 with an empty
// signal; a process dying FROM the signal reports the opposite shape).
func evidence(info *tmux.PaneInfo) (code *int, signal *string) {
	if info.ExitSignal != nil {
		s := strconv.Itoa(*info.ExitSignal)
		return nil, &s
	}
	return info.ExitStatus, nil
}

// Removal refusal sentinels, each with its own wire code: the launch
// window (ErrNotRunning), an unverifiable liveness check (ErrUnverifiable),
// and a process that survives even the force signal (ErrProcessAlive).
var (
	ErrUnverifiable = errors.New("kill unverifiable")
	ErrProcessAlive = errors.New("process alive without pane")
)

const removalExecutionStale = 20 * time.Second

// Remove deletes a session at your request. Two cases:
//
//   - With a verified current controller — a live pane whose complete
//     recorded identity tuple matches — ordinary stop-and-remove: the tmux
//     pane and scrollback are killed, any surviving process is signalled
//     against its exact pid+start-ticks identity, and the record is stamped
//     removed.
//   - Without a verified current controller — the pane is gone, dead,
//     foreign, or the row is already terminal — record-only removal: the
//     database row is stamped removed with zero effects on any old or
//     unverified runtime.
//
// There is no conflict refusal and no separate discard/forget gesture.
// A foreign or unverifiable pane is never signalled; it is simply left
// untouched while the Deck record is retired.
func (m *Manager) Remove(ctx context.Context, id string) error {
	gate := m.gate.Session(id)
	gate.Lock()
	defer gate.Unlock()
	if m.lifecycleHooks != nil && m.lifecycleHooks.removeGated != nil {
		m.lifecycleHooks.removeGated()
	}

	waitDeadline := time.Now().Add(recoveryGateBound)
	for tries := 0; ; tries++ {
		s, err := m.repo.Get(ctx, id)
		if err != nil {
			return err
		}
		if s.RemovedAt != nil {
			attempt, err := m.repo.OpenRemovalAttempt(ctx, id)
			if err != nil || attempt == nil {
				return err
			}
			staleBefore := time.Now().Add(-removalExecutionStale)
			if err := m.completeRemovalAttempt(ctx, attempt, &staleBefore); err != nil {
				if errors.Is(err, ErrControllerActionInProgress) && time.Now().Before(waitDeadline) {
					select {
					case <-ctx.Done():
						return ctx.Err()
					case <-time.After(25 * time.Millisecond):
						continue
					}
				}
				return err
			}
			return nil
		}

		// Only a freshly verified live pane on this manager's exact socket is
		// the current controller. Socket mismatch, absence, a dead pane,
		// observation failure, and identity drift are all record-only.
		verifiedController := false
		if s.TmuxSocketPath == m.tmux.SocketPath() && s.TmuxPaneID != nil {
			info, observeErr := m.tmux.PaneInfoByID(ctx, *s.TmuxPaneID)
			if observeErr == nil && VerifyIdentity(s, info) == nil && !info.Dead {
				verifiedController = true
			}
		}
		nonce, err := m.newID()
		if err != nil {
			return err
		}
		applied, err := m.repo.BeginRemovalAttempt(ctx, s, nonce, verifiedController)
		if err != nil {
			if (errors.Is(err, ErrRecoveryInProgress) ||
				errors.Is(err, ErrControllerActionInProgress)) && time.Now().Before(waitDeadline) {
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(25 * time.Millisecond):
					continue
				}
			}
			return err
		}
		if applied {
			attempt, err := m.repo.OpenRemovalAttempt(ctx, id)
			if err != nil || attempt == nil {
				return err
			}
			staleBefore := time.Now().Add(-removalExecutionStale)
			return m.completeRemovalAttempt(ctx, attempt, &staleBefore)
		}
		if tries >= 8 {
			return fmt.Errorf("session %s: removal lost repeated controller-state races", id)
		}
	}
}

// completeRemovalAttempt performs effects only after the durable removal
// claim has retired the exact source row. A crash re-enters this function at
// boot; every observation is fresh and every effect is identity-CAS guarded.
func (m *Manager) completeRemovalAttempt(ctx context.Context, attempt *RemovalAttempt,
	staleBefore *time.Time) error {
	executorToken, err := m.newID()
	if err != nil {
		return err
	}
	claimed, err := m.repo.ClaimRemovalExecution(ctx, attempt, executorToken, staleBefore)
	if err != nil {
		return err
	}
	if !claimed {
		return ErrControllerActionInProgress
	}
	if m.lifecycleHooks != nil && m.lifecycleHooks.removeExecuting != nil {
		m.lifecycleHooks.removeExecuting()
	}
	s, err := m.repo.Get(ctx, attempt.SessionID)
	if err != nil {
		return err
	}
	stopped := attempt.ProcessStopCompleted
	captureRevocable := false
	if attempt.ControllerVerified && s.TmuxSocketPath == m.tmux.SocketPath() &&
		s.TmuxPaneID != nil {
		info, observeErr := m.tmux.PaneInfoByID(ctx, *s.TmuxPaneID)
		if observeErr != nil && !errors.Is(observeErr, tmux.ErrNoSession) &&
			!errors.Is(observeErr, tmux.ErrNoServer) {
			// The process-stop fact does not prove the pane or its scrollback is
			// gone. Keep the durable removal executor open so a later bounded
			// convergence can finish the exact pane after tmux becomes observable.
			return fmt.Errorf("session %s: observe claimed removal pane: %w: %w",
				s.ID, observeErr, ErrUnverifiable)
		}
		exact := observeErr == nil && VerifyIdentity(s, info) == nil
		if stopped && errors.Is(observeErr, tmux.ErrNoSession) {
			captureRevocable = true
		}
		if exact && !info.Dead && !attempt.ProcessStopCompleted {
			if owned, ownErr := m.repo.RefreshRemovalExecution(ctx, attempt); ownErr != nil || !owned {
				return errors.Join(ErrControllerActionInProgress, ownErr)
			}
			// Process-facing effects occur only while the complete pane identity
			// still verifies. Pane removal comes last, so a crash can never turn
			// absence into authority for a replayed signal.
			ownershipFence := func(ctx context.Context) error {
				if m.lifecycleHooks != nil && m.lifecycleHooks.removeBeforeSignal != nil {
					m.lifecycleHooks.removeBeforeSignal()
				}
				owned, ownErr := m.repo.RefreshRemovalExecution(ctx, attempt)
				if ownErr != nil || !owned {
					return errors.Join(ErrControllerActionInProgress, ownErr)
				}
				return nil
			}
			if err := m.terminateRecordedProcess(ctx, s, AuditStopRequested,
				"removal escalation", ownershipFence); err != nil {
				return err
			}
			marked, markErr := m.repo.RecordRemovalProcessStopped(ctx, attempt)
			if markErr != nil || !marked {
				return errors.Join(ErrControllerActionInProgress, markErr)
			}
			stopped = true
			if m.lifecycleHooks != nil && m.lifecycleHooks.removeProcessStopped != nil {
				m.lifecycleHooks.removeProcessStopped()
			}
			info, observeErr = m.tmux.PaneInfoByID(ctx, *s.TmuxPaneID)
			if observeErr != nil && !errors.Is(observeErr, tmux.ErrNoSession) &&
				!errors.Is(observeErr, tmux.ErrNoServer) {
				return fmt.Errorf("session %s: reobserve stopped removal pane: %w: %w",
					s.ID, observeErr, ErrUnverifiable)
			}
			exact = observeErr == nil && VerifyIdentity(s, info) == nil
			if stopped && errors.Is(observeErr, tmux.ErrNoSession) {
				captureRevocable = true
			}
		}
		if exact && info.Dead && !attempt.ProcessStopCompleted {
			if err := recordedProcessGroupGone(s); err != nil {
				// The live leader is gone, so the remaining numeric process group
				// cannot safely authorize a signal. Finish as record-only and leave
				// both the pane and any surviving or reused group untouched.
				exact = false
			} else {
				if owned, ownErr := m.repo.RefreshRemovalExecution(ctx, attempt); ownErr != nil || !owned {
					return errors.Join(ErrControllerActionInProgress, ownErr)
				}
				marked, markErr := m.repo.RecordRemovalProcessStopped(ctx, attempt)
				if markErr != nil || !marked {
					return errors.Join(ErrControllerActionInProgress, markErr)
				}
				stopped = true
			}
		}
		if exact && info.Dead {
			if owned, ownErr := m.repo.RefreshRemovalExecution(ctx, attempt); ownErr != nil || !owned {
				return errors.Join(ErrControllerActionInProgress, ownErr)
			}
			applied, killErr := m.tmux.KillPaneIfIdentity(ctx, tmux.PaneIdentity{
				SessionID: info.SessionID, PaneID: info.PaneID, PanePID: info.PID,
				Generation: info.Generation, Marker: info.Marker,
				LaunchMarker: info.LaunchMarker,
			})
			if killErr != nil && !errors.Is(killErr, tmux.ErrNoSession) {
				return fmt.Errorf("session %s: kill tmux pane: %w", s.ID, killErr)
			}
			if killErr == nil && applied {
				captureRevocable = true
				if m.lifecycleHooks != nil && m.lifecycleHooks.removePaneKilled != nil {
					m.lifecycleHooks.removePaneKilled()
				}
			} else if errors.Is(killErr, tmux.ErrNoSession) {
				captureRevocable = true
			}
		}
		if exact && !info.Dead && attempt.ProcessStopCompleted {
			return fmt.Errorf("session %s: removed controller reappeared after a durable stop: %w",
				s.ID, ErrUnverifiable)
		}
	}
	finished, err := m.repo.FinishRemovalAttempt(ctx, attempt, stopped)
	if err != nil {
		return err
	}
	if !finished {
		return ErrControllerActionInProgress
	}
	if stopped && captureRevocable && s.IdentityMarker != nil {
		return m.revokeCaptureFenced(ctx, s.ID, s.Generation, *s.IdentityMarker)
	}
	return nil
}

// ConvergeRemovalAttempts completes crash-left removal claims before normal
// startup reconciliation can act on their panes.
func (m *Manager) ConvergeRemovalAttempts(ctx context.Context) error {
	attempts, err := m.repo.ListOpenRemovalAttempts(ctx)
	if err != nil {
		return err
	}
	for _, attempt := range attempts {
		for {
			cutoff := time.Now().Add(-removalExecutionStale)
			err := m.completeRemovalAttempt(ctx, attempt, &cutoff)
			if err == nil {
				break
			}
			if !errors.Is(err, ErrControllerActionInProgress) {
				return err
			}
			fresh, readErr := m.repo.OpenRemovalAttempt(ctx, attempt.SessionID)
			if readErr != nil {
				return readErr
			}
			if fresh == nil {
				break
			}
			attempt = fresh
			wait := time.Until(attempt.UpdatedAt.Add(removalExecutionStale))
			if wait < 25*time.Millisecond {
				wait = 25 * time.Millisecond
			}
			timer := time.NewTimer(wait)
			select {
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			case <-timer.C:
			}
		}
	}
	return nil
}

// terminateRecordedProcess kills the CLI tmux could not. The recorded
// pid+start-ticks tuple is the identity check: only a process that IS the
// recorded one is signalled, first with the provider's graceful signal,
// then its force signal. Returns nil when the process is verified gone (or
// no identity was recorded), ErrUnverifiable when liveness cannot be
// checked, and ErrProcessAlive when even the force signal fails.
func (m *Manager) terminateRecordedProcess(ctx context.Context, s *Session,
	auditType, auditDetail string, beforeSignal func(context.Context) error) error {
	if s.PanePID == nil || s.StartTicks == nil {
		return nil
	}
	// A stale boot epoch proves the recorded process cannot exist under
	// this boot — nothing crosses a host restart — and makes its tuple
	// meaningless: there is nothing to signal, and a coincidental
	// pid/start-ticks match must never be signalled.
	if err := VerifyBootEpoch(s); err != nil {
		if errors.Is(err, ErrBootEpochStale) {
			return nil
		}
		return fmt.Errorf("session %s: process liveness unverifiable: %w", s.ID, ErrUnverifiable)
	}
	alive, err := tmux.ProcAlive(*s.PanePID, *s.StartTicks)
	if err != nil {
		return fmt.Errorf("session %s: process liveness unverifiable: %w", s.ID, ErrUnverifiable)
	}
	if !alive {
		return recordedProcessGroupGone(s)
	}

	graceful, force := syscall.SIGTERM, syscall.SIGKILL
	if desc, ok := m.providers[provider.Name(s.Provider)]; ok {
		graceful, force = desc.GracefulStopSignal(), desc.ForceStopSignal()
	}
	for _, sig := range []syscall.Signal{graceful, force} {
		// A cancelled removal stops BEFORE the next escalation: a graceful
		// signal racing a cancel must never upgrade into a force signal the
		// user's request did not outlive.
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("session %s: removal interrupted before escalation: %w: %w",
				s.ID, ErrUnverifiable, err)
		}
		if s.TmuxPaneID == nil {
			return fmt.Errorf("session %s: pane identity unavailable before signal: %w",
				s.ID, ErrUnverifiable)
		}
		// A removal executor refreshes its durable lease before each final
		// complete-identity preflight. The refresh-to-signal interval is then
		// bounded by one tmux observation, well below removalExecutionStale.
		if beforeSignal != nil {
			if err := beforeSignal(ctx); err != nil {
				return err
			}
		}
		pane, paneErr := m.tmux.PaneInfoByID(ctx, *s.TmuxPaneID)
		if paneErr != nil || VerifyIdentity(s, pane) != nil || pane.Dead {
			if paneErr == nil && pane != nil && pane.Dead && VerifyIdentity(s, pane) == nil {
				return recordedProcessGroupGone(s)
			}
			return fmt.Errorf("session %s: complete pane identity unavailable before signal: %w",
				s.ID, ErrUnverifiable)
		}
		// The signal targets the process's CURRENT group, read from
		// /proc under the same tuple verification — the recorded pgid cannot
		// prove it still names this process's group. Nothing but the kill
		// sits between verification and signal: any I/O in that window is a
		// recycling opportunity.
		pgid, err := m.procGroup(*s.PanePID, *s.StartTicks)
		if err != nil {
			// The verified-alive process from the entry check is gone or
			// unreadable; either way removal cannot proceed on a guess.
			return fmt.Errorf("session %s: process liveness unverifiable: %w", s.ID, ErrUnverifiable)
		}
		if pgid != pane.ProcessGroupID || s.ProcessGroupID == nil || pgid != *s.ProcessGroupID {
			return fmt.Errorf("session %s: process group changed at signal boundary: %w",
				s.ID, ErrUnverifiable)
		}
		if err := syscall.Kill(-pgid, sig); err != nil {
			return fmt.Errorf("session %s: signal recorded process: %w", s.ID, err)
		}
		// The audit follows the signal: it records what happened, never an
		// intention, and a cancellation there is an interrupted removal,
		// not a bare context error.
		if err := m.repo.AppendEvent(ctx, s.ID, auditType,
			fmt.Sprintf("%s: signal=%d pgid=%d", auditDetail, sig, pgid)); err != nil {
			if cerr := ctx.Err(); cerr != nil {
				return fmt.Errorf("session %s: removal interrupted after the signal: %w: %w",
					s.ID, ErrUnverifiable, cerr)
			}
			return err
		}
		deadline := time.Now().Add(3 * time.Second)
		for {
			alive, err := tmux.ProcAlive(*s.PanePID, *s.StartTicks)
			if err != nil {
				return fmt.Errorf("session %s: process liveness unverifiable: %w", s.ID, ErrUnverifiable)
			}
			if !alive {
				return recordedProcessGroupGone(s)
			}
			if err := ctx.Err(); err != nil {
				return fmt.Errorf("session %s: removal interrupted while verifying the signal: %w: %w",
					s.ID, ErrUnverifiable, err)
			}
			if time.Now().After(deadline) {
				break
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("session %s: removal interrupted before the final verdict: %w: %w",
			s.ID, ErrUnverifiable, err)
	}
	return fmt.Errorf("session %s: the CLI survived the force signal; stop it first: %w",
		s.ID, ErrProcessAlive)
}

func recordedProcessGroupGone(s *Session) error {
	if s == nil || s.ProcessGroupID == nil || *s.ProcessGroupID <= 1 {
		return fmt.Errorf("session: recorded process group is unavailable: %w", ErrUnverifiable)
	}
	members, err := tmux.GroupMembers(*s.ProcessGroupID)
	if err != nil {
		return fmt.Errorf("session %s: enumerate recorded process group: %w: %w",
			s.ID, err, ErrUnverifiable)
	}
	if len(members) != 0 {
		return fmt.Errorf("session %s: recorded process leader ended with group members remaining: %w",
			s.ID, ErrUnverifiable)
	}
	return nil
}

func (m *Manager) Stop(ctx context.Context, id string, force bool) error {
	gate := m.gate.Session(id)
	gate.Lock()
	defer gate.Unlock()

	s, err := m.repo.Get(ctx, id)
	if err != nil {
		return err
	}
	if s.Status != StatusRunning || s.TmuxPaneID == nil {
		return fmt.Errorf("session %s in %s: %w", id, s.Status, ErrNotRunning)
	}

	desc, ok := m.providers[provider.Name(s.Provider)]
	if !ok {
		return fmt.Errorf("session %s: unknown provider %q", id, s.Provider)
	}

	if s.TmuxSocketPath != m.tmux.SocketPath() {
		return fmt.Errorf("session %s: recorded on socket %q, manager owns %q: %w",
			id, s.TmuxSocketPath, m.tmux.SocketPath(), ErrIdentityMismatch)
	}

	info, err := m.tmux.PaneInfoByID(ctx, *s.TmuxPaneID)
	if err != nil {
		// A missing pane on a live server proves the process is gone, but
		// with the pane went the exit evidence — that is `interrupted`
		// territory and the reconciler's call, not a stop.
		return fmt.Errorf("session %s: observe pane: %w", id, err)
	}
	if err := VerifyIdentity(s, info); err != nil {
		return err
	}

	nonce, err := m.newID()
	if err != nil {
		return err
	}
	attempt, err := m.repo.BeginStopAttempt(ctx, s, nonce, force)
	if err != nil {
		return err
	}
	finish := func(outcome string, preserveStopRequest bool) error {
		fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), stopDetachedWriteTimeout)
		defer cancel()
		return m.repo.FinishStopAttempt(fctx, attempt, outcome, preserveStopRequest)
	}
	executing, err := m.repo.MarkStopExecuting(ctx, attempt)
	if err != nil {
		return err
	}
	if !executing {
		if ferr := finish("stop superseded before the signal boundary", false); ferr != nil {
			return ferr
		}
		return ErrIdentityMismatch
	}

	// Re-observe after the durable execution claim. Removal cannot cross this
	// state, and no stale pre-claim pane or process tuple authorizes a signal.
	current, err := m.repo.Get(ctx, id)
	if err != nil {
		return errors.Join(err, finish("stop could not reload the claimed controller", false))
	}
	if current.RemovedAt != nil || current.Status != StatusRunning ||
		current.Generation != attempt.SourceGeneration ||
		current.IdentityMarker == nil || *current.IdentityMarker != attempt.SourceIdentityMarker ||
		current.ControllerRevision != attempt.SourceControllerRevision || current.TmuxPaneID == nil {
		return errors.Join(ErrIdentityMismatch,
			finish("stop completed without a signal because the claimed controller changed", false))
	}
	info, err = m.tmux.PaneInfoByID(ctx, *current.TmuxPaneID)
	if err != nil {
		return errors.Join(fmt.Errorf("session %s: reobserve pane before stop: %w", id, err),
			finish("stop completed without a signal because attachment was unavailable", false))
	}
	if err := VerifyIdentity(current, info); err != nil {
		return errors.Join(err, finish("stop completed without a signal because identity changed", false))
	}
	if info.Dead {
		code, sig := evidence(info)
		var recordErr error
		if code == nil && sig == nil {
			_, recordErr = m.repo.MarkDeadInterruptedObserved(ctx, current,
				"exact controller pane was already dead without exit evidence")
		} else {
			_, recordErr = m.repo.RecordExitObserved(ctx, current, code, sig, m.now())
		}
		result := errors.Join(recordErr,
			finish("stop observed the exact controller already ended", true))
		if result == nil {
			result = m.revokeCaptureFenced(ctx, current.ID, current.Generation, *current.IdentityMarker)
		}
		return result
	}
	alive, err := tmux.ProcAlive(info.PID, info.StartTicks)
	if err != nil || !alive {
		if err == nil {
			err = ErrIdentityMismatch
		}
		return errors.Join(err, finish("stop completed without a signal because process identity was unavailable", false))
	}
	pgid, err := m.procGroup(info.PID, info.StartTicks)
	if err != nil || pgid != info.ProcessGroupID {
		if err == nil {
			err = ErrIdentityMismatch
		}
		return errors.Join(err, finish("stop completed without a signal because process group changed", false))
	}
	sig := desc.GracefulStopSignal()
	if force {
		sig = desc.ForceStopSignal()
	}
	if err := syscall.Kill(-pgid, sig); err != nil {
		return errors.Join(fmt.Errorf("session %s: signal process group: %w", id, err),
			finish("stop signal failed at the exact process boundary", false))
	}
	if m.lifecycleHooks != nil && m.lifecycleHooks.stopSignaled != nil {
		m.lifecycleHooks.stopSignaled()
	}

	err = m.awaitExit(ctx, current)
	finishErr := finish("stop signal attempt completed", true)
	if finishErr != nil {
		return errors.Join(err, finishErr)
	}
	if err == nil {
		err = m.revokeCaptureFenced(ctx, current.ID, current.Generation, *current.IdentityMarker)
	}
	return err
}

// ConvergeStopAttempts closes crash-left signal boundaries without replaying a
// signal whose delivery is uncertain. Normal reconciliation observes whatever
// the exact controller did; a later explicit stop is a new gesture.
func (m *Manager) ConvergeStopAttempts(ctx context.Context) error {
	attempts, err := m.repo.ListOpenStopAttempts(ctx)
	if err != nil {
		return err
	}
	for _, attempt := range attempts {
		outcome := "stop claim ended before its signal boundary"
		if attempt.State == stopExecuting {
			outcome = "stop signal outcome was uncertain after restart; signal was not replayed"
		}
		preserveStopRequest := attempt.State == stopExecuting
		if err := m.repo.FinishStopAttempt(ctx, attempt, outcome, preserveStopRequest); err != nil {
			return err
		}
	}
	return nil
}

// awaitExit polls the pane until it is dead or ctx expires, then records
// what was OBSERVED — never what was hoped. The deadline is checked before
// every observation, and the unobserved-stop audit runs on a detached,
// bounded context: the record of "we signalled and saw no exit" must not be
// lost to the very deadline that produced it.
func (m *Manager) awaitExit(ctx context.Context, source *Session) error {
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		if ctx.Err() != nil {
			return m.recordUnobserved(ctx, source.ID, "signal delivered; no exit observed at deadline")
		}
		info, err := m.tmux.PaneInfoByID(ctx, *source.TmuxPaneID)
		if err != nil {
			if ctx.Err() != nil {
				return m.recordUnobserved(ctx, source.ID, "deadline expired before the pane could be observed after the signal")
			}
			return fmt.Errorf("session %s: observe pane after signal: %w", source.ID, err)
		}
		if err := VerifyIdentity(source, info); err != nil {
			return m.recordUnobserved(ctx, source.ID,
				"signal delivered; pane identity changed before an exact exit was observed")
		}
		if info.Dead {
			return m.recordObservedPaneExit(ctx, source, info)
		}
		select {
		case <-ctx.Done():
			return m.recordUnobserved(ctx, source.ID, "signal delivered; pane still alive at deadline")
		case <-tick.C:
		}
	}
}

// recordUnobserved appends the stop_unobserved audit on a context detached
// from the caller's expired deadline but still bounded, so the write can
// neither be dropped nor hang.
func (m *Manager) recordUnobserved(ctx context.Context, id, detail string) error {
	actx, cancel := context.WithTimeout(context.WithoutCancel(ctx), stopDetachedWriteTimeout)
	defer cancel()
	if aerr := m.repo.AppendEvent(actx, id, AuditStopUnobserved, detail); aerr != nil {
		return errors.Join(ErrStopUnobserved, errStopUnobservedAudit, aerr)
	}
	return ErrStopUnobserved
}

// recordObservedPaneExit maps one exact dead controller's preserved evidence
// through a full-row CAS. A pane-ID replacement can never settle the source
// generation's lifecycle even if it dies before the next poll.
func (m *Manager) recordObservedPaneExit(ctx context.Context, source *Session,
	info *tmux.PaneInfo) error {
	if err := VerifyIdentity(source, info); err != nil {
		return err
	}
	var (
		applied bool
		err     error
	)
	if info.ExitStatus == nil && info.ExitSignal == nil {
		applied, err = m.repo.MarkDeadInterruptedObserved(ctx, source,
			"pane is dead but tmux reported no exit status or signal")
	} else {
		code, sig := evidence(info)
		applied, err = m.repo.RecordExitObserved(ctx, source, code, sig, m.now())
	}
	if err != nil {
		return err
	}
	if applied {
		return nil
	}

	// A concurrent reconciler may have committed the same exact pane's death.
	// Accept only that same generation; a successor or metadata rewrite never
	// turns this stale stop observer into authority over a later controller.
	cur, err := m.repo.Get(ctx, source.ID)
	if err != nil {
		return err
	}
	sameGeneration := cur.Generation == source.Generation && cur.IdentityMarker != nil &&
		source.IdentityMarker != nil && *cur.IdentityMarker == *source.IdentityMarker
	if sameGeneration && (cur.Status == StatusExited || cur.Status == StatusInterrupted) {
		return nil
	}
	return fmt.Errorf("session %s: exact exit not recorded; controller moved to generation %d status %s: %w",
		source.ID, cur.Generation, cur.Status, ErrIdentityMismatch)
}

// VerifyIdentity compares the observed pane against the recorded tuple.
// Every member matters because no single member sees everything — markers
// survive an out-of-band respawn, while pid and start ticks move.
//
// A DEAD pane is compared only on the members death preserves: the tmux
// session id, pane id, pane options (marker, generation), and pane pid. Tmux
// retains the pid that exited; a different pid proves an out-of-band respawn
// even after that replacement also dies. Process group, start ticks, and the
// kernel executable path are live-only and cannot be revalidated after death.
//
// The recorded boot epoch is not a live-only member: it is a claim about
// which boot recorded the row, so it is verified for dead and live panes
// alike. A dead pane whose row predates the current boot cannot be the
// recorded workload's honest corpse in every topology — a database
// restored onto another host can collide with this tmux namespace — so it
// is a mismatch, never exit evidence. For a live pane the
// epoch also guards the boot-relative members: across a host restart a
// pid/start-ticks match can name an unrelated process, and a stale epoch
// fails the comparison before any of them is trusted.
func VerifyIdentity(s *Session, info *tmux.PaneInfo) error {
	mismatch := func(field string, want, got any) error {
		return fmt.Errorf("%w: %s recorded %v, observed %v", ErrIdentityMismatch, field, want, got)
	}
	if s.TmuxSessionID == nil || *s.TmuxSessionID == "" || info.SessionID == "" ||
		info.SessionID != *s.TmuxSessionID {
		return mismatch("tmux_session_id", deref(s.TmuxSessionID), info.SessionID)
	}
	if s.TmuxPaneID == nil || *s.TmuxPaneID == "" || info.PaneID == "" ||
		info.PaneID != *s.TmuxPaneID {
		return mismatch("tmux_pane_id", deref(s.TmuxPaneID), info.PaneID)
	}
	if s.IdentityMarker == nil || *s.IdentityMarker == "" || info.Marker == "" ||
		info.Marker != *s.IdentityMarker {
		return mismatch("identity_marker", deref(s.IdentityMarker), info.Marker)
	}
	if s.Generation < 1 || info.Generation < 1 || info.Generation != s.Generation {
		return mismatch("generation", s.Generation, info.Generation)
	}
	if err := VerifyBootEpoch(s); err != nil {
		return err
	}
	if info.Dead {
		if s.PanePID == nil || *s.PanePID <= 1 || info.PID <= 1 || info.PID != *s.PanePID {
			return mismatch("pane_pid", derefInt(s.PanePID), info.PID)
		}
		return nil
	}
	if s.PanePID == nil || *s.PanePID <= 1 || info.PID <= 1 || info.PID != *s.PanePID {
		return mismatch("pane_pid", derefInt(s.PanePID), info.PID)
	}
	if s.StartTicks == nil || *s.StartTicks <= 0 || info.StartTicks <= 0 ||
		info.StartTicks != *s.StartTicks {
		return mismatch("process_start_ticks", derefInt64(s.StartTicks), info.StartTicks)
	}
	if s.ProcessGroupID == nil || *s.ProcessGroupID <= 1 || info.ProcessGroupID <= 1 ||
		info.ProcessGroupID != *s.ProcessGroupID {
		return mismatch("process_group_id", derefInt(s.ProcessGroupID), info.ProcessGroupID)
	}
	// The kernel executable is deliberately NOT compared. Pane id, marker,
	// generation, pid, start ticks, and boot epoch already identify one exact
	// process; a user upgrading the runtime under a live session changes
	// the path without changing which process this is. Comparing it here would
	// let a routine `brew upgrade node` mark every running session interrupted.
	return nil
}
