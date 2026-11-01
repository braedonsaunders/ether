package trackprefs

import (
	"context"
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
)

func plexSelect(t *testing.T, layer PlexLayer, user int64, setup func(*Service)) Selection {
	t.Helper()
	ctx := context.Background()
	db, err := store.OpenMemory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	s := New(db, Options{PlexLayer: func(_ context.Context, id int64) (PlexLayer, bool) {
		if id != user {
			return PlexLayer{}, false
		}
		return layer, true
	}})
	if setup != nil {
		setup(s)
	}
	info := core.MediaInfo{Streams: []core.StreamInfo{
		{Type: "audio", Language: "jpn", Index: 0},
		{Type: "audio", Language: "eng", Index: 1},
		{Type: "subtitle", Language: "eng", Index: 2},
		{Type: "subtitle", Language: "eng", Forced: true, Index: 3},
	}}
	sel, err := s.Select(ctx, user, core.MovieKey(1), info, "ja")
	if err != nil {
		t.Fatal(err)
	}
	return sel
}

func TestPlexLayerAppliesWhenNothingStored(t *testing.T) {
	sel := plexSelect(t, PlexLayer{AudioLanguage: "ja", AutoSelectAudio: true, SubtitleLanguage: "en", SubtitleMode: "always"}, 7, nil)
	if sel.Audio != 0 || sel.Subtitle != 2 {
		t.Fatalf("defaults %+v", sel)
	}
}

func TestPlexLayerOwnerRulesWin(t *testing.T) {
	sel := plexSelect(t, PlexLayer{AudioLanguage: "ja", AutoSelectAudio: true, SubtitleLanguage: "en", SubtitleMode: "always"}, 7, func(s *Service) {
		r := Default()
		r.AudioLanguages = []string{"en"}
		r.SubtitleLanguages = []string{"en"}
		r.SubtitleMode = "off"
		if err := s.PutRules(context.Background(), 0, r); err != nil {
			t.Fatal(err)
		}
	})
	if sel.Audio != 1 || sel.Subtitle != -1 {
		t.Fatalf("owner %+v", sel)
	}
}

func TestPlexLayerExplicitPickWins(t *testing.T) {
	sel := plexSelect(t, PlexLayer{AudioLanguage: "ja", AutoSelectAudio: true, SubtitleLanguage: "en", SubtitleMode: "always"}, 7, func(s *Service) {
		info := core.MediaInfo{Streams: []core.StreamInfo{{Type: "audio", Language: "eng", Index: 1}}}
		if err := s.Remember(context.Background(), 7, core.MovieKey(1), "audio", &info.Streams[0]); err != nil {
			t.Fatal(err)
		}
	})
	if sel.Audio != 1 {
		t.Fatalf("pick %+v", sel)
	}
}

func TestPlexLayerIsBaselineUnderScopedOverride(t *testing.T) {
	ctx := context.Background()
	db, err := store.OpenMemory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	s := New(db, Options{PlexLayer: func(_ context.Context, id int64) (PlexLayer, bool) {
		if id != 7 {
			return PlexLayer{}, false
		}
		return PlexLayer{AudioLanguage: "ja", AutoSelectAudio: true, SubtitleLanguage: "en", SubtitleMode: "always"}, true
	}})
	r, err := s.GetRules(ctx, 7)
	if err != nil || r.SubtitleMode != "always" || len(r.AudioLanguages) != 1 || r.AudioLanguages[0] != "ja" {
		t.Fatalf("plex baseline %+v %v", r, err)
	}
	off := "off"
	if err := s.PutScoped(ctx, 7, "device", "tv", ScopedRules{SubtitleMode: &off}); err != nil {
		t.Fatal(err)
	}
	got, sources, err := s.ScopedRulesWithSources(ctx, 7, Scope{Device: "tv"})
	if err != nil || got.SubtitleMode != "off" || sources["subtitle_mode"] != "device" {
		t.Fatalf("scoped mode %+v %v %v", got, sources, err)
	}
	if sources["audio"] != "plex" || sources["subtitle"] != "plex" {
		t.Fatalf("plex languages must remain marked plex: %v", sources)
	}
	info := core.MediaInfo{Streams: []core.StreamInfo{
		{Type: "audio", Language: "jpn", Index: 0},
		{Type: "subtitle", Language: "eng", Index: 2},
	}}
	sel, changed, err := s.SelectScoped(ctx, 7, core.MovieKey(1), info, "ja", Scope{Device: "tv"})
	if err != nil || !changed || sel.Audio != 0 || sel.Subtitle != -1 {
		t.Fatalf("scoped over plex %+v changed=%v %v", sel, changed, err)
	}
	_, onlyPlex, err := s.ScopedRules(ctx, 7, Scope{})
	if err != nil || onlyPlex {
		t.Fatalf("plex-only is not a scoped change: %v %v", onlyPlex, err)
	}
}

func TestPlexLayerViewerRulesWin(t *testing.T) {
	sel := plexSelect(t, PlexLayer{AudioLanguage: "ja", AutoSelectAudio: true, SubtitleMode: "always"}, 7, func(s *Service) {
		r := Default()
		r.AudioLanguages = []string{"en"}
		r.SubtitleMode = "off"
		if err := s.PutRules(context.Background(), 7, r); err != nil {
			t.Fatal(err)
		}
	})
	if sel.Audio != 1 || sel.Subtitle != -1 {
		t.Fatalf("viewer %+v", sel)
	}
}
