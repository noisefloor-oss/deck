package provider

import (
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"syscall"
)

// Name identifies a provider CLI.
type Name string

const (
	Claude      Name = "claude"
	Cline       Name = "cline"
	CommandCode Name = "command-code"
	Codex       Name = "codex"
	Cursor      Name = "cursor"
	Devin       Name = "devin"
	Grok        Name = "grok"
	Kimi        Name = "kimi"
	Kilo        Name = "kilo"
	Mimo        Name = "mimo"
	Omp         Name = "omp"
	Opencode    Name = "opencode"
	Pi          Name = "pi"
	Qoder       Name = "qoder"
	Qwen        Name = "qwen"
	Shell       Name = "shell"
)

// IdentityContract describes how a provider's exact native session identity is
// established.
type IdentityContract int

const (
	// IdentityUnavailable means there is no documented way to assign or
	// retrieve an exact native session ID. Such a provider fails Phase 0 and
	// is not first-release supported.
	IdentityUnavailable IdentityContract = iota
	// IdentityAssigned means Deck supplies the ID at launch, which is
	// the strongest form: the ID is known before the process starts.
	IdentityAssigned
	// IdentityRetrieved means a documented CLI command returns the ID of an
	// existing session.
	IdentityRetrieved
	// IdentityNone means the provider has no identity BY CONTRACT: the shell
	// provider. It is supported, never resumable, and owes no native session
	// ID — distinct from IdentityUnavailable, which marks a provider that
	// failed Phase 0.
	IdentityNone
	// IdentityFreshRelaunch is an explicit agent-provider contract: the CLI
	// has provider-owned state, but Deck intentionally does not claim native
	// conversation resume. Recovery starts a new conversation with fresh
	// launch argv and never replays input.
	IdentityFreshRelaunch
)

func (c IdentityContract) String() string {
	switch c {
	case IdentityAssigned:
		return "assigned-at-launch"
	case IdentityRetrieved:
		return "retrieved-by-command"
	case IdentityNone:
		return "none-by-contract"
	case IdentityFreshRelaunch:
		return "fresh-relaunch-only"
	default:
		return "unavailable"
	}
}

// IdentityCaptureMechanism names a trustworthy provider-native path for
// establishing the exact opaque identity of a newly launched session. The
// order returned by IdentityCaptureDescriptor is the provider's preference
// order; callers must not invent a fallback that the provider did not declare.
type IdentityCaptureMechanism string

const (
	// IdentityCaptureAssignedAtLaunch means Deck supplies the native identity
	// in the provider's launch argv before the process starts.
	IdentityCaptureAssignedAtLaunch IdentityCaptureMechanism = "assigned-at-launch"
	// IdentityCaptureStructuredNativeHook means a provider-owned hook or plugin
	// delivers a structured native identity to Deck's capture endpoint.
	IdentityCaptureStructuredNativeHook IdentityCaptureMechanism = "structured-native-hook"
	// IdentityCaptureHostAttestedZeroArgTool is reserved for a future
	// host-provisioned, zero-argument tool whose result is bound to the current
	// provider execution context. A model-supplied identity is never attested.
	IdentityCaptureHostAttestedZeroArgTool IdentityCaptureMechanism = "host-attested-zero-arg-tool"
	// IdentityCaptureDocumentedAPI is reserved for a provider's documented
	// identity API. No current provider has earned this declaration.
	IdentityCaptureDocumentedAPI IdentityCaptureMechanism = "documented-api"
)

// IdentityCaptureDescriptor is the provider-declared identity capture
// extension to Descriptor. Keeping it separate lets identity-less test and
// utility descriptors implement Descriptor without claiming a capture path.
type IdentityCaptureDescriptor interface {
	Descriptor
	IdentityCaptureMechanisms() []IdentityCaptureMechanism
}

// ResumeIdentityAttestationDescriptor is implemented only when a pinned,
// provider-documented structured hook distinguishes exact resume from a
// silent fresh-session fallback. The returned source is matched byte-for-byte;
// it is not provider output and Deck never invents an equivalent.
type ResumeIdentityAttestationDescriptor interface {
	Descriptor
	ExactResumeHookSource() string
}

// StartupResumeCaptureDescriptor is implemented by providers whose resume
// flight reports identity through the same process-launch callback a fresh
// launch uses — omp's session_start fires on --resume and carries the
// resumed id with a startup-shaped source. The returned source is matched
// byte-for-byte; Deck never renames a startup callback a resume.
type StartupResumeCaptureDescriptor interface {
	Descriptor
	ResumeCaptureHookSource() string
}

// StorePinningDescriptor is implemented by providers whose conversations live
// in a store the CLI resolves from its environment, so a resume can be made
// to consult the store the conversation actually wrote instead of whatever
// the current environment happens to resolve. The recorded store root comes
// from the provider's own structured hook (the transcript's ancestor), never
// from mining provider state.
// ErrStoreRootUnresolvable marks a CurrentStoreRoot resolution that has no
// evidence to offer: the environment names no store at all. It is a distinct
// condition rather than a plain error because it decides nothing — a caller
// must neither pin nor refuse on it — while any other resolution failure is a
// real fault that must propagate.
var ErrStoreRootUnresolvable = errors.New("provider: store root unresolvable")

type StorePinningDescriptor interface {
	Descriptor
	// CurrentStoreRoot resolves the store root the NEXT launch would read,
	// from the same environment values the launch allowlist would pass the
	// pane. An environment that names no store returns
	// ErrStoreRootUnresolvable.
	CurrentStoreRoot() (string, error)
	// StorePinEnvironment returns the environment assignments that pin a
	// launch to the recorded store root.
	StorePinEnvironment(recordedRoot string) []string
}

// ProvisionedHookSettingsDescriptor is implemented by providers whose identity
// hook registration is injected per launch through a Deck-provisioned settings
// file named in argv, rather than installed in the user's own provider
// config. The launch caller writes HookSettingsDocument to a Deck-owned
// per-generation path and hands that path back through
// LaunchSpec/ResumeSpec.HookSettingsPath; the user's ~/.claude and project
// .claude/settings.json are never written.
type ProvisionedHookSettingsDescriptor interface {
	Descriptor
	// HookSettingsDocument is the exact file content to provision. It must be
	// self-contained: the hook command reads its capture endpoint and
	// credential from the launched process's environment, never from the file.
	HookSettingsDocument() []byte
	// HookSettingsFileName is the base name the document is written under
	// inside the generation's Deck-owned directory.
	HookSettingsFileName() string
}

// InjectionContract records how terminal input must be delivered to a
// provider.
//
// These are DECLARATIONS, not measurements. An injection contract counts only
// once it has been tested against a pinned CLI version, and the test that earns
// that adjective is the Phase 0 conformance spike. Until Verified is
// true, nothing here has been demonstrated against the real CLI, and no release
// claim may rest on it.
type InjectionContract struct {
	UseBracketedPaste bool
	// SubmitKey is a tmux key name, sent separately from the body. Paste and
	// submit are distinct non-transactional operations in the native CLI, so
	// the acceptance gate sends them as separate keystrokes and a broken
	// control-key path cannot hide behind a printable echo.
	SubmitKey         string
	SupportsMultiline bool
	// AcceptsInputDuringTurn records what the CLI does with input submitted
	// while it is already working. Deck never holds or queues input on a
	// provider's behalf, so the CLI's own behaviour is the whole of it, and
	// the first-release gate measures it directly: ten nonce-tagged
	// submissions during an active turn must each appear exactly once. It is
	// unknowable without measurement, which is why it defaults to unknown
	// rather than to a guess.
	AcceptsInputDuringTurn ActiveTurnBehaviour
	// DiscardsInputDuringModal records that input delivered while a modal
	// prompt is open is thrown away rather than queued.
	//
	// Measured true for Claude 2.1.220: a bracketed paste sent while its
	// startup trust picker was open produced nothing at all — no input line,
	// no queue entry, no change to the modal — while tmux reported the write
	// as successful.
	//
	// This is the concrete reason the vertical-slice gate waits for a stable
	// ready screen before typing, and the reason Deck never infers
	// provider mode: a user typing into a modal is interacting with the
	// native TUI directly, and nothing may be queued on their behalf.
	DiscardsInputDuringModal bool
	Verified                 bool
}

// ActiveTurnBehaviour describes what a provider does with input submitted while
// a turn is already in flight.
type ActiveTurnBehaviour int

const (
	// ActiveTurnUnknown means it has not been measured. No release claim may
	// rest on a descriptor still reporting this.
	ActiveTurnUnknown ActiveTurnBehaviour = iota
	// ActiveTurnQueued means the CLI accepts the input and processes it after
	// the current turn.
	ActiveTurnQueued
	// ActiveTurnRejected means the CLI discards or refuses the input.
	ActiveTurnRejected
	// ActiveTurnInterrupts means the input interrupts the current turn.
	ActiveTurnInterrupts
)

func (b ActiveTurnBehaviour) String() string {
	switch b {
	case ActiveTurnQueued:
		return "queued"
	case ActiveTurnRejected:
		return "rejected"
	case ActiveTurnInterrupts:
		return "interrupts"
	default:
		return "unknown"
	}
}

// LaunchSpec is the input to NewSessionArgs.
type LaunchSpec struct {
	// WorkDir is the project directory the CLI runs in. It is supplied to tmux
	// as the pane's working directory rather than to the provider, so a
	// descriptor only uses it if its CLI needs an explicit flag.
	WorkDir string
	// SessionID is the identity Deck assigns. Providers whose contract
	// is IdentityAssigned embed it in argv.
	SessionID string
	// SkipPermissions launches the CLI in its own permission-skipping mode
	// (the user's console setting). Each descriptor names its OWN flag —
	// there is no generic one, and a descriptor whose CLI has no such flag
	// must say so rather than approximate it: launching without the mode the
	// user asked for is a smaller failure than launching with a flag that
	// means something else.
	SkipPermissions bool
	// HookSettingsPath is the Deck-provisioned hook settings file for
	// providers implementing ProvisionedHookSettingsDescriptor. Empty means
	// the launch caller could not provision one; such a launch proceeds with
	// identity tracking degraded rather than refused.
	HookSettingsPath string
}

// DelegatedLaunch is the fixed initial-prompt surface used only by the
// default-off unconfined capacity-routing experiment. It is deliberately separate
// from LaunchSpec so ordinary browser session creation cannot acquire an
// arbitrary prompt field by accident.
type DelegatedLaunch struct {
	Model  string
	Prompt string
}

// DelegationDescriptor is implemented only by providers whose real
// interactive CLI has a measured initial-prompt argv contract.
type DelegationDescriptor interface {
	Descriptor
	NewDelegatedSessionArgs(LaunchSpec, DelegatedLaunch) ([]string, error)
}

// delegatedModelPattern bounds the one argv element a caller names. The
// leading class keeps a model from being read as a flag, and the slash admits
// the namespaced ids providers like omp and kimi use ("zai/glm-5.3"); argv is
// passed without a shell, so no other separator is meaningful here.
var delegatedModelPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/\[\]-]{0,127}$`)

func validateDelegatedLaunch(spec DelegatedLaunch) error {
	if !delegatedModelPattern.MatchString(spec.Model) {
		return fmt.Errorf("provider: delegated model %q is invalid", spec.Model)
	}
	if strings.TrimSpace(spec.Prompt) == "" || len(spec.Prompt) > 4096 || strings.ContainsRune(spec.Prompt, '\x00') {
		return errors.New("provider: delegated initial prompt is invalid")
	}
	return nil
}

// ResumeSpec is the input to ResumeSessionArgs.
type ResumeSpec struct {
	WorkDir string
	// NativeSessionID must be the exact ID recorded at launch. Resuming
	// anything less specific — "the last session", "the session for this
	// directory" — is not exact resume and is not acceptable.
	NativeSessionID string
	// SkipPermissions carries the console setting into the resumed
	// process. Automatic recovery is unattended, so a resume that
	// dropped the mode would silently flip a permission-free session
	// into one that stops on a prompt nobody is watching.
	SkipPermissions bool
	// HookSettingsPath mirrors LaunchSpec.HookSettingsPath for resumed
	// generations.
	HookSettingsPath string
}

// Descriptor is the entire provider-specific surface of Deck.
//
// What is deliberately absent matters as much as what is present. A descriptor
// does not parse token streams into a shared event model; does not implement
// approvals, questions, tools, or model catalogs; and does not discover
// identity or status by reading provider transcripts, databases, state files,
// or terminal output.
//
// In particular there is no ExtractNativeSessionID(output string). Recovering
// identity by scraping what the CLI printed is exactly the coupling Deck
// refuses — it would break on any cosmetic change to the provider's startup
// banner, and it is why identity must come from an ID we assign or a command
// documented to return it.
type Descriptor interface {
	// Name reports the provider identifier.
	Name() Name

	// Executable is the absolute path to the provider binary.
	Executable() string

	// IdentityContract reports how exact session identity is established.
	IdentityContract() IdentityContract

	// Availability reports nil if this provider may be used, or an error
	// explaining why not. A provider that cannot establish exact resumable
	// identity is unavailable, not degraded — unless its contract is
	// IdentityNone, where identity-less operation is the design.
	Availability() error

	// NewSessionArgs builds the full argv for a new session, argv[0] being the
	// executable. It fails rather than returning approximate arguments.
	NewSessionArgs(spec LaunchSpec) ([]string, error)

	// ResumeSessionArgs builds the full argv to resume an exact native session.
	ResumeSessionArgs(spec ResumeSpec) ([]string, error)

	// Environment returns the environment additions this provider requires,
	// as KEY=VALUE. It must not return secrets: provider credentials live in
	// the provider's own persisted home, not in argv or env we build.
	Environment() []string

	// PermittedEnvironment lists the variable names that may be inherited from
	// the service's own environment.
	//
	// It is an allowlist rather than a denylist: the service process holds the
	// user's shell environment, which routinely contains API keys for
	// unrelated tools, and passing that wholesale into a provider would hand it
	// credentials it has no business seeing.
	PermittedEnvironment() []string

	// RequiredConfigPaths lists the paths this provider must be able to reach
	// for its identity and credentials to work.
	//
	// These are what a deployment has to persist. Losing them does not merely
	// log the user out — it destroys the native sessions recovery resumes.
	RequiredConfigPaths() []string

	// ExecutableIdentity reports how this provider's process is recognised
	// from /proc. It is provider-specific because a native binary and an
	// interpreted script are recognised differently.
	ExecutableIdentity() ExecutableIdentity

	// Injection reports how to deliver terminal input.
	Injection() InjectionContract

	// GracefulStopSignal and ForceStopSignal are provider policy: which signal
	// this CLI handles cleanly, and which one guarantees termination.
	GracefulStopSignal() syscall.Signal
	ForceStopSignal() syscall.Signal
}

// ExecutableIdentity describes what /proc can and cannot tell us about a
// running provider. It is provider-specific, and for two of the three
// providers the answer is "almost nothing".
//
// Measured on the installed CLIs:
//
//	           /proc/exe                    /proc/cmdline
//	claude     .../versions/2.1.220         (the launch path)
//	codex      /usr/local/bin/node          "node /usr/local/bin/codex"
//	kimi       /usr/local/bin/node          "kimi-code"
//
// Read that table carefully, because every shortcut it forbids was tried:
//
//   - /proc/exe is the INTERPRETER for both Node providers. It is the same
//     value for Codex and Kimi and for every other Node process on the host,
//     so it cannot distinguish them from each other or from anything else.
//   - /proc/cmdline does not rescue it. Codex shows the SYMLINK path, not the
//     resolved script; Kimi has rewritten argv[0] to "kimi-code" and shows no
//     path at all.
//
// So for an interpreted provider there is NO reliable runtime executable
// evidence. That is not a gap to work around — it is the honest answer, and it
// is why a name, PID, or command line alone is never sufficient. Identity rests
// on the rest of the tuple, and in particular on the generation marker and
// identity marker Deck writes as tmux pane options.
//
// Those markers are the best available evidence, but they are NOT tamper-proof
// and must not be described as such. Measured: a pane process inherits
// TMUX=<socket>,<pid>,<session> and TMUX_PANE=%N in its environment, and
// anything holding that environment can rewrite the pane's options —
// `tmux set-option -p @noisedeck_marker …` from inside the pane succeeds. tmux
// re-injects those variables even when new-session is given `-e TMUX=`, so it
// cannot be suppressed.
//
// This sits inside the threat boundary the first release already accepts:
// other processes running as the same Unix user can bypass Deck and are
// explicitly outside it. So markers do not establish anything against a
// hostile process, and no part of the design may depend on them doing so.
//
// Nor do markers catch every accident. Measured: `respawn-pane -k` replaces the
// pane's process while the pane id AND both markers survive unchanged —
// pid 16918 -> 16926, start ticks 12980990 -> 12980992, gen and marker
// identical. An out-of-band respawn is invisible to markers alone.
//
// This is why identity is a TUPLE rather than a field: no single member
// sees everything. What each member has actually been MEASURED to catch is
// recorded in docs/provider-contracts/. Two failures matter most for
// reconciliation, because the tuple cannot see them at all: an escaped child
// process, and a pane moved between windows.
type ExecutableIdentity struct {
	// Interpreted is true when the provider runs under an interpreter.
	Interpreted bool

	// ExpectedProcExe is what /proc/<pid>/exe should resolve to. For an
	// interpreted provider this is the interpreter, and matching it only rules
	// out an obviously wrong process.
	ExpectedProcExe string

	// LaunchTarget is what the PATH entry resolves to on disk. It is a static
	// fact about the installation, useful for diagnostics and for detecting an
	// upgrade — NOT something to compare against a running process. Neither
	// /proc/exe nor /proc/cmdline reports it for the interpreted providers.
	LaunchTarget string

	// ObservedCmdline is what /proc/<pid>/cmdline was measured to contain.
	// Recorded so the next person does not re-derive it and reach for it as
	// evidence; a process can rewrite its own argv, and Kimi does.
	ObservedCmdline string
}

// Sufficient reports whether matching ExpectedProcExe establishes that a
// process is this provider.
//
// False for interpreted providers, where nothing in /proc identifies them.
// When this is false, a caller must not treat an executable match as identity;
// the pane identity markers and the rest of the identity tuple are what decide.
func (e ExecutableIdentity) Sufficient() bool { return !e.Interpreted }

// ErrUnavailable is the sentinel for a provider that may not be used.
var ErrUnavailable = errors.New("provider unavailable")

// AvailableNames reports, sorted, the names in a boot-built registry. Boot
// registers only the harnesses whose executable resolved, so the registry is
// what this image can actually launch. Every consumer of that answer — the
// picker's inventory endpoint and the create endpoint's admission gate —
// derives it here, because a second enumeration written by hand is a list
// that drifts the day an image variant ships without one of its entries.
func AvailableNames(registry map[Name]Descriptor) []string {
	out := make([]string, 0, len(registry))
	for name := range registry {
		out = append(out, string(name))
	}
	sort.Strings(out)
	return out
}
