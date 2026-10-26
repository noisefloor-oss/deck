package provider

import (
	"fmt"
	"strings"
	"syscall"
)

// The contracts below were measured against Devin 3000.5.20 and re-measured
// against 3000.6.14 by the conformance probe.

// DevinDescriptor transports Cognition's real interactive Devin CLI. Devin
// allocates an opaque session id and reports it through its documented
// SessionStart hook before the first prompt. Exact resume uses --resume.
//
// Devin (re-measured on 3000.6.14) silently starts a new session when --resume names an unknown
// id. The documented hook's source field is therefore an attestation boundary:
// the successor stays provisional until source=resume carries the exact id
// Deck requested. source=startup or a different id fails closed and permits
// cleanup only of that successor's complete runtime tuple.
type DevinDescriptor struct {
	execPath     string
	hookPath     string
	receiverPath string
}

func NewDevin(execPath, hookConfigPath, receiverPath string) (*DevinDescriptor, error) {
	if !strings.HasPrefix(execPath, "/") {
		return nil, fmt.Errorf("provider: devin executable %q must be an absolute path", execPath)
	}
	if !strings.HasPrefix(hookConfigPath, "/") {
		return nil, fmt.Errorf("provider: devin hook config %q must be an absolute path", hookConfigPath)
	}
	if !strings.HasPrefix(receiverPath, "/") {
		return nil, fmt.Errorf("provider: devin hook receiver %q must be an absolute path", receiverPath)
	}
	return &DevinDescriptor{execPath: execPath, hookPath: hookConfigPath, receiverPath: receiverPath}, nil
}

func (d *DevinDescriptor) Name() Name                         { return Devin }
func (d *DevinDescriptor) Executable() string                 { return d.execPath }
func (d *DevinDescriptor) IdentityContract() IdentityContract { return IdentityRetrieved }
func (d *DevinDescriptor) IdentityCaptureMechanisms() []IdentityCaptureMechanism {
	return []IdentityCaptureMechanism{IdentityCaptureStructuredNativeHook}
}
func (d *DevinDescriptor) Availability() error { return nil }
func (d *DevinDescriptor) IdentityHook() IdentityHook {
	return IdentityHook{
		Event:               "SessionStart",
		SessionIDField:      "session_id",
		ConfigPath:          d.hookPath,
		ReceiverPath:        d.receiverPath,
		Timing:              IdentityHookAtProcessLaunch,
		RequiresNativeTrust: false,
	}
}

func (d *DevinDescriptor) ExactResumeHookSource() string { return "resume" }

func (d *DevinDescriptor) NewSessionArgs(spec LaunchSpec) ([]string, error) {
	if spec.SessionID != "" {
		return nil, fmt.Errorf("provider: devin allocates its own session id and cannot be assigned %q; capture the id from the %s hook instead", spec.SessionID, d.IdentityHook().Event)
	}
	argv := []string{d.execPath, "--respect-workspace-trust", "false"}
	if spec.SkipPermissions {
		argv = append(argv, "--permission-mode", "dangerous")
	}
	return argv, nil
}

// ResumeSessionArgs resumes an exact recorded session. The recorded id is
// replayed verbatim: the CLI owns its own id format, and a format change
// upstream must never strand a recorded session. Only an empty id is
// refused — an empty recording is a bookkeeping bug on our side, not a
// provider format change.
func (d *DevinDescriptor) ResumeSessionArgs(spec ResumeSpec) ([]string, error) {
	id := strings.TrimSpace(spec.NativeSessionID)
	if id == "" {
		return nil, fmt.Errorf("provider: devin resume requires a native session id")
	}
	argv := []string{d.execPath, "--resume", id, "--respect-workspace-trust", "false"}
	if spec.SkipPermissions {
		argv = append(argv, "--permission-mode", "dangerous")
	}
	return argv, nil
}

func (d *DevinDescriptor) Environment() []string { return nil }

func (d *DevinDescriptor) PermittedEnvironment() []string {
	return []string{"HOME", "PATH", "TERM", "LANG", "LC_ALL"}
}

func (d *DevinDescriptor) RequiredConfigPaths() []string {
	return []string{"~/.config/devin", "~/.local/share/devin", d.hookPath}
}

func (d *DevinDescriptor) ExecutableIdentity() ExecutableIdentity {
	return ExecutableIdentity{
		ExpectedProcExe: "/opt/devin/3000.11.205/bin/devin",
		LaunchTarget:    "/opt/devin/3000.11.205/bin/devin",
		ObservedCmdline: "/usr/local/bin/devin",
	}
}

func (d *DevinDescriptor) Injection() InjectionContract {
	return InjectionContract{
		UseBracketedPaste:      true,
		SubmitKey:              "Enter",
		SupportsMultiline:      true,
		AcceptsInputDuringTurn: ActiveTurnQueued,
		Verified:               true,
	}
}

func (d *DevinDescriptor) GracefulStopSignal() syscall.Signal { return syscall.SIGTERM }
func (d *DevinDescriptor) ForceStopSignal() syscall.Signal    { return syscall.SIGKILL }
