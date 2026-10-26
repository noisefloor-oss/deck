//go:build linux

package workspaceproxy

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/rs/zerolog"

	"github.com/noisefloor-oss/deck/internal/auth"
	"github.com/noisefloor-oss/deck/internal/workspacegateway"
)

type accessFixture struct {
	mu      sync.Mutex
	allowed map[string]bool
	closers map[string]map[uint64]func()
	next    uint64
}

func newAccess() *accessFixture {
	return &accessFixture{allowed: map[string]bool{strings.Repeat("a", 64): true, strings.Repeat("b", 64): true}, closers: make(map[string]map[uint64]func())}
}

func (a *accessFixture) Require(_ context.Context, login auth.Login) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.allowed[login.ID] {
		return errors.New("denied")
	}
	return nil
}

func (a *accessFixture) Attach(_ context.Context, login auth.Login, closeStream func()) (func(), error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.allowed[login.ID] {
		return nil, errors.New("denied")
	}
	if a.closers[login.ID] == nil {
		a.closers[login.ID] = make(map[uint64]func())
	}
	a.next++
	id := a.next
	a.closers[login.ID][id] = closeStream
	return func() { a.mu.Lock(); delete(a.closers[login.ID], id); a.mu.Unlock() }, nil
}

func (a *accessFixture) revoke(id string) {
	a.mu.Lock()
	a.allowed[id] = false
	callbacks := a.closers[id]
	delete(a.closers, id)
	a.mu.Unlock()
	for _, closeStream := range callbacks {
		closeStream()
	}
}

type runtimeFixture struct {
	observe func() (workspacegateway.RuntimeState, error)
}

func (r runtimeFixture) ObserveSecurity(context.Context) (workspacegateway.RuntimeState, error) {
	if r.observe != nil {
		return r.observe()
	}
	return workspacegateway.RuntimeState{Storage: "open", Runtime: "running"}, nil
}

func unixBackend(t *testing.T, handler http.Handler) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "deck-proxy-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	socket := filepath.Join(dir, "backend.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(handler)
	_ = server.Listener.Close()
	server.Listener = listener
	server.Start()
	t.Cleanup(server.Close)
	return socket
}

var signingKey = bytes.Repeat([]byte{7}, 32)

func sessionHeader(id string) http.Header {
	now := time.Now()
	parts := []string{"v3", fmt.Sprint(now.Unix()), fmt.Sprint(now.Add(time.Hour).Unix()), id}
	for _, value := range []string{"https://accounts.google.com", "test-subject", "owner@example.test"} {
		parts = append(parts, base64.RawURLEncoding.EncodeToString([]byte(value)))
	}
	payload := strings.Join(parts, ":")
	sign := func(value string) string {
		mac := hmac.New(sha256.New, signingKey)
		_, _ = mac.Write([]byte(value))
		return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	}
	header := http.Header{}
	header.Set("Cookie", "__Host-noisedeck_session="+base64.RawURLEncoding.EncodeToString([]byte(payload))+"."+sign(payload)+"; __Host-deck_workspace=grant-canary; __Host-deck_enrollment=receipt-canary")
	header.Set(auth.CSRFHeader, sign("csrf:"+payload))
	return header
}

func proxyServer(t *testing.T, socket string, uid int, access Access, runtime workspacegateway.RuntimeObserver) *httptest.Server {
	t.Helper()
	proxy, err := New(Config{Origin: "https://workspace.example.test", Socket: socket, OwnerUID: uid}, access, runtime)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(proxy.Close)
	identity, err := auth.New(auth.Config{Mode: "oidc", ClientID: "test-client", ClientSecret: "test-secret", RedirectURL: "https://workspace.example.test/auth/callback", AllowedEmail: "owner@example.test", SessionSecret: signingKey, SecureCookies: true}, zerolog.Nop())
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(identity.Require(proxy))
	server.Config.ConnContext = ConnectionContext
	server.Start()
	t.Cleanup(server.Close)
	return server
}

func request(t *testing.T, server *httptest.Server, method, path, id string) *http.Response {
	t.Helper()
	r, err := http.NewRequest(method, server.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	r.Header = sessionHeader(id)
	r.Header.Set("Forwarded", "for=evil")
	r.Header.Set("X-Forwarded-For", "evil")
	response, err := server.Client().Do(r)
	if err != nil {
		t.Fatal(err)
	}
	return response
}

func TestForwardingKeepsGatewayBearersOutsideWorkload(t *testing.T) {
	var reached atomic.Int32
	socket := unixBackend(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached.Add(1)
		if r.Host != "workspace.example.test" || r.URL.Path != "/api/sessions" || r.URL.RawQuery != "limit=2" {
			t.Error("fixed backend changed request routing")
		}
		if strings.Contains(r.Header.Get("Cookie"), "canary") || len(r.Cookies()) != 1 || r.Cookies()[0].Name != "__Host-noisedeck_session" {
			t.Error("gateway bearer crossed into workload")
		}
		if r.Header.Get(auth.CSRFHeader) == "" || r.Header.Get("Forwarded") != "" || r.Header.Get("X-Forwarded-For") != "" {
			t.Error("incorrect forwarded authentication headers")
		}
		http.SetCookie(w, &http.Cookie{Name: "__Host-deck_workspace", Value: "forged"})
		http.SetCookie(w, &http.Cookie{Name: "__Host-noisedeck_session", Value: "renewed"})
		_, _ = io.WriteString(w, "workspace data")
	}))
	server := proxyServer(t, socket, os.Geteuid(), newAccess(), runtimeFixture{})
	response := request(t, server, "POST", "/api/sessions?limit=2", strings.Repeat("a", 64))
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil || response.StatusCode != 200 || string(body) != "workspace data" || reached.Load() != 1 {
		t.Fatalf("proxy failed: status=%d error=%v", response.StatusCode, err)
	}
	if response.Header.Get("Cache-Control") != "no-store" || response.Header.Get("X-Deck-Workspace-Security") != "v1" {
		t.Fatal("private response lost security cache boundary")
	}
	for _, cookie := range response.Cookies() {
		if cookie.Name == "__Host-deck_workspace" {
			t.Fatal("workload set a gateway grant cookie")
		}
	}
}

func TestProxyRejectsUnqualifiedAccessBeforeBackendBytes(t *testing.T) {
	for _, kind := range []string{"missing grant", "unknown runtime", "closed runtime", "revoked during observation", "wrong peer UID", "internal path", "encoded internal path", "encoded slash", "security path", "hook path", "conversation hook path", "encoded conversation hook path"} {
		t.Run(kind, func(t *testing.T) {
			var reached atomic.Int32
			socket := unixBackend(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { reached.Add(1); w.WriteHeader(200) }))
			access := newAccess()
			runtime := runtimeFixture{}
			uid := os.Geteuid()
			path := "/api/sessions"
			switch kind {
			case "missing grant":
				access.revoke(strings.Repeat("a", 64))
			case "unknown runtime":
				runtime.observe = func() (workspacegateway.RuntimeState, error) {
					return workspacegateway.RuntimeState{}, errors.New("unknown")
				}
			case "closed runtime":
				runtime.observe = func() (workspacegateway.RuntimeState, error) {
					return workspacegateway.RuntimeState{Storage: "closed", Runtime: "stopped"}, nil
				}
			case "revoked during observation":
				runtime.observe = func() (workspacegateway.RuntimeState, error) {
					access.revoke(strings.Repeat("a", 64))
					return workspacegateway.RuntimeState{Storage: "open", Runtime: "running"}, nil
				}
			case "wrong peer UID":
				uid++
			case "internal path":
				path = "/api/internal/workspace/prepare-lock"
			case "encoded internal path":
				path = "/api/%69nternal/workspace/prepare-lock"
			case "encoded slash":
				path = "/api/internal%2fworkspace/prepare-lock"
			case "security path":
				path = "/api/security/v1/state"
			case "hook path":
				path = "/api/hooks/capture"
			case "conversation hook path":
				path = "/api/hooks/conversation"
			case "encoded conversation hook path":
				path = "/api/hooks%2fconversation"
			}
			server := proxyServer(t, socket, uid, access, runtime)
			response := request(t, server, "GET", path, strings.Repeat("a", 64))
			defer response.Body.Close()
			body, _ := io.ReadAll(response.Body)
			if response.StatusCode < 400 || reached.Load() != 0 || bytes.Contains(body, []byte(socket)) {
				t.Fatalf("refusal leaked backend authority: status=%d reached=%d", response.StatusCode, reached.Load())
			}
			if kind == "missing grant" || kind == "revoked during observation" {
				if response.StatusCode != http.StatusForbidden || response.Header.Get("Content-Type") != "application/json" ||
					response.Header.Get("Cache-Control") != "no-store" || response.Header.Get("X-Deck-Workspace-Security") != "v1" {
					t.Fatalf("lost-grant refusal omitted the browser's security contract: status=%d type=%q cache=%q security=%q",
						response.StatusCode, response.Header.Get("Content-Type"), response.Header.Get("Cache-Control"), response.Header.Get("X-Deck-Workspace-Security"))
				}
				var refusal struct {
					Code string `json:"code"`
				}
				if err := json.Unmarshal(body, &refusal); err != nil || refusal.Code != "unlock-required" {
					t.Fatalf("lost-grant refusal must identify required browser teardown: code=%q error=%v", refusal.Code, err)
				}
			}
		})
	}
}

func TestRevocationClosesOnlyTheMatchingLiveWebSocket(t *testing.T) {
	var open atomic.Int32
	socket := unixBackend(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
		connection, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		open.Add(1)
		defer open.Add(-1)
		defer connection.Close()
		for {
			kind, message, err := connection.ReadMessage()
			if err != nil {
				return
			}
			if connection.WriteMessage(kind, message) != nil {
				return
			}
		}
	}))
	access := newAccess()
	server := proxyServer(t, socket, os.Geteuid(), access, runtimeFixture{})
	dial := func(id string) *websocket.Conn {
		t.Helper()
		connection, response, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/api/sessions/test/terminal", sessionHeader(id))
		if response != nil && response.Body != nil {
			_ = response.Body.Close()
		}
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = connection.Close() })
		return connection
	}
	a := dial(strings.Repeat("a", 64))
	b := dial(strings.Repeat("b", 64))
	access.revoke(strings.Repeat("a", 64))
	_ = a.SetReadDeadline(time.Now().Add(time.Second))
	if _, _, err := a.ReadMessage(); err == nil {
		t.Fatal("revoked socket stayed open")
	}
	if err := b.WriteMessage(websocket.TextMessage, []byte("still authorized")); err != nil {
		t.Fatal(err)
	}
	_ = b.SetReadDeadline(time.Now().Add(time.Second))
	_, body, err := b.ReadMessage()
	if err != nil || string(body) != "still authorized" {
		t.Fatalf("other browser lost its stream: %v", err)
	}
	deadline := time.Now().Add(time.Second)
	for open.Load() != 1 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if open.Load() != 1 {
		t.Fatal("revoked backend connection survived")
	}
}

func TestRevocationClosesAnIdleHTTPStream(t *testing.T) {
	backendClosed := make(chan struct{})
	socket := unixBackend(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: connected\n\n")
		_ = http.NewResponseController(w).Flush()
		<-r.Context().Done()
		close(backendClosed)
	}))
	access := newAccess()
	server := proxyServer(t, socket, os.Geteuid(), access, runtimeFixture{})
	response := request(t, server, "GET", "/api/session-activity/events", strings.Repeat("a", 64))
	defer response.Body.Close()
	first := make([]byte, len("data: connected\n\n"))
	if _, err := io.ReadFull(response.Body, first); err != nil {
		t.Fatal(err)
	}
	access.revoke(strings.Repeat("a", 64))
	if _, err := io.ReadAll(response.Body); err == nil {
		t.Fatal("revoked HTTP stream ended as successful complete response")
	}
	select {
	case <-backendClosed:
	case <-time.After(time.Second):
		t.Fatal("backend stream did not close")
	}
}
