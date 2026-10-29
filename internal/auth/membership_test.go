package auth

import (
	"context"
	"github.com/braedonsaunders/ether/internal/plextv"
	"github.com/braedonsaunders/ether/internal/store"
	"testing"
)

func TestOwnerAndPlexMembershipAreIndependent(t *testing.T) {
	ctx := context.Background()
	db := store.OpenTest(t)
	s, err := New(ctx, db, Options{})
	if err != nil {
		t.Fatal(err)
	}
	friend := plextv.Friend{PlexID: 42, Username: "friend"}
	if err = s.SyncFriends(ctx, []plextv.Friend{friend}); err != nil {
		t.Fatal(err)
	}
	var id int64
	if err = db.QueryRowContext(ctx, `SELECT id FROM auth_users WHERE plex_id=42`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	if err = s.SetOwnerEnabled(ctx, id, false); err != nil {
		t.Fatal(err)
	}
	if err = s.SyncFriends(ctx, []plextv.Friend{friend}); err != nil {
		t.Fatal(err)
	}
	var active, enabled, shared int
	check := func(a, e, p int) {
		t.Helper()
		if err = db.QueryRowContext(ctx, `SELECT active,owner_enabled,plex_shared FROM auth_users WHERE id=?`, id).Scan(&active, &enabled, &shared); err != nil || active != a || enabled != e || shared != p {
			t.Fatalf("membership %d %d %d: %v", active, enabled, shared, err)
		}
	}
	check(0, 0, 1)
	if err = s.SyncFriends(ctx, nil); err != nil {
		t.Fatal(err)
	}
	check(0, 0, 0)
	if err = s.SetOwnerEnabled(ctx, id, true); err != nil {
		t.Fatal(err)
	}
	check(0, 1, 0)
	if err = s.SyncFriends(ctx, []plextv.Friend{friend}); err != nil {
		t.Fatal(err)
	}
	check(1, 1, 1)
}
