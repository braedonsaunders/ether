package experience

import (
	"context"
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
)

func TestSeriesNextUpNoHistoryReturnsFirstRegularEpisode(t *testing.T) {
	series := core.ShowKey(10)
	special := core.Title{Key: core.EpisodeKey(10, 0, 1), Type: core.TypeEpisode, ShowKey: series, Season: 0, Episode: 1, Title: "Special"}
	first := core.Title{Key: core.EpisodeKey(10, 1, 1), Type: core.TypeEpisode, ShowKey: series, Season: 1, Episode: 1, Title: "S1E1"}
	second := core.Title{Key: core.EpisodeKey(10, 1, 2), Type: core.TypeEpisode, ShowKey: series, Season: 1, Episode: 2, Title: "S1E2"}
	got := SeriesNextUp([]core.Title{special, first, second}, nil, series, true, nil)
	if got == nil || got.Key != first.Key {
		t.Fatalf("no history: %+v", got)
	}
}

func TestSeriesNextUpPartialHistoryReturnsNextEpisode(t *testing.T) {
	series := core.ShowKey(10)
	first := core.Title{Key: core.EpisodeKey(10, 1, 1), Type: core.TypeEpisode, ShowKey: series, Season: 1, Episode: 1}
	second := core.Title{Key: core.EpisodeKey(10, 1, 2), Type: core.TypeEpisode, ShowKey: series, Season: 1, Episode: 2}
	third := core.Title{Key: core.EpisodeKey(10, 1, 3), Type: core.TypeEpisode, ShowKey: series, Season: 1, Episode: 3}
	st := map[core.RatingKey]core.WatchStatus{first.Key: {ViewCount: 1}}
	got := SeriesNextUp([]core.Title{first, second, third}, st, series, true, nil)
	if got == nil || got.Key != second.Key {
		t.Fatalf("partial history: %+v", got)
	}
}

func TestSeriesNextUpIgnoresOtherSeriesHistory(t *testing.T) {
	series := core.ShowKey(10)
	other := core.ShowKey(99)
	wanted := core.Title{Key: core.EpisodeKey(10, 1, 1), Type: core.TypeEpisode, ShowKey: series, Season: 1, Episode: 1}
	later := core.Title{Key: core.EpisodeKey(10, 1, 2), Type: core.TypeEpisode, ShowKey: series, Season: 1, Episode: 2}
	foreign := core.Title{Key: core.EpisodeKey(99, 1, 2), Type: core.TypeEpisode, ShowKey: other, Season: 1, Episode: 2}
	st := map[core.RatingKey]core.WatchStatus{foreign.Key: {ViewCount: 1, ViewOffset: 1}}
	got := SeriesNextUp([]core.Title{foreign, wanted, later}, st, series, true, nil)
	if got == nil || got.Key != wanted.Key {
		t.Fatalf("other series leaked: %+v", got)
	}
}

func TestSeriesNextUpEnableResumable(t *testing.T) {
	series := core.ShowKey(10)
	first := core.Title{Key: core.EpisodeKey(10, 1, 1), Type: core.TypeEpisode, ShowKey: series, Season: 1, Episode: 1}
	second := core.Title{Key: core.EpisodeKey(10, 1, 2), Type: core.TypeEpisode, ShowKey: series, Season: 1, Episode: 2}
	third := core.Title{Key: core.EpisodeKey(10, 1, 3), Type: core.TypeEpisode, ShowKey: series, Season: 1, Episode: 3}
	st := map[core.RatingKey]core.WatchStatus{first.Key: {ViewCount: 1}, second.Key: {ViewOffset: 1}}
	resumable := SeriesNextUp([]core.Title{first, second, third}, st, series, true, nil)
	if resumable == nil || resumable.Key != second.Key {
		t.Fatalf("resumable: %+v", resumable)
	}
	next := SeriesNextUp([]core.Title{first, second, third}, st, series, false, nil)
	if next == nil || next.Key != second.Key {
		t.Fatalf("EnableResumable=false still returns the first unwatched: %+v", next)
	}
	st[second.Key] = core.WatchStatus{ViewCount: 1, ViewOffset: 1}
	skipped := SeriesNextUp([]core.Title{first, second, third}, st, series, false, nil)
	if skipped == nil || skipped.Key != third.Key {
		t.Fatalf("completed-with-offset advances: %+v", skipped)
	}
}

func TestLoadSeriesNextUpUsesIndexedSeriesQuery(t *testing.T) {
	series := core.ShowKey(10)
	other := core.ShowKey(99)
	first := core.Title{Key: core.EpisodeKey(10, 1, 1), Type: core.TypeEpisode, ShowKey: series, Season: 1, Episode: 1}
	second := core.Title{Key: core.EpisodeKey(10, 1, 2), Type: core.TypeEpisode, ShowKey: series, Season: 1, Episode: 2}
	foreign := core.Title{Key: core.EpisodeKey(99, 1, 8), Type: core.TypeEpisode, ShowKey: other, Season: 1, Episode: 8}
	var gotKey core.RatingKey
	var gotQuery frontend.ItemQuery
	children := func(_ context.Context, key core.RatingKey, q frontend.ItemQuery) (core.Page, error) {
		gotKey, gotQuery = key, q
		if key != series {
			t.Fatalf("children key %v", key)
		}
		return core.Page{Items: []core.Title{foreign, first, second}, Total: 3}, nil
	}
	userData := func(_ context.Context, keys []core.RatingKey) (map[core.RatingKey]core.WatchStatus, error) {
		st := map[core.RatingKey]core.WatchStatus{foreign.Key: {ViewCount: 1}}
		for _, key := range keys {
			if key == first.Key {
				st[key] = core.WatchStatus{ViewCount: 1}
			}
		}
		return st, nil
	}
	got, err := LoadSeriesNextUp(context.Background(), series, true, children, userData)
	if err != nil || got == nil || got.Key != second.Key {
		t.Fatalf("load: %+v %v", got, err)
	}
	want := SeriesNextUpQuery()
	if gotKey != series || !gotQuery.Recursive || gotQuery.Limit != SeriesNextUpLimit || len(gotQuery.Types) != 1 || gotQuery.Types[0] != core.TypeEpisode {
		t.Fatalf("query not series-indexed: key=%v q=%+v want limit %d types %v", gotKey, gotQuery, want.Limit, want.Types)
	}
}
