package terminal

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/rs/zerolog"

	"github.com/noisefloor-oss/deck/internal/db"
	"github.com/noisefloor-oss/deck/internal/inputgate"
	"github.com/noisefloor-oss/deck/internal/lease"
	"github.com/noisefloor-oss/deck/internal/session"
	"github.com/noisefloor-oss/deck/internal/tmux"
)

// Everything real: a real tmux server on a private socket, real tmux
// clients, a real SQLite control database, and a real WebSocket over a real
// HTTP server. The "provider" is a shell — the CLI itself is the acceptance
// suite's business (PRD §3.7); what is under test here is the transport,
// the fencing, and the lifetime rules.

const testOrigin = "http://console.test"

type env struct {
	proxy   *Proxy
	srv     *httptest.Server
	db      *db.DB
	tmux    *tmux.Client
	repo    *session.Repository
	leases  *lease.Manager
	session *session.Session
}

func newEnv(t *testing.T) *env {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}

	d, err := db.Open(context.Background(), db.Config{
		Path: filepath.Join(t.TempDir(), "control.db"),
	})
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })

	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	tc, err := tmux.NewClient(tmux.Config{
		SocketPath: filepath.Join(dir, "ac.sock"), Timeout: 10 * time.Second,
	})
	if err != nil {
		t.Fatalf("tmux.NewClient: %v", err)
	}
	t.Cleanup(func() { _ = tc.KillServer(context.Background()) })

	ctx := context.Background()
	name := "acterm-test"
	info, err := tc.CreateSession(ctx, tmux.SessionSpec{
		Name: name, WorkDir: t.TempDir(),
		// A shell stands in for the provider: it echoes, redraws, and
		// resizes like any TUI-less program, which is all the transport
		// needs to be exercised.
		Argv:       []string{"/bin/sh"},
		Generation: 1, Marker: "test-marker",
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	repo := session.NewRepository(d)
	bootEpoch, err := tmux.BootID()
	if err != nil {
		t.Skipf("read boot epoch: %v", err)
	}
	s := &session.Session{
		ID: "sess-1", ProjectID: "prj", Provider: "claude",
		TmuxSocketPath: tc.SocketPath(), TmuxSessionName: name, Generation: 1,
		BootEpoch: &bootEpoch,
	}
	seedProjectAndSession(t, d, repo, s, info)

	leases := lease.NewManager(d)
	p := New(tc, repo, leases, inputgate.New(), []string{testOrigin}, zerolog.Nop())

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/sessions/{id}/terminal", p.Handle)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	return &env{proxy: p, srv: srv, db: d, tmux: tc, repo: repo, leases: leases,
		session: s}
}

func seedProjectAndSession(t *testing.T, d *db.DB, repo *session.Repository,
	s *session.Session, info *tmux.PaneInfo) {
	t.Helper()
	ctx := context.Background()
	err := d.WithTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		now := db.FormatTime(time.Now())
		_, err := tx.ExecContext(ctx, `
			INSERT INTO projects (id, name, canonical_path, created_at, last_used_at)
			VALUES ('prj', 'test', '/tmp/prj', ?, ?)`, now, now)
		return err
	})
	if err != nil {
		t.Fatalf("seed project: %v", err)
	}
	marker, exe := "test-marker", info.Executable
	s.IdentityMarker = &marker
	if err := repo.Create(ctx, s); err != nil {
		t.Fatalf("create session: %v", err)
	}
	if exe == "" {
		exe = "/bin/sh"
	}
	s.TmuxSessionID, s.TmuxPaneID = &info.SessionID, &info.PaneID
	s.PanePID, s.ProcessGroupID, s.StartTicks = &info.PID, &info.ProcessGroupID, &info.StartTicks
	s.ExpectedExecutable = &exe
	if err := repo.MarkRunning(ctx, s); err != nil {
		t.Fatalf("mark running: %v", err)
	}
}

// dial opens an attachment as controllerID, returning the connection (nil
// when refused) and the handshake's HTTP status. Returning a status rather
// than the response keeps the body's lifetime entirely inside this helper.
func (e *env) dial(t *testing.T, controllerID string, takeover bool) (*websocket.Conn, int) {
	return e.dialAt(t, controllerID, takeover, "80", "24")
}

func (e *env) dialAt(t *testing.T, controllerID string, takeover bool,
	cols, rows string) (*websocket.Conn, int) {
	t.Helper()
	u, _ := url.Parse(e.srv.URL)
	u.Scheme = "ws"
	u.Path = "/api/sessions/" + e.session.ID + "/terminal"
	q := u.Query()
	q.Set("controller", controllerID)
	q.Set("cols", cols)
	q.Set("rows", rows)
	if takeover {
		q.Set("takeover", "1")
	}
	u.RawQuery = q.Encode()

	conn, resp, err := websocket.DefaultDialer.Dial(u.String(),
		http.Header{"Origin": []string{testOrigin}})
	if err != nil && resp == nil {
		t.Fatalf("dial %s: %v", controllerID, err)
	}
	status := 0
	if resp != nil {
		status = resp.StatusCode
		_ = resp.Body.Close()
	}
	return conn, status
}

func TestInitialViewportGeometryReachesPhysicalTmuxAttach(t *testing.T) {
	e := newEnv(t)
	attachCalls := make(chan tmux.AttachObservation, 1)
	e.tmux.SetAttachObserverForTest(func(observation tmux.AttachObservation) {
		attachCalls <- observation
	})
	t.Cleanup(func() { e.tmux.SetAttachObserverForTest(nil) })

	c, status := e.dialAt(t, "browser-a", false, "151", "49")
	if status != http.StatusSwitchingProtocols {
		t.Fatalf("attach status = %d, want 101", status)
	}
	defer c.Close()
	awaitAttached(t, c)
	select {
	case got := <-attachCalls:
		want := (tmux.AttachObservation{SessionName: e.session.TmuxSessionName, Cols: 151, Rows: 49})
		if got != want {
			t.Fatalf("physical attach = %+v, want %+v", got, want)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("physical tmux attach was not observed")
	}
}

func TestInvalidInitialViewportGeometryHasNoLeaseOrTmuxEffect(t *testing.T) {
	for name, size := range map[string][2]string{
		"missing columns": {"", "24"},
		"zero rows":       {"80", "0"},
		"negative":        {"-1", "24"},
		"overflow":        {"65536", "24"},
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			attachCalls := make(chan tmux.AttachObservation, 1)
			e.tmux.SetAttachObserverForTest(func(observation tmux.AttachObservation) {
				attachCalls <- observation
			})
			c, status := e.dialAt(t, "browser-a", false, size[0], size[1])
			if c != nil {
				c.Close()
			}
			if status != http.StatusBadRequest {
				t.Fatalf("invalid geometry status = %d, want 400", status)
			}
			select {
			case got := <-attachCalls:
				t.Fatalf("invalid geometry invoked tmux Attach: %+v", got)
			default:
			}
			var leases int
			if err := e.db.Reader().QueryRowContext(context.Background(),
				`SELECT COUNT(*) FROM controller_leases WHERE session_id = ?`, e.session.ID).
				Scan(&leases); err != nil {
				t.Fatalf("count leases: %v", err)
			}
			if leases != 0 {
				t.Fatalf("invalid geometry created %d controller lease(s)", leases)
			}
		})
	}
}

// readFrame reads one server frame, failing on timeout.
func readFrame(t *testing.T, c *websocket.Conn) ServerFrame {
	t.Helper()
	_ = c.SetReadDeadline(time.Now().Add(10 * time.Second))
	_, msg, err := c.ReadMessage()
	if err != nil {
		t.Fatalf("read frame: %v", err)
	}
	var f ServerFrame
	if err := json.Unmarshal(msg, &f); err != nil {
		t.Fatalf("decode frame: %v", err)
	}
	return f
}

// awaitAttached consumes frames until the attach acknowledgement.
func awaitAttached(t *testing.T, c *websocket.Conn) int64 {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		f := readFrame(t, c)
		if f.Type == TypeAttached {
			return f.Token
		}
		if f.Type == TypeError {
			t.Fatalf("error before attach: %s %s", f.Code, f.Message)
		}
	}
	t.Fatal("never attached")
	return 0
}

func send(t *testing.T, c *websocket.Conn, f ClientFrame) {
	t.Helper()
	b, err := json.Marshal(f)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	_ = c.SetWriteDeadline(time.Now().Add(5 * time.Second))
	if err := c.WriteMessage(websocket.TextMessage, b); err != nil {
		t.Fatalf("write: %v", err)
	}
}

// inputFrame builds a raw input frame stamped with the env session's
// current generation — the frame shape every input/reply write must carry.
func (e *env) inputFrame(token int64, data string) ClientFrame {
	return ClientFrame{Type: TypeInput, Token: token,
		Generation: e.session.Generation, Data: data}
}

// awaitStatus reads frames until a write-status frame with the given outcome
// arrives, failing on any protocol error first.
func awaitStatus(t *testing.T, c *websocket.Conn, want string) ServerFrame {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		f := readFrame(t, c)
		if f.Type == TypeWriteStatus {
			if f.Code == want {
				return f
			}
			continue
		}
		if f.Type == TypeError {
			t.Fatalf("error before %s status: %s %s", want, f.Code, f.Message)
		}
	}
	t.Fatalf("never saw %s write status", want)
	return ServerFrame{}
}

// awaitScrollStatus reads until tmux's post-command pane-mode
// acknowledgment arrives.
func awaitScrollStatus(t *testing.T, c *websocket.Conn, want bool) ServerFrame {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		f := readFrame(t, c)
		if f.Type == TypeScrollStatus {
			if f.Active == nil {
				t.Fatal("scroll status omitted pane state")
			}
			if *f.Active == want {
				return f
			}
			continue
		}
		if f.Type == TypeError {
			t.Fatalf("error before scroll status: %s %s", f.Code, f.Message)
		}
	}
	t.Fatalf("never saw active=%t scroll status", want)
	return ServerFrame{}
}

// awaitOutput reads until output containing want appears.
func awaitOutput(t *testing.T, c *websocket.Conn, want string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	var seen strings.Builder
	for time.Now().Before(deadline) {
		f := readFrame(t, c)
		if f.Type != TypeOutput {
			continue
		}
		raw, err := base64.StdEncoding.DecodeString(f.Data)
		if err != nil {
			t.Fatalf("decode output: %v", err)
		}
		seen.Write(raw)
		if strings.Contains(seen.String(), want) {
			return
		}
	}
	t.Fatalf("never saw %q; got:\n%s", want, seen.String())
}

func awaitAttachmentCount(t *testing.T, p *Proxy, sessionID string, want int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		p.mu.Lock()
		got := len(p.live[sessionID])
		p.mu.Unlock()
		if got == want {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	p.mu.Lock()
	got := len(p.live[sessionID])
	p.mu.Unlock()
	t.Fatalf("live attachment count = %d, want %d", got, want)
}

// awaitSocketClosed consumes frames the single writer had already queued
// before teardown. The production contract deliberately drains that queue so
// the final reason frame is not lost; a buffered output frame therefore does
// not mean the attachment stayed open. A deadline timeout does.
func awaitSocketClosed(t *testing.T, c *websocket.Conn) {
	t.Helper()
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	for {
		if _, _, err := c.ReadMessage(); err != nil {
			var netErr net.Error
			if errors.As(err, &netErr) && netErr.Timeout() {
				t.Fatal("socket stayed open until the close deadline")
			}
			return
		}
	}
}

// The round trip: input reaches the real terminal and its output comes
// back — the product's entire reason to exist.
func TestInputReachesTerminalAndOutputReturns(t *testing.T) {
	e := newEnv(t)
	c, _ := e.dial(t, "browser-a", false)
	defer c.Close()
	token := awaitAttached(t, c)

	send(t, c, e.inputFrame(token, base64.StdEncoding.EncodeToString([]byte("echo hello-terminal\n"))))
	awaitOutput(t, c, "hello-terminal")
}

// A write acknowledged by tmux is reported as sent — bytes handed to tmux
// for the live pane, never a delivery claim.
func TestSentOutcomeAfterSuccessfulWrite(t *testing.T) {
	e := newEnv(t)
	c, _ := e.dial(t, "browser-a", false)
	token := awaitAttached(t, c)

	send(t, c, e.inputFrame(token, base64.StdEncoding.EncodeToString([]byte("echo sent-ok\n"))))
	if f := awaitStatus(t, c, WriteSent); f.Code != WriteSent {
		t.Fatalf("status frame = %+v, want sent", f)
	}
	snapshot := e.proxy.ConnectionSnapshot(e.session.ID)
	if snapshot.AttachmentCount != 1 || snapshot.FencingToken != token ||
		snapshot.LastWriteResult != WriteSent {
		t.Fatalf("live connection snapshot = %+v", snapshot)
	}
	_ = c.Close()
	awaitAttachmentCount(t, e.proxy, e.session.ID, 0)
	if snapshot := e.proxy.ConnectionSnapshot(e.session.ID); snapshot != (ConnectionSnapshot{}) {
		t.Fatalf("disconnected attachment retained state: %+v", snapshot)
	}
}

// The test-only write fault makes the single serialized writer fail AFTER
// genuinely writing a prefix to the PTY — an outcome that cannot prove how
// many bytes crossed the tmux boundary. The browser is told the input may
// not have reached the CLI (uncertain, never "sent"), and the attachment
// ends instead of pretending the write succeeded or retrying it.
func TestWriteFaultForcesUncertainOutcome(t *testing.T) {
	e := newEnv(t)
	c, _ := e.dial(t, "browser-a", false)
	defer c.Close()
	token := awaitAttached(t, c)

	e.proxy.SetWriteFaultForTest(func(att *tmux.Attachment, data []byte) (int, error) {
		// Genuinely deliver a prefix through the real bounded writer, then
		// fail: some bytes crossed the boundary, so the write is unverifiable
		// — the fault hook must NOT fabricate (0, error), which would prove
		// zero bytes crossed.
		n, werr := att.WriteWithin(data[:len(data)/2], 5*time.Second)
		if werr != nil {
			return n, werr
		}
		return n, errors.New("injected: lost acknowledgement after a partial write")
	})
	send(t, c, e.inputFrame(token, base64.StdEncoding.EncodeToString([]byte("echo should-not-run\n"))))

	f := awaitStatus(t, c, WriteUncertain)
	if strings.Contains(f.Message, "delivered") || strings.Contains(f.Message, "sent") {
		t.Fatalf("uncertain frame overclaims delivery: %q", f.Message)
	}
	// The writer dropped the attachment: the socket closes, so the shell is
	// never fed the rest of the input through a dead authorizer.
	awaitSocketClosed(t, c)
}

// A frame carrying a stale session generation is a provable pre-write
// rejection — failed, before any byte crosses the tmux boundary.
func TestPreflightRejectsStaleGeneration(t *testing.T) {
	e := newEnv(t)
	c, _ := e.dial(t, "browser-a", false)
	defer c.Close()
	token := awaitAttached(t, c)

	send(t, c, ClientFrame{Type: TypeInput, Token: token,
		Generation: e.session.Generation + 1,
		Data:       base64.StdEncoding.EncodeToString([]byte("echo gen-stale\n"))})
	f := awaitStatus(t, c, WriteFailed)
	if !strings.Contains(f.Message, "restarted") {
		t.Fatalf("failed message = %q, want a generation hint", f.Message)
	}
	if pane, _ := e.tmux.CapturePane(context.Background(), *e.session.TmuxPaneID); strings.Contains(pane, "gen-stale") {
		t.Fatal("stale-generation write reached the terminal")
	}
}

// Once attach has reconciled the live tmux client, input does not reload the
// recorded identity on every frame. Lifecycle changes use the same process-
// local session gate and invalidate the lease; only an out-of-band second
// writer can create this row drift, and it must not add latency to typing.
func TestAttachedInputDoesNotReloadRecordedIdentity(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	c, _ := e.dial(t, "browser-a", false)
	defer c.Close()
	token := awaitAttached(t, c)

	if err := e.db.WithTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE sessions SET
			tmux_socket_path = '/tmp/out-of-band.sock', tmux_pane_id = '%9999',
			pane_pid = 999999, process_group_id = 999999,
			process_start_ticks = 999999 WHERE id = 'sess-1'`)
		return err
	}); err != nil {
		t.Fatalf("rewrite recorded identity: %v", err)
	}

	send(t, c, e.inputFrame(token,
		base64.StdEncoding.EncodeToString([]byte("echo attached-client-stays-live\n"))))
	awaitStatus(t, c, WriteSent)
	awaitOutput(t, c, "attached-client-stays-live")
}

// A provider exiting leaves its remain-on-exit pane and browser attachment
// in place, but there is no live CLI to receive input. The pane-state read
// refuses the write as failed instead of reporting sent to a dead pane.
func TestPreflightRejectsDeadPane(t *testing.T) {
	e := newEnv(t)
	c, _ := e.dial(t, "browser-a", false)
	defer c.Close()
	token := awaitAttached(t, c)

	if e.session.PanePID == nil {
		t.Fatal("test session has no recorded pane PID")
	}
	if err := syscall.Kill(*e.session.PanePID, syscall.SIGKILL); err != nil {
		t.Fatalf("kill pane process: %v", err)
	}

	send(t, c, e.inputFrame(token, base64.StdEncoding.EncodeToString([]byte("echo dead-target\n"))))
	f := awaitStatus(t, c, WriteFailed)
	if !strings.Contains(f.Message, "identity") {
		t.Fatalf("failed message = %q, want an identity hint", f.Message)
	}
}

// When the tmux connection is lost (the session is killed), the browser is
// told the input may have been interrupted — the only honest report without
// a durable ledger — and the attachment ends with no replay.
func TestInterruptedAfterSessionLoss(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	c, _ := e.dial(t, "browser-a", false)
	defer c.Close()
	token := awaitAttached(t, c)

	// Prove the attachment is live first.
	send(t, c, e.inputFrame(token, base64.StdEncoding.EncodeToString([]byte("echo before-loss\n"))))
	awaitOutput(t, c, "before-loss")

	if err := e.tmux.KillSession(ctx, e.session.TmuxSessionName); err != nil {
		t.Fatalf("kill session: %v", err)
	}
	if f := awaitStatus(t, c, WriteInterrupted); f.Code != WriteInterrupted {
		t.Fatalf("status frame = %+v, want interrupted", f)
	}
	// The attachment ends: the socket closes after the report.
	awaitSocketClosed(t, c)
}

// The fault seam is one-shot: arming it affects exactly one write, so a
// fault installed for one frame cannot poison later input in a long-lived
// acceptance run.
func TestWriteFaultIsOneShot(t *testing.T) {
	e := newEnv(t)
	e.proxy.SetWriteFaultForTest(func(att *tmux.Attachment, data []byte) (int, error) {
		return 0, errors.New("boom")
	})
	n, err := e.proxy.writeBounded(nil, []byte("x"))
	if err == nil || n != 0 {
		t.Fatalf("armed fault: n=%d err=%v, want n=0 err", n, err)
	}
	e.proxy.writeFaultMu.Lock()
	armed := e.proxy.writeFault != nil
	e.proxy.writeFaultMu.Unlock()
	if armed {
		t.Fatal("fault was not consumed by the first write")
	}
}

// A second browser without a deliberate takeover is told IN-BAND — an error
// frame, then a close — because the browser WebSocket API cannot expose a
// handshake failure's status to the takeover screen.
func TestSecondBrowserRefusedWithoutTakeover(t *testing.T) {
	e := newEnv(t)
	c, _ := e.dial(t, "browser-a", false)
	defer c.Close()
	awaitAttached(t, c)

	c2, status := e.dial(t, "browser-b", false)
	if status != http.StatusSwitchingProtocols {
		t.Fatalf("second browser: status = %d, want the in-band refusal (101)", status)
	}
	defer c2.Close()

	// The first frame says why, and then the socket closes.
	_ = c2.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, raw, err := c2.ReadMessage()
	if err != nil {
		t.Fatalf("read refusal frame: %v", err)
	}
	var f ServerFrame
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatalf("decode refusal frame: %v", err)
	}
	if f.Type != TypeError || f.Code != CodeLeaseHeld {
		t.Fatalf("refusal frame = %+v, want error/%s", f, CodeLeaseHeld)
	}
	awaitSocketClosed(t, c2)
}

// A deliberate takeover fences the first browser — its input is refused and
// its attachment closed, even though its socket was healthy.
func TestTakeoverFencesFirstBrowser(t *testing.T) {
	e := newEnv(t)
	c1, _ := e.dial(t, "browser-a", false)
	defer c1.Close()
	token1 := awaitAttached(t, c1)

	c2, _ := e.dial(t, "browser-b", true)
	defer c2.Close()
	token2 := awaitAttached(t, c2)
	if token2 <= token1 {
		t.Fatalf("takeover token = %d, want > %d", token2, token1)
	}

	// The fenced browser tries to type: it must be told and closed.
	send(t, c1, e.inputFrame(token1, base64.StdEncoding.EncodeToString([]byte("echo should-not-run\n"))))

	sawTold := false
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		_ = c1.SetReadDeadline(time.Now().Add(2 * time.Second))
		_, msg, err := c1.ReadMessage()
		if err != nil {
			break // closed, which is the required outcome
		}
		var f ServerFrame
		if json.Unmarshal(msg, &f) == nil {
			// The displaced controller is told it lost control (fenced), and
			// its write is reported as a provable pre-write rejection
			// (status/failed); either satisfies the requirement.
			if f.Type == TypeError && f.Code == CodeFenced {
				sawTold = true
			}
			if f.Type == TypeWriteStatus && f.Code == WriteFailed {
				sawTold = true
			}
		}
	}
	if !sawTold {
		t.Fatal("fenced controller was never told it lost control")
	}

	// The new controller still works.
	send(t, c2, e.inputFrame(token2, base64.StdEncoding.EncodeToString([]byte("echo new-controller\n"))))
	awaitOutput(t, c2, "new-controller")
}

// An idle browser is no exception: a takeover must close the displaced
// attachment promptly even if it never sends another frame — otherwise two
// tmux clients stay attached to one session for as long as the old browser
// stays quiet.
func TestIdleAttachmentClosedOnTakeover(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()

	c1, _ := e.dial(t, "browser-a", false)
	defer c1.Close()
	awaitAttached(t, c1)

	clients := func() int {
		out, err := exec.CommandContext(ctx, "tmux", "-S", e.tmux.SocketPath(),
			"list-clients", "-t", e.session.TmuxSessionName).Output()
		if err != nil {
			return -1
		}
		s := strings.TrimSpace(string(out))
		if s == "" {
			return 0
		}
		return len(strings.Split(s, "\n"))
	}
	deadline := time.Now().Add(10 * time.Second)
	for clients() < 1 && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
	}

	// browser-a now goes silent — no reads, no writes — and browser-b takes
	// over.
	c2, _ := e.dial(t, "browser-b", true)
	defer c2.Close()
	awaitAttached(t, c2)

	deadline = time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if clients() == 1 {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("displaced idle attachment was not closed: %d tmux clients remain", clients())
}

// Expiry revokes input but does not by itself create a new browser owner. An
// idle attachment can keep showing output until it next speaks or another
// browser acquires the lease; no database polling loop tears it down merely
// because its heartbeat stopped.
func TestExpiredIdleAttachmentWaitsForInputOrAcquisition(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	c, _ := e.dial(t, "browser-a", false)
	defer c.Close()
	awaitAttached(t, c)

	if err := e.db.WithTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`UPDATE controller_leases SET expires_at = ? WHERE session_id = ?`,
			db.FormatTime(time.Now().Add(-time.Second)), e.session.ID)
		return err
	}); err != nil {
		t.Fatalf("expire lease: %v", err)
	}

	// The retired watcher polled every two seconds and would close this tmux
	// client. Waiting beyond that interval proves expiry alone is no longer a
	// teardown signal.
	time.Sleep(2500 * time.Millisecond)
	out, err := exec.CommandContext(ctx, "tmux", "-S", e.tmux.SocketPath(),
		"list-clients", "-t", e.session.TmuxSessionName).Output()
	if err != nil {
		t.Fatalf("list clients: %v", err)
	}
	clients := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(clients) != 1 {
		t.Fatalf("tmux clients after idle expiry = %d, want 1", len(clients))
	}
}

// An expired reclaim does not increment the fencing token, so token-only
// attachment matching would leave the old browser connected. Controller id
// and token together identify which in-process attachment the acquisition
// keeps; no database polling loop is needed.
func TestFenceOthersUsesControllerAndToken(t *testing.T) {
	_, oldCancel := context.WithCancel(context.Background())
	defer oldCancel()
	keepCtx, keepCancel := context.WithCancel(context.Background())
	defer keepCancel()
	oldOut := make(chan ServerFrame, 1)
	keepOut := make(chan ServerFrame, 1)
	old := &attachment{sessionID: "s", controller: "browser-a", token: 7, cancel: oldCancel, out: oldOut}
	keep := &attachment{sessionID: "s", controller: "browser-b", token: 7, cancel: keepCancel, out: keepOut}
	p := &Proxy{live: map[string][]*attachment{"s": {old, keep}}, log: zerolog.Nop()}

	p.fenceOthers("s", "browser-b", 7)
	if got := p.live["s"]; len(got) != 1 || got[0] != keep {
		t.Fatalf("kept attachments = %#v, want only the acquired controller", got)
	}
	select {
	case frame := <-oldOut:
		if frame.Type != TypeError || frame.Code != CodeFenced {
			t.Fatalf("displaced frame = %+v, want fenced", frame)
		}
	default:
		t.Fatal("same-token displaced controller was not fenced immediately")
	}
	select {
	case frame := <-keepOut:
		t.Fatalf("acquired controller was fenced: %+v", frame)
	default:
	}
	select {
	case <-keepCtx.Done():
		t.Fatal("acquired controller was canceled")
	default:
	}
}

// A stale token is refused even from the controller that currently holds
// the lease: fencing is a property of the token, not the name. The write is
// reported as a provable pre-write rejection, failed, never sent.
func TestStaleTokenRefused(t *testing.T) {
	e := newEnv(t)
	c, _ := e.dial(t, "browser-a", false)
	defer c.Close()
	token := awaitAttached(t, c)

	send(t, c, ClientFrame{Type: TypeInput, Token: token + 99,
		Generation: e.session.Generation,
		Data:       base64.StdEncoding.EncodeToString([]byte("echo nope\n"))})
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
		_, msg, err := c.ReadMessage()
		if err != nil {
			return // closed: acceptable
		}
		var f ServerFrame
		if json.Unmarshal(msg, &f) == nil && f.Type == TypeWriteStatus && f.Code == WriteFailed {
			return
		}
	}
	t.Fatal("a stale token was not refused")
}

// A disconnect destroys only that attachment — the tmux session and its
// process survive, and a new browser can attach to the same session.
func TestDisconnectLeavesSessionRunning(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()

	c1, _ := e.dial(t, "browser-a", false)
	token := awaitAttached(t, c1)
	send(t, c1, e.inputFrame(token, base64.StdEncoding.EncodeToString([]byte("echo first-attachment\n"))))
	awaitOutput(t, c1, "first-attachment")
	_ = c1.Close()
	awaitAttachmentCount(t, e.proxy, e.session.ID, 0)

	// The session's pane is untouched by the disconnect.
	info, err := e.tmux.PaneInfoForSession(ctx, e.session.TmuxSessionName)
	if err != nil {
		t.Fatalf("session gone after disconnect: %v", err)
	}
	if info.Dead {
		t.Fatal("pane died with the attachment")
	}

	// And a new browser attaches and drives the same live session.
	c2, _ := e.dial(t, "browser-b", true)
	defer c2.Close()
	token2 := awaitAttached(t, c2)
	send(t, c2, e.inputFrame(token2, base64.StdEncoding.EncodeToString([]byte("echo second-attachment\n"))))
	awaitOutput(t, c2, "second-attachment")
}

// A reconnect keeps the same controller and token; teardown of a stale socket
// must not end the attachment a live socket continues to hold.
func TestSameControllerReconnectKeepsAttachment(t *testing.T) {
	e := newEnv(t)
	c1, _ := e.dial(t, "browser-a", false)
	defer c1.Close()
	token1 := awaitAttached(t, c1)
	c2, _ := e.dial(t, "browser-a", false)
	defer c2.Close()
	token2 := awaitAttached(t, c2)
	if token2 != token1 {
		t.Fatalf("same-controller reconnect token = %d, want %d", token2, token1)
	}

	// Closing the stale socket leaves the live attachment intact.
	_ = c1.Close()
	awaitAttachmentCount(t, e.proxy, e.session.ID, 1)
	send(t, c2, e.inputFrame(token2, base64.StdEncoding.EncodeToString([]byte("echo live-reconnect\n"))))
	awaitOutput(t, c2, "live-reconnect")
}

// Same-controller reconnects reuse the fencing token, but the current
// attachment's write evidence is still fresh. A late outcome from the
// overlapping old socket must not become the new socket's diagnostic
// result.
func TestSameControllerReconnectScopesWriteOutcomeToAttachment(t *testing.T) {
	e := newEnv(t)
	c1, _ := e.dial(t, "browser-a", false)
	defer c1.Close()
	token1 := awaitAttached(t, c1)
	c2, _ := e.dial(t, "browser-a", false)
	defer c2.Close()
	token2 := awaitAttached(t, c2)
	if token2 != token1 {
		t.Fatalf("same-controller reconnect token = %d, want %d", token2, token1)
	}

	send(t, c1, e.inputFrame(token1,
		base64.StdEncoding.EncodeToString([]byte("echo old-overlap\n"))))
	awaitStatus(t, c1, WriteSent)
	snapshot := e.proxy.ConnectionSnapshot(e.session.ID)
	if snapshot.AttachmentCount != 2 || snapshot.LastWriteResult != "" {
		t.Fatalf("old attachment outcome leaked into reconnect: %+v", snapshot)
	}

	send(t, c2, e.inputFrame(token2,
		base64.StdEncoding.EncodeToString([]byte("echo new-overlap\n"))))
	awaitStatus(t, c2, WriteSent)
	snapshot = e.proxy.ConnectionSnapshot(e.session.ID)
	if snapshot.LastWriteResult != WriteSent {
		t.Fatalf("current attachment outcome = %+v, want sent", snapshot)
	}
}

// A live connection that outlives its lease (heartbeat expiry) is still
// fenced: every input frame is revalidated against the current lease under
// the session write gate, so an expired lease is refused even with the
// originally issued token.
func TestExpiredControllerStillFenced(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	c, _ := e.dial(t, "browser-a", false)
	defer c.Close()
	token := awaitAttached(t, c)

	// Expire the lease out from under the live attachment. The controller
	// still believes it holds the session; revalidation must not.
	if err := e.db.WithTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`UPDATE controller_leases SET expires_at = ? WHERE session_id = ?`,
			db.FormatTime(time.Now().Add(-time.Second)), e.session.ID)
		return err
	}); err != nil {
		t.Fatalf("expire lease: %v", err)
	}

	send(t, c, e.inputFrame(token, base64.StdEncoding.EncodeToString([]byte("echo stale-under-expired-lease\n"))))
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
		_, msg, err := c.ReadMessage()
		if err != nil {
			return // closed: acceptable
		}
		var f ServerFrame
		if json.Unmarshal(msg, &f) == nil && f.Type == TypeWriteStatus && f.Code == WriteFailed {
			return
		}
	}
	t.Fatal("a token from an expired lease was not refused under fencing")
}

// The WebSocket handshake requires an allowlisted Origin. A foreign or
// absent Origin is refused — this is a WebSocket's only cross-site defense.
func TestOriginEnforcedOnHandshake(t *testing.T) {
	e := newEnv(t)
	u, _ := url.Parse(e.srv.URL)
	u.Scheme = "ws"
	u.Path = "/api/sessions/" + e.session.ID + "/terminal"
	u.RawQuery = "controller=browser-x"

	for name, hdr := range map[string]http.Header{
		"foreign origin": {"Origin": []string{"https://evil.example"}},
		"absent origin":  {},
	} {
		t.Run(name, func(t *testing.T) {
			conn, resp, err := websocket.DefaultDialer.Dial(u.String(), hdr)
			if conn != nil {
				conn.Close()
			}
			status := 0
			if resp != nil {
				status = resp.StatusCode
				_ = resp.Body.Close()
			}
			if err == nil {
				t.Fatal("handshake succeeded with a disallowed origin")
			}
			if status == http.StatusSwitchingProtocols {
				t.Fatal("upgrade completed with a disallowed origin")
			}
		})
	}
}

// Resize is accepted and the terminal really changes size: tmux
// renegotiates with the pane, which is why this product needs no terminal
// understanding of its own.
func TestResizeAppliesToPane(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	c, _ := e.dial(t, "browser-a", false)
	defer c.Close()
	token := awaitAttached(t, c)

	send(t, c, ClientFrame{Type: TypeResize, Token: token, Cols: 120, Rows: 40})

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if w, err := paneWidth(ctx, e); err == nil && w == "120" {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatal("pane never resized to the requested width")
}

// paneWidth asks tmux for the pane's own width. The target is the PANE ID:
// `=name` as a pane target silently resolves to nothing and reports every
// field empty (the trap documented in internal/tmux/target.go), which is
// exactly how an earlier version of this test "failed" against a resize
// that had in fact worked.
func paneWidth(ctx context.Context, e *env) (string, error) {
	out, err := exec.CommandContext(ctx, "tmux", "-S", e.tmux.SocketPath(),
		"display-message", "-p", "-t", *e.session.TmuxPaneID, "#{pane_width}").Output()
	return strings.TrimSpace(string(out)), err
}

// A session that is not running is refused before any attach is attempted.
func TestNotRunningSessionRefused(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	if applied, err := e.repo.MarkInterruptedObserved(ctx, e.session, "test non-running fixture"); err != nil || !applied {
		t.Fatalf("interrupt session: applied=%v err=%v", applied, err)
	}
	c, status := e.dial(t, "browser-a", false)
	if c != nil {
		c.Close()
	}
	if status != http.StatusConflict {
		t.Fatalf("not-running session: status = %d, want 409", status)
	}
}

func TestInitialIdentityMismatchRefusesBeforeTmuxAttach(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	before, err := e.tmux.PaneInfoByID(ctx, *e.session.TmuxPaneID)
	if err != nil {
		t.Fatalf("observe pane: %v", err)
	}
	attachCalls := make(chan tmux.AttachObservation, 1)
	e.tmux.SetAttachObserverForTest(func(observation tmux.AttachObservation) {
		attachCalls <- observation
	})
	t.Cleanup(func() { e.tmux.SetAttachObserverForTest(nil) })
	if err := e.db.WithTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`UPDATE sessions SET tmux_socket_path = '/tmp/foreign/ac.sock' WHERE id = 'sess-1'`)
		return err
	}); err != nil {
		t.Fatalf("rewrite recorded socket path: %v", err)
	}

	c, _ := e.dial(t, "browser-a", false)
	if c == nil {
		t.Fatal("identity refusal did not complete the websocket handshake")
	}
	defer c.Close()
	frame := readFrame(t, c)
	if frame.Type != TypeError || frame.Code != CodeAttachFailed {
		t.Fatalf("identity refusal frame = %+v, want attach_failed", frame)
	}
	select {
	case observation := <-attachCalls:
		t.Fatalf("identity-mismatched row invoked tmux Attach for %+v", observation)
	default:
	}
	if snapshot := e.proxy.ConnectionSnapshot(e.session.ID); snapshot.AttachmentCount != 0 {
		t.Fatalf("identity-mismatched row registered an attachment: %+v", snapshot)
	}
	after, err := e.tmux.PaneInfoByID(ctx, before.PaneID)
	if err != nil || after.Dead || after.PID != before.PID ||
		after.StartTicks != before.StartTicks {
		t.Fatalf("identity refusal affected pane: before=%+v after=%+v err=%v",
			before, after, err)
	}
}

// paneInMode polls tmux's own pane_in_mode flag until it reads want or the
// deadline passes. The scroll frame is handled asynchronously relative to
// the WebSocket send, so the observation must poll.
func awaitPaneInMode(t *testing.T, e *env, want string) {
	t.Helper()
	if e.session.TmuxPaneID == nil {
		t.Fatal("test session has no recorded pane id")
	}
	deadline := time.Now().Add(10 * time.Second)
	var got string
	for time.Now().Before(deadline) {
		out, err := exec.Command("tmux", "-S", e.tmux.SocketPath(),
			"display-message", "-p", "-t", *e.session.TmuxPaneID,
			"#{pane_in_mode}").Output()
		if err == nil {
			got = strings.TrimSpace(string(out))
			if got == want {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("pane_in_mode = %q, want %q", got, want)
}

// scrollFrame builds a scroll frame stamped with the env session's current
// generation — scroll crosses the same lease and generation fence as input.
func (e *env) scrollFrame(token int64, direction string, lines int) ClientFrame {
	return ClientFrame{Type: TypeScroll, Token: token,
		Generation: e.session.Generation, Direction: direction, Lines: lines}
}

// A scroll frame moves the pane's tmux copy-mode view — up enters
// copy-mode, and scrolling back to the bottom leaves it — without any byte
// crossing the input path.
func TestScrollFrameDrivesTmuxCopyMode(t *testing.T) {
	e := newEnv(t)
	c, _ := e.dial(t, "browser-a", false)
	defer c.Close()
	token := awaitAttached(t, c)

	send(t, c, e.scrollFrame(token, ScrollUp, 3))
	awaitScrollStatus(t, c, true)
	awaitPaneInMode(t, e, "1")

	send(t, c, e.scrollFrame(token, ScrollDown, tmux.MaxCopyModeScrollLines))
	awaitScrollStatus(t, c, false)
	awaitPaneInMode(t, e, "0")

	send(t, c, e.scrollFrame(token, ScrollUp, 3))
	awaitScrollStatus(t, c, true)
	awaitPaneInMode(t, e, "1")
	send(t, c, e.scrollFrame(token, ScrollLive, 0))
	awaitScrollStatus(t, c, false)
	awaitPaneInMode(t, e, "0")

	// The attachment survived both scrolls: ordinary input still works.
	send(t, c, e.inputFrame(token, base64.StdEncoding.EncodeToString([]byte("echo after-scroll\n"))))
	awaitStatus(t, c, WriteSent)
}

// Returning to live is the escape from copy-mode, so it has a distinct
// bounded allowance from ordinary movement. Saturating the movement bucket
// must still permit that transition while excess up/down frames remain
// droppable.
func TestScrollLiveSurvivesAnExhaustedMovementBucket(t *testing.T) {
	e := newEnv(t)
	c, _ := e.dial(t, "browser-a", false)
	defer c.Close()
	token := awaitAttached(t, c)

	send(t, c, e.scrollFrame(token, ScrollUp, 1))
	awaitScrollStatus(t, c, true)
	awaitPaneInMode(t, e, "1")

	const movementFrames = 100
	for i := 0; i < movementFrames; i++ {
		send(t, c, e.scrollFrame(token, ScrollUp, 1))
	}
	send(t, c, e.scrollFrame(token, ScrollLive, 0))

	acknowledgedMovements := 0
	for {
		f := readFrame(t, c)
		if f.Type == TypeError {
			t.Fatalf("error while awaiting live status: %s %s", f.Code, f.Message)
		}
		if f.Type != TypeScrollStatus || f.Active == nil {
			continue
		}
		if !*f.Active {
			break
		}
		acknowledgedMovements++
	}
	if acknowledgedMovements >= movementFrames {
		t.Fatalf("movement limiter acknowledged %d/%d burst frames; want excess frames dropped",
			acknowledgedMovements, movementFrames)
	}
	awaitPaneInMode(t, e, "0")

	// One browser gesture emits one live frame. After the browser's normal
	// coalescing interval permits the next up movement, its live reversal has
	// reserved capacity immediately and returns the pane live.
	time.Sleep(30 * time.Millisecond) // one normal movement token at 40/s
	send(t, c, e.scrollFrame(token, ScrollUp, 1))
	awaitScrollStatus(t, c, true)
	send(t, c, e.scrollFrame(token, ScrollLive, 0))
	awaitScrollStatus(t, c, false)
	awaitPaneInMode(t, e, "0")
}

// Reconnect publishes attached only after the gated identity preflight has
// exited a preserved tmux copy-mode view. A transient refusal exposes an
// attach failure and a later connection retries the invariant.
func TestAttachRetriesLiveViewAfterAPaneIdentityRefusal(t *testing.T) {
	e := newEnv(t)
	if e.session.TmuxPaneID == nil {
		t.Fatal("test session has no recorded pane id")
	}
	ctx := context.Background()
	if err := e.tmux.CopyModeScrollUp(ctx, *e.session.TmuxPaneID, 1); err != nil {
		t.Fatalf("enter copy-mode before reconnect: %v", err)
	}
	awaitPaneInMode(t, e, "1")

	if err := e.db.WithTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE sessions SET tmux_pane_id = '%9999' WHERE id = 'sess-1'`)
		return err
	}); err != nil {
		t.Fatalf("make recorded pane identity stale: %v", err)
	}

	refused, _ := e.dial(t, "browser-a", false)
	defer refused.Close()
	for {
		f := readFrame(t, refused)
		if f.Type != TypeError {
			continue
		}
		if f.Code != CodeAttachFailed {
			t.Fatalf("attach refusal code = %q, want %q", f.Code, CodeAttachFailed)
		}
		break
	}
	awaitPaneInMode(t, e, "1")
	_ = refused.Close()
	awaitAttachmentCount(t, e.proxy, e.session.ID, 0)

	if err := e.db.WithTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE sessions SET tmux_pane_id = ? WHERE id = 'sess-1'`, *e.session.TmuxPaneID)
		return err
	}); err != nil {
		t.Fatalf("restore recorded pane identity: %v", err)
	}

	retry, _ := e.dial(t, "browser-a", false)
	defer retry.Close()
	sawLive := false
	for {
		f := readFrame(t, retry)
		switch f.Type {
		case TypeScrollStatus:
			if f.Active == nil || *f.Active {
				t.Fatalf("attach live-view status = %v, want active=false", f.Active)
			}
			sawLive = true
		case TypeAttached:
			if !sawLive {
				t.Fatal("attached was published before authoritative live-view status")
			}
			awaitPaneInMode(t, e, "0")
			return
		case TypeError:
			t.Fatalf("retry failed: %s %s", f.Code, f.Message)
		}
	}
}

// A scroll frame with a direction outside the protocol is a bad frame, the
// same answer every other malformed known frame gets.
func TestScrollFrameRejectsUnknownDirection(t *testing.T) {
	e := newEnv(t)
	c, _ := e.dial(t, "browser-a", false)
	defer c.Close()
	token := awaitAttached(t, c)

	send(t, c, e.scrollFrame(token, "sideways", 1))
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		f := readFrame(t, c)
		if f.Type == TypeError {
			if f.Code != CodeBadFrame {
				t.Fatalf("error code = %q, want %q", f.Code, CodeBadFrame)
			}
			return
		}
	}
	t.Fatal("never saw the bad_frame error")
}

// A scroll frame from a fenced controller is refused on the lease, exactly
// like resize: view commands are controller actions.
func TestScrollFrameRequiresTheLease(t *testing.T) {
	e := newEnv(t)
	a, _ := e.dial(t, "browser-a", false)
	defer a.Close()
	tokenA := awaitAttached(t, a)

	b, _ := e.dial(t, "browser-b", true)
	defer b.Close()
	awaitAttached(t, b)

	// browser-a was displaced; its stale token must not move the view.
	send(t, a, e.scrollFrame(tokenA, ScrollUp, 3))
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		_ = a.SetReadDeadline(time.Now().Add(10 * time.Second))
		_, msg, err := a.ReadMessage()
		if err != nil {
			break // fenced attachments end in a close; either signal is fine
		}
		var f ServerFrame
		if jerr := json.Unmarshal(msg, &f); jerr == nil &&
			f.Type == TypeError && f.Code == CodeFenced {
			break
		}
	}
	awaitPaneInMode(t, e, "0")
}

// A pane left in copy-mode routes every key to the
// mode's key table — typed input would be silently swallowed while tmux
// still acknowledged the write. User input must exit the view first and
// then land in the TUI, honestly reported as sent.
func TestTypingAfterScrollExitsCopyModeAndReachesTheTUI(t *testing.T) {
	e := newEnv(t)
	c, _ := e.dial(t, "browser-a", false)
	defer c.Close()
	token := awaitAttached(t, c)

	send(t, c, e.scrollFrame(token, ScrollUp, 3))
	awaitPaneInMode(t, e, "1")

	// Typing while the view is open: the proxy cancels copy-mode under the
	// write gate, then writes — the bytes must reach the shell, not the
	// mode's key table.
	send(t, c, e.inputFrame(token, base64.StdEncoding.EncodeToString([]byte("echo typed-in-mode\n"))))
	awaitStatus(t, c, WriteSent)
	awaitOutput(t, c, "typed-in-mode")
	awaitPaneInMode(t, e, "0")
}

// The user can enter copy-mode through tmux's default prefix without a
// scroll frame. Input must observe that pane-owned state, leave the view, and
// only then report the following bytes as sent to the live shell.
func TestTypingAfterPrefixEnteredCopyModeExitsViewAndReachesTheTUI(t *testing.T) {
	e := newEnv(t)
	c, _ := e.dial(t, "browser-a", false)
	defer c.Close()
	token := awaitAttached(t, c)

	// C-b [ uses the real input path and tmux's default binding. No scroll
	// frame tells the proxy that tmux entered copy-mode.
	send(t, c, e.inputFrame(token, base64.StdEncoding.EncodeToString([]byte{'\x02', '['})))
	awaitStatus(t, c, WriteSent)
	awaitPaneInMode(t, e, "1")

	send(t, c, e.inputFrame(token,
		base64.StdEncoding.EncodeToString([]byte("echo typed-after-prefix\n"))))
	awaitStatus(t, c, WriteSent)
	awaitOutput(t, c, "typed-after-prefix")
	awaitPaneInMode(t, e, "0")
}

// Copy-mode survives detach/reattach: pane_in_mode is per-pane, not
// per-client, so a browser closed while scrolled up leaves the NEXT
// attachment aimed at a pane that eats typing. The exit-before-input rule
// must hold for a fresh attachment that never scrolled.
func TestCopyModeSurvivingDetachDoesNotEatTheNextAttachmentsTyping(t *testing.T) {
	e := newEnv(t)
	a, _ := e.dial(t, "browser-a", false)
	tokenA := awaitAttached(t, a)

	send(t, a, e.scrollFrame(tokenA, ScrollUp, 3))
	awaitPaneInMode(t, e, "1")
	_ = a.Close()

	// The mode outlives the attachment (copy-mode is
	// pane state). Give the teardown a moment, then prove it is still set.
	awaitAttachmentCount(t, e.proxy, e.session.ID, 0)
	awaitPaneInMode(t, e, "1")

	// The lease outlives the closed socket until its TTL, so the reattach
	// takes over — exactly what a real browser does after a tab dies.
	b, _ := e.dial(t, "browser-b", true)
	defer b.Close()
	tokenB := awaitAttached(t, b)

	send(t, b, e.inputFrame(tokenB, base64.StdEncoding.EncodeToString([]byte("echo typed-after-reattach\n"))))
	awaitStatus(t, b, WriteSent)
	awaitOutput(t, b, "typed-after-reattach")
	awaitPaneInMode(t, e, "0")
}

// Hostile line counts must be clamped before they reach tmux —
// an unclamped -N repeat count in the hundreds of millions wedges the whole
// tmux server. The frames must be served promptly and harmlessly.
func TestScrollFrameClampsHostileLineCounts(t *testing.T) {
	e := newEnv(t)
	c, _ := e.dial(t, "browser-a", false)
	defer c.Close()
	token := awaitAttached(t, c)

	// An enormous count: clamped to MaxCopyModeScrollLines, so the pane
	// enters copy-mode within the polling deadline instead of wedging tmux.
	send(t, c, e.scrollFrame(token, ScrollUp, 100_000_000))
	awaitPaneInMode(t, e, "1")

	// Zero and negative counts: clamped to 1, never "repeat count too
	// small" errors.
	send(t, c, e.scrollFrame(token, ScrollDown, -5))
	send(t, c, e.scrollFrame(token, ScrollDown, tmux.MaxCopyModeScrollLines))
	awaitPaneInMode(t, e, "0")

	// The server (and the attachment) survived all of it.
	send(t, c, e.inputFrame(token, base64.StdEncoding.EncodeToString([]byte("echo clamp-survived\n"))))
	awaitStatus(t, c, WriteSent)
	awaitOutput(t, c, "clamp-survived")
}

// The scroll rate limit is a server-side control, not a client
// courtesy. A same-instant burst spends the bucket and is then refused
// until time refills it.
func TestScrollLimiterBoundsTheForkRate(t *testing.T) {
	now := time.Now()
	l := newScrollLimiter(now)
	for i := 0; i < int(scrollBurst); i++ {
		if !l.allow(now) {
			t.Fatalf("burst token %d refused; the browser's legitimate burst must pass", i+1)
		}
	}
	if l.allow(now) {
		t.Fatal("token beyond the burst allowed at the same instant")
	}
	// 25ms — one browser coalescing interval — refills one token.
	if !l.allow(now.Add(25 * time.Millisecond)) {
		t.Fatal("token refused after a full refill interval")
	}
	if l.allow(now.Add(25 * time.Millisecond)) {
		t.Fatal("second token allowed without a second refill")
	}
	// A long quiet period refills to the cap, never beyond.
	late := now.Add(10 * time.Second)
	for i := 0; i < int(scrollBurst); i++ {
		if !l.allow(late) {
			t.Fatalf("token %d refused after a full refill", i+1)
		}
	}
	if l.allow(late) {
		t.Fatal("bucket refilled beyond its cap")
	}

	live := newLiveScrollLimiter(now)
	for i := 0; i < int(liveScrollBurst); i++ {
		if !live.allow(now) {
			t.Fatalf("reserved live token %d refused", i+1)
		}
	}
	if live.allow(now) {
		t.Fatal("live retry beyond the reserved burst was allowed")
	}
	if !live.allow(now.Add(250 * time.Millisecond)) {
		t.Fatal("reserved live token did not refill independently of movement")
	}
}
