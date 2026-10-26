// Package workspaceproxy forwards authorized application traffic to one fixed
// private backend. It never serves the gateway's own security endpoints.
package workspaceproxy

import (
	"context"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/noisefloor-oss/deck/internal/auth"
	"github.com/noisefloor-oss/deck/internal/workspacegateway"
)

type Config struct {
	Origin      string
	Socket      string
	OwnerUID    int
	Unencrypted bool
}

type Access interface {
	Require(context.Context, auth.Login) error
	Attach(context.Context, auth.Login, func()) (func(), error)
}

type connectionKey struct{}
type leaseKey struct{}

// ConnectionContext belongs on the gateway HTTP server. Keeping the actual
// transport lets revocation close even a blocked response or idle WebSocket
// synchronously, rather than waiting for the next application message.
func ConnectionContext(ctx context.Context, connection net.Conn) context.Context {
	return context.WithValue(ctx, connectionKey{}, connection)
}

type connections struct {
	mu     sync.Mutex
	closed bool
	values []net.Conn
	cancel context.CancelFunc
}

func (c *connections) add(connection net.Conn) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		_ = connection.Close()
		return false
	}
	c.values = append(c.values, connection)
	return true
}

func (c *connections) close() {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	values := c.values
	c.values = nil
	c.mu.Unlock()
	c.cancel()
	for _, connection := range values {
		_ = connection.Close()
	}
}

type Handler struct {
	cfg       Config
	access    Access
	runtime   workspacegateway.RuntimeObserver
	proxy     *httputil.ReverseProxy
	transport *http.Transport
}

func validConfig(cfg Config) bool {
	u, err := url.Parse(cfg.Origin)
	return err == nil && u.Scheme == "https" && u.Host != "" && u.User == nil && u.Path == "" && u.RawQuery == "" && u.Fragment == "" &&
		filepath.IsAbs(cfg.Socket) && filepath.Clean(cfg.Socket) == cfg.Socket && len(cfg.Socket) <= 103 && cfg.OwnerUID >= 0
}

func New(cfg Config, access Access, runtime workspacegateway.RuntimeObserver) (*Handler, error) {
	if !validConfig(cfg) || access == nil || runtime == nil {
		return nil, errors.New("workspace proxy requires fixed private transport and authorization")
	}
	transport := SocketTransport(cfg.Socket, cfg.OwnerUID)
	h := &Handler{cfg: cfg, access: access, runtime: runtime, transport: transport}
	u, _ := url.Parse(cfg.Origin)
	h.proxy = &httputil.ReverseProxy{
		Transport: transport,
		ErrorLog:  log.New(io.Discard, "", 0),
		Rewrite: func(request *httputil.ProxyRequest) {
			request.Out.URL.Scheme = "http"
			request.Out.URL.Host = "localhost"
			request.Out.Host = u.Host
			request.Out.Header.Del("Forwarded")
			request.Out.Header.Del("X-Forwarded-For")
			request.Out.Header.Del("X-Forwarded-Host")
			request.Out.Header.Del("X-Forwarded-Proto")
			request.Out.Header.Del("X-Real-IP")
			request.Out.Header.Del("Cookie")
			// Only the existing Google session crosses into the workload.
			// Workspace and enrollment bearers remain with the gateway.
			for _, cookie := range request.In.CookiesNamed("__Host-noisedeck_session") {
				request.Out.AddCookie(cookie)
			}
		},
		ModifyResponse: func(response *http.Response) error {
			cookies := response.Cookies()
			response.Header.Del("Set-Cookie")
			for _, cookie := range cookies {
				if cookie.Name == "__Host-noisedeck_session" || cookie.Name == "__Host-noisedeck_csrf" {
					response.Header.Add("Set-Cookie", cookie.String())
				}
			}
			response.Header.Set("Cache-Control", "no-store")
			response.Header.Set("X-Deck-Workspace-Security", "v1")
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, _ error) {
			http.Error(w, "workspace unavailable", http.StatusServiceUnavailable)
		},
	}
	return h, nil
}

func (h *Handler) Close() { h.transport.CloseIdleConnections() }

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Go and the backend must interpret exactly the same path. A browser
	// grant never authorizes the internal stop hook or a security API bypass.
	if !strings.HasPrefix(r.URL.Path, "/api/") || path.Clean(r.URL.Path) != r.URL.Path || r.URL.RawPath != "" || strings.Contains(r.URL.Path, "\\") ||
		strings.HasPrefix(r.URL.Path, "/api/internal/") || strings.HasPrefix(r.URL.Path, "/api/security/") || strings.HasPrefix(r.URL.Path, "/api/hooks/") {
		http.NotFound(w, r)
		return
	}
	login, ok := auth.CurrentLogin(r.Context())
	if !ok || h.access.Require(r.Context(), login) != nil {
		unlockRequired(w)
		return
	}
	state, err := h.runtime.ObserveSecurity(r.Context())
	storage := "open"
	if h.cfg.Unencrypted {
		storage = "unencrypted"
	}
	if err != nil || state.Storage != storage || state.Runtime != "running" || state.Locked {
		http.Error(w, "workspace unavailable", http.StatusServiceUnavailable)
		return
	}
	client, ok := r.Context().Value(connectionKey{}).(net.Conn)
	if !ok || client == nil {
		http.Error(w, "workspace transport unavailable", http.StatusServiceUnavailable)
		return
	}
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	lease := &connections{cancel: cancel}
	lease.add(client)
	ctx = context.WithValue(ctx, leaseKey{}, lease)
	// Attach repeats grant/method/epoch validation after runtime observation
	// and before dialing or exposing any backend bytes.
	release, err := h.access.Attach(ctx, login, lease.close)
	if err != nil {
		unlockRequired(w)
		return
	}
	defer release()
	h.proxy.ServeHTTP(w, r.WithContext(ctx))
}

// The browser removes its private UI on this code, including when a grant is
// revoked between the initial check and attaching the request's connection.
func unlockRequired(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Deck-Workspace-Security", "v1")
	w.WriteHeader(http.StatusForbidden)
	_, _ = io.WriteString(w, "{\"code\":\"unlock-required\"}\n")
}

// SocketTransport is for the fixed backend only. It has no proxy environment
// support or TCP fallback. A separate connection per request keeps a revoked
// browser from retaining an idle pooled transport owned by another grant.
func SocketTransport(socket string, ownerUID int) *http.Transport {
	return &http.Transport{
		DisableKeepAlives:     true,
		ForceAttemptHTTP2:     false,
		ResponseHeaderTimeout: 30 * time.Second,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			connection, err := dialPrivate(ctx, socket, ownerUID)
			if err != nil {
				return nil, errors.New("private workspace transport unavailable")
			}
			if lease, ok := ctx.Value(leaseKey{}).(*connections); ok && !lease.add(connection) {
				return nil, context.Canceled
			}
			return connection, nil
		},
	}
}
