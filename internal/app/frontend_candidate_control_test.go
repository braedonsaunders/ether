package app

import (
	"context"
	"errors"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/experience"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/plextv"
	"reflect"
	"strconv"
	"testing"
	"time"
)

func (a *App) controlLegacyProductionExperienceRows(ctx context.Context, req frontend.RowRequest, libraries []experience.Library, visibility experience.Visibility) ([]core.Hub, error) {
	if req.Viewer == nil || a.personalize == nil {
		return nil, core.ErrNotFound
	}
	sit, ok := ctx.Value(experienceSituationKey{}).(core.HomeSituation)
	if !ok {
		return nil, core.ErrNotFound
	}
	hc := core.HubContext{Device: sit.Device, Daypart: sit.Daypart, Weekend: sit.Weekend, Now: sit.Now, SectionID: sit.LibraryID, Product: sit.Product, Platform: sit.Platform, SessionID: sit.SessionID, ClientID: sit.ClientID}
	restricted := !req.Viewer.IsOwner && (req.Viewer.MaxContentRating != "" || len(req.Viewer.AllowedSections) > 0 || a.safety.Restricted(req.Viewer))
	allow := func(ctx context.Context, u *core.User, t *core.Title) bool { return visibility.Visible(ctx, u, t) }
	switch req.Placement {
	case frontend.PlacementHome:
		var hubs []core.Hub
		var err error
		if restricted {
			hubs, err = a.personalize.HomeCandidatesWithPolicy(ctx, req.Viewer, hc, req.Limit, allow)
		} else {
			hubs, err = a.personalize.HomeCandidatesFor(ctx, req.Viewer, hc, req.Limit)
		}
		if err != nil {
			return nil, err
		}
		resume, err := a.experienceContinue(ctx, req.Viewer, libraries, req.Limit)
		if err != nil {
			return nil, err
		}
		inserted := false
		for i := range hubs {
			if hubs[i].ID == "home.continue" {
				hubs[i].Items = resume
				inserted = true
			}
		}
		if !inserted {
			hubs = append([]core.Hub{{ID: "home.continue", Title: "Continue Watching", Style: "hero", Promoted: true, Items: resume}}, hubs...)
		}
		return hubs, nil
	case frontend.PlacementLibrary:
		id, err := strconv.Atoi(req.ViewID)
		if err != nil {
			return nil, core.ErrNotFound
		}
		var kind core.MediaType
		for _, library := range libraries {
			if library.ID == id {
				kind = library.Type
				break
			}
		}
		if kind == "" {
			return nil, core.ErrNotFound
		}
		if !experience.SectionAllowed(req.Viewer, id) {
			return nil, core.ErrNotFound
		}
		hc.SectionID = id
		if restricted {
			return a.personalize.SectionCandidatesWithPolicy(ctx, req.Viewer, kind, hc, req.Limit, allow)
		}
		return a.personalize.SectionCandidatesFor(ctx, req.Viewer, kind, hc, req.Limit)
	case frontend.PlacementDetail:
		if a.catalog == nil || req.Item == 0 {
			return nil, core.ErrNotFound
		}
		title, err := a.catalog.Get(ctx, req.Item)
		if err != nil || title == nil || !visibility.Visible(ctx, req.Viewer, title) {
			return nil, core.ErrNotFound
		}
		var postPlay func(context.Context, int) []core.PostPlayPick
		if a.cfg.Get().Personalize.PostPlay {
			postPlay = func(ctx context.Context, limit int) []core.PostPlayPick {
				picks, err := a.personalize.PostPlay(ctx, req.Viewer, req.Item, hc, limit)
				if err != nil {
					a.log.Debug("postplay picks", "key", req.Item, "err", err)
				}
				return picks
			}
		}
		return controlLegacyExperienceDetailRows(ctx, req.Viewer, req.Item, req.Limit, visibility, a.catalog.NextEpisode, postPlay,
			func(ctx context.Context, limit int) ([]core.Hub, error) {
				return a.personalize.Related(ctx, req.Viewer, req.Item, limit)
			})
	default:
		return nil, core.ErrNotFound
	}
}

func TestCandidateComposerMatchesLegacyAppPath(t *testing.T) {
	ctx := context.Background()
	a := setupTestApp(t, true)
	if err := a.users.SetOwner(ctx, plextv.Account{ID: 77901, Username: "owner"}, "owner-token"); err != nil {
		t.Fatal(err)
	}
	if err := a.users.SyncFriends(ctx, []plextv.Friend{{PlexID: 77902, Username: "restricted", AllLibs: true}}); err != nil {
		t.Fatal(err)
	}
	owner, err := a.users.Owner(ctx)
	if err != nil {
		t.Fatal(err)
	}
	users, err := a.users.Users(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var restricted *core.User
	for i := range users {
		if users[i].PlexID == 77902 {
			restricted = &users[i]
			break
		}
	}
	if restricted == nil {
		t.Fatal("restricted viewer missing")
	}
	restricted.MaxContentRating = "PG"
	if err = a.users.UpdateUser(ctx, *restricted); err != nil {
		t.Fatal(err)
	}
	released := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	visible := core.Title{Key: core.MovieKey(77901), Type: core.TypeMovie, Title: "Visible", ContentRating: "PG", Released: released}
	hidden := core.Title{Key: core.MovieKey(77902), Type: core.TypeMovie, Title: "Hidden", Adult: true, Released: released}
	for _, title := range []core.Title{visible, hidden} {
		if err = a.catalog.UpsertTitle(ctx, title, true); err != nil {
			t.Fatal(err)
		}
		if _, err = a.db.ExecContext(ctx, `UPDATE catalog_titles SET detail=1,detail_at=? WHERE key=?`, time.Now().Unix(), int64(title.Key)); err != nil {
			t.Fatal(err)
		}
	}
	for _, u := range []*core.User{owner, restricted} {
		if err = a.state.Progress(ctx, u.ID, visible.Key, 10*time.Minute, time.Hour, "paused"); err != nil {
			t.Fatal(err)
		}
		if err = a.personalize.Rebuild(ctx, u); err != nil {
			t.Fatal(err)
		}
	}
	old := a.availability
	a.availability = nil
	t.Cleanup(func() { a.availability = old })
	libs := []experience.Library{{ID: 1, Name: "Movies", Type: core.TypeMovie}, {ID: 2, Name: "TV Shows", Type: core.TypeShow}}
	vis := experienceVisibility(a.safety, a.catalog.Get, nil, nil, libs)
	situation := core.HomeSituation{Device: "tv", Daypart: "evening", Now: time.Date(2026, 1, 1, 18, 0, 0, 0, time.UTC), ClientID: "test-client", SessionID: "fixed-visit"}
	ctx = context.WithValue(ctx, experienceSituationKey{}, situation)
	for _, viewer := range []*core.User{owner, restricted} {
		for _, placement := range []frontend.Placement{frontend.PlacementHome, frontend.PlacementLibrary} {
			req := frontend.RowRequest{Viewer: viewer, Device: core.Device{ID: "test-client"}, Placement: placement, ViewID: "1", Limit: 8}
			want, we := a.controlLegacyProductionExperienceRows(ctx, req, libs, vis)
			got, ge := a.productionExperienceRows(ctx, req, libs, vis)
			if (we == nil) != (ge == nil) || !reflect.DeepEqual(experienceHubRows(want), experienceCandidateRows(got)) {
				t.Fatalf("viewer=%d placement=%s legacy=%+v %v shared=%+v %v", viewer.ID, placement, want, we, got, ge)
			}
		}
	}
	denied := *restricted
	denied.AllowedSections = []int{1}
	for _, tc := range []struct {
		name string
		ctx  context.Context
		req  frontend.RowRequest
	}{
		{"invalid library", ctx, frontend.RowRequest{Viewer: owner, Placement: frontend.PlacementLibrary, ViewID: "invalid"}},
		{"unauthorized library", ctx, frontend.RowRequest{Viewer: &denied, Placement: frontend.PlacementLibrary, ViewID: "2"}},
		{"missing situation", context.Background(), frontend.RowRequest{Viewer: owner, Placement: frontend.PlacementHome}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, wantErr := a.controlLegacyProductionExperienceRows(tc.ctx, tc.req, libs, vis)
			_, gotErr := a.productionExperienceRows(tc.ctx, tc.req, libs, vis)
			if !errors.Is(wantErr, core.ErrNotFound) || !errors.Is(gotErr, core.ErrNotFound) {
				t.Fatalf("legacy error=%v shared error=%v", wantErr, gotErr)
			}
		})
	}
}

func TestCandidateProjectionMatchesLegacyRowsWithoutWireContext(t *testing.T) {
	source := []core.Hub{{ID: "same", Title: "first", Context: "hub.first", Type: core.TypeMovie, Style: "hero", Promoted: true, Items: []core.Title{{Key: core.MovieKey(1)}}, More: true, Key: core.CollectionKey(4), ItemCount: 7}, {ID: "same", Title: "second", Context: "hub.second", Type: core.TypeShow}}
	rows, err := rankedCandidates(source, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := reflect.TypeOf(rows[0]).FieldByName("Context"); ok {
		t.Fatal("wire context leaked into candidate")
	}
	want := experienceHubRows(source)
	got := experienceCandidateRows(rows)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("legacy rows=%+v neutral rows=%+v", want, got)
	}
	if got[0].Purpose != frontend.RowCollection || got[1].Purpose != frontend.RowRecommendation {
		t.Fatalf("semantic purposes changed: %+v", got)
	}
}
