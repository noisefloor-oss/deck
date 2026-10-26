//go:build linux

package workspacehost

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/fstest"
	"time"

	"github.com/gorilla/websocket"
	"github.com/rs/zerolog"

	"github.com/noisefloor-oss/deck/internal/auth"
	"github.com/noisefloor-oss/deck/internal/lock"
	"github.com/noisefloor-oss/deck/internal/session"
	"github.com/noisefloor-oss/deck/internal/workspacegateway"
	"github.com/noisefloor-oss/deck/internal/workspacegrants"
	"github.com/noisefloor-oss/deck/internal/workspacekeys"
	"github.com/noisefloor-oss/deck/internal/workspaceruntime"
	"github.com/noisefloor-oss/deck/internal/workspaceservice"
)

// Only native storage/process observations are synthetic. HTTP, Unix transport,
// Google-cookie verification, registry proofs, grant transactions and stream
// revocation use the production assembly. This is not host qualification.
type assemblyNative struct {
	mu    sync.Mutex
	state workspaceruntime.State
	ops   map[string]workspaceruntime.Operation
	calls []string
}

func (n *assemblyNative) Observe(context.Context) (workspaceruntime.State, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.state, nil
}
func (n *assemblyNative) Initialize(_ context.Context, id string, secret []byte) (workspaceruntime.Operation, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if len(secret) != 32 {
		return workspaceruntime.Operation{}, workspaceruntime.ErrPrecondition
	}
	n.calls = append(n.calls, "initialize")
	n.state = workspaceruntime.State{Configured: true, Storage: "open", Runtime: "stopped", Epoch: "mounted"}
	op := workspaceruntime.Operation{ID: id, Kind: "initialize", Status: "succeeded", State: n.state}
	n.ops[id] = op
	return op, nil
}
func (n *assemblyNative) Initialization(ctx context.Context, id string) (workspaceruntime.Operation, error) {
	return n.Operation(ctx, id)
}
func (*assemblyNative) ReopenInitialization(context.Context, string, []byte) (workspaceruntime.Operation, error) {
	return workspaceruntime.Operation{}, workspaceruntime.ErrPrecondition
}
func (*assemblyNative) Unlock(context.Context, string, []byte) (workspaceruntime.Operation, error) {
	return workspaceruntime.Operation{}, workspaceruntime.ErrPrecondition
}
func (n *assemblyNative) Start(_ context.Context, id string) (workspaceruntime.Operation, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.calls = append(n.calls, "start")
	n.state.Runtime, n.state.Epoch = "running", "running"
	op := workspaceruntime.Operation{ID: id, Kind: "start", Status: "succeeded", State: n.state}
	n.ops[id] = op
	return op, nil
}
func (n *assemblyNative) Lock(ctx context.Context, id string, prepare func(context.Context) error) (workspaceruntime.Operation, error) {
	n.mu.Lock()
	n.calls = append(n.calls, "lock")
	n.mu.Unlock()
	err := prepare(ctx)
	n.mu.Lock()
	defer n.mu.Unlock()
	status := "uncertain"
	if err == nil {
		n.state = workspaceruntime.State{Configured: true, Locked: true, Storage: "closed", Runtime: "stopped", Epoch: "locked"}
		status = "succeeded"
	}
	op := workspaceruntime.Operation{ID: id, Kind: "lock", Status: status, State: n.state}
	if errors.Is(err, workspaceruntime.ErrLockRefused) {
		op.Status, op.Code = "failed", "lock-preparation-refused"
	}
	n.ops[id] = op
	return op, err
}
func (n *assemblyNative) Operation(_ context.Context, id string) (workspaceruntime.Operation, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	op, ok := n.ops[id]
	if !ok {
		return op, workspaceruntime.ErrNotFound
	}
	op.State = n.state
	return op, nil
}
func (n *assemblyNative) callCount() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return len(n.calls)
}

type assemblyFixture struct {
	t             *testing.T
	h             *Host
	native        *assemblyNative
	front         *httptest.Server
	backend       *httptest.Server
	backendHits   atomic.Int32
	prepareHits   atomic.Int32
	prepareStatus atomic.Int32
	redirect      string
	sseClosed     chan string
}
type assemblyBrowser struct {
	f       *assemblyFixture
	login   auth.Login
	cookies map[string]*http.Cookie
}
type assemblyMethod struct {
	ref workspacekeys.MethodRef
	key *ecdsa.PrivateKey
}

func requireOK(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
func newAssembly(t *testing.T) *assemblyFixture {
	t.Helper()
	return newAssemblyWithPlain(t, hostConfig(t), nil)
}

func newAssemblyWithPlain(t *testing.T, cfg Config, plain plainNative) *assemblyFixture {
	t.Helper()
	requireOK(t, cfg.Validate())
	f := &assemblyFixture{t: t, h: &Host{cfg: cfg}, sseClosed: make(chan string, 4), native: &assemblyNative{
		state: workspaceruntime.State{Storage: "closed", Runtime: "stopped", Epoch: "closed"}, ops: make(map[string]workspaceruntime.Operation)}}
	t.Cleanup(f.h.Close)
	requireOK(t, f.h.acquire())
	var encrypted workspaceservice.Native = f.native
	if cfg.policy() == workspacekeys.EncryptionUnavailable {
		encrypted = nil
	}
	requireOK(t, f.h.mountWithPlain(context.Background(), fstest.MapFS{"index.html": {Data: []byte("Security")}}, "test", encrypted, plain))
	socket := cfg.Socket()
	if plain != nil {
		socket = filepath.Join(cfg.Plain.StateRoot, "data", "gateway", "backend.sock")
	}
	requireOK(t, os.MkdirAll(filepath.Dir(socket), 0700))
	listener, err := net.Listen("unix", socket)
	requireOK(t, err)
	identity, err := auth.New(cfg.Auth, zerolog.Nop())
	requireOK(t, err)
	f.prepareStatus.Store(http.StatusNoContent)
	f.backend = httptest.NewUnstartedServer(identity.Require(http.HandlerFunc(f.serveBackend)))
	requireOK(t, f.backend.Listener.Close())
	f.backend.Listener = listener
	f.backend.Start()
	t.Cleanup(f.backend.Close)
	f.front = httptest.NewUnstartedServer(nil)
	f.front.Config = f.h.server
	f.front.Start()
	f.front.Client().CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	t.Cleanup(f.front.Close)
	return f
}
func (f *assemblyFixture) browser(id string) *assemblyBrowser {
	return &assemblyBrowser{f: f, login: auth.Login{ID: strings.Repeat(id, 64), Issuer: "https://accounts.google.com", Subject: "fixture-owner", Email: f.h.cfg.AllowedEmail}, cookies: make(map[string]*http.Cookie)}
}
func (b *assemblyBrowser) headers() http.Header {
	now := time.Now()
	parts := []string{"v3", fmt.Sprint(now.Unix()), fmt.Sprint(now.Add(time.Hour).Unix()), b.login.ID}
	for _, value := range []string{b.login.Issuer, b.login.Subject, b.login.Email} {
		parts = append(parts, base64.RawURLEncoding.EncodeToString([]byte(value)))
	}
	payload := strings.Join(parts, ":")
	sign := func(value string) string {
		mac := hmac.New(sha256.New, b.f.h.cfg.Auth.SessionSecret)
		_, _ = mac.Write([]byte(value))
		return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	}
	u, _ := url.Parse(b.f.h.cfg.Origin)
	headers := http.Header{}
	headers.Set("Host", u.Host)
	headers.Set("Origin", b.f.h.cfg.Origin)
	headers.Set("Content-Type", "application/json")
	headers.Set(auth.CSRFHeader, sign("csrf:"+payload))
	cookies := []string{"__Host-noisedeck_session=" + base64.RawURLEncoding.EncodeToString([]byte(payload)) + "." + sign(payload)}
	for _, cookie := range b.cookies {
		cookies = append(cookies, cookie.Name+"="+cookie.Value)
	}
	headers.Set("Cookie", strings.Join(cookies, "; "))
	return headers
}
func (b *assemblyBrowser) request(method, path string, body any) *http.Request {
	b.f.t.Helper()
	var raw []byte
	if body != nil {
		var err error
		raw, err = json.Marshal(body)
		requireOK(b.f.t, err)
	}
	r, err := http.NewRequest(method, b.f.front.URL+path, bytes.NewReader(raw))
	requireOK(b.f.t, err)
	r.Header = b.headers()
	r.Host = r.Header.Get("Host")
	r.Header.Del("Host")
	return r
}
func (b *assemblyBrowser) call(method, path string, body any, status int, target any) {
	b.f.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	response, err := b.f.front.Client().Do(b.request(method, path, body).WithContext(ctx))
	requireOK(b.f.t, err)
	defer response.Body.Close()
	raw, err := io.ReadAll(response.Body)
	requireOK(b.f.t, err)
	if response.StatusCode != status {
		b.f.t.Fatalf("%s %s status=%d, wanted %d", method, path, response.StatusCode, status)
	}
	for _, cookie := range response.Cookies() {
		if cookie.Name == "__Host-noisedeck_session" || cookie.Name == "__Host-noisedeck_csrf" {
			continue
		}
		if cookie.MaxAge < 0 {
			delete(b.cookies, cookie.Name)
		} else {
			b.cookies[cookie.Name] = cookie
		}
	}
	if target != nil {
		requireOK(b.f.t, json.Unmarshal(raw, target))
	}
}
func (b *assemblyBrowser) method(kind workspacekeys.Kind, commitment string) assemblyMethod {
	b.f.t.Helper()
	var prepared workspacekeys.PreparedMethod
	b.call("POST", "/api/security/v1/methods/prepare", map[string]any{"kind": kind, "label": string(kind)}, 201, &prepared)
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	requireOK(b.f.t, err)
	public, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	requireOK(b.f.t, err)
	b64 := base64.RawURLEncoding.EncodeToString
	// The gateway stores opaque envelopes. Browser encryption is tested in its
	// own implementation; these synthetic ciphertext bytes carry no plaintext.
	envelope := workspacekeys.Envelope{Version: 1, InstanceID: b.f.h.cfg.Native.InstanceID, MethodID: prepared.Method.ID,
		Revision: prepared.Method.Revision, Kind: kind, Salt: prepared.Method.Salt, Nonce: b64(make([]byte, 12)), Ciphertext: b64(make([]byte, 64)), ProofPublicKey: b64(public)}
	if kind == workspacekeys.Password {
		iterations := uint32(600000)
		envelope.Iterations = &iterations
	}
	b.call("PUT", "/api/security/v1/methods/"+prepared.Method.ID+"/envelope", map[string]any{"envelope": envelope, "commitment": commitment}, 204, nil)
	m := assemblyMethod{ref: prepared.Method.MethodRef, key: key}
	b.prove(m, workspacekeys.Action{Operation: workspacekeys.VerifyMethod, Method: m.ref}, nil, 200)
	return m
}
func (b *assemblyBrowser) prove(m assemblyMethod, action workspacekeys.Action, secret []byte, status int) string {
	b.f.t.Helper()
	var challenge workspacekeys.Challenge
	b.call("POST", "/api/security/v1/challenges", action, 201, &challenge)
	raw, err := base64.RawURLEncoding.DecodeString(challenge.Challenge)
	requireOK(b.f.t, err)
	hash := sha256.Sum256(raw)
	r, s, err := ecdsa.Sign(rand.Reader, m.key, hash[:])
	requireOK(b.f.t, err)
	signature := make([]byte, 64)
	r.FillBytes(signature[:32])
	s.FillBytes(signature[32:])
	body := map[string]string{"challengeId": challenge.ID, "signature": base64.RawURLEncoding.EncodeToString(signature)}
	if secret != nil {
		body["volumeSecret"] = base64.RawURLEncoding.EncodeToString(secret)
	}
	b.call("POST", "/api/security/v1/operations", body, status, nil)
	return challenge.OperationID
}
func (f *assemblyFixture) initialize(b *assemblyBrowser) assemblyMethod {
	f.t.Helper()
	ctx := context.Background()
	keys, err := workspacekeys.OpenExisting(ctx, f.h.db, workspacekeys.Config{InstanceID: f.h.cfg.Native.InstanceID, Origin: f.h.cfg.Origin, RPID: f.h.cfg.Gateway().RPID})
	requireOK(f.t, err)
	invites, err := workspacegateway.NewInvitations(ctx, f.h.db, keys, f.h.cfg.AllowedEmail, nil)
	requireOK(f.t, err)
	token, _, err := invites.Issue(ctx)
	requireOK(f.t, err)
	b.call("POST", "/api/security/v1/invitation/redeem", map[string]string{"token": token}, 200, nil)
	secret := bytes.Repeat([]byte{9}, 32)
	hash := sha256.New()
	_, _ = hash.Write([]byte("deck.volume.v1\x00"))
	_, _ = hash.Write(secret)
	commitment := base64.RawURLEncoding.EncodeToString(hash.Sum(nil))
	day, recovery := b.method(workspacekeys.Password, commitment), b.method(workspacekeys.Recovery, commitment)
	b.prove(day, workspacekeys.Action{Operation: workspacekeys.Initialize, Method: day.ref, Recovery: &recovery.ref}, secret, 200)
	if b.cookies[workspacegrants.CookieName] == nil {
		f.t.Fatal("assembly did not deliver the native-completion browser grant")
	}
	return day
}

func (f *assemblyFixture) serveBackend(w http.ResponseWriter, r *http.Request) {
	f.backendHits.Add(1)
	login, ok := auth.CurrentLogin(r.Context())
	if !ok || len(r.Cookies()) != 1 || r.Cookies()[0].Name != "__Host-noisedeck_session" {
		f.t.Error("backend did not receive exactly the existing Google credential")
		w.WriteHeader(http.StatusForbidden)
		return
	}
	switch r.URL.Path {
	case "/api/internal/workspace/prepare-lock":
		f.prepareHits.Add(1)
		var grants int
		err := f.h.db.QueryRow(`SELECT count(*) FROM workspacegrants_grants`).Scan(&grants)
		if err != nil || grants != 0 || r.Method != "POST" || r.ContentLength != 0 || r.Header.Get(auth.CSRFHeader) == "" || r.Header.Get("Origin") != f.h.cfg.Origin || r.Header.Get("Authorization") != "" {
			f.t.Error("private preparation retained grants or forwarded excess authority")
			w.WriteHeader(http.StatusForbidden)
			return
		}
		if budget, err := strconv.ParseInt(r.Header.Get(session.WorkspaceLockBudgetHeader), 10, 64); err != nil || budget <= 0 || budget > session.WorkspaceLockTimeout.Milliseconds() {
			f.t.Errorf("private preparation forwarded budget %q, want within the lock ceiling", r.Header.Get(session.WorkspaceLockBudgetHeader))
			w.WriteHeader(http.StatusForbidden)
			return
		}
		status := int(f.prepareStatus.Load())
		if status == http.StatusFound {
			http.Redirect(w, r, f.redirect, status)
			return
		}
		w.WriteHeader(status)
	case "/api/sessions/test/terminal":
		upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
		connection, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer connection.Close()
		for {
			kind, body, err := connection.ReadMessage()
			if err != nil || connection.WriteMessage(kind, body) != nil {
				return
			}
		}
	case "/api/session-activity/events":
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: connected\n\n")
		_ = http.NewResponseController(w).Flush()
		<-r.Context().Done()
		f.sseClosed <- login.ID
	default:
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data":"synthetic private workspace"}`)
	}
}
func (b *assemblyBrowser) websocket() *websocket.Conn {
	b.f.t.Helper()
	connection, response, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(b.f.front.URL, "http")+"/api/sessions/test/terminal", b.headers())
	if response != nil && response.Body != nil {
		_ = response.Body.Close()
	}
	requireOK(b.f.t, err)
	b.f.t.Cleanup(func() { _ = connection.Close() })
	return connection
}
func (b *assemblyBrowser) events() *http.Response {
	b.f.t.Helper()
	response, err := b.f.front.Client().Do(b.request("GET", "/api/session-activity/events", nil))
	requireOK(b.f.t, err)
	b.f.t.Cleanup(func() { _ = response.Body.Close() })
	if response.StatusCode != 200 {
		b.f.t.Fatal("event stream refused", response.StatusCode)
	}
	first := make([]byte, len("data: connected\n\n"))
	_, err = io.ReadFull(response.Body, first)
	requireOK(b.f.t, err)
	return response
}
func requireClosedSocket(t *testing.T, connection *websocket.Conn) {
	t.Helper()
	requireOK(t, connection.SetReadDeadline(time.Now().Add(2*time.Second)))
	_, _, err := connection.ReadMessage()
	var timeout net.Error
	if err == nil || errors.As(err, &timeout) && timeout.Timeout() {
		t.Fatal("revocation did not close the idle WebSocket", err)
	}
}
func requireClosedEvents(t *testing.T, response *http.Response) {
	t.Helper()
	closed := make(chan error, 1)
	go func() { _, err := io.ReadAll(response.Body); closed <- err }()
	select {
	case err := <-closed:
		if err == nil {
			t.Fatal("revoked SSE was reported as a completed response")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("revocation did not close the idle SSE response")
	}
}

func TestAssemblyRequiresMethodProofAndLogoutClosesOnlyItsBrowser(t *testing.T) {
	f := newAssembly(t)
	a, b := f.browser("a"), f.browser("b")
	method := f.initialize(a)
	before := f.native.callCount()
	b.call("GET", "/api/sessions", nil, http.StatusForbidden, nil)
	// An ordinary reader may copy the Google session identity and every public
	// control record. A stored bearer hash is still not an unlock cookie.
	forged := f.browser("a")
	var storedHash string
	requireOK(t, f.h.db.QueryRow(`SELECT hash FROM workspacegrants_grants`).Scan(&storedHash))
	forged.cookies[workspacegrants.CookieName] = &http.Cookie{Name: workspacegrants.CookieName, Value: storedHash}
	forged.call("GET", "/api/sessions", nil, http.StatusForbidden, nil)
	if f.backendHits.Load() != 0 {
		t.Fatal("Google-only browser reached the private backend")
	}
	b.prove(method, workspacekeys.Action{Operation: workspacekeys.AuthorizeBrowser, Method: method.ref}, nil, 200)
	var data map[string]string
	b.call("GET", "/api/sessions", nil, 200, &data)
	if data["data"] != "synthetic private workspace" || f.native.callCount() != before {
		t.Fatal("browser authorization remounted or failed to deliver private data")
	}
	wa, wb := a.websocket(), b.websocket()
	ea, eb := a.events(), b.events()
	defer ea.Body.Close()
	defer eb.Body.Close()
	a.call("POST", "/auth/logout", nil, 204, nil)
	requireClosedSocket(t, wa)
	requireClosedEvents(t, ea)
	select {
	case id := <-f.sseClosed:
		if id != a.login.ID {
			t.Fatal("logout closed another browser's backend stream")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("logout left the backend SSE connection open")
	}
	requireOK(t, wb.WriteMessage(websocket.TextMessage, []byte("still authorized")))
	requireOK(t, wb.SetReadDeadline(time.Now().Add(2*time.Second)))
	_, body, err := wb.ReadMessage()
	requireOK(t, err)
	if string(body) != "still authorized" || f.native.callCount() != before {
		t.Fatal("logout changed another browser or the running runtime")
	}
	select {
	case <-f.sseClosed:
		t.Fatal("other browser's idle SSE did not survive logout")
	default:
	}
	_ = eb.Body.Close()
	a.call("GET", "/api/sessions", nil, 403, nil)
}

func TestAssemblyDuplicateStartupPreservesLiveBrowser(t *testing.T) {
	f := newAssembly(t)
	b := f.browser("a")
	f.initialize(b)
	connection, events := b.websocket(), b.events()
	defer events.Body.Close()
	before := f.native.callCount()
	var originalHash string
	requireOK(t, f.h.db.QueryRow(`SELECT hash FROM workspacegrants_grants`).Scan(&originalHash))
	assets := fstest.MapFS{"index.html": {Data: []byte("Security")}}
	for _, bind := range []string{f.h.cfg.Bind, "127.0.0.1:18444"} {
		cfg := f.h.cfg
		cfg.Bind = bind
		// Valid native configuration can open its journal without dispatching
		// host commands. The lifetime guard must refuse even that second owner.
		cfg.Native.HomeDir = filepath.Dir(cfg.Native.ControlDir)
		cfg.Native.GocryptfsPath = filepath.Join(cfg.Native.HomeDir, "gocryptfs")
		cfg.Native.FusermountPath = filepath.Join(cfg.Native.HomeDir, "fusermount3")
		cfg.Native.PodmanPath = filepath.Join(cfg.Native.HomeDir, "podman")
		cfg.Native.SealedComposePath = filepath.Join(cfg.Native.HomeDir, "sealed-compose.sh")
		cfg.Native.ComposeEnvFile = filepath.Join(cfg.Native.HomeDir, "compose.env")
		for _, path := range []string{cfg.Native.GocryptfsPath, cfg.Native.FusermountPath, cfg.Native.PodmanPath, cfg.Native.SealedComposePath} {
			requireOK(t, os.WriteFile(path, []byte("#!/bin/sh\nexit 1\n"), 0700))
		}
		requireOK(t, os.WriteFile(cfg.Native.ComposeEnvFile, nil, 0600))
		cfg.Native.ComposeProject = "fixture-workspace"
		cfg.Native.AppContainer = "fixture-workspace-app-1"
		second, err := Open(context.Background(), cfg, assets, "test")
		if second != nil {
			second.Close()
			t.Fatal("duplicate gateway acquired the same installation")
		}
		if !errors.Is(err, lock.ErrHeld) {
			t.Fatal("duplicate gateway did not fail at the lifetime guard", err)
		}
		if _, err := os.Stat(filepath.Join(cfg.Native.ControlDir, "native.sqlite")); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("duplicate startup opened the native journal", err)
		}
		var storedHash string
		requireOK(t, f.h.db.QueryRow(`SELECT hash FROM workspacegrants_grants`).Scan(&storedHash))
		if storedHash != originalHash || f.native.callCount() != before {
			t.Fatal("duplicate startup changed live authority or native state")
		}
		b.call("GET", "/api/sessions", nil, http.StatusOK, nil)
		requireOK(t, connection.WriteMessage(websocket.TextMessage, []byte("still authorized")))
		requireOK(t, connection.SetReadDeadline(time.Now().Add(2*time.Second)))
		_, body, err := connection.ReadMessage()
		requireOK(t, err)
		if string(body) != "still authorized" {
			t.Fatal("duplicate startup disturbed the live WebSocket")
		}
		select {
		case <-f.sseClosed:
			t.Fatal("duplicate startup closed the idle backend stream")
		default:
		}
	}
	f.h.Close()
	requireClosedSocket(t, connection)
	requireClosedEvents(t, events)
	held, err := lock.Acquire(filepath.Join(f.h.cfg.Native.ControlDir, "gateway.lock"))
	requireOK(t, err)
	requireOK(t, held.Release())
}

func TestAssemblyFailedStartupReleasesLifetimeGuard(t *testing.T) {
	cfg := hostConfig(t)
	// This passes gateway validation but lacks the native executable paths.
	// Startup must release its acquired lifetime lock when native Open fails.
	requireOK(t, cfg.Validate())
	host, err := Open(context.Background(), cfg, fstest.MapFS{"index.html": {Data: []byte("Security")}}, "test")
	if host != nil {
		host.Close()
		t.Fatal("incomplete native configuration opened")
	}
	if !errors.Is(err, ErrConfiguration) {
		t.Fatal("startup did not reach native validation", err)
	}
	held, err := lock.Acquire(filepath.Join(cfg.Native.ControlDir, "gateway.lock"))
	requireOK(t, err)
	requireOK(t, held.Release())
}

func TestAssemblyExplicitLockUsesPrivatePreparationAndRevokesStreams(t *testing.T) {
	f := newAssembly(t)
	b := f.browser("a")
	method := f.initialize(b)
	connection, events := b.websocket(), b.events()
	defer events.Body.Close()
	before := f.backendHits.Load()
	for _, path := range []string{"/api/internal/workspace/prepare-lock", "/api/%69nternal/workspace/prepare-lock", "/api/internal%2fworkspace/prepare-lock", "/api/hooks/capture", "/api/hooks/conversation"} {
		b.call("POST", path, nil, 404, nil)
	}
	if f.backendHits.Load() != before || f.prepareHits.Load() != 0 {
		t.Fatal("public API bypass reached the private preparation route")
	}
	operation := b.prove(method, workspacekeys.Action{Operation: workspacekeys.Lock, Method: method.ref}, nil, 200)
	requireClosedSocket(t, connection)
	requireClosedEvents(t, events)
	var status workspacegateway.OperationState
	b.call("GET", "/api/security/v1/operations/"+operation, nil, 200, &status)
	if f.prepareHits.Load() != 1 || status.Status != "complete" || !status.Locked {
		t.Fatal("lock did not pass through authenticated preparation and closure")
	}
	b.call("GET", "/api/sessions", nil, 403, nil)
}

func TestAssemblyCleanLockRefusalAllowsFreshProof(t *testing.T) {
	f := newAssembly(t)
	b := f.browser("a")
	method := f.initialize(b)
	f.prepareStatus.Store(http.StatusUnprocessableEntity)
	first := b.prove(method, workspacekeys.Action{Operation: workspacekeys.Lock, Method: method.ref}, nil, 202)
	var status workspacegateway.OperationState
	b.call("GET", "/api/security/v1/operations/"+first, nil, 200, &status)
	if status.Status != "refused" || status.Locked || status.Runtime != "running" {
		t.Fatalf("clean refusal: %+v", status)
	}
	b.call("GET", "/api/sessions", nil, 403, nil)
	b.prove(method, workspacekeys.Action{Operation: workspacekeys.AuthorizeBrowser, Method: method.ref}, nil, 200)
	b.call("GET", "/api/sessions", nil, 200, nil)
	f.prepareStatus.Store(http.StatusNoContent)
	second := b.prove(method, workspacekeys.Action{Operation: workspacekeys.Lock, Method: method.ref}, nil, 200)
	if second == first {
		t.Fatal("retry reused a consumed lock proof")
	}
}

func TestPrivateLockTransportDistinguishesCleanRefusal(t *testing.T) {
	f := newAssembly(t)
	b := f.browser("a")
	f.initialize(b)
	request := b.request(http.MethodPost, "/", nil)
	var ownerCookie string
	for _, cookie := range request.Cookies() {
		if cookie.Name == "__Host-noisedeck_session" {
			ownerCookie = cookie.Value
		}
	}
	ctx := context.WithValue(context.Background(), privateRequestKey{}, privateRequest{session: ownerCookie, csrf: request.Header.Get(auth.CSRFHeader)})
	// The production private transport authenticates the owner cookie and carries
	// only the explicit preparation request. The fixture returns reviewed outcomes.
	for _, tc := range []struct {
		status int
		want   error
	}{
		{http.StatusUnprocessableEntity, workspaceruntime.ErrLockRefused},
		{http.StatusServiceUnavailable, ErrUnavailable},
		{http.StatusNoContent, nil},
	} {
		f.prepareStatus.Store(int32(tc.status))
		// Preparation requires browser grants to have been revoked before dispatch.
		_, err := f.h.db.Exec(`DELETE FROM workspacegrants_grants`)
		requireOK(t, err)
		if err := f.h.prepareLock(ctx); !errors.Is(err, tc.want) {
			t.Fatalf("status %d mapped to %v, want %v", tc.status, err, tc.want)
		}
	}
}

func TestAssemblyRefusesRedirectedPreparationAndMissingUnixBackend(t *testing.T) {
	f := newAssembly(t)
	b := f.browser("a")
	method := f.initialize(b)
	var redirected atomic.Int32
	trap := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { redirected.Add(1); w.WriteHeader(http.StatusNoContent) }))
	defer trap.Close()
	f.redirect = trap.URL
	f.prepareStatus.Store(http.StatusFound)
	operation := b.prove(method, workspacekeys.Action{Operation: workspacekeys.Lock, Method: method.ref}, nil, 202)
	var status workspacegateway.OperationState
	b.call("GET", "/api/security/v1/operations/"+operation, nil, 200, &status)
	if redirected.Load() != 0 || status.Locked || status.Runtime != "running" || status.Status != "unconfirmed" {
		t.Fatal("private preparation followed a redirect or fabricated a lock")
	}
	// A new proof can authorize the still-running workspace after the failed
	// stop, but the public gateway never falls back when its Unix peer vanishes.
	b.prove(method, workspacekeys.Action{Operation: workspacekeys.AuthorizeBrowser, Method: method.ref}, nil, 200)
	requireOK(t, os.Rename(f.h.cfg.Socket(), f.h.cfg.Socket()+".offline"))
	before := f.backendHits.Load()
	b.call("GET", "/api/sessions?url="+url.QueryEscape(trap.URL), nil, 503, nil)
	if f.backendHits.Load() != before || redirected.Load() != 0 {
		t.Fatal("missing private peer fell back to another backend")
	}
}
