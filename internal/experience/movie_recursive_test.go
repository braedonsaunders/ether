package experience

import (
	"context"
	"testing"

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
