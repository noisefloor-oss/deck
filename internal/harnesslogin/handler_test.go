package harnesslogin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/noisefloor-oss/deck/internal/auth"
	"github.com/rs/zerolog"
	"golang.org/x/crypto/bcrypt"
)

func TestAuthenticatedCSRFProtectedPost(t *testing.T) {
	service, rt, _ := fixture(t, "pi")
	h := NewHandler(service, []string{"https://deck.example"})
	authn, err := auth.New(auth.Config{Mode: "trusted_proxy", ProxyAddresses: []string{"127.0.0.1"}, IdentityHeader: "X-Identity"}, zerolog.Nop())
	if err != nil {
		t.Fatal(err)
	}
	handler := authn.Require(h)
	call := func(method, principal, origin, csrf, contentType, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, "/api/harness-login?session=session-a", strings.NewReader(body))
		req.RemoteAddr = "127.0.0.1:30000"
		req.Header.Set("X-Identity", principal)
		req.Header.Set("Origin", origin)
		req.Header.Set(CSRFHeader, csrf)
		req.Header.Set("Content-Type", contentType)
		out := httptest.NewRecorder()
		handler.ServeHTTP(out, req)
		return out
	}
	out := call("GET", "user-a", "", "", "", "")
	var page struct {
		CSRF string `json:"csrf"`
	}
	if json.Unmarshal(out.Body.Bytes(), &page) != nil || page.CSRF == "" {
		t.Fatal("missing authenticated CSRF token")
	}
	if out.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("sensitive response cacheable")
	}
	for _, c := range []struct {
		name, principal, origin, csrf, media string
		status                               int
	}{
		{"unauthenticated", "", "https://deck.example", page.CSRF, "application/json", 401},
		{"missing-origin", "user-a", "", page.CSRF, "application/json", 403},
		{"foreign-origin", "user-a", "https://evil.invalid", page.CSRF, "application/json", 403},
		{"missing-csrf", "user-a", "https://deck.example", "", "application/json", 403},
		{"other-user", "user-b", "https://deck.example", page.CSRF, "application/json", 403},
		{"simple-form", "user-a", "https://deck.example", page.CSRF, "text/plain", 403},
	} {
		t.Run(c.name, func(t *testing.T) {
			out := call("POST", c.principal, c.origin, c.csrf, c.media, `{"action":"submit","callback":"secret-canary"}`)
			if out.Code != c.status || strings.Contains(out.Body.String(), "secret-canary") {
				t.Fatalf("status=%d body=%s", out.Code, out.Body.String())
			}
		})
	}
	for _, body := range []string{`{"action":"begin","unknown":"secret-canary"}`, `{secret-canary`, strings.Repeat("secret-canary", MaxURLBytes)} {
		out := call("POST", "user-a", "https://deck.example", page.CSRF, "application/json", body)
		if out.Code != 400 || strings.Contains(out.Body.String(), "secret-canary") {
			t.Fatal("invalid request was accepted or echoed")
		}
	}
	if rt.dials != 0 {
		t.Fatal("rejected browser request reached a CLI")
	}
}

func TestPasswordAuthenticatedDeliveryAndBothCSRFChecks(t *testing.T) {
	service, rt, _ := fixture(t, "pi")
	if err := service.Register(context.Background(), 124, "pi", authURL("next-state")); err != nil {
		t.Fatal(err)
	}
	hash, _ := bcrypt.GenerateFromPassword([]byte("fixture-password"), bcrypt.MinCost)
	a, err := auth.New(auth.Config{Mode: "password", PasswordHash: hash, SessionSecret: []byte("0123456789abcdef0123456789abcdef")}, zerolog.Nop())
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	a.Mount(mux.Handle)
	mux.Handle("/api/harness-login", a.Require(NewHandler(service, []string{"https://deck.example"})))
	login := httptest.NewRequest("POST", "/auth/password", strings.NewReader(`{"password":"fixture-password"}`))
	login.Header.Set("Content-Type", "application/json")
	loginResult := httptest.NewRecorder()
	mux.ServeHTTP(loginResult, login)
	if loginResult.Code != 204 {
		t.Fatal("fixture Deck password login failed")
	}
	cookies := loginResult.Result().Cookies()
	cookieCSRF := ""
	for _, cookie := range cookies {
		if strings.HasSuffix(cookie.Name, "_csrf") {
			cookieCSRF = cookie.Value
		}
	}
	call := func(method, body, csrf, browserCSRF string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "/api/harness-login?session=session-a", strings.NewReader(body))
		for _, cookie := range cookies {
			r.AddCookie(cookie)
		}
		r.Header.Set("Origin", "https://deck.example")
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set(CSRFHeader, csrf)
		r.Header.Set(auth.CSRFHeader, browserCSRF)
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		return w
	}
	var page struct {
		Pending []Pending `json:"pending"`
		CSRF    string    `json:"csrf"`
	}
	if err := json.Unmarshal(call("GET", "", "", "").Body.Bytes(), &page); err != nil || len(page.Pending) != 1 {
		t.Fatal("missing pending attempt")
	}
	body, _ := json.Marshal(map[string]string{"action": "begin", "session": "session-a", "registration": page.Pending[0].ID})
	for _, pair := range [][2]string{{"", cookieCSRF}, {page.CSRF, ""}} {
		if call("POST", string(body), pair[0], pair[1]).Code != 403 {
			t.Fatal("missing CSRF accepted")
		}
	}
	response := call("POST", string(body), page.CSRF, cookieCSRF)
	var handle Handle
	if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &handle) != nil || handle.ID == "" {
		t.Fatal("begin failed")
	}
	rt.responses = []string{"HTTP/1.1 200 OK\r\nContent-Length: 13\r\nSet-Cookie: native=secret-canary\r\n\r\nsecret-canary"}
	body, _ = json.Marshal(map[string]string{"action": "submit", "handle": handle.ID, "callback": strings.Replace(callbackURL, "native-state", "next-state", 1)})
	response = call("POST", string(body), page.CSRF, cookieCSRF)
	if response.Code != 200 || response.Body.String() != "{\"status\":\"delivered\"}\n" || response.Header().Get("Set-Cookie") != "" {
		t.Fatal("native response escaped or delivery failed")
	}
	if rt.requests[0].Header.Get("Cookie") != "" {
		t.Fatal("Deck cookies reached native listener")
	}
	if call("POST", string(body), page.CSRF, cookieCSRF).Code != 409 {
		t.Fatal("HTTP replay accepted")
	}
}

func TestOnlyConfiguredProxyUserCanClaimNativeAttempt(t *testing.T) {
	s, _, _ := fixture(t, "pi")
	h := NewHandler(s, []string{"https://deck.example"})
	h.User = auth.ProxyPrincipal("instance-owner")
	a, err := auth.New(auth.Config{Mode: "trusted_proxy", ProxyAddresses: []string{"127.0.0.1"}, IdentityHeader: "X-Identity"}, zerolog.Nop())
	if err != nil {
		t.Fatal(err)
	}
	for _, method := range []string{"GET", "POST"} {
		r := httptest.NewRequest(method, "/api/harness-login", nil)
		r.RemoteAddr = "127.0.0.1:30000"
		r.Header.Set("X-Identity", "another-user")
		w := httptest.NewRecorder()
		a.Require(h).ServeHTTP(w, r)
		if w.Code != 403 {
			t.Fatal("another proxy identity could reach this instance's native login")
		}
	}
}
func TestAuthenticationDisabledCannotUseHandoff(t *testing.T) {
	h := NewHandler(nil, nil)
	a, err := auth.New(auth.Config{Mode: "none"}, zerolog.Nop())
	if err != nil {
		t.Fatal(err)
	}
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		w := httptest.NewRecorder()
		a.Require(h).ServeHTTP(w, httptest.NewRequest(method, "/api/harness-login", nil))
		if w.Code != 401 {
			t.Fatal(w.Code)
		}
	}
}

func TestDismissLifecycleOverHTTP(t *testing.T) {
	service, rt, _ := fixture(t, "pi")
	if err := service.Register(context.Background(), 124, "pi", authURL("next-state")); err != nil {
		t.Fatal(err)
	}
	a, err := auth.New(auth.Config{Mode: "trusted_proxy", ProxyAddresses: []string{"127.0.0.1"}, IdentityHeader: "X-Identity"}, zerolog.Nop())
	if err != nil {
		t.Fatal(err)
	}
	handler := a.Require(NewHandler(service, []string{"https://deck.example"}))
	call := func(method, body, csrf string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, "/api/harness-login?session=session-a", strings.NewReader(body))
		req.RemoteAddr = "127.0.0.1:30000"
		req.Header.Set("X-Identity", "user-a")
		if method == http.MethodPost {
			req.Header.Set("Origin", "https://deck.example")
			req.Header.Set(CSRFHeader, csrf)
			req.Header.Set("Content-Type", "application/json")
		}
		out := httptest.NewRecorder()
		handler.ServeHTTP(out, req)
		return out
	}
	var page struct {
		Pending []Pending `json:"pending"`
		CSRF    string    `json:"csrf"`
	}
	if json.Unmarshal(call(http.MethodGet, "", "").Body.Bytes(), &page) != nil || len(page.Pending) != 1 || page.CSRF == "" {
		t.Fatal("missing pending attempt")
	}
	beginBody, _ := json.Marshal(map[string]string{"action": "begin", "session": "session-a", "registration": page.Pending[0].ID})
	out := call(http.MethodPost, string(beginBody), page.CSRF)
	var handle Handle
	if out.Code != 200 || json.Unmarshal(out.Body.Bytes(), &handle) != nil || handle.ID == "" {
		t.Fatal("begin failed")
	}
	dismissBody, _ := json.Marshal(map[string]string{"action": "dismiss", "handle": handle.ID})
	out = call(http.MethodPost, string(dismissBody), page.CSRF)
	if out.Code != 200 || out.Body.String() != "{\"status\":\"dismissed\"}\n" {
		t.Fatalf("dismiss failed: %d %s", out.Code, out.Body.String())
	}
	// The same native attempt is listed again and a reopened selector reclaims
	// it through a fresh handle.
	if json.Unmarshal(call(http.MethodGet, "", "").Body.Bytes(), &page) != nil || len(page.Pending) != 1 {
		t.Fatal("dismissed attempt not pending again")
	}
	out = call(http.MethodPost, string(beginBody), page.CSRF)
	var resumed Handle
	if out.Code != 200 || json.Unmarshal(out.Body.Bytes(), &resumed) != nil || resumed.ID == "" || resumed.ID == handle.ID {
		t.Fatal("reclaimed handle missing or recycled")
	}
	// The released handle stays consumed: replayed dismissal is refused.
	if out := call(http.MethodPost, string(dismissBody), page.CSRF); out.Code != 409 {
		t.Fatalf("released handle dismissed again: %d", out.Code)
	}
	if rt.dials != 0 {
		t.Fatal("dismissal reached a CLI")
	}
}
