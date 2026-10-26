//go:build linux

package tmux

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func waitForZombie(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		stat, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
		if err == nil {
			fields := strings.Fields(string(stat)[strings.LastIndexByte(string(stat), ')')+1:])
			if len(fields) > 0 && fields[0] == "Z" {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("process %d did not become a zombie: %v", pid, err)
		}
		time.Sleep(time.Millisecond)
	}
}

func startExitProcess(t *testing.T) *exec.Cmd {
	t.Helper()
	cmd := exec.Command("/bin/sh", "-c", "exit 0")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})
	return cmd
}

func prepareTestPIDWait(t *testing.T, pid int) pidWaiter {
	t.Helper()
	wait, err := preparePIDWait(nil, pid)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(wait.close)
	return wait
}

func startLiveProcess(t *testing.T) *exec.Cmd {
	t.Helper()
	cmd := exec.Command("/bin/sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})
	return cmd
}

func TestWaitPIDGoneObservesZombieExit(t *testing.T) {
	cmd := startExitProcess(t)
	waitForZombie(t, cmd.Process.Pid)
	wait := prepareTestPIDWait(t, cmd.Process.Pid)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	started := time.Now()
	if err := wait.wait(ctx); err != nil {
		t.Fatalf("wait for zombie: %v", err)
	}
	if elapsed := time.Since(started); elapsed > 100*time.Millisecond {
		t.Fatalf("zombie observation took %v", elapsed)
	}
}

func TestWaitPIDGoneIgnoresReplacementProcess(t *testing.T) {
	exited := startLiveProcess(t)
	wait := prepareTestPIDWait(t, exited.Process.Pid)
	if err := exited.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if _, err := exited.Process.Wait(); err != nil {
		t.Fatal(err)
	}
	replacement := startLiveProcess(t)
	if err := wait.wait(context.Background()); err != nil {
		t.Fatalf("wait for original process: %v", err)
	}
	if err := syscall.Kill(replacement.Process.Pid, 0); err != nil {
		t.Fatalf("replacement process is not running: %v", err)
	}
}

func TestWaitPIDGoneTimesOutForLiveProcess(t *testing.T) {
	cmd := startLiveProcess(t)
	wait := prepareTestPIDWait(t, cmd.Process.Pid)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := wait.wait(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("live process wait error = %v, want deadline exceeded", err)
	}
}

func TestWaitPIDGoneHonorsCancellation(t *testing.T) {
	for _, tc := range []struct {
		name   string
		cancel func(context.CancelFunc)
	}{
		{name: "already canceled", cancel: func(cancel context.CancelFunc) { cancel() }},
		{name: "during poll", cancel: func(cancel context.CancelFunc) {
			time.AfterFunc(20*time.Millisecond, cancel)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := startLiveProcess(t)
			wait := prepareTestPIDWait(t, cmd.Process.Pid)
			ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
			defer cancel()
			tc.cancel(cancel)
			started := time.Now()
			if err := wait.wait(ctx); !errors.Is(err, context.Canceled) {
				t.Fatalf("wait error = %v, want canceled", err)
			}
			if elapsed := time.Since(started); elapsed > 150*time.Millisecond {
				t.Fatalf("cancellation took %v", elapsed)
			}
		})
	}
}

func TestKillServerSocketLossDoesNotProveProcessExit(t *testing.T) {
	c := newTestClient(t)
	mustCreate(t, c, spec("socket-loss", "/bin/sleep", "300"))
	pidText, err := c.run(context.Background(), "display-message", "-p", "#{pid}")
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(pidText)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = syscall.Kill(pid, syscall.SIGKILL)
	})
	c.timeout = 50 * time.Millisecond
	c.testHooks = &clientTestHooks{beforeServerKill: func() {
		if err := os.Remove(c.SocketPath()); err != nil {
			t.Errorf("remove socket: %v", err)
		}
	}}
	if err := c.KillServer(context.Background()); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("KillServer with a live process and missing socket = %v, want deadline exceeded", err)
	}
	if err := syscall.Kill(pid, 0); err != nil {
		t.Fatalf("original server did not remain running: %v", err)
	}
}
