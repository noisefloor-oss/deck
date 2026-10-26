package server

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type releaseTransport func(*http.Request) (*http.Response, error)

func (f releaseTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestReleaseNoticeVersionAndCache(t *testing.T) {
	for _, tc := range []struct {
		installed, latest string
		want              bool
	}{
		{"0.3.0", "v0.4.0", true},
		{"v0.3.0", "v0.10.0", true},
		{"0.4.0", "v0.4.0", false},
		{"0.5.0", "v0.4.0", false},
		{"dev+abc", "v0.4.0", false},
		{"v0.3.0-12-gabc", "v0.4.0", false},
		{"0.4.0-rc.1", "v0.4.0", false},
		{"0.3.0", "v0.4.0-rc.1", false},
		{"0.3.0", "v00.4.0", false},
	} {
		t.Run(tc.installed+"/"+tc.latest, func(t *testing.T) {
			var calls atomic.Int32
			n := NewReleaseNotice(tc.installed)
			n.client = &http.Client{Transport: releaseTransport(func(r *http.Request) (*http.Response, error) {
				calls.Add(1)
				if r.URL.String() != publicReleases || r.Header.Get("Authorization") != "" {
					t.Error("unexpected target or credential")
				}
				return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(fmt.Sprintf(`{"tag_name":%q}`, tc.latest)))}, nil
			})}
			var wg sync.WaitGroup
			for range 8 {
				wg.Go(func() {
					rr := httptest.NewRecorder()
					n.ServeHTTP(rr, httptest.NewRequest("GET", "/", nil))
					if strings.Contains(rr.Body.String(), "https://github.com/") != tc.want {
						t.Errorf("notice = %s", rr.Body.String())
					}
				})
			}
			wg.Wait()
			_, stable := releaseVersion(tc.installed)
			want := int32(0)
			if stable {
				want = 1
			}
			if calls.Load() != want {
				t.Fatalf("calls=%d want=%d", calls.Load(), want)
			}
		})
	}
}

func TestReleaseNoticeQuietFailureRetriesAndRejectsDrafts(t *testing.T) {
	now := time.Now()
	n := NewReleaseNotice("0.3.0")
	n.now = func() time.Time { return now }
	calls := 0
	body := `{"tag_name":"v0.4.0","draft":true}`
	n.client = &http.Client{Transport: releaseTransport(func(*http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body))}, nil
	})}
	if n.latest() != "" {
		t.Fatal("draft advertised")
	}
	body = `{"tag_name":"v0.4.0","prerelease":true}`
	now = now.Add(16 * time.Minute)
	if n.latest() != "" {
		t.Fatal("prerelease advertised")
	}
	body = `{"tag_name":"v0.4.0"}`
	if n.latest() != "" || calls != 2 {
		t.Fatal("failure not cached")
	}
	now = now.Add(16 * time.Minute)
	if n.latest() != "v0.4.0" || calls != 3 {
		t.Fatal("retry failed")
	}
	now = now.Add(time.Hour)
	if n.latest() != "v0.4.0" || calls != 3 {
		t.Fatal("success not cached")
	}
}

func TestReleaseNoticeNetworkFailuresBackOff(t *testing.T) {
	for _, mode := range []string{"offline", "rate-limit", "redirect", "malformed"} {
		t.Run(mode, func(t *testing.T) {
			n := NewReleaseNotice("0.3.0")
			calls := 0
			n.client.Transport = releaseTransport(func(r *http.Request) (*http.Response, error) {
				calls++
				if calls > 1 {
					t.Fatal("failure or redirect made another request")
				}
				if mode == "offline" {
					return nil, errors.New("offline")
				}
				response := &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`not JSON`)), Request: r}
				if mode == "rate-limit" {
					response.StatusCode = 429
				}
				if mode == "redirect" {
					response.StatusCode = 302
					response.Header.Set("Location", "https://example.invalid/")
				}
				return response, nil
			})
			first, second := n.latest(), n.latest()
			if first != "" || second != "" || calls != 1 {
				t.Fatal("failed check advertised a release or bypassed backoff")
			}
		})
	}
}
