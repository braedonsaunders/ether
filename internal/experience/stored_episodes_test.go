package experience

import (
	"context"
	"reflect"
	"slices"
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
)

// TestStoredEpisodePageMatchesTheLibraryWalk pages a recursive episode
// listing from the stored episodes and from the library walk, and checks
// they agree while the stored path expands no series.
func TestStoredEpisodePageMatchesTheLibraryWalk(t *testing.T) {
	ctx := context.Background()
	viewer := &core.User{ID: 7}
	shows := []core.Title{
		{Key: core.ShowKey(30), Type: core.TypeShow},
		{Key: core.ShowKey(10), Type: core.TypeShow},
		{Key: core.ShowKey(20), Type: core.TypeShow, Adult: true}, // hidden series
		{Key: core.ShowKey(40), Type: core.TypeShow},
	}
	byKey := map[core.RatingKey]core.Title{}
	episodes := map[core.RatingKey][]core.Title{}
	var stored []core.Title
	for _, show := range shows {
		byKey[show.Key] = show
		for s := 1; s <= 3; s++ {
			for e := 1; e <= 7; e++ {
				ep := core.Title{Key: core.EpisodeKey(show.Key.TMDB(), s, e), ShowKey: show.Key, Type: core.TypeEpisode, Season: s, Episode: e, Adult: s == 2 && e == 3}
				episodes[show.Key] = append(episodes[show.Key], ep)
				if !ep.Adult {
					stored = append(stored, ep)
				}
			}
		}
	}
	slices.SortFunc(stored, func(a, b core.Title) int { return int(a.Key - b.Key) })
	expanded := 0
	svc := &Service{cursors: &episodeCursors{}, d: ServiceDeps{
		Active:     func(context.Context, int64) (*core.User, error) { return viewer, nil },
		Libraries:  []Library{{ID: 2, Type: core.TypeShow}},
		State:      &countingBrowseState{},
		Visibility: Visibility{SectionFor: func(core.MediaType) (int, bool) { return 2, true }, GetTitle: func(_ context.Context, k core.RatingKey) (*core.Title, error) { t := byKey[k]; return &t, nil }},
		Browse: func(_ context.Context, q core.BrowseQuery) (core.Page, error) {
			if q.Offset > 0 {
				return core.Page{Total: len(shows)}, nil
			}
			return core.Page{Items: shows, Total: len(shows)}, nil
		},
		Grandchildren: func(_ context.Context, _ *core.User, k core.RatingKey) ([]core.Title, error) {
			expanded++
			return episodes[k], nil
		},
		ParentTitles: func(_ context.Context, keys []core.RatingKey) (map[core.RatingKey]core.Title, error) {
			out := map[core.RatingKey]core.Title{}
			for _, k := range keys {
				out[k] = byKey[k]
			}
			return out, nil
		},
	}}
	storedDeps := StoredEpisodeDeps{
		Page: func(_ context.Context, _ core.Filter, after core.RatingKey, limit int) ([]core.Title, error) {
			var out []core.Title
			for _, ep := range stored {
				if ep.Key > after && len(out) < limit {
					out = append(out, ep)
				}
			}
			return out, nil
		},
		Count:      func(context.Context, core.Filter) (int, error) { return 63 - 3, nil },
		Unranked:   func(context.Context) (bool, error) { return true, nil },
		Customized: func(context.Context, *core.User) (bool, error) { return false, nil },
	}
	list := func(offset, limit int) core.Page {
		t.Helper()
		page, err := svc.LibraryItems(ctx, viewer, core.Device{}, "", frontend.ItemQuery{Recursive: true, Types: []core.MediaType{core.TypeEpisode}, Offset: offset, Limit: limit})
		if err != nil {
			t.Fatal(err)
		}
		return page
	}
	for _, p := range [][2]int{{0, 5}, {5, 5}, {10, 17}, {0, 100}, {55, 10}, {70, 5}} {
		svc.d.StoredEpisodes = StoredEpisodeDeps{}
		want := list(p[0], p[1])
		svc.d.StoredEpisodes = storedDeps
		expanded = 0
		got := list(p[0], p[1])
		if !reflect.DeepEqual(keysOf(got.Items), keysOf(want.Items)) || got.Total != want.Total {
			t.Fatalf("page %v:\n got %v (total %d)\nwant %v (total %d)", p, keysOf(got.Items), got.Total, keysOf(want.Items), want.Total)
		}
		if expanded != 0 {
			t.Fatalf("the stored path expanded %d series", expanded)
		}
	}
	// A custom episode order keeps the walk.
	svc.d.StoredEpisodes.Customized = func(context.Context, *core.User) (bool, error) { return true, nil }
	expanded = 0
	list(0, 5)
	if expanded == 0 {
		t.Fatal("a custom episode order did not walk the library")
	}
}

func keysOf(items []core.Title) []core.RatingKey {
	out := make([]core.RatingKey, len(items))
	for i := range items {
		out[i] = items[i].Key
	}
	return out
}
