package harnesslogin

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/noisefloor-oss/deck/internal/auth"
	"github.com/rs/zerolog"
)

func TestRegistrationNotificationsAreSessionScopedAndCoalesced(t *testing.T) {
	s, _, _ := fixture(t, "codex")
	first, cancelFirst := s.subscribe("session-a")
	second, cancelSecond := s.subscribe("session-a")
	other, cancelOther := s.subscribe("session-b")
	defer cancelFirst()
	defer cancelSecond()
	defer cancelOther()
	for _, state := range []string{"next-state", "later-state"} {
		if err := s.Register(context.Background(), 124, "codex", authURL(state)); err != nil {
			t.Fatal(err)
		}
	}
	for _, ch := range []<-chan struct{}{first, second} {
		select {
		case <-ch:
		default:
			t.Fatal("successful registration did not notify subscriber")
		}
		select {
		case <-ch:
			t.Fatal("notifications were not coalesced")
		default:
		}
	}
	select {
	case <-other:
		t.Fatal("registration notified a different session")
	default:
	}
	for _, attempt := range []struct{ provider, url string }{
		{"codex", authURL("later-state")},
		{"codex", "invalid"},
		{"pi", authURL("wrong-provider")},
	} {
		if err := s.Register(context.Background(), 124, attempt.provider, attempt.url); err == nil {
			t.Fatal("invalid registration succeeded")
		}
		select {
		case <-first:
			t.Fatal("rejected registration sent a notification")
		default:
		}
	}
	cancelFirst()
	cancelFirst()
	if err := s.Register(context.Background(), 124, "codex", authURL("after-unsubscribe")); err != nil {
		t.Fatal(err)
	}
	select {
	case <-first:
		t.Fatal("cancelled subscriber received a notification")
	default:
	}
	select {
	case <-second:
	default:
		t.Fatal("cancelling one subscriber removed another")
	}
	cancelSecond()
	cancelOther()
	if len(s.subscribers) != 0 {
		t.Fatal("subscriptions survived cancellation")
	}
}

func authenticatedNotificationHandler(t *testing.T, h *Handler) http.Handler {
	t.Helper()
	a, err := auth.New(auth.Config{Mode: "trusted_proxy", ProxyAddresses: []string{"127.0.0.1"}, IdentityHeader: "X-Identity"}, zerolog.Nop())
	if err != nil {
		t.Fatal(err)
	}
	return a.Require(h)
}

func TestNotificationStreamRefreshesImmediatelyAndCleansUp(t *testing.T) {
	s, _, _ := fixture(t, "codex")
	h := NewHandler(s, []string{"https://deck.example"})
	h.User = auth.ProxyPrincipal("user-a")
	wrapped := authenticatedNotificationHandler(t, h)
	done := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(done)
		wrapped.ServeHTTP(w, r)
	}))
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", server.URL+"/api/harness-login?session=session-a&watch=1", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Identity", "user-a")
	req.Header.Set("Origin", "https://deck.example")
	client := server.Client()
	client.Timeout = 5 * time.Second
	response, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != 200 || response.Header.Get("Content-Type") != "text/event-stream" || response.Header.Get("Cache-Control") != "no-store" || response.Header.Get("Referrer-Policy") != "no-referrer" || response.Header.Get("X-Accel-Buffering") != "no" {
		t.Fatal("notification response missing stream or privacy headers")
	}
	reader := bufio.NewReader(response.Body)
	readEvent := func() {
		t.Helper()
		for _, want := range []string{"event: pending\n", "data: {}\n", "\n"} {
			line, err := reader.ReadString('\n')
			if err != nil || line != want {
				t.Fatal("missing content-free pending event")
			}
		}
	}
	readEvent()
	if err := s.Register(context.Background(), 124, "codex", authURL("notification-secret-canary")); err != nil {
		t.Fatal(err)
	}
	readEvent()
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("notification handler survived client cancellation")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.subscribers) != 0 {
		t.Fatal("disconnected stream left a subscriber")
	}
}

func TestNotificationStreamRetainsAuthenticationAndOriginGates(t *testing.T) {
	for _, tc := range []struct {
		name, principal, origin, fetchSite, session string
		unavailable                                 bool
		status                                      int
	}{
		{name: "unauthenticated", session: "session-a", status: 401},
		{name: "other user", principal: "user-b", session: "session-a", status: 403},
		{name: "foreign origin", principal: "user-a", origin: "https://evil.invalid", session: "session-a", status: 403},
		{name: "cross-site", principal: "user-a", fetchSite: "cross-site", session: "session-a", status: 403},
		{name: "missing session", principal: "user-a", status: 400},
		{name: "unavailable", principal: "user-a", session: "session-a", unavailable: true, status: 503},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _, _ := fixture(t, "codex")
			if tc.unavailable {
				s = nil
			}
			h := NewHandler(s, []string{"https://deck.example"})
			h.User = auth.ProxyPrincipal("user-a")
			req := httptest.NewRequest("GET", "/api/harness-login?watch=1&session="+tc.session, nil)
			req.RemoteAddr = "127.0.0.1:30000"
			req.Header.Set("X-Identity", tc.principal)
			req.Header.Set("Origin", tc.origin)
			req.Header.Set("Sec-Fetch-Site", tc.fetchSite)
			out := httptest.NewRecorder()
			authenticatedNotificationHandler(t, h).ServeHTTP(out, req)
			if out.Code != tc.status {
				t.Fatalf("status = %d, want %d", out.Code, tc.status)
			}
		})
	}
}

func TestNotificationStreamAcceptsSameOriginEventSourceWithoutOrigin(t *testing.T) {
	s, _, _ := fixture(t, "codex")
	h := NewHandler(s, []string{"https://deck.example"})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := httptest.NewRequest("GET", "/api/harness-login?watch=1&session=session-a", nil).WithContext(ctx)
	req.RemoteAddr = "127.0.0.1:30000"
	req.Header.Set("X-Identity", "user-a")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	out := httptest.NewRecorder()
	authenticatedNotificationHandler(t, h).ServeHTTP(out, req)
	if out.Code != 200 || out.Body.String() != "event: pending\ndata: {}\n\n" {
		t.Fatal("same-origin EventSource did not receive its initial refresh")
	}
	if len(s.subscribers) != 0 {
		t.Fatal("cancelled request retained a subscription")
	}
}
