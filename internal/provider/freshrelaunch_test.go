package provider

import (
	"reflect"
	"syscall"
	"testing"
)

// freshRelaunchCase pins one row's whole observable surface. The expectations
// are written out literally rather than read back from freshRelaunchSpecs: a
// test that consults the table it guards proves only that the table equals
// itself.
type freshRelaunchCase struct {
	name         Name
	execPath     string
	construct    func(string) (*FreshRelaunchDescriptor, error)
	wantPlain    []string
	wantSkip     []string
	wantEnv      []string
	wantAllowed  []string
	wantPaths    []string
	wantIdentity ExecutableIdentity
	wantInject   InjectionContract
}

func freshRelaunchCases() []freshRelaunchCase {
	return []freshRelaunchCase{
		{
			name:      Cline,
			execPath:  "/usr/local/bin/cline",
			construct: NewCline,
			// --auto-approve takes an explicit boolean rather than being a
			// bare flag, so both permission postures name it.
			wantPlain:   []string{"/usr/local/bin/cline", "--tui", "--auto-approve", "false"},
			wantSkip:    []string{"/usr/local/bin/cline", "--tui", "--auto-approve", "true"},
			wantEnv:     []string{"NOISEDECK_MEMPALACE_HARNESS=cline"},
			wantAllowed: []string{"HOME", "PATH", "TERM", "LANG", "LC_ALL"},
			wantPaths:   []string{"~/.cline"},
			wantIdentity: ExecutableIdentity{
				Interpreted:     true,
				ExpectedProcExe: "/usr/local/bin/node",
				LaunchTarget:    "/usr/local/lib/node_modules/cline/bin/cline",
				ObservedCmdline: "node /usr/local/bin/cline",
			},
			wantInject: InjectionContract{
				UseBracketedPaste:      true,
				SubmitKey:              "Enter",
				SupportsMultiline:      true,
				AcceptsInputDuringTurn: ActiveTurnUnknown,
			},
		},
		{
			name:      Cursor,
			execPath:  "/usr/local/bin/cursor-agent",
			construct: NewCursor,
			// --trust suppresses the measured workspace-trust prompt for the
			// launch directory; --yolo is the CLI's own documented alias for
			// --force, its "force allow commands unless explicitly denied"
			// posture.
			wantPlain: []string{"/usr/local/bin/cursor-agent", "--trust"},
			wantSkip:  []string{"/usr/local/bin/cursor-agent", "--trust", "--yolo"},
			// The launcher pins its own Node compile cache under the cache
			// home; that is a cache, not credentials.
			wantAllowed: []string{"HOME", "PATH", "TERM", "LANG", "LC_ALL", "XDG_CACHE_HOME"},
			wantPaths:   []string{"~/.cursor"},
			wantIdentity: ExecutableIdentity{
				Interpreted:     true,
				ExpectedProcExe: "/opt/cursor-agent/2026.10.22-27dcf5f/dist-package/node",
				LaunchTarget:    "/opt/cursor-agent/2026.10.22-27dcf5f/dist-package/index.js",
				ObservedCmdline: "/usr/local/bin/cursor-agent --use-system-ca",
			},
		},
		{
			name:      Grok,
			execPath:  "/usr/local/bin/grok",
			construct: NewGrok,
			// --always-approve is the CLI's own "auto-approve all tool
			// executions" flag; the plain launch carries no flag because the
			// measured default posture is the interactive approval flow.
			wantPlain:   []string{"/usr/local/bin/grok"},
			wantSkip:    []string{"/usr/local/bin/grok", "--always-approve"},
			wantAllowed: []string{"HOME", "PATH", "TERM", "LANG", "LC_ALL"},
			wantPaths:   []string{"~/.grok"},
			wantIdentity: ExecutableIdentity{
				Interpreted:     true,
				ExpectedProcExe: "/usr/local/bin/node",
				LaunchTarget:    "/usr/local/lib/node_modules/@xai-official/grok/bin/grok",
				ObservedCmdline: "node /usr/local/bin/grok",
			},
		},
		{
			name:        Mimo,
			execPath:    "/usr/local/bin/mimo",
			construct:   NewMimo,
			wantPlain:   []string{"/usr/local/bin/mimo", "--trust"},
			wantSkip:    []string{"/usr/local/bin/mimo", "--trust", "--yolo"},
			wantAllowed: []string{"HOME", "PATH", "TERM", "LANG", "LC_ALL", "XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_CACHE_HOME", "XDG_STATE_HOME"},
			wantPaths:   []string{"~/.config/mimocode", "~/.local/share/mimocode", "~/.local/state/mimocode", "~/.cache/mimocode"},
			wantIdentity: ExecutableIdentity{
				Interpreted:     true,
				ExpectedProcExe: "/usr/local/bin/node",
				LaunchTarget:    "/usr/local/lib/node_modules/@mimo-ai/cli/bin/mimo",
				ObservedCmdline: "node /usr/local/bin/mimo",
			},
		},
		{
			name:      Qoder,
			execPath:  "/usr/local/bin/qoder",
			construct: NewQoder,
			// --dangerously-skip-permissions is the CLI's own "bypass all
			// permission checks" flag; the plain launch carries no flag
			// because the measured default permission mode is the interactive
			// confirmation posture.
			wantPlain:   []string{"/usr/local/bin/qoder"},
			wantSkip:    []string{"/usr/local/bin/qoder", "--dangerously-skip-permissions"},
			wantAllowed: []string{"HOME", "PATH", "TERM", "LANG", "LC_ALL"},
			wantPaths:   []string{"~/.qoder"},
			wantIdentity: ExecutableIdentity{
				Interpreted:     true,
				ExpectedProcExe: "/usr/local/bin/node",
				LaunchTarget:    "/usr/local/lib/node_modules/@qoder-ai/qodercli/bundle/qodercli.js",
				ObservedCmdline: "node /usr/local/bin/qoder",
			},
		},
		{
			name:      Qwen,
			execPath:  "/usr/local/bin/qwen",
			construct: NewQwen,
			// Chat recording is pinned on: the CLI's help states
			// --continue/--resume do not work when recording is false. --yolo
			// is the CLI's own "automatically accept all actions" flag.
			wantPlain:   []string{"/usr/local/bin/qwen", "--chat-recording"},
			wantSkip:    []string{"/usr/local/bin/qwen", "--chat-recording", "--yolo"},
			wantAllowed: []string{"HOME", "PATH", "TERM", "LANG", "LC_ALL"},
			wantPaths:   []string{"~/.qwen"},
			wantIdentity: ExecutableIdentity{
				Interpreted:     true,
				ExpectedProcExe: "/usr/local/bin/node",
				LaunchTarget:    "/usr/local/lib/node_modules/@qwen-code/qwen-code/cli-entry.js",
				ObservedCmdline: "node /usr/local/bin/qwen",
			},
		},
	}
}

func (tc freshRelaunchCase) descriptor(t *testing.T) *FreshRelaunchDescriptor {
	t.Helper()
	d, err := tc.construct(tc.execPath)
	if err != nil {
		t.Fatalf("construct %s: %v", tc.name, err)
	}
	return d
}

// The table must cover exactly the rows the package ships, so a new row
// cannot arrive with no assertions behind it.
func TestFreshRelaunchTableCoversEveryRow(t *testing.T) {
	covered := make(map[Name]bool, len(freshRelaunchCases()))
	for _, tc := range freshRelaunchCases() {
		covered[tc.name] = true
	}
	for name := range freshRelaunchSpecs {
		if !covered[name] {
			t.Errorf("fresh-relaunch row %s has no test case", name)
		}
	}
	if len(covered) != len(freshRelaunchSpecs) {
		t.Fatalf("cases = %d, rows = %d", len(covered), len(freshRelaunchSpecs))
	}
}

func TestFreshRelaunchLaunchArgv(t *testing.T) {
	for _, tc := range freshRelaunchCases() {
		t.Run(string(tc.name), func(t *testing.T) {
			d := tc.descriptor(t)
			if d.Name() != tc.name || d.IdentityContract() != IdentityFreshRelaunch {
				t.Fatalf("descriptor = %s/%s", d.Name(), d.IdentityContract())
			}
			if d.Executable() != tc.execPath {
				t.Fatalf("executable = %q", d.Executable())
			}
			plain, err := d.NewSessionArgs(LaunchSpec{})
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(plain, tc.wantPlain) {
				t.Fatalf("plain launch argv = %#v, want %#v", plain, tc.wantPlain)
			}
			skip, err := d.NewSessionArgs(LaunchSpec{SkipPermissions: true})
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(skip, tc.wantSkip) {
				t.Fatalf("skip-permissions launch argv = %#v, want %#v", skip, tc.wantSkip)
			}
		})
	}
}

// A fresh-relaunch CLI is never handed an identity Deck assigned, and never
// builds a resume argv: recovery starts a new conversation rather than run a
// resume contract nobody measured.
func TestFreshRelaunchRefusesIdentityAndResume(t *testing.T) {
	for _, tc := range freshRelaunchCases() {
		t.Run(string(tc.name), func(t *testing.T) {
			d := tc.descriptor(t)
			if _, err := d.NewSessionArgs(LaunchSpec{SessionID: testUUID}); err == nil {
				t.Fatal("assigned native id accepted")
			}
			if _, err := d.NewSessionArgs(LaunchSpec{SessionID: "provider-owned"}); err == nil {
				t.Fatal("assigned native id accepted")
			}
			if _, err := d.ResumeSessionArgs(ResumeSpec{NativeSessionID: testUUID}); err == nil {
				t.Fatal("resume argv built for an unmeasured resume contract")
			}
			if _, err := d.ResumeSessionArgs(ResumeSpec{}); err == nil {
				t.Fatal("empty resume spec accepted")
			}
			if _, err := d.ResumeSessionArgs(ResumeSpec{NativeSessionID: "chat-7f3a", SkipPermissions: true}); err == nil {
				t.Fatal("resume argv built for an unmeasured resume contract")
			}
		})
	}
}

// Identity capture is unmeasured for every row: a descriptor that grew an
// identity hook or a capture mechanism would claim identity authority for
// hook delivery nobody has observed.
func TestFreshRelaunchDeclaresNoIdentityCapture(t *testing.T) {
	for _, tc := range freshRelaunchCases() {
		t.Run(string(tc.name), func(t *testing.T) {
			d := tc.descriptor(t)
			if got := d.IdentityCaptureMechanisms(); got != nil {
				t.Fatalf("identity capture mechanisms = %v, want none", got)
			}
			if _, ok := any(d).(hookedDescriptor); ok {
				t.Fatal("exposes an identity hook nobody has observed firing")
			}
			if _, ok := any(d).(ResumeIdentityAttestationDescriptor); ok {
				t.Fatal("declared an exact-resume hook source no authenticated run measured")
			}
		})
	}
}

func TestFreshRelaunchEnvironmentAndPathsAreBounded(t *testing.T) {
	for _, tc := range freshRelaunchCases() {
		t.Run(string(tc.name), func(t *testing.T) {
			d := tc.descriptor(t)
			if got := d.Environment(); !reflect.DeepEqual(got, tc.wantEnv) {
				t.Fatalf("environment = %#v, want %#v", got, tc.wantEnv)
			}
			if got := d.PermittedEnvironment(); !reflect.DeepEqual(got, tc.wantAllowed) {
				t.Fatalf("permitted environment = %#v, want %#v", got, tc.wantAllowed)
			}
			if got := d.RequiredConfigPaths(); !reflect.DeepEqual(got, tc.wantPaths) {
				t.Fatalf("config paths = %#v, want %#v", got, tc.wantPaths)
			}
			// Qoder measures the trap this rule exists for: with CI=true its
			// Gemini-derived CLI skips the TUI, takes the headless path, and
			// exits 42 before any interactive wall appears. No launch
			// allowlist inherits one.
			for _, name := range d.PermittedEnvironment() {
				if name == "CI" || name == "GITHUB_ACTIONS" {
					t.Fatalf("launch allowlist carries %s, which forces the measured headless-exit path", name)
				}
			}
		})
	}
}

func TestFreshRelaunchExecutableIdentity(t *testing.T) {
	for _, tc := range freshRelaunchCases() {
		t.Run(string(tc.name), func(t *testing.T) {
			id := tc.descriptor(t).ExecutableIdentity()
			if id != tc.wantIdentity {
				t.Fatalf("executable identity = %+v, want %+v", id, tc.wantIdentity)
			}
			if id.Sufficient() {
				t.Fatal("an interpreted launcher's /proc match was treated as pane identity")
			}
		})
	}
}

// The input contract carries only what a real run measured. Nothing here is
// verified, because no authenticated session has accepted input from any of
// these CLIs — the measured runs stop at a login wall or a trust modal, which
// injected bytes would answer instead of the user.
func TestFreshRelaunchInjectionCarriesNoUnmeasuredValues(t *testing.T) {
	for _, tc := range freshRelaunchCases() {
		t.Run(string(tc.name), func(t *testing.T) {
			got := tc.descriptor(t).Injection()
			if got != tc.wantInject {
				t.Fatalf("injection = %+v, want %+v", got, tc.wantInject)
			}
			if got.Verified {
				t.Fatal("unmeasured input contract marked verified")
			}
			if got.DiscardsInputDuringModal {
				t.Fatal("modal-discard behaviour declared without a measurement")
			}
			if got.AcceptsInputDuringTurn != ActiveTurnUnknown {
				t.Fatalf("active-turn behaviour = %s, want unknown", got.AcceptsInputDuringTurn)
			}
		})
	}
}

func TestFreshRelaunchStopSignalsAndAvailability(t *testing.T) {
	for _, tc := range freshRelaunchCases() {
		t.Run(string(tc.name), func(t *testing.T) {
			d := tc.descriptor(t)
			if err := d.Availability(); err != nil {
				t.Fatalf("availability = %v", err)
			}
			if d.GracefulStopSignal() != syscall.SIGTERM || d.ForceStopSignal() != syscall.SIGKILL {
				t.Fatalf("signals = %v/%v", d.GracefulStopSignal(), d.ForceStopSignal())
			}
		})
	}
}

// A relative executable is refused: the launch path must name the binary on
// disk, not whatever a PATH lookup resolves at launch time.
func TestFreshRelaunchRequiresAbsoluteExecutable(t *testing.T) {
	for _, tc := range freshRelaunchCases() {
		t.Run(string(tc.name), func(t *testing.T) {
			if _, err := tc.construct("bin/agent"); err == nil {
				t.Fatal("relative executable accepted")
			}
		})
	}
}

// mustCline builds the Cline row for the package-wide descriptor tests.
func mustCline(t *testing.T) *FreshRelaunchDescriptor {
	t.Helper()
	d, err := NewCline("/usr/local/bin/cline")
	if err != nil {
		t.Fatal(err)
	}
	return d
}
