package workspacehost

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/noisefloor-oss/deck/internal/auth"
	"github.com/noisefloor-oss/deck/internal/session"
	"github.com/noisefloor-oss/deck/internal/workspaceruntime"
)

type privateRequestKey struct{}
type privateRequest struct{ session, csrf string }

func privateRequestContext(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// This transient request material has no authority by itself. The service
		// invokes preparation only after consuming a fresh lock proof; the backend
		// verifies the original Google session and CSRF value again.
		value := privateRequest{csrf: r.Header.Get(auth.CSRFHeader)}
		count := 0
		for _, cookie := range r.Cookies() {
			if cookie.Name == "__Host-noisedeck_session" {
				count++
				value.session = cookie.Value
			}
		}
		if count != 1 {
			value.session = ""
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), privateRequestKey{}, value)))
	})
}

func (h *Host) prepareLock(ctx context.Context) error {
	value, ok := ctx.Value(privateRequestKey{}).(privateRequest)
	if !ok || value.session == "" || value.csrf == "" {
		return ErrUnavailable
	}
	ctx, cancel := context.WithTimeout(ctx, session.WorkspaceLockTimeout)
	defer cancel()
	deadline, _ := ctx.Deadline()
	r, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://localhost/api/internal/workspace/prepare-lock", nil)
	if err != nil {
		return ErrUnavailable
	}
	origin, _ := url.Parse(h.cfg.Origin)
	r.Host = origin.Host
	r.Header.Set("Origin", h.cfg.Origin)
	r.Header.Set(auth.CSRFHeader, value.csrf)
	r.Header.Set(session.WorkspaceLockBudgetHeader, strconv.FormatInt(time.Until(deadline).Milliseconds(), 10))
	r.AddCookie(&http.Cookie{Name: "__Host-noisedeck_session", Value: value.session})
	client := &http.Client{Transport: h.control, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(r)
	if err != nil {
		return ErrUnavailable
	}
	_ = response.Body.Close()
	if response.StatusCode == http.StatusUnprocessableEntity {
		return workspaceruntime.ErrLockRefused
	}
	if response.StatusCode != http.StatusNoContent {
		return ErrUnavailable
	}
	return nil
}
