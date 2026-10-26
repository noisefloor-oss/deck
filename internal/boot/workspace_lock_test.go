package boot

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/noisefloor-oss/deck/internal/db"
	"github.com/noisefloor-oss/deck/internal/session"
	"github.com/noisefloor-oss/deck/internal/tmux"
)

func TestPrivateLockPreparationReportsActualOutcome(t *testing.T) {
	for _, tc := range []struct {
		name, body, budget string
		err                error
		want, calls        int
		deadline           time.Duration
	}{
		{"prepared", "", "", nil, http.StatusNoContent, 1, session.WorkspaceLockTimeout},
		{"forwarded budget", "", "20000", nil, http.StatusNoContent, 1, 20 * time.Second},
		{"refused", "", "", session.ErrWorkspaceLockRefused, http.StatusUnprocessableEntity, 1, session.WorkspaceLockTimeout},
		{"ambiguous stop", "", "", session.ErrUnverifiable, http.StatusServiceUnavailable, 1, session.WorkspaceLockTimeout},
		{"unconfirmed", "", "", errors.New("private session path must not escape"), http.StatusServiceUnavailable, 1, session.WorkspaceLockTimeout},
		{"unexpected body", "{}", "", nil, http.StatusBadRequest, 0, 0},
		{"malformed budget", "", "90s", nil, http.StatusBadRequest, 0, 0},
		{"exhausted budget", "", "0", nil, http.StatusBadRequest, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			h := workspaceLockHandler(func(ctx context.Context) error {
				calls++
				if deadline, ok := ctx.Deadline(); !ok || time.Until(deadline) > tc.deadline {
					t.Errorf("preparation deadline = %v %v, want within %s", deadline, ok, tc.deadline)
				}
				return tc.err
			})
			r := httptest.NewRequest(http.MethodPost, "http://localhost/api/internal/workspace/prepare-lock", strings.NewReader(tc.body))
			if tc.budget != "" {
				r.Header.Set(session.WorkspaceLockBudgetHeader, tc.budget)
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != tc.want || calls != tc.calls || strings.Contains(w.Body.String(), "private session path") {
				t.Fatalf("status=%d calls=%d", w.Code, calls)
			}
		})
	}
}

func TestPrivateLockPreparationRefusesOversizedCohortBeforeCommitting(t *testing.T) {
	for _, tc := range []struct{ name, budget string }{
		{"no forwarded budget", ""},
		{"budget beyond the ceiling", "3600000"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			dir := t.TempDir()
			d, err := db.Open(ctx, db.Config{Path: filepath.Join(dir, "control.db")})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = d.Close() })
			now := db.FormatTime(time.Now())
			if err := d.WithTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
				_, err := tx.ExecContext(ctx, `
					INSERT INTO projects (id, name, canonical_path, git_author_name, git_author_email, created_at, last_used_at)
					VALUES ('lock-project', 'lock', ?, 'Test Author', 'test@example.invalid', ?, ?)`, dir, now, now)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			socketDir := t.TempDir()
			if err := os.Chmod(socketDir, 0o700); err != nil {
				t.Fatal(err)
			}
			tmuxClient, err := tmux.NewClient(tmux.Config{SocketPath: filepath.Join(socketDir, "ac.sock")})
			if err != nil {
				t.Fatal(err)
			}
			repo := session.NewRepository(d)
			mgr := session.NewManager(repo, tmuxClient, nil, "lock-")
			var ids []string
			for i := range 12 {
				id := fmt.Sprintf("lock-%d", i)
				if err := repo.Create(ctx, &session.Session{ID: id, ProjectID: "lock-project", Provider: "shell",
					TmuxSocketPath: filepath.Join(socketDir, "ac.sock"), TmuxSessionName: id, Generation: 1}); err != nil {
					t.Fatal(err)
				}
				ids = append(ids, id)
			}
			r := httptest.NewRequest(http.MethodPost, "http://localhost/api/internal/workspace/prepare-lock", nil)
			if tc.budget != "" {
				r.Header.Set(session.WorkspaceLockBudgetHeader, tc.budget)
			}
			w := httptest.NewRecorder()
			workspaceLockHandler(mgr.PrepareWorkspaceLock).ServeHTTP(w, r)
			if w.Code != http.StatusUnprocessableEntity {
				t.Fatalf("oversized cohort status = %d", w.Code)
			}
			for _, id := range ids {
				got, err := repo.Get(ctx, id)
				if err != nil || got.StopRequestedAt != nil || got.WorkspaceStopGeneration != 0 {
					t.Fatalf("refused lock committed a stop marker: %+v %v", got, err)
				}
			}
		})
	}
}
