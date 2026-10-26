package auth

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// Login identifies one browser login across cookie renewals. It is not an
// unlock grant. Issuer, Subject and Email are present only after verified OIDC
// sign-in; password and legacy sessions cannot establish a Google identity.
type Login struct {
	ID      string
	Issuer  string
	Subject string
	Email   string
}

type loginKey struct{}
type sessionExpiryKey struct{}

// SessionExpires returns the deadline of the verified cookie on this request.
// A long-lived stream must reconnect with a current cookie after this deadline.
func SessionExpires(ctx context.Context) (time.Time, bool) {
	expires, ok := ctx.Value(sessionExpiryKey{}).(time.Time)
	return expires, ok
}

func withSessionExpiry(ctx context.Context, payload string) context.Context {
	parts := strings.SplitN(payload, ":", 4)
	if len(parts) != 4 || (parts[0] != "v3" && parts[0] != "v4") {
		return ctx
	}
	seconds, err := strconv.ParseInt(parts[2], 10, 64)
	if err != nil {
		return ctx
	}
	return context.WithValue(ctx, sessionExpiryKey{}, time.Unix(seconds, 0))
}

// CurrentLogin returns the verified browser login established by Require.
// Legacy cookies and trusted-proxy headers have no browser login identity.
func CurrentLogin(ctx context.Context) (Login, bool) {
	login, ok := ctx.Value(loginKey{}).(Login)
	return login, ok
}

func loginFromPayload(payload string) (Login, bool) {
	parts := strings.Split(payload, ":")
	// v3 carries the login identity; v4 adds the logout epoch as an eighth
	// field, so both lengths parse here and the epoch is read where it is
	// checked.
	if (len(parts) != 7 && len(parts) != 8) || (parts[0] != "v3" && parts[0] != "v4") {
		return Login{}, false
	}
	id, err := hex.DecodeString(parts[3])
	if err != nil || len(id) != 32 || hex.EncodeToString(id) != parts[3] {
		return Login{}, false
	}
	login := Login{ID: parts[3]}
	for i, target := range []*string{&login.Issuer, &login.Subject, &login.Email} {
		decoded, err := base64.RawURLEncoding.Strict().DecodeString(parts[i+4])
		if err != nil || !utf8.Valid(decoded) || len(decoded) > 1024 {
			return Login{}, false
		}
		*target = string(decoded)
	}
	if login.Issuer == "" && login.Subject == "" && login.Email == "" {
		return login, true
	}
	if login.Issuer == "" || !validSubject(login.Subject) || login.Email == "" {
		return Login{}, false
	}
	return login, true
}
