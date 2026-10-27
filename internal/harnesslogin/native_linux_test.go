//go:build linux

package harnesslogin

import (
	"bufio"
	"context"
	"crypto/sha256"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// This opt-in probe drives an unmodified, pinned Pi through its native UI.
// It never authenticates an account or supplies a valid authorization code.
// Terminal output is drained without inspection or storage. Native browser
// registration, process ownership and state rejection are the observations.
func TestNativePiBrowserRegistration(t *testing.T) {
	executable := os.Getenv("NOISEDECK_TEST_PI_EXECUTABLE")
	if executable == "" {
		t.Skip("set NOISEDECK_TEST_PI_EXECUTABLE for pinned native launch qualification")
	}
	version, err := exec.Command(executable, "--version").Output()
	if err != nil {
		t.Fatal("native Pi version command failed")
	}
	if strings.TrimSpace(string(version)) != shippedVersions["pi"] {
		t.Fatal("native Pi version does not match shipped release")
	}
	if !shippedPiModules(executable) {
		t.Fatal("native Pi package identity, layout, or dependency contract does not match shipped release")
	}
	for _, provider := range []string{"anthropic", "openai-codex"} {
		t.Run(provider, func(t *testing.T) {
			nativeBrowserRegistration(t, executable, "pi", provider)
		})
	}
}

// Exercise Pi's shipped callback implementation without an account or token exchange.
func TestNativePiCallbackStateSuffix(t *testing.T) {
	executable := os.Getenv("NOISEDECK_TEST_PI_EXECUTABLE")
	if executable == "" {
		t.Skip("set NOISEDECK_TEST_PI_EXECUTABLE for pinned native callback qualification")
	}
	if !shippedPiModules(executable) {
		t.Fatal("native Pi package contract does not match shipped release")
	}
	real, err := filepath.EvalSymlinks(executable)
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Dir(filepath.Dir(filepath.Dir(real)))
	module := (&url.URL{Scheme: "file", Path: filepath.Join(root, "node_modules/@earendil-works/pi-ai/dist/auth/oauth/callback-server.js")}).String()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	script := `
const { startOAuthCallbackServer } = await import(process.argv[1]);
const callback = await startOAuthCallbackServer({ providerName: "Pi probe", host: "127.0.0.1", port: 0, path: "/auth/callback", state: "native-probe-state", timeoutMs: 9000,
 complete: async code => { if (code !== "native-probe-code") throw new Error("wrong probe code"); return code; } });
console.log(callback.redirectUri);
try { await callback.wait(); } finally { callback.close(); }
`
	cmd := exec.CommandContext(ctx, "node", "--input-type=module", "-e", script, module)
	cmd.Stderr = io.Discard
	output, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { cancel(); _ = cmd.Wait() }()
	line, err := bufio.NewReader(io.LimitReader(output, 1024)).ReadString('\n')
	if err != nil {
		t.Fatal("native callback server did not start")
	}
	endpoint, err := url.Parse(strings.TrimSpace(line))
	if err != nil || endpoint.Hostname() != "127.0.0.1" || endpoint.Path != "/auth/callback" {
		t.Fatal("unexpected native callback endpoint")
	}
	cb := callback{flow: PiOpenAI, authority: endpoint.Host, path: endpoint.Path, state: sha256.Sum256([]byte("native-probe-state"))}
	query := url.Values{"code": {"native-probe-code"}, "state": {"native-probe-state" + lifeSciencesStateSuffix}}
	target, err := cb.requestTarget("http://" + endpoint.Host + endpoint.Path + "?" + query.Encode())
	if err != nil {
		t.Fatal("matching suffixed callback refused")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+endpoint.Host+target, nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal("native callback delivery failed")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("native Pi callback rejected normalized state: %d", response.StatusCode)
	}
}

func TestNativeCodexBrowserRegistration(t *testing.T) {
	executable := os.Getenv("NOISEDECK_TEST_CODEX_EXECUTABLE")
	if executable == "" {
		t.Skip("set NOISEDECK_TEST_CODEX_EXECUTABLE for pinned native launch qualification")
	}
	version, err := exec.Command(executable, "--version").Output()
	if err != nil || strings.TrimSpace(string(version)) != shippedVersions["codex"] {
		t.Fatal("native Codex version does not match shipped release")
	}
	nativeBrowserRegistration(t, executable, "codex", "openai-codex")
}

func nativeBrowserRegistration(t *testing.T, executable, harness, provider string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "agent"), 0700); err != nil {
		t.Fatal(err)
	}
	var nativePID atomic.Int64
	var nativeStart atomic.Uint64
	if harness == "codex" {
		nativeExecutable := ShippedCodexExecutable(context.Background(), executable)
		if nativeExecutable == "" {
			t.Fatal("packaged Codex executable was not admitted")
		}
		reserveCodexLoginPorts(t, &nativePID, nativeExecutable)
	}
	runtime := &LinuxRuntime{Resolve: func(ctx context.Context, pid int, start uint64) (SessionIdentity, bool) {
		return SessionIdentity{ID: "native-probe", Generation: 1, Provider: harness, CodexExecutable: ShippedCodexExecutable(ctx, executable)}, pid == int(nativePID.Load()) && start == nativeStart.Load() && nativePID.Load() != 0
	}}
	service := New(runtime)
	socket := filepath.Join(dir, "register.sock")
	local, err := StartLocal(socket, service)
	if err != nil {
		t.Fatal(err)
	}
	defer local.Close()
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	// This file contains only the test executable's path and fixed flags.
	// No URL, state, callback or native output is written to a file.
	wrapper := "#!/bin/sh\nexec '" + strings.ReplaceAll(binary, "'", "'\\''") + "' -test.run=^TestNativeBrowserOpener$ -- \"$@\"\n"
	if err := os.WriteFile(filepath.Join(dir, "xdg-open"), []byte(wrapper), 0700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "python3", "-c", nativePTY, executable, provider, harness)
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = 4 * time.Second
	cmd.Dir = dir
	cmd.Env = []string{
		"PATH=" + dir + string(os.PathListSeparator) + os.Getenv("PATH"), "TERM=xterm-256color", "LANG=C.UTF-8",
		"PI_CODING_AGENT_DIR=" + filepath.Join(dir, "agent"),
		"CODEX_HOME=" + filepath.Join(dir, "agent"), "RUST_LOG=off", "BROWSER=" + filepath.Join(dir, "xdg-open"),
		"NOISEDECK_LOGIN_SOCKET=" + socket, "NOISEDECK_LOGIN_PROVIDER=" + harness, "DECK_NATIVE_BROWSER_OPENER_TEST=1",
	}
	cmd.Stderr = io.Discard
	input, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	output, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { input.Close(); _ = cmd.Wait() }()
	line, err := bufio.NewReader(io.LimitReader(output, 64)).ReadString('\n')
	if err != nil {
		t.Fatal("native process did not start")
	}
	pid, err := strconv.Atoi(strings.TrimSpace(line))
	if err != nil {
		t.Fatal("native process identity unavailable")
	}
	process, err := readProcess(pid)
	if err != nil {
		t.Fatal("native process exited before identity capture")
	}
	nativeStart.Store(process.start)
	nativePID.Store(int64(pid))
	var pending []Pending
	for ctx.Err() == nil {
		pending = service.Pending(ctx, "user-a", "native-probe")
		if len(pending) == 1 {
			break
		}
		if _, err := readProcess(pid); err != nil {
			t.Fatal("native CLI exited before browser registration")
		}
		time.Sleep(50 * time.Millisecond)
	}
	if len(pending) != 1 {
		t.Fatal("native browser launch did not register; terminal output was not inspected")
	}
	r := service.registrations[pending[0].ID]
	if r.binding.PID != pid {
		owner, err := readProcess(r.binding.PID)
		if harness != "codex" || err != nil || owner.parent != pid || !processExecutable(owner.pid, ShippedCodexExecutable(ctx, executable)) {
			t.Fatal("listener is not owned by the native CLI")
		}
	}
	conn, err := runtime.Dial(ctx, r.binding, r.callback.port())
	if err != nil {
		t.Fatal("native accepted socket ownership could not be verified")
	}
	status, _, err := exchange(ctx, conn, r.callback.authority, r.callback.path+"?code=invalid-probe-code&state=deliberately-wrong-state")
	if err != nil || status != 400 {
		t.Fatalf("native state rejection: status=%d", status)
	}
	if !runtime.Alive(ctx, r.binding) {
		t.Fatal("invalid state ended native pending attempt")
	}
}

func TestNativeBrowserOpener(t *testing.T) {
	if os.Getenv("DECK_NATIVE_BROWSER_OPENER_TEST") == "1" {
		os.Exit(BrowserOpener(os.Args[len(os.Args)-1:]))
	}
}

// Codex's first bind failure sends /cancel to the occupying listener. Reserve
// both native ports before launch, skipping if either is already occupied.
// Release the default port only after this test's native process has made its
// one cancellation request. The pinned CLI never sends another cancellation
// during retry/fallback, so a racing listener cannot receive one from the probe.
func reserveCodexLoginPorts(t *testing.T, nativePID *atomic.Int64, executable string) {
	t.Helper()
	var ports []*net.TCPListener
	for _, port := range []int{1455, 1457} {
		listener, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: port})
		if err != nil {
			t.Skip("native Codex probe ports are occupied; no existing listener was contacted")
		}
		ports = append(ports, listener)
		t.Cleanup(func() { listener.Close() })
	}
	done := make(chan struct{})
	t.Cleanup(func() { ports[0].Close(); <-done })
	go func() {
		defer close(done)
		for {
			conn, err := ports[0].Accept()
			if err != nil {
				return
			}
			_ = conn.SetDeadline(time.Now().Add(time.Second))
			owned := false
			for range 25 {
				pid := int(nativePID.Load())
				sockets, _ := tcpSockets()
				for _, socket := range sockets {
					if socket.local == tcpAddress(conn.RemoteAddr()) && socket.remote == tcpAddress(conn.LocalAddr()) && nativeProbeOwnsSocket(pid, executable, socket.inode) {
						owned = true
						break
					}
				}
				if owned {
					break
				}
				time.Sleep(10 * time.Millisecond)
			}
			if !owned {
				conn.Close()
				continue
			}
			req, err := http.ReadRequest(bufio.NewReader(io.LimitReader(conn, 1024)))
			if err == nil {
				req.Body.Close()
			}
			if err != nil || req.Method != "GET" || req.URL.RequestURI() != "/cancel" {
				conn.Close()
				continue
			}
			_, _ = io.WriteString(conn, "HTTP/1.1 200 OK\r\nContent-Length: 0\r\nConnection: close\r\n\r\n")
			conn.Close()
			ports[0].Close()
			return
		}
	}()
}

func nativeProbeOwnsSocket(root int, executable string, inode uint64) bool {
	if processExecutable(root, executable) && ownsSocket(root, inode) {
		return true
	}
	children, err := os.ReadFile("/proc/" + strconv.Itoa(root) + "/task/" + strconv.Itoa(root) + "/children")
	if err != nil {
		return false
	}
	for _, child := range strings.Fields(string(children)) {
		pid, err := strconv.Atoi(child)
		if err != nil {
			continue
		}
		p, err := readProcess(pid)
		if err == nil && p.parent == root && processExecutable(pid, executable) && ownsSocket(pid, inode) {
			return true
		}
	}
	return false
}

const nativePTY = `
import os,pty,subprocess,sys,threading,time,signal,fcntl,termios,struct
master,slave=pty.openpty()
fcntl.ioctl(slave,termios.TIOCSWINSZ,struct.pack('HHHH',40,140,0,0))
args=[sys.argv[1],'login'] if sys.argv[3]=='codex' else [sys.argv[1],'--no-session','--no-extensions','--no-skills','--no-prompt-templates','--no-themes']
proc=subprocess.Popen(args,stdin=slave,stdout=slave,stderr=slave,start_new_session=True)
os.close(slave)
print(proc.pid,flush=True)
def drain():
    try:
        while os.read(master,65536): pass
    except OSError: pass
threading.Thread(target=drain,daemon=True).start()
stopped=False
def stop(*args):
    global stopped
    if stopped: return
    stopped=True
    if proc.poll() is not None: return
    try: os.killpg(proc.pid,signal.SIGTERM)
    except ProcessLookupError: pass
    try: proc.wait(timeout=3)
    except subprocess.TimeoutExpired:
        os.killpg(proc.pid,signal.SIGKILL); proc.wait()
def terminate(*args):
    stop(); sys.exit(0)
signal.signal(signal.SIGTERM,terminate)
try:
    if sys.argv[3]=='pi':
        time.sleep(2)
        os.write(master,('/login '+sys.argv[2]+'\r').encode())
        time.sleep(1)
        os.write(master,b'\r')
        if sys.argv[2]=='anthropic':
            # Choose browser login in Pi's native login-method selector.
            time.sleep(1)
            os.write(master,b'\r')
    sys.stdin.buffer.read()
finally:
    stop(); os.close(master)
`
