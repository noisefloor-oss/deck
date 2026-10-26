package harnesslogin

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type fakeRuntime struct {
	binding   Binding
	alive     bool
	mu        sync.Mutex
	requests  []*http.Request
	responses []string
	dials     int
	dialError bool
}

func (f *fakeRuntime) Capture(context.Context, int, int) (Binding, error) { return f.binding, nil }
func (f *fakeRuntime) Alive(_ context.Context, b Binding) bool            { return f.alive && b == f.binding }
func (f *fakeRuntime) Dial(ctx context.Context, b Binding, port int) (net.Conn, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.dials++
	if f.dialError || !f.Alive(ctx, b) {
		return nil, fmt.Errorf("private transport error: secret-canary")
	}
	response := "HTTP/1.1 200 OK\r\nContent-Length: 0\r\n\r\n"
	if len(f.responses) > 0 {
		response = f.responses[0]
		f.responses = f.responses[1:]
	}
	client, server := net.Pipe()
	go func() {
		defer server.Close()
		req, err := http.ReadRequest(bufio.NewReader(server))
		if err != nil {
			return
		}
		f.mu.Lock()
		f.requests = append(f.requests, req)
		f.mu.Unlock()
		_, _ = io.WriteString(server, response)
	}()
	return client, nil
}
func authURL(state string) string {
	q := url.Values{"response_type": {"code"}, "state": {state}, "code_challenge": {"challenge"}, "code_challenge_method": {"S256"}, "redirect_uri": {"http://localhost:1455/auth/callback"}}
	return "https://auth.openai.com/oauth/authorize?" + q.Encode()
}
func fixture(t *testing.T, provider string) (*Service, *fakeRuntime, Handle) {
	t.Helper()
	f := &fakeRuntime{binding: Binding{Session: "session-a", Generation: 1, Provider: provider, PID: 123, StartTicks: 456, Listener: 789, Network: "network-a"}, alive: true}
	s := New(f)
	if err := s.Register(context.Background(), 124, provider, authURL("native-state")); err != nil {
		t.Fatal(err)
	}
	pending := s.Pending(context.Background(), "operator-a", "session-a")
	if len(pending) != 1 {
		t.Fatal(pending)
	}
	handle, err := s.Begin(context.Background(), "operator-a", "session-a", pending[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	return s, f, handle
}

const callbackURL = "http://localhost:1455/auth/callback?state=native-state&code=callback-canary"

func TestCallbackDeliveryDoesNotExposeNativeResponse(t *testing.T) {
	s, f, h := fixture(t, "pi")
	f.responses = []string{"HTTP/1.1 200 OK\r\nContent-Length: 13\r\nSet-Cookie: token=secret-canary\r\n\r\nsecret-canary"}
	status, err := s.Submit(context.Background(), "operator-a", h.ID, callbackURL)
	if err != nil || status != "delivered" {
		t.Fatalf("status=%q err=%v", status, err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.requests) != 1 {
		t.Fatal(len(f.requests))
	}
	req := f.requests[0]
	if req.Host != "localhost:1455" || req.Header.Get("Cookie") != "" || req.Header.Get("Authorization") != "" || req.Header.Get("Referer") != "" {
		t.Fatal("unexpected upstream headers")
	}
}
func TestCodexCompletesOnlyFixedSuccessPath(t *testing.T) {
	s, f, h := fixture(t, "codex")
	f.responses = []string{"HTTP/1.1 302 Found\r\nLocation: http://localhost:1455/success?id_token=secret-canary\r\nContent-Length: 0\r\n\r\n", "HTTP/1.1 200 OK\r\nContent-Length: 13\r\n\r\nsecret-canary"}
	status, err := s.Submit(context.Background(), "operator-a", h.ID, callbackURL)
	if err != nil || status != "completed" {
		t.Fatalf("status=%q err=%v", status, err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.requests) != 2 || f.requests[1].URL.RequestURI() != "/success" {
		t.Fatal("native redirect was not reconstructed safely")
	}
}
func TestUnreviewedNativeRedirectNeverFollowed(t *testing.T) {
	for _, location := range []string{"https://example.com/?token=secret-canary", "http://localhost:1455/cancel", "http://127.0.0.1:1455/success", "http://localhost:1455@evil.invalid/success", "http://localhost:1455/success#secret-canary"} {
		t.Run(location, func(t *testing.T) {
			s, f, h := fixture(t, "codex")
			f.responses = []string{"HTTP/1.1 302 Found\r\nLocation: " + location + "\r\nContent-Length: 0\r\n\r\n"}
			status, err := s.Submit(context.Background(), "operator-a", h.ID, callbackURL)
			if !errors.Is(err, ErrUnavailable) || status != "" || f.dials != 1 {
				t.Fatal("unreviewed redirect accepted")
			}
		})
	}
}
func TestMalformedCallbacksNeverDial(t *testing.T) {
	cases := []string{
		"https://localhost:1455/auth/callback?code=x&state=native-state",
		"http://127.0.0.1:1455/auth/callback?code=x&state=native-state",
		"http://localhost.evil:1455/auth/callback?code=x&state=native-state",
		"http://169.254.169.254/latest/meta-data/",
		"http://localhost:1455@evil.invalid/auth/callback?code=x&state=native-state",
		"http://localhost:1456/auth/callback?code=x&state=native-state",
		"http://localhost:1455/%61uth/callback?code=x&state=native-state",
		callbackURL + "#fragment", callbackURL + "&code=second", callbackURL + "&state=second",
		callbackURL + "&error_description=secret-canary", callbackURL + "&unknown=x",
		strings.Replace(callbackURL, "native-state", "stale-state", 1),
		strings.Replace(callbackURL, "callback-canary", "%0d%0aInjected", 1),
		strings.Replace(callbackURL, "callback-canary", "%00", 1),
		strings.Replace(callbackURL, "callback-canary", "%GG", 1),
		strings.Replace(callbackURL, "callback-canary", "", 1),
		"file:///etc/passwd", "//localhost:1455/auth/callback", callbackURL + strings.Repeat("x", MaxURLBytes),
	}
	for _, raw := range cases {
		t.Run(raw[:min(len(raw), 80)], func(t *testing.T) {
			s, f, h := fixture(t, "pi")
			_, err := s.Submit(context.Background(), "operator-a", h.ID, raw)
			if !errors.Is(err, ErrInvalid) || f.dials != 0 {
				t.Fatal("invalid callback reached transport")
			}
		})
	}
}
func TestOperatorSessionAndReplayBinding(t *testing.T) {
	s, f, h := fixture(t, "pi")
	if _, err := s.Submit(context.Background(), "operator-b", h.ID, callbackURL); !errors.Is(err, ErrConsumed) {
		t.Fatal(err)
	}
	if err := s.Cancel("operator-b", h.ID); !errors.Is(err, ErrConsumed) {
		t.Fatal(err)
	}
	if len(s.Pending(context.Background(), "operator-a", "session-b")) != 0 {
		t.Fatal("cross-session pending attempt")
	}
	if _, err := s.Submit(context.Background(), "operator-a", h.ID, callbackURL); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Submit(context.Background(), "operator-a", h.ID, callbackURL); !errors.Is(err, ErrConsumed) {
		t.Fatal(err)
	}
	if err := s.Register(context.Background(), 124, "pi", authURL("native-state")); !errors.Is(err, ErrConsumed) {
		t.Fatal("same native attempt re-registered")
	}
	if f.dials != 1 {
		t.Fatal(f.dials)
	}
}
func TestExpiredCancelledReplacedAndRestartedAttempts(t *testing.T) {
	for _, kind := range []string{"expired", "cancelled", "generation", "network", "process", "listener", "exit", "new-attempt", "backend-restart"} {
		t.Run(kind, func(t *testing.T) {
			s, f, h := fixture(t, "pi")
			switch kind {
			case "expired":
				s.now = func() time.Time { return h.Expires }
			case "cancelled":
				_ = s.Cancel("operator-a", h.ID)
			case "generation":
				f.binding.Generation++
			case "network":
				f.binding.Network = "network-b"
			case "process":
				f.binding.StartTicks++
			case "listener":
				f.binding.Listener++
			case "exit":
				f.alive = false
			case "new-attempt":
				if err := s.Register(context.Background(), 124, "pi", authURL("new-state")); err != nil {
					t.Fatal(err)
				}
			case "backend-restart":
				s = New(f)
			}
			if _, err := s.Submit(context.Background(), "operator-a", h.ID, callbackURL); err == nil {
				t.Fatal("stale authority survived")
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			if len(f.requests) != 0 {
				t.Fatal("stale callback written")
			}
		})
	}
}
func TestConcurrentSubmissionConsumesNativeAttemptOnce(t *testing.T) {
	s, f, h := fixture(t, "pi")
	var wg sync.WaitGroup
	var accepted atomic.Int32
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := s.Submit(context.Background(), "operator-a", h.ID, callbackURL); err == nil {
				accepted.Add(1)
			}
		}()
	}
	wg.Wait()
	if accepted.Load() != 1 || f.dials != 1 {
		t.Fatalf("accepted=%d dials=%d", accepted.Load(), f.dials)
	}
}

func TestCancelRejectsConsumedAndExpiredHandles(t *testing.T) {
	for _, kind := range []string{"cancelled", "submitted", "expired", "superseded"} {
		t.Run(kind, func(t *testing.T) {
			s, _, h := fixture(t, "pi")
			switch kind {
			case "cancelled":
				if err := s.Cancel("operator-a", h.ID); err != nil {
					t.Fatal(err)
				}
			case "submitted":
				if _, err := s.Submit(context.Background(), "operator-a", h.ID, callbackURL); err != nil {
					t.Fatal(err)
				}
			case "expired":
				s.now = func() time.Time { return h.Expires }
			case "superseded":
				if err := s.Register(context.Background(), 124, "pi", authURL("new-state")); err != nil {
					t.Fatal(err)
				}
			}
			if err := s.Cancel("operator-a", h.ID); !errors.Is(err, ErrConsumed) {
				t.Fatal("stale cancellation reported success")
			}
		})
	}
}

func TestSubmitAndCancelHaveOnlyOneWinner(t *testing.T) {
	for range 50 {
		s, f, h := fixture(t, "pi")
		start := make(chan struct{})
		cancelled, submitted := make(chan error, 1), make(chan error, 1)
		go func() {
			<-start
			cancelled <- s.Cancel("operator-a", h.ID)
		}()
		go func() {
			<-start
			_, err := s.Submit(context.Background(), "operator-a", h.ID, callbackURL)
			submitted <- err
		}()
		close(start)
		cancelErr, submitErr := <-cancelled, <-submitted
		if cancelErr == nil {
			if !errors.Is(submitErr, ErrConsumed) || f.dials != 0 {
				t.Fatal("callback delivered after cancellation won")
			}
		} else if !errors.Is(cancelErr, ErrConsumed) || submitErr != nil || f.dials != 1 {
			t.Fatal("cancellation and submission did not consume one shared authority")
		}
	}
}

func TestUncertainDeliveryNeverRetriesOrLeaksErrors(t *testing.T) {
	s, f, h := fixture(t, "pi")
	f.dialError = true
	_, err := s.Submit(context.Background(), "operator-a", h.ID, callbackURL)
	if !errors.Is(err, ErrUnavailable) || strings.Contains(err.Error(), "canary") {
		t.Fatal("private error escaped")
	}
	_, err = s.Submit(context.Background(), "operator-a", h.ID, callbackURL)
	if !errors.Is(err, ErrConsumed) || f.dials != 1 {
		t.Fatal("uncertain delivery retried")
	}
}

func registrationFixture(t *testing.T) (*Service, *fakeRuntime, string) {
	t.Helper()
	f := &fakeRuntime{binding: Binding{Session: "session-a", Generation: 1, Provider: "pi", PID: 123, StartTicks: 456, Listener: 789, Network: "network-a"}, alive: true}
	s := New(f)
	if err := s.Register(context.Background(), 124, "pi", authURL("native-state")); err != nil {
		t.Fatal(err)
	}
	pending := s.Pending(context.Background(), "operator-a", "session-a")
	if len(pending) != 1 {
		t.Fatal(pending)
	}
	return s, f, pending[0].ID
}

func TestDismissReturnsSameLiveAttemptToPending(t *testing.T) {
	s, f, registration := registrationFixture(t)
	h, err := s.Begin(context.Background(), "operator-a", "session-a", registration)
	if err != nil {
		t.Fatal(err)
	}
	if pending := s.Pending(context.Background(), "operator-a", "session-a"); len(pending) != 0 {
		t.Fatal("held attempt visible to a second browser")
	}
	if _, err := s.Begin(context.Background(), "operator-a", "session-a", registration); !errors.Is(err, ErrConsumed) {
		t.Fatal("second browser claimed a held attempt")
	}
	if err := s.Dismiss(context.Background(), "operator-a", h.ID); err != nil {
		t.Fatal(err)
	}
	pending := s.Pending(context.Background(), "operator-a", "session-a")
	if len(pending) != 1 || pending[0].ID != registration {
		t.Fatal("dismissal did not offer the same native attempt again")
	}
	resumed, err := s.Begin(context.Background(), "operator-a", "session-a", registration)
	if err != nil {
		t.Fatal("reopened selector could not claim the dismissed attempt")
	}
	if resumed.ID == h.ID {
		t.Fatal("resume recycled a released handle")
	}
	// The released handle is dead for every gesture; only the fresh one works.
	if _, err := s.Submit(context.Background(), "operator-a", h.ID, callbackURL); !errors.Is(err, ErrConsumed) {
		t.Fatal("released handle still submits")
	}
	if err := s.Cancel("operator-a", h.ID); !errors.Is(err, ErrConsumed) {
		t.Fatal("released handle still cancels")
	}
	if err := s.Dismiss(context.Background(), "operator-a", h.ID); !errors.Is(err, ErrConsumed) {
		t.Fatal("released handle dismissed twice")
	}
	if f.dials != 0 {
		t.Fatal("released handle reached transport")
	}
	status, err := s.Submit(context.Background(), "operator-a", resumed.ID, callbackURL)
	if err != nil || status != "delivered" || f.dials != 1 {
		t.Fatalf("resumed handoff failed: status=%q err=%v dials=%d", status, err, f.dials)
	}
}

func TestDismissRequiresClaimingOperatorAndSession(t *testing.T) {
	s, f, h := fixture(t, "pi")
	if err := s.Dismiss(context.Background(), "operator-b", h.ID); !errors.Is(err, ErrConsumed) {
		t.Fatal(err)
	}
	if pending := s.Pending(context.Background(), "operator-a", "session-a"); len(pending) != 0 {
		t.Fatal("foreign user released the attempt")
	}
	if pending := s.Pending(context.Background(), "operator-a", "session-b"); len(pending) != 0 {
		t.Fatal("attempt visible outside its session after dismissal attempt")
	}
	status, err := s.Submit(context.Background(), "operator-a", h.ID, callbackURL)
	if err != nil || status != "delivered" || f.dials != 1 {
		t.Fatal("claiming user lost the handle to a foreign dismissal")
	}
}

func TestDismissFailsClosedForExpiredOrChangedAttempt(t *testing.T) {
	for _, kind := range []string{"expired", "generation", "network", "process", "listener", "exit", "backend-restart"} {
		t.Run(kind, func(t *testing.T) {
			s, f, h := fixture(t, "pi")
			switch kind {
			case "expired":
				s.now = func() time.Time { return h.Expires }
			case "generation":
				f.binding.Generation++
			case "network":
				f.binding.Network = "network-b"
			case "process":
				f.binding.StartTicks++
			case "listener":
				f.binding.Listener++
			case "exit":
				f.alive = false
			case "backend-restart":
				s = New(f)
			}
			if err := s.Dismiss(context.Background(), "operator-a", h.ID); err == nil {
				t.Fatal("stale dismissal succeeded")
			}
			if pending := s.Pending(context.Background(), "operator-a", "session-a"); len(pending) != 0 {
				t.Fatal("stale dismissal resurrected an attempt")
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			if len(f.requests) != 0 {
				t.Fatal("stale dismissal delivered bytes")
			}
		})
	}
}

func TestCancelledOrSubmittedAttemptNeverReturnsToPending(t *testing.T) {
	for _, kind := range []string{"cancelled", "submitted"} {
		t.Run(kind, func(t *testing.T) {
			s, _, registration := registrationFixture(t)
			h, err := s.Begin(context.Background(), "operator-a", "session-a", registration)
			if err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "cancelled":
				if err := s.Cancel("operator-a", h.ID); err != nil {
					t.Fatal(err)
				}
			case "submitted":
				if _, err := s.Submit(context.Background(), "operator-a", h.ID, callbackURL); err != nil {
					t.Fatal(err)
				}
			}
			if err := s.Dismiss(context.Background(), "operator-a", h.ID); !errors.Is(err, ErrConsumed) {
				t.Fatal("terminal handle dismissed")
			}
			if _, err := s.Begin(context.Background(), "operator-a", "session-a", registration); !errors.Is(err, ErrConsumed) {
				t.Fatal("terminal attempt claimed again")
			}
			if pending := s.Pending(context.Background(), "operator-a", "session-a"); len(pending) != 0 {
				t.Fatal("terminal attempt returned to pending")
			}
		})
	}
}

func TestInvalidCallbackCorrectionStillAllowsDismissal(t *testing.T) {
	s, f, h := fixture(t, "pi")
	if _, err := s.Submit(context.Background(), "operator-a", h.ID, "http://localhost:1455/auth/callback?state=wrong-state&code=x"); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
	if err := s.Dismiss(context.Background(), "operator-a", h.ID); err != nil {
		t.Fatal("a correctable malformed URL consumed the handle")
	}
	if pending := s.Pending(context.Background(), "operator-a", "session-a"); len(pending) != 1 || f.dials != 0 {
		t.Fatal("corrected dismissal did not return the attempt untouched")
	}
}

func TestCodexAcceptsOnlyTheOnboardingStateSuffix(t *testing.T) {
	const base = "http://localhost:1455/auth/callback?code=x&state="
	for state, accepted := range map[string]bool{
		"native-state": true,
		"native-state.onboarding_entrypoint=life_sciences":       true,
		"native-state.onboarding_entrypoint=other":               false,
		"native-state.onboarding_entrypoint=life_sciences.extra": false,
		"wrong-state.onboarding_entrypoint=life_sciences":        false,
		".onboarding_entrypoint=life_sciences":                   false,
	} {
		t.Run(state, func(t *testing.T) {
			s, f, h := fixture(t, "codex")
			f.responses = []string{"HTTP/1.1 302 Found\r\nLocation: http://localhost:1455/success\r\nContent-Length: 0\r\n\r\n", "HTTP/1.1 200 OK\r\nContent-Length: 0\r\n\r\n"}
			status, err := s.Submit(context.Background(), "operator-a", h.ID, base+url.QueryEscape(state))
			if accepted && (err != nil || status != "completed") {
				t.Fatalf("status=%q err=%v", status, err)
			}
			if !accepted && (!errors.Is(err, ErrInvalid) || f.dials != 0) {
				t.Fatalf("status=%q err=%v dials=%d", status, err, f.dials)
			}
			if accepted {
				f.mu.Lock()
				defer f.mu.Unlock()
				if got := f.requests[0].URL.Query().Get("state"); got != state {
					t.Fatalf("forwarded state %q", got)
				}
			}
		})
	}
}

func TestPiOpenAICallbackUsesTheIssuedState(t *testing.T) {
	for _, state := range []string{"native-state", "native-state" + lifeSciencesStateSuffix} {
		t.Run(state, func(t *testing.T) {
			s, f, h := fixture(t, "pi")
			status, err := s.Submit(context.Background(), "operator-a", h.ID, "http://localhost:1455/auth/callback?code=callback-canary&state="+url.QueryEscape(state))
			if err != nil || status != "delivered" {
				t.Fatalf("callback: %s %v", status, err)
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			if len(f.requests) != 1 || f.requests[0].URL.Query().Get("state") != "native-state" {
				t.Fatal("native Pi received a modified state")
			}
			if f.requests[0].URL.Query().Get("code") != "callback-canary" {
				t.Fatal("authorization code changed")
			}
		})
	}
}

func TestSubmitAndDismissHaveOnlyOneWinner(t *testing.T) {
	for range 50 {
		s, f, h := fixture(t, "pi")
		start := make(chan struct{})
		dismissed, submitted := make(chan error, 1), make(chan error, 1)
		go func() {
			<-start
			dismissed <- s.Dismiss(context.Background(), "operator-a", h.ID)
		}()
		go func() {
			<-start
			_, err := s.Submit(context.Background(), "operator-a", h.ID, callbackURL)
			submitted <- err
		}()
		close(start)
		dismissErr, submitErr := <-dismissed, <-submitted
		pending := s.Pending(context.Background(), "operator-a", "session-a")
		if dismissErr == nil {
			if !errors.Is(submitErr, ErrConsumed) || f.dials != 0 {
				t.Fatal("callback delivered after dismissal won")
			}
			if len(pending) != 1 {
				t.Fatal("winning dismissal did not return the attempt")
			}
		} else if !errors.Is(dismissErr, ErrConsumed) || submitErr != nil || f.dials != 1 || len(pending) != 0 {
			t.Fatal("dismissal and submission did not consume one shared authority")
		}
	}
}

func TestDismissedAttemptStaysWithFirstOperator(t *testing.T) {
	s, f, registration := registrationFixture(t)
	h, err := s.Begin(context.Background(), "operator-a", "session-a", registration)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Dismiss(context.Background(), "operator-a", h.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Begin(context.Background(), "operator-b", "session-a", registration); !errors.Is(err, ErrConsumed) {
		t.Fatal("another user claimed a dismissed attempt")
	}
	resumed, err := s.Begin(context.Background(), "operator-a", "session-a", registration)
	if err != nil {
		t.Fatal("claiming user could not resume a dismissed attempt")
	}
	status, err := s.Submit(context.Background(), "operator-a", resumed.ID, callbackURL)
	if err != nil || status != "delivered" || f.dials != 1 {
		t.Fatalf("resumed handoff failed: status=%q err=%v dials=%d", status, err, f.dials)
	}
}

func TestPendingFiltersByAuthenticatedOperator(t *testing.T) {
	s, _, registration := registrationFixture(t)
	// Unclaimed and never held: every authenticated user sees it.
	if pending := s.Pending(context.Background(), "operator-a", "session-a"); len(pending) != 1 {
		t.Fatal("unowned attempt not listed")
	}
	if pending := s.Pending(context.Background(), "operator-b", "session-a"); len(pending) != 1 {
		t.Fatal("unowned attempt not listed for a second user")
	}
	// An unauthenticated caller is never offered an attempt.
	if pending := s.Pending(context.Background(), "", "session-a"); len(pending) != 0 {
		t.Fatal("anonymous caller listed a pending attempt")
	}
	h, err := s.Begin(context.Background(), "operator-a", "session-a", registration)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Dismiss(context.Background(), "operator-a", h.ID); err != nil {
		t.Fatal(err)
	}
	// Dismissed: listed only for the user who held it; anyone else would
	// be offered a handoff Begin must refuse.
	pending := s.Pending(context.Background(), "operator-a", "session-a")
	if len(pending) != 1 || pending[0].ID != registration {
		t.Fatal("dismissed attempt not listed for its user")
	}
	if pending := s.Pending(context.Background(), "operator-b", "session-a"); len(pending) != 0 {
		t.Fatal("dismissed attempt listed for a foreign user")
	}
	if _, err := s.Begin(context.Background(), "operator-b", "session-a", registration); !errors.Is(err, ErrConsumed) {
		t.Fatal("foreign user claimed a dismissed attempt")
	}
}

func TestPendingFilterAndDismissRace(t *testing.T) {
	for range 50 {
		s, _, registration := registrationFixture(t)
		h, err := s.Begin(context.Background(), "operator-a", "session-a", registration)
		if err != nil {
			t.Fatal(err)
		}
		start := make(chan struct{})
		var wg sync.WaitGroup
		var foreignListed, ownerOverlisted atomic.Int32
		// Owner and foreign listings race the dismissal. The foreign user
		// must never observe the owned attempt, before or after it is released.
		for range 4 {
			wg.Add(2)
			go func() {
				defer wg.Done()
				<-start
				if n := len(s.Pending(context.Background(), "operator-b", "session-a")); n != 0 {
					foreignListed.Add(int32(n))
				}
			}()
			go func() {
				defer wg.Done()
				<-start
				if n := len(s.Pending(context.Background(), "operator-a", "session-a")); n > 1 {
					ownerOverlisted.Add(int32(n))
				}
			}()
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_ = s.Dismiss(context.Background(), "operator-a", h.ID)
		}()
		close(start)
		wg.Wait()
		if foreignListed.Load() != 0 {
			t.Fatal("dismissal exposed the attempt to a foreign user")
		}
		if ownerOverlisted.Load() != 0 {
			t.Fatal("attempt listed more than once for its user")
		}
		if pending := s.Pending(context.Background(), "operator-a", "session-a"); len(pending) != 1 {
			t.Fatal("attempt not pending exactly once for its user after the race")
		}
		if pending := s.Pending(context.Background(), "operator-b", "session-a"); len(pending) != 0 {
			t.Fatal("attempt pending for a foreign user after the race")
		}
	}
}

func TestDismissAndForeignBeginHaveOnlyOneOutcome(t *testing.T) {
	for range 50 {
		s, f, registration := registrationFixture(t)
		h, err := s.Begin(context.Background(), "operator-a", "session-a", registration)
		if err != nil {
			t.Fatal(err)
		}
		start := make(chan struct{})
		dismissed, begun := make(chan error, 1), make(chan error, 1)
		go func() {
			<-start
			dismissed <- s.Dismiss(context.Background(), "operator-a", h.ID)
		}()
		go func() {
			<-start
			_, err := s.Begin(context.Background(), "operator-b", "session-a", registration)
			begun <- err
		}()
		close(start)
		if err := <-dismissed; err != nil {
			t.Fatal(err)
		}
		if err := <-begun; !errors.Is(err, ErrConsumed) {
			t.Fatal("dismissal handed the attempt to another user")
		}
		if f.dials != 0 {
			t.Fatal("foreign begin reached transport")
		}
		if _, err := s.Begin(context.Background(), "operator-a", "session-a", registration); err != nil {
			t.Fatal("claiming user could not resume after the race")
		}
	}
}

func TestConcurrentDismissalReleasesHandleOnce(t *testing.T) {
	for range 50 {
		s, _, h := fixture(t, "pi")
		var wg sync.WaitGroup
		var released atomic.Int32
		for range 10 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if s.Dismiss(context.Background(), "operator-a", h.ID) == nil {
					released.Add(1)
				}
			}()
		}
		wg.Wait()
		if released.Load() != 1 {
			t.Fatalf("released=%d", released.Load())
		}
		if pending := s.Pending(context.Background(), "operator-a", "session-a"); len(pending) != 1 {
			t.Fatal("attempt not pending exactly once after duplicate dismissals")
		}
	}
}

func TestRepeatedDismissalKeepsTicketMapBounded(t *testing.T) {
	s, f, registration := registrationFixture(t)
	var released []string
	for range 20 {
		h, err := s.Begin(context.Background(), "operator-a", "session-a", registration)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.Dismiss(context.Background(), "operator-a", h.ID); err != nil {
			t.Fatal(err)
		}
		released = append(released, h.ID)
		if len(s.tickets) != 0 {
			t.Fatalf("dismissal retained a handle: %d tickets", len(s.tickets))
		}
	}
	for _, id := range released {
		if _, err := s.Submit(context.Background(), "operator-a", id, callbackURL); !errors.Is(err, ErrConsumed) {
			t.Fatal("released handle still submits")
		}
		if err := s.Cancel("operator-a", id); !errors.Is(err, ErrConsumed) {
			t.Fatal("released handle still cancels")
		}
		if err := s.Dismiss(context.Background(), "operator-a", id); !errors.Is(err, ErrConsumed) {
			t.Fatal("released handle dismissed twice")
		}
	}
	if f.dials != 0 {
		t.Fatal("released handle reached transport")
	}
	if _, err := s.Begin(context.Background(), "operator-b", "session-a", registration); !errors.Is(err, ErrConsumed) {
		t.Fatal("another user claimed a dismissed attempt")
	}
	resumed, err := s.Begin(context.Background(), "operator-a", "session-a", registration)
	if err != nil {
		t.Fatal("original user could not reopen the dismissed attempt")
	}
	status, err := s.Submit(context.Background(), "operator-a", resumed.ID, callbackURL)
	if err != nil || status != "delivered" || f.dials != 1 {
		t.Fatalf("resumed handoff failed: status=%q err=%v dials=%d", status, err, f.dials)
	}
}

func TestPiRejectsForeignCallbackStates(t *testing.T) {
	for _, state := range []string{"wrong-state" + lifeSciencesStateSuffix, "native-state.onboarding_entrypoint=other", "native-state" + lifeSciencesStateSuffix + lifeSciencesStateSuffix, lifeSciencesStateSuffix} {
		t.Run(state, func(t *testing.T) {
			s, f, h := fixture(t, "pi")
			_, err := s.Submit(context.Background(), "operator-a", h.ID, "http://localhost:1455/auth/callback?code=callback-canary&state="+url.QueryEscape(state))
			if !errors.Is(err, ErrInvalid) || f.dials != 0 {
				t.Fatal("foreign state reached native Pi")
			}
		})
	}
}
