package app

import (
	"context"
	"errors"
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/identity"
	"github.com/braedonsaunders/ether/internal/plextv"
)

func TestViewerPolicyRequiresOwner(t *testing.T) {
	ctx := context.Background()
	a := setupTestApp(t, true)
	if err := a.users.SetOwner(ctx, plextv.Account{ID: 42, Username: "owner"}, "token"); err != nil {
		t.Fatal(err)
	}
	owner, err := a.users.Owner(ctx)
	if err != nil {
		t.Fatal(err)
	}
	local, err := identity.NewLocal(a.db)
	if err != nil {
		t.Fatal(err)
	}
	id, err := local.CreateViewer(ctx, "child", "password")
	if err != nil {
		t.Fatal(err)
	}
	if err = a.users.RefreshViewers(ctx); err != nil {
		t.Fatal(err)
	}
	policy := surfaceViewerPolicy{app: a}
	if err = policy.SetMaxRating(ctx, &core.Viewer{ID: id}, ""); !errors.Is(err, core.ErrUnauthorized) {
		t.Fatalf("non-owner: %v", err)
	}
	if err = policy.SetMaxRating(ctx, owner, "PG"); err != nil {
		t.Fatal(err)
	}
	updated, err := a.users.ActiveUser(ctx, owner.ID)
	if err != nil || updated.MaxContentRating != "PG" {
		t.Fatalf("updated %+v %v", updated, err)
	}
	delegated := *owner
	delegated.Delegated = true
	if err = policy.SetMaxRating(ctx, &delegated, ""); !errors.Is(err, core.ErrUnauthorized) {
		t.Fatalf("delegated: %v", err)
	}
}
