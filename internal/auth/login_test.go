package auth

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/rs/zerolog"
	"time"
)

func TestStreamDeadlineComesFromVerifiedRequestCookie(t *testing.T) {
	h := builtinForMiddleware(t)
	if _, ok := SessionExpires(context.Background()); ok {
		t.Fatal("missing authentication acquired a deadline")
	}
	identity := Login{Issuer: "https://accounts.google.com", Subject: "operator-subject", Email: "operator@example.com"}
	token, err := h.issueLoginSession(h.now(), h.now(), identity)
	if err != nil {
		t.Fatal(err)
	}
	want := h.now().Add(SessionDuration).Truncate(time.Second)
	// Renewal may send a later cookie, but cannot extend the cookie already
	// admitted on this request or an existing stream opened with that cookie.
	later := NewLater(t, h, 2*time.Hour)
	called := false
	handler := later.Require(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		called = true
		expires, ok := SessionExpires(r.Context())
		if !ok || !expires.Equal(want) {
			t.Fatalf("stream deadline = %v, want %v", expires, want)
		}
	}))
	r := httptest.NewRequest(http.MethodGet, "/api/events", nil)
	r.AddCookie(&http.Cookie{Name: h.sessionCookieName(), Value: token})
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, r)
	if !called || len(recorder.Result().Cookies()) == 0 {
		t.Fatal("fixture must admit the request and renew its cookie")
	}
	called = false
	r = httptest.NewRequest(http.MethodGet, "/api/events", nil)
	r.AddCookie(&http.Cookie{Name: h.sessionCookieName(), Value: token + "tampered"})
	handler.ServeHTTP(httptest.NewRecorder(), r)
	if called {
		t.Fatal("tampered cookie reached the stream handler")
	}
}

func TestBrowserLoginIdentitySurvivesRenewal(t *testing.T) {
	h := builtinForMiddleware(t)
	identity := Login{Issuer: "https://accounts.google.com", Subject: "operator-subject", Email: "operator@example.com"}
	first, err := h.issueLoginSession(h.now(), h.now(), identity)
	if err != nil {
		t.Fatal(err)
	}
	second, err := h.issueLoginSession(h.now(), h.now(), identity)
	if err != nil {
		t.Fatal(err)
	}
	firstPayload, err := h.verifySessionValue(first, h.now())
	if err != nil {
		t.Fatal(err)
	}
	firstLogin, ok := loginFromPayload(firstPayload)
	if !ok || len(firstLogin.ID) != 64 || firstLogin.Issuer != identity.Issuer || firstLogin.Subject != identity.Subject || firstLogin.Email != identity.Email {
		t.Fatal("login did not retain its verified identity and opaque ID")
	}
	secondPayload, err := h.verifySessionValue(second, h.now())
	if err != nil {
		t.Fatal(err)
	}
	secondLogin, ok := loginFromPayload(secondPayload)
	if !ok || firstLogin.ID == secondLogin.ID {
		t.Fatal("independent logins share a browser ID")
	}
	later := NewLater(t, h, 2*time.Hour)
	replacement, renewedPayload, err := later.sessionRenewal(firstPayload, later.now())
	if err != nil || replacement == "" || replacement == first {
		t.Fatalf("aging login did not renew: %v", err)
	}
	renewedLogin, ok := loginFromPayload(renewedPayload)
	if !ok || renewedLogin != firstLogin {
		t.Fatal("renewal changed the browser login identity")
	}
	for _, token := range []string{first, replacement} {
		request := httptest.NewRequest(http.MethodGet, "/api/x", nil)
		request.AddCookie(&http.Cookie{Name: h.sessionCookieName(), Value: token})
		request.Header.Set("X-Session-ID", secondLogin.ID)
		called := false
		later.Require(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			called = true
			got, ok := CurrentLogin(r.Context())
			if !ok || got != firstLogin {
				t.Error("middleware lost or accepted a supplied browser identity")
			}
			if Principal(r.Context()) != principalHash("operator") {
				t.Error("browser identity changed the existing principal")
			}
		})).ServeHTTP(httptest.NewRecorder(), request)
		if !called {
			t.Fatal("verified browser request was refused")
		}
	}
}

func TestLegacySessionsDoNotInventBrowserOrGoogleIdentity(t *testing.T) {
	h := builtinForMiddleware(t)
	expiry := strconv.FormatInt(h.now().Add(24*time.Hour).Unix(), 10)
	for _, payload := range []string{
		"v1:" + expiry + ":00",
		"v2:" + strconv.FormatInt(h.now().Unix(), 10) + ":" + expiry + ":00",
	} {
		r := httptest.NewRequest(http.MethodGet, "/api/x", nil)
		r.AddCookie(&http.Cookie{Name: h.sessionCookieName(), Value: encodeSigned(payload, h.cfg.SessionSecret)})
		called := false
		h.Require(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			called = true
			if _, ok := CurrentLogin(r.Context()); ok {
				t.Error("legacy cookie acquired an identity never recorded at login")
			}
		})).ServeHTTP(httptest.NewRecorder(), r)
		if !called {
			t.Fatal("legacy authentication stopped working")
		}
	}
}

func TestPasswordLoginHasNoOIDCIdentity(t *testing.T) {
	h := builtinForMiddleware(t)
	value, err := h.issueSessionValue(h.now())
	if err != nil {
		t.Fatal(err)
	}
	payload, err := h.verifySessionValue(value, h.now())
	if err != nil {
		t.Fatal(err)
	}
	login, ok := loginFromPayload(payload)
	if !ok || login.ID == "" || login.Issuer != "" || login.Subject != "" || login.Email != "" {
		t.Fatal("password login must identify the browser without claiming an OIDC ceremony")
	}
}

func TestBrowserLogoutUsesOnlyTheAuthenticatedLogin(t *testing.T) {
	h := builtinForMiddleware(t)
	var revoked []Login
	h.OnBrowserLogout(func(login Login) { revoked = append(revoked, login) })
	first, csrf := h.mintSession(t)
	second, _ := h.mintSession(t)
	payload, err := h.verifySessionValue(first.Value, h.now())
	if err != nil {
		t.Fatal(err)
	}
	want, _ := loginFromPayload(payload)
	for _, token := range []string{"", csrf} {
		r := httptest.NewRequest(http.MethodPost, "/auth/logout", nil)
		r.AddCookie(first)
		r.Header.Set(CSRFHeader, token)
		rec := httptest.NewRecorder()
		h.handleLogout(rec, r)
		if token == "" && (rec.Code != http.StatusForbidden || len(revoked) != 0) {
			t.Fatal("logout without CSRF invoked browser revocation")
		}
		if token != "" && rec.Code != http.StatusNoContent {
			t.Fatalf("verified logout failed: %d", rec.Code)
		}
	}
	if len(revoked) != 1 || revoked[0] != want {
		t.Fatal("logout did not target exactly the authenticated browser login")
	}
	if _, err := h.verifySessionValue(second.Value, h.now()); err != nil {
		t.Fatal("another browser's login was invalidated")
	}
}

func TestLogoutRevokesBrowserWhenCallbackCrossesSessionExpiry(t *testing.T) {
	h := builtinForMiddleware(t)
	now := h.now()
	h.now = func() time.Time { return now }
	cookie, csrf := h.mintSession(t)
	payload, err := h.verifySessionValue(cookie.Value, now)
	if err != nil {
		t.Fatal(err)
	}
	want, ok := loginFromPayload(payload)
	if !ok {
		t.Fatal("fixture has no browser login")
	}
	called := 0
	h.OnLogout(func(string) { now = now.Add(SessionDuration + time.Second) })
	h.OnBrowserLogout(func(login Login) {
		called++
		if login != want {
			t.Error("revocation lost the identity verified at request admission")
		}
	})
	r := httptest.NewRequest(http.MethodPost, "/auth/logout", nil)
	r.AddCookie(cookie)
	r.Header.Set(CSRFHeader, csrf)
	w := httptest.NewRecorder()
	h.handleLogout(w, r)
	if w.Code != http.StatusNoContent || called != 1 {
		t.Fatalf("status=%d browser revocations=%d", w.Code, called)
	}
	if _, err := h.verifySessionValue(cookie.Value, now); err == nil {
		t.Fatal("fixture did not cross session expiry")
	}
}

func TestMalformedBrowserLoginCookiesAreRefused(t *testing.T) {
	h := builtinForMiddleware(t)
	value, err := h.issueLoginSession(h.now(), h.now(), Login{
		Issuer: "https://accounts.google.com", Subject: "operator-subject", Email: "operator@example.com",
	})
	if err != nil {
		t.Fatal(err)
	}
	payload, err := h.verifySessionValue(value, h.now())
	if err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func([]string){
		"short ID":              func(p []string) { p[3] = "00" },
		"non-hex ID":            func(p []string) { p[3] = strings.Repeat("z", 64) },
		"bad encoding":          func(p []string) { p[4] = "!" },
		"missing issuer":        func(p []string) { p[4] = "" },
		"missing subject":       func(p []string) { p[5] = "" },
		"missing email":         func(p []string) { p[6] = "" },
		"non-UTF8 email":        func(p []string) { p[6] = base64.RawURLEncoding.EncodeToString([]byte{255}) },
		"oversized claim":       func(p []string) { p[6] = base64.RawURLEncoding.EncodeToString([]byte(strings.Repeat("x", 1025))) },
		"future authentication": func(p []string) { p[1] = strconv.FormatInt(h.now().Add(time.Hour).Unix(), 10) },
		"expiry beyond cap":     func(p []string) { p[2] = strconv.FormatInt(h.now().Add(SessionMaxDuration+time.Second).Unix(), 10) },
	} {
		t.Run(name, func(t *testing.T) {
			parts := strings.Split(payload, ":")
			change(parts)
			malformed := encodeSigned(strings.Join(parts, ":"), h.cfg.SessionSecret)
			if _, err := h.verifySessionValue(malformed, h.now()); err == nil {
				t.Fatal("malformed signed browser identity accepted")
			}
		})
	}
}

// epochStoreSpy records what logout persistence saw, and can fail, so the
// tests can hold both halves of the best-effort contract.
type epochStoreSpy struct {
	loaded  map[string]uint64
	saved   map[string]uint64
	loadErr error
	saveErr error
}

func (s *epochStoreSpy) LoadLogoutEpochs(context.Context) (map[string]uint64, error) {
	if s.loadErr != nil {
		return nil, s.loadErr
	}
	return s.loaded, nil
}

func (s *epochStoreSpy) SaveLogoutEpoch(_ context.Context, loginID string, epoch uint64) error {
	if s.saveErr != nil {
		return s.saveErr
	}
	if s.saved == nil {
		s.saved = map[string]uint64{}
	}
	s.saved[loginID] = epoch
	return nil
}

// The logout/renewal race: a renewal response generated before logout
// carries the pre-logout epoch, and once logout has bumped it, that cookie
// verifies stale no matter when the browser applies it.
func TestLogoutMakesInFlightRenewalsStale(t *testing.T) {
	h := builtinForMiddleware(t)
	first, csrf := h.mintSession(t)
	// The renewal that was already in flight: issued before logout, so its
	// epoch is the pre-logout one. Minting a second token for the SAME
	// login is exactly what a renewal is.
	renewed := renewForLogin(t, h, first.Value)
	other, _ := h.mintSession(t)
	r := httptest.NewRequest(http.MethodPost, "/auth/logout", nil)
	r.AddCookie(first)
	r.Header.Set(CSRFHeader, csrf)
	h.handleLogout(httptest.NewRecorder(), r)
	for name, token := range map[string]string{"the logged-out login": renewed, "its original cookie": first.Value} {
		if _, err := h.verifySessionValue(token, h.now()); err == nil {
			t.Fatalf("%s still verifies after logout", name)
		}
	}
	if _, err := h.verifySessionValue(other.Value, h.now()); err != nil {
		t.Fatal("another browser's login was invalidated by this logout")
	}
}

// A persisted epoch binds a restarted process: the store loads at New and
// the logout's write is best effort with the in-memory epoch immediate.
func TestLogoutEpochPersistence(t *testing.T) {
	store := &epochStoreSpy{}
	h, err := New(Config{
		Mode:          "builtin",
		ClientID:      "client-id",
		ClientSecret:  "s",
		RedirectURL:   "http://127.0.0.1:8837/auth/callback",
		AllowedEmail:  "operator@example.com",
		SessionSecret: []byte("0123456789abcdef0123456789abcdef"),
		JWKSURL:       "http://127.0.0.1:1/certs",
		EpochStore:    store,
	}, zerolog.Nop())
	if err != nil {
		t.Fatal(err)
	}
	token, csrf := h.mintSession(t)
	r := httptest.NewRequest(http.MethodPost, "/auth/logout", nil)
	r.AddCookie(token)
	r.Header.Set(CSRFHeader, csrf)
	h.handleLogout(httptest.NewRecorder(), r)
	if len(store.saved) != 1 {
		t.Fatalf("logout persisted %d epochs, want the logging-in browser's one", len(store.saved))
	}
	// A process restarted on the same store rejects the pre-logout cookie.
	restarted, err := New(Config{
		Mode:          "builtin",
		ClientID:      "client-id",
		ClientSecret:  "s",
		RedirectURL:   "http://127.0.0.1:8837/auth/callback",
		AllowedEmail:  "operator@example.com",
		SessionSecret: h.cfg.SessionSecret,
		JWKSURL:       "http://127.0.0.1:1/certs",
		EpochStore:    &epochStoreSpy{loaded: store.saved},
	}, zerolog.Nop())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := restarted.verifySessionValue(token.Value, restarted.now()); err == nil {
		t.Fatal("a persisted logout epoch did not bind the restarted process")
	}
}

// renewForLogin mints a fresh token for the same login identity as value,
// which is byte-for-byte what a renewal in flight produced.
func renewForLogin(t *testing.T, h *Handler, value string) string {
	t.Helper()
	payload, err := h.verifySessionValue(value, h.now())
	if err != nil {
		t.Fatal(err)
	}
	login, ok := loginFromPayload(payload)
	if !ok {
		t.Fatal("fixture cookie carries no login")
	}
	renewed, err := h.issueLoginSession(h.now(), h.now(), login)
	if err != nil {
		t.Fatal(err)
	}
	return renewed
}

// The verify/logout/issue ordering: Require verifies an aged cookie, and a
// logout can land between that verify and the renewal the same request
// issues. The renewal must inherit the cookie's pre-logout epoch and fail
// its own verification, never revive the login with the post-logout
// generation.
func TestRenewalAfterLogoutCannotReviveTheLogin(t *testing.T) {
	h := builtinForMiddleware(t)
	token, csrf := h.mintSession(t)
	// Age the cookie past the renewal threshold: only an aged session
	// renews, so this is the shape the race actually rides.
	later := NewLater(t, h, 2*time.Hour)
	payload, err := later.verifySessionValue(token.Value, later.now())
	if err != nil {
		t.Fatal(err)
	}
	if value, _, renewErr := later.sessionRenewal(payload, later.now()); renewErr != nil || value == "" {
		t.Fatal("fixture must be old enough to renew before the logout")
	}
	// Logout lands between verify and renewal.
	r := httptest.NewRequest(http.MethodPost, "/auth/logout", nil)
	r.AddCookie(token)
	r.Header.Set(CSRFHeader, csrf)
	later.handleLogout(httptest.NewRecorder(), r)
	value, _, renewErr := later.sessionRenewal(payload, later.now())
	if renewErr == nil && value != "" {
		if _, err := later.verifySessionValue(value, later.now()); err == nil {
			t.Fatal("renewal minted after logout revived the logged-out login")
		}
	}
	if _, err := later.verifySessionValue(token.Value, later.now()); err == nil {
		t.Fatal("the original cookie still verifies after logout")
	}
}

// A legacy (v1/v2) logout bumps the empty-ID epoch. A fresh login after it
// gets its own new ID and epoch zero, so its own logout must leave its
// cookie stale - reading the empty-ID epoch at issue would stamp 1 under
// the new ID, and that login's first logout (0 -> 1) could never reject it.
func TestLegacyLogoutDoesNotPoisonFreshLogins(t *testing.T) {
	h := builtinForMiddleware(t)
	// A legacy (v1/v2) browser has no login identity, so its logout bumps
	// the empty-ID epoch; bumping it directly is that logout's whole effect
	// on the epoch store.
	h.bumpLogoutEpoch("")
	// A fresh login after the legacy logout.
	fresh, freshCsrf := h.mintSession(t)
	if _, err := h.verifySessionValue(fresh.Value, h.now()); err != nil {
		t.Fatalf("fresh login does not verify after a legacy logout: %v", err)
	}
	r2 := httptest.NewRequest(http.MethodPost, "/auth/logout", nil)
	r2.AddCookie(fresh)
	r2.Header.Set(CSRFHeader, freshCsrf)
	h.handleLogout(httptest.NewRecorder(), r2)
	if _, err := h.verifySessionValue(fresh.Value, h.now()); err == nil {
		t.Fatal("the fresh login's own logout did not stale its cookie")
	}
}
