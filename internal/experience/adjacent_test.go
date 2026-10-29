package experience

import (
	"context"
	"errors"
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
)

func TestAdjacentChildrenRespectPolicyAndSeriesOrder(t *testing.T) {
	ctx := context.Background()
	v := &core.User{ID: 1}
	show := core.Title{Key: core.ShowKey(1), Type: core.TypeShow}
	episodes := []core.Title{{Key: core.EpisodeKey(1, 1, 1), Type: core.TypeEpisode, ShowKey: show.Key, Season: 1, Episode: 1}, {Key: core.EpisodeKey(1, 1, 2), Type: core.TypeEpisode, ShowKey: show.Key, Season: 1, Episode: 2, Adult: true}, {Key: core.EpisodeKey(1, 1, 3), Type: core.TypeEpisode, ShowKey: show.Key, Season: 1, Episode: 3}, {Key: core.EpisodeKey(1, 2, 1), Type: core.TypeEpisode, ShowKey: show.Key, Season: 2, Episode: 1}}
	s := &Service{d: ServiceDeps{Active: func(context.Context, int64) (*core.User, error) { return v, nil }, Get: func(_ context.Context, _ *core.User, k core.RatingKey) (*core.Title, error) {
		if k == show.Key {
			return &show, nil
		}
		return nil, core.ErrNotFound
	}, Grandchildren: func(context.Context, *core.User, core.RatingKey) ([]core.Title, error) { return episodes, nil }, Visibility: Visibility{SectionFor: func(core.MediaType) (int, bool) { return 2, true }, GetTitle: func(context.Context, core.RatingKey) (*core.Title, error) { return &show, nil }}}}
	q := frontend.ItemQuery{Recursive: true, Types: []core.MediaType{core.TypeEpisode}, AdjacentTo: episodes[2].Key}
	page, err := s.Children(ctx, v, core.Device{}, show.Key, q)
	if err != nil || page.Total != 3 || len(page.Items) != 3 || page.Items[0].Key != episodes[0].Key || page.Items[1].Key != episodes[2].Key || page.Items[2].Key != episodes[3].Key {
		t.Fatalf("cross-season window %+v %v", page, err)
	}
	q.Offset = 1
	q.Limit = 1
	page, err = s.Children(ctx, v, core.Device{}, show.Key, q)
	if err != nil || page.Total != 3 || len(page.Items) != 1 || page.Items[0].Key != episodes[2].Key {
		t.Fatalf("pagination %+v %v", page, err)
	}
	q.Offset = 0
	q.Limit = 0
	q.AdjacentTo = episodes[0].Key
	page, err = s.Children(ctx, v, core.Device{}, show.Key, q)
	if err != nil || page.Total != 2 || len(page.Items) != 2 || page.Items[0].Key != episodes[0].Key {
		t.Fatalf("first window %+v %v", page, err)
	}
	q.AdjacentTo = episodes[1].Key
	if _, err = s.Children(ctx, v, core.Device{}, show.Key, q); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("hidden anchor %v", err)
	}
	q.AdjacentTo = core.EpisodeKey(1, 9, 9)
	if _, err = s.Children(ctx, v, core.Device{}, show.Key, q); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("missing anchor %v", err)
	}
	q.AdjacentTo = episodes[2].Key
	season := 2
	q.Season = &season
	if _, err = s.Children(ctx, v, core.Device{}, show.Key, q); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("filtered anchor %v", err)
	}
	q.AdjacentTo = 0
	q.Season = nil
	page, err = s.Children(ctx, v, core.Device{}, show.Key, q)
	if err != nil || page.Total != 3 {
		t.Fatalf("normal query %+v %v", page, err)
	}
}

func TestAdjacentLibraryFiltersAndSortsBeforeSelection(t *testing.T) {
	ctx := context.Background()
	v := &core.User{ID: 1}
	titles := []core.Title{{Key: core.MovieKey(1), Type: core.TypeMovie, Title: "Zulu"}, {Key: core.MovieKey(2), Type: core.TypeMovie, Title: "Alpha"}, {Key: core.MovieKey(3), Type: core.TypeMovie, Title: "Bravo", Adult: true}, {Key: core.MovieKey(4), Type: core.TypeMovie, Title: "Charlie"}, {Key: core.MovieKey(5), Type: core.TypeMovie, Title: "Echo"}}
	s := &Service{d: ServiceDeps{Active: func(context.Context, int64) (*core.User, error) { return v, nil }, Libraries: []Library{{ID: 1, Type: core.TypeMovie}}, Visibility: Visibility{SectionFor: func(core.MediaType) (int, bool) { return 1, true }}, Browse: func(_ context.Context, q core.BrowseQuery) (core.Page, error) {
		ordered := []core.Title{titles[1], titles[2], titles[3], titles[4], titles[0]}
		return core.Page{Items: ordered, Total: len(ordered)}, nil
	}}}
	q := frontend.ItemQuery{Sort: core.SortTitle, AdjacentTo: titles[3].Key}
	page, err := s.LibraryItems(ctx, v, core.Device{}, "1", q)
	if err != nil || page.Total != 3 || len(page.Items) != 3 || page.Items[0].Key != titles[1].Key || page.Items[1].Key != titles[3].Key || page.Items[2].Key != titles[4].Key {
		t.Fatalf("sorted window %+v %v", page, err)
	}
	q.AdjacentTo = titles[2].Key
	if _, err = s.LibraryItems(ctx, v, core.Device{}, "1", q); !errors.Is(err, core.ErrNotFound) {
		t.Fatal("hidden anchor", err)
	}
}

func TestAdjacentChildrenRestrictedViewerAlternateOrderAndEdges(t *testing.T) {
	ctx := context.Background()
	viewer := &core.User{ID: 1, MaxContentRating: "TV-PG", AllowedSections: []int{2}}
	show := core.Title{Key: core.ShowKey(5), Type: core.TypeShow, ContentRating: "TV-PG"}
	first := core.Title{Key: core.EpisodeKey(5, 2, 1), Type: core.TypeEpisode, ShowKey: show.Key, Season: 2, Episode: 1, ContentRating: "TV-PG"}
	blocked := core.Title{Key: core.EpisodeKey(5, 1, 2), Type: core.TypeEpisode, ShowKey: show.Key, Season: 1, Episode: 2, ContentRating: "TV-MA"}
	middle := core.Title{Key: core.EpisodeKey(5, 1, 1), Type: core.TypeEpisode, ShowKey: show.Key, Season: 1, Episode: 1, ContentRating: "TV-PG"}
	last := core.Title{Key: core.EpisodeKey(5, 3, 1), Type: core.TypeEpisode, ShowKey: show.Key, Season: 3, Episode: 1, ContentRating: "TV-PG"}
	// Fake a viewer-specific episode layout deliberately different from canonical
	// season order; Experience must preserve the supplied presentation order.
	order := []core.Title{first, blocked, middle, last}
	s := &Service{d: ServiceDeps{Active: func(_ context.Context, id int64) (*core.User, error) {
		if id != viewer.ID {
			return nil, core.ErrUnauthorized
		}
		return viewer, nil
	}, Get: func(_ context.Context, _ *core.User, k core.RatingKey) (*core.Title, error) {
		if k == show.Key {
			return &show, nil
		}
		return nil, core.ErrNotFound
	}, Grandchildren: func(context.Context, *core.User, core.RatingKey) ([]core.Title, error) { return order, nil }, Visibility: Visibility{SectionFor: func(core.MediaType) (int, bool) { return 2, true }, GetTitle: func(context.Context, core.RatingKey) (*core.Title, error) { return &show, nil }}}}
	q := frontend.ItemQuery{Recursive: true, AdjacentTo: middle.Key}
	page, err := s.Children(ctx, viewer, core.Device{}, show.Key, q)
	if err != nil || page.Total != 3 || len(page.Items) != 3 || page.Items[0].Key != first.Key || page.Items[1].Key != middle.Key || page.Items[2].Key != last.Key {
		t.Fatalf("alternate order or restricted neighbor: %+v %v", page, err)
	}
	q.AdjacentTo = blocked.Key
	if _, err = s.Children(ctx, viewer, core.Device{}, show.Key, q); !errors.Is(err, core.ErrNotFound) {
		t.Fatal("restricted anchor", err)
	}
	q.AdjacentTo = last.Key
	q.Offset = 9
	q.Limit = 1
	page, err = s.Children(ctx, viewer, core.Device{}, show.Key, q)
	if err != nil || page.Total != 2 || len(page.Items) != 0 {
		t.Fatalf("last window offset: %+v %v", page, err)
	}
	q.Offset = -1
	q.Limit = int(^uint(0) >> 1)
	page, err = s.Children(ctx, viewer, core.Device{}, show.Key, q)
	if err != nil || page.Total != 2 || len(page.Items) != 2 || page.Items[0].Key != middle.Key || page.Items[1].Key != last.Key {
		t.Fatalf("last window limit: %+v %v", page, err)
	}
	show.ContentRating = "TV-MA"
	if _, err = s.Children(ctx, viewer, core.Device{}, show.Key, q); !errors.Is(err, core.ErrNotFound) {
		t.Fatal("restricted show", err)
	}
}

func TestAdjacentAggregateLibraryExcludesRestrictedSections(t *testing.T) {
	ctx := context.Background()
	viewer := &core.User{ID: 1, AllowedSections: []int{1}, MaxContentRating: "PG"}
	movieA := core.Title{Key: core.MovieKey(1), Type: core.TypeMovie, Title: "A", ContentRating: "G"}
	movieB := core.Title{Key: core.MovieKey(2), Type: core.TypeMovie, Title: "B", ContentRating: "PG-13"}
	movieC := core.Title{Key: core.MovieKey(3), Type: core.TypeMovie, Title: "C", ContentRating: "PG"}
	show := core.Title{Key: core.ShowKey(3), Type: core.TypeShow, Title: "D"}
	showCalls := 0
	s := &Service{d: ServiceDeps{Active: func(context.Context, int64) (*core.User, error) { return viewer, nil }, Libraries: []Library{{ID: 1, Type: core.TypeMovie}, {ID: 2, Type: core.TypeShow}}, Visibility: Visibility{SectionFor: func(typ core.MediaType) (int, bool) {
		if typ == core.TypeShow {
			return 2, true
		}
		return 1, true
	}}, Browse: func(_ context.Context, q core.BrowseQuery) (core.Page, error) {
		if q.Filter.Type == core.TypeShow {
			showCalls++
			return core.Page{Items: []core.Title{show}, Total: 1}, nil
		}
		return core.Page{Items: []core.Title{movieA, movieB, movieC}, Total: 3}, nil
	}}}
	q := frontend.ItemQuery{Sort: core.SortTitle, AdjacentTo: movieC.Key, Offset: 1, Limit: 1}
	page, err := s.LibraryItems(ctx, viewer, core.Device{}, "", q)
	if err != nil || page.Total != 2 || len(page.Items) != 1 || page.Items[0].Key != movieC.Key || showCalls != 0 {
		t.Fatalf("aggregate restricted page %+v %v, show calls %d", page, err, showCalls)
	}
	q.AdjacentTo = show.Key
	if _, err = s.LibraryItems(ctx, viewer, core.Device{}, "", q); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("restricted section anchor %v", err)
	}
}
