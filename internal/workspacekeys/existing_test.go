package workspacekeys

import "testing"

func TestUserRegistryOpenPreservesLiveChallenge(t *testing.T) {
	f := newFixture(t)
	prepared, err := f.r.PrepareMethod(f.ctx, f.actor, Passkey, "pending passkey")
	if err != nil {
		t.Fatal(err)
	}
	var before []byte
	if err := f.db.QueryRow(`SELECT registration FROM workspacekeys_methods WHERE id=?`, prepared.Method.ID).Scan(&before); err != nil || len(before) == 0 {
		t.Fatal("missing pending registration", err)
	}
	user, err := OpenExisting(f.ctx, f.db, f.r.cfg)
	if err != nil {
		t.Fatal(err)
	}
	var after []byte
	if err := f.db.QueryRow(`SELECT registration FROM workspacekeys_methods WHERE id=?`, prepared.Method.ID).Scan(&after); err != nil || string(after) != string(before) {
		t.Fatal("admin command invalidated live passkey ceremony", err)
	}
	if _, err := user.State(f.ctx, f.actor.Login); err != nil {
		t.Fatal(err)
	}
	wrong := f.r.cfg
	wrong.InstanceID = "another-instance"
	if _, err := OpenExisting(f.ctx, f.db, wrong); err == nil {
		t.Fatal("admin opened a different installation")
	}
}
