package experience

import (
	"context"
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
)

func TestIndexedLibraryPagePushesOffsetIntoOneBrowse(t *testing.T) {
	viewer := &core.User{ID: 1, IsOwner: true}
	const n = 3000
	calls := 0
	var got core.BrowseQuery
	s := &Service{d: ServiceDeps{Active: func(context.Context, int64) (*core.User, error) { return viewer, nil }, Libraries: []Library{{ID: 1, Type: core.TypeMovie}}, Visibility: Visibility{SectionFor: func(core.MediaType) (int, bool) { return 1, true }}, PageExclusions: func(context.Context, *core.User) LibraryPageExclusions {
		return LibraryPageExclusions{Keys: []core.RatingKey{core.MovieKey(2)}, ExcludeGenres: []string{"Horror"}, ExcludeKeywords: []string{"gore"}, MaxContentRating: "PG"}
	}, Browse: func(_ context.Context, q core.BrowseQuery) (core.Page, error) {
		calls++
		got = q
		hidden := map[core.RatingKey]bool{}
		for _, key := range q.Filter.ExcludeKeys {
			hidden[key] = true
		}
		var items []core.Title
		for i := 1; i <= n; i++ {
			key := core.MovieKey(int64(i))
			if hidden[key] {
				continue
			}
			items = append(items, core.Title{Key: key, Type: core.TypeMovie, Title: "Movie"})
		}
		total := len(items)
		start := min(max(q.Offset, 0), len(items))
		items = items[start:]
		if q.Limit > 0 && len(items) > q.Limit {
			items = items[:q.Limit]
		}
		return core.Page{Items: items, Total: total}, nil
	}}}
	page, err := s.LibraryItems(context.Background(), viewer, core.Device{}, "1", frontend.ItemQuery{Offset: 10, Limit: 5})
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 || got.Offset != 10 || got.Limit != 5 || got.Filter.Type != core.TypeMovie || got.Filter.MaxContentRating != "PG" || len(got.Filter.ExcludeGenres) != 1 || got.Filter.ExcludeGenres[0] != "Horror" || len(got.Filter.ExcludeKeywords) != 1 {
		t.Fatalf("browse calls=%d query=%+v", calls, got)
	}
	if page.Total != n-1 || len(page.Items) != 5 || page.Items[0].Key != core.MovieKey(12) {
		t.Fatalf("page total=%d first=%v len=%d", page.Total, page.Items, len(page.Items))
	}
}

func TestIndexedLibraryPagePushesRecursiveSortNameIntoSQL(t *testing.T) {
	viewer := &core.User{ID: 1, IsOwner: true}
	calls := 0
	var got core.BrowseQuery
	s := &Service{d: ServiceDeps{Active: func(context.Context, int64) (*core.User, error) { return viewer, nil }, Libraries: []Library{{ID: 1, Type: core.TypeMovie}, {ID: 2, Type: core.TypeShow}}, Visibility: Visibility{SectionFor: func(typ core.MediaType) (int, bool) {
		if typ == core.TypeShow {
			return 2, true
		}
		return 1, true
	}}, PageExclusions: func(context.Context, *core.User) LibraryPageExclusions {
		return LibraryPageExclusions{}
	}, Browse: func(_ context.Context, q core.BrowseQuery) (core.Page, error) {
		calls++
		got = q
		return core.Page{Items: []core.Title{{Key: core.MovieKey(1), Type: q.Filter.Type, Title: "A"}}, Total: 40}, nil
	}, Grandchildren: func(context.Context, *core.User, core.RatingKey) ([]core.Title, error) {
		t.Fatal("recursive movie and series pages must not expand episodes")
		return nil, nil
	}}}
	q := frontend.ItemQuery{Recursive: true, Types: []core.MediaType{core.TypeMovie}, Sort: core.SortTitle, Limit: 50}
	page, err := s.LibraryItems(context.Background(), viewer, core.Device{}, "1", q)
	if err != nil || calls != 1 || got.Offset != 0 || got.Limit != 50 || got.Sort != core.SortTitle || got.Filter.Type != core.TypeMovie || page.Total != 40 {
		t.Fatalf("movie calls=%d query=%+v page=%+v err=%v", calls, got, page, err)
	}
	q.Types = []core.MediaType{core.TypeShow}
	page, err = s.LibraryItems(context.Background(), viewer, core.Device{}, "2", q)
	if err != nil || calls != 2 || got.Filter.Type != core.TypeShow || got.Limit != 50 || page.Items[0].Type != core.TypeShow {
		t.Fatalf("series calls=%d query=%+v page=%+v err=%v", calls, got, page, err)
	}
	q.Types = []core.MediaType{core.TypeEpisode}
	expanded := 0
	s.d.Grandchildren = func(context.Context, *core.User, core.RatingKey) ([]core.Title, error) {
		expanded++
		return []core.Title{{Key: core.EpisodeKey(9, 1, 1), Type: core.TypeEpisode}}, nil
	}
	s.d.Children = func(context.Context, *core.User, core.RatingKey) ([]core.Title, error) { return nil, nil }
	s.d.Browse = func(_ context.Context, q core.BrowseQuery) (core.Page, error) {
		calls++
		return core.Page{Items: []core.Title{{Key: core.ShowKey(9), Type: core.TypeShow}}, Total: 1}, nil
	}
	page, err = s.LibraryItems(context.Background(), viewer, core.Device{}, "2", q)
	if err != nil || expanded != 1 || page.Total != 1 || page.Items[0].Type != core.TypeEpisode {
		t.Fatalf("episodes expanded=%d page=%+v err=%v", expanded, page, err)
	}
}

func TestIndexedLibraryPageFallsBackWhenARowIsHidden(t *testing.T) {
	viewer := &core.User{ID: 1, IsOwner: true}
	calls := 0
	titles := []core.Title{{Key: core.MovieKey(1), Type: core.TypeMovie}, {Key: core.MovieKey(2), Type: core.TypeMovie, Adult: true}, {Key: core.MovieKey(3), Type: core.TypeMovie}}
	s := &Service{d: ServiceDeps{Active: func(context.Context, int64) (*core.User, error) { return viewer, nil }, Libraries: []Library{{ID: 1, Type: core.TypeMovie}}, Visibility: Visibility{SectionFor: func(core.MediaType) (int, bool) { return 1, true }}, PageExclusions: func(context.Context, *core.User) LibraryPageExclusions {
		return LibraryPageExclusions{}
	}, Browse: func(_ context.Context, q core.BrowseQuery) (core.Page, error) {
		calls++
		start := min(max(q.Offset, 0), len(titles))
		end := len(titles)
		if q.Limit > 0 && start+q.Limit < end {
			end = start + q.Limit
		}
		return core.Page{Items: append([]core.Title(nil), titles[start:end]...), Total: len(titles)}, nil
	}}}
	page, err := s.LibraryItems(context.Background(), viewer, core.Device{}, "1", frontend.ItemQuery{Offset: 1, Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if calls < 2 || page.Total != 2 || len(page.Items) != 1 || page.Items[0].Key != core.MovieKey(3) {
		t.Fatalf("calls=%d page=%+v", calls, page)
	}
}
