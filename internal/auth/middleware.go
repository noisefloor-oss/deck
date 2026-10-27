package auth

import (
	"context"
	"crypto/subtle"
	"fmt"
	"net"
	"net/http"
)

// trustedPeer is one configured proxy source: a single IP or a CIDR.
type trustedPeer struct {
	ip  net.IP
	net *net.IPNet
}

func parseTrustedPeers(addrs []string) ([]trustedPeer, error) {
	out := make([]trustedPeer, 0, len(addrs))
	for _, a := range addrs {
		if ip := net.ParseIP(a); ip != nil {
			out = append(out, trustedPeer{ip: ip})
			continue
		}
		_, cidr, err := net.ParseCIDR(a)
		if err != nil {
			return nil, fmt.Errorf("auth: trusted proxy %q is neither an IP nor a CIDR", a)
		}
		out = append(out, trustedPeer{net: cidr})
	}
	return out, nil
}

func (h *Handler) trusted(remoteAddr string) bool {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	for _, p := range h.proxies {
		if p.ip != nil && p.ip.Equal(ip) {
			return true
		}
		if p.net != nil && p.net.Contains(ip) {
			return true
		}
	}
	return false
}

// Require authenticates every request it wraps. Password and OIDC verify
// the signed session cookie and enforces the session-bound CSRF token on
// state-changing methods. Trusted-proxy honors the identity header only
// from a configured proxy address, refusing everyone else before the
// header is even read, and consumes the header so nothing downstream can
// be confused by it.
func (h *Handler) Require(next http.Handler) http.Handler {
	switch h.cfg.Mode {
	case "none":
		return next
	case "trusted_proxy":
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !h.trusted(r.RemoteAddr) {
				// Strip the header WITHOUT reading it: a spoofed value from
				// an untrusted peer must not survive, even though this
				// request is being refused. Defense in depth.
				r.Header.Del(h.cfg.IdentityHeader)
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
			identity := r.Header.Get(h.cfg.IdentityHeader)
			r.Header.Del(h.cfg.IdentityHeader)
			if identity == "" {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			next.ServeHTTP(w, r.WithContext(withPrincipal(r.Context(), "proxy:"+identity)))
		})
	default: // password or oidc
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Renewal must not re-sign an old cookie with a password change's new secret.
			h.credentialMu.RLock()
			payload, err := h.sessionPayload(r)
			if err != nil {
				h.credentialMu.RUnlock()
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			// A session in use slides forward; only absence ends it. The
			// renewal keeps the login that authenticated it, so the absolute
			// cap still lands where it always would.
			if value, renewed, renewErr := h.sessionRenewal(payload, h.now()); renewErr == nil && value != "" {
				// A response that rotates a bearer cookie must never come
				// out of a shared cache: Set-Cookie alone is not a cache
				// prohibition to every proxy.
				w.Header().Set("Cache-Control", "no-store")
				h.setSessionCookies(w, value, renewed)
			}
			switch r.Method {
			case http.MethodGet, http.MethodHead, http.MethodOptions:
			default:
				if !h.checkCSRF(r) {
					h.credentialMu.RUnlock()
					http.Error(w, "forbidden", http.StatusForbidden)
					return
				}
			}
			ctx := withPrincipal(r.Context(), legacySingleUserPrincipal)
			if login, ok := loginFromPayload(payload); ok {
				ctx = context.WithValue(ctx, loginKey{}, login)
				ctx = withSessionExpiry(ctx, payload)
			}
			h.credentialMu.RUnlock()
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// sessionPayload extracts and verifies the session cookie.
func (h *Handler) sessionPayload(r *http.Request) (string, error) {
	c, err := r.Cookie(h.sessionCookieName())
	if err != nil {
		return "", errBadSession
	}
	return h.verifySessionValue(c.Value, h.now())
}

// checkCSRF requires the CSRF header to match the token derived from the
// verified session (double submit, session-bound, constant-time).
func (h *Handler) checkCSRF(r *http.Request) bool {
	_, ok := h.csrfPayload(r)
	return ok
}

// csrfPayload retains the identity that passed the request's CSRF decision so
// logout callbacks cannot lose that identity if revocation crosses expiry.
func (h *Handler) csrfPayload(r *http.Request) (string, bool) {
	payload, err := h.sessionPayload(r)
	if err != nil {
		return "", false
	}
	want := h.csrfFor(payload)
	got := r.Header.Get(CSRFHeader)
	if got == "" || subtle.ConstantTimeCompare([]byte(got), []byte(want)) != 1 {
		return "", false
	}
	return payload, true
}
