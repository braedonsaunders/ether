package app

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontends/plex"
	"github.com/braedonsaunders/ether/internal/sessions"
)

func TestPublishedTVFilterLibraryProfile(t *testing.T) {
	a := setupTestApp(t, true)
	ctx := context.Background()
	show := core.Title{Key: core.ShowKey(464), Type: core.TypeShow, Title: "Anime show", Released: time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC), Genres: []string{"Animation"}}
	if err := a.catalog.UpsertTitle(ctx, show, true); err != nil {
		t.Fatal(err)
	}
	var present core.Presentations
	if err := json.Unmarshal([]byte(`{"plex":{"base":"show"}}`), &present); err != nil {
		t.Fatal(err)
	}
	lib, err := a.libraries.Create(ctx, "Anime Shows", core.TypeShow, libraryRules(core.Filter{Type: core.TypeShow, Genres: []string{"Animation"}}), "hide", present)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.libraries.SetQualityProfile(ctx, lib.ID, "Anime 1080p"); err != nil {
		t.Fatal(err)
	}
	exp, err := a.newSurfaceExperience("plex")
	if err != nil {
		t.Fatal(err)
	}
	srv := plex.NewServer(plex.Deps{Host: frontend.Host{Experience: exp, Sessions: sessions.NewLocalRuntime(sessions.LocalRuntimeDeps{})}, DisableEnrichment: true})
	defer srv.Close()
	found := false
	for _, sec := range srv.LibrarySections() {
		if sec.LibraryID == lib.ID {
			found = sec.Title == "Anime Shows" && sec.Type == core.TypeShow
		}
	}
	if !found {
		t.Fatal("published filter library missing or renamed")
	}
	profiles := newLibraryProfiles(a)
	for _, title := range []*core.Title{
		{Key: core.EpisodeKey(464, 1, 1), Type: core.TypeEpisode, ShowKey: show.Key},
		{Key: core.SeasonKey(464, 1), Type: core.TypeSeason, ShowKey: show.Key},
		&show,
	} {
		if got := profiles.QualityProfileForTitle(ctx, title); got != "Anime 1080p" {
			t.Fatalf("%s profile = %q", title.Type, got)
		}
	}
	if _, ok := profiles.cached(show.Key); !ok {
		t.Fatal("missing parent show cache entry")
	}
}

func libraryRules(f core.Filter) []core.LibraryExclusion {
	return []core.LibraryExclusion{{Action: "show", MediaType: string(f.Type), Match: "all", Rules: core.LibraryPredicateConditions(f)}}
}
