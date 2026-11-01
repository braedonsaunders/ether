package app

import (
	"context"
	"errors"
	"net/netip"
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/frontendtest"
	"github.com/braedonsaunders/ether/internal/identity"
	"github.com/braedonsaunders/ether/internal/safety"
)

func TestViewerCapabilitiesPolicyAndMembership(t *testing.T) {
	ctx := context.Background()
	a := setupTestApp(t, true)
	local, err := identity.NewLocal(a.db)
	if err != nil {
		t.Fatal(err)
	}
	id, err := local.CreateViewer(ctx, "alice", "password")
	if err != nil {
		t.Fatal(err)
	}
	if err = a.users.RefreshViewers(ctx); err != nil {
		t.Fatal(err)
	}
	if err = a.safety.Put(ctx, id, safety.Policy{RemoteDisabled: true, MaxContentRating: "PG", MaxBitrateKbps: 3000, StreamLimit: 2, BlockedKeywords: []string{"violence"}}); err != nil {
		t.Fatal(err)
	}
	experience := frontendtest.Experience{ViewsFunc: func(_ context.Context, v *core.Viewer) ([]frontend.View, error) {
		if v.ID != id {
			t.Fatal("wrong viewer")
		}
		return []frontend.View{{ID: "2", Name: "Films"}}, nil
	}}
	call := a.viewerCapabilities(local, experience)
	viewer := &frontend.Viewer{ID: id}
	lan, err := call(ctx, viewer, frontend.Device{}, netip.MustParseAddr("192.168.1.2"))
	if err != nil {
		t.Fatal(err)
	}
	if lan.IsOwner == nil || *lan.IsOwner || lan.CanPlayRemote == nil || *lan.CanPlayRemote || lan.PeerRemote == nil || *lan.PeerRemote || lan.HasLocalPassword == nil || !*lan.HasLocalPassword || lan.RemoteBitrateKbps == nil || *lan.RemoteBitrateKbps != 3000 || lan.MaxRating == nil || *lan.MaxRating != "PG" || lan.AllowedLibraries == nil || len(*lan.AllowedLibraries) != 1 {
		t.Fatalf("LAN: %+v", lan)
	}
	if lan.CanPlay == nil || !*lan.CanPlay || lan.MaxActiveSessions == nil || *lan.MaxActiveSessions != 2 || lan.BlockedTags == nil || len(*lan.BlockedTags) != 1 || (*lan.BlockedTags)[0] != "violence" {
		t.Fatalf("LAN policy: %+v", lan)
	}
	active, err := a.users.ActiveUser(ctx, id)
	if err != nil || lan.CanDownload == nil || *lan.CanDownload != a.plexServer.DownloadsAllowed(active) {
		t.Fatalf("download policy: %+v, %v", lan, err)
	}
	remote, err := call(ctx, viewer, frontend.Device{}, netip.MustParseAddr("8.8.8.8"))
	if err != nil || remote.PeerRemote == nil || !*remote.PeerRemote || remote.CanPlay == nil || *remote.CanPlay {
		t.Fatalf("remote: %+v %v", remote, err)
	}
	if err = a.users.SetOwnerEnabled(ctx, id, false); err != nil {
		t.Fatal(err)
	}
	if _, err = call(ctx, viewer, frontend.Device{}, netip.MustParseAddr("192.168.1.2")); !errors.Is(err, core.ErrUnauthorized) {
		t.Fatalf("inactive: %v", err)
	}
}
