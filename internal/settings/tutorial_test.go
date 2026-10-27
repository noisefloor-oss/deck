package settings

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/noisefloor-oss/deck/internal/auth"
)

// mountTutorial wires the handler with no authentication: every request then
// lands in the installation-wide bucket, which is the honest scope of a
// single-tenant unauthenticated installation.
func mountTutorial(t *testing.T) (*Repository, *http.ServeMux) {
	t.Helper()
	repo, _ := newTestRepo(t)
	h := NewHandler(repo, zerolog.Nop())
	mux := http.NewServeMux()
	h.Mount(mux.Handle, func(next http.Handler) http.Handler { return next })
	return repo, mux
}

// mountTutorialBehindAuth wires the handler behind the real trusted-proxy
// middleware so tests observe the server-derived principal scope rather than
// trusting a context the test itself planted.
func mountTutorialBehindAuth(t *testing.T) *http.ServeMux {
	t.Helper()
	repo, _ := newTestRepo(t)
	h := NewHandler(repo, zerolog.Nop())
	authHandler, err := auth.New(auth.Config{
		Mode:           "trusted_proxy",
		ProxyAddresses: []string{"127.0.0.1"},
		IdentityHeader: "X-User",
	}, zerolog.Nop())
	if err != nil {
		t.Fatalf("auth.New: %v", err)
	}
	mux := http.NewServeMux()
	h.Mount(mux.Handle, authHandler.Require)
	return mux
}

func proxyRequest(t *testing.T, method, target, body, identity string) *http.Request {
	t.Helper()
	var reader *strings.Reader
	if body == "" {
		reader = strings.NewReader("")
	} else {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, target, reader)
	// The middleware trusts the header only from a configured peer.
	req.RemoteAddr = "127.0.0.1:443"
	if identity != "" {
		req.Header.Set("X-User", identity)
	}
	return req
}

func decodeTutorial(t *testing.T, rec *httptest.ResponseRecorder) tutorialResponse {
	t.Helper()
	var got tutorialResponse
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode response: %v (%s)", err, rec.Body)
	}
	return got
}

// First use is a missing record: 404, never a fabricated empty document.
func TestTutorialMissingReadsAsNotFound(t *testing.T) {
	_, mux := mountTutorial(t)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/settings/tutorial", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("GET status = %d (%s), want 404", rec.Code, rec.Body)
	}
}

func TestTutorialRoundTrip(t *testing.T) {
	_, mux := mountTutorial(t)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPut, "/api/settings/tutorial",
		strings.NewReader(`{"version":1,"dismissed":true,"lesson":"welcome","step":2,"done":["welcome"]}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT status = %d (%s)", rec.Code, rec.Body)
	}
	written := decodeTutorial(t, rec)
	if written.Scope != "installation" {
		t.Fatalf("unauthenticated write scope = %q, want installation", written.Scope)
	}

	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/settings/tutorial", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET status = %d (%s)", rec.Code, rec.Body)
	}
	got := decodeTutorial(t, rec)
	want := TutorialPreference{Version: 1, Dismissed: true, Lesson: "welcome", Step: 2, Done: []string{"welcome"}}
	if !reflect.DeepEqual(got.TutorialPreference, want) || got.Scope != "installation" {
		t.Fatalf("GET = %+v, want %+v at installation scope", got, want)
	}

	// An omitted done list stores as [], so readers never branch on null.
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPut, "/api/settings/tutorial",
		strings.NewReader(`{"version":1,"dismissed":false,"lesson":"welcome","step":0}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT without done status = %d (%s)", rec.Code, rec.Body)
	}
	if got := decodeTutorial(t, rec); got.Done == nil || len(got.Done) != 0 {
		t.Fatalf("omitted done decoded as %v, want a non-nil empty list", got.Done)
	}
}

// The scope comes from the authenticated principal, established by the
// middleware — never from a caller-supplied field. Two users are
// independent, the header is consumed so nothing downstream can read it,
// and an unauthenticated request is refused before any scope is consulted.
func TestTutorialScopeIsServerDerived(t *testing.T) {
	mux := mountTutorialBehindAuth(t)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, proxyRequest(t, http.MethodPut, "/api/settings/tutorial",
		`{"version":1,"dismissed":true,"lesson":"welcome","step":1,"done":[]}`, "alice"))
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT status = %d (%s)", rec.Code, rec.Body)
	}
	if got := decodeTutorial(t, rec); got.Scope != "user" {
		t.Fatalf("authenticated write scope = %q, want user", got.Scope)
	}

	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, proxyRequest(t, http.MethodGet, "/api/settings/tutorial", "", "bob"))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("bob GET status = %d, want 404 — alice's record must not leak across scopes", rec.Code)
	}

	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, proxyRequest(t, http.MethodGet, "/api/settings/tutorial", "", "alice"))
	if rec.Code != http.StatusOK {
		t.Fatalf("alice GET status = %d (%s)", rec.Code, rec.Body)
	}
	if got := decodeTutorial(t, rec); got.Lesson != "welcome" || got.Step != 1 || !got.Dismissed {
		t.Fatalf("alice GET = %+v, want her own record", got)
	}

	// No identity header from a trusted peer is an authentication failure,
	// not a fall-through into a shared scope.
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, proxyRequest(t, http.MethodGet, "/api/settings/tutorial", "", ""))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous GET status = %d, want 401", rec.Code)
	}

	legacy := proxyRequest(t, http.MethodGet, "/api/settings/tutorial", "", "alice")
	legacy.Header.Del("X-User")
	legacy.Header.Set("X-Operator", "alice")
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, legacy)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unconfigured legacy identity status = %d, want 401", rec.Code)
	}
}

func TestTutorialValidationRejects(t *testing.T) {
	bodies := map[string]string{
		"not json":           `not json`,
		"version zero":       `{"version":0,"lesson":"welcome"}`,
		"version newer":      `{"version":2,"lesson":"welcome"}`,
		"lesson missing":     `{"version":1,"step":0}`,
		"lesson too long":    `{"version":1,"lesson":"` + strings.Repeat("a", 65) + `"}`,
		"lesson bad charset": `{"version":1,"lesson":"Welcome Home"}`,
		"lesson path":        `{"version":1,"lesson":"../etc"}`,
		"step negative":      `{"version":1,"lesson":"welcome","step":-1}`,
		"step too large":     `{"version":1,"lesson":"welcome","step":100}`,
		"done entry bad":     `{"version":1,"lesson":"welcome","done":["ok","Bad One"]}`,
		"done too many": `{"version":1,"lesson":"welcome","done":[` + func() string {
			ids := make([]string, 33)
			for i := range ids {
				ids[i] = fmt.Sprintf(`"l%d"`, i)
			}
			return strings.Join(ids, ",")
		}() + `]}`,
	}
	for name, body := range bodies {
		t.Run(name, func(t *testing.T) {
			_, mux := mountTutorial(t)
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPut, "/api/settings/tutorial", strings.NewReader(body)))
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("PUT status = %d (%s), want 400", rec.Code, rec.Body)
			}
			rec = httptest.NewRecorder()
			mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/settings/tutorial", nil))
			if rec.Code != http.StatusNotFound {
				t.Fatalf("a rejected write stored a record: GET status = %d", rec.Code)
			}
		})
	}
}

// A record written by a build speaking another contract version is data
// nobody here may reinterpret: reads return it as stored, and a write from
// this build is refused rather than overwriting it.
func TestTutorialUnknownStoredVersionIsPreserved(t *testing.T) {
	repo, d := newTestRepo(t)
	ctx := context.Background()
	h := NewHandler(repo, zerolog.Nop())
	mux := http.NewServeMux()
	h.Mount(mux.Handle, func(next http.Handler) http.Handler { return next })

	const planted = `{"version":99,"dismissed":true,"lesson":"from-the-future","step":4,"done":[]}`
	plant := func() {
		if err := d.WithTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
			return putString(ctx, tx, tutorialKey(""), planted, repo.now())
		}); err != nil {
			t.Fatalf("plant: %v", err)
		}
	}
	plant()

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/settings/tutorial", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET status = %d (%s)", rec.Code, rec.Body)
	}
	if got := decodeTutorial(t, rec); got.Version != 99 || got.Lesson != "from-the-future" {
		t.Fatalf("GET = %+v, want the stored version-99 record returned as written", got)
	}

	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPut, "/api/settings/tutorial",
		strings.NewReader(`{"version":1,"dismissed":false,"lesson":"welcome","step":0,"done":[]}`)))
	if rec.Code != http.StatusConflict {
		t.Fatalf("PUT over an unknown-version record status = %d (%s), want 409", rec.Code, rec.Body)
	}
	var stored string
	if err := d.Reader().QueryRow(`SELECT value FROM settings WHERE key = ?`, tutorialKey("")).Scan(&stored); err != nil {
		t.Fatalf("read stored row: %v", err)
	}
	if stored != planted {
		t.Fatalf("stored row = %s, want the unknown-version record untouched", stored)
	}
}

// The tutorial document shares the settings table but not the safety
// settings document: neither write touches the other's keys, and a
// presentation write never enters the safety audit trail.
func TestTutorialWriteLeavesSafetySettingsAlone(t *testing.T) {
	repo, d := newTestRepo(t)
	ctx := context.Background()
	if err := repo.SetTutorial(ctx, "", TutorialPreference{
		Version: 1, Dismissed: true, Lesson: "welcome", Step: 1, Done: []string{"welcome"},
	}); err != nil {
		t.Fatalf("write tutorial: %v", err)
	}
	s, err := repo.Get(ctx)
	if err != nil {
		t.Fatalf("get settings: %v", err)
	}
	if s.SkipPermissions || s.SkipCloseConfirm || s.SkipRemoveProjectConfirm {
		t.Fatalf("a tutorial write flipped a safety setting: %+v", s)
	}
	if err := repo.Set(ctx, Settings{SkipCloseConfirm: true}); err != nil {
		t.Fatalf("write settings: %v", err)
	}
	p, found, err := repo.GetTutorial(ctx, "")
	if err != nil || !found {
		t.Fatalf("tutorial after settings write: %+v, found=%v, %v", p, found, err)
	}
	if !p.Dismissed || p.Lesson != "welcome" || p.Step != 1 {
		t.Fatalf("a settings write clobbered the tutorial record: %+v", p)
	}
	var audits int
	if err := d.Reader().QueryRow(`SELECT COUNT(*) FROM audit_events`).Scan(&audits); err != nil {
		t.Fatalf("count audit events: %v", err)
	}
	if audits != 1 {
		t.Fatalf("audit events = %d, want exactly 1 (the safety-settings change alone is audited)", audits)
	}
}

// A stored row that no longer decodes fails closed: reads error, writes are
// refused, and the bytes stay where they are for whoever can repair them.
func TestTutorialCorruptStoredRowFailsClosed(t *testing.T) {
	repo, d := newTestRepo(t)
	ctx := context.Background()
	h := NewHandler(repo, zerolog.Nop())
	mux := http.NewServeMux()
	h.Mount(mux.Handle, func(next http.Handler) http.Handler { return next })

	const planted = `{"version":1,"lesson":`
	if err := d.WithTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		return putString(ctx, tx, tutorialKey(""), planted, repo.now())
	}); err != nil {
		t.Fatalf("plant: %v", err)
	}

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/settings/tutorial", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("GET over a corrupt row status = %d, want 500", rec.Code)
	}
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPut, "/api/settings/tutorial",
		strings.NewReader(`{"version":1,"dismissed":true,"lesson":"welcome","step":0,"done":[]}`)))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("PUT over a corrupt row status = %d, want 500 — a write must not paper over it", rec.Code)
	}
	var stored string
	if err := d.Reader().QueryRow(`SELECT value FROM settings WHERE key = ?`, tutorialKey("")).Scan(&stored); err != nil {
		t.Fatalf("read stored row: %v", err)
	}
	if stored != planted {
		t.Fatalf("stored row = %q, want the corrupt row untouched", stored)
	}
}

func putTutorialAt(t *testing.T, repo *Repository, at string, body TutorialPreference) {
	t.Helper()
	ts, err := time.Parse(time.RFC3339, at)
	if err != nil {
		t.Fatal(err)
	}
	repo.now = func() time.Time { return ts }
	body.Version = TutorialPreferenceVersion
	body.normalize()
	if err := repo.SetTutorial(context.Background(), "", body); err != nil {
		t.Fatalf("SetTutorial: %v", err)
	}
}

func installationHistory(t *testing.T, repo *Repository) []TutorialStep {
	t.Helper()
	records, err := repo.TutorialHistories(context.Background())
	if err != nil {
		t.Fatalf("TutorialHistories: %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("records = %+v, want one", records)
	}
	return records[0].History
}

// History grows only when the position changes or a lesson is newly done;
// a repeated save of the same position adds nothing, and a server-side
// timestamp is used whatever the client sends.
func TestTutorialHistoryAppendsTransitions(t *testing.T) {
	repo, _ := newTestRepo(t)
	putTutorialAt(t, repo, "2026-01-02T03:04:05Z", TutorialPreference{Lesson: "a", Step: 0})
	putTutorialAt(t, repo, "2026-01-02T03:04:09Z", TutorialPreference{Lesson: "a", Step: 0, Dismissed: true})
	putTutorialAt(t, repo, "2026-01-02T03:05:00Z", TutorialPreference{Lesson: "a", Step: 1})
	putTutorialAt(t, repo, "2026-01-02T03:06:00Z", TutorialPreference{Lesson: "b", Step: 0, Done: []string{"a"}})
	putTutorialAt(t, repo, "2026-01-02T03:07:00Z", TutorialPreference{Lesson: "b", Step: 0, Done: []string{"a", "c"}})

	want := []TutorialStep{
		{"a", 0, "2026-01-02T03:04:05Z", "position"},
		{"a", 1, "2026-01-02T03:05:00Z", "position"},
		{"a", 1, "2026-01-02T03:06:00Z", "done"},
		{"b", 0, "2026-01-02T03:06:00Z", "position"},
		{"c", 0, "2026-01-02T03:07:00Z", "done"},
	}
	if got := installationHistory(t, repo); !reflect.DeepEqual(got, want) {
		t.Fatalf("history = %+v, want %+v", got, want)
	}
}

func TestTutorialHistoryKeepsLastTwoHundred(t *testing.T) {
	repo, _ := newTestRepo(t)
	base := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 250; i++ {
		putTutorialAt(t, repo, base.Add(time.Duration(i)*time.Second).Format(time.RFC3339),
			TutorialPreference{Lesson: "a", Step: i % 90})
	}
	got := installationHistory(t, repo)
	if len(got) != 200 {
		t.Fatalf("len = %d, want 200", len(got))
	}
	if got[0].At != base.Add(50*time.Second).Format(time.RFC3339) || got[199].Step != 249%90 {
		t.Fatalf("kept the wrong window: first %+v last %+v", got[0], got[199])
	}
}

// A record written before the history existed is valid, reads without it,
// and starts one on the next transition.
func TestTutorialHistoryOldRecord(t *testing.T) {
	repo, d := newTestRepo(t)
	err := d.WithTx(context.Background(), func(ctx context.Context, tx *sql.Tx) error {
		return putString(ctx, tx, tutorialKey(""),
			`{"version":1,"dismissed":false,"lesson":"a","step":2,"done":[]}`, time.Now())
	})
	if err != nil {
		t.Fatal(err)
	}
	got, found, err := repo.GetTutorial(context.Background(), "")
	if err != nil || !found || got.Step != 2 {
		t.Fatalf("GetTutorial = %+v %v %v", got, found, err)
	}
	if records, _ := repo.TutorialHistories(context.Background()); len(records) != 1 || len(records[0].History) != 0 {
		t.Fatalf("old record history = %+v, want empty", records)
	}
	putTutorialAt(t, repo, "2026-01-02T03:04:05Z", TutorialPreference{Lesson: "a", Step: 2})
	if got := installationHistory(t, repo); len(got) != 0 {
		t.Fatalf("an unchanged position wrote history: %+v", got)
	}
	putTutorialAt(t, repo, "2026-01-02T03:04:06Z", TutorialPreference{Lesson: "a", Step: 3})
	if got := installationHistory(t, repo); len(got) != 1 || got[0].Step != 3 {
		t.Fatalf("history = %+v", got)
	}
}

// A client that sends a history, or none, neither replaces nor erases the
// server's, and the API never returns it.
func TestTutorialHistoryIgnoresClient(t *testing.T) {
	repo, mux := mountTutorial(t)
	putTutorialAt(t, repo, "2026-01-02T03:04:05Z", TutorialPreference{Lesson: "a", Step: 0})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPut, "/api/settings/tutorial",
		strings.NewReader(`{"version":1,"dismissed":false,"lesson":"a","step":0,"done":[],"history":[{"lesson":"x","step":9,"at":"1999-01-01T00:00:00Z"}]}`)))
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), "history") {
		t.Fatalf("PUT = %d %s", rec.Code, rec.Body)
	}
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/settings/tutorial", nil))
	if strings.Contains(rec.Body.String(), "history") {
		t.Fatalf("GET exposes history: %s", rec.Body)
	}
	want := []TutorialStep{{"a", 0, "2026-01-02T03:04:05Z", "position"}}
	if got := installationHistory(t, repo); !reflect.DeepEqual(got, want) {
		t.Fatalf("history = %+v, want %+v", got, want)
	}
}

// A done-only update records the completion but keeps the stored position
// as the current one.
func TestTutorialHistoryDoneOnlyUpdateKeepsPosition(t *testing.T) {
	repo, _ := newTestRepo(t)
	putTutorialAt(t, repo, "2026-01-02T03:00:00Z", TutorialPreference{Lesson: "b", Step: 0, Done: []string{"a"}})
	putTutorialAt(t, repo, "2026-01-02T03:01:00Z", TutorialPreference{Lesson: "b", Step: 0, Done: []string{"a", "c"}})
	putTutorialAt(t, repo, "2026-01-02T03:03:00Z", TutorialPreference{Lesson: "b", Step: 1, Done: []string{"a", "c"}})
	var positions []TutorialStep
	for _, e := range installationHistory(t, repo) {
		if e.IsPosition() {
			positions = append(positions, e)
		}
	}
	want := []TutorialStep{
		{"b", 0, "2026-01-02T03:00:00Z", "position"},
		{"b", 1, "2026-01-02T03:03:00Z", "position"},
	}
	if !reflect.DeepEqual(positions, want) {
		t.Fatalf("positions = %+v, want %+v", positions, want)
	}
}

// Passing the welcome page is stored in Done but is not a finished lesson.
func TestTutorialHistorySkipsWelcomed(t *testing.T) {
	repo, _ := newTestRepo(t)
	putTutorialAt(t, repo, "2026-01-02T03:00:00Z", TutorialPreference{Lesson: "a", Step: 0, Done: []string{"welcomed"}})
	putTutorialAt(t, repo, "2026-01-02T03:01:00Z", TutorialPreference{Lesson: "b", Step: 0, Done: []string{"welcomed", "a"}})
	want := []TutorialStep{
		{"a", 0, "2026-01-02T03:00:00Z", "position"},
		{"a", 0, "2026-01-02T03:01:00Z", "done"},
		{"b", 0, "2026-01-02T03:01:00Z", "position"},
	}
	if got := installationHistory(t, repo); !reflect.DeepEqual(got, want) {
		t.Fatalf("history = %+v, want %+v", got, want)
	}
}
