package app

import (
	"context"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/experience"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/plextv"
	"reflect"
	"testing"
	"time"
)

// controlLegacyExperienceAdditionalRows supplies the viewer's real playlist, watchlist and
// live rows; the shared row service applies policy before delivery.
func (a *App) controlLegacyExperienceAdditionalRows(ctx context.Context, req frontend.RowRequest, playlists core.Playlists, visibility experience.Visibility) ([]frontend.Row, error) {
	if req.Placement != frontend.PlacementHome || req.Viewer == nil {
		return nil, nil
	}
	var rows []frontend.Row
	if playlists != nil {
		entries, more, err := experience.PlaylistEntries(ctx, req.Viewer, playlists, req.Limit)
		if err != nil {
			return nil, err
		}
		if len(entries) > 0 {
			rows = append(rows, frontend.Row{ID: "playlists", Title: "Recent Playlists", Purpose: core.RowPurpose("playlist"), Entries: entries, More: more})
		}
	}
	restricted := !req.Viewer.IsOwner && (req.Viewer.MaxContentRating != "" || len(req.Viewer.AllowedSections) > 0 || a.safety.Restricted(req.Viewer))
	if a.watchlist != nil {
		hubs, err := controlLegacyLoadExperienceDiscovery(ctx, req.Viewer, req.Limit, restricted, visibility, a.watchlist.Hubs)
		if err != nil {
			return nil, err
		}
		rows = append(rows, experienceHubRows(hubs)...)
	}
	if a.discoveryPersonal != nil {
		hubs, err := controlLegacyLoadExperienceDiscovery(ctx, req.Viewer, req.Limit, restricted, visibility, a.discoveryPersonal.Hubs)
		if err != nil {
			return nil, err
		}
		rows = append(rows, experienceHubRows(hubs)...)
	}
	if a.livetv != nil {
		now := time.Now()
		entries, err := experience.LiveEntries(ctx, req.Viewer, a.livetv, now, now.Add(24*time.Hour), nil)
		if err != nil {
			return nil, err
		}
		if len(entries) > 0 {
			rows = append(rows, frontend.Row{ID: "live", Title: "On Now", Purpose: frontend.RowLive, Entries: entries})
		}
	}
	return rows, nil
}

// controlLegacyLoadExperienceDiscovery expands only restricted discovery pools. Ranked
// personalization rows remain assembled once with their original visit seed.
func controlLegacyLoadExperienceDiscovery(ctx context.Context, viewer *core.User, limit int, restricted bool, visibility experience.Visibility, load func(context.Context, *core.User, int) ([]core.Hub, error)) ([]core.Hub, error) {
	if !restricted {
		return load(ctx, viewer, limit)
	}
	var hubs []core.Hub
	var loadErr error
	rows := experience.LoadDiscovery(ctx, viewer, visibility, limit, func(ctx context.Context, fetch int) []experience.DiscoveryCandidate {
		hubs, loadErr = load(ctx, viewer, fetch)
		if loadErr != nil {
			return nil
		}
		candidates := make([]experience.DiscoveryCandidate, len(hubs))
		for i, hub := range hubs {
			candidates[i] = experience.DiscoveryCandidate{Index: i, Items: hub.Items}
		}
		return candidates
	})
	if loadErr != nil {
		return nil, loadErr
	}
	out := make([]core.Hub, 0, len(rows))
	for _, row := range rows {
		h := hubs[row.Index]
		h.Items = row.Items
		out = append(out, h)
	}
	return out, nil
}

func TestAdditionalRowsMatchesLegacyProductionPath(t *testing.T) {
	ctx := context.Background()
	a := setupTestApp(t, true)
	if err := a.users.SetOwner(ctx, plextv.Account{ID: 99111, Username: "owner"}, "owner-token"); err != nil {
		t.Fatal(err)
	}
	owner, err := a.users.Owner(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = a.playlists.Create(ctx, owner.ID, "Saved", nil); err != nil {
		t.Fatal(err)
	}
	libs := []experience.Library{{ID: 1, Type: core.TypeMovie}, {ID: 2, Type: core.TypeShow}}
	vis := experienceVisibility(a.safety, a.catalog.Get, nil, nil, libs)
	for _, placement := range []frontend.Placement{frontend.PlacementHome, frontend.PlacementLibrary} {
		req := frontend.RowRequest{Viewer: owner, Device: core.Device{ID: "test"}, Placement: placement, Limit: 10}
		want, we := a.controlLegacyExperienceAdditionalRows(ctx, req, a.playlists, vis)
		got, ge := a.experienceAdditionalRows(ctx, req, a.playlists, vis)
		if (we == nil) != (ge == nil) || !reflect.DeepEqual(want, got) {
			t.Fatalf("placement=%s legacy=%+v %v shared=%+v %v", placement, want, we, got, ge)
		}
	}
}
