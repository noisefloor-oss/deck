package workspacegateway

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/rs/zerolog"

	"github.com/noisefloor-oss/deck/internal/auth"
	"github.com/noisefloor-oss/deck/internal/server"
	"github.com/noisefloor-oss/deck/internal/workspacekeys"
)

const apiPrefix = "/api/security/v1"
const enrollmentCookie = "__Host-deck_enrollment"
const invitationCookie = "__Host-deck_invitation"

type Config struct {
	InstanceID string
	Origin     string
	RPID       string
	Auth       auth.Config
}

// RuntimeState contains no session names, paths, or process details. An
// observation failure is unknown, never evidence that a workspace is locked.
type RuntimeState struct {
	Storage   string `json:"storage"`
	Runtime   string `json:"runtime"`
	Locked    bool   `json:"locked"`
	Migration string `json:"migration,omitempty"`
}

// RuntimeObserver must inspect the native controller, not infer state from a
// browser grant or the last operation written to the control store.
type RuntimeObserver interface {
	ObserveSecurity(context.Context) (RuntimeState, error)
}

// Operations receives only a registry-authorized operation. A raw volume secret
// is transient handoff material and must not enter operation records or errors.
type Operations interface {
	Execute(context.Context, auth.Login, workspacekeys.AuthorizedOperation, []byte) (*BrowserAuthorization, error)
	Status(context.Context, auth.Login, string) (OperationState, error)
}

// BrowserAuthorization is transient cookie delivery after the grant store has
// committed its hash. It is never part of an operation's JSON representation.
type BrowserAuthorization struct {
	token   string
	expires time.Time
}

func NewBrowserAuthorization(token string, expires time.Time) (*BrowserAuthorization, error) {
	raw, err := base64.RawURLEncoding.Strict().DecodeString(token)
	if err != nil || len(raw) != 32 || expires.IsZero() {
		return nil, errors.New("invalid browser authorization")
	}
	clear(raw)
	return &BrowserAuthorization{token: token, expires: expires}, nil
}

func (*BrowserAuthorization) String() string { return "workspacegateway.BrowserAuthorization" }

type OperationState struct {
	ID     string `json:"id"`
	Status string `json:"status"`
	RuntimeState
}

// BrowserAccess verifies access for the installed protection mode. Encrypted
// workspaces require the independent method-backed bearer; explicitly
// unencrypted workspaces require the pinned owner and observed private runtime.
type BrowserAccess interface {
	Require(context.Context, auth.Login) error
	Logout(context.Context, auth.Login) error
}

type Options struct {
	Assets          fs.FS
	BuildID         string
	Runtime         RuntimeObserver
	Operations      Operations
	PlainOperations PlainOperations
	Access          BrowserAccess
	Application     http.Handler
}

type Handler struct {
	cfg     Config
	keys    *workspacekeys.Registry
	invites *Invitations
	options Options
	auth    *auth.Handler
	mux     *http.ServeMux
}

func (c Config) Validate() error {
	u, err := url.Parse(c.Origin)
	if err != nil || u.Scheme != "https" || u.Hostname() != c.RPID || u.Host == "" || u.User != nil ||
		u.Path != "" || u.RawQuery != "" || u.Fragment != "" || c.InstanceID == "" || c.RPID == "" {
		return errors.New("workspace gateway requires a fixed HTTPS origin and RP ID")
	}
	if c.Auth.Mode != "oidc" || !c.Auth.SecureCookies || len(c.Auth.SessionSecret) < 32 ||
		!validEmail(c.Auth.AllowedEmail) || c.Auth.ClientID == "" || c.Auth.ClientSecret == "" ||
		c.Auth.RedirectURL != c.Origin+"/auth/callback" ||
		(c.Auth.Issuer != "" && c.Auth.Issuer != "https://accounts.google.com") ||
		c.Auth.AuthURL != "" || c.Auth.TokenURL != "" || c.Auth.JWKSURL != "" ||
		len(c.Auth.ProxyAddresses) != 0 || c.Auth.IdentityHeader != "" || len(c.Auth.PasswordHash) != 0 {
		return errors.New("workspace gateway requires Google sign-in with secure cookies")
	}
	return nil
}

func New(cfg Config, keys *workspacekeys.Registry, invites *Invitations, options Options) (*Handler, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if keys == nil || invites == nil || options.Assets == nil || invites.keys != keys ||
		!strings.EqualFold(invites.email, cfg.Auth.AllowedEmail) || !keys.CheckConfiguration(cfg.InstanceID, cfg.RPID, cfg.Origin) {
		return nil, errors.New("workspace gateway configuration incomplete")
	}
	authHandler, err := auth.New(cfg.Auth, zerolog.Nop())
	if err != nil {
		return nil, err
	}
	h := &Handler{cfg: cfg, keys: keys, invites: invites, options: options, auth: authHandler, mux: http.NewServeMux()}
	authHandler.Mount(func(pattern string, handler http.Handler) {
		if pattern == "POST /auth/logout" {
			h.mux.Handle(pattern, h.logout())
			return
		}
		h.mux.Handle(pattern, handler)
	})
	h.route("GET /state", h.state)
	h.mux.HandleFunc("GET "+apiPrefix+"/onboarding", h.onboarding)
	h.route("POST /onboarding/protection", h.selectProtection)
	h.route("POST /onboarding/start", h.plainStart)
	h.route("GET /onboarding/operations/{id}", h.plainStatus)
	h.mux.HandleFunc("POST "+apiPrefix+"/invitation/handoff", h.invitationHandoff)
	h.route("POST /invitation/redeem", h.redeem)
	h.route("GET /unlock-options", h.unlockOptions)
	h.route("POST /methods/prepare", h.prepare)
	h.route("PATCH /methods/{id}/label", h.labelPending)
	h.route("PUT /methods/{id}/envelope", h.storeEnvelope)
	h.route("GET /methods/{id}/envelope", h.envelope)
	h.route("GET /methods", h.methods)
	h.route("POST /challenges", h.challenge)
	h.route("POST /operations", h.operation)
	h.route("GET /operations/{id}", h.operationStatus)
	if options.Application != nil {
		h.mux.Handle("/api/", h.auth.Require(options.Application))
	}
	h.mux.HandleFunc("GET /livez", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	static := server.StaticHandler(options.Assets, options.BuildID)
	h.mux.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			fail(w, http.StatusMethodNotAllowed, "method-not-allowed")
			return
		}
		static.ServeHTTP(w, r)
	}))
	return h, nil
}

func (h *Handler) route(pattern string, handler http.HandlerFunc) {
	method, path, _ := strings.Cut(pattern, " ")
	h.mux.Handle(method+" "+apiPrefix+path, h.auth.Require(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		login, ok := auth.CurrentLogin(r.Context())
		if !ok || login.Issuer != "https://accounts.google.com" || !strings.EqualFold(login.Email, h.cfg.Auth.AllowedEmail) {
			fail(w, http.StatusUnauthorized, "sign-in-required")
			return
		}
		handler.ServeHTTP(w, r)
	})))
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Deck-Workspace-Security", "v1")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	u, _ := url.Parse(h.cfg.Origin)
	if r.Host != u.Host {
		fail(w, http.StatusMisdirectedRequest, "wrong-origin")
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead && r.Method != http.MethodOptions {
		if r.Header.Get("Origin") != h.cfg.Origin {
			fail(w, http.StatusForbidden, "wrong-origin")
			return
		}
	}
	if r.Header.Get("Sec-Fetch-Site") == "cross-site" && strings.HasPrefix(r.URL.Path, "/api/") {
		fail(w, http.StatusForbidden, "wrong-origin")
		return
	}
	h.mux.ServeHTTP(w, r)
}

func (h *Handler) logout() http.Handler {
	return h.auth.Require(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		login, ok := auth.CurrentLogin(r.Context())
		if ok {
			bound, err := h.keys.OwnerMatches(r.Context(), login)
			if err != nil {
				keyError(w, err)
				return
			}
			if bound {
				if err := h.keys.RevokeEnrollment(r.Context(), login); err != nil {
					keyError(w, err)
					return
				}
			}
			if bound && h.options.Access != nil {
				if err := h.options.Access.Logout(r.Context(), login); err != nil {
					fail(w, http.StatusServiceUnavailable, "sign-out-unconfirmed")
					return
				}
			}
		}
		h.auth.RevokeLogin(login)
		for _, name := range []string{invitationCookie, enrollmentCookie, "__Host-deck_workspace"} {
			http.SetCookie(w, &http.Cookie{Name: name, Path: "/", MaxAge: -1, Secure: true, HttpOnly: true, SameSite: http.SameSiteStrictMode})
		}
		h.auth.ClearSessionCookies(w)
		w.WriteHeader(http.StatusNoContent)
	}))
}

func actor(r *http.Request) workspacekeys.Actor {
	login, _ := auth.CurrentLogin(r.Context())
	var receipt string
	if c, err := r.Cookie(enrollmentCookie); err == nil {
		receipt = c.Value
	}
	return workspacekeys.NewActor(login, receipt)
}

func (h *Handler) state(w http.ResponseWriter, r *http.Request) {
	login, _ := auth.CurrentLogin(r.Context())
	state, err := h.keys.State(r.Context(), login)
	if err != nil {
		keyError(w, err)
		return
	}
	view := RuntimeState{Storage: "unknown", Runtime: "unknown"}
	if h.options.Runtime != nil {
		if observed, err := h.options.Runtime.ObserveSecurity(r.Context()); err == nil && validRuntimeState(observed) {
			view = observed
		}
	}
	phase := "invitation-required"
	if state.OwnerBound {
		phase = "awaiting-method"
	}
	if state.OwnerBound && !state.Initialized && state.Onboarding.Protection != workspacekeys.ProtectionNone {
		methods, err := h.keys.Methods(r.Context(), actor(r))
		if err != nil {
			keyError(w, err)
			return
		}
		for _, method := range methods {
			if method.Kind != workspacekeys.Recovery && method.Status == "pending" && method.VerifiedAt != nil {
				phase = "awaiting-recovery"
			}
		}
	}
	if state.InitializingOperation != "" {
		phase = "initializing"
	}
	var initializingMethod *workspacekeys.MethodRef
	if state.InitializingOperation != "" {
		if pending, err := h.keys.PendingInitialization(r.Context()); err == nil && pending.ID == state.InitializingOperation {
			ref := pending.Action.Method
			initializingMethod = &ref
		}
	}
	if state.Initialized {
		phase = "ready"
	}
	if state.OwnerBound && state.Onboarding.Protection == workspacekeys.ProtectionUndecided && state.Onboarding.Policy != workspacekeys.EncryptionRequired {
		phase = "awaiting-protection"
	}
	if state.Onboarding.Protection == workspacekeys.ProtectionNone {
		phase = "awaiting-start"
		if state.Onboarding.SetupComplete {
			phase = "ready"
		}
	}
	eligible := state.Onboarding.Protection == workspacekeys.ProtectionEncrypted && state.Initialized || state.Onboarding.Protection == workspacekeys.ProtectionNone && state.Onboarding.SetupComplete
	authorized := eligible && h.options.Access != nil && h.options.Access.Require(r.Context(), login) == nil
	granted := state.Initialized && authorized
	respond(w, http.StatusOK, struct {
		InstanceID            string                   `json:"instanceId"`
		Phase                 string                   `json:"phase"`
		Commitment            string                   `json:"commitment,omitempty"`
		InitializingOperation string                   `json:"initializingOperation,omitempty"`
		InitializingMethod    *workspacekeys.MethodRef `json:"initializingMethod,omitempty"`
		RuntimeState
		BrowserGranted      bool                          `json:"browserGranted"`
		OwnerBound          bool                          `json:"ownerBound"`
		Onboarding          workspacekeys.OnboardingState `json:"onboarding"`
		PlainOperationID    string                        `json:"plainOperationId,omitempty"`
		WorkspaceAuthorized bool                          `json:"workspaceAuthorized"`
	}{h.cfg.InstanceID, phase, state.Commitment, state.InitializingOperation, initializingMethod, view, granted, state.OwnerBound, state.Onboarding, state.PlainOperationID, authorized})
}

func validRuntimeState(s RuntimeState) bool {
	return (s.Migration == "" || s.Migration == "pending" || s.Migration == "complete") && (s.Storage == "closed" || s.Storage == "open" || s.Storage == "unencrypted" || s.Storage == "unknown") &&
		(s.Runtime == "stopped" || s.Runtime == "starting" || s.Runtime == "running" || s.Runtime == "stopping" || s.Runtime == "unknown") &&
		(!s.Locked || s.Storage == "closed" && s.Runtime == "stopped")
}

// invitationHandoff preserves an invitation across Google's redirect without
// putting it in the redirect URL or JavaScript storage. It grants no enrollment
// authority and does not consume or validate the invitation's stored record.
func (h *Handler) invitationHandoff(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Token string `json:"token"`
	}
	if !decode(w, r, &request) {
		return
	}
	raw, err := base64.RawURLEncoding.Strict().DecodeString(request.Token)
	if err != nil || len(raw) != 32 {
		clear(raw)
		fail(w, http.StatusBadRequest, "invalid-request")
		return
	}
	clear(raw)
	http.SetCookie(w, &http.Cookie{Name: invitationCookie, Value: request.Token, Path: "/", Secure: true,
		HttpOnly: true, SameSite: http.SameSiteLaxMode, MaxAge: 15 * 60})
	request.Token = ""
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) redeem(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Token string `json:"token"`
	}
	if !decode(w, r, &request) {
		return
	}
	cookies := r.CookiesNamed(invitationCookie)
	if len(cookies) > 1 || len(cookies) == 1 && request.Token != "" && request.Token != cookies[0].Value {
		fail(w, http.StatusBadRequest, "invalid-request")
		return
	}
	if request.Token == "" && len(cookies) == 1 {
		request.Token = cookies[0].Value
	}
	login, _ := auth.CurrentLogin(r.Context())
	receipt, err := h.invites.Redeem(r.Context(), login, request.Token)
	request.Token = ""
	if err != nil {
		fail(w, http.StatusForbidden, "invitation-unavailable")
		return
	}
	setReceipt(w, receipt)
	http.SetCookie(w, &http.Cookie{Name: invitationCookie, Path: "/", Secure: true, HttpOnly: true,
		SameSite: http.SameSiteLaxMode, MaxAge: -1})
	respond(w, http.StatusOK, struct {
		Revision uint32 `json:"revision"`
	}{receipt.Revision})
}

func setReceipt(w http.ResponseWriter, receipt workspacekeys.EnrollmentReceipt) {
	http.SetCookie(w, &http.Cookie{Name: enrollmentCookie, Value: receipt.Token(), Path: "/", Secure: true,
		HttpOnly: true, SameSite: http.SameSiteStrictMode, Expires: receipt.ExpiresAt})
}

func (h *Handler) unlockOptions(w http.ResponseWriter, r *http.Request) {
	methods, err := h.keys.Methods(r.Context(), actor(r))
	if err != nil {
		keyError(w, err)
		return
	}
	for i := range methods {
		methods[i].Label = ""
		methods[i].VerifiedAt = nil
	}
	respond(w, http.StatusOK, methods)
}

func (h *Handler) prepare(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Kind  workspacekeys.Kind `json:"kind"`
		Label string             `json:"label"`
	}
	if !decode(w, r, &request) {
		return
	}
	prepared, err := h.keys.PrepareMethod(r.Context(), actor(r), request.Kind, request.Label)
	if err != nil {
		keyError(w, err)
		return
	}
	respond(w, http.StatusCreated, prepared)
}

func (h *Handler) labelPending(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Label string `json:"label"`
	}
	if !decode(w, r, &request) {
		return
	}
	if err := h.keys.LabelPending(r.Context(), actor(r), r.PathValue("id"), request.Label); err != nil {
		keyError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) storeEnvelope(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Envelope     json.RawMessage `json:"envelope"`
		Registration json.RawMessage `json:"registration,omitempty"`
		Commitment   string          `json:"commitment"`
	}
	if !decode(w, r, &request) {
		return
	}
	if err := h.keys.StoreEnvelope(r.Context(), actor(r), r.PathValue("id"), request.Envelope, request.Registration, request.Commitment); err != nil {
		keyError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) envelope(w http.ResponseWriter, r *http.Request) {
	method, err := h.keys.Method(r.Context(), actor(r), r.PathValue("id"))
	if err != nil {
		keyError(w, err)
		return
	}
	if method.Envelope == nil {
		fail(w, http.StatusNotFound, "envelope-unavailable")
		return
	}
	respond(w, http.StatusOK, method.Envelope)
}

func (h *Handler) methods(w http.ResponseWriter, r *http.Request) {
	login, _ := auth.CurrentLogin(r.Context())
	if h.options.Access == nil || h.options.Access.Require(r.Context(), login) != nil {
		fail(w, http.StatusForbidden, "unlock-required")
		return
	}
	methods, err := h.keys.Methods(r.Context(), actor(r))
	if err != nil {
		keyError(w, err)
		return
	}
	respond(w, http.StatusOK, methods)
}

func (h *Handler) challenge(w http.ResponseWriter, r *http.Request) {
	var action workspacekeys.Action
	if !decode(w, r, &action) {
		return
	}
	if h.options.Operations == nil && action.Operation != workspacekeys.VerifyMethod && action.Operation != workspacekeys.ResumeEnrollment {
		fail(w, http.StatusServiceUnavailable, "controller-unavailable")
		return
	}
	challenge, err := h.keys.BeginChallenge(r.Context(), actor(r), action)
	if err != nil {
		keyError(w, err)
		return
	}
	respond(w, http.StatusCreated, challenge)
}

func (h *Handler) operation(w http.ResponseWriter, r *http.Request) {
	var request struct {
		ChallengeID  string          `json:"challengeId"`
		Signature    string          `json:"signature"`
		Assertion    json.RawMessage `json:"assertion,omitempty"`
		VolumeSecret string          `json:"volumeSecret,omitempty"`
	}
	if !decode(w, r, &request) {
		return
	}
	action, err := h.keys.ChallengeAction(r.Context(), actor(r), request.ChallengeID)
	if err != nil {
		keyError(w, err)
		return
	}
	needsSecret := action.Operation == workspacekeys.Initialize || action.Operation == workspacekeys.ResumeInitialization || action.Operation == workspacekeys.Unlock
	if needsSecret != (request.VolumeSecret != "") {
		fail(w, http.StatusBadRequest, "invalid-request")
		return
	}
	if h.options.Operations == nil && action.Operation != workspacekeys.VerifyMethod && action.Operation != workspacekeys.ResumeEnrollment {
		fail(w, http.StatusServiceUnavailable, "controller-unavailable")
		return
	}
	var secret []byte
	if request.VolumeSecret != "" {
		var err error
		secret, err = base64.RawURLEncoding.Strict().DecodeString(request.VolumeSecret)
		request.VolumeSecret = ""
		if err != nil || len(secret) != 32 {
			fail(w, http.StatusBadRequest, "invalid-request")
			return
		}
		defer clear(secret)
		if h.options.Operations == nil {
			fail(w, http.StatusServiceUnavailable, "controller-unavailable")
			return
		}
		login, _ := auth.CurrentLogin(r.Context())
		state, err := h.keys.State(r.Context(), login)
		if err != nil {
			keyError(w, err)
			return
		}
		commitment, err := base64.RawURLEncoding.Strict().DecodeString(state.Commitment)
		if err != nil || len(commitment) != 32 {
			fail(w, http.StatusConflict, "state-changed")
			return
		}
		hash := sha256.New()
		_, _ = hash.Write([]byte("deck.volume.v1\x00"))
		_, _ = hash.Write(secret)
		if subtle.ConstantTimeCompare(hash.Sum(nil), commitment) != 1 {
			fail(w, http.StatusForbidden, "invalid-proof")
			return
		}
	}
	signature, err := base64.RawURLEncoding.Strict().DecodeString(request.Signature)
	if err != nil || len(signature) != 64 {
		fail(w, http.StatusBadRequest, "invalid-request")
		return
	}
	operation, err := h.keys.ConsumeProof(r.Context(), actor(r), request.ChallengeID, request.Signature, request.Assertion)
	if err != nil {
		keyError(w, err)
		return
	}
	if operation.Receipt != nil {
		setReceipt(w, *operation.Receipt)
	}
	if operation.Action.Operation != workspacekeys.VerifyMethod && operation.Action.Operation != workspacekeys.ResumeEnrollment {
		if h.options.Operations == nil {
			fail(w, http.StatusServiceUnavailable, "controller-unavailable")
			return
		}
		login, _ := auth.CurrentLogin(r.Context())
		authorization, err := h.options.Operations.Execute(r.Context(), login, operation, secret)
		if err != nil {
			respond(w, http.StatusAccepted, struct {
				ID     string `json:"id"`
				Status string `json:"status"`
			}{operation.ID, "unconfirmed"})
			return
		}
		if authorization != nil {
			http.SetCookie(w, &http.Cookie{Name: "__Host-deck_workspace", Value: authorization.token, Path: "/", Secure: true, HttpOnly: true, SameSite: http.SameSiteStrictMode, Expires: authorization.expires})
		}
	}
	respond(w, http.StatusOK, struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	}{operation.ID, "accepted"})
}

func (h *Handler) operationStatus(w http.ResponseWriter, r *http.Request) {
	if h.options.Operations == nil {
		fail(w, http.StatusServiceUnavailable, "controller-unavailable")
		return
	}
	login, _ := auth.CurrentLogin(r.Context())
	status, err := h.options.Operations.Status(r.Context(), login, r.PathValue("id"))
	if err != nil {
		fail(w, http.StatusNotFound, "operation-unavailable")
		return
	}
	respond(w, http.StatusOK, status)
}

func decode(w http.ResponseWriter, r *http.Request, target any) bool {
	media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || media != "application/json" {
		fail(w, http.StatusUnsupportedMediaType, "json-required")
		return false
	}
	reader := http.MaxBytesReader(w, r.Body, 192<<10)
	defer func() { _ = reader.Close() }()
	d := json.NewDecoder(reader)
	d.DisallowUnknownFields()
	if err := d.Decode(target); err != nil {
		fail(w, http.StatusBadRequest, "invalid-request")
		return false
	}
	if err := d.Decode(new(any)); err != io.EOF {
		fail(w, http.StatusBadRequest, "invalid-request")
		return false
	}
	return true
}

func respond(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func fail(w http.ResponseWriter, status int, code string) {
	respond(w, status, struct {
		Code string `json:"code"`
	}{code})
}

func keyError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, workspacekeys.ErrInvalid):
		fail(w, http.StatusBadRequest, "invalid-request")
	case errors.Is(err, workspacekeys.ErrDenied):
		fail(w, http.StatusForbidden, "authorization-required")
	case errors.Is(err, workspacekeys.ErrNotFound):
		fail(w, http.StatusNotFound, "method-unavailable")
	case errors.Is(err, workspacekeys.ErrConflict):
		fail(w, http.StatusConflict, "state-changed")
	default:
		fail(w, http.StatusServiceUnavailable, "security-unavailable")
	}
}
