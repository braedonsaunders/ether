package app

import (
	"context"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/plextv"
)

func TestSurfaceLibraryActivityUsesCatalogAndWatchState(t *testing.T) {
	ctx := context.Background()
	a := setupTestApp(t, true)
	if err := a.users.SetOwner(ctx, plextv.Account{ID: 831, Username: "owner"}, "owner-token"); err != nil {
		t.Fatal(err)
	}
	viewer, err := a.users.Owner(ctx)
	if err != nil {
		t.Fatal(err)
	}
	title := core.Title{Key: core.MovieKey(831), Type: core.TypeMovie, Title: "Cold library item", Released: time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)}
	if err = a.catalog.UpsertTitle(ctx, title, true); err != nil {
		t.Fatal(err)
	}
	if err = a.state.Progress(ctx, viewer.ID, title.Key, 10*time.Minute, time.Hour, "paused"); err != nil {
		t.Fatal(err)
	}
	resumable := core.Title{Key: core.MovieKey(832), Type: core.TypeMovie, Title: "One minute", Released: title.Released}
	tooEarly := core.Title{Key: core.MovieKey(833), Type: core.TypeMovie, Title: "Thirty seconds", Released: title.Released}
	for _, item := range []core.Title{resumable, tooEarly} {
		if err = a.catalog.UpsertTitle(ctx, item, true); err != nil {
			t.Fatal(err)
		}
	}
	// 61s of 97 minutes is under 5%, so it is not a resume point.
	if err = a.state.Progress(ctx, viewer.ID, resumable.Key, 61*time.Second, 97*time.Minute, "paused"); err != nil {
		t.Fatal(err)
	}
	if err = a.state.Progress(ctx, viewer.ID, tooEarly.Key, 30*time.Second, 97*time.Minute, "paused"); err != nil {
		t.Fatal(err)
	}
	availability := a.availability
	a.availability = nil
	t.Cleanup(func() { a.availability = availability })
	experience, err := a.newSurfaceExperience("jellyfin")
	if err != nil {
		t.Fatal(err)
	}
	activity, ok := experience.(frontend.LibraryActivity)
	if !ok {
		t.Fatal("production experience lacks library activity")
	}
	latest, err := activity.LatestAdded(ctx, viewer, core.Device{ID: "tv"}, "1", 20)
	if err != nil || len(latest) < 3 || latest[0].AddedAt == nil {
		t.Fatalf("cold library latest %+v: %v", latest, err)
	}
	resume, err := activity.Resume(ctx, viewer, core.Device{ID: "tv"}, "", []core.MediaType{core.TypeMovie}, 0, 20, true)
	if err != nil {
		t.Fatalf("watch state resume %+v: %v", resume, err)
	}
	seen := make(map[core.RatingKey]bool)
	for _, item := range resume.Items {
		seen[item.Key] = true
	}
	if !seen[title.Key] || seen[resumable.Key] || seen[tooEarly.Key] {
		t.Fatalf("resume threshold: %+v", resume)
	}
}

func TestSurfaceLatestTVUsesAiredEpisodeDates(t *testing.T) {
	ctx := context.Background()
	a := setupTestApp(t, true)
	if err := a.users.SetOwner(ctx, plextv.Account{ID: 832, Username: "owner"}, "owner-token"); err != nil {
		t.Fatal(err)
	}
	viewer, err := a.users.Owner(ctx)
	if err != nil {
		t.Fatal(err)
	}
	show := core.Title{Key: core.ShowKey(832), Type: core.TypeShow, Title: "Series", Released: time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)}
	early := core.Title{Key: core.EpisodeKey(832, 1, 1), Type: core.TypeEpisode, ShowKey: show.Key, Title: "First", Released: time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)}
	latest := core.Title{Key: core.EpisodeKey(832, 1, 2), Type: core.TypeEpisode, ShowKey: show.Key, Title: "Latest", Released: time.Date(2021, 1, 1, 0, 0, 0, 0, time.UTC)}
	future := core.Title{Key: core.EpisodeKey(832, 1, 3), Type: core.TypeEpisode, ShowKey: show.Key, Title: "Unaired", Released: time.Now().Add(48 * time.Hour)}
	for _, title := range []core.Title{show, early, latest, future} {
		if err = a.catalog.UpsertTitle(ctx, title, title.Type == core.TypeShow); err != nil {
			t.Fatal(err)
		}
	}
	availability := a.availability
	a.availability = nil
	t.Cleanup(func() { a.availability = availability })
	experience, err := a.newSurfaceExperience("jellyfin")
	if err != nil {
		t.Fatal(err)
	}
	activity, ok := experience.(frontend.LibraryActivityGrouping)
	if !ok {
		t.Fatal("production experience lacks latest grouping")
	}
	grouped, err := activity.LatestAddedGrouped(ctx, viewer, core.Device{ID: "tv"}, "2", 20, true)
	if err != nil || len(grouped) != 1 || grouped[0].Title.Key != show.Key || grouped[0].AddedAt == nil || !grouped[0].AddedAt.Equal(latest.Released) {
		t.Fatalf("grouped TV latest %+v: %v", grouped, err)
	}
	ungrouped, err := activity.LatestAddedGrouped(ctx, viewer, core.Device{ID: "tv"}, "2", 20, false)
	if err != nil || len(ungrouped) != 2 || ungrouped[0].Title.Key != latest.Key || ungrouped[0].AddedAt == nil || !ungrouped[0].AddedAt.Equal(latest.Released) || ungrouped[1].Title.Key != early.Key {
		t.Fatalf("ungrouped TV latest %+v: %v", ungrouped, err)
	}
}
