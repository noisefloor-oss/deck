package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestTiniKeepsStoppedLatchAliveAndForwardsContinue(t *testing.T) {
	if os.Getenv("LATCH_TINI_HELPER") == "1" {
		runHelper()
		return
	}
	if _, err := os.Stat("/usr/bin/tini"); err != nil {
		t.Skip("local tini is unavailable")
	}
	canary := filepath.Join(t.TempDir(), "provider-ran")
	cmd := exec.Command("/usr/bin/tini", "-g", "--", os.Args[0], "-test.run=TestTiniKeepsStoppedLatchAliveAndForwardsContinue", "--", "/usr/local/bin/node", "-e", "require('fs').writeFileSync(process.argv[1], '')", canary)
	cmd.Env = latchEnvironment("LATCH_TINI_HELPER=1", strings.Repeat("a", 32))
	cmd.Stderr = os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := false
	t.Cleanup(func() {
		if !done {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			_, _ = cmd.Process.Wait()
		}
	})
	child := waitChild(t, cmd.Process.Pid)
	waitStopped(t, child)
	if _, err := os.Stat(canary); !os.IsNotExist(err) {
		t.Fatalf("provider ran before acceptance: %v", err)
	}
	if err := cmd.Process.Signal(syscall.SIGCONT); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("tini/provider after acceptance: %v", err)
	}
	done = true
	if _, err := os.Stat(canary); err != nil {
		t.Fatalf("provider did not run: %v", err)
	}
}

func TestLatchStopsBeforeProviderAndContinuesExactlyOnce(t *testing.T) {
	if os.Getenv("LATCH_TEST_HELPER") == "1" {
		runHelper()
		return
	}
	canary := filepath.Join(t.TempDir(), "provider-ran")
	cmd := exec.Command(os.Args[0], "-test.run=TestLatchStopsBeforeProviderAndContinuesExactlyOnce", "--", "/usr/local/bin/node", "-e", "require('fs').writeFileSync(process.argv[1], '')", canary)
	cmd.Env = latchEnvironment("LATCH_TEST_HELPER=1", strings.Repeat("a", 32))
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() })
	waitStopped(t, cmd.Process.Pid)
	if _, err := os.Stat(canary); !os.IsNotExist(err) {
		t.Fatalf("provider ran before acceptance: %v", err)
	}
	if err := cmd.Process.Signal(syscall.SIGCONT); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("provider after acceptance: %v", err)
	}
	if _, err := os.Stat(canary); err != nil {
		t.Fatalf("provider did not run: %v", err)
	}
}

func TestLatchRejectsRelativePathAndDangerousEnvironment(t *testing.T) {
	if os.Getenv("LATCH_REJECT_HELPER") == "1" {
		runHelper()
		return
	}
	for name, args := range map[string][]string{"relative": {"touch", "/tmp/never"}, "writable path": {"/dev/shm/provider"}, "loader": {"/usr/local/bin/provider"}} {
		t.Run(name, func(t *testing.T) {
			cmd := exec.Command(os.Args[0], append([]string{"-test.run=TestLatchRejectsRelativePathAndDangerousEnvironment", "--"}, args...)...)
			cmd.Env = latchEnvironment("LATCH_REJECT_HELPER=1", strings.Repeat("a", 32))
			if name == "loader" {
				cmd.Env = append(cmd.Env, "LD_PRELOAD=/tmp/x")
			}
			if err := cmd.Run(); err == nil || cmd.ProcessState.ExitCode() != 64 {
				t.Fatalf("unsafe bootstrap accepted: %v", err)
			}
		})
	}
}

func TestEnvironmentValidationRejectsDuplicateMalformedAndGitSelection(t *testing.T) {
	valid := []string{"HOME=/home/agent", "PATH=/usr/local/bin:/usr/bin:/bin", "NODE_VERSION=26.12.1", "YARN_VERSION=1.22.22", "TERM=xterm-256color", "HOSTNAME=ndq-" + strings.Repeat("a", 32)}
	cases := [][]string{
		{"HOME=x", "HOME=y"}, {"bad=x"}, {"GIT_EXEC_PATH=/tmp"}, {"GIT_CONFIG_COUNT=1"}, {"BASH_ENV=/tmp/x"}, {"UNKNOWN=value"},
		valid[:len(valid)-1], append(append([]string(nil), valid...), "HOSTNAME=ndq-"+strings.Repeat("b", 32)),
		append(append([]string(nil), valid[:len(valid)-1]...), "HOSTNAME=ndq-short"),
		append(append([]string(nil), valid[:len(valid)-1]...), "HOSTNAME=ndq-"+strings.Repeat("A", 32)),
	}
	for _, legacy := range []string{"LANG=C", "LC_ALL=C", "COLORTERM=truecolor", "NO_COLOR=1", "CI=1"} {
		cases = append(cases, append(append([]string(nil), valid...), legacy))
	}
	wrongValues := []string{"HOME=/tmp", "PATH=/tmp", "NODE_VERSION=0", "YARN_VERSION=0", "TERM=dumb", "HOSTNAME=ndq-" + strings.Repeat("A", 32)}
	for n := range valid {
		missing := append([]string(nil), valid[:n]...)
		missing = append(missing, valid[n+1:]...)
		cases = append(cases, missing, append(append([]string(nil), valid...), valid[n]))
		changed := append([]string(nil), valid...)
		changed[n] = wrongValues[n]
		cases = append(cases, changed)
	}
	for _, env := range cases {
		if validateEnvironment(env) == nil {
			t.Fatalf("accepted %v", env)
		}
	}
}

func TestProviderEnvironmentPreservesCanonicalHostname(t *testing.T) {
	environment := []string{"HOME=/home/agent", "PATH=/usr/local/bin:/usr/bin:/bin", "NODE_VERSION=26.12.1", "YARN_VERSION=1.22.22", "TERM=xterm-256color", "HOSTNAME=ndq-" + strings.Repeat("a", 32)}
	got := providerEnvironment(environment)
	if len(got) != len(environment) {
		t.Fatalf("provider environment length = %d, want %d", len(got), len(environment))
	}
	for n := range environment {
		if got[n] != environment[n] {
			t.Fatalf("provider environment[%d] = %q, want %q", n, got[n], environment[n])
		}
	}
}

func TestCanonicalHostnameShapeDoesNotReleaseProvider(t *testing.T) {
	if os.Getenv("LATCH_HOSTNAME_HELPER") == "1" {
		runHelper()
		return
	}
	canary := filepath.Join(t.TempDir(), "provider-ran")
	cmd := exec.Command(os.Args[0], "-test.run=TestCanonicalHostnameShapeDoesNotReleaseProvider", "--", "/usr/local/bin/node", "-e", "require('fs').writeFileSync(process.argv[1], '')", canary)
	cmd.Env = latchEnvironment("LATCH_HOSTNAME_HELPER=1", strings.Repeat("b", 32))
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waitStopped(t, cmd.Process.Pid)
	if _, err := os.Stat(canary); !os.IsNotExist(err) {
		t.Fatalf("provider ran before hostname authority was verified: %v", err)
	}
	_ = cmd.Process.Kill()
	_, _ = cmd.Process.Wait()
}

func TestBootstrapNegativeHostnameExits64WithoutProvider(t *testing.T) {
	if os.Getenv("LATCH_NEGATIVE_HELPER") == "1" {
		runHelper()
		return
	}
	commands := map[string]func(string) *exec.Cmd{
		"direct": func(canary string) *exec.Cmd {
			return exec.Command(os.Args[0], "-test.run=TestBootstrapNegativeHostnameExits64WithoutProvider", "--", "/usr/local/bin/node", "-e", "require('fs').writeFileSync(process.argv[1], '')", canary)
		},
	}
	if _, err := os.Stat("/usr/bin/tini"); err == nil {
		commands["tini"] = func(canary string) *exec.Cmd {
			return exec.Command("/usr/bin/tini", "-g", "--", os.Args[0], "-test.run=TestBootstrapNegativeHostnameExits64WithoutProvider", "--", "/usr/local/bin/node", "-e", "require('fs').writeFileSync(process.argv[1], '')", canary)
		}
	}
	for name, command := range commands {
		t.Run(name, func(t *testing.T) {
			canary := filepath.Join(t.TempDir(), "provider-ran")
			cmd := command(canary)
			cmd.Env = latchEnvironment("LATCH_NEGATIVE_HELPER=1", strings.Repeat("a", 32))
			for i, entry := range cmd.Env {
				if strings.HasPrefix(entry, "HOSTNAME=") {
					cmd.Env[i] = "HOSTNAME=ndq-" + strings.Repeat("a", 32) + "-bootstrap-negative"
				}
			}
			err := cmd.Run()
			if err == nil || cmd.ProcessState.ExitCode() != 64 {
				t.Fatalf("bootstrap-negative exit = %v, want 64", err)
			}
			if _, err := os.Stat(canary); !os.IsNotExist(err) {
				t.Fatalf("provider canary executed: %v", err)
			}
		})
	}
}

func runHelper() {
	_ = os.Unsetenv("LATCH_TEST_HELPER")
	_ = os.Unsetenv("LATCH_TINI_HELPER")
	_ = os.Unsetenv("LATCH_REJECT_HELPER")
	_ = os.Unsetenv("LATCH_HOSTNAME_HELPER")
	_ = os.Unsetenv("LATCH_NEGATIVE_HELPER")
	for n, arg := range os.Args {
		if arg == "--" {
			os.Args = append([]string{os.Args[0]}, os.Args[n+1:]...)
			main()
			return
		}
	}
	os.Exit(65)
}

func latchEnvironment(helper, runID string) []string {
	environment := []string{
		"HOME=/home/agent",
		"PATH=/usr/local/bin:/usr/bin:/bin",
		"NODE_VERSION=26.12.1",
		"YARN_VERSION=1.22.22",
		"TERM=xterm-256color",
		"HOSTNAME=ndq-" + runID,
	}
	if helper != "" {
		environment = append(environment, helper)
	}
	return environment
}
func waitStopped(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	var lastState string
	for time.Now().Before(deadline) {
		raw, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "status"))
		if err != nil {
			t.Fatalf("read latch %d state: %v", pid, err)
		}
		for _, line := range strings.Split(string(raw), "\n") {
			if strings.HasPrefix(line, "State:") {
				lastState = line
				if strings.Contains(line, "T (stopped)") {
					assertStillLatch(t, pid)
					return
				}
				if strings.Contains(line, "Z (zombie)") || strings.Contains(line, "X (dead)") {
					t.Fatalf("latch %d exited before stopping: %s", pid, line)
				}
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("latch %d did not stop: %s", pid, lastState)
}

// assertStillLatch fails when the stopped process has already exec'd the
// provider, which must wait for SIGCONT.
func assertStillLatch(t *testing.T, pid int) {
	t.Helper()
	want, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.Readlink(filepath.Join("/proc", strconv.Itoa(pid), "exe"))
	if err != nil {
		t.Fatalf("read latch %d image: %v", pid, err)
	}
	if got != want {
		t.Fatalf("latch %d stopped as %s, want %s", pid, got, want)
	}
}

func waitChild(t *testing.T, pid int) int {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		entries, err := os.ReadDir("/proc")
		if err == nil {
			for _, entry := range entries {
				child, err := strconv.Atoi(entry.Name())
				if err != nil || child <= 0 {
					continue
				}
				raw, err := os.ReadFile(filepath.Join("/proc", entry.Name(), "status"))
				if err == nil && strings.Contains(string(raw), "\nPPid:\t"+strconv.Itoa(pid)+"\n") {
					return child
				}
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("tini did not start latch child")
	return 0
}
