package harnesslogin

import (
	"context"
	"errors"
	"net/url"
	"testing"
)

func codexAuthorizationAt(authority string) string {
	u, _ := url.Parse(authURL("native-state"))
	q := u.Query()
	q.Set("redirect_uri", "http://"+authority+"/auth/callback")
	u.RawQuery = q.Encode()
	return u.String()
}

func TestCodexLoopbackAuthoritiesRemainExact(t *testing.T) {
	authorities := []string{"localhost:1455", "localhost:1457", "127.0.0.1:1455", "127.0.0.1:1457"}
	for _, authority := range authorities {
		t.Run(authority, func(t *testing.T) {
			flow, cb, err := authorization("codex", codexAuthorizationAt(authority))
			if err != nil || flow != CodexBrowser || cb.authority != authority {
				t.Fatal("native loopback authority was refused or rewritten")
			}
			for _, candidate := range authorities {
				_, err := cb.requestTarget("http://" + candidate + "/auth/callback?state=native-state&code=callback-canary")
				if (err == nil) != (candidate == authority) {
					t.Fatal("callback host or port was not bound to the native attempt")
				}
			}
		})
	}
	for _, authority := range []string{"127.0.0.1", "127.0.0.1:1456", "127.0.0.2:1455", "0.0.0.0:1455", "[::1]:1455", "localhost.evil:1455", "127.0.0.1.evil:1455", "127.0.0.1:1455@evil.invalid", "2130706433:1455", "127.1:1455"} {
		if _, _, err := authorization("codex", codexAuthorizationAt(authority)); err == nil {
			t.Errorf("unapproved callback authority admitted: %s", authority)
		}
	}
	if _, _, err := authorization("pi", codexAuthorizationAt("127.0.0.1:1455")); err == nil {
		t.Fatal("Codex's alternate spelling widened Pi's callback contract")
	}
}

func TestCodexIPv4CallbackPreservesNativeAuthority(t *testing.T) {
	ctx := context.Background()
	for _, redirectAuthority := range []string{"127.0.0.1:1455", "localhost:1455"} {
		t.Run(redirectAuthority, func(t *testing.T) {
			f := &fakeRuntime{binding: Binding{Session: "session-a", Provider: "codex"}, alive: true}
			s := New(f)
			raw := codexAuthorizationAt("127.0.0.1:1455")
			if err := s.Register(ctx, 124, "codex", raw); err != nil {
				t.Fatal(err)
			}
			pending := s.Pending(ctx, "user-a", "session-a")
			if len(pending) != 1 {
				t.Fatal("native IPv4 login did not become available")
			}
			if got, err := s.Authorization(ctx, "user-a", "session-a", pending[0].ID); err != nil || got != raw {
				t.Fatal("native authorization URL was rewritten")
			}
			h, err := s.Begin(ctx, "user-a", "session-a", pending[0].ID)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.Submit(ctx, "user-a", h.ID, callbackURL); !errors.Is(err, ErrInvalid) || f.dials != 0 {
				t.Fatal("localhost alias reached an IPv4-bound callback")
			}
			f.responses = []string{"HTTP/1.1 302 Found\r\nLocation: http://" + redirectAuthority + "/success?id_token=secret-canary\r\nContent-Length: 0\r\n\r\n", "HTTP/1.1 200 OK\r\nContent-Length: 0\r\n\r\n"}
			status, err := s.Submit(ctx, "user-a", h.ID, "http://127.0.0.1:1455/auth/callback?state=native-state&code=callback-canary")
			if redirectAuthority != "127.0.0.1:1455" {
				if !errors.Is(err, ErrUnavailable) || status != "" || f.dials != 1 {
					t.Fatal("success redirect crossed to another authority")
				}
				return
			}
			if err != nil || status != "completed" {
				t.Fatalf("completion status=%q err=%v", status, err)
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			if len(f.requests) != 2 || f.requests[0].Host != "127.0.0.1:1455" || f.requests[1].Host != "127.0.0.1:1455" || f.requests[1].URL.RequestURI() != "/success" {
				t.Fatal("native completion did not preserve authority and strip redirect credentials")
			}
		})
	}
}
