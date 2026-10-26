package settings

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/noisefloor-oss/deck/internal/db"
)

// Logout epochs must survive a restart of the process that wrote them: a
// renewal already in flight when logout landed verifies stale against the
// reloaded store too. This exercises the SQLite load/save the handler's
// restart guarantee rides on, covering the legacy empty key and separate
// login IDs.
func TestLogoutEpochsRoundTripAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "control.db")
	open := func(t *testing.T) *Repository {
		t.Helper()
		d, err := db.Open(context.Background(), db.Config{Path: path})
		if err != nil {
			t.Fatalf("db.Open: %v", err)
		}
		t.Cleanup(func() { _ = d.Close() })
		return NewRepository(d)
	}
	repo := open(t)
	if epochs, err := repo.LoadLogoutEpochs(context.Background()); err != nil || len(epochs) != 0 {
		t.Fatalf("fresh database loaded epochs %v (err %v); want none", epochs, err)
	}
	for loginID, epoch := range map[string]uint64{"": 1, "login-a": 3, "login-b": 2} {
		if err := repo.SaveLogoutEpoch(context.Background(), loginID, epoch); err != nil {
			t.Fatalf("save %q: %v", loginID, err)
		}
	}
	// Reopen from the same file: the store a restarted process loads.
	reopened := open(t)
	epochs, err := reopened.LoadLogoutEpochs(context.Background())
	if err != nil {
		t.Fatalf("load after reopen: %v", err)
	}
	want := map[string]uint64{"": 1, "login-a": 3, "login-b": 2}
	if len(epochs) != len(want) {
		t.Fatalf("epochs after reopen = %v, want %v", epochs, want)
	}
	for loginID, epoch := range want {
		if epochs[loginID] != epoch {
			t.Fatalf("epoch for %q = %d, want %d", loginID, epochs[loginID], epoch)
		}
	}
	// A later logout advances one login without disturbing the others.
	if err := reopened.SaveLogoutEpoch(context.Background(), "login-a", 4); err != nil {
		t.Fatalf("advance: %v", err)
	}
	epochs, err = reopened.LoadLogoutEpochs(context.Background())
	if err != nil || epochs["login-a"] != 4 || epochs["login-b"] != 2 || epochs[""] != 1 {
		t.Fatalf("epochs after advance = %v (err %v)", epochs, err)
	}
}
