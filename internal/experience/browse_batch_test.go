package experience

import (
	"context"
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
)

type countingBrowseState struct {
	serviceState
	gets, batches int
}

func (s *countingBrowseState) Get(ctx context.Context, id int64, key core.RatingKey) (core.WatchStatus, error) {
	s.gets++
	return s.serviceState.Get(ctx, id, key)
}
func (s *countingBrowseState) GetMany(_ context.Context, _ int64, keys []core.RatingKey) (map[core.RatingKey]core.WatchStatus, error) {
	s.batches++
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
}
