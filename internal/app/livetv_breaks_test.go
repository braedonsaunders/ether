package app

import (
	"context"
	"testing"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/plextv"
)

func TestBreakAnalysisIdentityRevalidation(t *testing.T) {
	ctx := context.Background()
	a := pinTestApp(t, 42)
	if _, err := a.channelBreakUser(ctx, 0); err == nil {
		t.Fatal("background caller without an admitted identity got setup owner")
	}
	u, err := a.channelBreakUser(ctx, core.SetupOwnerID)
	if err != nil || u.ID != core.SetupOwnerID {
		t.Fatalf("admitted open-setup job: %+v %v", u, err)
	}
	if err := a.cfg.Update(func(c *config.Config) error { c.SetupComplete = true; return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := a.channelBreakUser(ctx, core.SetupOwnerID); err == nil {
		t.Fatal("closed setup retained the stand-in")
	}
	if err := a.users.SetOwner(ctx, plextv.Account{ID: 42, Username: "owner"}, "fixture-token"); err != nil {
		t.Fatal(err)
	}
	u, err = a.channelBreakUser(ctx, core.SetupOwnerID)
	if err != nil || u.ID <= 0 || u.ID == core.SetupOwnerID || !u.IsOwner {
		t.Fatalf("owner transition retained stand-in: %+v %v", u, err)
	}
}
