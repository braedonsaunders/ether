package experience

import (
	"context"
	"sort"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
)

func TestRecursiveMovieQuerySkipsShowExpansion(t *testing.T) {
	ctx := context.Background()
	viewer := &core.User{ID: 1}
	movie := core.Title{Key: core.MovieKey(1), Type: core.TypeMovie}
	show := core.Title{Key: core.ShowKey(2), Type: core.TypeShow}
	episode := core.Title{Key: core.EpisodeKey(2, 1, 1), Type: core.TypeEpisode, ShowKey: show.Key}
	movieCalls, showCalls, expansions := 0, 0, 0
	s := &Service{d: ServiceDeps{Active: func(context.Context, int64) (*core.User, error) { return viewer, nil }, Libraries: []Library{{ID: 1, Type: core.TypeMovie}, {ID: 2, Type: core.TypeShow}}, Visibility: Visibility{SectionFor: func(typ core.MediaType) (int, bool) {
		if typ == core.TypeShow || typ == core.TypeEpisode {
			return 2, true
		}
		return 1, true
	}, GetTitle: func(context.Context, core.RatingKey) (*core.Title, error) { return &show, nil }}, Browse: func(_ context.Context, q core.BrowseQuery) (core.Page, error) {
		if q.Filter.Type == core.TypeMovie {
			movieCalls++
			return core.Page{Items: []core.Title{movie}, Total: 1}, nil
		}
		showCalls++
		return core.Page{Items: []core.Title{show}, Total: 1}, nil
	}, Children: func(context.Context, *core.User, core.RatingKey) ([]core.Title, error) {
		return nil, nil
	}, Grandchildren: func(context.Context, *core.User, core.RatingKey) ([]core.Title, error) {
		expansions++
		return []core.Title{episode}, nil
	}}}
	q := frontend.ItemQuery{Recursive: true, Types: []core.MediaType{core.TypeMovie}}
	page, err := s.LibraryItems(ctx, viewer, core.Device{}, "", q)
	if err != nil || page.Total != 1 || len(page.Items) != 1 || page.Items[0].Key != movie.Key || movieCalls != 1 || showCalls != 0 || expansions != 0 {
		t.Fatalf("movie page=%+v err=%v calls=%d/%d/%d", page, err, movieCalls, showCalls, expansions)
	}
	s.d.Grandchildren = nil
	page, err = s.LibraryItems(ctx, viewer, core.Device{}, "2", q)
	if err != nil || page.Total != 0 || showCalls != 0 {
		t.Fatalf("excluded show without provider: %+v %v", page, err)
	}
	s.d.Grandchildren = func(context.Context, *core.User, core.RatingKey) ([]core.Title, error) {
		expansions++
		return []core.Title{episode}, nil
	}
	q.Types = []core.MediaType{core.TypeEpisode}
	page, err = s.LibraryItems(ctx, viewer, core.Device{}, "2", q)
	if err != nil || page.Total != 1 || page.Items[0].Key != episode.Key || showCalls != 1 || expansions != 1 {
		t.Fatalf("episode query: %+v %v calls=%d/%d", page, err, showCalls, expansions)
	}
	q.Types = nil
	page, err = s.LibraryItems(ctx, viewer, core.Device{}, "2", q)
	if err != nil || page.Total != 2 || showCalls != 2 || expansions != 2 {
		t.Fatalf("unfiltered query: %+v %v calls=%d/%d", page, err, showCalls, expansions)
	}
	viewer.AllowedSections = []int{1}
	q.Types = []core.MediaType{core.TypeMovie}
	page, err = s.LibraryItems(ctx, viewer, core.Device{}, "", q)
	if err != nil || page.Total != 1 || showCalls != 2 || expansions != 2 {
		t.Fatalf("restricted query: %+v %v calls=%d/%d", page, err, showCalls, expansions)
	}
}

// A bounded page across every library reads only each library's first
// offset+limit titles and returns what the full merge would.
func TestRootPageMergesPerLibraryWindows(t *testing.T) {
	ctx := context.Background()
	viewer := &core.User{ID: 1}
	base := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	var movies, shows []core.Title
	for i := range 40 {
		movies = append(movies, core.Title{Key: core.MovieKey(int64(i + 1)), Type: core.TypeMovie, Title: "M", Released: base.AddDate(0, 0, 3*i)})
		shows = append(shows, core.Title{Key: core.ShowKey(int64(i + 1)), Type: core.TypeShow, Title: "S", Released: base.AddDate(0, 0, 3*i+1)})
	}
	maxLimit := 0
	page := func(items []core.Title, q core.BrowseQuery) core.Page {
		sorted := append([]core.Title(nil), items...)
		sort.Slice(sorted, func(a, b int) bool { return sorted[a].Released.After(sorted[b].Released) })
		start := min(q.Offset, len(sorted))
		end := len(sorted)
		if q.Limit > 0 {
			end = min(start+q.Limit, len(sorted))
			maxLimit = max(maxLimit, q.Limit)
		}
		return core.Page{Items: sorted[start:end], Total: len(sorted)}
	}
	s := &Service{d: ServiceDeps{Active: func(context.Context, int64) (*core.User, error) { return viewer, nil }, Libraries: []Library{{ID: 1, Type: core.TypeMovie}, {ID: 2, Type: core.TypeShow}}, Visibility: Visibility{SectionFor: func(typ core.MediaType) (int, bool) {
		if typ == core.TypeShow {
			return 2, true
		}
		return 1, true
	}}, Browse: func(_ context.Context, q core.BrowseQuery) (core.Page, error) {
		if q.Filter.Type == core.TypeMovie {
			return page(movies, q), nil
		}
		return page(shows, q), nil
	}, PageExclusions: func(context.Context, *core.User) ([]core.RatingKey, []string, []string, string) {
		return nil, nil, nil, ""
	},
		Children:      func(context.Context, *core.User, core.RatingKey) ([]core.Title, error) { return nil, nil },
		Grandchildren: func(context.Context, *core.User, core.RatingKey) ([]core.Title, error) { return nil, nil }}}
	q := frontend.ItemQuery{Recursive: true, Types: []core.MediaType{core.TypeMovie, core.TypeShow}, Sort: core.SortReleased, Desc: true}
	full, err := s.LibraryItems(ctx, viewer, core.Device{}, "", q)
	if err != nil || full.Total != 80 {
		t.Fatalf("full = %d, %v", full.Total, err)
	}
	maxLimit = 0
	q.Offset, q.Limit = 3, 5
	got, err := s.LibraryItems(ctx, viewer, core.Device{}, "", q)
	if err != nil {
		t.Fatal(err)
	}
	if got.Total != 80 || len(got.Items) != 5 || maxLimit > q.Offset+q.Limit {
		t.Fatalf("page total %d items %d library window %d", got.Total, len(got.Items), maxLimit)
	}
	for i, item := range got.Items {
		if item.Key != full.Items[3+i].Key {
			t.Fatalf("item %d = %s, full merge has %s", i, item.Key, full.Items[3+i].Key)
		}
	}
}
