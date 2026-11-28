package identity_test

import (
	"context"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/identity"
	"github.com/braedonsaunders/ether/internal/store"
)

// The issued token is kept only for the replay window; prune clears it as
// soon as the window has passed instead of when the row itself is deleted.
func TestPruneClearsQuickConnectReplayToken(t *testing.T) {
	ctx := context.Background()
	db := store.OpenTest(t)
	if _, err := db.ExecContext(ctx, `INSERT INTO auth_users(id,plex_id,created_at,updated_at) VALUES(1,99,0,0)`); err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1000, 0)
	clock := func() time.Time { return now }
	q, err := identity.NewQuickConnect(db, time.Minute, time.Hour, clock)
	if err != nil {
		t.Fatal(err)
	}
	g, err := identity.NewGrants(db, members{db}, time.Hour, clock)
	if err != nil {
		t.Fatal(err)
	}
	code, secret, err := q.Initiate(ctx, "jellyfin", core.Device{ID: "tv"})
	if err != nil {
		t.Fatal(err)
	}
	if err = q.Approve(ctx, code, 1); err != nil {
		t.Fatal(err)
	}
	if _, err = q.Poll(ctx, secret); err != nil {
		t.Fatal(err)
	}
	stored := func() string {
		var tok string
		if err := db.QueryRowContext(ctx, `SELECT replay_token FROM identity_quickconnect WHERE code=?`, code).Scan(&tok); err != nil {
			t.Fatal(err)
		}
		return tok
	}
	if _, err = g.Prune(ctx); err != nil {
		t.Fatal(err)
	}
	if stored() == "" {
		t.Fatal("replay token cleared inside its replay window")
	}
	now = now.Add(5 * time.Minute)
	if _, err = g.Prune(ctx); err != nil {
		t.Fatal(err)
	}
	if tok := stored(); tok != "" {
		t.Fatal("replay token still stored after its replay window")
	}
}
