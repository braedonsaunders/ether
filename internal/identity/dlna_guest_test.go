package identity_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/auth"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/identity"
	"github.com/braedonsaunders/ether/internal/plextv"
	"github.com/braedonsaunders/ether/internal/safety"
	"github.com/braedonsaunders/ether/internal/store"
)

func TestDLNAGuestIsPasswordlessAndSurfaceRestricted(t *testing.T) {
	ctx := context.Background()
	db := store.OpenTest(t)
	local, err := identity.NewLocal(db)
	if err != nil {
		t.Fatal(err)
	}
	id, err := local.CreateDLNAGuest(ctx, "Living room guest")
	if err != nil {
		t.Fatal(err)
	}
	if err = local.ActivateDLNAGuest(ctx, id); !errors.Is(err, core.ErrUnauthorized) {
		t.Fatalf("activated without safety policy: %v", err)
	}
	policy, err := safety.New(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	defer policy.Close()
	if err = policy.Put(ctx, id, safety.KidsPreset("children")); err != nil {
		t.Fatal(err)
	}
	if err = local.ActivateDLNAGuest(ctx, id); err != nil {
		t.Fatal(err)
	}
	grants, err := identity.NewGrants(db, members{db}, time.Hour, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = grants.Issue(ctx, id, nil, "stremio", core.Device{}); !errors.Is(err, core.ErrUnauthorized) {
		t.Fatalf("stremio grant: %v", err)
	}
	if _, err = grants.Issue(ctx, id, nil, "viewer-portal", core.Device{}); !errors.Is(err, core.ErrUnauthorized) {
		t.Fatalf("portal grant: %v", err)
	}
	if _, err = grants.Issue(ctx, id, nil, "dlna", core.Device{}); err != nil {
		t.Fatalf("dlna grant: %v", err)
	}
	if err = local.UpsertPassword(ctx, id, "guest", "password"); !errors.Is(err, core.ErrUnauthorized) {
		t.Fatalf("password link: %v", err)
	}
	invites, err := identity.NewInvite(db, time.Hour, time.Hour, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = invites.Create(ctx, id); !errors.Is(err, core.ErrUnauthorized) {
		t.Fatalf("invite: %v", err)
	}
	if err = local.LinkPlex(ctx, id, 123, "Plex"); err == nil {
		t.Fatal("Plex link accepted")
	}
	quick, err := identity.NewQuickConnect(db, time.Minute, time.Hour, nil)
	if err != nil {
		t.Fatal(err)
	}
	code, _, err := quick.Initiate(ctx, "jellyfin", core.Device{})
	if err != nil {
		t.Fatal(err)
	}
	if err = quick.Approve(ctx, code, id); !errors.Is(err, core.ErrUnauthorized) {
		t.Fatalf("quick connect: %v", err)
	}
}

func TestGuestOriginRejectsPreviouslyIssuedNonDLNACredentials(t *testing.T) {
	ctx := context.Background()
	db := store.OpenTest(t)
	local, err := identity.NewLocal(db)
	if err != nil {
		t.Fatal(err)
	}
	id, err := local.CreateViewer(ctx, "legacy-guest", "password")
	if err != nil {
		t.Fatal(err)
	}
	grants, err := identity.NewGrants(db, members{db}, time.Hour, nil)
	if err != nil {
		t.Fatal(err)
	}
	device := core.Device{ID: "tv"}
	token, err := grants.Issue(ctx, id, nil, "silo", device)
	if err != nil {
		t.Fatal(err)
	}
	refresh, err := grants.IssueRefresh(ctx, "silo", token, device)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.ExecContext(ctx, `UPDATE auth_users SET origin='dlna_guest' WHERE id=?`, id); err != nil {
		t.Fatal(err)
	}
	if _, err = grants.Resolve(ctx, "silo", token); !errors.Is(err, core.ErrUnauthorized) {
		t.Fatalf("grant: %v", err)
	}
	if _, _, err = grants.ResolveDevice(ctx, "silo", token, device); !errors.Is(err, core.ErrUnauthorized) {
		t.Fatalf("device grant: %v", err)
	}
	if _, _, err = grants.Refresh(ctx, "silo", refresh, device); !errors.Is(err, core.ErrUnauthorized) {
		t.Fatalf("refresh: %v", err)
	}
	if _, _, err = local.Authenticate(ctx, "legacy-guest", "password"); !errors.Is(err, core.ErrUnauthorized) {
		t.Fatalf("local auth: %v", err)
	}
	viewers, err := local.PublicViewers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range viewers {
		if v.ID == id {
			t.Fatal("guest appeared in picker")
		}
	}
}

func TestGuestOriginRejectsLegacyPlexAndDelegationCredentials(t *testing.T) {
	ctx := context.Background()
	db := store.OpenTest(t)
	local, err := identity.NewLocal(db)
	if err != nil {
		t.Fatal(err)
	}
	id, err := local.CreateViewer(ctx, "guest-legacy-plex", "password")
	if err != nil {
		t.Fatal(err)
	}
	token := "legacy-plex-token"
	if _, err = db.ExecContext(ctx, `INSERT INTO auth_tokens(token_hash,user_id,source,created_at) VALUES(?,?,?,?)`, auth.HashToken(token), id, "test", 0); err != nil {
		t.Fatal(err)
	}
	service, err := auth.New(ctx, db, auth.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = service.Authenticate(ctx, token); err != nil {
		t.Fatal(err)
	}
	delegated, err := service.IssueDelegation(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.ExecContext(ctx, `UPDATE auth_users SET origin='dlna_guest' WHERE id=?`, id); err != nil {
		t.Fatal(err)
	}
	if err = service.RefreshViewers(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err = service.Authenticate(ctx, token); !errors.Is(err, core.ErrUnauthorized) {
		t.Fatalf("Plex token: %v", err)
	}
	if _, err = service.Authenticate(ctx, delegated); !errors.Is(err, core.ErrUnauthorized) {
		t.Fatalf("delegation: %v", err)
	}
	if _, err = service.IssueDelegation(ctx, id); !errors.Is(err, core.ErrUnauthorized) {
		t.Fatalf("new delegation: %v", err)
	}
	if _, err = service.ActiveUser(ctx, id); err != nil {
		t.Fatalf("guest unavailable to DLNA: %v", err)
	}
}

func TestGuestCannotBeBackfilledOrPromotedToPlexOwner(t *testing.T) {
	ctx := context.Background()
	db := store.OpenTest(t)
	local, err := identity.NewLocal(db)
	if err != nil {
		t.Fatal(err)
	}
	id, err := local.CreateDLNAGuest(ctx, "Guest")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.ExecContext(ctx, `UPDATE auth_users SET plex_id=123 WHERE id=?`, id); err != nil {
		t.Fatal(err)
	}
	service, err := auth.New(ctx, db, auth.Options{})
	if err != nil {
		t.Fatal(err)
	}
	var count int
	if err = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM identity_links WHERE viewer_id=? AND provider='plex'`, id).Scan(&count); err != nil || count != 0 {
		t.Fatalf("backfilled guest Plex link: %d %v", count, err)
	}
	if err = service.SetOwner(ctx, plextv.Account{ID: 123, Username: "guest"}, "server-token"); err == nil {
		t.Fatal("guest promoted to Plex owner")
	}
	var origin string
	var owner int
	if err = db.QueryRowContext(ctx, `SELECT origin,is_owner FROM auth_users WHERE id=?`, id).Scan(&origin, &owner); err != nil || origin != "dlna_guest" || owner != 0 {
		t.Fatalf("guest changed: %q %d %v", origin, owner, err)
	}
}
