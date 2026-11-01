package admin_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/admin"
	"github.com/braedonsaunders/ether/internal/auth"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/identity"
	"github.com/braedonsaunders/ether/internal/store"
)

func TestDLNAGuestCreationRequiresOwner(t *testing.T) {
	ctx := context.Background()
	db := store.OpenTest(t)
	users, err := auth.New(ctx, db, auth.Options{})
	if err != nil {
		t.Fatal(err)
	}
	local, _ := identity.NewLocal(db)
	ownerID, err := local.CreateOwner(ctx, "owner", "password-1234")
	if err != nil {
		t.Fatal(err)
	}
	viewerID, err := local.CreateViewer(ctx, "viewer", "password-1234")
	if err != nil {
		t.Fatal(err)
	}
	if err = users.RefreshViewers(ctx); err != nil {
		t.Fatal(err)
	}
	grants, _ := identity.NewGrants(db, users, time.Hour, nil)
	owner, _ := grants.Issue(ctx, ownerID, nil, "owner", core.Device{})
	viewer, _ := grants.Issue(ctx, viewerID, nil, "viewer-portal", core.Device{})
	called := 0
	h := admin.New(admin.Deps{Config: newStore(t, nil), IdentityGrants: grants, CreateDLNAGuest: func(context.Context) (*core.Viewer, error) { called++; return &core.Viewer{ID: 42}, nil }})
	do := func(token string, csrf bool) int {
		r := req("POST", "/ether/api/frontends/dlna/guest", "")
		r.Header.Set("Authorization", "Bearer "+token)
		if !csrf {
			r.Header.Del("X-Ether-Request")
		}
		return serve(h, r).Code
	}
	if got := do(viewer, true); got == http.StatusCreated || called != 0 {
		t.Fatalf("viewer created guest: %d", got)
	}
	if got := do(owner, false); got != http.StatusForbidden || called != 0 {
		t.Fatalf("missing CSRF: %d", got)
	}
	if got := do(owner, true); got != http.StatusCreated || called != 1 {
		t.Fatalf("owner: %d %d", got, called)
	}
}
