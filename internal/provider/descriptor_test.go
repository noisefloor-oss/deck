package provider

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

const testUUID = "cadd0c7c-f370-42c2-9a13-486319db13b5"

// hookedDescriptor is implemented by providers whose identity arrives via a
// hook callback. It is deliberately not on Descriptor: Claude has no hook,
// and a nil-event hook is how an assigned provider would declare that.
type hookedDescriptor interface {
	IdentityHook() IdentityHook
}

func mustClaude(t *testing.T) *ClaudeDescriptor {
	t.Helper()
	d, err := NewClaude("/usr/local/bin/claude")
	if err != nil {
		t.Fatalf("NewClaude: %v", err)
	}
	return d
}

func TestClaudeNewSessionArgsAssignIdentity(t *testing.T) {
	d := mustClaude(t)

	argv, err := d.NewSessionArgs(LaunchSpec{WorkDir: "/workspace/p", SessionID: testUUID})
	if err != nil {
		t.Fatalf("NewSessionArgs: %v", err)
	}
	want := []string{"/usr/local/bin/claude", "--session-id", testUUID}
	if len(argv) != len(want) {
		t.Fatalf("argv = %v, want %v", argv, want)
	}
	for i := range want {
		if argv[i] != want[i] {
			t.Errorf("argv[%d] = %q, want %q", i, argv[i], want[i])
		}
	}
}

func TestClaudeDelegatedLaunchCarriesMeasuredModelAndPrompt(t *testing.T) {
	d := mustClaude(t)
	argv, err := d.NewDelegatedSessionArgs(
		LaunchSpec{SessionID: testUUID},
		DelegatedLaunch{Model: "fable", Prompt: "accept task 295"},
	)
	if err != nil {
		t.Fatalf("NewDelegatedSessionArgs: %v", err)
	}
	want := []string{"/usr/local/bin/claude", "--session-id", testUUID, "--model", "fable", "accept task 295"}
	if strings.Join(argv, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("argv = %q, want %q", argv, want)
	}
}

func TestCodexDelegatedLaunchCarriesMeasuredModelAndPrompt(t *testing.T) {
	d := mustCodex(t)
	argv, err := d.NewDelegatedSessionArgs(
		LaunchSpec{},
		DelegatedLaunch{Model: "gpt-5.6-sol", Prompt: "accept task 295"},
	)
	if err != nil {
		t.Fatalf("NewDelegatedSessionArgs: %v", err)
	}
	want := []string{d.Executable(), "--no-daemon", "--no-alt-screen", "--model", "gpt-5.6-sol", "accept task 295"}
	if strings.Join(argv, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("argv = %q, want %q", argv, want)
	}
}

func TestDelegatedLaunchRejectsUnboundedOrMalformedInput(t *testing.T) {
	for _, delegated := range []DelegatedLaunch{
		{Model: "fable;sh", Prompt: "accept"},
		{Model: "fable", Prompt: ""},
		{Model: "fable", Prompt: strings.Repeat("x", 4097)},
		{Model: "fable", Prompt: "accept\x00task"},
	} {
		if _, err := mustClaude(t).NewDelegatedSessionArgs(LaunchSpec{SessionID: testUUID}, delegated); err == nil {
			t.Errorf("delegated launch accepted malformed input: model=%q prompt-length=%d", delegated.Model, len(delegated.Prompt))
		}
	}
}

// Resume must target an exact session. --continue ("most recent") and
// --fork-session (creates a NEW id) would both break that, so neither may
// appear.
func TestClaudeResumeTargetsAnExactSession(t *testing.T) {
	d := mustClaude(t)

	argv, err := d.ResumeSessionArgs(ResumeSpec{NativeSessionID: testUUID})
	if err != nil {
		t.Fatalf("ResumeSessionArgs: %v", err)
	}
	joined := strings.Join(argv, " ")
	if !strings.Contains(joined, "--resume "+testUUID) {
		t.Errorf("resume argv does not name the exact session: %v", argv)
	}
	for _, forbidden := range []string{"--continue", "-c", "--fork-session"} {
		for _, arg := range argv {
			if arg == forbidden {
				t.Errorf("resume argv contains %q, which does not resume an exact session", forbidden)
			}
		}
	}
}

func TestClaudeRejectsMalformedSessionIDs(t *testing.T) {
	d := mustClaude(t)

	bad := []string{
		"", "   ", "not-a-uuid", "cadd0c7c-f370-42c2-9a13", testUUID + "-extra",
		"cadd0c7cf37042c29a13486319db13b5", // unhyphenated
		"gggggggg-f370-42c2-9a13-486319db13b5",
	}
	for _, id := range bad {
		if _, err := d.NewSessionArgs(LaunchSpec{SessionID: id}); err == nil {
			t.Errorf("NewSessionArgs accepted malformed session id %q", id)
		}
		if _, err := d.ResumeSessionArgs(ResumeSpec{NativeSessionID: id}); err == nil {
			t.Errorf("ResumeSessionArgs accepted malformed session id %q", id)
		}
	}
}

func TestNewClaudeRequiresAbsolutePath(t *testing.T) {
	if _, err := NewClaude("claude"); err == nil {
		t.Error("NewClaude accepted a bare command name; argv[0] must be an absolute path")
	}
}

// Provider credentials live in the provider's own persisted home. Nothing this
// package builds may carry them.
func TestClaudeEnvironmentCarriesNoSecrets(t *testing.T) {
	d := mustClaude(t)
	env := d.Environment()
	if len(env) != 0 {
		t.Errorf("Environment() = %v, want empty: credentials come from the "+
			"provider's own home, not from argv or env we construct", env)
	}
}

// An injection contract may be Verified only after the Phase 0 spike
// measures it. Claiming otherwise would be exactly the unverified assertion
// this product forbids.
func TestClaudeInjectionContractRecordsMeasurements(t *testing.T) {
	inj := mustClaude(t).Injection()

	// Verified only because the Phase 0 spike measured every field against
	// 2.1.220. See docs/provider-contracts/claude.yaml.
	if !inj.Verified {
		t.Error("Claude's injection contract should be Verified after the Phase 0 spike")
	}
	if inj.AcceptsInputDuringTurn != ActiveTurnQueued {
		t.Errorf("active-turn behaviour = %v, want queued (measured: the TUI showed "+
			"\"Press up to edit queued messages\" and processed it exactly once)",
			inj.AcceptsInputDuringTurn)
	}
	if !inj.DiscardsInputDuringModal {
		t.Error("modal behaviour not recorded: measured that a bracketed paste sent " +
			"while the trust picker was open produced nothing at all")
	}
	if inj.SubmitKey != "Enter" || !inj.UseBracketedPaste || !inj.SupportsMultiline {
		t.Errorf("injection contract lost a measured value: %+v", inj)
	}
}

// Codex's values are declarations only because the real 0.145.0 conformance
// spike measured every field. Product code consumes this static contract; it
// does not inspect terminal presentation to rediscover provider state.
func TestCodexInjectionContractRecordsMeasurements(t *testing.T) {
	inj := mustCodex(t).Injection()

	if !inj.Verified {
		t.Error("Codex's injection contract should be Verified after the Phase 0 spike")
	}
	if inj.AcceptsInputDuringTurn != ActiveTurnQueued {
		t.Errorf("active-turn behaviour = %v, want queued", inj.AcceptsInputDuringTurn)
	}
	if !inj.DiscardsInputDuringModal {
		t.Error("modal behaviour not recorded: /permissions discarded bracketed input")
	}
	if inj.SubmitKey != "Enter" || !inj.UseBracketedPaste || !inj.SupportsMultiline {
		t.Errorf("injection contract lost a measured value: %+v", inj)
	}
}

// Kimi has verified identity but an unmeasured injection contract. Claiming
// otherwise would assert a measurement never taken.
func TestKimiUnmeasuredInjectionContractStaysUnverified(t *testing.T) {
	d := mustKimi(t)
	inj := d.Injection()
	if inj.Verified {
		t.Error("Kimi claims a verified injection contract, but the Phase 0 spike " +
			"has only measured its identity contract")
	}
	if inj.AcceptsInputDuringTurn != ActiveTurnUnknown {
		t.Errorf("Kimi declares active-turn behaviour %v without measuring it",
			inj.AcceptsInputDuringTurn)
	}
}

func TestStopSignalsAreProviderPolicy(t *testing.T) {
	d := mustClaude(t)
	if d.GracefulStopSignal() != syscall.SIGTERM {
		t.Errorf("graceful signal = %v, want SIGTERM", d.GracefulStopSignal())
	}
	if d.ForceStopSignal() != syscall.SIGKILL {
		t.Errorf("force signal = %v, want SIGKILL", d.ForceStopSignal())
	}
}

func fakeVersionHelpExecutable(t *testing.T, version, help string, failHelp bool) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "claude")
	versionPath := filepath.Join(dir, "version")
	helpPath := filepath.Join(dir, "help")
	if err := os.WriteFile(versionPath, []byte(version+"\n"), 0o600); err != nil {
		t.Fatalf("write fake Claude version: %v", err)
	}
	if err := os.WriteFile(helpPath, []byte(help), 0o600); err != nil {
		t.Fatalf("write fake Claude help: %v", err)
	}
	helpCase := "/bin/cat \"" + helpPath + "\""
	if failHelp {
		helpCase = "exit 1"
	}
	script := "#!/bin/sh\n[ -z \"$NOISEDECK_TEST_PROVIDER_SECRET\" ] || exit 77\ncase \"$1\" in\n  --version) /bin/cat \"" + versionPath + "\" ;;\n  --help) " + helpCase + " ;;\n  *) exit 2 ;;\nesac\n"
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatalf("write fake Claude: %v", err)
	}
	t.Setenv("NOISEDECK_TEST_PROVIDER_SECRET", "must not reach the probe")
	return path
}

func mustKimi(t *testing.T) *KimiDescriptor {
	t.Helper()
	d, err := NewKimi("/usr/local/bin/kimi", "/home/claude/.kimi-code/config.toml", HookReceiverPath)
	if err != nil {
		t.Fatalf("NewKimi: %v", err)
	}
	return d
}

func mustCodex(t *testing.T) *CodexDescriptor {
	t.Helper()
	d, err := NewCodex("/usr/local/bin/codex", "/home/claude/.codex/hooks.json", HookReceiverPath)
	if err != nil {
		t.Fatalf("NewCodex: %v", err)
	}
	return d
}

// hookProvider is a descriptor whose identity comes from a SessionStart hook.
type hookProvider interface {
	Descriptor
	IdentityHook() IdentityHook
}

// Codex and Kimi both allocate their own id and report it through a
// SessionStart hook: exact identity by retrieval rather than assignment.
func TestHookIdentityProviders(t *testing.T) {
	for _, d := range []hookProvider{mustCodex(t), mustKimi(t)} {
		t.Run(string(d.Name()), func(t *testing.T) { assertHookIdentity(t, d) })
	}
}

func TestCodexHookTimingRecordsLateIdentityAndNativeTrust(t *testing.T) {
	hook := mustCodex(t).IdentityHook()
	if hook.Timing != IdentityHookAfterFirstSubmission {
		t.Errorf("Codex hook timing = %v, want after-first-submission", hook.Timing)
	}
	if !hook.RequiresNativeTrust {
		t.Error("Codex hook must record its provisioning-owned native trust prerequisite")
	}
}

// Kimi's SessionStart event carries source:"startup" but is held until the
// user's first submission (measured on 0.39.0), so the declared timing
// must be after-first-submission: a pane sitting at its prompt has no id to
// report yet, and calling that a broken identity path would be a lie.
func TestKimiHookTimingRecordsFirstSubmissionIdentity(t *testing.T) {
	hook := mustKimi(t).IdentityHook()
	if hook.Timing != IdentityHookAfterFirstSubmission {
		t.Errorf("Kimi hook timing = %v, want after-first-submission", hook.Timing)
	}
	if hook.RequiresNativeTrust {
		t.Error("Kimi hook claims a native trust prerequisite without measurement")
	}
}

func assertHookIdentity(t *testing.T, d hookProvider) {
	t.Helper()

	if err := d.Availability(); err != nil {
		t.Fatalf("%s reports unavailable: %v", d.Name(), err)
	}
	if d.IdentityContract() != IdentityRetrieved {
		t.Errorf("IdentityContract() = %v, want retrieved-by-command", d.IdentityContract())
	}

	hook := d.IdentityHook()
	if hook.Event != "SessionStart" {
		t.Errorf("hook event = %q, want SessionStart", hook.Event)
	}
	if hook.SessionIDField != "session_id" {
		t.Errorf("session id field = %q, want session_id", hook.SessionIDField)
	}
	if hook.ConfigPath == "" {
		t.Error("hook config path is empty; deployment would not know where to install it")
	}

	// The hook config must be listed as required: losing it silently breaks
	// identity capture for every new session.
	var listed bool
	for _, p := range d.RequiredConfigPaths() {
		if p == hook.ConfigPath {
			listed = true
		}
	}
	if !listed {
		t.Errorf("hook config %q is not in RequiredConfigPaths() %v",
			hook.ConfigPath, d.RequiredConfigPaths())
	}

	// Assigning an id is an error, not a silent no-op: a caller that believed
	// it assigned one would record the wrong identity and never resume.
	if _, err := d.NewSessionArgs(LaunchSpec{SessionID: testUUID}); err == nil {
		t.Errorf("NewSessionArgs silently accepted an id %s cannot honour", d.Name())
	}
	argv, err := d.NewSessionArgs(LaunchSpec{})
	if err != nil {
		t.Fatalf("NewSessionArgs: %v", err)
	}
	want := []string{d.Executable()}
	if d.Name() == Codex {
		want = append(want, "--no-daemon", "--no-alt-screen")
	}
	if strings.Join(argv, "\x00") != strings.Join(want, "\x00") {
		t.Errorf("argv = %q, want %q", argv, want)
	}
}

// Kimi's ids are NOT bare UUIDs — they carry a "session_" prefix — so they must
// be stored and replayed verbatim rather than parsed.
func TestKimiResumeTargetsAnExactSession(t *testing.T) {
	d := mustKimi(t)
	const id = "session_772662c7-b482-4700-95fa-4f4495fbce00"

	argv, err := d.ResumeSessionArgs(ResumeSpec{NativeSessionID: id})
	if err != nil {
		t.Fatalf("ResumeSessionArgs: %v", err)
	}
	if strings.Join(argv, " ") != "/usr/local/bin/kimi -S "+id {
		t.Errorf("argv = %v, want the exact id after -S", argv)
	}
	for _, arg := range argv {
		if arg == "--continue" || arg == "-c" {
			t.Error("resume argv uses --continue, which is not exact identity")
		}
	}

	for _, id := range []string{"", "   "} {
		if _, err := d.ResumeSessionArgs(ResumeSpec{NativeSessionID: id}); err == nil {
			t.Errorf("ResumeSessionArgs accepted empty id %q", id)
		}
	}
	// Any other recorded value is the CLI's to judge: replay it verbatim
	// rather than stranding a session whose shape we do not recognize.
	for _, id := range []string{"-S", "garbage", "772662c7-b482-4700-95fa-4f4495fbce00"} {
		argv, err := d.ResumeSessionArgs(ResumeSpec{NativeSessionID: id})
		if err != nil {
			t.Fatalf("ResumeSessionArgs refused recorded id %q: %v", id, err)
		}
		if !containsArg(argv, id) {
			t.Errorf("resume argv = %v, want the recorded id %q verbatim", argv, id)
		}
	}
}

// --last resumes "the most recent recorded session", which is not exact
// identity and must never appear.
func TestCodexResumeTargetsAnExactSession(t *testing.T) {
	d := mustCodex(t)

	argv, err := d.ResumeSessionArgs(ResumeSpec{NativeSessionID: "019fb4f0-e1f4-7e82-ad6a-d08ed5b0e496"})
	if err != nil {
		t.Fatalf("ResumeSessionArgs: %v", err)
	}
	joined := strings.Join(argv, " ")
	if !strings.Contains(joined, "resume 019fb4f0-e1f4-7e82-ad6a-d08ed5b0e496") {
		t.Errorf("resume argv does not name the exact session: %v", argv)
	}
	for _, arg := range argv {
		if arg == "--last" {
			t.Error("resume argv contains --last, which is not exact identity")
		}
	}

	for _, id := range []string{"", "   "} {
		if _, err := d.ResumeSessionArgs(ResumeSpec{NativeSessionID: id}); err == nil {
			t.Errorf("ResumeSessionArgs accepted empty id %q", id)
		}
	}
	// Any other recorded value is the CLI's to judge: replay it verbatim
	// rather than stranding a session whose shape we do not recognize.
	for _, id := range []string{"--last", "garbage", "019fb4f0-e1f4-7e82-ad6a-d08ed5b0e49z"} {
		argv, err := d.ResumeSessionArgs(ResumeSpec{NativeSessionID: id})
		if err != nil {
			t.Fatalf("ResumeSessionArgs refused recorded id %q: %v", id, err)
		}
		if !containsArg(argv, id) {
			t.Errorf("resume argv = %v, want the recorded id %q verbatim", argv, id)
		}
	}
}

// Every descriptor declares a permitted environment and the configuration
// paths its identity and credentials need.
func TestDescriptorsDeclareEnvironmentAndConfigContracts(t *testing.T) {
	for _, d := range []Descriptor{mustClaude(t), mustCodex(t), mustKimi(t), mustPi(t)} {
		t.Run(string(d.Name()), func(t *testing.T) {
			permitted := d.PermittedEnvironment()
			if len(permitted) == 0 {
				t.Error("no permitted environment declared; the service environment " +
					"would have to be inherited wholesale")
			}
			for _, name := range permitted {
				if strings.Contains(strings.ToUpper(name), "KEY") ||
					strings.Contains(strings.ToUpper(name), "TOKEN") ||
					strings.Contains(strings.ToUpper(name), "SECRET") {
					t.Errorf("permitted environment includes %q, which looks like a credential", name)
				}
			}
			if len(d.RequiredConfigPaths()) == 0 {
				t.Error("no required config paths declared; a deployment would not " +
					"know what to persist for native resume to survive")
			}
		})
	}
}

// The active-turn behaviour is what the 13.2 gate depends on and cannot be
// guessed, so it must start unknown.
func TestActiveTurnBehaviourStrings(t *testing.T) {
	if ActiveTurnQueued.String() != "queued" || ActiveTurnUnknown.String() != "unknown" {
		t.Error("ActiveTurnBehaviour strings are wrong")
	}
	if ActiveTurnRejected.String() != "rejected" || ActiveTurnInterrupts.String() != "interrupts" {
		t.Error("ActiveTurnBehaviour strings are wrong")
	}
}

func TestIdentityContractStrings(t *testing.T) {
	for c, want := range map[IdentityContract]string{
		IdentityAssigned:      "assigned-at-launch",
		IdentityRetrieved:     "retrieved-by-command",
		IdentityNone:          "none-by-contract",
		IdentityFreshRelaunch: "fresh-relaunch-only",
		IdentityUnavailable:   "unavailable",
	} {
		if got := c.String(); got != want {
			t.Errorf("IdentityContract(%d).String() = %q, want %q", c, got, want)
		}
	}
}

func TestDescriptorsDeclareOnlyTheirTrustworthyIdentityCaptureMechanisms(t *testing.T) {
	cases := []struct {
		d    IdentityCaptureDescriptor
		want []IdentityCaptureMechanism
	}{
		{mustClaude(t), []IdentityCaptureMechanism{IdentityCaptureAssignedAtLaunch, IdentityCaptureStructuredNativeHook}},
		{mustPi(t), []IdentityCaptureMechanism{IdentityCaptureStructuredNativeHook}},
		{commandCodeDescriptor(t), []IdentityCaptureMechanism{IdentityCaptureStructuredNativeHook}},
		{mustCodex(t), []IdentityCaptureMechanism{IdentityCaptureStructuredNativeHook}},
		{devinDescriptor(t), []IdentityCaptureMechanism{IdentityCaptureStructuredNativeHook}},
		{mustKimi(t), []IdentityCaptureMechanism{IdentityCaptureStructuredNativeHook}},
		{mustKilo(t), []IdentityCaptureMechanism{IdentityCaptureStructuredNativeHook}},
		{mustOmp(t), []IdentityCaptureMechanism{IdentityCaptureStructuredNativeHook}},
		{mustOpencode(t), []IdentityCaptureMechanism{IdentityCaptureStructuredNativeHook}},
		{mustShell(t), nil},
		{mustCline(t), nil},
	}
	for _, tc := range cases {
		t.Run(string(tc.d.Name()), func(t *testing.T) {
			got := tc.d.IdentityCaptureMechanisms()
			if len(got) != len(tc.want) {
				t.Fatalf("capture mechanisms = %v, want %v", got, tc.want)
			}
			for i := range tc.want {
				if got[i] != tc.want[i] {
					t.Fatalf("capture mechanisms = %v, want %v", got, tc.want)
				}
			}
			for _, mechanism := range got {
				if mechanism == IdentityCaptureHostAttestedZeroArgTool ||
					mechanism == IdentityCaptureDocumentedAPI {
					t.Fatalf("provider advertises unimplemented capture mechanism %q", mechanism)
				}
			}
		})
	}
}

func TestFutureIdentityCaptureMechanismsAreRepresentable(t *testing.T) {
	if IdentityCaptureHostAttestedZeroArgTool == "" || IdentityCaptureDocumentedAPI == "" {
		t.Fatal("future trustworthy capture mechanism kinds must be representable")
	}
	if IdentityCaptureHostAttestedZeroArgTool == IdentityCaptureDocumentedAPI {
		t.Fatal("future capture mechanisms must remain distinguishable")
	}
}

// Nothing in /proc identifies an interpreted provider. Every shortcut this
// test forbids was actually attempted during the Phase 0 spike.
func TestExecutableIdentityIsProviderSpecific(t *testing.T) {
	t.Run("claude is a native binary", func(t *testing.T) {
		id := mustClaude(t).ExecutableIdentity()
		if id.Interpreted {
			t.Error("claude marked interpreted; it is a native binary")
		}
		if !id.Sufficient() {
			t.Error("matching /proc/exe should be sufficient for a native binary")
		}
	})

	for _, d := range []Descriptor{mustCodex(t), mustKimi(t), mustPi(t)} {
		t.Run(string(d.Name())+" has no usable executable evidence", func(t *testing.T) {
			id := d.ExecutableIdentity()
			if !id.Interpreted {
				t.Fatalf("%s not marked interpreted; /proc/exe is the node binary", d.Name())
			}
			if id.Sufficient() {
				t.Error("matching the interpreter must NOT be sufficient")
			}
			if id.ExpectedProcExe != "/usr/local/bin/node" {
				t.Errorf("/proc/exe = %q, want the node interpreter", id.ExpectedProcExe)
			}
			// The measured cmdline must NOT contain the resolved target — that
			// is the whole reason LaunchTarget is not runtime evidence.
			if id.ObservedCmdline == "" {
				t.Fatal("no measured cmdline recorded")
			}
			if strings.Contains(id.ObservedCmdline, id.LaunchTarget) {
				t.Errorf("cmdline %q contains the launch target %q; if that is now "+
					"true, cmdline became usable evidence and this reasoning changes",
					id.ObservedCmdline, id.LaunchTarget)
			}
		})
	}

	codex, kimi := mustCodex(t).ExecutableIdentity(), mustKimi(t).ExecutableIdentity()
	if codex.ExpectedProcExe != kimi.ExpectedProcExe {
		t.Error("precondition changed: the Node providers no longer share an " +
			"interpreter, so the Sufficient() reasoning needs revisiting")
	}
	if codex.LaunchTarget == kimi.LaunchTarget {
		t.Error("codex and kimi report the same launch target")
	}
	// Kimi rewrites argv[0] entirely — the sharpest illustration that a
	// process's self-reported command line is not identity.
	if strings.Contains(kimi.ObservedCmdline, "/") {
		t.Errorf("kimi cmdline %q now contains a path; it was measured as a bare "+
			"rewritten argv[0], which is why cmdline is not trusted", kimi.ObservedCmdline)
	}
}

// --- The console's permission-skipping launch setting ---
//
// Each CLI's flag was verified against the installed binary's own help
// (claude 2.1.220, codex-cli 0.145.0, kimi 0.29.2, opencode 1.18.4). The
// tests pin the EXACT flag: a wrong-but-plausible one (claude's
// --allow-dangerously-skip-permissions merely enables the bypass;
// codex's -a only moves the approval threshold; kimi's --yolo still asks
// questions) would launch a mode the user did not choose.

func TestSkipPermissionsAddsEachProvidersOwnFlag(t *testing.T) {
	kimi, err := NewKimi("/usr/local/bin/kimi", "/home/claude/.kimi-code/config.toml", HookReceiverPath)
	if err != nil {
		t.Fatalf("NewKimi: %v", err)
	}
	codex, err := NewCodex("/usr/local/bin/codex", "/home/claude/.codex/hooks.json", HookReceiverPath)
	if err != nil {
		t.Fatalf("NewCodex: %v", err)
	}
	cases := []struct {
		name string
		argv func(LaunchSpec) ([]string, error)
		spec LaunchSpec
		want string
	}{
		{"claude", mustClaude(t).NewSessionArgs, LaunchSpec{SessionID: testUUID}, "--dangerously-skip-permissions"},
		{"codex", codex.NewSessionArgs, LaunchSpec{}, "--dangerously-bypass-approvals-and-sandbox"},
		{"kimi", kimi.NewSessionArgs, LaunchSpec{}, "--auto"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			off := c.spec
			argv, err := c.argv(off)
			if err != nil {
				t.Fatalf("argv (off): %v", err)
			}
			for _, a := range argv {
				if strings.Contains(a, "dangerous") || strings.Contains(a, "bypass") ||
					a == "--auto" || a == "--yolo" || a == "-y" {
					t.Fatalf("argv with the setting OFF carries %q: %v", a, argv)
				}
			}
			on := c.spec
			on.SkipPermissions = true
			argv, err = c.argv(on)
			if err != nil {
				t.Fatalf("argv (on): %v", err)
			}
			found := false
			for _, a := range argv {
				if a == c.want {
					found = true
				}
			}
			if !found {
				t.Fatalf("argv = %v, want it to carry %q", argv, c.want)
			}
			// The flag is ADDITIVE: identity arguments must survive it.
			if c.name == "claude" {
				if len(argv) < 3 || argv[1] != "--session-id" || argv[2] != testUUID {
					t.Fatalf("claude argv lost its assigned identity: %v", argv)
				}
			}
		})
	}
}

// opencode's flag is a TOP-LEVEL option, so it reaches the default
// command the descriptor launches (measured, 1.18.4). It is weaker than
// claude's total bypass — explicit denies in opencode.json still hold —
// but it is the CLI's own permission-skipping mode, which is what the
// setting promises.
func TestOpencodeSkipPermissionsUsesAuto(t *testing.T) {
	d := mustOpencode(t)
	argv, err := d.NewSessionArgs(LaunchSpec{})
	if err != nil {
		t.Fatalf("NewSessionArgs: %v", err)
	}
	if len(argv) != 1 {
		t.Fatalf("argv with the setting OFF = %v, want the bare executable", argv)
	}
	argv, err = d.NewSessionArgs(LaunchSpec{SkipPermissions: true})
	if err != nil {
		t.Fatalf("NewSessionArgs with the setting ON: %v", err)
	}
	if len(argv) != 2 || argv[1] != "--auto" {
		t.Fatalf("argv = %v, want it to carry --auto", argv)
	}
}

// Resume carries the mode too. Recovery is unattended: a resumed
// session that lost the flag would stop on a prompt nobody is watching,
// which is exactly the failure the setting exists to avoid.
func TestSkipPermissionsSurvivesResume(t *testing.T) {
	kimi, err := NewKimi("/usr/local/bin/kimi", "/home/claude/.kimi-code/config.toml", HookReceiverPath)
	if err != nil {
		t.Fatalf("NewKimi: %v", err)
	}
	codex, err := NewCodex("/usr/local/bin/codex", "/home/claude/.codex/hooks.json", HookReceiverPath)
	if err != nil {
		t.Fatalf("NewCodex: %v", err)
	}
	cases := []struct {
		name string
		argv func(ResumeSpec) ([]string, error)
		id   string
		want string
	}{
		{"claude", mustClaude(t).ResumeSessionArgs, testUUID, "--dangerously-skip-permissions"},
		{"codex", codex.ResumeSessionArgs, "01998e0f-7c5f-7a12-9b5e-2f7f8f1a4b3c", "--dangerously-bypass-approvals-and-sandbox"},
		{"kimi", kimi.ResumeSessionArgs, "session_772662c7-b482-4700-95fa-4f4495fbce00", "--auto"},
		{"opencode", mustOpencode(t).ResumeSessionArgs, "ses_032d1af39ffejke3TwnEL00DxZ", "--auto"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			off, err := c.argv(ResumeSpec{NativeSessionID: c.id})
			if err != nil {
				t.Fatalf("resume argv (off): %v", err)
			}
			for _, a := range off {
				if a == c.want {
					t.Fatalf("resume argv carries %q with the setting OFF: %v", c.want, off)
				}
			}
			on, err := c.argv(ResumeSpec{NativeSessionID: c.id, SkipPermissions: true})
			if err != nil {
				t.Fatalf("resume argv (on): %v", err)
			}
			found := false
			for _, a := range on {
				if a == c.want {
					found = true
				}
			}
			if !found {
				t.Fatalf("resume argv = %v, want it to carry %q", on, c.want)
			}
			// The exact recorded identity must survive the addition.
			hasID := false
			for _, a := range on {
				if a == c.id {
					hasID = true
				}
			}
			if !hasID {
				t.Fatalf("resume argv lost the native id: %v", on)
			}
		})
	}
}
