package identity_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/identity"
	"github.com/braedonsaunders/ether/internal/store"
)

func TestRefreshRotationAndReuse(t *testing.T) {
	ctx := context.Background()
	db := store.OpenTest(t)
	if _, err := db.Exec(`INSERT INTO auth_users(id,plex_id,created_at,updated_at) VALUES(1,1,0,0)`); err != nil {
		t.Fatal(err)
	}
	now := time.Unix(10000, 0)
	g, err := identity.NewGrants(db, members{db}, time.Hour, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	d := core.Device{ID: "tv"}
	access, err := g.Issue(ctx, 1, nil, "silo", d)
	if err != nil {
		t.Fatal(err)
	}
	refresh, err := g.IssueRefresh(ctx, "silo", access, d)
	if err != nil {
		t.Fatal(err)
	}
	scope, err := g.GrantScope(ctx, "silo", access, d)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = g.GrantScopeCurrent(ctx, "emby", scope, d); !errors.Is(err, core.ErrUnauthorized) {
		t.Fatal("cross-surface family scope", err)
	}
	if _, err = g.GrantScopeCurrent(ctx, "silo", scope, core.Device{ID: "other"}); !errors.Is(err, core.ErrUnauthorized) {
		t.Fatal("cross-device family scope", err)
	}
	if _, err = g.Resolve(ctx, "silo", refresh); !errors.Is(err, core.ErrUnauthorized) {
		t.Fatalf("refresh used as access: %v", err)
	}
	if _, _, err = g.Refresh(ctx, "silo", access, d); !errors.Is(err, core.ErrUnauthorized) {
		t.Fatalf("access used as refresh: %v", err)
	}
	if _, _, err = g.Refresh(ctx, "emby", refresh, d); !errors.Is(err, core.ErrUnauthorized) {
		t.Fatalf("cross surface: %v", err)
	}
	if _, _, err = g.Refresh(ctx, "silo", refresh, core.Device{ID: "other"}); !errors.Is(err, core.ErrUnauthorized) {
		t.Fatalf("cross device: %v", err)
	}
	next, nextRefresh, err := g.Refresh(ctx, "silo", refresh, d)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = g.Resolve(ctx, "silo", access); !errors.Is(err, core.ErrUnauthorized) {
		t.Fatalf("old access valid: %v", err)
	}
	if _, err = g.Resolve(ctx, "silo", next); err != nil {
		t.Fatal(err)
	}
	rotatedScope, err := g.GrantScope(ctx, "silo", next, d)
	if err != nil || rotatedScope != scope {
		t.Fatal("family scope changed across valid refresh", err)
	}
	if _, err = g.GrantScopeCurrent(ctx, "silo", scope, d); err != nil {
		t.Fatal("valid rotation ended family", err)
	}
	if _, _, err = g.Refresh(ctx, "silo", refresh, d); !errors.Is(err, core.ErrUnauthorized) {
		t.Fatalf("reuse: %v", err)
	}
	if _, err = g.Resolve(ctx, "silo", next); !errors.Is(err, core.ErrUnauthorized) {
		t.Fatalf("reuse did not revoke access: %v", err)
	}
	if _, err = g.GrantScopeCurrent(ctx, "silo", scope, d); !errors.Is(err, core.ErrUnauthorized) {
		t.Fatal("revoked family remains current", err)
	}
	if _, _, err = g.Refresh(ctx, "silo", nextRefresh, d); !errors.Is(err, core.ErrUnauthorized) {
		t.Fatalf("reuse did not revoke family: %v", err)
	}
}
func TestRefreshInactiveAndRevocation(t *testing.T) {
	ctx := context.Background()
	db := store.OpenTest(t)
	if _, err := db.Exec(`INSERT INTO auth_users(id,plex_id,created_at,updated_at) VALUES(1,1,0,0)`); err != nil {
		t.Fatal(err)
	}
	g, err := identity.NewGrants(db, members{db}, time.Hour, nil)
	if err != nil {
		t.Fatal(err)
	}
	d := core.Device{ID: "tv"}
	a, err := g.Issue(ctx, 1, nil, "silo", d)
	if err != nil {
		t.Fatal(err)
	}
	r, err := g.IssueRefresh(ctx, "silo", a, d)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`UPDATE auth_users SET active=0 WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	if _, _, err = g.Refresh(ctx, "silo", r, d); !errors.Is(err, core.ErrUnauthorized) {
		t.Fatalf("inactive: %v", err)
	}
	if _, err = db.Exec(`UPDATE auth_users SET active=1 WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	if err = g.RevokeDevice(ctx, 1, "tv"); err != nil {
		t.Fatal(err)
	}
	if _, _, err = g.Refresh(ctx, "silo", r, d); !errors.Is(err, core.ErrUnauthorized) {
		t.Fatalf("revoked: %v", err)
	}
}

func TestRefreshExpiry(t *testing.T) {
	ctx := context.Background()
	db := store.OpenTest(t)
	if _, err := db.Exec(`INSERT INTO auth_users(id,plex_id,created_at,updated_at) VALUES(1,1,0,0)`); err != nil {
		t.Fatal(err)
	}
	now := time.Unix(10000, 0)
	g, err := identity.NewGrants(db, members{db}, time.Hour, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	d := core.Device{ID: "tv"}
	issue := func() string {
		a, e := g.Issue(ctx, 1, nil, "silo", d)
		if e != nil {
			t.Fatal(e)
		}
		r, e := g.IssueRefresh(ctx, "silo", a, d)
		if e != nil {
			t.Fatal(e)
		}
		return r
	}
	r := issue()
	now = now.Add(31 * 24 * time.Hour)
	if _, _, err = g.Refresh(ctx, "silo", r, d); !errors.Is(err, core.ErrUnauthorized) {
		t.Fatalf("idle expiry: %v", err)
	}
	r = issue()
	for j := 0; j < 3; j++ {
		now = now.Add(29 * 24 * time.Hour)
		_, r, err = g.Refresh(ctx, "silo", r, d)
		if err != nil {
			t.Fatal(err)
		}
	}
	now = now.Add(4 * 24 * time.Hour)
	if _, _, err = g.Refresh(ctx, "silo", r, d); !errors.Is(err, core.ErrUnauthorized) {
		t.Fatalf("absolute expiry: %v", err)
	}
}

func TestPruneKeepsLiveGrantsAndFamilies(t *testing.T) {
	ctx := context.Background()
	db := store.OpenTest(t)
	if _, err := db.Exec(`INSERT INTO auth_users(id,plex_id,created_at,updated_at) VALUES(1,1,0,0)`); err != nil {
		t.Fatal(err)
	}
	now := time.Unix(10000, 0)
	clock := func() time.Time { return now }
	g, err := identity.NewGrants(db, members{db}, time.Hour, clock)
	if err != nil {
		t.Fatal(err)
	}
	long, err := identity.NewGrants(db, members{db}, 30*24*time.Hour, clock)
	if err != nil {
		t.Fatal(err)
	}
	d := core.Device{ID: "tv"}
	// Live family whose current access grant has expired: Refresh still needs it.
	a, _ := g.Issue(ctx, 1, nil, "silo", d)
	r1, _ := g.IssueRefresh(ctx, "silo", a, d)
	_, r2, err := g.Refresh(ctx, "silo", r1, d)
	if err != nil {
		t.Fatal(err)
	}
	// Dead family: replay revoked it and its grants.
	other := core.Device{ID: "phone"}
	c, _ := g.Issue(ctx, 1, nil, "silo", other)
	rc, _ := g.IssueRefresh(ctx, "silo", c, other)
	if _, _, err = g.Refresh(ctx, "silo", rc, other); err != nil {
		t.Fatal(err)
	}
	if _, _, err = g.Refresh(ctx, "silo", rc, other); !errors.Is(err, core.ErrUnauthorized) {
		t.Fatal(err)
	}
	_, _ = g.Issue(ctx, 1, nil, "silo", core.Device{ID: "gone"})
	live, _ := long.Issue(ctx, 1, nil, "silo", core.Device{ID: "pc"})

	now = now.Add(8 * 24 * time.Hour)
	n, err := g.Prune(ctx)
	if err != nil || n != 5 {
		t.Fatalf("pruned %d, %v; want 4 grants and 1 family", n, err)
	}
	if _, err = long.Resolve(ctx, "silo", live); err != nil {
		t.Fatalf("live grant pruned: %v", err)
	}
	_, r3, err := g.Refresh(ctx, "silo", r2, d)
	if err != nil {
		t.Fatalf("live family's current token pruned: %v", err)
	}
	// The live family's consumed tokens still trip replay detection.
	if _, _, err = g.Refresh(ctx, "silo", r1, d); !errors.Is(err, core.ErrUnauthorized) {
		t.Fatal(err)
	}
	if _, _, err = g.Refresh(ctx, "silo", r3, d); !errors.Is(err, core.ErrUnauthorized) {
		t.Fatalf("replay after prune did not revoke the family: %v", err)
	}
}
