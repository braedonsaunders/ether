package trackprefs

import (
	"context"
	"encoding/json"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
	"testing"
)

func TestPolicyAndShowMemory(t *testing.T) {
	ctx := context.Background()
	db, err := store.OpenMemory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s := New(db)
	r := Default()
	r.AudioLanguages = []string{"en"}
	r.SubtitleLanguages = []string{"en"}
	r.SubtitleMode = "foreign-only"
	if err = s.PutRules(ctx, 0, r); err != nil {
		t.Fatal(err)
	}
	info := core.MediaInfo{Streams: []core.StreamInfo{{Type: "audio", Language: "jpn", Index: 3}, {Type: "audio", Language: "eng", Index: 5}, {Type: "subtitle", Language: "eng", Index: 9}}}
	key := core.EpisodeKey(1396, 1, 1)
	sel, err := s.Select(ctx, 7, key, info, "ja")
	if err != nil || sel.Audio != 1 || sel.Subtitle != -1 {
		t.Fatalf("policy: %+v %v", sel, err)
	}
	if err = s.Remember(ctx, 7, key, "audio", &info.Streams[0]); err != nil {
		t.Fatal(err)
	}
	sel, err = s.Select(ctx, 7, core.EpisodeKey(1396, 1, 2), info, "ja")
	if err != nil || sel.Audio != 0 || sel.Subtitle != 2 {
		t.Fatalf("memory: %+v %v", sel, err)
	}
	sel, _ = s.Select(ctx, 8, key, info, "")
	if sel.Audio != 1 {
		t.Fatal("memory leaked between viewers")
	}
	if err = s.Remember(ctx, 7, key, "subtitle", nil); err != nil {
		t.Fatal(err)
	}
	sel, _ = s.Select(ctx, 7, key, info, "")
	if sel.Subtitle != -1 {
		t.Fatal("subtitle off not remembered")
	}
	if err = s.Forget(ctx, 7, 1396); err != nil {
		t.Fatal(err)
	}
	sel, _ = s.Select(ctx, 7, key, info, "")
	if sel.Audio != 1 {
		t.Fatal("forget failed")
	}
}
func TestFiltersAndValidation(t *testing.T) {
	r := Default()
	r.Loudness = true
	r.DialogueBoost = true
	r.NightMode = true
	if len(r.AudioFilters(2)) != 2 || len(r.AudioFilters(6)) != 3 {
		t.Fatal("filter conditions")
	}
	r.SubtitleMode = "invented"
	if r.Validate() == nil {
		t.Fatal("invalid mode accepted")
	}
}

func TestSubtitleModeSelectsATrackAndDoesNotFetch(t *testing.T) {
	ctx := context.Background()
	db, err := store.OpenMemory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s := New(db)
	info := core.MediaInfo{Streams: []core.StreamInfo{
		{Type: "audio", Language: "ja"},
		{Type: "audio", Language: "en"},
		{Type: "subtitle", Language: "en", Forced: true},
		{Type: "subtitle", Language: "en"},
	}}
	key := core.MovieKey(1)
	put := func(mode string, audio, subs []string) {
		t.Helper()
		r := Default()
		r.AudioLanguages = audio
		r.SubtitleLanguages = subs
		r.SubtitleMode = mode
		if err := s.PutRules(ctx, 4, r); err != nil {
			t.Fatal(err)
		}
	}
	put("off", []string{"ja"}, []string{"en"})
	sel, err := s.Select(ctx, 4, key, info, "ja")
	if err != nil || sel.Subtitle != -1 {
		t.Fatalf("off %+v %v", sel, err)
	}
	put("foreign-only", nil, nil)
	sel, err = s.Select(ctx, 4, key, info, "ja")
	if err != nil || sel.Subtitle != -1 {
		t.Fatalf("foreign without languages %+v %v", sel, err)
	}
	put("foreign-only", []string{"en"}, []string{"en"})
	sel, err = s.Select(ctx, 4, key, info, "ja")
	if err != nil || sel.Audio != 1 || sel.Subtitle != -1 {
		t.Fatalf("matching audio %+v %v", sel, err)
	}
	put("foreign-only", []string{"ja"}, []string{"en"})
	sel, err = s.Select(ctx, 4, key, info, "ja")
	if err != nil || sel.Audio != 0 || sel.Subtitle != 3 {
		t.Fatalf("foreign audio %+v %v", sel, err)
	}
	put("forced-only", []string{"ja"}, []string{"en"})
	sel, err = s.Select(ctx, 4, key, info, "ja")
	if err != nil || sel.Subtitle != 2 {
		t.Fatalf("forced %+v %v", sel, err)
	}
	put("always", []string{"en"}, []string{"en"})
	sel, err = s.Select(ctx, 4, key, info, "ja")
	if err != nil || sel.Audio != 1 || sel.Subtitle != 3 {
		t.Fatalf("always %+v %v", sel, err)
	}
}

func TestAppearanceRoundTrip(t *testing.T) {
	ctx := context.Background()
	db, err := store.OpenMemory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s := New(db)
	raw := json.RawMessage(`{"fontSize":"small","fontFamily":"sans-serif","fontColor":"#ffffff","backgroundColor":"#000000","backgroundStyle":"box","backgroundOpacity":75,"textOutline":false,"textOutlineColor":"#000000","position":"top"}`)
	rules := Default()
	rules.SubtitleAppearance = raw
	if err = s.PutRules(ctx, 4, rules); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetRules(ctx, 4)
	if err != nil || string(got.SubtitleAppearance) != string(raw) || got.SubtitleMode != "off" {
		t.Fatalf("%s %s %v", got.SubtitleAppearance, got.SubtitleMode, err)
	}
	rules.SubtitleAppearance = json.RawMessage(`{"fontSize":"huge"}`)
	if err = s.PutRules(ctx, 4, rules); err == nil {
		t.Fatal("invalid appearance accepted")
	}
	scoped := raw
	if err = s.PutScoped(ctx, 4, "device", "tv", ScopedRules{SubtitleAppearance: &scoped}); err != nil {
		t.Fatal(err)
	}
	rules, sources, err := s.ScopedRulesWithSources(ctx, 4, Scope{Device: "tv"})
	if err != nil || sources["appearance"] != "device" || string(rules.SubtitleAppearance) != string(raw) {
		t.Fatalf("%s %v %v", rules.SubtitleAppearance, sources, err)
	}
}
