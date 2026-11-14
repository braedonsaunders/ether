package app

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontends/plex"
	"github.com/braedonsaunders/ether/internal/sessions"
)

func TestPublishedTVFilterLibraryProfile(t *testing.T) {
	a := setupTestAppConfigured(t, true, Options{}, func(c *config.Config) {
		c.Quality.Profiles = append(c.Quality.Profiles, core.QualityProfile{Name: "Anime 1080p", MediaType: core.MediaTV, MaxHeight: 1080})
	})
	ctx := context.Background()
	show := core.Title{Key: core.ShowKey(464), Type: core.TypeShow, Title: "Anime show", Released: time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC), Genres: []string{"Animation"}}
	if err := a.catalog.UpsertTitle(ctx, show, true); err != nil {
		t.Fatal(err)
	}
	var present core.Presentations
	if err := json.Unmarshal([]byte(`{"plex":{"base":"show"}}`), &present); err != nil {
		t.Fatal(err)
	}
	lib, err := a.libraries.CreateFilter(ctx, "Anime Shows", core.Filter{Type: core.TypeShow, Genres: []string{"Animation"}}, present)
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
		{Key: core.EpisodeKey(464, 1, 2), Type: core.TypeEpisode},
		{Key: core.SeasonKey(464, 2), Type: core.TypeSeason},
	} {
		if got := profiles.QualityProfileForTitle(ctx, title); got != "Anime 1080p" {
			t.Fatalf("%s profile = %q", title.Type, got)
		}
	}
	if _, ok := profiles.cached(show.Key); !ok {
		t.Fatal("missing parent show cache entry")
	}
}

// Overlapping libraries must make the same choice for every client entry point.
func TestLibraryProfileResolutionInvariant(t *testing.T) {
	a := setupTestAppConfigured(t, true, Options{}, func(c *config.Config) {
		c.Quality.Profiles = []core.QualityProfile{
			{Name: "Home", MediaType: core.MediaMovie, MaxHeight: 2160},
			{Name: "Custom", MediaType: core.MediaMovie, MaxHeight: 720},
			{Name: "Other", MediaType: core.MediaMovie, MaxHeight: 1080},
			{Name: "Shows", MediaType: core.MediaTV, MaxHeight: 1080},
		}
		c.Quality.DefaultMovie = "Home"
		c.Quality.DefaultTV = "Shows"
		c.Quality.Remote = ""
		c.Quality.RemoteMovie = ""
	})
	ctx := t.Context()
	title := core.Title{Key: core.MovieKey(311), Type: core.TypeMovie, Title: "Test", Genres: []string{"Animation"}}
	if err := a.catalog.UpsertTitle(ctx, title, true); err != nil {
		t.Fatal(err)
	}
	var libs []core.Library
	for _, name := range []string{"Default library", "First assigned", "Second assigned"} {
		lib, err := a.libraries.CreateFilter(ctx, name, core.Filter{Type: core.TypeMovie, Genres: []string{"Animation"}}, core.Presentations{})
		if err != nil {
			t.Fatal(err)
		}
		libs = append(libs, lib)
	}
	for i, name := range []string{"", "Custom", "Other"} {
		if err := a.libraries.SetQualityProfile(ctx, libs[i].ID, name); err != nil {
			t.Fatal(err)
		}
	}
	lookup := newLibraryProfiles(a)
	if got := lookup.QualityProfileForTitle(ctx, &title); got != "Custom" {
		t.Fatalf("overlap profile = %q", got)
	}
	for _, protocol := range []string{"plex", "jellyfin", "emby", "silo", "stremio", "xtream", "vela"} {
		caps := core.ClientCaps{Protocol: core.Protocol(protocol)}
		if got := a.playbackProfile(caps, nil, title.Key); got.Name != "Custom" {
			t.Fatalf("%s playback = %q", protocol, got.Name)
		}
		if got := a.resolver.ProfileForTitle(ctx, caps, nil, &title); got.Name != "Custom" {
			t.Fatalf("%s resolver = %q", protocol, got.Name)
		}
	}
	// An unrelated collection must not capture a title merely because it exists.
	collection, err := a.libraries.CreateCollection(ctx, "Unrelated", 311, core.TypeMovie, core.Presentations{})
	if err != nil {
		t.Fatal(err)
	}
	if err := a.libraries.SetQualityProfile(ctx, collection.ID, "Home"); err != nil {
		t.Fatal(err)
	}
	outsider := core.Title{Key: core.MovieKey(312), Type: core.TypeMovie, Title: "Outside"}
	if err := a.catalog.UpsertTitle(ctx, outsider, true); err != nil {
		t.Fatal(err)
	}
	if got := lookup.QualityProfileForTitle(ctx, &outsider); got != "" {
		t.Fatalf("nonmember captured by collection: %q", got)
	}
	if _, err := a.db.ExecContext(ctx, `INSERT INTO collections_members(spec_id,pos,key,shared) VALUES(311,0,?,1)`, outsider.Key); err != nil {
		t.Fatal(err)
	}
	// A library edit invalidates membership immediately; catalogue writes do too.
	if err := a.libraries.SetQualityProfile(ctx, collection.ID, "Other"); err != nil {
		t.Fatal(err)
	}
	if got := lookup.QualityProfileForTitle(ctx, &outsider); got != "Other" {
		t.Fatalf("collection member profile = %q", got)
	}
	before := lookup.PolicyKey(ctx)
	if err := a.libraries.SetQualityProfile(ctx, libs[1].ID, "Shows"); err != nil {
		t.Fatal(err)
	}
	if got := lookup.QualityProfileForTitle(ctx, &title); got != "Other" {
		t.Fatalf("mismatch fallback = %q", got)
	}
	if before == lookup.PolicyKey(ctx) {
		t.Fatal("library edit did not invalidate evidence policy")
	}
	if got := a.playbackProfile(core.ClientCaps{}, nil, core.PluginKey(1)); got.Name != "" || got.MinHeight != 0 {
		t.Fatalf("plugin video policy = %+v", got)
	}
}
