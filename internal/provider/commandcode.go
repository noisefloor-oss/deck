package provider

import (
	"fmt"
	"strings"
	"syscall"
)

// The contracts below were measured against Command Code 1.21.0.

// CommandCodeDescriptor transports the real Command Code interactive CLI.
// Command Code allocates its native UUID and reports it through its documented
// SessionStart hook. Exact resume uses --session with the complete UUID; the
// descriptor rejects prefixes even though the CLI accepts them.
type CommandCodeDescriptor struct {
	execPath     string
	hookPath     string
	receiverPath string
}

func NewCommandCode(execPath, hookConfigPath, receiverPath string) (*CommandCodeDescriptor, error) {
	if !strings.HasPrefix(execPath, "/") {
		return nil, fmt.Errorf("provider: command-code executable %q must be an absolute path", execPath)
	}
	if !strings.HasPrefix(hookConfigPath, "/") {
		return nil, fmt.Errorf("provider: command-code hook config %q must be an absolute path", hookConfigPath)
	}
	if !strings.HasPrefix(receiverPath, "/") {
		return nil, fmt.Errorf("provider: command-code hook receiver %q must be an absolute path", receiverPath)
	}
	return &CommandCodeDescriptor{execPath: execPath, hookPath: hookConfigPath, receiverPath: receiverPath}, nil
}

func (c *CommandCodeDescriptor) Name() Name                         { return CommandCode }
func (c *CommandCodeDescriptor) Executable() string                 { return c.execPath }
func (c *CommandCodeDescriptor) IdentityContract() IdentityContract { return IdentityRetrieved }
func (c *CommandCodeDescriptor) IdentityCaptureMechanisms() []IdentityCaptureMechanism {
	return []IdentityCaptureMechanism{IdentityCaptureStructuredNativeHook}
}
func (c *CommandCodeDescriptor) Availability() error { return nil }

func (c *CommandCodeDescriptor) IdentityHook() IdentityHook {
	return IdentityHook{
		Event:               "SessionStart",
		SessionIDField:      "session_id",
		ConfigPath:          c.hookPath,
		ReceiverPath:        c.receiverPath,
		Timing:              IdentityHookAtProcessLaunch,
		RequiresNativeTrust: false,
	}
}

func (c *CommandCodeDescriptor) NewSessionArgs(spec LaunchSpec) ([]string, error) {
	if spec.SessionID != "" {
		return nil, fmt.Errorf("provider: command-code allocates its own session id and cannot be assigned %q; capture the id from the %s hook instead", spec.SessionID, c.IdentityHook().Event)
	}
	argv := []string{c.execPath, "--no-auto-update", "--skip-onboarding"}
	if spec.SkipPermissions {
		argv = append(argv, "--yolo")
	}
	return argv, nil
}

// ResumeSessionArgs resumes an exact recorded session. The recorded id is
// replayed verbatim: the CLI owns its own id format, and a format change
// upstream must never strand a recorded session. Only an empty id is
// refused — an empty recording is a bookkeeping bug on our side, not a
// provider format change.
func (c *CommandCodeDescriptor) ResumeSessionArgs(spec ResumeSpec) ([]string, error) {
	id := strings.TrimSpace(spec.NativeSessionID)
	if id == "" {
		return nil, fmt.Errorf("provider: command-code resume requires a native session id")
	}
	argv := []string{c.execPath, "--no-auto-update", "--skip-onboarding", "--session", id}
	if spec.SkipPermissions {
		argv = append(argv, "--yolo")
	}
	return argv, nil
}

func (c *CommandCodeDescriptor) Environment() []string { return nil }
func (c *CommandCodeDescriptor) PermittedEnvironment() []string {
	return []string{"HOME", "PATH", "TERM", "LANG", "LC_ALL"}
}
func (c *CommandCodeDescriptor) RequiredConfigPaths() []string {
	return []string{"~/.commandcode", c.hookPath}
}
func (c *CommandCodeDescriptor) ExecutableIdentity() ExecutableIdentity {
	return ExecutableIdentity{
		Interpreted:     true,
		ExpectedProcExe: "/usr/local/bin/node",
		LaunchTarget:    "/usr/local/lib/command-code/node_modules/command-code/dist/index.mjs",
		ObservedCmdline: "node /usr/local/bin/command-code",
	}
}
func (c *CommandCodeDescriptor) Injection() InjectionContract {
	return InjectionContract{
		UseBracketedPaste:        true,
		SubmitKey:                "Enter",
		SupportsMultiline:        true,
		AcceptsInputDuringTurn:   ActiveTurnQueued,
		DiscardsInputDuringModal: true,
		Verified:                 false,
	}
}
func (c *CommandCodeDescriptor) GracefulStopSignal() syscall.Signal { return syscall.SIGTERM }
func (c *CommandCodeDescriptor) ForceStopSignal() syscall.Signal    { return syscall.SIGKILL }
