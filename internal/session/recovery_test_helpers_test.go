package session

import (
	"context"
	"syscall"
	"testing"
	"time"

	"github.com/noisefloor-oss/deck/internal/inputgate"
	"github.com/noisefloor-oss/deck/internal/lease"
	"github.com/noisefloor-oss/deck/internal/provider"
	"github.com/noisefloor-oss/deck/internal/tmux"
)

type resumableDescriptor struct {
	*fakeDescriptor
	resumeArgv []string
	resumeErr  error
}

func (r *resumableDescriptor) ResumeSessionArgs(provider.ResumeSpec) ([]string, error) {
	if r.resumeErr != nil {
		return nil, r.resumeErr
	}
	return append([]string{r.executable}, r.resumeArgv...), nil
}

func testWorkDirResolver(t *testing.T) WorkDirResolver {
	t.Helper()
	dir := t.TempDir()
	return func(context.Context, string) (string, error) { return dir, nil }
}

func newResumeEnv(t *testing.T, resumeArgv []string) (*Manager, *Repository,
	string, *tmux.Client, *inputgate.Gate) {
	t.Helper()
	desc := &resumableDescriptor{fakeDescriptor: sleeperDescriptor(), resumeArgv: resumeArgv}
	mgr, repo, client, projectID := newTestEnv(t, desc)
	gate := inputgate.New()
	mgr.SetResumeDeps(lease.NewManager(repo.db), gate, testWorkDirResolver(t))
	return mgr, repo, projectID, client, gate
}

func createInterrupted(t *testing.T, mgr *Manager, projectID string) *Session {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	s, err := mgr.Create(ctx, CreateSpec{
		ProjectID: projectID, Provider: provider.Claude, WorkDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("create interrupted fixture: %v", err)
	}
	info, err := mgr.tmux.PaneInfoByID(ctx, *s.TmuxPaneID)
	if err != nil {
		t.Fatalf("observe interrupted fixture: %v", err)
	}
	if err := syscall.Kill(-info.ProcessGroupID, syscall.SIGKILL); err != nil {
		t.Fatalf("kill interrupted fixture: %v", err)
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		// The killed leader can leave /proc before tmux publishes its exit.
		dead, err := mgr.tmux.PaneInfoAfterSpawn(ctx, info)
		if err != nil {
			t.Fatalf("observe interrupted fixture death: %v", err)
		}
		if dead.Dead {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("interrupted fixture remained live")
		}
		time.Sleep(25 * time.Millisecond)
	}
	applied, err := mgr.repo.MarkInterruptedObserved(ctx, s, "test fixture records no exit evidence")
	if err != nil || !applied {
		t.Fatalf("mark interrupted fixture: applied=%v err=%v", applied, err)
	}
	got, err := mgr.repo.Get(ctx, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func createExplicitRecoverySource(t *testing.T, mgr *Manager, projectID string) *Session {
	t.Helper()
	return createInterrupted(t, mgr, projectID)
}

func crashAndObserve(t *testing.T, mgr *Manager, s *Session) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	spawned, err := mgr.tmux.PaneInfoByID(ctx, *s.TmuxPaneID)
	if err != nil {
		t.Fatalf("observe crash fixture before kill: %v", err)
	}
	if err := syscall.Kill(-*s.ProcessGroupID, syscall.SIGKILL); err != nil {
		t.Fatalf("crash fixture: %v", err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		// The killed leader can leave /proc before tmux publishes its exit.
		info, err := mgr.tmux.PaneInfoAfterSpawn(ctx, spawned)
		if err != nil {
			t.Fatal(err)
		}
		if info.Dead {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("crash fixture remained live")
		}
		time.Sleep(25 * time.Millisecond)
	}
	row, err := mgr.repo.Get(ctx, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mgr.Observe(ctx, row); err != nil {
		t.Fatal(err)
	}
}

func crashShellAndSettle(t *testing.T, mgr *Manager, repo *Repository,
	s *Session) *Session {
	t.Helper()
	ctx := context.Background()
	if err := syscall.Kill(-*s.ProcessGroupID, syscall.SIGKILL); err != nil {
		t.Fatalf("crash shell fixture: %v", err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		row, err := repo.Get(ctx, s.ID)
		if err != nil {
			t.Fatal(err)
		}
		row, err = mgr.Observe(ctx, row)
		if err != nil {
			t.Fatal(err)
		}
		if row.Status == StatusExited || row.Status == StatusInterrupted {
			return row
		}
		if time.Now().After(deadline) {
			t.Fatal("shell fixture remained live")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func countAuditType(t *testing.T, repo *Repository, id, want string) int {
	t.Helper()
	count := 0
	for _, typ := range auditTypes(t, repo, id) {
		if typ == want {
			count++
		}
	}
	return count
}
