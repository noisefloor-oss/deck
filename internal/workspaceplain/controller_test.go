//go:build linux

package workspaceplain

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

type fakeExit int

func (e fakeExit) Error() string { return "synthetic output must not escape" }
func (e fakeExit) ExitCode() int { return int(e) }

type invocation struct {
	path      string
	args, env []string
	capture   bool
}
type fixture struct {
	t                                                *testing.T
	cfg                                              Config
	c                                                *Controller
	calls                                            []invocation
	app                                              bool
	health, network, markers, started, boot, process string
	ports                                            map[string][]any
	tmpfs                                            map[string]string
	mutateMounts                                     func([]map[string]any) []map[string]any
	extra                                            bool
	wrongBind                                        bool
	rootful                                          bool
	failHealth                                       bool
	failLaunch                                       bool
	listener                                         net.Listener
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("native controller refuses root ownership")
	}
	root, err := os.MkdirTemp("/tmp", "deck-plain-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	for _, name := range []string{"state", "control", "home", "run", "bin"} {
		if err := os.Mkdir(filepath.Join(root, name), 0700); err != nil {
			t.Fatal(err)
		}
	}
	f := &fixture{t: t, health: "healthy", network: "bridge", markers: "bspcau", started: "2026-09-22T00:00:00Z", boot: "fixture-boot", process: "1000"}
	f.tmpfs = map[string]string{"/run/noisedeck": "rw,noexec,nosuid,nodev,size=16m,uid=1026,gid=100,mode=0700"}
	f.cfg = Config{InstanceID: "plain_fixture", OwnerUID: os.Geteuid(), StateRoot: filepath.Join(root, "state"), ControlDir: filepath.Join(root, "control"), HomeDir: filepath.Join(root, "home"), RuntimeDir: filepath.Join(root, "run"), PodmanPath: filepath.Join(root, "bin/podman"), ComposePath: filepath.Join(root, "plain-compose.sh"), ComposeEnvFile: filepath.Join(root, "compose.env"), ComposeProject: "plain_fixture", AppContainer: "plain_fixture-app", PublicOrigin: "https://plain.example.test"}
	for _, p := range []string{f.cfg.PodmanPath, f.cfg.ComposePath, f.cfg.ComposeEnvFile} {
		if err := os.WriteFile(p, []byte("synthetic installation file"), 0700); err != nil {
			t.Fatal(err)
		}
	}
	f.open()
	t.Cleanup(func() {
		if f.listener != nil {
			_ = f.listener.Close()
		}
		if f.c != nil {
			_ = f.c.Close()
		}
	})
	return f
}
func (f *fixture) open() {
	f.t.Helper()
	c, err := open(f.cfg, f, func(int, int) (string, error) {
		if f.process == "" {
			return "", ErrUnavailable
		}
		return f.process, nil
	}, func() (string, error) { return f.boot, nil })
	if err != nil {
		f.t.Fatal(err)
	}
	f.c = c
}
func (f *fixture) launches() int {
	n := 0
	for _, c := range f.calls {
		if c.path == f.cfg.ComposePath {
			n++
		}
	}
	return n
}
func (f *fixture) socket() {
	f.t.Helper()
	if f.listener != nil {
		return
	}
	p := filepath.Join(f.cfg.StateRoot, "data/gateway/backend.sock")
	l, err := net.Listen("unix", p)
	if err != nil {
		f.t.Fatal(err)
	}
	if err = os.Chmod(p, 0600); err != nil {
		f.t.Fatal(err)
	}
	f.listener = l
}

func (f *fixture) run(_ context.Context, path string, args, env []string, capture bool) ([]byte, error) {
	f.calls = append(f.calls, invocation{path, slices.Clone(args), slices.Clone(env), capture})
	if path == f.cfg.ComposePath {
		if f.failLaunch {
			return []byte("OIDC secret must not escape"), errors.New("OIDC secret must not escape")
		}
		f.app = true
		f.socket()
		return nil, nil
	}
	switch args[0] {
	case "info":
		if f.rootful {
			return []byte("false"), nil
		}
		return []byte("true"), nil
	case "ps":
		if f.app {
			ids := strings.Repeat("a", 64)
			if f.extra {
				ids += "\n" + strings.Repeat("b", 64)
			}
			return []byte(ids), nil
		}
		return nil, nil
	case "container":
		if !f.app {
			return nil, fakeExit(1)
		}
		return nil, nil
	case "exec":
		if f.failHealth {
			return nil, fakeExit(9)
		}
		return nil, nil
	case "inspect":
		if args[2] == transportFormat {
			return []byte(f.markers), nil
		}
		mounts := []map[string]any{}
		for dest, rel := range map[string]string{"/data": "data", "/home/agent": "home", "/workspace": "projects", "/tmp": "tmp", "/home/agent/.config/gh": "gh"} {
			source := filepath.Join(f.cfg.StateRoot, rel)
			if f.wrongBind && dest == "/data" {
				source = "/unmanaged/data"
			}
			mounts = append(mounts, map[string]any{"Type": "bind", "Source": source, "Destination": dest, "RW": true})
		}
		mounts = append(mounts, map[string]any{"Type": "bind", "Source": "/etc/machine-id", "Destination": "/run/noisedeck-host-machine-id", "RW": false})
		if f.mutateMounts != nil {
			mounts = f.mutateMounts(mounts)
		}
		b, _ := json.Marshal(map[string]any{"id": strings.Repeat("a", 64), "running": true, "pid": 123, "status": "running", "started": f.started, "health": f.health, "project": f.cfg.ComposeProject, "network": f.network, "ports": f.ports, "mounts": mounts, "tmpfs": f.tmpfs})
		return b, nil
	}
	f.t.Fatalf("unexpected synthetic command %q", args)
	return nil, ErrCommand
}

func TestFreshStartAndRestartPreserveRuntime(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	entries, _ := os.ReadDir(f.cfg.StateRoot)
	if len(entries) != 0 {
		t.Fatal("Open created workload stores")
	}
	s, err := f.c.Observe(ctx)
	if err != nil || s.Configured || s.Runtime != "stopped" {
		t.Fatalf("fresh: %+v %v", s, err)
	}
	op, err := f.c.Start(ctx, "first_start")
	if err != nil || op.Status != "succeeded" || !op.State.Configured || op.State.Runtime != "running" {
		t.Fatalf("start: %+v %v", op, err)
	}
	epoch := op.State.Epoch
	if err = f.c.Close(); err != nil {
		t.Fatal(err)
	}
	f.c = nil
	f.open()
	s, err = f.c.Observe(ctx)
	if err != nil || s.Epoch != epoch || s.Runtime != "running" {
		t.Fatalf("restart changed reality: %+v %v", s, err)
	}
	for _, id := range []string{"first_start", "another_start"} {
		op, err = f.c.Start(ctx, id)
		if err != nil || op.Status != "succeeded" || op.State.Epoch != epoch {
			t.Fatalf("repeat: %+v %v", op, err)
		}
	}
	if f.launches() != 1 {
		t.Fatal("restart or running start dispatched Compose")
	}
	f.app = false
	s, err = f.c.Observe(ctx)
	if err != nil || !s.Configured || s.Runtime != "stopped" || s.Epoch == epoch {
		t.Fatalf("stopped plain root: %+v %v", s, err)
	}
	if _, err = f.c.Start(ctx, "new_start"); err != nil || f.launches() != 2 {
		t.Fatalf("explicit subsequent start: %v", err)
	}
}

func TestFailedLaunchObservedStoppedAllowsFreshStart(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.failLaunch = true
	op, err := f.c.Start(ctx, "lost")
	if !errors.Is(err, ErrCommand) || op.Status != "failed" || op.Stage != "complete" {
		t.Fatalf("lost: %+v %v", op, err)
	}
	if strings.Contains(err.Error(), "OIDC") {
		t.Fatal("command error disclosed output")
	}
	if err = f.c.Close(); err != nil {
		t.Fatal(err)
	}
	f.c = nil
	f.open()
	f.failLaunch = false
	op, err = f.c.Start(ctx, "lost")
	if err != nil || op.Status != "failed" || f.launches() != 1 {
		t.Fatalf("replayed: %+v %v", op, err)
	}
	op, err = f.c.Start(ctx, "fresh")
	if err != nil || op.Status != "succeeded" || f.launches() != 2 {
		t.Fatalf("fresh start: %+v %v", op, err)
	}
	op, err = f.c.Operation(ctx, "lost")
	if err != nil || op.Status != "failed" || f.launches() != 2 {
		t.Fatalf("terminal result changed: %+v %v", op, err)
	}
}

func TestStartingIsNotReady(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.health = "starting"
	op, err := f.c.Start(ctx, "start")
	if err != nil || op.Status != "running" || op.State.Runtime != "starting" {
		t.Fatalf("starting: %+v %v", op, err)
	}
	if err = f.c.Close(); err != nil {
		t.Fatal(err)
	}
	f.c = nil
	f.open()
	op, err = f.c.Operation(ctx, "start")
	if err != nil || op.Status != "uncertain" {
		t.Fatalf("interruption: %+v %v", op, err)
	}
	if _, err = f.c.Start(ctx, "bypass"); !errors.Is(err, ErrUncertain) {
		t.Fatalf("new ID bypassed pending intent: %v", err)
	}
	f.health = "healthy"
	op, err = f.c.Operation(ctx, "start")
	if err != nil || op.Status != "succeeded" || f.launches() != 1 {
		t.Fatalf("readiness: %+v %v", op, err)
	}
}

func TestStoppedStartSettlesWithoutControllerRestart(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.health = "starting"
	op, err := f.c.Start(ctx, "starting")
	if err != nil || op.Status != "running" {
		t.Fatalf("start: %+v %v", op, err)
	}
	f.app = false
	f.rootful = true
	if _, err := f.c.Operation(ctx, "starting"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("unknown observation settled start: %v", err)
	}
	if _, err := f.c.Start(ctx, "fresh"); !errors.Is(err, ErrUncertain) {
		t.Fatalf("unknown observation allowed another start: %v", err)
	}
	f.rootful = false
	op, err = f.c.Operation(ctx, "starting")
	if err != nil || op.Status != "failed" || op.Stage != "complete" {
		t.Fatalf("stopped start did not settle: %+v %v", op, err)
	}
	if f.launches() != 1 {
		t.Fatal("observation replayed dispatch")
	}
	f.health = "healthy"
	op, err = f.c.Start(ctx, "fresh")
	if err != nil || op.Status != "succeeded" || f.launches() != 2 {
		t.Fatalf("fresh start: %+v %v", op, err)
	}
	op, err = f.c.Operation(ctx, "starting")
	if err != nil || op.Status != "failed" || f.launches() != 2 {
		t.Fatalf("terminal receipt changed: %+v %v", op, err)
	}
}

func TestInterruptedStartObservedStoppedAllowsFreshStart(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.health = "starting"
	if _, err := f.c.Start(ctx, "interrupted"); err != nil {
		t.Fatal(err)
	}
	if err := f.c.Close(); err != nil {
		t.Fatal(err)
	}
	f.c = nil
	f.open()
	f.app = false
	f.rootful = true
	op, err := f.c.Operation(ctx, "interrupted")
	if !errors.Is(err, ErrUnavailable) || op.Status != "uncertain" || op.State.Runtime != "unknown" {
		t.Fatalf("unavailable observation resolved interruption: %+v %v", op, err)
	}
	if _, err = f.c.Start(ctx, "bypass"); !errors.Is(err, ErrUncertain) {
		t.Fatalf("new ID bypassed unknown runtime: %v", err)
	}
	f.rootful = false
	op, err = f.c.Start(ctx, "interrupted")
	if err != nil || op.Status != "failed" || op.Stage != "complete" || op.Code != "interrupted" || f.launches() != 1 {
		t.Fatalf("stopped interruption: %+v %v", op, err)
	}
	f.health = "healthy"
	op, err = f.c.Start(ctx, "fresh")
	if err != nil || op.Status != "succeeded" || f.launches() != 2 {
		t.Fatalf("fresh start: %+v %v", op, err)
	}
}

func TestObservationRefusesBypassAndFalseHealth(t *testing.T) {
	cases := map[string]func(*fixture){
		"rootful":           func(f *fixture) { f.rootful = true },
		"published port":    func(f *fixture) { f.ports = map[string][]any{"8837/tcp": {map[string]string{"HostPort": "8837"}}} },
		"host network":      func(f *fixture) { f.network = "host" },
		"container network": func(f *fixture) { f.network = "container:other" },
		"extra service":     func(f *fixture) { f.extra = true },
		"unmanaged bind":    func(f *fixture) { f.wrongBind = true },
		"unmanaged volume": func(f *fixture) {
			f.mutateMounts = func(m []map[string]any) []map[string]any {
				return append(m, map[string]any{"Type": "volume", "Source": "/unmanaged/palace", "Destination": "/home/agent/.mempalace", "RW": true})
			}
		},
		"unexpected read-only image": func(f *fixture) {
			f.mutateMounts = func(m []map[string]any) []map[string]any {
				return append(m, map[string]any{"Type": "image", "Source": "unmanaged-image", "Destination": "/data/cache", "RW": false})
			}
		},
		"duplicate bind": func(f *fixture) {
			f.mutateMounts = func(m []map[string]any) []map[string]any { return append(m, m[0]) }
		},
		"missing machine identity": func(f *fixture) {
			f.mutateMounts = func(m []map[string]any) []map[string]any { return m[:len(m)-1] }
		},
		"writable machine identity": func(f *fixture) {
			f.mutateMounts = func(m []map[string]any) []map[string]any {
				m[len(m)-1]["RW"] = true
				return m
			}
		},
		"machine identity volume": func(f *fixture) {
			f.mutateMounts = func(m []map[string]any) []map[string]any {
				m[len(m)-1]["Type"] = "volume"
				return m
			}
		},
		"workspace tmpfs overlay": func(f *fixture) { f.tmpfs["/data/cache"] = "rw" },
		"missing runtime tmpfs":   func(f *fixture) { f.tmpfs = nil },
		"redirected runtime tmpfs": func(f *fixture) {
			f.tmpfs = map[string]string{"/data": "rw"}
		},
		"missing private listener":    func(f *fixture) { f.markers = "bpcau" },
		"duplicate transport setting": func(f *fixture) { f.markers = "bspcauu" },
		"dead process":                func(f *fixture) { f.process = "" },
		"stale health":                func(f *fixture) { f.failHealth = true },
		"world readable socket": func(f *fixture) {
			if err := os.Chmod(filepath.Join(f.cfg.StateRoot, "data/gateway/backend.sock"), 0666); err != nil {
				t.Fatal(err)
			}
		},
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			if _, err := f.c.Start(context.Background(), "start"); err != nil {
				t.Fatal(err)
			}
			change(f)
			s, err := f.c.Observe(context.Background())
			if err == nil || s.Runtime != "unknown" || s.Epoch != "" {
				t.Fatalf("unsafe observed ready: %+v %v", s, err)
			}
			if f.launches() != 1 {
				t.Fatal("observation dispatched start")
			}
		})
	}
}

func TestUnmanagedRootsAndChangedIdentitiesRefused(t *testing.T) {
	for _, name := range []string{"existing.db", "gocryptfs.conf", "data"} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			p := filepath.Join(f.cfg.StateRoot, name)
			if name == "data" {
				if err := os.Mkdir(p, 0700); err != nil {
					t.Fatal(err)
				}
			} else if err := os.WriteFile(p, []byte("not owned by controller"), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := f.c.Start(context.Background(), "start"); !errors.Is(err, ErrStorage) {
				t.Fatalf("adopted unmanaged root: %v", err)
			}
			if f.launches() != 0 {
				t.Fatal("unsafe root launched")
			}
		})
	}
	for _, name := range []string{"root", "data", "data/gateway"} {
		t.Run("replace "+name, func(t *testing.T) {
			f := newFixture(t)
			if _, err := f.c.Start(context.Background(), "start"); err != nil {
				t.Fatal(err)
			}
			p := f.cfg.StateRoot
			if name != "root" {
				p = filepath.Join(p, name)
			}
			if err := os.Rename(p, p+"-original"); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(p, 0700); err != nil {
				t.Fatal(err)
			}
			if _, err := f.c.Observe(context.Background()); !errors.Is(err, ErrStorage) {
				t.Fatalf("replaced root/store accepted: %v", err)
			}
		})
	}
}

func TestPreparedResumeOnlyKnownOwnedDirectories(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if _, err := f.c.db.Exec(`INSERT INTO operations(id,status,stage) VALUES('prepared','running','prepared')`); err != nil {
		t.Fatal(err)
	}
	if err := f.c.Close(); err != nil {
		t.Fatal(err)
	}
	f.c = nil
	f.open()
	op, err := f.c.Operation(ctx, "prepared")
	if err != nil || op.Status != "uncertain" || f.launches() != 0 {
		t.Fatalf("observation performed provisioning: %+v %v", op, err)
	}
	op, err = f.c.Start(ctx, "prepared")
	if err != nil || op.Status != "succeeded" || f.launches() != 1 {
		t.Fatalf("known pre-dispatch resume: %+v %v", op, err)
	}
	// A created but unrecorded directory could belong to another writer.
	g := newFixture(t)
	if _, err = g.c.db.Exec(`INSERT INTO operations(id,status,stage) VALUES('partial','running','prepared')`); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(g.cfg.StateRoot, marker), []byte(g.c.fingerprint+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.Mkdir(filepath.Join(g.cfg.StateRoot, "data"), 0700); err != nil {
		t.Fatal(err)
	}
	if _, err = g.c.Start(ctx, "partial"); !errors.Is(err, ErrStorage) || g.launches() != 0 {
		t.Fatalf("ambiguous provisioning adopted: %v", err)
	}
}

func TestConfigurationAndEnvironmentBoundaries(t *testing.T) {
	f := newFixture(t)
	for _, field := range []func(*Config) *string{func(c *Config) *string { return &c.StateRoot }, func(c *Config) *string { return &c.ControlDir }, func(c *Config) *string { return &c.HomeDir }, func(c *Config) *string { return &c.RuntimeDir }, func(c *Config) *string { return &c.ComposeEnvFile }} {
		cfg := f.cfg
		original := *field(&cfg)
		alias := filepath.Join(t.TempDir(), "alias")
		if err := os.Symlink(original, alias); err != nil {
			t.Fatal(err)
		}
		*field(&cfg) = alias
		if ValidateConfig(cfg) == nil {
			t.Fatal("symlink configuration accepted")
		}
	}
	t.Setenv("NOISEDECK_AUTH_SESSION_SECRET", "do-not-inherit")
	t.Setenv("CONTAINER_HOST", "ssh://unexpected")
	if _, err := f.c.Start(context.Background(), "start"); err != nil {
		t.Fatal(err)
	}
	for _, call := range f.calls {
		for _, v := range call.env {
			if strings.Contains(v, "do-not-inherit") || strings.HasPrefix(v, "CONTAINER_HOST=") {
				t.Fatal("inherited ambient credential or engine")
			}
		}
		if call.path == f.cfg.ComposePath && (!slices.Equal(call.args, []string{f.cfg.ComposeEnvFile, "up"}) || call.capture) {
			t.Fatal("arbitrary wrapper parameters or captured output")
		}
	}
	for _, id := range []string{"../start", "", "-start", strings.Repeat("x", 129)} {
		if _, err := f.c.Start(context.Background(), id); !errors.Is(err, ErrPrecondition) {
			t.Fatalf("bad operation ID: %v", err)
		}
	}
}
