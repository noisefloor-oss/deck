package session

import (
	"context"
	"testing"
	"time"

	"github.com/noisefloor-oss/deck/internal/inputgate"
	"github.com/noisefloor-oss/deck/internal/lease"
	"github.com/noisefloor-oss/deck/internal/provider"
)

// A provider that starts, commits, and exits again is relaunched at most
// maxAutomaticRecoveries times. Without the bound each committed recovery
// clears ResumeAttempts and earns another, so the sweep relaunches forever —
// measured in production at generation 2881 for one session.
func TestAutomaticRecoveryStopsAfterTheBound(t *testing.T) {
	desc := sleeperDescriptor()
	desc.contract = provider.IdentityFreshRelaunch
	// Exits immediately every time, which is the crash loop being bounded.
	desc.args = []string{"-c", "exit 1"}
	mgr, repo, _, projectID := newTestEnv(t, desc)
	mgr.SetResumeDeps(lease.NewManager(repo.db), inputgate.New(), testWorkDirResolver(t))

	ctx := context.Background()
	working := sleeperDescriptor()
	mgr.providers[provider.Claude] = working
	s, err := mgr.Create(ctx, CreateSpec{
		ProjectID: projectID, Provider: provider.Claude, WorkDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	crashAndObserve(t, mgr, s)
	mgr.providers[provider.Claude] = desc

	// Sweep well past the bound; every pass finds a terminal row.
	for range maxAutomaticRecoveries + 4 {
		if err := mgr.AutoResumeSweep(ctx); err != nil {
			t.Fatalf("sweep: %v", err)
		}
		_ = mgr.ConvergeRecoveryCandidates(ctx)
	}
	got, err := repo.Get(ctx, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.AutomaticRecoveries > maxAutomaticRecoveries {
		t.Fatalf("automatic recoveries = %d, want at most %d",
			got.AutomaticRecoveries, maxAutomaticRecoveries)
	}
	if got.AutomaticRecoveries == 0 {
		t.Fatal("no recovery was attempted; the test never exercised the bound")
	}
	// The row is left terminal for a person to act on, not relaunched again.
	if !terminalRecoverySource(got) {
		t.Fatalf("status = %s, want a terminal row once the bound is spent", got.Status)
	}
	if automaticRecoveryOpportunity(got, desc, true, time.Now()) {
		t.Fatal("the sweep would relaunch again past the bound")
	}
}

// A series older than the window is not a series. A session that recovers
// rarely over a long life must not be starved by recoveries it made days ago.
func TestAutomaticRecoveryWindowExpiresTheSeries(t *testing.T) {
	spent := &Session{
		Status: StatusExited, AutomaticRecoveries: maxAutomaticRecoveries,
	}
	now := time.Now()
	recent := now.Add(-automaticRecoveryWindow / 2)
	spent.AutomaticRecoverySeriesAt = &recent
	if !automaticRecoveryExhausted(spent, now) {
		t.Fatal("a full series inside the window was not exhausted")
	}
	old := now.Add(-automaticRecoveryWindow - time.Minute)
	spent.AutomaticRecoverySeriesAt = &old
	if automaticRecoveryExhausted(spent, now) {
		t.Fatal("a series older than the window still counted against the row")
	}
}

// The user's explicit retry is not the unattended loop the bound exists
// to stop, so it starts the series over.
func TestUserRecoverClearsTheSeries(t *testing.T) {
	mgr, repo, _, projectID := newTestEnv(t, sleeperDescriptor())
	mgr.SetResumeDeps(lease.NewManager(repo.db), inputgate.New(), testWorkDirResolver(t))
	ctx := context.Background()
	s, err := mgr.Create(ctx, CreateSpec{
		ProjectID: projectID, Provider: provider.Claude, WorkDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	crashAndObserve(t, mgr, s)
	source, err := repo.Get(ctx, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.ReopenRecoveryOpportunity(ctx, source); err != nil {
		t.Fatalf("ReopenRecoveryOpportunity: %v", err)
	}
	got, err := repo.Get(ctx, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.AutomaticRecoveries != 0 || got.AutomaticRecoverySeriesAt != nil {
		t.Fatalf("user retry left series count=%d at=%v",
			got.AutomaticRecoveries, got.AutomaticRecoverySeriesAt)
	}
}
