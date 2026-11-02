package auth

import (
	"encoding/json"
	"io"
	"math"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"
)

const (
	maxPasswordBody    = 4096
	maxPasswordLength  = 72 // bcrypt's defined input bound; reject instead of silently truncating.
	minPasswordLength  = 8  // characters, applied only when a password is created or changed.
	passwordWindow     = 10 * time.Minute
	passwordLockout    = 5 * time.Minute
	globalLockout      = time.Minute
	maxSourceFailures  = 8
	maxGlobalFailures  = 80
	maxBackoff         = 30 * time.Second
	maxPasswordSources = 4096
)

type passwordAttempt struct {
	failures  int
	inFlight  int
	windowAt  time.Time
	retryAt   time.Time
	lockedTil time.Time
}

// passwordGate is an in-process credential-abuse boundary. One mutex makes
// admission and failure accounting linear under concurrent requests.
type passwordGate struct {
	mu      sync.Mutex
	now     func() time.Time
	sources map[string]passwordAttempt
	global  passwordAttempt
}

func newPasswordGate(now func() time.Time) *passwordGate {
	return &passwordGate{now: now, sources: make(map[string]passwordAttempt)}
}

func (g *passwordGate) admit(source string) (bool, time.Duration) {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := g.now()
	g.global = freshAttempt(g.global, now)
	s := freshAttempt(g.sources[source], now)
	// Exponential retry is source-local. Applying one attacker's global
	// retry to every client would keep the operator locked out forever.
	until := later(g.global.lockedTil, s.lockedTil)
	until = later(until, s.retryAt)
	if now.Before(until) {
		return false, until.Sub(now)
	}
	if g.global.failures+g.global.inFlight >= maxGlobalFailures || s.failures+s.inFlight >= maxSourceFailures {
		return false, time.Second
	}
	if _, exists := g.sources[source]; !exists {
		g.makeSourceRoom(now)
	}
	g.global.inFlight++
	s.inFlight++
	g.sources[source] = s
	return true, 0
}

func (g *passwordGate) record(source string, success bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := g.now()
	g.global.inFlight = max(0, g.global.inFlight-1)
	s := g.sources[source]
	s.inFlight = max(0, s.inFlight-1)
	if success {
		g.global = passwordAttempt{windowAt: g.global.windowAt, inFlight: g.global.inFlight}
		if s.inFlight == 0 {
			delete(g.sources, source)
		} else {
			g.sources[source] = passwordAttempt{windowAt: s.windowAt, inFlight: s.inFlight}
		}
		return
	}
	g.global = recordFailure(freshAttempt(g.global, now), now, maxGlobalFailures, globalLockout)
	if _, exists := g.sources[source]; !exists {
		g.makeSourceRoom(now)
	}
	g.sources[source] = recordFailure(freshAttempt(s, now), now, maxSourceFailures, passwordLockout)
}

func freshAttempt(a passwordAttempt, now time.Time) passwordAttempt {
	if a.windowAt.IsZero() || now.Sub(a.windowAt) >= passwordWindow {
		fresh := passwordAttempt{windowAt: now, inFlight: a.inFlight}
		if a.retryAt.After(now) {
			fresh.retryAt = a.retryAt
		}
		if a.lockedTil.After(now) {
			fresh.lockedTil = a.lockedTil
		}
		return fresh
	}
	return a
}

func attemptInactive(a passwordAttempt, now time.Time) bool {
	return a.failures == 0 && a.inFlight == 0 && !a.retryAt.After(now) && !a.lockedTil.After(now)
}

// makeSourceRoom bounds adversarial distinct-source memory. Expired entries
// go first; otherwise the oldest window without active attempts is evicted.
func (g *passwordGate) makeSourceRoom(now time.Time) {
	if len(g.sources) < maxPasswordSources {
		return
	}
	for source, attempt := range g.sources {
		fresh := freshAttempt(attempt, now)
		if attemptInactive(fresh, now) {
			delete(g.sources, source)
		}
	}
	if len(g.sources) < maxPasswordSources {
		return
	}
	var oldestSource string
	var oldest time.Time
	for source, attempt := range g.sources {
		if attempt.inFlight > 0 {
			continue
		}
		if oldestSource == "" || attempt.windowAt.Before(oldest) {
			oldestSource, oldest = source, attempt.windowAt
		}
	}
	delete(g.sources, oldestSource)
}

func recordFailure(a passwordAttempt, now time.Time, limit int, lockout time.Duration) passwordAttempt {
	a.failures++
	shift := min(a.failures-1, 7)
	backoff := time.Duration(math.Pow(2, float64(shift))) * 250 * time.Millisecond
	if backoff > maxBackoff {
		backoff = maxBackoff
	}
	a.retryAt = now.Add(backoff)
	if a.failures >= limit {
		a.lockedTil = now.Add(lockout)
	}
	return a
}

func later(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

func requestSource(remote string) string {
	host, _, err := net.SplitHostPort(remote)
	if err == nil {
		return host
	}
	return remote
}

func (h *Handler) passwordSource(r *http.Request) string {
	remote := requestSource(r.RemoteAddr)
	if !h.trusted(r.RemoteAddr) {
		return remote
	}
	forwarded := strings.Join(r.Header.Values("X-Forwarded-For"), ",")
	if forwarded == "" {
		return remote
	}
	hops := strings.Split(forwarded, ",")
	for i := len(hops) - 1; i >= 0; i-- {
		candidate := strings.TrimSpace(hops[i])
		if host, port, err := net.SplitHostPort(candidate); err == nil {
			if port == "" || strings.Trim(port, "0123456789") != "" {
				return remote
			}
			number, err := strconv.Atoi(port)
			if err != nil || number < 1 || number > 65535 {
				return remote
			}
			candidate = host
		}
		ip := net.ParseIP(candidate)
		if ip == nil {
			return remote
		}
		if !h.trustedIP(ip) {
			return candidate
		}
	}
	return remote
}

func (h *Handler) handlePasswordLogin(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	source := h.passwordSource(r)
	if ok, wait := h.password.admit(source); !ok {
		w.Header().Set("Retry-After", retryAfter(wait))
		h.passwordRefusal(w, http.StatusTooManyRequests)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxPasswordBody)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	var body struct {
		Password string `json:"password"`
	}
	if err := dec.Decode(&body); err != nil || body.Password == "" || len(body.Password) > maxPasswordLength || dec.Decode(&struct{}{}) != io.EOF {
		h.password.record(source, false)
		h.passwordRefusal(w, http.StatusUnauthorized)
		return
	}
	h.credentialMu.RLock()
	defer h.credentialMu.RUnlock()
	if err := bcrypt.CompareHashAndPassword(h.credentials.PasswordHash, []byte(body.Password)); err != nil {
		h.password.record(source, false)
		h.passwordRefusal(w, http.StatusUnauthorized)
		return
	}
	h.password.record(source, true)
	value, err := h.issueSessionValue(h.now())
	if err != nil {
		http.Error(w, "login unavailable", http.StatusInternalServerError)
		return
	}
	payload, err := h.verifySessionValue(value, h.now())
	if err != nil {
		http.Error(w, "login unavailable", http.StatusInternalServerError)
		return
	}
	h.setSessionCookies(w, value, payload)
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) passwordRefusal(w http.ResponseWriter, status int) {
	http.Error(w, "login failed", status)
}

func retryAfter(wait time.Duration) string {
	return strconv.Itoa(max(1, int(math.Ceil(wait.Seconds()))))
}
