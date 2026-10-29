package experience

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/trackprefs"
)

type fakeTrackPreferences struct {
	viewer    int64
	key       core.RatingKey
	original  string
	selection trackprefs.Selection
	rules     trackprefs.Rules
	err       error
}

func (f *fakeTrackPreferences) Select(_ context.Context, viewer int64, key core.RatingKey, _ core.MediaInfo, original string) (trackprefs.Selection, error) {
	f.viewer, f.key, f.original = viewer, key, original
	return f.selection, f.err
}
func (f *fakeTrackPreferences) GetRules(context.Context, int64) (trackprefs.Rules, error) {
	return f.rules, f.err
}

func TestTrackPreferencesInheritedLanguageAndExplicitChoices(t *testing.T) {
	ctx := context.Background()
	key := core.EpisodeKey(42, 1, 2)
	viewer := &core.User{ID: 7}
	fake := &fakeTrackPreferences{selection: trackprefs.Selection{Audio: 3, Subtitle: 4}}
	lookups := 0
	service := TrackPreferences{Source: fake, GetTitle: func(_ context.Context, k core.RatingKey) (*core.Title, error) {
		lookups++
		if k != core.ShowKey(42) {
			t.Fatalf("unexpected parent %v", k)
		}
		return &core.Title{Language: "fr"}, nil
	}}
	explicit := TrackSelection{Audio: 2, AudioSet: true, Subtitle: -1, SubtitleSet: true}
	got, err := service.Select(ctx, viewer, key, core.MediaInfo{}, "", explicit)
	if err != nil || got != explicit || fake.original != "fr" || fake.viewer != viewer.ID || fake.key != key || lookups != 1 {
		t.Fatalf("explicit preference or parent lost: %+v, fake %+v, err %v", got, fake, err)
	}
	got, err = service.Select(ctx, viewer, key, core.MediaInfo{}, "de", TrackSelection{Audio: 1, AudioSet: true})
	if err != nil || got.Audio != 1 || got.Subtitle != 4 || !got.SubtitleSet || fake.original != "de" || lookups != 1 {
		t.Fatalf("persisted fallback or original language wrong: %+v, fake %+v, err %v", got, fake, err)
	}
}

func TestTrackPreferencesFailureKeepsSelectionAndRules(t *testing.T) {
	ctx := context.Background()
	fake := &fakeTrackPreferences{err: errors.New("unavailable")}
	service := TrackPreferences{Source: fake, GetTitle: func(context.Context, core.RatingKey) (*core.Title, error) { return nil, errors.New("missing") }}
	want := TrackSelection{Audio: 1, AudioSet: true, Subtitle: -1, SubtitleSet: true}
	got, err := service.Select(ctx, &core.User{ID: 1}, core.EpisodeKey(1, 1, 1), core.MediaInfo{}, "", want)
	if err == nil || got != want || fake.original != "" {
		t.Fatalf("failure changed selection: %+v %v", got, err)
	}
	if _, err = service.AudioFilters(ctx, &core.User{ID: 1}, core.MediaInfo{}, want); err == nil {
		t.Fatal("rules failure hidden")
	}
	fake.err = nil
	fake.rules = trackprefs.Rules{NightMode: true, DialogueBoost: true}
	info := core.MediaInfo{Streams: []core.StreamInfo{{Channels: 2}, {Channels: 6}}}
	filters, err := service.AudioFilters(ctx, &core.User{ID: 1}, info, want)
	if err != nil || !reflect.DeepEqual(filters, fake.rules.AudioFilters(6)) {
		t.Fatalf("wrong filters: %v, %v", filters, err)
	}
}

func TestTrackPreferencesPositionsAndOmittedZero(t *testing.T) {
	ctx := context.Background()
	fake := &fakeTrackPreferences{selection: trackprefs.Selection{Audio: 1, Subtitle: 2}}
	service := TrackPreferences{Source: fake}
	info := core.MediaInfo{Streams: []core.StreamInfo{{Index: 40, Type: "audio"}, {Index: 9, Type: "audio"}, {Index: 0, Type: "subtitle"}}}
	viewer := &core.User{ID: 1}
	got, err := service.Select(ctx, viewer, core.MovieKey(1), info, "", TrackSelection{Audio: 0, AudioSet: true})
	if err != nil || got.Audio != 0 || got.Subtitle != 2 || !got.SubtitleSet {
		t.Fatalf("explicit position zero was lost: %+v, %v", got, err)
	}
	got, err = service.Select(ctx, viewer, core.MovieKey(1), info, "", TrackSelection{})
	if err != nil || got.Audio != 1 || got.Subtitle != 2 || !got.AudioSet || !got.SubtitleSet {
		t.Fatalf("omitted zero should use stored stream positions: %+v, %v", got, err)
	}
}
