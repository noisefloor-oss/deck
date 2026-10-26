package provider

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"syscall"
)

// FreshRelaunchDescriptor serves every CLI whose identity contract is
// fresh-relaunch-only: the provider owns its own conversation state, but Deck
// never represents a replacement process as continuation of a conversation.
// Such a harness has no resume argv, no assigned identity, and no capture
// mechanism, so nothing is left for a per-provider type to hold except the
// measured values in freshRelaunchSpec. One implementation carries them all,
// and a new such CLI is a table row.
type FreshRelaunchDescriptor struct {
	execPath string
	spec     freshRelaunchSpec
}

// freshRelaunchSpec is the whole declared surface of a fresh-relaunch CLI.
// Every field is a measured value, never a default: a provider that has not
// measured one leaves it empty rather than borrowing a neighbour's.
type freshRelaunchSpec struct {
	name Name
	// launchArgs follow the executable on every launch.
	launchArgs []string
	// permissiveArgs are appended when the user's skip-permissions
	// setting is on; restrictedArgs when it is off. A CLI whose own default
	// posture is the interactive approval flow leaves restrictedArgs empty,
	// because the flag it would carry does not exist.
	permissiveArgs []string
	restrictedArgs []string
	// resumeRefusal is the reason this CLI issues no resume argv. It reads
	// after "provider: <name> ".
	resumeRefusal        string
	environment          []string
	permittedEnvironment []string
	configPaths          []string
	executableIdentity   ExecutableIdentity
	injection            InjectionContract
}

// cursorShippedVersion is the image's install prefix: it follows the
// Dockerfile pin, never the measured contract, because the launch paths
// name where the CLI IS, not the version its behavior was measured on.
const cursorShippedVersion = "2026.10.22-27dcf5f"

// freshRelaunchSpecs is the table. Each row records what was measured against
// the version its comment names; nothing here is inferred from a neighbour.
var freshRelaunchSpecs = map[Name]freshRelaunchSpec{
	// Cline 3.0.53. Cline is deliberately fresh-relaunch-only in this
	// release: provider-owned state persists, but no trustworthy native
	// identity source has been measured. --auto-approve takes an explicit
	// boolean rather than being a bare flag, so both permission postures
	// name it.
	Cline: {
		name:                 Cline,
		launchArgs:           []string{"--tui"},
		permissiveArgs:       []string{"--auto-approve", "true"},
		restrictedArgs:       []string{"--auto-approve", "false"},
		resumeRefusal:        "native conversation resume is deferred; recovery must use a fresh launch",
		environment:          []string{"NOISEDECK_MEMPALACE_HARNESS=cline"},
		permittedEnvironment: []string{"HOME", "PATH", "TERM", "LANG", "LC_ALL"},
		configPaths:          []string{"~/.cline"},
		executableIdentity: ExecutableIdentity{
			Interpreted:     true,
			ExpectedProcExe: "/usr/local/bin/node",
			LaunchTarget:    "/usr/local/lib/node_modules/cline/bin/cline",
			ObservedCmdline: "node /usr/local/bin/cline",
		},
		injection: InjectionContract{
			UseBracketedPaste:      true,
			SubmitKey:              "Enter",
			SupportsMultiline:      true,
			AcceptsInputDuringTurn: ActiveTurnUnknown,
			Verified:               false,
		},
	},

	// cursor-agent 2026.08.11-e8db854. The shipped artifact is Cursor's
	// official agent-cli-package tarball, pinned by version and per-arch
	// sha256, unpacked under /opt/cursor-agent and launched through its
	// cursor-agent launcher script. The measured surface: an unauthenticated
	// launch with --trust stays up at the login wall ("Press any key to log
	// in..."), the launcher execs the package's vendored node on its bundled
	// index.js, and --trust suppresses the workspace-trust prompt for the
	// launch directory. --yolo is the CLI's own documented alias for --force,
	// its "force allow commands unless explicitly denied" posture. No
	// authenticated session was reachable, so nothing an authenticated run
	// would show is declared here. --resume/--continue and the ls/resume
	// subcommands are real, pinned syntax, but Deck issues no resume argv
	// until an authenticated run proves resume returns that conversation
	// rather than silently starting a new one.
	//
	// The launcher pins its own Node compile cache under the cache home; that
	// is a cache, not credentials, so the plain interactive allowlist plus
	// XDG_CACHE_HOME applies.
	//
	// The executable identity is the shipped package's runtime shape,
	// measured in a real tmux pane: the launcher script execs the package's
	// vendored node binary on its bundled index.js, so the pane process is
	// that vendored interpreter with the configured launch path as argv[0]
	// (the script's exec -a). The vendored interpreter path is unique to this
	// package, but the process is still an interpreter running a script, so
	// identity rests on the pane tuple like every interpreted harness.
	Cursor: {
		name:                 Cursor,
		launchArgs:           []string{"--trust"},
		permissiveArgs:       []string{"--yolo"},
		resumeRefusal:        "resume is unmeasured; recovery must use a fresh launch",
		permittedEnvironment: []string{"HOME", "PATH", "TERM", "LANG", "LC_ALL", "XDG_CACHE_HOME"},
		configPaths:          []string{"~/.cursor"},
		executableIdentity: ExecutableIdentity{
			Interpreted:     true,
			ExpectedProcExe: "/opt/cursor-agent/" + cursorShippedVersion + "/dist-package/node",
			LaunchTarget:    "/opt/cursor-agent/" + cursorShippedVersion + "/dist-package/index.js",
			ObservedCmdline: "/usr/local/bin/cursor-agent --use-system-ca",
		},
	},

	// Grok Build 1.0.5 (@xai-official/grok). The shipped artifact is the npm
	// trampoline plus the per-platform binary package npm selects for the
	// build arch; the binary itself ships brotli-compressed and is
	// bootstrapped into ~/.grok/bin at install/first-run time by the
	// package's own postinstall and trampoline. The measured surface: an
	// unauthenticated interactive launch stays up at the browser sign-in wall
	// (a device code and "Waiting for approval..."), the pane process is the
	// Node trampoline whose child is the bootstrapped grok binary in the same
	// process group, and `grok --version` prints "grok <version> (<commit>)".
	// --always-approve is the CLI's own "auto-approve all tool executions"
	// flag; the plain launch carries none because the measured default
	// posture is the interactive approval flow. No authenticated session was
	// reachable, so nothing an authenticated run would show is declared here.
	// --resume resolves session IDs but ALSO resolves titles and ID prefixes
	// with different ambiguity rules, and -s/--session-id assigns; both stay
	// unused until an authenticated run measures them.
	//
	// The executable identity is the npm package's runtime shape, measured in
	// a real tmux pane: the pane process is the Node trampoline (/proc/exe =
	// the interpreter, cmdline = "node <configured executable>"), and the CLI
	// proper runs as its child — the bootstrapped binary at ~/.grok/bin/grok,
	// argv[0] the canonical symlink path, in the same process group and
	// session. Nothing in /proc distinguishes the trampoline from any other
	// Node process, so pane identity rests on the pane tuple.
	Grok: {
		name:                 Grok,
		permissiveArgs:       []string{"--always-approve"},
		resumeRefusal:        "resume is unmeasured; recovery must use a fresh launch",
		permittedEnvironment: []string{"HOME", "PATH", "TERM", "LANG", "LC_ALL"},
		configPaths:          []string{"~/.grok"},
		executableIdentity: ExecutableIdentity{
			Interpreted:     true,
			ExpectedProcExe: "/usr/local/bin/node",
			LaunchTarget:    "/usr/local/lib/node_modules/@xai-official/grok/bin/grok",
			ObservedCmdline: "node /usr/local/bin/grok",
		},
	},

	// Xiaomi MiMo Code 0.1.13. This integration is deliberately
	// fresh-relaunch-only: MiMo exposes session flags, but Deck has not
	// measured a trustworthy provider-native identity source and never reads
	// MiMo's output or private state to invent one.
	//
	// The executable identity is the shipped npm wrapper's runtime shape,
	// measured in a real tmux pane: the pane process is the Node launcher
	// (/proc/exe = the interpreter, cmdline = "node <launch path> --trust"),
	// and the actual CLI runs as its child, the vendored platform binary the
	// launcher picks for the host CPU. The launcher path component of the
	// cmdline is the configured executable path; the interpreter path is
	// /usr/local/bin/node both in the image and at the measurement site
	// (docs/provider-contracts/mimo.yaml records the full observation,
	// including the child). Nothing here distinguishes MiMo from any other
	// Node process, so pane identity rests on the pane tuple.
	Mimo: {
		name:                 Mimo,
		launchArgs:           []string{"--trust"},
		permissiveArgs:       []string{"--yolo"},
		resumeRefusal:        "native conversation resume is deferred; recovery must use a fresh launch",
		permittedEnvironment: []string{"HOME", "PATH", "TERM", "LANG", "LC_ALL", "XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_CACHE_HOME", "XDG_STATE_HOME"},
		configPaths:          []string{"~/.config/mimocode", "~/.local/share/mimocode", "~/.local/state/mimocode", "~/.cache/mimocode"},
		executableIdentity: ExecutableIdentity{
			Interpreted:     true,
			ExpectedProcExe: "/usr/local/bin/node",
			LaunchTarget:    "/usr/local/lib/node_modules/@mimo-ai/cli/bin/mimo",
			ObservedCmdline: "node /usr/local/bin/mimo",
		},
	},

	// Qoder 1.1.28 (@qoder-ai/qodercli). The measured surface: an
	// unauthenticated interactive launch stays up and shows two walls in
	// order — the folder-trust modal ("Do you trust the files in this
	// folder?" with Trust folder / Don't trust and exit) and the "Not Login
	// Please Auth" banner — and the CLI's own `--list-models` reports "Not
	// logged in" headlessly. --dangerously-skip-permissions is the CLI's own
	// "bypass all permission checks" flag; the plain launch carries none
	// because the measured default is the interactive confirmation posture.
	// No authenticated session was reachable, so nothing an authenticated run
	// would show is declared here. --resume and --session-id are real, pinned
	// syntax, but Deck issues no resume argv and no assigned id until an
	// authenticated run proves resume returns that conversation rather than
	// silently starting a new one.
	//
	// Qoder derives from the Gemini CLI, and that lineage has one measured
	// trap: with CI=true in the environment the CLI skips its TUI, takes the
	// headless path, and exits 42 ("No input provided via stdin") before any
	// wall appears. The allowlist below deliberately carries no CI variable,
	// so a Deck launch can never inherit one.
	//
	// The executable identity is the shipped npm package's runtime shape,
	// measured in a real tmux pane: the pane process is the Node launcher
	// (the npm bin shim, /proc/exe = the interpreter, cmdline =
	// "node <configured executable>"), and the CLI proper runs as its child,
	// Node on the package's bundle entry. Nothing in /proc distinguishes
	// Qoder from any other Node process, so pane identity rests on the pane
	// tuple. Qoder's writes extend past its own root: even --version appends
	// its local files block to the user's global git ignore
	// (~/.config/git/ignore), which docs/provider-contracts/qoder.yaml
	// records.
	Qoder: {
		name:                 Qoder,
		permissiveArgs:       []string{"--dangerously-skip-permissions"},
		resumeRefusal:        "resume is unmeasured; recovery must use a fresh launch",
		permittedEnvironment: []string{"HOME", "PATH", "TERM", "LANG", "LC_ALL"},
		configPaths:          []string{"~/.qoder"},
		executableIdentity: ExecutableIdentity{
			Interpreted:     true,
			ExpectedProcExe: "/usr/local/bin/node",
			LaunchTarget:    "/usr/local/lib/node_modules/@qoder-ai/qodercli/bundle/qodercli.js",
			ObservedCmdline: "node /usr/local/bin/qoder",
		},
	},

	// Qwen Code 0.21.14. The measured surface: the parser rejects unknown
	// flags outside --help with exit 1, an unauthenticated launch stops at
	// the "Connect a Provider" modal, and SIGTERM to the pane's process group
	// exits the process. Chat recording is pinned on for launch because the
	// CLI's own help states --continue/--resume do not work when recording is
	// false, and --yolo is the CLI's own "automatically accept all actions"
	// flag. No authenticated session was reachable, so nothing an
	// authenticated run would show is declared here. --resume is real, pinned
	// syntax, but Deck issues no resume argv until an authenticated run
	// proves --resume <exact-id> resumes that conversation rather than
	// silently starting a new one.
	Qwen: {
		name:                 Qwen,
		launchArgs:           []string{"--chat-recording"},
		permissiveArgs:       []string{"--yolo"},
		resumeRefusal:        "resume is unmeasured; recovery must use a fresh launch",
		permittedEnvironment: []string{"HOME", "PATH", "TERM", "LANG", "LC_ALL"},
		configPaths:          []string{"~/.qwen"},
		executableIdentity: ExecutableIdentity{
			Interpreted:     true,
			ExpectedProcExe: "/usr/local/bin/node",
			LaunchTarget:    "/usr/local/lib/node_modules/@qwen-code/qwen-code/cli-entry.js",
			ObservedCmdline: "node /usr/local/bin/qwen",
		},
	},
}

// newFreshRelaunch builds the descriptor for a table row.
func newFreshRelaunch(name Name, execPath string) (*FreshRelaunchDescriptor, error) {
	spec, ok := freshRelaunchSpecs[name]
	if !ok {
		return nil, fmt.Errorf("provider: no fresh-relaunch contract for %q", name)
	}
	if !strings.HasPrefix(execPath, "/") {
		return nil, fmt.Errorf("provider: %s executable %q must be an absolute path", name, execPath)
	}
	return &FreshRelaunchDescriptor{execPath: execPath, spec: spec}, nil
}

// NewCline builds the descriptor for Cline's real interactive TUI.
func NewCline(execPath string) (*FreshRelaunchDescriptor, error) {
	return newFreshRelaunch(Cline, execPath)
}

// NewCursor builds the descriptor for the real interactive Cursor Agent CLI.
func NewCursor(execPath string) (*FreshRelaunchDescriptor, error) {
	return newFreshRelaunch(Cursor, execPath)
}

// NewGrok builds the descriptor for the real interactive Grok Build CLI.
func NewGrok(execPath string) (*FreshRelaunchDescriptor, error) {
	return newFreshRelaunch(Grok, execPath)
}

// NewMimo builds the descriptor for Xiaomi MiMo Code's real interactive CLI.
func NewMimo(execPath string) (*FreshRelaunchDescriptor, error) {
	return newFreshRelaunch(Mimo, execPath)
}

// NewQoder builds the descriptor for the real interactive Qoder CLI.
func NewQoder(execPath string) (*FreshRelaunchDescriptor, error) {
	return newFreshRelaunch(Qoder, execPath)
}

// NewQwen builds the descriptor for the real interactive Qwen Code CLI.
func NewQwen(execPath string) (*FreshRelaunchDescriptor, error) {
	return newFreshRelaunch(Qwen, execPath)
}

func (d *FreshRelaunchDescriptor) Name() Name         { return d.spec.name }
func (d *FreshRelaunchDescriptor) Executable() string { return d.execPath }

func (d *FreshRelaunchDescriptor) IdentityContract() IdentityContract {
	return IdentityFreshRelaunch
}

// IdentityCaptureMechanisms is empty for every row: no authenticated run has
// observed a capture path, and declaring one would claim identity authority
// for delivery nobody has seen.
func (d *FreshRelaunchDescriptor) IdentityCaptureMechanisms() []IdentityCaptureMechanism {
	return nil
}

func (d *FreshRelaunchDescriptor) Availability() error { return nil }

func (d *FreshRelaunchDescriptor) NewSessionArgs(spec LaunchSpec) ([]string, error) {
	if spec.SessionID != "" {
		return nil, fmt.Errorf("provider: %s is fresh-relaunch-only but a session id %q was supplied", d.spec.name, spec.SessionID)
	}
	argv := make([]string, 0, 1+len(d.spec.launchArgs)+2)
	argv = append(argv, d.execPath)
	argv = append(argv, d.spec.launchArgs...)
	if spec.SkipPermissions {
		argv = append(argv, d.spec.permissiveArgs...)
	} else {
		argv = append(argv, d.spec.restrictedArgs...)
	}
	return argv, nil
}

func (d *FreshRelaunchDescriptor) ResumeSessionArgs(ResumeSpec) ([]string, error) {
	return nil, errors.New("provider: " + string(d.spec.name) + " " + d.spec.resumeRefusal)
}

func (d *FreshRelaunchDescriptor) Environment() []string {
	return slices.Clone(d.spec.environment)
}

func (d *FreshRelaunchDescriptor) PermittedEnvironment() []string {
	return slices.Clone(d.spec.permittedEnvironment)
}

func (d *FreshRelaunchDescriptor) RequiredConfigPaths() []string {
	return slices.Clone(d.spec.configPaths)
}

func (d *FreshRelaunchDescriptor) ExecutableIdentity() ExecutableIdentity {
	return d.spec.executableIdentity
}

// Injection carries only what a real run measured. A row that never reached
// its CLI's composer — every one whose launch stops at a login wall or trust
// modal — holds the zero contract, because a plausible default here would
// become the bytes Deck sends.
func (d *FreshRelaunchDescriptor) Injection() InjectionContract {
	return d.spec.injection
}

func (d *FreshRelaunchDescriptor) GracefulStopSignal() syscall.Signal { return syscall.SIGTERM }
func (d *FreshRelaunchDescriptor) ForceStopSignal() syscall.Signal    { return syscall.SIGKILL }
