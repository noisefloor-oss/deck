//go:build linux

package workspaceruntime

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/noisefloor-oss/deck/internal/sealedstate"
)

type runnerFunc func(context.Context, string, []string, []string, []byte, bool) ([]byte, error)

func (r runnerFunc) run(ctx context.Context, path string, args, env []string, input []byte, capture bool) ([]byte, error) {
	return r(ctx, path, args, env, input, capture)
}

func runningFixture(t *testing.T) *fixture {
	t.Helper()
	f := newFixture(t)
	if _, err := f.c.Initialize(context.Background(), "init", bytes.Repeat([]byte{1}, 32)); err != nil {
		t.Fatal(err)
	}
	if _, err := f.c.Start(context.Background(), "start"); err != nil {
		t.Fatal(err)
	}
	f.calls = nil
	return f
}

func TestLockStopsProjectBeforeNormalUnmount(t *testing.T) {
	f := runningFixture(t)
	prepared := false
	f.c.run = runnerFunc(func(ctx context.Context, path string, args, env []string, input []byte, capture bool) ([]byte, error) {
		if (path == f.cfg.SealedComposePath || path == f.cfg.FusermountPath) && !prepared {
			t.Fatal("native shutdown crossed preparation")
		}
		if path == f.cfg.FusermountPath {
			if f.app || !slices.Equal(args, []string{"-u", f.cfg.MountPoint}) {
				t.Fatal("unmount was lazy, forced, or before runtime shutdown")
			}
		}
		return f.run(ctx, path, args, env, input, capture)
	})
	op, err := f.c.Lock(context.Background(), "lock", func(context.Context) error { prepared = true; return nil })
	if err != nil || op.Status != "succeeded" || !op.State.Locked || op.State.Storage != "closed" || op.State.Runtime != "stopped" {
		t.Fatalf("lock: %+v %v", op, err)
	}
	if !prepared {
		t.Fatal("backend was not prepared")
	}
	count := len(f.calls)
	_, err = f.c.Lock(context.Background(), "lock", func(context.Context) error { t.Fatal("replayed preparation"); return nil })
	if err != nil {
		t.Fatal(err)
	}
	for _, call := range f.calls[count:] {
		if call.path == f.cfg.FusermountPath || call.path == f.cfg.SealedComposePath {
			t.Fatal("replayed native shutdown")
		}
	}
}

func TestLockCleanRefusalAllowsFreshAttempt(t *testing.T) {
	f := runningFixture(t)
	ctx := context.Background()
	op, err := f.c.Lock(ctx, "refused", func(context.Context) error { return ErrLockRefused })
	if !errors.Is(err, ErrLockRefused) || op.Status != "failed" || op.Stage != "complete" || op.State.Storage != "open" || !f.app {
		t.Fatalf("clean refusal: %+v %v", op, err)
	}
	for _, call := range f.calls {
		if call.path == f.cfg.SealedComposePath || call.path == f.cfg.FusermountPath {
			t.Fatal("refusal authorized shutdown")
		}
	}
	if err := f.c.Close(); err != nil {
		t.Fatal(err)
	}
	f.open()
	op, err = f.c.Lock(ctx, "refused", func(context.Context) error { t.Fatal("replayed refused preparation"); return nil })
	if err != nil || op.Status != "failed" {
		t.Fatalf("refusal history changed: %+v %v", op, err)
	}
	op, err = f.c.Lock(ctx, "retry", func(context.Context) error { return nil })
	if err != nil || op.Status != "succeeded" || !op.State.Locked {
		t.Fatalf("fresh attempt blocked: %+v %v", op, err)
	}
}

func TestLockPreparationFailurePreservesOpenRuntime(t *testing.T) {
	f := runningFixture(t)
	op, err := f.c.Lock(context.Background(), "lock", func(context.Context) error { return errors.New("synthetic private failure") })
	if !errors.Is(err, ErrUncertain) || op.State.Locked || op.State.Storage != "open" || !f.app {
		t.Fatalf("failed preparation: %+v %v", op, err)
	}
	for _, call := range f.calls {
		if call.path == f.cfg.FusermountPath || call.path == f.cfg.SealedComposePath {
			t.Fatal("failed preparation authorized native shutdown")
		}
	}
	if _, err := f.c.Lock(context.Background(), "lock-new", func(context.Context) error { return nil }); !errors.Is(err, ErrUncertain) {
		t.Fatalf("new ID evaded interrupted preparation: %v", err)
	}
}

func TestLockClosedObservationRequiresDurablePreparation(t *testing.T) {
	for _, failure := range []string{"prepare", "record-preparation", "stop-runtime", "unmount"} {
		t.Run(failure, func(t *testing.T) {
			f := runningFixture(t)
			if failure == "record-preparation" {
				if _, err := f.c.db.Exec(`CREATE TRIGGER reject_prepared_stage
BEFORE UPDATE OF stage ON operations WHEN NEW.stage = 'stopping-runtime'
BEGIN SELECT RAISE(ABORT, 'synthetic preparation receipt failure'); END`); err != nil {
					t.Fatal(err)
				}
			}
			f.c.run = runnerFunc(func(ctx context.Context, path string, args, env []string, input []byte, capture bool) ([]byte, error) {
				if (failure == "stop-runtime" && path == f.cfg.SealedComposePath) ||
					(failure == "unmount" && path == f.cfg.FusermountPath) {
					return nil, ErrCommand
				}
				return f.run(ctx, path, args, env, input, capture)
			})
			op, err := f.c.Lock(context.Background(), "lock", func(context.Context) error {
				if failure == "prepare" {
					return errors.New("synthetic failure before the session cohort commit")
				}
				return nil
			})
			if !errors.Is(err, ErrUncertain) || op.Status != "uncertain" {
				t.Fatalf("interrupted lock: %+v %v", op, err)
			}
			wantStage := "preparing-stop"
			if failure == "stop-runtime" {
				wantStage = "stopping-runtime"
			}
			if failure == "unmount" {
				wantStage = "unmounting"
			}
			if op.Stage != wantStage {
				t.Fatalf("stage = %q, want %q", op.Stage, wantStage)
			}
			// A host reboot can close the workload without completing backend
			// preparation. Reopening the journal must preserve that distinction.
			if err := f.c.Close(); err != nil {
				t.Fatal(err)
			}
			f.app = false
			f.facts.Mounts = f.facts.Mounts[:1]
			f.open()
			f.calls = nil
			op, err = f.c.Lock(context.Background(), "lock", func(context.Context) error {
				t.Fatal("replayed lock preparation")
				return nil
			})
			if err != nil || !op.State.Locked {
				t.Fatalf("closed runtime observation: %+v %v", op, err)
			}
			if wantStage == "preparing-stop" {
				if op.Status != "uncertain" || op.Stage != wantStage {
					t.Fatalf("closed mount certified missing stop intent: %+v", op)
				}
				if _, err := f.c.Unlock(context.Background(), "unlock", make([]byte, 32)); !errors.Is(err, ErrUncertain) {
					t.Fatalf("unmarked generations became unlockable: %v", err)
				}
			} else if op.Status != "succeeded" {
				t.Fatalf("durably prepared lock could not converge: %+v", op)
			}
			for _, call := range f.calls {
				if call.path == f.cfg.SealedComposePath || call.path == f.cfg.FusermountPath || call.path == f.cfg.GocryptfsPath {
					t.Fatal("reconciliation dispatched a native mutation")
				}
			}
		})
	}
}

func TestLockSidecarSurvivesDownRefusesUnmount(t *testing.T) {
	f := runningFixture(t)
	f.c.run = runnerFunc(func(ctx context.Context, path string, args, env []string, input []byte, capture bool) ([]byte, error) {
		if path == f.cfg.FusermountPath {
			t.Fatal("sidecar survived but unmount was attempted")
		}
		if len(args) > 0 && args[0] == "ps" {
			return []byte("remaining-synthetic-sidecar"), nil
		}
		return f.run(ctx, path, args, env, input, capture)
	})
	op, err := f.c.Lock(context.Background(), "lock", func(context.Context) error { return nil })
	if !errors.Is(err, ErrUncertain) || op.State.Locked || op.State.Storage != "open" {
		t.Fatalf("remaining sidecar: %+v %v", op, err)
	}
}

func TestLockHangingUnmountIsUncertainAndNeverReplayed(t *testing.T) {
	f := runningFixture(t)
	unmounts := 0
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.c.run = runnerFunc(func(ctx context.Context, path string, args, env []string, input []byte, capture bool) ([]byte, error) {
		if path == f.cfg.FusermountPath {
			unmounts++
			timer := time.AfterFunc(30*time.Millisecond, cancel)
			defer timer.Stop()
			<-ctx.Done()
			return nil, ctx.Err()
		}
		return f.run(ctx, path, args, env, input, capture)
	})
	op, err := f.c.Lock(ctx, "lock", func(context.Context) error { return nil })
	if !errors.Is(err, ErrUncertain) || op.State.Locked || op.State.Storage != "open" || unmounts != 1 {
		t.Fatalf("hanging unmount: %+v %v", op, err)
	}
	op, err = f.c.Lock(context.Background(), "lock", func(context.Context) error { t.Fatal("replayed preparation"); return nil })
	if err != nil || op.Status != "uncertain" || unmounts != 1 {
		t.Fatalf("replayed uncertain operation: %+v %v", op, err)
	}
}

func TestLockRequiresAliasesAndDaemonGone(t *testing.T) {
	for _, condition := range []string{"alias", "daemon"} {
		t.Run(condition, func(t *testing.T) {
			f := runningFixture(t)
			daemon := condition == "daemon"
			f.c.closedProcesses = func(context.Context) error {
				if daemon {
					return ErrUnsafeStorage
				}
				return nil
			}
			f.c.run = runnerFunc(func(ctx context.Context, path string, args, env []string, input []byte, capture bool) ([]byte, error) {
				out, err := f.run(ctx, path, args, env, input, capture)
				if path == f.cfg.FusermountPath && condition == "alias" {
					f.facts.Mounts = append(f.facts.Mounts, sealedstate.Mount{ID: 90, MountPoint: "/synthetic-retained-alias", FSType: "fuse.gocryptfs", Source: f.cfg.CipherDir})
				}
				return out, err
			})
			op, err := f.c.Lock(context.Background(), "lock", func(context.Context) error { return nil })
			if !errors.Is(err, ErrUncertain) || op.State.Locked || op.State.Storage != "unknown" {
				t.Fatalf("remaining %s: %+v %v", condition, op, err)
			}
			daemon = false
			f.facts.Mounts = f.facts.Mounts[:1]
			op, err = f.c.Operation(context.Background(), "lock")
			if err != nil || op.Status != "succeeded" || !op.State.Locked {
				t.Fatalf("observed eventual completion: %+v %v", op, err)
			}
		})
	}
}

func TestLockCrashReopenOnlyObserves(t *testing.T) {
	f := runningFixture(t)
	_, err := f.c.db.Exec(`INSERT INTO operations(id,kind,status,stage,config_digest) VALUES('lock','lock','running','stopping-runtime',?)`, f.c.configDigest())
	if err != nil {
		t.Fatal(err)
	}
	if err := f.c.Close(); err != nil {
		t.Fatal(err)
	}
	f.open()
	op, err := f.c.Lock(context.Background(), "lock", func(context.Context) error { t.Fatal("replayed crash-left stop"); return nil })
	if err != nil || op.Status != "uncertain" || op.State.Locked || !f.app {
		t.Fatalf("crash reconciliation: %+v %v", op, err)
	}
	for _, call := range f.calls {
		if call.path == f.cfg.SealedComposePath || call.path == f.cfg.FusermountPath {
			t.Fatal("reopen dispatched native mutation")
		}
	}
}
