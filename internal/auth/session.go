package auth

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// SessionDuration bounds a signed local session between renewals. A
// web-process restart does not invalidate sessions (they are stateless and
// signed); expiry does. The middleware renews an aging session on use, so
// this is an idle bound, not a login cadence: only a month away from the
// app ends the session on its own.
const SessionDuration = 30 * 24 * time.Hour

// SessionMaxDuration caps a session absolutely, counted from the login that
// authenticated it and carried through every renewal. Sliding renewal alone
// would keep one login alive forever; the cap is what finally asks the
// user to prove the identity again. It sits well above the idle window
// on purpose: a cap equal to the window would clamp the first renewal and
// turn the session back into the absolute expiry this change removes.
const SessionMaxDuration = 90 * 24 * time.Hour

// sessionRenewAfter is how much of a session's window must have elapsed
// before a use renews it — renewal on every request would set cookies on
// every response for nothing.
const sessionRenewAfter = time.Hour

// CSRFHeader carries the session-bound CSRF token on state-changing
// requests. The frontend reads it from the CSRF cookie, which
// is deliberately not HttpOnly.
const CSRFHeader = "X-CSRF-Token"

// v3 retains a random browser-login ID and verified OIDC identity across
// renewal. HMAC-SHA256 signs the complete payload, including both timestamps.
// Legacy v1/v2 cookies remain valid but cannot establish that missing identity.
func (h *Handler) issueSessionValue(now time.Time) (string, error) {
	return h.issueSessionValueAt(now, now)
}

func (h *Handler) sessionSecret() []byte {
	h.sessionMu.RLock()
	defer h.sessionMu.RUnlock()
	return h.cfg.SessionSecret
}

// currentLogoutEpoch is the logout generation a session for loginID must
// carry to verify. Sessions without a login (v1, v2) share the "" key.
func (h *Handler) currentLogoutEpoch(loginID string) uint64 {
	h.sessionMu.RLock()
	defer h.sessionMu.RUnlock()
	return h.logoutEpochs[loginID]
}

// bumpLogoutEpoch makes every outstanding session of this login stale:
// the next verify of any of them - including a renewal that was already
// in flight when logout landed - fails. Persistence is best effort; the
// in-process epoch binds immediately.
func (h *Handler) bumpLogoutEpoch(loginID string) {
	h.sessionMu.Lock()
	h.logoutEpochs[loginID]++
	epoch := h.logoutEpochs[loginID]
	h.sessionMu.Unlock()
	if h.cfg.EpochStore != nil {
		if err := h.cfg.EpochStore.SaveLogoutEpoch(context.Background(), loginID, epoch); err != nil {
			h.log.Warn().Err(err).Str("login", loginID).Msg("auth: logout epoch not persisted; it binds this process only")
		}
	}
}

// payloadLogoutEpoch reads the epoch a verified payload carries. v3 and
// earlier predate the field and count as zero.
func payloadLogoutEpoch(parts []string) uint64 {
	if len(parts) == 8 && parts[0] == "v4" {
		if epoch, err := strconv.ParseUint(parts[7], 10, 64); err == nil {
			return epoch
		}
	}
	return 0
}

func (h *Handler) issueSessionValueAt(authTime, now time.Time) (string, error) {
	return h.issueLoginSession(authTime, now, Login{})
}

func (h *Handler) issueLoginSession(authTime, now time.Time, login Login) (string, error) {
	if login.ID == "" {
		var id [32]byte
		if _, err := rand.Read(id[:]); err != nil {
			return "", fmt.Errorf("auth: session randomness: %w", err)
		}
		login.ID = hex.EncodeToString(id[:])
	}
	// A fresh session carries its login's CURRENT epoch, read only after
	// the login has its ID: reading before would key on the empty legacy
	// ID, and a legacy logout's bump would then land on a fresh login that
	// its own logout could never exceed.
	return h.issueLoginSessionEpoch(authTime, now, login, h.currentLogoutEpoch(login.ID))
}

// issueLoginSessionEpoch issues a login session carrying a NAMED logout
// epoch. The renewal path names the epoch of the cookie it renews, so a
// logout landing between the caller's verify and this issue leaves the
// replacement exactly as stale as the original; a fresh issue names the
// login's current epoch.
func (h *Handler) issueLoginSessionEpoch(authTime, now time.Time, login Login, epoch uint64) (string, error) {
	if login.ID == "" {
		return "", errBadSession
	}
	fields := []string{"v4", strconv.FormatInt(authTime.Unix(), 10),
		strconv.FormatInt(sessionExpiry(authTime, now).Unix(), 10), login.ID}
	for _, value := range []string{login.Issuer, login.Subject, login.Email} {
		fields = append(fields, base64.RawURLEncoding.EncodeToString([]byte(value)))
	}
	fields = append(fields, strconv.FormatUint(epoch, 10))
	payload := strings.Join(fields, ":")
	if _, ok := loginFromPayload(payload); !ok {
		return "", errBadSession
	}
	return encodeSigned(payload, h.sessionSecret()), nil
}

func sessionExpiry(authTime, now time.Time) time.Time {
	expiry := now.Add(SessionDuration)
	if cap := authTime.Add(SessionMaxDuration); expiry.After(cap) {
		return cap
	}
	return expiry
}

func (h *Handler) issueLegacySession(authTime, now time.Time) (string, error) {
	var randPart [16]byte
	if _, err := rand.Read(randPart[:]); err != nil {
		return "", fmt.Errorf("auth: session randomness: %w", err)
	}
	payload := "v2:" + strconv.FormatInt(authTime.Unix(), 10) + ":" +
		strconv.FormatInt(sessionExpiry(authTime, now).Unix(), 10) + ":" + hex.EncodeToString(randPart[:])
	return encodeSigned(payload, h.sessionSecret()), nil
}

func encodeSigned(payload string, secret []byte) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(payload))
	return base64.RawURLEncoding.EncodeToString([]byte(payload)) + "." +
		base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

var errBadSession = errors.New("auth: invalid session")

// verifySessionValue returns the verified payload of a session token.
func (h *Handler) verifySessionValue(token string, now time.Time) (string, error) {
	if len(token) > 8192 {
		return "", errBadSession
	}
	dot := strings.IndexByte(token, '.')
	if dot < 0 {
		return "", errBadSession
	}
	rawPayload, err := base64.RawURLEncoding.DecodeString(token[:dot])
	if err != nil {
		return "", errBadSession
	}
	gotMAC, err := base64.RawURLEncoding.DecodeString(token[dot+1:])
	if err != nil {
		return "", errBadSession
	}
	mac := hmac.New(sha256.New, h.sessionSecret())
	mac.Write(rawPayload)
	if !hmac.Equal(mac.Sum(nil), gotMAC) {
		return "", errBadSession
	}
	parts := strings.Split(string(rawPayload), ":")
	var expField string
	switch {
	case len(parts) == 3 && parts[0] == "v1":
		if h.currentLogoutEpoch("") > 0 {
			return "", errBadSession
		}
		expField = parts[1]
	case len(parts) == 4 && parts[0] == "v2":
		if h.currentLogoutEpoch("") > 0 {
			return "", errBadSession
		}
		expField = parts[2]
	case (len(parts) == 7 && parts[0] == "v3") || (len(parts) == 8 && parts[0] == "v4"):
		login, ok := loginFromPayload(string(rawPayload))
		if !ok {
			return "", errBadSession
		}
		// A session from before its login's latest logout is dead, whatever
		// its expiry says: this is the check that makes logout final when a
		// renewal was already in flight. v3 predates epochs and counts as
		// epoch zero, so a logout binds it too.
		epoch := uint64(0)
		if parts[0] == "v4" {
			parsed, err := strconv.ParseUint(parts[7], 10, 64)
			if err != nil {
				return "", errBadSession
			}
			epoch = parsed
		}
		if epoch < h.currentLogoutEpoch(login.ID) {
			return "", errBadSession
		}
		auth, err := strconv.ParseInt(parts[1], 10, 64)
		if err != nil || auth <= 0 || auth > now.Unix() {
			return "", errBadSession
		}
		exp, err := strconv.ParseInt(parts[2], 10, 64)
		if err != nil || exp <= auth || exp-auth > int64(SessionMaxDuration.Seconds()) {
			return "", errBadSession
		}
		expField = parts[2]
	default:
		return "", errBadSession
	}
	exp, err := strconv.ParseInt(expField, 10, 64)
	if err != nil || now.Unix() >= exp {
		return "", errBadSession
	}
	return string(rawPayload), nil
}

// sessionRenewal returns the replacement for a verified session payload when
// its age says a use should extend it, and "" when the session is fresh
// enough to leave alone. A v1 session renews into v2, counting its cap from
// now — the true login time was never recorded, and assuming the worst would
// log the user out early for no failure of theirs.
func (h *Handler) sessionRenewal(payload string, now time.Time) (value, renewed string, err error) {
	parts := strings.Split(payload, ":")
	authTime := now
	var exp int64
	switch {
	case len(parts) == 3 && parts[0] == "v1":
		exp, _ = strconv.ParseInt(parts[1], 10, 64)
	case (len(parts) == 4 && parts[0] == "v2") || (len(parts) == 7 && parts[0] == "v3") || (len(parts) == 8 && parts[0] == "v4"):
		at, _ := strconv.ParseInt(parts[1], 10, 64)
		authTime = time.Unix(at, 0)
		exp, _ = strconv.ParseInt(parts[2], 10, 64)
	default:
		return "", "", errBadSession
	}
	// Fresh enough: more than (window - renewAfter) of life left means the
	// session was issued under an hour ago.
	if time.Unix(exp, 0).Sub(now) > SessionDuration-sessionRenewAfter {
		return "", "", nil
	}
	if !now.Before(authTime.Add(SessionMaxDuration)) {
		return "", "", nil
	}
	// Already clamped: once the cap is the expiry, re-issuing cannot extend
	// anything, and doing it anyway would rotate both cookies on every
	// request for the session's whole final month.
	if !authTime.Add(SessionMaxDuration).After(time.Unix(exp, 0)) {
		return "", "", nil
	}
	var v string
	var issueErr error
	if login, ok := loginFromPayload(payload); ok {
		// The renewal inherits the EPOCH OF THE COOKIE IT RENEWS, not a
		// fresh read: a logout that lands between the caller's verify and
		// this issue must leave the replacement just as stale as the
		// original. v3 predates epochs and renews at zero, which the same
		// rule makes stale the moment its login logs out.
		v, issueErr = h.issueLoginSessionEpoch(authTime, now, login, payloadLogoutEpoch(parts))
	} else {
		v, issueErr = h.issueLegacySession(authTime, now)
	}
	if issueErr != nil {
		return "", "", issueErr
	}
	renewedPayload, verifyErr := h.verifySessionValue(v, now)
	if verifyErr != nil {
		return "", "", verifyErr
	}
	return v, renewedPayload, nil
}

// csrfFor derives the session-bound CSRF token: recomputable server-side
// from the verified session payload, so it needs no storage and cannot
// outlive its session.
func (h *Handler) csrfFor(sessionPayload string) string {
	mac := hmac.New(sha256.New, h.sessionSecret())
	mac.Write([]byte("csrf:" + sessionPayload))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// Cookie names: the __Host- prefix binds a cookie to this host, Path=/, and
// Secure — exactly what a session cookie wants, and impossible on plain
// HTTP, so the plain names exist for declared-loopback development. Config
// derives this choice from the homogeneous allowed-origin scheme.
func (h *Handler) sessionCookieName() string {
	if h.cfg.SecureCookies {
		return "__Host-noisedeck_session"
	}
	return "noisedeck_session"
}

func (h *Handler) csrfCookieName() string {
	if h.cfg.SecureCookies {
		return "__Host-noisedeck_csrf"
	}
	return "noisedeck_csrf"
}

func (h *Handler) setSessionCookies(w http.ResponseWriter, sessionValue, payload string) {
	http.SetCookie(w, &http.Cookie{
		Name:     h.sessionCookieName(),
		Value:    sessionValue,
		Path:     "/",
		MaxAge:   int(SessionDuration.Seconds()),
		HttpOnly: true,
		Secure:   h.cfg.SecureCookies,
		SameSite: http.SameSiteLaxMode,
	})
	// Deliberately not HttpOnly: the frontend reads it to populate the CSRF
	// header. Everything else matches the session cookie.
	http.SetCookie(w, &http.Cookie{
		Name:     h.csrfCookieName(),
		Value:    h.csrfFor(payload),
		Path:     "/",
		MaxAge:   int(SessionDuration.Seconds()),
		HttpOnly: false,
		Secure:   h.cfg.SecureCookies,
		SameSite: http.SameSiteLaxMode,
	})
}

// ClearSessionCookies ends this browser's local login after a caller has
// completed its authenticated, CSRF-protected revocation. It does not recheck
// cookie expiry while that revocation is in progress.
func (h *Handler) ClearSessionCookies(w http.ResponseWriter) { h.clearSessionCookies(w) }

func (h *Handler) clearSessionCookies(w http.ResponseWriter) {
	for _, name := range []string{h.sessionCookieName(), h.csrfCookieName()} {
		http.SetCookie(w, &http.Cookie{
			Name: name, Value: "", Path: "/", MaxAge: -1,
			HttpOnly: name == h.sessionCookieName(),
			Secure:   h.cfg.SecureCookies,
			SameSite: http.SameSiteLaxMode,
		})
	}
}
