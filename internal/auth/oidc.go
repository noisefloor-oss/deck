package auth

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/rs/zerolog"
	"golang.org/x/crypto/bcrypt"
)

// Fixed Google endpoints. Deployment code, not discovery results and never
// user-supplied (doc.go); tests inject fakes through Config.
const (
	googleIssuer  = "https://accounts.google.com"
	googleAuthURL = "https://accounts.google.com/o/oauth2/v2/auth"
	googleToken   = "https://oauth2.googleapis.com/token" // #nosec G101 -- a public OAuth endpoint URL, not a credential
	googleJWKS    = "https://www.googleapis.com/oauth2/v3/certs"
)

// Response-size and field bounds, mirroring the research harness.
const (
	maxCodeLength     = 4096
	maxTokenResponse  = 1 << 20
	exchangeTimeout   = 10 * time.Second
	postLoginLocation = "/"
)

// Config parameterizes the auth handler. Endpoint fields default to Google
// and exist only so tests can stand up a deterministic fake provider.
type Config struct {
	Mode string // "none", "password", "oidc", or "trusted_proxy"

	// Builtin (Google OIDC).
	ClientID          string
	ClientSecret      string
	RedirectURL       string
	AllowedEmail      string
	SessionSecret     []byte
	PasswordStatePath string
	SetupOutput       io.Writer
	PasswordPinned    bool
	AllowedOrigins    []string
	PasswordHash      []byte
	SecureCookies     bool

	// EpochStore persists per-login logout epochs. Logout bumps the epoch
	// so a session renewal that was already in flight when logout landed
	// verifies as stale afterwards, instead of rewriting a still-valid
	// cookie into the browser after the logout response. Nil disables
	// persistence: the epoch then lives for this process only.
	EpochStore EpochStore

	// Trusted proxy.
	ProxyAddresses []string
	IdentityHeader string

	// Test seams. Zero values mean production Google.
	Issuer   string
	AuthURL  string
	TokenURL string
	JWKSURL  string
	Client   *http.Client
	Now      func() time.Time
}

// EpochStore persists the per-login logout epochs the session verifier
// consults, so a logout outlives the process that served it. Implementations
// key by login ID ("" for legacy sessions without one).
type EpochStore interface {
	LoadLogoutEpochs(ctx context.Context) (map[string]uint64, error)
	SaveLogoutEpoch(ctx context.Context, loginID string, epoch uint64) error
}

// Handler owns the login ceremony and the request-authentication
// middleware.
type Handler struct {
	cfg          Config
	log          zerolog.Logger
	txns         *txnStore
	verifier     *oidc.IDTokenVerifier
	client       *http.Client
	now          func() time.Time
	proxies      []trustedPeer
	password     *passwordGate
	credentialMu sync.RWMutex
	credentials  BootstrapState
	sessionMu    sync.RWMutex
	// logoutEpochs carries the per-login logout generation. sessionMu guards
	// it with the secret: both are read on the session verify path.
	logoutEpochs    map[string]uint64
	onLogout        func(string)
	onBrowserLogout func(Login)
}

// OnLogout registers revocation of existing per-user capabilities.
// Set it during boot, before serving requests.
func (h *Handler) OnLogout(revoke func(string)) { h.onLogout = revoke }

// OnBrowserLogout registers revocation scoped to one verified browser login.
// Set it before serving requests. Legacy cookies do not invoke this callback.
func (h *Handler) OnBrowserLogout(revoke func(Login)) { h.onBrowserLogout = revoke }

// New builds the handler. It performs no network request: the JWKS key set
// is fetched lazily at the first callback (doc.go — startup and health
// never touch Google).
func New(cfg Config, log zerolog.Logger) (*Handler, error) {
	if cfg.Issuer == "" {
		cfg.Issuer = googleIssuer
	}
	if cfg.AuthURL == "" {
		cfg.AuthURL = googleAuthURL
	}
	if cfg.TokenURL == "" {
		cfg.TokenURL = googleToken
	}
	if cfg.JWKSURL == "" {
		cfg.JWKSURL = googleJWKS
	}
	if cfg.Client == nil {
		cfg.Client = &http.Client{Timeout: exchangeTimeout}
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.SetupOutput == nil {
		cfg.SetupOutput = os.Stderr
	}

	h := &Handler{
		cfg:          cfg,
		log:          log,
		txns:         newTxnStore(cfg.Now),
		client:       cfg.Client,
		now:          cfg.Now,
		logoutEpochs: map[string]uint64{},
	}
	if cfg.EpochStore != nil {
		epochs, err := cfg.EpochStore.LoadLogoutEpochs(context.Background())
		if err != nil {
			// An unreadable epoch store is the status quo this mechanism
			// upgrades: sessions verify by signature and expiry, and the
			// first logout of this process re-establishes its epoch.
			log.Warn().Err(err).Msg("auth: logout epochs could not be loaded; pre-restart logouts do not bind this process")
		} else if epochs != nil {
			h.logoutEpochs = epochs
		}
	}

	switch cfg.Mode {
	case "password":
		if len(cfg.SessionSecret) < 32 {
			return nil, errors.New("auth: password mode requires a 32-byte session secret")
		}
		if _, err := bcrypt.Cost(cfg.PasswordHash); len(cfg.PasswordHash) != 0 && err != nil {
			return nil, fmt.Errorf("auth: password hash: %w", err)
		}
		h.credentials = BootstrapState{PasswordHash: cfg.PasswordHash, SessionSecret: cfg.SessionSecret}
		if cfg.PasswordStatePath != "" {
			result, err := readBootstrapState(cfg.PasswordStatePath)
			if err != nil {
				return nil, err
			}
			if !bytes.Equal(cfg.SessionSecret, result.State.SessionSecret) ||
				(len(cfg.PasswordHash) != 0 && !bytes.Equal(cfg.PasswordHash, result.State.PasswordHash)) {
				return nil, errors.New("auth: effective password configuration does not match durable state")
			}
			h.credentials = result.State
		} else if len(cfg.PasswordHash) == 0 {
			return nil, errors.New("auth: password mode requires durable setup state")
		}
		h.password = newPasswordGate(cfg.Now)
	case "oidc", "builtin":
		keys := oidc.NewRemoteKeySet(oidc.ClientContext(context.Background(), cfg.Client), cfg.JWKSURL)
		h.verifier = oidc.NewVerifier(cfg.Issuer, keys, &oidc.Config{
			ClientID:             cfg.ClientID,
			SupportedSigningAlgs: []string{oidc.RS256},
			Now:                  cfg.Now,
		})
	case "trusted_proxy":
		peers, err := parseTrustedPeers(cfg.ProxyAddresses)
		if err != nil {
			return nil, err
		}
		h.proxies = peers
	case "none":
		// Explicit loopback-only opt-out; config validation owns the declared
		// origin gate. This is not a network ACL.
	default:
		return nil, fmt.Errorf("auth: unknown mode %q", cfg.Mode)
	}
	return h, nil
}

// Mount registers the authenticated session probe for every mode and the
// OIDC login ceremony. The browser uses the probe before it renders any
// application chrome, so authentication gates the whole app rather than an
// individual workspace region.
func (h *Handler) Mount(handle func(pattern string, handler http.Handler)) {
	probe := h.Require(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	handle("GET /auth/session", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// This response describes the browser's current authentication state.
		// It must never be reused across logout, expiry, or proxy identities.
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Noisedeck-Auth-Mode", h.cfg.Mode)
		probe.ServeHTTP(w, r)
	}))
	if h.cfg.Mode == "password" {
		handle("POST /auth/password", http.HandlerFunc(h.handlePasswordLogin))
		handle("POST /auth/setup/check", http.HandlerFunc(h.handleSetupCheck))
		handle("POST /auth/setup", http.HandlerFunc(h.handleSetup))
		handle("POST /auth/password/change", h.Require(http.HandlerFunc(h.handlePasswordChange)))
		handle("POST /auth/logout", http.HandlerFunc(h.handleLogout))
		return
	}
	if h.cfg.Mode != "oidc" && h.cfg.Mode != "builtin" {
		return
	}
	handle("GET /auth/login", http.HandlerFunc(h.handleLogin))
	handle("GET /auth/callback", http.HandlerFunc(h.handleCallback))
	handle("POST /auth/logout", http.HandlerFunc(h.handleLogout))
}

func (h *Handler) txnCookieName() string {
	if h.cfg.SecureCookies {
		return txnHostName
	}
	return txnCookie
}

func (h *Handler) handleLogin(w http.ResponseWriter, r *http.Request) {
	state, err1 := randomToken()
	nonce, err2 := randomToken()
	verifier, err3 := randomToken()
	if err1 != nil || err2 != nil || err3 != nil {
		http.Error(w, "login unavailable", http.StatusInternalServerError)
		return
	}
	id, err := h.txns.put(transaction{state: state, nonce: nonce, verifier: verifier})
	if err != nil {
		http.Error(w, "login unavailable", http.StatusInternalServerError)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:  h.txnCookieName(),
		Value: id,
		// Path=/ is mandatory for a __Host- cookie; browsers reject the
		// prefix with any other path. The cookie is HttpOnly and consumed
		// once at callback, so the wider path costs nothing.
		Path:     "/",
		MaxAge:   int(txnTTL.Seconds()),
		HttpOnly: true,
		Secure:   h.cfg.SecureCookies,
		SameSite: http.SameSiteLaxMode,
	})

	sum := sha256.Sum256([]byte(verifier))
	q := url.Values{
		"client_id":             {h.cfg.ClientID},
		"redirect_uri":          {h.cfg.RedirectURL},
		"response_type":         {"code"},
		"scope":                 {"openid email"},
		"state":                 {state},
		"nonce":                 {nonce},
		"code_challenge":        {base64.RawURLEncoding.EncodeToString(sum[:])},
		"code_challenge_method": {"S256"},
	}
	if r.URL.Query().Get("choose_account") == "1" {
		q.Set("prompt", "select_account")
	}
	http.Redirect(w, r, h.cfg.AuthURL+"?"+q.Encode(), http.StatusFound)
}

// handleCallback completes a login. Every refusal is the same generic 401:
// which check failed is for the log, not the response.
func (h *Handler) handleCallback(w http.ResponseWriter, r *http.Request) {
	refuse := func(reason string, err error) {
		h.log.Warn().Str("reason", reason).Err(err).Msg("login refused")
		h.clearTxnCookie(w)
		http.Error(w, "login failed", http.StatusUnauthorized)
	}

	cookie, err := r.Cookie(h.txnCookieName())
	if err != nil {
		refuse("no transaction cookie", nil)
		return
	}
	txn, ok := h.txns.take(cookie.Value)
	h.clearTxnCookie(w)
	if !ok {
		refuse("unknown, reused, or expired transaction", nil)
		return
	}
	if subtle.ConstantTimeCompare([]byte(r.URL.Query().Get("state")), []byte(txn.state)) != 1 {
		refuse("state mismatch", nil)
		return
	}
	code := r.URL.Query().Get("code")
	if code == "" || len(code) > maxCodeLength {
		refuse("missing or oversized code", nil)
		return
	}

	rawIDToken, err := h.exchange(r.Context(), code, txn.verifier)
	if err != nil {
		refuse("token exchange", err)
		return
	}
	idToken, err := h.verifier.Verify(oidc.ClientContext(r.Context(), h.client), rawIDToken)
	if err != nil {
		refuse("id token verification", err)
		return
	}

	// go-oidc's Verify checks the signature, issuer, expiry, and that the
	// audience CONTAINS the client id. This product is stricter: an
	// exact single audience, an exact issuer, a bounded issued-at,
	// and a subject shape — all re-checked here after cryptographic
	// verification so a token minted for a different app, replayed from the
	// future, or with a padded audience list is refused.
	// AZP is decoded as json.RawMessage so an explicit null or a non-string
	// type is distinguishable from an absent claim — each of those is a
	// malformed token, not "azp omitted".
	var claims struct {
		Nonce         string          `json:"nonce"`
		Email         string          `json:"email"`
		EmailVerified bool            `json:"email_verified"`
		AZP           json.RawMessage `json:"azp"`
	}
	if err := idToken.Claims(&claims); err != nil {
		refuse("claims decode", err)
		return
	}
	azp, azpPresent, azpOK := decodeAZP(claims.AZP)
	now := h.now()
	switch {
	case idToken.Issuer != h.cfg.Issuer:
		refuse("issuer mismatch", nil)
		return
	case len(idToken.Audience) != 1 || idToken.Audience[0] != h.cfg.ClientID:
		refuse("audience is not exactly the client id", nil)
		return
	case idToken.IssuedAt.IsZero() || idToken.IssuedAt.After(now.Add(txnSkew)):
		refuse("issued-at missing or in the future", nil)
		return
	case !validSubject(idToken.Subject):
		refuse("subject shape", nil)
		return
	case subtle.ConstantTimeCompare([]byte(claims.Nonce), []byte(txn.nonce)) != 1:
		refuse("nonce mismatch", nil)
		return
	case !azpOK:
		refuse("authorized party is present but not a string", nil)
		return
	case azpPresent && azp != h.cfg.ClientID:
		// Present means present: an empty-string azp is not "absent", and
		// it is not the client id, so it is refused.
		refuse("authorized party mismatch", nil)
		return
	case !claims.EmailVerified:
		refuse("email not verified", nil)
		return
	case !strings.EqualFold(claims.Email, h.cfg.AllowedEmail):
		refuse("email is not the allowed email", nil)
		return
	}

	value, err := h.issueLoginSession(now, now, Login{
		Issuer: idToken.Issuer, Subject: idToken.Subject, Email: claims.Email,
	})
	if err != nil {
		refuse("session issue", err)
		return
	}
	payload, err := h.verifySessionValue(value, h.now())
	if err != nil {
		refuse("session self-check", err)
		return
	}
	h.setSessionCookies(w, value, payload)
	// The destination is fixed: nothing about it comes from the request.
	http.Redirect(w, r, postLoginLocation, http.StatusSeeOther)
}

func (h *Handler) handleLogout(w http.ResponseWriter, r *http.Request) {
	payload, ok := h.csrfPayload(r)
	if !ok {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	login, hasLogin := loginFromPayload(payload)
	if h.onLogout != nil {
		h.onLogout(principalHash(legacySingleUserPrincipal))
	}
	if h.onBrowserLogout != nil && hasLogin {
		h.onBrowserLogout(login)
	}
	// The epoch bump is what makes this logout final: a session renewal
	// already in flight when this response left carries the pre-logout
	// epoch, and its cookie verifies stale no matter when the browser
	// applies it. Legacy sessions share the "" epoch key.
	if hasLogin {
		h.bumpLogoutEpoch(login.ID)
	} else {
		h.bumpLogoutEpoch("")
	}
	h.clearSessionCookies(w)
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) clearTxnCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name: h.txnCookieName(), Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, Secure: h.cfg.SecureCookies, SameSite: http.SameSiteLaxMode,
	})
}

// exchange redeems the authorization code: one bounded POST to the fixed
// token endpoint. The response is size-capped and only the id_token is
// read; access and refresh tokens are dropped on the floor by design.
func (h *Handler) exchange(ctx context.Context, code, verifier string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, exchangeTimeout)
	defer cancel()
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"client_id":     {h.cfg.ClientID},
		"client_secret": {h.cfg.ClientSecret},
		"redirect_uri":  {h.cfg.RedirectURL},
		"code_verifier": {verifier},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.cfg.TokenURL,
		strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := h.client.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxTokenResponse))
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("token endpoint status %d", resp.StatusCode)
	}
	var tr struct {
		IDToken string `json:"id_token"`
	}
	if err := json.Unmarshal(body, &tr); err != nil {
		return "", fmt.Errorf("token response decode: %w", err)
	}
	if tr.IDToken == "" || len(tr.IDToken) > 16384 {
		return "", fmt.Errorf("missing or oversized id_token")
	}
	return tr.IDToken, nil
}

// validSubject requires a non-empty, ≤255-char subject of printable ASCII
// with no control characters or spaces. Google's `sub` is an opaque
// identifier of that shape; a control or non-ASCII subject is malformed and
// a log-injection vector.
func validSubject(sub string) bool {
	if sub == "" || len(sub) > 255 {
		return false
	}
	for _, r := range sub {
		if r < 0x21 || r > 0x7e {
			return false
		}
	}
	return true
}

// decodeAZP distinguishes three cases of the optional azp claim: ABSENT
// (fine), PRESENT-and-a-string (must equal the client id — including the
// empty string, which is present and not the client id), and MALFORMED (a
// present null or non-string type). Returns the value, whether azp was
// present at all, and whether the token is well-formed with respect to azp.
func decodeAZP(raw json.RawMessage) (value string, present, ok bool) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return "", false, true // absent — azp is optional
	}
	if string(trimmed) == "null" {
		// A present `null` is malformed: json.Unmarshal into a string would
		// accept it silently as "", so it must be rejected explicitly.
		return "", true, false
	}
	var s string
	if err := json.Unmarshal(trimmed, &s); err != nil {
		return "", true, false // present but not a string
	}
	return s, true, true
}

func randomToken() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b[:]), nil
}
