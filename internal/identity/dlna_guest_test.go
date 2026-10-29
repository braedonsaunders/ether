package identity_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/identity"
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
}
