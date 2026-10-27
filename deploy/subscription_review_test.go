package deploy

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestSubscriptionReviewRejectsMissingOrAPIAuthentication(t *testing.T) {
	for _, fixture := range []struct{ name, auth string }{
		{"missing", ""},
		{"API", `{"auth_mode":"apikey","OPENAI_API_KEY":"canary"}`},
		{"incomplete", `{"auth_mode":"chatgpt","tokens":{}}`},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			if _, err := exec.LookPath("jq"); err != nil {
				t.Skip("jq is required")
			}
			dir := t.TempDir()
			if fixture.auth != "" {
				if err := os.WriteFile(filepath.Join(dir, "auth.json"), []byte(fixture.auth), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			cmd := exec.Command("bash", "../scripts/harness-bump/review-jail.sh", "--hostile")
			cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "LD_LIBRARY_PATH=" + os.Getenv("LD_LIBRARY_PATH"), "JAIL_AUTH_DIR=" + dir}
			for _, key := range []string{"JAIL_IMAGE", "JAIL_NODE", "JAIL_CODEX", "JAIL_GO", "JAIL_GOMOD", "JAIL_SOCKET", "JAIL_WORK", "JAIL_BROKER", "JAIL_MODEL", "JAIL_REPO", "JAIL_PR", "JAIL_TOOL", "JAIL_BUMP"} {
				cmd.Env = append(cmd.Env, key+"=unused")
			}
			out, err := cmd.CombinedOutput()
			if err == nil || !strings.Contains(string(out), "login") {
				t.Fatalf("invalid login not rejected: %v %s", err, out)
			}
			if strings.Contains(string(out), "canary") {
				t.Fatal("credential value reached output")
			}
		})
	}
}

func TestSubscriptionReviewWorkflowHasNoAPIKeyDependency(t *testing.T) {
	for _, workflow := range []string{"harness-bump.yaml", "harness-bump-merge.yaml"} {
		bytes, err := os.ReadFile(filepath.Join("..", ".github", "workflows", workflow))
		if os.IsNotExist(err) {
			continue // absent from the source export
		}
		if err != nil {
			t.Fatal(err)
		}
		raw := string(bytes)
		if strings.Contains(raw, "secrets.OPENAI_API_KEY") || !strings.Contains(raw, "vars.NOISE_REVIEW_CODEX_AUTH_DIR") {
			t.Fatalf("%s does not use the local subscription configuration", workflow)
		}
	}
	raw := string(readWorkflow(t, "ci-fast.yaml"))
	if !strings.Contains(raw, "Probe the subscription jail without a real login") || !strings.Contains(raw, "review-proxy -socket") {
		t.Fatal("CI does not exercise the subscription jail")
	}
}

func TestSubscriptionAuthenticationPromotionHasTrustedProvenance(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq is required")
	}
	for _, mode := range []string{"poisoned-review", "failed-refresh", "uncertain-stop", "successful-probe-uncertain-stop", "missing-cidfile", "successful-auth-probe", "invalid-label"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			for _, name := range []string{"bin", "auth", "candidate"} {
				if err := os.Mkdir(filepath.Join(dir, name), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			write := func(name, body string) {
				t.Helper()
				if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			write("auth/auth.json", `{"auth_mode":"chatgpt","tokens":{"access_token":"original","refresh_token":"original"}}`)
			write("candidate/untrusted", "candidate bytes")
			write("diff", "fixture")
			write("bin/bump", "#!/bin/sh\ncat\n")
			write("bin/docker", `#!/bin/bash
set -eu
case "$1" in
  ps)
    case "$MODE" in *uncertain-stop) exit 1 ;; esac
    if [ -f "$FIXTURE/container-name" ]; then
      printf '%064d %s\n' 1 "$(cat "$FIXTURE/container-name")"
    fi
    exit 0 ;;
  rm) rm -f "$FIXTURE/container-name"; touch "$FIXTURE/removed"; exit 0 ;;
  run) shift ;;
  *) exit 90 ;;
esac
auth= work= cid= name= mounts=0
while [ "$#" -gt 0 ]; do
  case "$1" in
    --cidfile) cid="$2"; shift 2 ;;
    --name) name="$2"; shift 2 ;;
    --volume)
      case "$2" in
        *:/home/jail/.codex:Z) auth="${2%:/home/jail/.codex:Z}" ;;
        *:/work:Z) work="${2%:/work:Z}" ;;
        *:/run/broker/broker.sock:ro,Z|*:/opt/broker/noise-review:ro,Z) ;;
        *:/opt/review-lib:ro,Z)
          library="${2%:/opt/review-lib:ro,Z}"
          case "$library" in "$FIXTURE"/tmp.*/review-lib) ;; *) exit 95 ;; esac
          test -d "$library" && test -z "$(ls -A "$library")" || exit 96 ;;
        *:/opt/broker/bump:ro,z|*:/opt/node:ro,z|*:/opt/codex:ro,z|*:/usr/local/go:ro,z|*:/opt/gomod:ro,z) ;;
        *) echo 'unexpected review mount or SELinux label' >&2; exit 93 ;;
      esac
      mounts=$((mounts + 1))
      shift 2 ;;
    *) shift ;;
  esac
done
test "$mounts" -eq 10 && test -n "$auth" && test -n "$work" || exit 94
if [ "$MODE" = successful-probe-uncertain-stop ]; then exit 0; fi
if [ "$MODE" = missing-cidfile ]; then
  printf %s "$name" >"$FIXTURE/container-name"
  exit 0
fi
printf '%064d' 0 >"$cid"
if [ ! -f "$FIXTURE/refreshed" ]; then
  test -z "$(ls -A "$work")" || exit 91
  printf '%s\n' '{"auth_mode":"chatgpt","tokens":{"access_token":"refreshed","refresh_token":"refreshed"}}' >"$auth/auth.json"
  touch "$FIXTURE/refreshed"
  [ "$MODE" != failed-refresh ] || exit 1
  exit 0
fi
test -f "$work/untrusted" || exit 92
printf '%s\n' '{"auth_mode":"chatgpt","tokens":{"access_token":"poison","refresh_token":"poison"},"untrusted":"persistent instruction"}' >"$auth/auth.json"
exit 1
`)
			cmd := exec.Command("bash", "../scripts/harness-bump/review-jail.sh")
			if mode == "successful-probe-uncertain-stop" || mode == "missing-cidfile" {
				cmd.Args = append(cmd.Args, "--hostile")
			}
			if mode == "successful-auth-probe" {
				cmd.Args = append(cmd.Args, "--auth-probe")
			}
			cmd.Stdin = strings.NewReader("fixture prompt")
			cmd.Env = []string{"PATH=" + filepath.Join(dir, "bin") + ":" + os.Getenv("PATH"), "LD_LIBRARY_PATH=" + os.Getenv("LD_LIBRARY_PATH"), "MODE=" + mode, "FIXTURE=" + dir,
				"JAIL_HEAD=" + strings.Repeat("1", 40), "JAIL_BASE=" + strings.Repeat("2", 40),
				"JAIL_AUTH_DIR=" + filepath.Join(dir, "auth"), "JAIL_WORK=" + filepath.Join(dir, "candidate"),
				"JAIL_BUMP=" + filepath.Join(dir, "bin", "bump"), "JAIL_DIFF=" + filepath.Join(dir, "diff"), "TMPDIR=" + dir}
			level := "s0:c21,c32"
			if mode == "invalid-label" {
				level = "disable"
			}
			cmd.Env = append(cmd.Env, "JAIL_SELINUX_LEVEL="+level)
			for _, key := range []string{"JAIL_IMAGE", "JAIL_NODE", "JAIL_CODEX", "JAIL_GO", "JAIL_GOMOD", "JAIL_SOCKET", "JAIL_BROKER", "JAIL_MODEL", "JAIL_REPO", "JAIL_PR", "JAIL_TOOL"} {
				cmd.Env = append(cmd.Env, key+"=unused")
			}
			out, err := cmd.CombinedOutput()
			if err == nil && mode != "missing-cidfile" && mode != "successful-auth-probe" {
				t.Fatalf("failed workload reported success: %s", out)
			}
			if mode == "successful-auth-probe" && (err != nil || !strings.Contains(string(out), "subscription authentication probe passed")) {
				t.Fatalf("fixed authentication probe failed: %v %s", err, out)
			}
			if mode == "invalid-label" {
				if _, err := os.Stat(filepath.Join(dir, "refreshed")); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("invalid SELinux level reached the container")
				}
			}
			if mode == "missing-cidfile" {
				if err != nil {
					t.Fatalf("named container cleanup failed: %v %s", err, out)
				}
				if _, err := os.Stat(filepath.Join(dir, "removed")); err != nil {
					t.Fatal("container with missing cidfile was not removed")
				}
			}
			auth, err := os.ReadFile(filepath.Join(dir, "auth", "auth.json"))
			if err != nil {
				t.Fatal(err)
			}
			want := "original"
			if mode == "poisoned-review" || mode == "successful-auth-probe" {
				want = "refreshed"
			}
			if !strings.Contains(string(auth), want) || strings.Contains(string(auth), "poison") || strings.Contains(string(auth), "untrusted") {
				t.Fatalf("saved login provenance failed for %s", mode)
			}
			if strings.HasSuffix(mode, "uncertain-stop") && !strings.Contains(string(out), "teardown uncertain") {
				t.Fatalf("uncertain teardown was not reported: %s", out)
			}
		})
	}
}

func TestReviewInterruptBeforeContainerRegistration(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"bin", "candidate"} {
		if err := os.Mkdir(filepath.Join(dir, name), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	fake := `#!/bin/bash
set -eu
case "$1" in
  ps) exit 0 ;;
  run)
    printf '%s' "$$" >"$FIXTURE/launcher.pid"
    sleep 1
    touch "$FIXTURE/late-registered"
    sleep 300 ;;
  *) exit 90 ;;
esac
`
	if err := os.WriteFile(filepath.Join(dir, "bin", "docker"), []byte(fake), 0o700); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", "../scripts/harness-bump/review-jail.sh", "--interrupt-probe")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Env = []string{"PATH=" + filepath.Join(dir, "bin") + ":" + os.Getenv("PATH"), "FIXTURE=" + dir, "TMPDIR=" + dir, "JAIL_WORK=" + filepath.Join(dir, "candidate")}
	for _, key := range []string{"JAIL_IMAGE", "JAIL_NODE", "JAIL_CODEX", "JAIL_GO", "JAIL_GOMOD", "JAIL_SOCKET", "JAIL_BROKER", "JAIL_MODEL", "JAIL_REPO", "JAIL_PR", "JAIL_TOOL", "JAIL_BUMP"} {
		cmd.Env = append(cmd.Env, key+"=unused")
	}
	var output bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &output
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(filepath.Join(dir, "launcher.pid")); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("launcher did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != 143 {
			t.Fatalf("interrupt exit: %v %s", err, output.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("interrupted launcher was not reaped")
	}
	time.Sleep(1100 * time.Millisecond)
	if _, err := os.Stat(filepath.Join(dir, "late-registered")); err == nil {
		t.Fatal("launcher registered a container after cleanup")
	}
}
