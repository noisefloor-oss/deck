package auth

import (
	"bytes"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"golang.org/x/crypto/bcrypt"
)

func newPasswordHandler(t *testing.T, now func() time.Time) *Handler {
	t.Helper()
	hash, err := bcrypt.GenerateFromPassword([]byte("correct horse battery staple"), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	h, err := New(Config{
		Mode:          "password",
		PasswordHash:  hash,
		SessionSecret: []byte("0123456789abcdef0123456789abcdef"),
		Now:           now,
	}, zerolog.Nop())
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func TestPasswordGateSerializesConcurrentFailures(t *testing.T) {
	now := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)
	g := newPasswordGate(func() time.Time { return now })
	var workers sync.WaitGroup
	for i := 0; i < 32; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			g.record("192.0.2.10", false)
		}()
	}
	workers.Wait()
	g.mu.Lock()
	source := g.sources["192.0.2.10"]
	global := g.global
	g.mu.Unlock()
	if source.failures != 32 || global.failures != 32 {
		t.Fatalf("failure accounting source=%d global=%d", source.failures, global.failures)
	}
	if ok, wait := g.admit("192.0.2.10"); ok || wait <= 0 || wait > passwordLockout {
		t.Fatalf("concurrent lockout admitted=%v wait=%v", ok, wait)
	}
}

func TestPasswordGateReservesConcurrentAttempts(t *testing.T) {
	for _, tc := range []struct {
		name    string
		sources int
		limit   int
	}{
		{name: "source", sources: 1, limit: maxSourceFailures},
		{name: "global", sources: maxGlobalFailures * 2, limit: maxGlobalFailures},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)
			g := newPasswordGate(func() time.Time { return now })
			start := make(chan struct{})
			admitted := make(chan string, maxGlobalFailures*2)
			var workers sync.WaitGroup
			for i := 0; i < cap(admitted); i++ {
				workers.Add(1)
				go func() {
					defer workers.Done()
					<-start
					source := fmt.Sprintf("192.0.2.%d", i%tc.sources)
					if ok, _ := g.admit(source); ok {
						admitted <- source
					}
				}()
			}
			close(start)
			workers.Wait()
			close(admitted)
			if len(admitted) != tc.limit {
				t.Fatalf("admitted %d attempts before any record, want %d", len(admitted), tc.limit)
			}
			source := <-admitted
			now = now.Add(passwordWindow)
			if ok, _ := g.admit(source); ok {
				t.Fatal("window rollover discarded active reservations")
			}
			g.record(source, true)
			if ok, _ := g.admit(source); !ok {
				t.Fatal("successful record did not release a reservation")
			}
			if ok, _ := g.admit(source); ok {
				t.Fatal("successful record released other active reservations")
			}
			g.record(source, false)
			for source := range admitted {
				g.record(source, false)
			}
			if g.global.inFlight != 0 || g.global.failures != tc.limit {
				t.Fatalf("global accounting = %+v", g.global)
			}
			for source, attempt := range g.sources {
				if attempt.inFlight != 0 {
					t.Fatalf("source %s retained %d reservations", source, attempt.inFlight)
				}
			}
		})
	}
}

func TestPasswordGateBoundsDistinctSourceState(t *testing.T) {
	now := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)
	g := newPasswordGate(func() time.Time { return now })
	for i := 0; i < maxPasswordSources*2; i++ {
		if ok, _ := g.admit(fmt.Sprintf("192.0.2.%d", i)); !ok {
			t.Fatal("clean admission was unexpectedly limited")
		}
		g.record(fmt.Sprintf("192.0.2.%d", i), true)
	}
	if len(g.sources) != 0 {
		t.Fatalf("successful sources retained = %d", len(g.sources))
	}
	for i := 0; i < maxPasswordSources+100; i++ {
		g.record(fmt.Sprintf("198.51.100.%d", i), false)
	}
	if len(g.sources) != maxPasswordSources {
		t.Fatalf("failure sources retained = %d, want cap %d", len(g.sources), maxPasswordSources)
	}
}

func TestPasswordGateWindowRolloverDoesNotShortenLockout(t *testing.T) {
	now := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)
	g := newPasswordGate(func() time.Time { return now })
	g.sources["192.0.2.10"] = passwordAttempt{
		failures:  maxSourceFailures,
		windowAt:  now,
		lockedTil: now.Add(15 * time.Minute),
	}
	now = now.Add(passwordWindow + time.Minute)
	if ok, wait := g.admit("192.0.2.10"); ok || wait != 4*time.Minute {
		t.Fatalf("rollover admitted=%v wait=%v", ok, wait)
	}
}

func TestPasswordSourceUsesForwardedIPOnlyFromTrustedProxies(t *testing.T) {
	h := newPasswordHandler(t, time.Now)
	h.proxies = []trustedPeer{{ip: net.ParseIP("192.0.2.10")}}
	req := passwordRequest(`{"password":"x"}`)
	req.Header.Set("X-Forwarded-For", "198.51.100.7, 192.0.2.10")
	if got := h.passwordSource(req); got != "198.51.100.7" {
		t.Fatalf("trusted forwarded source = %q", got)
	}
	req.Header.Set("X-Forwarded-For", "203.0.113.66, 198.51.100.7")
	if got := h.passwordSource(req); got != "198.51.100.7" {
		t.Fatalf("client-supplied prefix selected the forged source %q", got)
	}
	req.Header.Set("X-Forwarded-For", "203.0.113.66")
	req.Header.Add("X-Forwarded-For", "198.51.100.7")
	if got := h.passwordSource(req); got != "198.51.100.7" {
		t.Fatalf("repeated fields selected the forged source %q", got)
	}
	req.RemoteAddr = "192.0.2.11:40000"
	if got := h.passwordSource(req); got != "192.0.2.11" {
		t.Fatalf("untrusted forwarded source = %q", got)
	}
	req.RemoteAddr = "192.0.2.10:40000"
	req.Header.Set("X-Forwarded-For", "not-an-ip")
	if got := h.passwordSource(req); got != "192.0.2.10" {
		t.Fatalf("malformed forwarded source = %q", got)
	}
}

func TestPasswordSourceAcceptsForwardedPorts(t *testing.T) {
	h := newPasswordHandler(t, time.Now)
	h.proxies = []trustedPeer{{ip: net.ParseIP("192.0.2.10")}}
	for _, tc := range []struct{ forwarded, want string }{
		{"203.0.113.5:56725", "203.0.113.5"},
		{"[2001:db8::1]:8080", "2001:db8::1"},
		{"not-an-ip", "192.0.2.10"},
		{"203.0.113.5:", "192.0.2.10"},
		{"203.0.113.5:garbage", "192.0.2.10"},
		{"203.0.113.5:-1", "192.0.2.10"},
		{"203.0.113.5:+80", "192.0.2.10"},
		{"[2001:db8::1]:+80", "192.0.2.10"},
		{"[2001:db8::1]:65536", "192.0.2.10"},
		{"203.0.113.5, 192.0.2.10:garbage", "192.0.2.10"},
		{"203.0.113.5, 192.0.2.10:+80", "192.0.2.10"},
	} {
		req := passwordRequest(`{"password":"x"}`)
		req.Header.Set("X-Forwarded-For", tc.forwarded)
		if got := h.passwordSource(req); got != tc.want {
			t.Errorf("forwarded source %q = %q, want %q", tc.forwarded, got, tc.want)
		}
	}
}

func TestPasswordLoginSeparatesTrustedProxyClients(t *testing.T) {
	now := time.Now()
	h := newPasswordHandler(t, func() time.Time { return now })
	h.proxies = []trustedPeer{{ip: net.ParseIP("192.0.2.10")}}
	for range maxSourceFailures {
		req := passwordRequest(`{"password":"wrong"}`)
		req.Header.Set("X-Forwarded-For", "203.0.113.50")
		h.handlePasswordLogin(httptest.NewRecorder(), req)
		now = now.Add(35 * time.Second)
	}
	if !h.password.sources["203.0.113.50"].lockedTil.After(now) {
		t.Fatalf("attacking source was not locked: %+v", h.password.sources["203.0.113.50"])
	}
	req := passwordRequest(`{"password":"correct horse battery staple"}`)
	req.Header.Set("X-Forwarded-For", "198.51.100.7")
	rec := httptest.NewRecorder()
	h.handlePasswordLogin(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("different proxy client was locked out: %d %s", rec.Code, rec.Body.String())
	}
	if h.password.global.failures != 0 || h.password.global.retryAt.After(now) {
		t.Fatalf("successful login did not clear global failures: %+v", h.password.global)
	}
}

func passwordRequest(body string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/auth/password", strings.NewReader(body))
	req.RemoteAddr = "192.0.2.10:40000"
	req.Header.Set("Content-Type", "application/json")
	return req
}

func TestPasswordLoginIssuesExistingSessionCookies(t *testing.T) {
	now := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)
	h := newPasswordHandler(t, func() time.Time { return now })
	rec := httptest.NewRecorder()
	h.handlePasswordLogin(rec, passwordRequest(`{"password":"correct horse battery staple"}`))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("login = %d: %s", rec.Code, rec.Body.String())
	}
	cookies := rec.Result().Cookies()
	if len(cookies) != 2 || cookies[0].Name != h.sessionCookieName() || cookies[1].Name != h.csrfCookieName() {
		t.Fatalf("cookies = %+v", cookies)
	}

	probe := httptest.NewRequest(http.MethodGet, "/auth/session", nil)
	probe.AddCookie(cookies[0])
	passed := false
	h.Require(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { passed = true })).ServeHTTP(httptest.NewRecorder(), probe)
	if !passed {
		t.Fatal("password login cookie did not pass the existing session middleware")
	}
}

func TestPasswordLoginRefusesGenericallyAndBacksOffPerSource(t *testing.T) {
	now := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)
	h := newPasswordHandler(t, func() time.Time { return now })
	first := httptest.NewRecorder()
	h.handlePasswordLogin(first, passwordRequest(`{"password":"wrong"}`))
	if first.Code != http.StatusUnauthorized || first.Body.String() != "login failed\n" {
		t.Fatalf("first refusal = %d %q", first.Code, first.Body.String())
	}
	second := httptest.NewRecorder()
	h.handlePasswordLogin(second, passwordRequest(`{"password":"wrong"}`))
	if second.Code != http.StatusTooManyRequests || second.Body.String() != "login failed\n" || second.Header().Get("Retry-After") == "" {
		t.Fatalf("backoff refusal = %d %q retry=%q", second.Code, second.Body.String(), second.Header().Get("Retry-After"))
	}
	other := passwordRequest(`{"password":"correct horse battery staple"}`)
	other.RemoteAddr = "192.0.2.11:40000"
	rec := httptest.NewRecorder()
	h.handlePasswordLogin(rec, other)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("independent source = %d: %s", rec.Code, rec.Body.String())
	}
}

func TestPasswordLoginBoundsAndValidatesBody(t *testing.T) {
	for name, body := range map[string]string{
		"unknown field": `{"password":"x","operator":"known"}`,
		"oversized":     `{"password":"` + strings.Repeat("x", maxPasswordBody) + `"}`,
		"missing":       `{}`,
	} {
		t.Run(name, func(t *testing.T) {
			h := newPasswordHandler(t, time.Now)
			req := passwordRequest(body)
			req.RemoteAddr = name + ":40000"
			rec := httptest.NewRecorder()
			h.handlePasswordLogin(rec, req)
			if rec.Code != http.StatusUnauthorized || !bytes.Equal(rec.Body.Bytes(), []byte("login failed\n")) {
				t.Fatalf("response = %d %q", rec.Code, rec.Body.String())
			}
		})
	}
}
