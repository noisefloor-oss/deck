package session

import (
	"context"
	"database/sql"
	"testing"

	"github.com/noisefloor-oss/deck/internal/inputgate"
	"github.com/noisefloor-oss/deck/internal/lease"
	"github.com/noisefloor-oss/deck/internal/provider"
)

// A user can revive a session the automatic sweep already spent its one
// attempt on. The unattended loop is bounded because nobody is watching it; a
// person who has read the failure is the retry loop, and refusing them would
// bury work the CLI could still resume.
func TestUserRecoverReopensASpentOpportunity(t *testing.T) {
	desc := sleeperDescriptor()
	desc.contract = provider.IdentityFreshRelaunch
	mgr, repo, _, projectID := newTestEnv(t, desc)
	mgr.SetResumeDeps(lease.NewManager(repo.db), inputgate.New(), testWorkDirResolver(t))

	ctx := context.Background()
	s, err := mgr.Create(ctx, CreateSpec{
		ProjectID: projectID, Provider: provider.Claude, WorkDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	crashAndObserve(t, mgr, s)

	// Spend the automatic opportunity the way an attempt that reached the
	// provider and got nowhere would: the row stays terminal, the attempt is
	// consumed, and the unattended sweep will never look at it again.
	if err := repo.db.WithTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`UPDATE sessions SET resume_attempts = 1 WHERE id = ?`, s.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := mgr.recover(ctx, s.ID, "", false); err == nil {
		t.Fatal("automatic recovery ran twice without a user asking")
	}

	revived, _, err := mgr.Recover(ctx, s.ID)
	if err != nil {
		t.Fatalf("user Recover: %v", err)
	}
	if revived.Status != StatusRunning {
		t.Fatalf("status = %s, want running", revived.Status)
	}
	// Replace in place: one row, one card, never a sibling.
	rows, err := repo.ListByProject(ctx, projectID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("session rows = %d, want 1: recovery created a second card", len(rows))
	}
}

// A clean exit is the provider's choice, not the user's verdict. The
// automatic sweep honors exit 0 as intentional and stays away; the person
// who clicks Resume has decided otherwise, and the card promised them the
// record was kept.
func TestUserRecoverRevivesACleanExit(t *testing.T) {
	desc := sleeperDescriptor()
	desc.contract = provider.IdentityFreshRelaunch
	mgr, repo, _, projectID := newTestEnv(t, desc)
	mgr.SetResumeDeps(lease.NewManager(repo.db), inputgate.New(), testWorkDirResolver(t))

	ctx := context.Background()
	s, err := mgr.Create(ctx, CreateSpec{
		ProjectID: projectID, Provider: provider.Claude, WorkDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	crashAndObserve(t, mgr, s)
	if err := repo.db.WithTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`UPDATE sessions SET exit_code = 0, exit_signal = NULL WHERE id = ?`, s.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := mgr.recover(ctx, s.ID, "", false); err == nil {
		t.Fatal("the automatic sweep revived an intentional exit")
	}
	revived, _, err := mgr.Recover(ctx, s.ID)
	if err != nil {
		t.Fatalf("user Recover after a clean exit: %v", err)
	}
	if revived.Status != StatusRunning {
		t.Fatalf("status = %s, want running", revived.Status)
	}
}

// A stop request aims at the generation that was running when the user
// asked. Clicking Resume rescinds it: the successor must not inherit a stop
// meant for its predecessor, and the reopen clears the marker with the
// counters.
func TestUserRecoverRevivesAStoppedSession(t *testing.T) {
	desc := sleeperDescriptor()
	desc.contract = provider.IdentityFreshRelaunch
	mgr, repo, _, projectID := newTestEnv(t, desc)
	mgr.SetResumeDeps(lease.NewManager(repo.db), inputgate.New(), testWorkDirResolver(t))

	ctx := context.Background()
	s, err := mgr.Create(ctx, CreateSpec{
		ProjectID: projectID, Provider: provider.Claude, WorkDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	crashAndObserve(t, mgr, s)
	if err := repo.db.WithTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`UPDATE sessions SET stop_requested_at = updated_at WHERE id = ?`, s.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := mgr.recover(ctx, s.ID, "", false); err == nil {
		t.Fatal("the automatic sweep revived a stopped session")
	}
	revived, _, err := mgr.Recover(ctx, s.ID)
	if err != nil {
		t.Fatalf("user Recover after a stop: %v", err)
	}
	if revived.Status != StatusRunning {
		t.Fatalf("status = %s, want running", revived.Status)
	}
	if revived.StopRequestedAt != nil {
		t.Fatal("the successor inherited its predecessor's stop request")
	}
}

// Recover refuses a session that is alive: reviving a running process would
// destroy the work it is doing.
func TestUserRecoverRefusesALiveSession(t *testing.T) {
	mgr, repo, _, projectID := newTestEnv(t, sleeperDescriptor())
	mgr.SetResumeDeps(lease.NewManager(repo.db), inputgate.New(), testWorkDirResolver(t))
	ctx := context.Background()
	s, err := mgr.Create(ctx, CreateSpec{
		ProjectID: projectID, Provider: provider.Claude, WorkDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, _, err := mgr.Recover(ctx, s.ID); err == nil {
		t.Fatal("Recover revived a running session")
	}
	live, err := repo.Get(ctx, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if live.Status != StatusRunning {
		t.Fatalf("status = %s, want the live session untouched", live.Status)
	}
}

// A candidate that dies on launch must still be recognizable, so its slot can
// be retired and the user can try again. Its process group and start ticks
// die with it, but tmux keeps the pid that exited; without recording that, the
// slot outlives its corpse and every later retry is refused as a recovery that
// is already in progress — for a recovery that never will be.
func TestDeadCandidateDoesNotWedgeLaterRecoveries(t *testing.T) {
	desc := sleeperDescriptor()
	desc.contract = provider.IdentityFreshRelaunch
	mgr, repo, _, projectID := newTestEnv(t, desc)
	mgr.SetResumeDeps(lease.NewManager(repo.db), inputgate.New(), testWorkDirResolver(t))

	ctx := context.Background()
	s, err := mgr.Create(ctx, CreateSpec{
		ProjectID: projectID, Provider: provider.Claude, WorkDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	crashAndObserve(t, mgr, s)

	// The provider now exits the moment it is launched: a bad install, a
	// vanished work directory, a resume target the CLI no longer knows.
	desc.args = []string{"-c", "exit 3"}
	if _, _, err := mgr.Recover(ctx, s.ID); err == nil {
		t.Fatal("a candidate that exits immediately reported success")
	}
	if err := mgr.ConvergeRecoveryCandidates(ctx); err != nil {
		t.Fatalf("converge: %v", err)
	}
	if slot, err := repo.SuccessorSlot(ctx, s.ID); err != nil || slot != nil {
		t.Fatalf("dead candidate left slot = %+v err=%v", slot, err)
	}

	// With the slot retired, a working provider can be recovered onto the row.
	desc.args = []string{"-c", "sleep 300; exit 0"}
	revived, _, err := mgr.Recover(ctx, s.ID)
	if err != nil {
		t.Fatalf("second Recover after a dead candidate: %v", err)
	}
	if revived.Status != StatusRunning {
		t.Fatalf("status = %s, want running", revived.Status)
	}
}
