package trackprefs

import (
	"context"
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
)

func TestScopedSelectionPrecedence(t *testing.T) {
	ctx := context.Background()
	db := store.OpenTest(t)
	s := New(db)
	info := core.MediaInfo{Streams: []core.StreamInfo{{Type: "audio", Language: "en"}, {Type: "audio", Language: "fr"}, {Type: "audio", Language: "de"}, {Type: "audio", Language: "es"}}}
	key := core.EpisodeKey(123, 1, 1)
	global := Default()
	global.AudioLanguages = []string{"en"}
	if err := s.PutRules(ctx, 7, global); err != nil {
		t.Fatal(err)
	}
	scope := Scope{Device: "tv", Series: "123", Library: "1"}
	if _, changed, err := s.SelectScoped(ctx, 7, key, info, "", scope); err != nil || changed {
		t.Fatalf("empty scope changed selection: %v %v", changed, err)
	}
	for _, entry := range []struct {
		scope, id, language string
		want                int
	}{{"library", "1", "fr", 1}, {"series", "123", "de", 2}, {"device", "tv", "es", 3}} {
		languages := []string{entry.language}
		if err := s.PutScoped(ctx, 7, entry.scope, entry.id, ScopedRules{AudioLanguages: &languages}); err != nil {
			t.Fatal(err)
		}
		selected, changed, err := s.SelectScoped(ctx, 7, key, info, "", scope)
		if err != nil || !changed || selected.Audio != entry.want {
			t.Fatalf("%s: %+v %v %v", entry.scope, selected, changed, err)
		}
	}
	if err := s.Remember(ctx, 7, key, "audio", &info.Streams[0]); err != nil {
		t.Fatal(err)
	}
	selected, _, err := s.SelectScoped(ctx, 7, key, info, "", scope)
	if err != nil || selected.Audio != 0 {
		t.Fatalf("remembered choice: %+v %v", selected, err)
	}
}

func TestViewerBitrateNeverRaisesExistingCap(t *testing.T) {
	for _, tc := range []struct{ existing, preference, want int }{{2000, 8000, 2000}, {8000, 2000, 2000}, {0, 3000, 3000}, {3000, 0, 3000}} {
		if got := LimitBitrate(tc.existing, tc.preference); got != tc.want {
			t.Fatalf("limit(%d,%d) = %d, want %d", tc.existing, tc.preference, got, tc.want)
		}
	}
}

func TestScopedFieldUpdatesDoNotReplaceOtherFields(t *testing.T) {
	ctx := context.Background()
	db := store.OpenTest(t)
	s := New(db)
	audio, subs := []string{"fr"}, []string{"de"}
	if err := s.PutScoped(ctx, 1, "device", "tv", ScopedRules{AudioLanguages: &audio}); err != nil {
		t.Fatal(err)
	}
	if err := s.PutScoped(ctx, 1, "device", "tv", ScopedRules{SubtitleLanguages: &subs}); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteScopedField(ctx, 1, "device", "tv", "audio"); err != nil {
		t.Fatal(err)
	}
	r, changed, err := s.ScopedRules(ctx, 1, Scope{Device: "tv"})
	if err != nil || !changed || len(r.AudioLanguages) != 0 || len(r.SubtitleLanguages) != 1 || r.SubtitleLanguages[0] != "de" {
		t.Fatalf("scope: %+v %v %v", r, changed, err)
	}
}
