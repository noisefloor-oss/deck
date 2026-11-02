package workspacegateway

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/noisefloor-oss/deck/internal/auth"
	"github.com/noisefloor-oss/deck/internal/workspacekeys"
)

const testOrigin = "https://workspace.example.test"

type fixture struct {
	h       *Handler
	db      *sql.DB
	keys    *workspacekeys.Registry
	invites *Invitations
	now     time.Time
	login   auth.Login
	config  Config
	path    string
}

func newFixture(t *testing.T, options Options) *fixture {
	return newFixtureWithPolicy(t, options, "")
}

func newFixtureWithPolicy(t *testing.T, options Options, policy workspacekeys.EncryptionPolicy) *fixture {
	t.Helper()
	f := &fixture{now: time.Date(2026, 9, 21, 0, 0, 0, 0, time.UTC), path: filepath.Join(t.TempDir(), "control.sqlite")}
	f.login = auth.Login{ID: strings.Repeat("a", 64), Issuer: "https://accounts.google.com", Subject: "fixture-owner", Email: "owner@example.test"}
	f.config = Config{InstanceID: "fixture-instance", Origin: testOrigin, RPID: "workspace.example.test", Auth: auth.Config{
		Mode: "oidc", ClientID: "fixture-client", ClientSecret: "fixture-not-a-real-google-secret",
		RedirectURL: testOrigin + "/auth/callback", AllowedEmail: f.login.Email, SessionSecret: bytes.Repeat([]byte{7}, 32), SecureCookies: true,
		Now: func() time.Time { return f.now },
	}}
	var err error
	f.db, err = OpenControl(f.path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.db.Close() })
	epochStore, err := NewLogoutEpochStore(context.Background(), f.db)
	if err != nil {
		t.Fatal(err)
	}
	f.config.Auth.EpochStore = epochStore
	f.keys, err = workspacekeys.New(context.Background(), f.db, workspacekeys.Config{InstanceID: f.config.InstanceID, RPID: f.config.RPID, Origin: f.config.Origin, Now: func() time.Time { return f.now }, EncryptionPolicy: policy})
	if err != nil {
		t.Fatal(err)
	}
	f.invites, err = NewInvitations(context.Background(), f.db, f.keys, f.login.Email, func() time.Time { return f.now })
	if err != nil {
		t.Fatal(err)
	}
	options.Assets = fstest.MapFS{"index.html": {Data: []byte("<title>Deck</title>Security shell")}}
	f.h, err = New(f.config, f.keys, f.invites, options)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// This deliberately models a host that knows Google's local session signing
// secret. Encryption authorization must still require an independent bearer.
func (f *fixture) signedRequest(t *testing.T, method, path string, body any, login auth.Login, cookies ...*http.Cookie) *http.Request {
	t.Helper()
	var encoded []byte
	if body != nil {
		var err error
		encoded, err = json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
	}
	r := httptest.NewRequest(method, testOrigin+path, bytes.NewReader(encoded))
	r.Header.Set("Origin", testOrigin)
	r.Header.Set("Content-Type", "application/json")
	fields := []string{"v3", fmt.Sprint(f.now.Unix()), fmt.Sprint(f.now.Add(time.Hour).Unix()), login.ID}
	for _, value := range []string{login.Issuer, login.Subject, login.Email} {
		fields = append(fields, base64.RawURLEncoding.EncodeToString([]byte(value)))
	}
	payload := strings.Join(fields, ":")
	sign := func(value string) string {
		mac := hmac.New(sha256.New, f.config.Auth.SessionSecret)
		_, _ = mac.Write([]byte(value))
		return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	}
	r.AddCookie(&http.Cookie{Name: "__Host-noisedeck_session", Value: base64.RawURLEncoding.EncodeToString([]byte(payload)) + "." + sign(payload)})
	r.Header.Set(auth.CSRFHeader, sign("csrf:"+payload))
	for _, cookie := range cookies {
		r.AddCookie(cookie)
	}
	return r
}

func (f *fixture) signedRequestWithEpoch(t *testing.T, method, path string, body any, login auth.Login, epoch uint64, cookies ...*http.Cookie) *http.Request {
	t.Helper()
	r := f.signedRequest(t, method, path, body, login, cookies...)
	originalCookies := r.Cookies()
	cookie, _ := r.Cookie("__Host-noisedeck_session")
	parts := strings.SplitN(cookie.Value, ".", 2)
	raw, _ := base64.RawURLEncoding.DecodeString(parts[0])
	fields := strings.Split(string(raw), ":")
	fields[0] = "v4"
	fields = append(fields, strconv.FormatUint(epoch, 10))
	payload := strings.Join(fields, ":")
	mac := hmac.New(sha256.New, f.config.Auth.SessionSecret)
	_, _ = mac.Write([]byte(payload))
	signature := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	cookie.Value = base64.RawURLEncoding.EncodeToString([]byte(payload)) + "." + signature
	r.Header.Del("Cookie")
	r.AddCookie(cookie)
	for _, original := range originalCookies {
		if original.Name != "__Host-noisedeck_session" {
			r.AddCookie(original)
		}
	}
	mac.Reset()
	_, _ = mac.Write([]byte("csrf:" + payload))
	r.Header.Set(auth.CSRFHeader, base64.RawURLEncoding.EncodeToString(mac.Sum(nil)))
	return r
}

func (f *fixture) serve(r *http.Request) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	f.h.ServeHTTP(w, r)
	return w
}

func (f *fixture) enroll(t *testing.T) *http.Cookie {
	t.Helper()
	token, _, err := f.invites.Issue(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	w := f.serve(f.signedRequest(t, "POST", apiPrefix+"/invitation/redeem", map[string]string{"token": token}, f.login))
	if w.Code != 200 {
		t.Fatalf("redeem status=%d body=%s", w.Code, w.Body)
	}
	for _, c := range w.Result().Cookies() {
		if c.Name == enrollmentCookie {
			return c
		}
	}
	t.Fatal("missing independent enrollment cookie")
	return nil
}

func TestGatewaySurvivesLockedRuntimeWithoutRevealingData(t *testing.T) {
	f := newFixture(t, Options{})
	if w := f.serve(httptest.NewRequest("GET", testOrigin+"/", nil)); w.Code != 200 || !strings.Contains(w.Body.String(), "Security shell") {
		t.Fatal("security shell unavailable without runtime")
	}
	if w := f.serve(httptest.NewRequest("GET", testOrigin+"/livez", nil)); w.Code != 204 {
		t.Fatal("gateway liveness depends on encrypted runtime")
	}
	invitationLink := httptest.NewRequest("GET", testOrigin+"/", nil)
	invitationLink.Header.Set("Sec-Fetch-Site", "cross-site")
	if w := f.serve(invitationLink); w.Code != 200 {
		t.Fatal("invitation link from another site cannot reach sign-in")
	}
	for _, path := range []string{apiPrefix + "/state", "/api/sessions", "/api/projects", "/api/tasks", "/api/ws", "/api/preview"} {
		w := f.serve(httptest.NewRequest("GET", testOrigin+path, nil))
		if w.Code == 200 {
			t.Fatalf("unauthenticated data route %s succeeded", path)
		}
	}
	w := f.serve(f.signedRequest(t, "GET", apiPrefix+"/state", nil, f.login))
	if w.Code != 200 {
		t.Fatal(w.Code)
	}
	var state map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &state); err != nil {
		t.Fatal(err)
	}
	if state["runtime"] != "unknown" || state["storage"] != "unknown" || state["browserGranted"] != false || state["phase"] != "invitation-required" {
		t.Fatalf("invented runtime/access state: %v", state)
	}
	if w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("security state cacheable")
	}
}

func TestInvitationGETCannotConsumeAndRedemptionIsAtomic(t *testing.T) {
	f := newFixture(t, Options{})
	token, expires, err := f.invites.Issue(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if expires.Sub(f.now) != 72*time.Hour {
		t.Fatal("wrong invitation expiry")
	}
	w := f.serve(f.signedRequest(t, "GET", apiPrefix+"/invitation/redeem", nil, f.login))
	if w.Code == 200 {
		t.Fatal("GET redemption succeeded")
	}
	wrong := f.login
	wrong.Email = "someone-else@example.test"
	if _, err := f.invites.Redeem(context.Background(), wrong, token); err == nil {
		t.Fatal("wrong email redeemed")
	}
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := f.invites.Redeem(context.Background(), f.login, token)
			results <- err
		}()
	}
	wg.Wait()
	close(results)
	success := 0
	for err := range results {
		if err == nil {
			success++
		}
	}
	if success != 1 {
		t.Fatalf("concurrent redemption succeeded %d times", success)
	}
	if _, _, err := f.invites.Issue(context.Background()); err == nil {
		t.Fatal("new invitation can replace bound owner")
	}
	var stored []byte
	if err := f.db.QueryRow(`SELECT hash FROM workspace_invitation`).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	raw, _ := base64.RawURLEncoding.DecodeString(token)
	if bytes.Equal(stored, raw) || bytes.Contains(stored, []byte(token)) {
		t.Fatal("invitation secret stored directly")
	}
}

func TestInvitationHandoffRequiresOriginButGrantsNoEnrollmentAuthority(t *testing.T) {
	f := newFixture(t, Options{})
	token, _, err := f.invites.Issue(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(map[string]string{"token": token})
	if err != nil {
		t.Fatal(err)
	}
	handoff := func(origin string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", testOrigin+apiPrefix+"/invitation/handoff", bytes.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Origin", origin)
		return f.serve(r)
	}
	for _, origin := range []string{"", "https://evil.example.test"} {
		w := handoff(origin)
		if w.Code != http.StatusForbidden || len(w.Result().Cookies()) != 0 {
			t.Fatal("cross-origin request set an invitation cookie")
		}
	}
	w := handoff(testOrigin)
	if w.Code != http.StatusNoContent || w.Body.Len() != 0 {
		t.Fatalf("handoff failed: %d", w.Code)
	}
	cookies := w.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatal("handoff did not set exactly one cookie")
	}
	cookie := cookies[0]
	if cookie.Name != invitationCookie || cookie.Value != token || !cookie.Secure || !cookie.HttpOnly || cookie.SameSite != http.SameSiteLaxMode || cookie.Path != "/" || cookie.Domain != "" || cookie.MaxAge != 900 {
		t.Fatal("handoff cookie lacks its redirect-safe private scope")
	}
	state, err := f.keys.State(context.Background(), f.login)
	if err != nil || state.OwnerBound || state.Initialized {
		t.Fatalf("unauthenticated handoff changed enrollment: %v", err)
	}
	r := httptest.NewRequest("POST", testOrigin+apiPrefix+"/invitation/redeem", strings.NewReader("{}"))
	r.Header.Set("Origin", testOrigin)
	r.Header.Set("Content-Type", "application/json")
	r.AddCookie(cookie)
	if w := f.serve(r); w.Code != http.StatusUnauthorized {
		t.Fatal("handoff alone redeemed the invitation")
	}
	wrong := f.login
	wrong.Email = "wrong@example.test"
	if w := f.serve(f.signedRequest(t, "POST", apiPrefix+"/invitation/redeem", map[string]string{}, wrong, cookie)); w.Code != http.StatusUnauthorized {
		t.Fatal("ineligible Google identity redeemed handoff")
	}
	w = f.serve(f.signedRequest(t, "POST", apiPrefix+"/invitation/redeem", map[string]string{}, f.login, cookie))
	if w.Code != http.StatusOK {
		t.Fatalf("eligible redemption failed: %d", w.Code)
	}
	cleared, receipt := false, false
	for _, c := range w.Result().Cookies() {
		cleared = cleared || c.Name == invitationCookie && c.MaxAge < 0
		receipt = receipt || c.Name == enrollmentCookie && c.Value != ""
	}
	if !cleared || !receipt {
		t.Fatal("redemption did not replace handoff with enrollment receipt")
	}
	if w := f.serve(f.signedRequest(t, "POST", apiPrefix+"/invitation/redeem", map[string]string{}, f.login, cookie)); w.Code != http.StatusForbidden {
		t.Fatal("handoff replay reused a spent invitation")
	}
}

func TestInvitationHandoffRejectsMalformedOrAmbiguousTokens(t *testing.T) {
	f := newFixture(t, Options{})
	for _, token := range []string{"", "short", strings.Repeat("a", 44)} {
		r := httptest.NewRequest("POST", testOrigin+apiPrefix+"/invitation/handoff", strings.NewReader(`{"token":"`+token+`"}`))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Origin", testOrigin)
		if w := f.serve(r); w.Code != http.StatusBadRequest || len(w.Result().Cookies()) != 0 {
			t.Fatal("malformed handoff token accepted")
		}
	}
	cookie := &http.Cookie{Name: invitationCookie, Value: base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{1}, 32))}
	for _, r := range []*http.Request{
		f.signedRequest(t, "POST", apiPrefix+"/invitation/redeem", map[string]string{}, f.login, cookie, cookie),
		f.signedRequest(t, "POST", apiPrefix+"/invitation/redeem", map[string]string{"token": base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{2}, 32))}, f.login, cookie),
	} {
		if w := f.serve(r); w.Code != http.StatusBadRequest {
			t.Fatal("ambiguous invitation selection accepted")
		}
	}
}

func TestInvitationExpiryAndSupersession(t *testing.T) {
	f := newFixture(t, Options{})
	old, _, err := f.invites.Issue(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	next, _, err := f.invites.Issue(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.invites.Redeem(context.Background(), f.login, old); err == nil {
		t.Fatal("superseded invitation works")
	}
	f.now = f.now.Add(72 * time.Hour)
	if _, err := f.invites.Redeem(context.Background(), f.login, next); err == nil {
		t.Fatal("expired invitation works")
	}
	state, err := f.keys.State(context.Background(), f.login)
	if err != nil || state.OwnerBound {
		t.Fatal("refused invitation still pinned owner")
	}
}

func TestForgedGoogleSessionCannotPrepareWithoutReceipt(t *testing.T) {
	f := newFixture(t, Options{})
	receipt := f.enroll(t)
	if !receipt.Secure || !receipt.HttpOnly || receipt.Domain != "" || receipt.Path != "/" || receipt.SameSite != http.SameSiteStrictMode {
		t.Fatal("enrollment cookie lacks host protection")
	}
	prepare := map[string]string{"kind": "password", "label": "My password"}
	path := apiPrefix + "/methods/prepare"
	if w := f.serve(f.signedRequest(t, "POST", path, prepare, f.login)); w.Code != 403 {
		t.Fatalf("Google-only preparation=%d", w.Code)
	}
	other := f.login
	other.ID = strings.Repeat("b", 64)
	if w := f.serve(f.signedRequest(t, "POST", path, prepare, other, receipt)); w.Code != 403 {
		t.Fatal("receipt transferred to another login")
	}
	if w := f.serve(f.signedRequest(t, "POST", path, prepare, f.login, receipt)); w.Code != 201 {
		t.Fatalf("receipt preparation=%d body=%s", w.Code, w.Body)
	}
	if w := f.serve(f.signedRequest(t, "GET", apiPrefix+"/methods", nil, f.login, receipt)); w.Code != 403 {
		t.Fatal("enrollment receipt grants management access")
	}
	w := f.serve(f.signedRequest(t, "GET", apiPrefix+"/unlock-options", nil, f.login))
	if w.Code != 200 || strings.Contains(w.Body.String(), "My password") {
		t.Fatal("unlock options disclosed private method label")
	}
	for _, path := range []string{f.path, f.path + "-wal"} {
		data, err := os.ReadFile(path)
		if err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		if bytes.Contains(data, []byte(receipt.Value)) {
			t.Fatal("raw enrollment bearer persisted")
		}
	}
	logout := f.serve(f.signedRequest(t, "POST", "/auth/logout", nil, f.login, receipt))
	if logout.Code != 204 {
		t.Fatal(logout.Code)
	}
	request := f.signedRequestWithEpoch(t, "POST", path, prepare, f.login, 1, receipt)
	if current, _ := request.Cookie(enrollmentCookie); current.Value != receipt.Value {
		t.Fatal("current-epoch request lost the enrollment receipt")
	}
	if w := f.serve(request); w.Code != 403 {
		t.Fatal("signed-out receipt remains usable with forged Google cookie")
	}
}

func TestSecurityMutationsRejectOriginCSRFMismatchAndUnknownFields(t *testing.T) {
	f := newFixture(t, Options{})
	receipt := f.enroll(t)
	for _, change := range []func(*http.Request){func(r *http.Request) { r.Header.Del(auth.CSRFHeader) }, func(r *http.Request) { r.Header.Set("Origin", "https://hostile.example.test") }, func(r *http.Request) { r.Host = "hostile.example.test" }} {
		r := f.signedRequest(t, "POST", apiPrefix+"/methods/prepare", map[string]string{"kind": "password", "label": "Password"}, f.login, receipt)
		change(r)
		if w := f.serve(r); w.Code < 400 {
			t.Fatal("request crossed origin/CSRF boundary")
		}
	}
	r := f.signedRequest(t, "POST", apiPrefix+"/methods/prepare", map[string]string{"kind": "password", "label": "Password", "owner": "arbitrary"}, f.login, receipt)
	if w := f.serve(r); w.Code != 400 {
		t.Fatal("unknown request field accepted")
	}
}

func TestOpenControlRefusesPublicPermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "public.sqlite")
	if err := os.WriteFile(path, nil, 0644); err != nil {
		t.Fatal(err)
	}
	if db, err := OpenControl(path); err == nil {
		_ = db.Close()
		t.Fatal("public control database accepted")
	}
	if db, err := OpenControl("relative.sqlite"); err == nil {
		_ = db.Close()
		t.Fatal("relative control database accepted")
	}
	link := filepath.Join(t.TempDir(), "link.sqlite")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if db, err := OpenControl(link); err == nil {
		_ = db.Close()
		t.Fatal("symlinked control store accepted")
	}
}

type logoutAccess struct {
	during func()
	err    error
}

func (*logoutAccess) Require(context.Context, auth.Login) error { return workspacekeys.ErrDenied }
func (a *logoutAccess) Logout(context.Context, auth.Login) error {
	if a.during != nil {
		a.during()
	}
	return a.err
}

func TestGatewayLogoutBeforeEnrollmentAndAcrossExpiry(t *testing.T) {
	f := newFixture(t, Options{})
	if w := f.serve(f.signedRequest(t, "POST", "/auth/logout", nil, f.login)); w.Code != 204 {
		t.Fatal("unbound owner cannot sign out")
	}
	f.login.ID = strings.Repeat("c", 64)
	f.enroll(t)
	f.h.options.Access = &logoutAccess{during: func() { f.now = f.now.Add(2 * time.Hour) }}
	w := f.serve(f.signedRequest(t, "POST", "/auth/logout", nil, f.login))
	if w.Code != 204 {
		t.Fatalf("logout lost admission during revocation: %d", w.Code)
	}
	cookies := map[string]*http.Cookie{}
	for _, cookie := range w.Result().Cookies() {
		cookies[cookie.Name] = cookie
	}
	for _, name := range []string{enrollmentCookie, "__Host-deck_workspace", "__Host-noisedeck_session", "__Host-noisedeck_csrf"} {
		if cookie := cookies[name]; cookie == nil || cookie.MaxAge != -1 {
			t.Fatalf("logout retained %s", name)
		}
	}
	f.h.options.Access = &logoutAccess{err: workspacekeys.ErrConflict}
	f.login.ID = strings.Repeat("d", 64)
	if w := f.serve(f.signedRequest(t, "POST", "/auth/logout", nil, f.login)); w.Code != 503 {
		t.Fatal("failed grant revocation reported logout success")
	}
}

func TestGatewayLogoutInvalidatesExistingSessions(t *testing.T) {
	f := newFixture(t, Options{})
	stale := f.signedRequest(t, "GET", apiPrefix+"/state", nil, f.login)
	if w := f.serve(stale); w.Code != 200 {
		t.Fatalf("pre-logout session = %d", w.Code)
	}
	if w := f.serve(f.signedRequest(t, "POST", "/auth/logout", nil, f.login)); w.Code != 204 {
		t.Fatalf("logout = %d", w.Code)
	}
	if w := f.serve(stale); w.Code != 401 {
		t.Fatalf("captured pre-logout session = %d, want 401", w.Code)
	}

	restarted, err := New(f.config, f.keys, f.invites, Options{Assets: fstest.MapFS{"index.html": {Data: []byte("<title>Deck</title>Security shell")}}})
	if err != nil {
		t.Fatal(err)
	}
	f.h = restarted
	if w := f.serve(stale); w.Code != 401 {
		t.Fatalf("restart resurrected the captured pre-logout session: %d", w.Code)
	}

	other := f.login
	other.ID = strings.Repeat("b", 64)
	if w := f.serve(f.signedRequest(t, "GET", apiPrefix+"/state", nil, other)); w.Code != 200 {
		t.Fatalf("logout invalidated a different login: %d", w.Code)
	}
}

type operationProbe struct {
	calls  int
	secret []byte
}

func (p *operationProbe) Execute(_ context.Context, _ auth.Login, _ workspacekeys.AuthorizedOperation, secret []byte) (*BrowserAuthorization, error) {
	p.calls++
	p.secret = secret
	return nil, nil
}
func (*operationProbe) Status(context.Context, auth.Login, string) (OperationState, error) {
	return OperationState{}, nil
}

func signChallenge(t *testing.T, key *ecdsa.PrivateKey, challenge string) string {
	t.Helper()
	value, err := base64.RawURLEncoding.DecodeString(challenge)
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(value)
	r, s, err := ecdsa.Sign(rand.Reader, key, hash[:])
	if err != nil {
		t.Fatal(err)
	}
	proof := make([]byte, 64)
	r.FillBytes(proof[:32])
	s.FillBytes(proof[32:])
	return base64.RawURLEncoding.EncodeToString(proof)
}

func (f *fixture) testedMethod(t *testing.T, a workspacekeys.Actor, kind workspacekeys.Kind, commitment string) (workspacekeys.MethodRef, *ecdsa.PrivateKey) {
	t.Helper()
	ctx := context.Background()
	prepared, err := f.keys.PrepareMethod(ctx, a, kind, string(kind))
	if err != nil {
		t.Fatal(err)
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	public, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	b64 := base64.RawURLEncoding.EncodeToString
	envelope := workspacekeys.Envelope{Version: 1, InstanceID: f.config.InstanceID, MethodID: prepared.Method.ID, Revision: 1, Kind: kind, Salt: prepared.Method.Salt,
		Nonce: b64(bytes.Repeat([]byte{3}, 12)), Ciphertext: b64(bytes.Repeat([]byte{4}, 128)), ProofPublicKey: b64(public)}
	if kind == workspacekeys.Password {
		iterations := uint32(600000)
		envelope.Iterations = &iterations
	}
	raw, err := json.Marshal(envelope)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.keys.StoreEnvelope(ctx, a, prepared.Method.ID, raw, nil, commitment); err != nil {
		t.Fatal(err)
	}
	challenge, err := f.keys.BeginChallenge(ctx, a, workspacekeys.Action{Operation: workspacekeys.VerifyMethod, Method: prepared.Method.MethodRef})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.keys.ConsumeProof(ctx, a, challenge.ID, signChallenge(t, key, challenge.Challenge), nil); err != nil {
		t.Fatal(err)
	}
	return prepared.Method.MethodRef, key
}

func TestInvalidNativeSecretDoesNotConsumeProofOrBeginInitialization(t *testing.T) {
	probe := &operationProbe{}
	f := newFixture(t, Options{Operations: probe})
	receipt := f.enroll(t)
	a := workspacekeys.NewActor(f.login, receipt.Value)
	secret := bytes.Repeat([]byte{6}, 32)
	hash := sha256.New()
	_, _ = hash.Write([]byte("deck.volume.v1\x00"))
	_, _ = hash.Write(secret)
	commitment := base64.RawURLEncoding.EncodeToString(hash.Sum(nil))
	method, key := f.testedMethod(t, a, workspacekeys.Password, commitment)
	recovery, _ := f.testedMethod(t, a, workspacekeys.Recovery, commitment)
	challenge, err := f.keys.BeginChallenge(context.Background(), a, workspacekeys.Action{Operation: workspacekeys.Initialize, Method: method, Recovery: &recovery})
	if err != nil {
		t.Fatal(err)
	}
	body := map[string]any{"challengeId": challenge.ID, "signature": signChallenge(t, key, challenge.Challenge)}
	path := apiPrefix + "/operations"
	if w := f.serve(f.signedRequest(t, "POST", path, body, f.login, receipt)); w.Code != 400 {
		t.Fatalf("missingsecret status=%d body=%s", w.Code, w.Body)
	}
	body["volumeSecret"] = base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{9}, 32))
	if w := f.serve(f.signedRequest(t, "POST", path, body, f.login, receipt)); w.Code != 403 {
		t.Fatalf("wrongsecret status=%d body=%s", w.Code, w.Body)
	}
	if probe.calls != 0 {
		t.Fatal("invalid secret reached native controller")
	}
	state, err := f.keys.State(context.Background(), f.login)
	if err != nil || state.InitializingOperation != "" {
		t.Fatal("invalid handoff stranded initialization")
	}
	body["volumeSecret"] = base64.RawURLEncoding.EncodeToString(secret)
	w := f.serve(f.signedRequest(t, "POST", path, body, f.login, receipt))
	if w.Code != 200 || probe.calls != 1 {
		t.Fatalf("correcthandoff status=%d calls=%d body=%s", w.Code, probe.calls, w.Body)
	}
	if !bytes.Equal(probe.secret, make([]byte, 32)) {
		t.Fatal("native handoff byte buffer retained after request")
	}
	if strings.Contains(w.Body.String(), body["volumeSecret"].(string)) {
		t.Fatal("response leaked native secret")
	}
	if w := f.serve(f.signedRequest(t, "POST", path, body, f.login, receipt)); w.Code < 400 {
		t.Fatal("native operation replay succeeded")
	}
	stateResponse := f.serve(f.signedRequest(t, "GET", apiPrefix+"/state", nil, f.login))
	var pending struct {
		InitializingOperation string                  `json:"initializingOperation"`
		InitializingMethod    workspacekeys.MethodRef `json:"initializingMethod"`
	}
	if stateResponse.Code != http.StatusOK || json.Unmarshal(stateResponse.Body.Bytes(), &pending) != nil ||
		pending.InitializingOperation != challenge.OperationID || pending.InitializingMethod != method {
		t.Fatal("interrupted setup did not expose its original public method reference")
	}
}

func TestGatewayApplicationRoutesCoexistWithStaticShell(t *testing.T) {
	f := newFixture(t, Options{Application: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := auth.CurrentLogin(r.Context()); !ok {
			t.Error("application missing verified login")
		}
		w.WriteHeader(http.StatusAccepted)
	})})
	for _, method := range []string{"GET", "HEAD", "POST", "PUT", "PATCH", "DELETE"} {
		w := f.serve(f.signedRequest(t, method, "/api/projects", nil, f.login))
		if w.Code != http.StatusAccepted {
			t.Fatalf("application %s status=%d", method, w.Code)
		}
	}
	if w := f.serve(f.signedRequest(t, "GET", apiPrefix+"/state", nil, f.login)); w.Code != http.StatusOK {
		t.Fatal("application fallback shadowed the security API")
	}
	if w := f.serve(f.signedRequest(t, "POST", "/", nil, f.login)); w.Code != http.StatusMethodNotAllowed {
		t.Fatal("static fallback accepted a mutation")
	}
}
