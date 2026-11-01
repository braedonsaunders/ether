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

// TestStoredEpisodePageCarriesRestrictedViewerPredicates keeps a restricted
// viewer on the stored path: their exclusions and plex.tv share predicate go
// into the series filter the SQL pages and counts, instead of a library walk.
func TestStoredEpisodePageCarriesRestrictedViewerPredicates(t *testing.T) {
	ctx := context.Background()
	viewer := &core.User{ID: 9, MaxContentRating: "TV-14"}
	show := core.Title{Key: core.ShowKey(10), Type: core.TypeShow, ContentRating: "TV-PG"}
	ep := core.Title{Key: core.EpisodeKey(10, 1, 1), ShowKey: show.Key, Type: core.TypeEpisode, Season: 1, Episode: 1, ContentRating: "TV-PG"}
	var paged, counted []core.Filter
	expanded := 0
	svc := &Service{cursors: &episodeCursors{}, d: ServiceDeps{
		Active:     func(context.Context, int64) (*core.User, error) { return viewer, nil },
		Libraries:  []Library{{ID: 2, Type: core.TypeShow}},
		State:      &countingBrowseState{},
		Visibility: Visibility{SectionFor: func(core.MediaType) (int, bool) { return 2, true }, GetTitle: func(context.Context, core.RatingKey) (*core.Title, error) { return &show, nil }},
		Browse: func(context.Context, core.BrowseQuery) (core.Page, error) {
			return core.Page{Items: []core.Title{show}, Total: 1}, nil
		},
		Grandchildren: func(context.Context, *core.User, core.RatingKey) ([]core.Title, error) {
			expanded++
			return []core.Title{ep}, nil
		},
		ParentTitles: func(_ context.Context, keys []core.RatingKey) (map[core.RatingKey]core.Title, error) {
			return map[core.RatingKey]core.Title{show.Key: show}, nil
		},
		PageExclusions: func(context.Context, *core.User) LibraryPageExclusions {
			return LibraryPageExclusions{
				Keys:       []core.RatingKey{core.ShowKey(99)},
				Television: core.Filter{ContentRatings: []string{"TV-PG", "TV-14"}, ExcludeCollectionNames: []string{"Hidden"}},
			}
		},
		StoredEpisodes: StoredEpisodeDeps{
			Page: func(_ context.Context, f core.Filter, after core.RatingKey, _ int) ([]core.Title, error) {
				paged = append(paged, f)
				if after >= ep.Key {
					return nil, nil
				}
				return []core.Title{ep}, nil
			},
			Count: func(_ context.Context, f core.Filter) (int, error) {
				counted = append(counted, f)
				return 1, nil
			},
			Unranked:   func(context.Context) (bool, error) { return true, nil },
			Customized: func(context.Context, *core.User) (bool, error) { return false, nil },
		},
	}}
	q := frontend.ItemQuery{Recursive: true, Types: []core.MediaType{core.TypeEpisode}, Limit: 10}
	page, err := svc.LibraryItems(ctx, viewer, core.Device{}, "", q)
	if err != nil {
		t.Fatal(err)
	}
	if expanded != 0 {
		t.Fatalf("restricted viewer walked %d series", expanded)
	}
	if len(page.Items) != 1 || page.Total != 1 {
		t.Fatalf("page = %v total %d", keysOf(page.Items), page.Total)
	}
	if len(counted) == 0 || len(paged) == 0 {
		t.Fatal("stored episode SQL was not used")
	}
	f := counted[0]
	if f.MaxContentRating != "TV-14" || !slices.Equal(f.ContentRatings, []string{"TV-PG", "TV-14"}) ||
		!slices.Equal(f.ExcludeCollectionNames, []string{"Hidden"}) || !slices.Contains(f.ExcludeKeys, core.ShowKey(99)) {
		t.Fatalf("series filter lost the viewer's predicates: %+v", f)
	}
	if !reflect.DeepEqual(paged[0], f) {
		t.Fatalf("page and count filters differ:\n%+v\n%+v", paged[0], f)
	}
	// A library section the viewer may not open lists nothing, without a walk.
	viewer.AllowedSections = []int{1}
	page, err = svc.LibraryItems(ctx, viewer, core.Device{}, "", q)
	if err != nil || len(page.Items) != 0 || expanded != 0 {
		t.Fatalf("disallowed section: %v %v expanded %d", keysOf(page.Items), err, expanded)
	}
}

func TestMergeShareFilterIntersectsAllowLists(t *testing.T) {
	f, none := MergeShareFilter(core.Filter{ContentRatings: []string{"R", "PG"}}, core.Filter{ContentRatings: []string{"g", "pg"}})
	if none || !slices.Equal(f.ContentRatings, []string{"PG"}) {
		t.Fatalf("intersection = %v none=%v", f.ContentRatings, none)
	}
	if _, none := MergeShareFilter(core.Filter{ContentRatings: []string{"R"}}, core.Filter{ContentRatings: []string{"G", "PG"}}); !none {
		t.Fatal("a client R filter under a G,PG share must match nothing")
	}
	f, none = MergeShareFilter(core.Filter{}, core.Filter{ContentRatings: []string{"G"}, ExcludeCollectionNames: []string{"X"}, MaxContentRating: "PG"})
	if none || !slices.Equal(f.ContentRatings, []string{"G"}) || f.MaxContentRating != "PG" || !slices.Equal(f.ExcludeCollectionNames, []string{"X"}) {
		t.Fatalf("share alone = %+v", f)
	}
}
