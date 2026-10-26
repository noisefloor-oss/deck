package provider

import (
	"fmt"
	"strings"
	"syscall"
)

// The contracts below were measured against Pi 0.84.2.

// PiDescriptor launches the maintained Pi coding agent with its native
// provider and model settings. Pi allocates its own UUIDv7 session id and reports it
// through an extension hook rather than accepting one at launch, so its
// IdentityContract is IdentityRetrieved, like omp and opencode.
//
// MEASURED against 0.84.2 (private tmux namespace, scratch session store,
// no prompt ever submitted): a `--extension`-loaded module registering
// pi.on("session_start") fires ~0.8s after process start, before any user
// input; ctx.sessionManager.getSessionId() is already populated with a UUIDv7
// and ctx.sessionManager.getCwd() returns the launch working directory:
//
//	{"hook_event_name":"SessionStart",
//	 "session_id":"01a02c59-f6d2-7886-be70-f6d4173068d9",
//	 "cwd":"/tmp/pi-measure-work","source":"startup"}
//
// The id is eager because Pi keeps a brand-new session in memory and writes
// the session file only after the first assistant response: `--session-id`
// therefore warns "No project session found" on every genuinely new session,
// and nothing Deck can launch short of submitting a prompt makes the id
// discoverable earlier. Dropping the flag removes the warning; the hook
// recovers the id. A message-free session still persists no file, so its id
// is captured but a later resume of it fails loudly (unknown exact id) —
// the same boundary the assigned contract already had.
type PiDescriptor struct {
	execPath string
	hookPath string
}

// NewPi builds a pi descriptor. hookPath is the absolute path to the
// provisioning-owned capture extension that NewSessionArgs injects via
// `--extension`. Provisioning installs it byte-for-byte; Deck never
// writes it.
func NewPi(execPath, hookPath string) (*PiDescriptor, error) {
	if !strings.HasPrefix(execPath, "/") {
		return nil, fmt.Errorf("provider: pi executable %q must be an absolute path", execPath)
	}
	if !strings.HasPrefix(hookPath, "/") {
		return nil, fmt.Errorf("provider: pi capture hook %q must be an absolute path", hookPath)
	}
	return &PiDescriptor{execPath: execPath, hookPath: hookPath}, nil
}

func (p *PiDescriptor) Name() Name                         { return Pi }
func (p *PiDescriptor) Executable() string                 { return p.execPath }
func (p *PiDescriptor) IdentityContract() IdentityContract { return IdentityRetrieved }
func (p *PiDescriptor) IdentityCaptureMechanisms() []IdentityCaptureMechanism {
	return []IdentityCaptureMechanism{IdentityCaptureStructuredNativeHook}
}

// IdentityHook reports the callback that yields the native session id.
//
// pi's hook mechanism is a JavaScript event module loaded per launch via
// `--extension` — the same shape as omp's `--hook` TypeScript module. The
// event payload carries no id; the hook reads it from
// ctx.sessionManager.getSessionId() and POSTs directly to the capture
// endpoint inherited in the launch environment.
//
// MEASURED: no native trust prompt for `--extension`-loaded modules, and
// extension discovery scans only <cwd>/.pi/extensions/ and
// ~/.pi/agent/extensions/, so a hook installed at the ~/.pi config root
// loads only when Deck passes the flag. MEASURED timing: the id is eager
// at launch — IdentityHookAtProcessLaunch. Deck attaches the live process
// while its native ID is pending. ReceiverPath is empty because the
// byte-identity-pinned hook delivers in-process.
func (p *PiDescriptor) IdentityHook() IdentityHook {
	return IdentityHook{
		Event:               "session_start",
		SessionIDField:      "session_id",
		ConfigPath:          p.hookPath,
		Timing:              IdentityHookAtProcessLaunch,
		RequiresNativeTrust: false,
	}
}

// Availability reports nil: pi reports its exact native session ID through the
// session_start hook.
func (p *PiDescriptor) Availability() error { return nil }

// NewSessionArgs launches a new interactive session.
//
// pi allocates its own UUIDv7 and reports it through the hook, so supplying
// one is an error rather than being silently dropped: a caller that believed
// it assigned an id would record the wrong identity and never be able to
// resume. The capture hook is injected every launch via `--extension`; Deck
// controls argv, so it does not depend on pi's extension discovery.
//
// Deck selects no model or reasoning level — whatever the provider itself
// resolves is authoritative. Pi 1.4 requires --model with --provider, so
// neither selector belongs in the launch arguments.
func (p *PiDescriptor) NewSessionArgs(spec LaunchSpec) ([]string, error) {
	if spec.SessionID != "" {
		return nil, fmt.Errorf(
			"provider: pi allocates its own session id and cannot be assigned "+
				"%q; capture the id from the %s hook instead",
			spec.SessionID, p.IdentityHook().Event)
	}
	argv := []string{
		p.execPath,
		"--extension", p.hookPath,
	}
	if spec.SkipPermissions {
		// Pi 0.84.2 exposes no separate tool-approval bypass. --approve resolves
		// its project-local resource trust prompt for this run.
		argv = append(argv, "--approve")
	}
	return argv, nil
}

// ResumeSessionArgs resumes an exact recorded session. The recorded id is
// replayed verbatim: the CLI owns its own id format, and a format change
// upstream must never strand a recorded session. Only an empty id is
// refused — an empty recording is a bookkeeping bug on our side, not a
// provider format change.
//
// The argv is intentionally bare, without --extension: the identity is
// already recorded, and replaying the capture hook at resume would report
// a hook event no resume grant expects.
func (p *PiDescriptor) ResumeSessionArgs(spec ResumeSpec) ([]string, error) {
	id := strings.TrimSpace(spec.NativeSessionID)
	if id == "" {
		return nil, fmt.Errorf("provider: pi resume requires a native session id")
	}
	// --session resolves the exact UUID and exits non-zero when it does not
	// exist. --continue and --resume are selectors/recency-based and forbidden.
	// Deck selects no model or reasoning level on this path either.
	argv := []string{
		p.execPath,
		"--session", id,
	}
	if spec.SkipPermissions {
		argv = append(argv, "--approve")
	}
	return argv, nil
}

func (p *PiDescriptor) Environment() []string {
	return []string{
		"PI_SKIP_VERSION_CHECK=1",
		"PI_TELEMETRY=0",
		"NOISEDECK_MEMPALACE_HARNESS=pi",
	}
}

func (p *PiDescriptor) PermittedEnvironment() []string {
	return []string{"HOME", "PATH", "TERM", "LANG", "LC_ALL"}
}

// RequiredConfigPaths names what a deployment must persist. ~/.pi/agent is
// pi's state root: credentials, settings, and the session store. The capture
// hook is provisioning-owned and verified byte-for-byte before every launch,
// so losing it breaks identity capture for every new session.
func (p *PiDescriptor) RequiredConfigPaths() []string {
	return []string{"~/.pi/agent", p.hookPath}
}

func (p *PiDescriptor) ExecutableIdentity() ExecutableIdentity {
	return ExecutableIdentity{
		Interpreted:     true,
		ExpectedProcExe: "/usr/local/bin/node",
		LaunchTarget:    "/usr/local/lib/node_modules/@earendil-works/pi-coding-agent/dist/cli.js",
		ObservedCmdline: "node /usr/local/bin/pi",
	}
}

func (p *PiDescriptor) Injection() InjectionContract {
	return InjectionContract{
		UseBracketedPaste:        true,
		SubmitKey:                "Enter",
		SupportsMultiline:        true,
		AcceptsInputDuringTurn:   ActiveTurnQueued,
		DiscardsInputDuringModal: true,
		Verified:                 true,
	}
}

func (p *PiDescriptor) GracefulStopSignal() syscall.Signal { return syscall.SIGTERM }
func (p *PiDescriptor) ForceStopSignal() syscall.Signal    { return syscall.SIGKILL }
