package session

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/noisefloor-oss/deck/internal/db"
	"github.com/noisefloor-oss/deck/internal/hookcapture"
	"github.com/noisefloor-oss/deck/internal/provider"
	"github.com/noisefloor-oss/deck/internal/tmux"
)

// Integration tests against a REAL tmux server on a private per-test socket
// (never the user's namespace) and a real SQLite control
// database. The provider is faked — release claims about real CLIs belong to
// the acceptance suite (PRD §3.7) — but process launch, signalling, exit
// observation, and evidence recording are all real.

// fakeDescriptor implements provider.Descriptor for a shell stand-in. It
// honors the real contracts: NewSessionArgs returns the COMPLETE argv with
// argv[0] the executable, and PermittedEnvironment is an allowlist.
type fakeDescriptor struct {
	name         provider.Name
	executable   string
	args         []string
	contract     provider.IdentityContract
	unavailable  error
	graceful     syscall.Signal
	force        syscall.Signal
	permittedEnv []string
	extraEnv     []string
	hook         *provider.IdentityHook
	execIdentity *provider.ExecutableIdentity
}

type fakeDelegationDescriptor struct {
	*fakeDescriptor
	delegated provider.DelegatedLaunch
}

func (d *fakeDelegationDescriptor) NewDelegatedSessionArgs(spec provider.LaunchSpec,
	delegated provider.DelegatedLaunch) ([]string, error) {
	d.delegated = delegated
	return d.NewSessionArgs(spec)
}

func (f *fakeDescriptor) Name() provider.Name                         { return f.name }
func (f *fakeDescriptor) Executable() string                          { return f.executable }
func (f *fakeDescriptor) IdentityContract() provider.IdentityContract { return f.contract }
func (f *fakeDescriptor) Availability() error                         { return f.unavailable }
func (f *fakeDescriptor) Environment() []string                       { return f.extraEnv }
func (f *fakeDescriptor) PermittedEnvironment() []string              { return f.permittedEnv }
func (f *fakeDescriptor) RequiredConfigPaths() []string               { return nil }
func (f *fakeDescriptor) Injection() provider.InjectionContract       { return provider.InjectionContract{} }
func (f *fakeDescriptor) GracefulStopSignal() syscall.Signal          { return f.graceful }
func (f *fakeDescriptor) ForceStopSignal() syscall.Signal             { return f.force }
func (f *fakeDescriptor) IdentityHook() provider.IdentityHook {
	if f.hook == nil {
		return provider.IdentityHook{}
	}
	return *f.hook
}
func (f *fakeDescriptor) NewSessionArgs(provider.LaunchSpec) ([]string, error) {
	return append([]string{f.executable}, f.args...), nil
}

func (f *fakeDescriptor) ResumeSessionArgs(provider.ResumeSpec) ([]string, error) {
	return nil, errors.New("fake: no resume")
}
func (f *fakeDescriptor) ExecutableIdentity() provider.ExecutableIdentity {
	if f.execIdentity != nil {
		return *f.execIdentity
	}
	return provider.ExecutableIdentity{ExpectedProcExe: f.executable}
}

func shellScriptIdentity(t *testing.T) *provider.ExecutableIdentity {
	t.Helper()
	interpreter, err := filepath.EvalSymlinks("/bin/sh")
	if err != nil {
		t.Fatalf("resolve test shell: %v", err)
	}
	return &provider.ExecutableIdentity{Interpreted: true, ExpectedProcExe: interpreter}
}

func eagerRetrievedDescriptor(t *testing.T, cli string) *fakeDescriptor {
	t.Helper()
	return &fakeDescriptor{
		name: provider.Codex, executable: cli, contract: provider.IdentityRetrieved,
		graceful: syscall.SIGTERM, force: syscall.SIGKILL,
		execIdentity: shellScriptIdentity(t),
		hook: &provider.IdentityHook{
			Event: "SessionStart", SessionIDField: "session_id",
			ReceiverPath: provider.HookReceiverPath,
			Timing:       provider.IdentityHookAtProcessLaunch,
			ConfigPath: writeHookConfig(t,
				`{"hooks":{"SessionStart":[{"hooks":[{"type":"command","command":"/usr/local/bin/noisedeck-provider-hook"}]}]}}`),
		},
	}
}

func readLaunchToken(t *testing.T, path string) string {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		raw, err := os.ReadFile(path)
		if err == nil && strings.TrimSpace(string(raw)) != "" {
			return strings.TrimSpace(string(raw))
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("launch did not expose its test capture token")
	return ""
}

func newTestTmux(t *testing.T) *tmux.Client {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatalf("chmod socket dir: %v", err)
	}
	c, err := tmux.NewClient(tmux.Config{
		SocketPath: filepath.Join(dir, "ac.sock"),
		Timeout:    10 * time.Second,
	})
	if err != nil {
		t.Fatalf("tmux.NewClient: %v", err)
	}
	t.Cleanup(func() { _ = c.KillServer(context.Background()) })
	return c
}

func newTestEnv(t *testing.T, desc provider.Descriptor) (*Manager, *Repository, *tmux.Client, string) {
	t.Helper()
	d, err := db.Open(context.Background(), db.Config{
		Path: filepath.Join(t.TempDir(), "control.db"),
	})
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })

	const projectID = "prj-test"
	err = d.WithTx(context.Background(), func(ctx context.Context, tx *sql.Tx) error {
		const profileID = "00000000000000000000000000000001"
		now := db.FormatTime(time.Now())
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO git_identity_profiles (id, name, email, revision, created_at, updated_at)
			VALUES (?, 'Test Author', 'test@example.invalid', 1, ?, ?)`, profileID, now, now); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE git_identity_catalog SET default_profile_id = ?, revision = 1, updated_at = ?
			WHERE singleton = 1`, profileID, now); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO projects (
				id, name, canonical_path, git_author_name, git_author_email,
				git_identity_profile_id, git_identity_profile_revision, git_identity_setup_revision,
				created_at, last_used_at
			) VALUES (?, 'test', ?, 'Test Author', 'test@example.invalid', ?, 1, 1, ?, ?)`,
			projectID, t.TempDir(), profileID, now, now); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `
			INSERT INTO project_git_identity_snapshots (
				project_id, revision, profile_id, profile_revision, name, email, created_at
			) VALUES (?, 1, ?, 1, 'Test Author', 'test@example.invalid', ?)`, projectID, profileID, now)
		return err
	})
	if err != nil {
		t.Fatalf("insert project: %v", err)
	}

	tc := newTestTmux(t)
	repo := NewRepository(d)
	mgr := NewManager(repo, tc, map[provider.Name]provider.Descriptor{desc.Name(): desc}, "actest-")
	return mgr, repo, tc, projectID
}

func sleeperDescriptor() *fakeDescriptor {
	return &fakeDescriptor{
		name:       provider.Name("claude"),
		executable: "/bin/sh",
		// The compound command stops dash's tail-exec optimization from
		// replacing sh: like a real provider, the launched executable STAYS
		// the pane process, and dies FROM the group signal (the signal
		// evidence shape).
		args:     []string{"-c", "sleep 300; exit 0"},
		contract: provider.IdentityAssigned,
		graceful: syscall.SIGTERM,
		force:    syscall.SIGKILL,
		// PATH so the shell can find binaries; nothing else crosses over.
		permittedEnv: []string{"PATH", "HOME"},
	}
}

func auditTypes(t *testing.T, repo *Repository, sessionID string) []string {
	t.Helper()
	rows, err := repo.db.Reader().Query(
		`SELECT event_type FROM audit_events WHERE session_id = ? ORDER BY id`, sessionID)
	if err != nil {
		t.Fatalf("query audit: %v", err)
	}
	defer rows.Close()
	var types []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatalf("scan audit: %v", err)
		}
		types = append(types, s)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate audit: %v", err)
	}
	return types
}

func TestCreateRecordsAccurateIdentity(t *testing.T) {
	mgr, repo, tc, projectID := newTestEnv(t, sleeperDescriptor())
	ctx := context.Background()

	s, err := mgr.Create(ctx, CreateSpec{ProjectID: projectID, Provider: "claude", WorkDir: t.TempDir()})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if s.Status != StatusRunning {
		t.Fatalf("status = %s, want running", s.Status)
	}
	if s.NativeSessionID == nil || *s.NativeSessionID != s.ID {
		t.Fatalf("assigned contract must record the assigned id as native id")
	}

	// The recorded tuple must match independent observation of the pane.
	info, err := tc.PaneInfoByID(ctx, *s.TmuxPaneID)
	if err != nil {
		t.Fatalf("observe pane: %v", err)
	}
	if info.PID != *s.PanePID || info.ProcessGroupID != *s.ProcessGroupID ||
		info.StartTicks != *s.StartTicks || info.Marker != *s.IdentityMarker ||
		info.Generation != s.Generation {
		t.Fatalf("recorded tuple diverges from observed pane:\nrec %+v\nobs %+v", s, info)
	}

	// And the row read back must agree with what Create returned.
	got, err := repo.Get(ctx, s.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != StatusRunning || *got.TmuxPaneID != *s.TmuxPaneID {
		t.Fatalf("persisted row diverges: %+v", got)
	}

	// The expected executable is derived at launch, not configured: for a
	// native provider it must be the fully resolved launch path (a stable
	// symlink would go stale on the first upgrade).
	if s.ExpectedExecutable == nil || *s.ExpectedExecutable == "" {
		t.Fatal("expected_executable was not derived at launch")
	}
	if resolved, err := filepath.EvalSymlinks("/bin/sh"); err == nil && *s.ExpectedExecutable != resolved {
		t.Fatalf("expected_executable = %q, want resolved launch path %q", *s.ExpectedExecutable, resolved)
	}

	types := auditTypes(t, repo, s.ID)
	if len(types) != 2 || types[0] != AuditCreated || types[1] != AuditRunning {
		t.Fatalf("audit trail = %v, want [created running]", types)
	}
}

func TestCreateDelegatedRecordsDisplayNameAndUsesDelegationContract(t *testing.T) {
	desc := &fakeDelegationDescriptor{fakeDescriptor: sleeperDescriptor()}
	mgr, repo, _, projectID := newTestEnv(t, desc)
	ctx := context.Background()

	created, err := mgr.CreateDelegated(ctx, CreateSpec{
		ProjectID: projectID, Provider: provider.Claude, WorkDir: t.TempDir(),
		DisplayName: "reviewer · fable",
	}, provider.DelegatedLaunch{Model: "fable", Prompt: "accept task 295"})
	if err != nil {
		t.Fatalf("CreateDelegated: %v", err)
	}
	if desc.delegated.Model != "fable" || desc.delegated.Prompt != "accept task 295" {
		t.Fatalf("delegated launch = %+v", desc.delegated)
	}
	stored, err := repo.Get(ctx, created.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if stored.DisplayName != "reviewer · fable" {
		t.Fatalf("display name = %q", stored.DisplayName)
	}
}

func TestCreateDelegatedRefusesProviderWithoutMeasuredContract(t *testing.T) {
	mgr, _, _, projectID := newTestEnv(t, sleeperDescriptor())
	_, err := mgr.CreateDelegated(context.Background(), CreateSpec{
		ProjectID: projectID, Provider: provider.Claude, WorkDir: t.TempDir(),
	}, provider.DelegatedLaunch{Model: "fable", Prompt: "accept task 295"})
	if !errors.Is(err, ErrProviderNotLaunchable) {
		t.Fatalf("CreateDelegated error = %v, want ErrProviderNotLaunchable", err)
	}
}

func TestUnconfinedControlEnvIncludesProcessCapability(t *testing.T) {
	mgr := &Manager{}
	mgr.SetUnconfinedCapacityRouting("http://127.0.0.1:8837/", "test-capability")
	env := strings.Join(mgr.unconfinedControlEnv("project-1", "session-1"), "\n")
	for _, want := range []string{
		"NOISEDECK_CONTROL_URL=http://127.0.0.1:8837",
		"NOISEDECK_EXPERIMENT_CAPABILITY=test-capability",
		"NOISEDECK_PROJECT_ID=project-1",
		"NOISEDECK_SESSION_ID=session-1",
	} {
		if !strings.Contains(env, want) {
			t.Errorf("environment missing %q: %s", want, env)
		}
	}
}

// The pane process must start with exactly the allowlisted environment: the
// service's environment routinely holds credentials for unrelated tools,
// and none of it may cross into a provider un-permitted. The retired
// NOISEDECK_BROWSER_WS is set here to hold that line: a host that still
// exports it gets no forwarding, so nothing in a pane can keep reading it.
func TestLaunchEnvironmentIsAllowlisted(t *testing.T) {
	t.Setenv("NOISEDECK_TEST_SENTINEL_SECRET", "leaked")
	t.Setenv("NOISE_BROWSER_WS", "ws://127.0.0.1:3000/")
	t.Setenv("NOISEDECK_BROWSER_WS", "ws://127.0.0.1:3000/")
	outFile := filepath.Join(t.TempDir(), "env.txt")

	d := sleeperDescriptor()
	d.args = []string{"-c", "env > " + outFile + "; sleep 300; exit 0"}
	d.extraEnv = []string{"NOISEDECK_TEST_EXTRA=present"}
	mgr, _, _, projectID := newTestEnv(t, d)
	mgr.SetHarnessLoginEnvironment(func(context.Context, provider.Descriptor) []string {
		return []string{"NOISEDECK_LOGIN_SOCKET=/fixture/login.sock"}
	})
	mgr.SetProjectPreviewSocket("/fixture/preview.sock")
	ctx := context.Background()

	s, err := mgr.Create(ctx, CreateSpec{ProjectID: projectID, Provider: "claude", WorkDir: t.TempDir()})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	stopCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	t.Cleanup(func() { _ = mgr.Stop(stopCtx, s.ID, true) })

	var env string
	deadline := time.Now().Add(30 * time.Second)
	for {
		raw, err := os.ReadFile(outFile)
		if err == nil && len(raw) > 0 {
			env = string(raw)
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("pane never wrote its environment")
		}
		time.Sleep(50 * time.Millisecond)
	}
	if strings.Contains(env, "NOISEDECK_TEST_SENTINEL_SECRET") {
		t.Fatalf("un-permitted service variable leaked into the pane:\n%s", env)
	}
	if !strings.Contains(env, "NOISE_BROWSER_WS=ws://127.0.0.1:3000/") {
		t.Fatalf("user-enabled browser endpoint missing from pane environment:\n%s", env)
	}
	if strings.Contains(env, "NOISEDECK_BROWSER_WS") {
		t.Fatalf("retired browser endpoint name reached the pane environment:\n%s", env)
	}
	if !strings.Contains(env, "PATH=") {
		t.Fatalf("allowlisted PATH missing from pane environment:\n%s", env)
	}
	if !strings.Contains(env, "NOISEDECK_TEST_EXTRA=present") {
		t.Fatalf("provider environment addition missing:\n%s", env)
	}
	if !strings.Contains(env, "NOISEDECK_LOGIN_SOCKET=/fixture/login.sock") {
		t.Fatal("native browser registration environment missing")
	}
	if !strings.Contains(env, "NOISEDECK_PROJECT_PREVIEWS_METADATA_SOCKET=/fixture/preview.sock") {
		t.Fatal("new pane did not receive preview socket location")
	}

}

// The pane's TERM belongs to tmux, never to the service. A backend running
// under a supervisor often has TERM=dumb or none; a TUI launched with that
// loses color and cursor-control capabilities — even when the descriptor lists
// TERM as a permitted variable.
func TestLaunchTermComesFromTmuxNotService(t *testing.T) {
	t.Setenv("TERM", "dumb")
	outFile := filepath.Join(t.TempDir(), "env.txt")

	d := sleeperDescriptor()
	d.permittedEnv = []string{"PATH", "TERM"} // TERM permitted — still not carried
	d.args = []string{"-c", "env > " + outFile + "; sleep 300; exit 0"}
	mgr, _, tc, projectID := newTestEnv(t, d)
	ctx := context.Background()

	s, err := mgr.Create(ctx, CreateSpec{ProjectID: projectID, Provider: "claude", WorkDir: t.TempDir()})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	stopCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	t.Cleanup(func() { _ = mgr.Stop(stopCtx, s.ID, true) })

	var env string
	deadline := time.Now().Add(30 * time.Second)
	for {
		raw, err := os.ReadFile(outFile)
		if err == nil && len(raw) > 0 {
			env = string(raw)
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("pane never wrote its environment")
		}
		time.Sleep(50 * time.Millisecond)
	}

	want, err := tc.DefaultTerminal(ctx)
	if err != nil {
		t.Fatalf("DefaultTerminal: %v", err)
	}
	if !strings.Contains(env, "TERM="+want+"\n") {
		t.Fatalf("pane TERM is not tmux's default-terminal %q:\n%s", want, env)
	}
	if strings.Contains(env, "TERM=dumb") {
		t.Fatalf("service TERM leaked into the pane:\n%s", env)
	}
}

// A launch whose pane is already dead at first observation must be recorded
// with its preserved evidence — never as running with fabricated tuple
// members. The pane may also still be alive at that instant; both outcomes
// are legitimate, and the invariant is that whatever was recorded matches
// what was observed.
func TestCreateWithInstantExitNeverFabricatesRunning(t *testing.T) {
	d := sleeperDescriptor()
	d.args = []string{"-c", "exit 7"}
	mgr, repo, tc, projectID := newTestEnv(t, d)
	ctx := context.Background()

	s, err := mgr.Create(ctx, CreateSpec{ProjectID: projectID, Provider: "claude", WorkDir: t.TempDir()})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	switch s.Status {
	case StatusExited:
		if s.ExitCode == nil || *s.ExitCode != 7 {
			t.Fatalf("early exit recorded code %v, want 7", s.ExitCode)
		}
		if s.PanePID != nil || s.StartTicks != nil {
			t.Fatalf("early exit must not fabricate live tuple members: %+v", s)
		}
	case StatusRunning:
		// Died just after observation: the stop path must record honestly.
		deadline := time.Now().Add(30 * time.Second)
		for {
			info, err := tc.PaneInfoByID(ctx, *s.TmuxPaneID)
			if err != nil {
				t.Fatalf("observe: %v", err)
			}
			if info.Dead {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("pane never died")
			}
			time.Sleep(50 * time.Millisecond)
		}
		if err := mgr.Stop(ctx, s.ID, false); err != nil {
			t.Fatalf("Stop on dead pane: %v", err)
		}
		got, _ := repo.Get(ctx, s.ID)
		if got.Status != StatusExited || got.ExitCode == nil || *got.ExitCode != 7 {
			t.Fatalf("status=%s code=%v, want exited 7", got.Status, got.ExitCode)
		}
	default:
		t.Fatalf("status = %s, want exited or running", s.Status)
	}
}

func TestCreateRefusesRetrievedContractProvider(t *testing.T) {
	execPath := gatePassingCLI(t)
	d := &fakeDescriptor{
		name:       provider.Codex,
		executable: execPath,
		contract:   provider.IdentityRetrieved,
		hook: &provider.IdentityHook{
			Event:          "SessionStart",
			SessionIDField: "session_id",
			ReceiverPath:   provider.HookReceiverPath,
			ConfigPath:     writeHookConfig(t, `{"hooks":{"SessionStart":[{"hooks":[{"type":"command","command":"/usr/local/bin/noisedeck-provider-hook"}]}]}}`),
		},
	}
	mgr, _, _, projectID := newTestEnv(t, d)

	_, err := mgr.Create(context.Background(), CreateSpec{ProjectID: projectID, Provider: "codex", WorkDir: t.TempDir()})
	if !errors.Is(err, ErrProviderNotLaunchable) {
		t.Fatalf("err = %v, want ErrProviderNotLaunchable", err)
	}
}

// gatePassingCLI writes a deterministic runnable fake retrieved-identity CLI
// and returns its path. It is written rather than found on the host: an absent
// or rebranded coreutil must never skip the refusal assertions.
func gatePassingCLI(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fakecli")
	body := "#!/bin/sh\necho \"fakecli 1.2.3\"\n"
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatalf("write fake cli: %v", err)
	}
	return path
}

func writeHookConfig(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "hooks.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write hook config: %v", err)
	}
	// The codex gate also verifies the sibling config.toml carries the
	// hooks feature flag.
	if err := os.WriteFile(filepath.Join(dir, "config.toml"),
		[]byte("[features]\nhooks = true\n"), 0o600); err != nil {
		t.Fatalf("write codex config.toml: %v", err)
	}
	return path
}

func TestCreateRefusesUnavailableProvider(t *testing.T) {
	d := sleeperDescriptor()
	d.unavailable = errors.New("not installed")
	mgr, _, _, projectID := newTestEnv(t, d)

	_, err := mgr.Create(context.Background(), CreateSpec{ProjectID: projectID, Provider: "claude", WorkDir: t.TempDir()})
	if err == nil || !strings.Contains(err.Error(), "unavailable") {
		t.Fatalf("err = %v, want unavailable refusal", err)
	}
}

func capabilityCLI(t *testing.T, help string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "fakecli")
	helpPath := filepath.Join(dir, "help")
	if err := os.WriteFile(helpPath, []byte(help), 0o600); err != nil {
		t.Fatalf("write capability help: %v", err)
	}
	script := "#!/bin/sh\ncase \"$1\" in\n  --version) printf 'fakecli 9.9.9\\n' ;;\n  --help) /bin/cat \"" + helpPath + "\" ;;\n  *) sleep 300 ;;\nesac\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write capability CLI: %v", err)
	}
	return path
}

// The signal evidence shape: a process dying FROM the graceful signal is
// recorded as exit_signal with no exit_code, and the dead pane is preserved
// with no shell fallback.
func TestStopGracefulRecordsSignalShape(t *testing.T) {
	mgr, repo, tc, projectID := newTestEnv(t, sleeperDescriptor())
	ctx := context.Background()

	s, err := mgr.Create(ctx, CreateSpec{ProjectID: projectID, Provider: "claude", WorkDir: t.TempDir()})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	stopCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := mgr.Stop(stopCtx, s.ID, false); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	got, err := repo.Get(ctx, s.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != StatusExited {
		t.Fatalf("status = %s, want exited", got.Status)
	}
	if got.ExitSignal == nil || *got.ExitSignal != "15" || got.ExitCode != nil {
		t.Fatalf("evidence = code %v signal %v, want signal 15 only", got.ExitCode, got.ExitSignal)
	}

	// The corpse is evidence: native resume needs the dead pane to remain,
	// and it must not have fallen back to a shell.
	alive, err := tc.HasSession(ctx, s.TmuxSessionName)
	if err != nil || !alive {
		t.Fatalf("dead pane was destroyed (HasSession=%v err=%v) — exit evidence lost", alive, err)
	}
	info, err := tc.PaneInfoByID(ctx, *s.TmuxPaneID)
	if err != nil {
		t.Fatalf("observe dead pane: %v", err)
	}
	if !info.Dead {
		t.Fatal("pane reports alive after recorded exit — shell fallback?")
	}

	types := auditTypes(t, repo, s.ID)
	want := []string{AuditCreated, AuditRunning, AuditStopRequested,
		AuditStopExecuting, AuditExited, AuditStopCompleted}
	if len(types) != len(want) {
		t.Fatalf("audit trail = %v, want %v", types, want)
	}
	for i := range want {
		if types[i] != want[i] {
			t.Fatalf("audit trail = %v, want %v", types, want)
		}
	}
}

func TestStopHandledExitRecordsStatusShape(t *testing.T) {
	d := sleeperDescriptor()
	d.args = []string{"-c", `trap 'exit 143' TERM; sleep 300 & wait`}
	mgr, repo, _, projectID := newTestEnv(t, d)
	ctx := context.Background()

	s, err := mgr.Create(ctx, CreateSpec{ProjectID: projectID, Provider: "claude", WorkDir: t.TempDir()})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	stopCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := mgr.Stop(stopCtx, s.ID, false); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	got, err := repo.Get(ctx, s.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.ExitCode == nil || *got.ExitCode != 143 || got.ExitSignal != nil {
		t.Fatalf("evidence = code %v signal %v, want code 143 only", got.ExitCode, got.ExitSignal)
	}
}

func TestStopForceUsesForceSignal(t *testing.T) {
	mgr, repo, _, projectID := newTestEnv(t, sleeperDescriptor())
	captures, err := hookcapture.NewService(repo, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	mgr.SetHookCaptureDeps(captures, "http://127.0.0.1:8837")
	ctx := context.Background()

	// tmux loses a pane's cause of death outright when the process dies while
	// it is still registering the job behind that pane, and a stop issued this
	// soon after a launch lands inside that window. Deck records such a death
	// as interrupted rather than inventing evidence, which is correct but says
	// nothing about which signal was sent — so the run is repeated until tmux
	// does report a cause, and that cause must be the force signal.
	const attempts = 4
	for attempt := range attempts {
		s, err := mgr.Create(ctx, CreateSpec{ProjectID: projectID, Provider: "claude", WorkDir: t.TempDir()})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		stopCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		err = mgr.Stop(stopCtx, s.ID, true)
		cancel()
		if err != nil {
			t.Fatalf("Stop force: %v", err)
		}
		got, getErr := repo.Get(ctx, s.ID)
		if getErr != nil {
			t.Fatal(getErr)
		}
		if got.ExitSignal != nil {
			if *got.ExitSignal != "9" {
				t.Fatalf("evidence signal = %s, want 9", *got.ExitSignal)
			}
			return
		}
		if got.Status != StatusInterrupted {
			t.Fatalf("a death tmux could not explain was recorded %s, want interrupted",
				got.Status)
		}
		t.Logf("attempt %d: tmux reported the death with no cause", attempt+1)
	}
	t.Fatalf("tmux never reported a cause of death in %d force stops", attempts)
}

// An out-of-band respawn keeps the pane id and markers but moves the pid and
// start ticks (measured — the "markers do not catch every accident" finding).
// Stop must refuse to signal and leave the recorded lifecycle untouched.
func TestStopRefusesIdentityMismatch(t *testing.T) {
	mgr, repo, tc, projectID := newTestEnv(t, sleeperDescriptor())
	ctx := context.Background()

	s, err := mgr.Create(ctx, CreateSpec{ProjectID: projectID, Provider: "claude", WorkDir: t.TempDir()})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Replace the pane's process out of band, preserving name and pane id:
	// kill the recorded process directly, then respawn the dead pane with
	// fresh identity options and restore the recorded ones. The markers
	// survive; the pid and start ticks move.
	respawnAsRecorded(t, tc, s, []string{"/bin/sh", "-c", "exec sleep 300"})

	err = mgr.Stop(ctx, s.ID, false)
	if !errors.Is(err, ErrIdentityMismatch) {
		t.Fatalf("err = %v, want ErrIdentityMismatch", err)
	}

	// The imposter must NOT have been signalled.
	info, err := tc.PaneInfoByID(ctx, *s.TmuxPaneID)
	if err != nil {
		t.Fatalf("observe pane: %v", err)
	}
	if info.Dead {
		t.Fatal("mismatched pane was killed")
	}

	got, _ := repo.Get(ctx, s.ID)
	if got.Status != StatusRunning {
		t.Fatalf("status = %s, want running", got.Status)
	}
	if countAuditType(t, repo, s.ID, AuditAttention) != 0 {
		t.Fatal("identity-mismatch stop wrote retired attention state")
	}
}

func TestStopUnobservedWithinDeadline(t *testing.T) {
	d := sleeperDescriptor()
	d.graceful = syscall.Signal(0) // deliverable but has no effect
	mgr, repo, tc, projectID := newTestEnv(t, d)
	ctx := context.Background()

	s, err := mgr.Create(ctx, CreateSpec{ProjectID: projectID, Provider: "claude", WorkDir: t.TempDir()})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	stopCtx, cancel := context.WithTimeout(ctx, 700*time.Millisecond)
	defer cancel()
	err = mgr.Stop(stopCtx, s.ID, false)
	if !errors.Is(err, ErrStopUnobserved) {
		t.Fatalf("err = %v, want ErrStopUnobserved", err)
	}

	got, _ := repo.Get(ctx, s.ID)
	if got.Status != StatusRunning {
		t.Fatalf("status = %s, want running (nothing was observed)", got.Status)
	}
	info, _ := tc.PaneInfoByID(ctx, *s.TmuxPaneID)
	if info == nil || info.Dead {
		t.Fatal("process should still be alive")
	}
	types := auditTypes(t, repo, s.ID)
	if len(types) < 2 || types[len(types)-2] != AuditStopUnobserved ||
		types[len(types)-1] != AuditStopCompleted {
		t.Fatalf("stop outcome audit = %v, want stop_unobserved then stop_completed", types)
	}
}

func TestStopTwiceReportsNotRunning(t *testing.T) {
	mgr, _, _, projectID := newTestEnv(t, sleeperDescriptor())
	ctx := context.Background()

	s, err := mgr.Create(ctx, CreateSpec{ProjectID: projectID, Provider: "claude", WorkDir: t.TempDir()})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	stopCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := mgr.Stop(stopCtx, s.ID, false); err != nil {
		t.Fatalf("first Stop: %v", err)
	}
	if err := mgr.Stop(ctx, s.ID, false); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("second Stop err = %v, want ErrNotRunning", err)
	}
}

// A stop on a pane whose process died AFTER launch (killed out of band, no
// respawn) must verify the members death preserves — marker, generation,
// tmux session id — and record the preserved evidence rather than parking
// an honest corpse as needs_attention over its vanished /proc fields.
func TestStopOnAlreadyDeadPaneRecordsEvidence(t *testing.T) {
	mgr, repo, tc, projectID := newTestEnv(t, sleeperDescriptor())
	captures, err := hookcapture.NewService(repo, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	mgr.SetHookCaptureDeps(captures, "http://127.0.0.1:8837")
	ctx := context.Background()

	s, err := mgr.Create(ctx, CreateSpec{ProjectID: projectID, Provider: "claude", WorkDir: t.TempDir()})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := syscall.Kill(*s.PanePID, syscall.SIGKILL); err != nil {
		t.Fatalf("out-of-band kill: %v", err)
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		info, err := tc.PaneInfoByID(ctx, *s.TmuxPaneID)
		if err != nil {
			t.Fatalf("observe: %v", err)
		}
		if info.Dead {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("pane never died")
		}
		time.Sleep(50 * time.Millisecond)
	}

	if err := mgr.Stop(ctx, s.ID, false); err != nil {
		t.Fatalf("Stop on dead pane: %v", err)
	}
	got, _ := repo.Get(ctx, s.ID)
	if got.Status != StatusExited || got.ExitSignal == nil || *got.ExitSignal != "9" {
		t.Fatalf("status=%s signal=%v, want exited signal 9", got.Status, got.ExitSignal)
	}
	if got.StopRequestedAt == nil {
		t.Fatal("stop on an already-dead pane did not preserve user intent")
	}
	candidates, err := repo.ListResumeCandidates(ctx)
	if err != nil {
		t.Fatalf("ListResumeCandidates: %v", err)
	}
	for _, candidate := range candidates {
		if candidate.ID == s.ID {
			t.Fatal("explicitly stopped session remained eligible for automatic recovery")
		}
	}
}

func TestRecordPaneExitWithoutEvidenceIsInterrupted(t *testing.T) {
	mgr, repo, _, projectID := newTestEnv(t, sleeperDescriptor())
	ctx := context.Background()

	s, err := mgr.Create(ctx, CreateSpec{
		ProjectID: projectID,
		Provider:  "claude",
		WorkDir:   t.TempDir(),
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if applied, err := repo.MarkInterruptedObserved(ctx, s,
		"test dead pane without exit evidence"); err != nil || !applied {
		t.Fatalf("record interrupted: applied=%v err=%v", applied, err)
	}

	got, err := repo.Get(ctx, s.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != StatusInterrupted {
		t.Fatalf("status = %s, want interrupted", got.Status)
	}
	if got.ExitCode != nil || got.ExitSignal != nil {
		t.Fatalf("interrupted session fabricated exit evidence: code=%v signal=%v",
			got.ExitCode, got.ExitSignal)
	}
}

func TestStoppedDeadPaneWithoutEvidenceIsNotResumeCandidate(t *testing.T) {
	mgr, repo, _, projectID := newTestEnv(t, sleeperDescriptor())
	ctx := context.Background()

	s, err := mgr.Create(ctx, CreateSpec{
		ProjectID: projectID,
		Provider:  "claude",
		WorkDir:   t.TempDir(),
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	s, err = repo.Get(ctx, s.ID)
	if err != nil {
		t.Fatalf("refresh running row: %v", err)
	}
	attempt, err := repo.BeginStopAttempt(ctx, s, "dead-pane-fixture", false)
	if err != nil {
		t.Fatalf("BeginStopAttempt: %v", err)
	}
	if err := repo.FinishStopAttempt(ctx, attempt, "test stop gesture completed before a signal", true); err != nil {
		t.Fatalf("FinishStopAttempt: %v", err)
	}
	if applied, err := repo.MarkInterruptedObserved(ctx, s,
		"test stopped dead pane without exit evidence"); err != nil || !applied {
		t.Fatalf("record interrupted: applied=%v err=%v", applied, err)
	}

	got, err := repo.Get(ctx, s.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != StatusInterrupted || got.StopRequestedAt == nil {
		t.Fatalf("status=%s stop_requested_at=%v, want interrupted with stop attribution",
			got.Status, got.StopRequestedAt)
	}
	candidates, err := repo.ListResumeCandidates(ctx)
	if err != nil {
		t.Fatalf("ListResumeCandidates: %v", err)
	}
	for _, candidate := range candidates {
		if candidate.ID == s.ID {
			t.Fatal("explicitly stopped session remained eligible for automatic recovery")
		}
	}
}

// A CLI that ignores HUP and TERM survives tmux kill-session; the removal
// must escalate against the recorded identity — graceful, then force — and
// still mark the session removed, auditing the escalation on the way.
func TestRemoveEscalatesToForceOnSignalTrappingSurvivor(t *testing.T) {
	desc := sleeperDescriptor()
	desc.args = []string{"-c", "trap '' HUP TERM; sleep 300; exit 0"}
	mgr, repo, _, projectID := newTestEnv(t, desc)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	s, err := mgr.Create(ctx, CreateSpec{ProjectID: projectID, Provider: "claude", WorkDir: t.TempDir()})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	if err := mgr.Remove(ctx, s.ID); err != nil {
		t.Fatalf("remove: %v", err)
	}
	got, err := repo.Get(ctx, s.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.RemovedAt == nil {
		t.Fatal("signal-trapping CLI was not marked removed")
	}
	if got.PanePID == nil || got.StartTicks == nil {
		t.Fatal("removed session lost its recorded process identity")
	}
	alive, err := tmux.ProcAlive(*got.PanePID, *got.StartTicks)
	if err != nil || alive {
		t.Fatalf("CLI survived the removal escalation: alive=%v err=%v", alive, err)
	}
	// The trap swallows HUP (kill-session) and TERM (graceful): exactly two
	// escalations prove the graceful signal was tried, failed, and force
	// finished the removal.
	types := auditTypes(t, repo, s.ID)
	var escalations, removals int
	for _, ty := range types {
		switch ty {
		case AuditStopRequested:
			escalations++
		case AuditRemoved:
			removals++
		}
	}
	if escalations != 2 || removals != 1 {
		t.Fatalf("audit = %v, want graceful+force escalations and exactly one removal", types)
	}
	want := []string{AuditCreated, AuditRunning, AuditRemoved, AuditRemovalExecuting,
		AuditStopRequested, AuditStopRequested, AuditRemovalProcessStopped, AuditRemovalCompleted}
	if !slices.Equal(types, want) {
		t.Fatalf("audit = %v, want durable claim/effects/completion order %v", types, want)
	}
}

func TestRemoveRefusesProcessGroupChangeAtSignalBoundary(t *testing.T) {
	mgr, repo, tc, projectID := newTestEnv(t, sleeperDescriptor())
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	s, err := mgr.Create(ctx, CreateSpec{
		ProjectID: projectID, Provider: provider.Claude, WorkDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	info, err := tc.PaneInfoByID(ctx, *s.TmuxPaneID)
	if err != nil {
		t.Fatalf("observe current controller: %v", err)
	}
	mgr.procGroup = func(pid int, startTicks int64) (int, error) {
		if pid != info.PID || startTicks != info.StartTicks {
			t.Fatalf("process-group probe = pid %d ticks %d, want %d/%d",
				pid, startTicks, info.PID, info.StartTicks)
		}
		return info.ProcessGroupID + 1, nil
	}
	if err := mgr.Remove(ctx, s.ID); !errors.Is(err, ErrUnverifiable) {
		t.Fatalf("remove after process-group change = %v, want unverifiable", err)
	}
	alive, err := tmux.ProcAlive(info.PID, info.StartTicks)
	if err != nil || !alive {
		t.Fatalf("removal signalled a changed process group: alive=%v err=%v", alive, err)
	}
	if _, err := tc.PaneInfoByID(ctx, info.PaneID); err != nil {
		t.Fatalf("removal touched the exact pane after refusing the signal: %v", err)
	}
	row, err := repo.Get(ctx, s.ID)
	if err != nil {
		t.Fatalf("read durably removed row: %v", err)
	}
	if row.RemovedAt == nil {
		t.Fatal("failed process-group preflight did not preserve the durable record removal")
	}
	_ = syscall.Kill(-info.ProcessGroupID, syscall.SIGKILL)
}

func TestRemoveAuditDoesNotClaimAbsentPaneKilled(t *testing.T) {
	mgr, repo, tc, projectID := newTestEnv(t, sleeperDescriptor())
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	target, err := mgr.Create(ctx, CreateSpec{ProjectID: projectID, Provider: "claude", WorkDir: t.TempDir()})
	if err != nil {
		t.Fatalf("create target: %v", err)
	}
	if _, err := mgr.Create(ctx, CreateSpec{ProjectID: projectID, Provider: "claude", WorkDir: t.TempDir()}); err != nil {
		t.Fatalf("create server sentinel: %v", err)
	}
	info, err := tc.PaneInfoByID(ctx, *target.TmuxPaneID)
	if err != nil {
		t.Fatalf("observe target: %v", err)
	}
	applied, err := tc.KillPaneIfIdentity(ctx, tmux.PaneIdentity{
		SessionID: info.SessionID, PaneID: info.PaneID,
		Generation: info.Generation, Marker: info.Marker,
		LaunchMarker: info.LaunchMarker,
	})
	if err != nil || !applied {
		t.Fatalf("remove target pane before Manager.Remove: applied=%v err=%v", applied, err)
	}
	if err := mgr.Remove(ctx, target.ID); err != nil {
		t.Fatalf("remove absent pane: %v", err)
	}
	var detail string
	if err := repo.db.Reader().QueryRow(`SELECT details FROM audit_events
		WHERE session_id = ? AND event_type = ? ORDER BY id DESC LIMIT 1`,
		target.ID, AuditRemoved).Scan(&detail); err != nil {
		t.Fatalf("query removal audit: %v", err)
	}
	if !strings.Contains(detail, "record-only") {
		t.Fatalf("removal audit detail = %q, want record-only wording", detail)
	}
	if got := countAuditType(t, repo, target.ID, AuditRemovalCompleted); got != 1 {
		t.Fatalf("record-only removal completion audits = %d, want 1", got)
	}
}

// A pane replaced between spawn and the verified-live commit is never
// adopted. The launch spawned one process; whatever occupies the pane
// afterwards is a stranger, and recording it would hand the row an identity
// no later verification could hold it to.
func TestCreateRefusesReplacementBeforeVerifiedLiveCommit(t *testing.T) {
	mgr, repo, client, projectID := newTestEnv(t, sleeperDescriptor())
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	var replacement *tmux.PaneInfo
	client.SetFinalSpawnFailureForTest(func(kind string) error {
		if kind != "create" {
			t.Fatalf("spawn kind = %q, want create", kind)
		}
		replacement = replaceOnlyCreatedPane(t, client,
			[]string{"/bin/sh", "-c", "sleep 300; exit 0"})
		return nil
	})
	t.Cleanup(func() { client.SetFinalSpawnFailureForTest(nil) })

	created, err := mgr.Create(ctx, CreateSpec{
		ProjectID: projectID, Provider: "claude", WorkDir: t.TempDir(),
	})
	if !errors.Is(err, ErrIdentityMismatch) || created != nil {
		t.Fatalf("replacement create = session %+v err=%v, want identity mismatch", created, err)
	}
	rows, err := repo.ListByProject(ctx, projectID)
	if err != nil || len(rows) != 1 || rows[0].Status != StatusInterrupted ||
		rows[0].ResumeAttempts != 1 {
		t.Fatalf("replacement row = %+v err=%v, want closed interrupted launch", rows, err)
	}
	if n := countAuditType(t, repo, rows[0].ID, AuditRunning); n != 0 {
		t.Fatalf("foreign replacement committed running %d times", n)
	}
	current, err := client.PaneInfoByID(ctx, replacement.PaneID)
	if err != nil || current.Dead || current.PID != replacement.PID ||
		current.StartTicks != replacement.StartTicks {
		t.Fatalf("replacement received a create cleanup effect: current=%+v err=%v", current, err)
	}
}

// A descriptor whose measured interpreter path does not match what the pane
// actually runs still produces a running session, recorded against the
// executable that was OBSERVED. Interpreted CLIs move under version managers
// and start bundling their own runtimes; a path that has drifted describes a
// stale measurement, not a foreign process.
func TestCreateRecordsObservedExecutableRatherThanRefusing(t *testing.T) {
	desc := sleeperDescriptor()
	desc.execIdentity = &provider.ExecutableIdentity{
		Interpreted:     true,
		ExpectedProcExe: "/bin/false",
	}
	mgr, repo, _, projectID := newTestEnv(t, desc)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	created, err := mgr.Create(ctx, CreateSpec{
		ProjectID: projectID, Provider: "claude", WorkDir: t.TempDir(),
	})
	if err != nil || created == nil {
		t.Fatalf("create with a drifted interpreter path = %v, want a running session", err)
	}
	stored, err := repo.Get(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != StatusRunning {
		t.Fatalf("status = %s, want running", stored.Status)
	}
	if stored.ExpectedExecutable == nil || *stored.ExpectedExecutable == "/bin/false" {
		t.Fatalf("recorded executable = %v, want the observed path", stored.ExpectedExecutable)
	}
}

func TestPresentSocketMismatchNeverAdoptsCollidingPane(t *testing.T) {
	mgr, repo, client, projectID := newTestEnv(t, sleeperDescriptor())
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	s, err := mgr.Create(ctx, CreateSpec{
		ProjectID: projectID, Provider: "claude", WorkDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	before, err := client.PaneInfoByID(ctx, *s.TmuxPaneID)
	if err != nil {
		t.Fatalf("observe pane: %v", err)
	}
	t.Cleanup(func() { _ = client.KillSession(context.Background(), before.SessionName) })
	if err := repo.db.WithTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE sessions SET tmux_socket_path = ? WHERE id = ?`,
			client.SocketPath()+".foreign", s.ID)
		return err
	}); err != nil {
		t.Fatalf("seed socket mismatch: %v", err)
	}
	recorded, err := repo.Get(ctx, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	presented, err := mgr.Present(ctx, recorded)
	if err != nil {
		t.Fatalf("present socket mismatch: %v", err)
	}
	if presented.Status != StatusInterrupted ||
		presented.PresentationState != PresentationAttachmentUnavailable {
		t.Fatalf("socket-mismatched projection = status %q presentation %q",
			presented.Status, presented.PresentationState)
	}
	after, err := client.PaneInfoByID(ctx, before.PaneID)
	if err != nil || after.Dead || after.PID != before.PID ||
		after.StartTicks != before.StartTicks {
		t.Fatalf("socket-mismatched pane was affected: before=%+v after=%+v err=%v",
			before, after, err)
	}
}

func TestRemoveQueuedDuringCreateStopsOnlyCommittedController(t *testing.T) {
	mgr, repo, client, projectID := newTestEnv(t, sleeperDescriptor())
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	createReached := make(chan struct{})
	releaseCreate := make(chan struct{})
	client.SetFinalSpawnFailureForTest(func(kind string) error {
		if kind != "create" {
			return fmt.Errorf("unexpected spawn kind %q", kind)
		}
		close(createReached)
		<-releaseCreate
		return nil
	})
	t.Cleanup(func() { client.SetFinalSpawnFailureForTest(nil) })
	type createResult struct {
		session *Session
		err     error
	}
	created := make(chan createResult, 1)
	go func() {
		s, err := mgr.Create(ctx, CreateSpec{
			ProjectID: projectID, Provider: "claude", WorkDir: t.TempDir(),
		})
		created <- createResult{s, err}
	}()
	select {
	case <-createReached:
	case <-ctx.Done():
		t.Fatal("create did not reach the pre-commit barrier")
	}
	rows, err := repo.ListByProject(ctx, projectID)
	if err != nil || len(rows) != 1 || rows[0].Status != StatusStarting {
		t.Fatalf("visible launch row = %+v err=%v, want starting", rows, err)
	}
	removed := make(chan error, 1)
	go func() { removed <- mgr.Remove(ctx, rows[0].ID) }()
	select {
	case err := <-removed:
		t.Fatalf("remove crossed an in-flight create gate: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	close(releaseCreate)
	result := <-created
	if result.err != nil || result.session == nil || result.session.Status != StatusRunning {
		t.Fatalf("create result = %+v err=%v, want committed controller", result.session, result.err)
	}
	if err := <-removed; err != nil {
		t.Fatalf("queued remove: %v", err)
	}
	if _, err := client.PaneInfoByID(ctx, *result.session.TmuxPaneID); !errors.Is(err, tmux.ErrNoSession) {
		t.Fatalf("committed controller pane remains after queued remove: %v", err)
	}
	stored, err := repo.Get(ctx, result.session.ID)
	if err != nil || stored.RemovedAt == nil {
		t.Fatalf("removed row = %+v err=%v", stored, err)
	}
}

func TestRemoveWithUnreachableTmuxServer(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"plain CLI died with its server", []string{"-c", "sleep 300; exit 0"}},
		{"HUP-trapping CLI survived it", []string{"-c", "trap '' HUP TERM; sleep 300; exit 0"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			desc := sleeperDescriptor()
			desc.args = tc.args
			mgr, repo, tm, projectID := newTestEnv(t, desc)
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			s, err := mgr.Create(ctx, CreateSpec{ProjectID: projectID, Provider: "claude", WorkDir: t.TempDir()})
			if err != nil {
				t.Fatalf("create: %v", err)
			}
			if s.PanePID == nil || s.StartTicks == nil {
				t.Fatal("running session has no recorded tuple")
			}

			// The server dies as a container restart would kill it.
			if err := tm.KillServer(ctx); err != nil {
				t.Fatalf("kill server: %v", err)
			}

			// Prove the precondition each case claims: the plain CLI is
			// dead BEFORE the remove (deadline-bounded — HUP delivery is
			// prompt but not synchronous), the trapper is alive.
			expectAlive := strings.Contains(tc.name, "trapping")
			deadline := time.Now().Add(30 * time.Second)
			for {
				alive, err := tmux.ProcAlive(*s.PanePID, *s.StartTicks)
				if err != nil {
					t.Fatalf("precondition liveness: %v", err)
				}
				if alive == expectAlive {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("precondition: alive=%v, want %v", alive, expectAlive)
				}
				time.Sleep(50 * time.Millisecond)
			}

			if err := mgr.Remove(ctx, s.ID); err != nil {
				t.Fatalf("remove with unreachable server: %v", err)
			}
			got, err := repo.Get(ctx, s.ID)
			if err != nil {
				t.Fatalf("get: %v", err)
			}
			if got.RemovedAt == nil {
				t.Fatal("row not marked removed")
			}
			if expectAlive {
				// Record-only removal: no verified current
				// controller with the server gone, so zero effects reach the
				// surviving CLI.
				alive, err := tmux.ProcAlive(*got.PanePID, *got.StartTicks)
				if err != nil || !alive {
					t.Fatalf("surviving CLI was signalled: alive=%v err=%v", alive, err)
				}
			}
			// Record-only removal never escalates a signal.
			escalations := 0
			for _, ty := range auditTypes(t, repo, s.ID) {
				if ty == AuditStopRequested {
					escalations++
				}
			}
			if escalations != 0 {
				t.Fatalf("record-only removal escalated %d signals", escalations)
			}
		})
	}
}

// failingSettings is a settings source whose read always fails — the
// branch a healthy database never reaches.
type failingSettings struct{ err error }

func (f failingSettings) SkipPermissions(context.Context) (bool, error) { return false, f.err }

// A settings read failure REFUSES the launch instead of guessing a
// permission posture. Guessing either way is wrong: launching with the
// flag removes safety prompts the user may not have asked to
// remove, and launching without it silently ignores a setting they did
// choose. Nothing may be recorded — a refused launch leaves no session.
func TestCreateRefusesWhenTheSettingsReadFails(t *testing.T) {
	mgr, repo, _, projectID := newTestEnv(t, sleeperDescriptor())
	mgr.SetSettings(failingSettings{err: errors.New("settings table is gone")})
	ctx := context.Background()

	_, err := mgr.Create(ctx, CreateSpec{ProjectID: projectID, Provider: "claude", WorkDir: t.TempDir()})
	if err == nil || !strings.Contains(err.Error(), "skip-permissions") {
		t.Fatalf("err = %v, want a skip-permissions read refusal", err)
	}
	sessions, err := repo.ListActive(ctx)
	if err != nil {
		t.Fatalf("ListActive: %v", err)
	}
	if len(sessions) != 0 {
		t.Fatalf("a refused launch recorded %d session(s); it must record none", len(sessions))
	}
}

// The sidebar shows time since a session last produced output, so
// the transport records when bytes moved. The value is telemetry, not
// state — these assertions pin that it changes nothing else.
func TestRecordOutputStampsOnlyThatColumn(t *testing.T) {
	mgr, repo, _, projectID := newTestEnv(t, sleeperDescriptor())
	ctx := context.Background()

	s, err := mgr.Create(ctx, CreateSpec{ProjectID: projectID, Provider: "claude", WorkDir: t.TempDir()})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	before, err := repo.Get(ctx, s.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if before.LastOutputAt != nil {
		t.Fatalf("a session that has produced nothing reports %v; it must report nothing", before.LastOutputAt)
	}

	at := time.Now().Add(-90 * time.Second).UTC().Truncate(time.Millisecond)
	if err := repo.RecordOutput(ctx, s.ID, at); err != nil {
		t.Fatalf("RecordOutput: %v", err)
	}
	after, err := repo.Get(ctx, s.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if after.LastOutputAt == nil || !after.LastOutputAt.Equal(at) {
		t.Fatalf("last_output_at = %v, want %v", after.LastOutputAt, at)
	}
	// updated_at is a reconciliation-state stamp: output is not a state
	// transition and must not disturb it, or every streaming session would
	// look freshly reconciled.
	if !after.UpdatedAt.Equal(before.UpdatedAt) {
		t.Fatalf("updated_at moved from %v to %v; output must not touch it",
			before.UpdatedAt, after.UpdatedAt)
	}
	if after.Status != before.Status || after.Generation != before.Generation {
		t.Fatalf("status/generation changed: %s/%d -> %s/%d",
			before.Status, before.Generation, after.Status, after.Generation)
	}
	// And it appends no audit event: the audit log records transitions,
	// and "bytes moved" is not one.
	var events int
	if err := repo.db.Reader().QueryRow(
		`SELECT COUNT(*) FROM audit_events WHERE session_id = ?`, s.ID).Scan(&events); err != nil {
		t.Fatalf("count audit: %v", err)
	}
	if err := repo.RecordOutput(ctx, s.ID, at.Add(time.Second)); err != nil {
		t.Fatalf("second RecordOutput: %v", err)
	}
	var after2 int
	if err := repo.db.Reader().QueryRow(
		`SELECT COUNT(*) FROM audit_events WHERE session_id = ?`, s.ID).Scan(&after2); err != nil {
		t.Fatalf("count audit: %v", err)
	}
	if after2 != events {
		t.Fatalf("audit events went %d -> %d; recording output must append none", events, after2)
	}
}

// A removed session keeps whatever it last reported: the stamp would
// outlive the thing it describes.
func TestRecordOutputSkipsRemovedSessions(t *testing.T) {
	mgr, repo, _, projectID := newTestEnv(t, sleeperDescriptor())
	ctx := context.Background()
	s, err := mgr.Create(ctx, CreateSpec{ProjectID: projectID, Provider: "claude", WorkDir: t.TempDir()})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := mgr.Remove(ctx, s.ID); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if err := repo.RecordOutput(ctx, s.ID, time.Now()); err != nil {
		t.Fatalf("RecordOutput on a removed session must be a no-op, got %v", err)
	}
	// Get still returns removed rows here; read the column directly so
	// the assertion does not depend on that.
	var stamped *string
	if err := repo.db.Reader().QueryRow(
		`SELECT last_output_at FROM sessions WHERE id = ?`, s.ID).Scan(&stamped); err != nil {
		t.Fatalf("read last_output_at: %v", err)
	}
	if stamped != nil {
		t.Fatalf("a removed session was stamped: %v", *stamped)
	}
}

// Runtime replacement helpers for identity-boundary tests.

// respawnAsRecorded replaces a pane's process out of band while leaving its
// RECORDED identity observable. The spawn guard refuses a respawn
// whose generation or marker equals the pane's current ones, so the harness
// respawns with fresh ones and then restores the recorded pane options
// (generation, marker) via the tmux set-option path. The launch marker,
// stamped in the session environment at Create, already reads as recorded.
// The result is the shape the production paths must classify: the marker,
// generation, and launch marker survive while the pid and start ticks move.
func respawnAsRecorded(t *testing.T, tc *tmux.Client, s *Session, argv []string) {
	t.Helper()
	ctx := context.Background()
	if s.PanePID != nil {
		info, err := tc.PaneInfoByID(ctx, *s.TmuxPaneID)
		if err != nil {
			t.Fatalf("observe: %v", err)
		}
		if !info.Dead {
			if err := syscall.Kill(*s.PanePID, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
				t.Fatalf("out-of-band kill: %v", err)
			}
		}
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		info, err := tc.PaneInfoByID(ctx, *s.TmuxPaneID)
		if err != nil {
			t.Fatalf("observe: %v", err)
		}
		if info.Dead {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("pane never died after out-of-band kill")
		}
		time.Sleep(25 * time.Millisecond)
	}
	_, err := tc.RespawnPane(ctx, *s.TmuxPaneID, tmux.SessionSpec{
		Argv:       argv,
		WorkDir:    t.TempDir(),
		Generation: s.Generation + 1,
		Marker:     freshMarker(t),
	})
	if err != nil {
		t.Fatalf("out-of-band respawn: %v", err)
	}
	if err := tc.SetPaneOption(ctx, *s.TmuxPaneID, tmux.OptGeneration, strconv.Itoa(s.Generation)); err != nil {
		t.Fatalf("restore recorded generation: %v", err)
	}
	if err := tc.SetPaneOption(ctx, *s.TmuxPaneID, tmux.OptMarker, *s.IdentityMarker); err != nil {
		t.Fatalf("restore recorded marker: %v", err)
	}
}

func replaceOnlyCreatedPane(t *testing.T, tc *tmux.Client, argv []string) *tmux.PaneInfo {
	t.Helper()
	ctx := context.Background()
	panes, err := tc.ListPanes(ctx)
	if err != nil {
		t.Fatalf("list created panes: %v", err)
	}
	var created *tmux.PaneInfo
	for _, pane := range panes {
		if !pane.Dead {
			created, err = tc.PaneInfoByID(ctx, pane.PaneID)
			if err != nil {
				t.Fatalf("observe created pane: %v", err)
			}
			break
		}
	}
	if created == nil {
		t.Fatal("created pane was not discoverable")
	}
	paneID, pid, marker := created.PaneID, created.PID, created.Marker
	foreign := &Session{
		TmuxPaneID: &paneID, PanePID: &pid, Generation: created.Generation,
		IdentityMarker: &marker,
	}
	respawnAsRecorded(t, tc, foreign, argv)
	replacement, err := tc.PaneInfoByID(ctx, paneID)
	if err != nil {
		t.Fatalf("observe created-pane replacement: %v", err)
	}
	return replacement
}

func freshMarker(t *testing.T) string {
	t.Helper()
	m, err := newUUIDv4()
	if err != nil {
		t.Fatalf("uuid: %v", err)
	}
	return m
}

func TestEverySessionLaunchCarriesItsIdentityToTheHarness(t *testing.T) {
	env := launchArgv(sleeperDescriptor(), []string{"harness"}, "xterm", sessionIdentityEnv("project-1", "session-1"))
	joined := strings.Join(env, "\n")
	for _, want := range []string{"NOISEDECK_PROJECT_ID=project-1", "NOISEDECK_SESSION_ID=session-1"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("launch env lacks %s: %q", want, joined)
		}
	}
}

func TestEverySessionLaunchNamesTheAssetsFolder(t *testing.T) {
	t.Setenv("NOISEDECK_ASSETS_DIR", "/elsewhere")
	env := launchArgv(sleeperDescriptor(), []string{"harness"}, "xterm", nil)
	var got []string
	for _, assignment := range env {
		if strings.HasPrefix(assignment, "NOISEDECK_ASSETS_DIR=") {
			got = append(got, assignment)
		}
	}
	if len(got) != 1 || got[0] != "NOISEDECK_ASSETS_DIR=.deck/assets" {
		t.Fatalf("assets folder in launch env = %q", got)
	}
}

func TestPreviewMetadataEnvironment(t *testing.T) {
	mgr := &Manager{}
	if len(mgr.projectPreviewEnvironment()) != 0 {
		t.Fatal("disabled previews supplied a socket")
	}
	mgr.SetProjectPreviewSocket("/fixture/preview.sock")
	env := mgr.projectPreviewEnvironment()
	if len(env) != 1 || env[0] != "NOISEDECK_PROJECT_PREVIEWS_METADATA_SOCKET=/fixture/preview.sock" {
		t.Fatalf("unexpected preview environment: %q", env)
	}
}
