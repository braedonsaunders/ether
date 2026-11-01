package app

import (
	"context"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/experience"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/plextv"
)

func TestExperienceHubRowsKeepsSemanticData(t *testing.T) {
	rows := experience.HubRows([]core.Hub{{ID: "home.continue", Context: "hub.home.continue", Items: []core.Title{{Key: core.MovieKey(1)}}}, {ID: "home.watchlist", Context: "hub.private", Title: "Saved"}})
	if len(rows) != 2 || rows[0].Purpose != frontend.RowContinue || len(rows[0].Entries) != 1 || rows[0].Entries[0].Title.Key != core.MovieKey(1) || rows[1].Purpose != frontend.RowWatchlist {
		t.Fatalf("unexpected rows: %+v", rows)
	}
}

func TestExperienceHomeFallbackUsesVisibleLibrariesAndReleaseOrder(t *testing.T) {
	ctx := context.Background()
	a := setupTestApp(t, true)
	if err := a.users.SyncFriends(ctx, []plextv.Friend{{PlexID: 831, Username: "restricted", AllLibs: true}}); err != nil {
		t.Fatal(err)
	}
	users, err := a.users.Users(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var viewer *core.User
	for i := range users {
		if users[i].PlexID == 831 {
			viewer = &users[i]
			break
		}
	}
	if viewer == nil {
		t.Fatal("viewer missing")
	}
	viewer.AllowedSections = []int{1}
	viewer.MaxContentRating = "PG"
	if err = a.users.UpdateUser(ctx, *viewer); err != nil {
		t.Fatal(err)
	}
	newerReleased := time.Date(2024, 6, 1, 0, 0, 0, 0, time.UTC)
	olderReleased := time.Date(1990, 1, 1, 0, 0, 0, 0, time.UTC)
	same := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	newer := core.Title{Key: core.MovieKey(301), Type: core.TypeMovie, Title: "Newer", ContentRating: "G", Released: newerReleased}
	older := core.Title{Key: core.MovieKey(304), Type: core.TypeMovie, Title: "Older", ContentRating: "G", Released: olderReleased}
	for _, title := range []core.Title{
		newer,
		{Key: core.MovieKey(302), Type: core.TypeMovie, Title: "Excluded", ContentRating: "G", Released: same},
		{Key: core.MovieKey(303), Type: core.TypeMovie, Title: "Restricted", ContentRating: "R", Released: same},
		older,
		{Key: core.ShowKey(301), Type: core.TypeShow, Title: "Hidden section", ContentRating: "TV-G", Released: same},
	} {
		if err = a.catalog.UpsertTitle(ctx, title, true); err != nil {
			t.Fatal(err)
		}
	}
	// Catalogue first-observed time is the reverse of release order. The shelf
	// must follow the release date anyway.
	if _, err = a.db.ExecContext(ctx, `UPDATE catalog_titles SET added_at=? WHERE key=?`, int64(10), int64(newer.Key)); err != nil {
		t.Fatal(err)
	}
	if _, err = a.db.ExecContext(ctx, `UPDATE catalog_titles SET added_at=? WHERE key=?`, int64(9000), int64(older.Key)); err != nil {
		t.Fatal(err)
	}
	if _, err = a.notInterested.Set(ctx, viewer, core.MovieKey(302), true); err != nil {
		t.Fatal(err)
	}
	libs := []experience.Library{{ID: 1, Name: "Movies", Type: core.TypeMovie}, {ID: 2, Name: "TV Shows", Type: core.TypeShow}}
	vis := experienceVisibility(a.safety, a.catalog.Get, nil, nil, libs)
	rows, err := a.experienceHomeFallback(ctx, frontend.RowRequest{Viewer: viewer, Limit: 20}, libs, vis)
	if err != nil || len(rows) != 1 || rows[0].Purpose != frontend.RowRecentlyAdded || len(rows[0].Entries) != 2 {
		t.Fatalf("fallback %+v %v", rows, err)
	}
	if rows[0].Entries[0].Title.Key != newer.Key || rows[0].Entries[1].Title.Key != older.Key {
		t.Fatalf("release order = %v then %v", rows[0].Entries[0].Title.Key, rows[0].Entries[1].Title.Key)
	}
	if !rows[0].Entries[0].Title.AddedAt.Equal(newerReleased) || rows[0].Entries[0].Title.AddedAt.Equal(time.Unix(10, 0)) {
		t.Fatalf("added time = %v, want release %v", rows[0].Entries[0].Title.AddedAt, newerReleased)
	}
}

func TestExperienceRowSourceAppendsAdditionalRows(t *testing.T) {
	source := experience.CandidateRowSource{
		Load: func(context.Context, frontend.RowRequest) ([]experience.RankedCandidate, error) {
			return []experience.RankedCandidate{{ID: "home.continue", Style: "hero", Purpose: frontend.RowContinue}}, nil
		},
		Additional: func(context.Context, frontend.RowRequest) ([]frontend.Row, error) {
			return []frontend.Row{{ID: "playlists", Purpose: core.RowPurpose("playlist")}}, nil
		},
	}
	rows, err := source.Rows(context.Background(), frontend.RowRequest{})
	if err != nil || len(rows) != 2 || rows[0].ID != "home.continue" || rows[0].Layout.Style != "hero" || rows[1].ID != "playlists" {
		t.Fatalf("rows = %+v, error = %v", rows, err)
	}
}
