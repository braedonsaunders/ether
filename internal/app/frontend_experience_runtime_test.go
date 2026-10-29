package app

import (
	"context"
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/plextv"
)

type captureExperienceFrontend struct{ host *frontend.Host }

func (f captureExperienceFrontend) Describe() frontend.Descriptor {
	return frontend.Descriptor{ID: "experience-probe"}
}
func (f captureExperienceFrontend) Mount(_ context.Context, h frontend.Host) (frontend.Instance, error) {
	*f.host = h
	return sharedInstance{id: "experience-probe", prefix: "/probe"}, nil
}

func TestRuntimeSuppliesSurfaceExperienceAndRejectsUnknownViewer(t *testing.T) {
	a := setupTestApp(t, true)
	ctx := context.Background()
	if err := a.users.SetOwner(ctx, plextv.Account{ID: 42, Username: "owner"}, "owner-token"); err != nil {
		t.Fatal(err)
	}
	owner, err := a.users.Owner(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var host frontend.Host
	if err := a.frontends.registry.Register(captureExperienceFrontend{host: &host}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.frontends.registry.Mount(ctx, "experience-probe"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.frontends.registry.Unmount(context.Background(), "experience-probe") })
	if host.Experience == nil {
		t.Fatal("surface has no experience")
	}
	views, err := host.Experience.Views(ctx, owner)
	if err != nil || len(views) != 2 {
		t.Fatalf("owner views = %v, %v", views, err)
	}
	unknown := &core.User{ID: owner.ID + 10000}
	if _, err := host.Experience.Views(ctx, unknown); err == nil {
		t.Fatal("unknown viewer obtained views")
	}
	if err := host.Experience.SetPlayed(ctx, unknown, core.MovieKey(1)); err == nil {
		t.Fatal("unknown viewer changed watch state")
	}
}
