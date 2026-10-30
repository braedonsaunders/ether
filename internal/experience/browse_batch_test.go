package experience

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
)

type countingBrowseState struct {
	serviceState
	gets, batches int
	keys          []core.RatingKey
	err           error
}

func (s *countingBrowseState) Get(ctx context.Context, id int64, key core.RatingKey) (core.WatchStatus, error) {
	s.gets++
	return s.serviceState.Get(ctx, id, key)
}
func (s *countingBrowseState) GetMany(_ context.Context, _ int64, keys []core.RatingKey) (map[core.RatingKey]core.WatchStatus, error) {
	s.batches++
	s.keys = append(s.keys, keys...)
	if s.err != nil {
		return nil, s.err
	}
	result := make(map[core.RatingKey]core.WatchStatus, len(keys))
	for _, key := range keys {
		if int64(key)%2 == 0 {
			result[key] = core.WatchStatus{Key: key, ViewCount: 1}
		}
	}
	return result, nil
}
func TestLibraryBrowseBatchesWatchStateBeforePagination(t *testing.T) {
	viewer := &core.User{ID: 1}
	st := &countingBrowseState{}
	titles := make([]core.Title, 501)
	for i := range titles {
		titles[i] = core.Title{Key: core.RatingKey(i + 1), Type: core.TypeMovie}
	}
	titles[3].Adult = true
	s := &Service{d: ServiceDeps{Active: func(context.Context, int64) (*core.User, error) { return viewer, nil }, Libraries: []Library{{ID: 1, Type: core.TypeMovie}}, State: st, Visibility: Visibility{SectionFor: func(core.MediaType) (int, bool) { return 1, true }}, Browse: func(_ context.Context, q core.BrowseQuery) (core.Page, error) {
		end := min(q.Offset+q.Limit, len(titles))
		return core.Page{Items: titles[q.Offset:end], Total: len(titles)}, nil
	}}}
	watched := true
	page, err := s.LibraryItems(context.Background(), viewer, core.Device{}, "1", frontend.ItemQuery{Watched: &watched, Offset: 2, Limit: 3})
	if err != nil {
		t.Fatal(err)
	}
	if page.Total != 249 || len(page.Items) != 3 || page.Items[0].Key != 8 || page.Items[1].Key != 10 || page.Items[2].Key != 12 {
		t.Fatalf("unexpected page: %+v", page)
	}
	if st.gets != 0 || st.batches != 3 {
		t.Fatalf("state calls: get=%d batch=%d", st.gets, st.batches)
	}
	for _, key := range st.keys {
		if key == 4 {
			t.Fatal("hidden item reached watch-state lookup")
		}
	}
	unwatched := false
	unlimited, err := s.LibraryItems(context.Background(), viewer, core.Device{}, "1", frontend.ItemQuery{Watched: &unwatched})
	if err != nil || unlimited.Total != 251 {
		t.Fatalf("unwatched: %+v %v", unlimited, err)
	}
	bounded, err := s.LibraryItems(context.Background(), viewer, core.Device{}, "1", frontend.ItemQuery{Watched: &unwatched, Offset: 3, Limit: 5})
	if err != nil || bounded.Total != unlimited.Total || !reflect.DeepEqual(bounded.Items, unlimited.Items[3:8]) {
		t.Fatalf("bounded differs: %+v %v", bounded, err)
	}
	beyond, err := s.LibraryItems(context.Background(), viewer, core.Device{}, "1", frontend.ItemQuery{Watched: &unwatched, Offset: 999, Limit: 5})
	if err != nil || beyond.Total != unlimited.Total || len(beyond.Items) != 0 {
		t.Fatalf("offset beyond total: %+v %v", beyond, err)
	}
	st.err = errors.New("state unavailable")
	failed, err := s.LibraryItems(context.Background(), viewer, core.Device{}, "1", frontend.ItemQuery{Watched: &unwatched, Limit: 5})
	if !errors.Is(err, st.err) || len(failed.Items) != 0 {
		t.Fatalf("state failure must fail closed: %+v %v", failed, err)
	}
}

func TestLibraryBrowseBatchesFavoriteBeforePagination(t *testing.T) {
	viewer := &core.User{ID: 1}
	titles := []core.Title{{Key: core.MovieKey(1), Type: core.TypeMovie}, {Key: core.MovieKey(2), Type: core.TypeMovie}, {Key: core.MovieKey(3), Type: core.TypeMovie}}
	calls := 0
	s := &Service{d: ServiceDeps{Active: func(context.Context, int64) (*core.User, error) { return viewer, nil }, Libraries: []Library{{ID: 1, Type: core.TypeMovie}}, Visibility: Visibility{SectionFor: func(core.MediaType) (int, bool) { return 1, true }}, Browse: func(context.Context, core.BrowseQuery) (core.Page, error) {
		return core.Page{Items: titles, Total: len(titles)}, nil
	}, IsFavoriteMany: func(_ context.Context, _ *core.User, keys []core.RatingKey) (map[core.RatingKey]bool, error) {
		calls++
		return map[core.RatingKey]bool{titles[0].Key: true, titles[2].Key: true}, nil
	}}}
	favorite := true
	page, err := s.LibraryItems(context.Background(), viewer, core.Device{}, "1", frontend.ItemQuery{Favorite: &favorite, Offset: 1, Limit: 1})
	if err != nil || page.Total != 2 || len(page.Items) != 1 || page.Items[0].Key != titles[2].Key || calls != 1 {
		t.Fatalf("favorite page %+v calls=%d err=%v", page, calls, err)
	}
}
