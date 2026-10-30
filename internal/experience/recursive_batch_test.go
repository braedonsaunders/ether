package experience

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
)

func TestRecursiveBrowseBatchesOnlyVisibleEpisodes(t *testing.T) {
	ctx := context.Background()
	viewer := &core.User{ID: 7}
	active := true
	missingParent := false
	show := core.Title{Key: core.ShowKey(1), Type: core.TypeShow}
	hidden := core.Title{Key: core.ShowKey(2), Type: core.TypeShow, Adult: true}
	var episodes []core.Title
	for i := 1; i <= 803; i++ {
		episodes = append(episodes, core.Title{Key: core.EpisodeKey(1, 1, i), ShowKey: show.Key, Type: core.TypeEpisode, Adult: i == 4})
	}
	st := &countingBrowseState{}
	expandedHidden := 0
	favoriteBatches := 0
	favoriteErr := error(nil)
	svc := &Service{d: ServiceDeps{Active: func(context.Context, int64) (*core.User, error) {
		if !active {
			return nil, core.ErrNotFound
		}
		return viewer, nil
	}, Libraries: []Library{{ID: 2, Type: core.TypeShow}}, State: st, Visibility: Visibility{SectionFor: func(core.MediaType) (int, bool) { return 2, true }, GetTitle: func(_ context.Context, k core.RatingKey) (*core.Title, error) {
		if missingParent {
			return nil, core.ErrNotFound
		}
		if k == show.Key {
			return &show, nil
		}
		return nil, core.ErrNotFound
	}}, Browse: func(context.Context, core.BrowseQuery) (core.Page, error) {
		return core.Page{Items: []core.Title{show, hidden}, Total: 2}, nil
	}, Grandchildren: func(_ context.Context, _ *core.User, k core.RatingKey) ([]core.Title, error) {
		if k == hidden.Key {
			expandedHidden++
		}
		return episodes, nil
	}, IsFavoriteMany: func(_ context.Context, _ *core.User, keys []core.RatingKey) (map[core.RatingKey]bool, error) {
		favoriteBatches++
		if len(keys) > 400 {
			t.Fatalf("unbounded favorites batch %d", len(keys))
		}
		if favoriteErr != nil {
			return nil, favoriteErr
		}
		out := make(map[core.RatingKey]bool, len(keys))
		for _, k := range keys {
			out[k] = true
		}
		return out, nil
	}}}
	watched, favorite := true, true
	q := frontend.ItemQuery{Recursive: true, Types: []core.MediaType{core.TypeEpisode}, Watched: &watched, Favorite: &favorite, Offset: 2, Limit: 3}
	page, err := svc.LibraryItems(ctx, viewer, core.Device{}, "2", q)
	if err != nil || page.Total != 400 || len(page.Items) != 3 || page.Items[0].Key != core.EpisodeKey(1, 1, 8) || page.Items[1].Key != core.EpisodeKey(1, 1, 10) || page.Items[2].Key != core.EpisodeKey(1, 1, 12) {
		t.Fatalf("page %+v: %v", page, err)
	}
	if st.gets != 0 || st.batches != 3 || favoriteBatches != 3 || expandedHidden != 0 {
		t.Fatalf("calls get=%d state batches=%d favorite batches=%d hidden expansion=%d", st.gets, st.batches, favoriteBatches, expandedHidden)
	}
	for _, k := range st.keys {
		if k == core.EpisodeKey(1, 1, 4) || k == show.Key {
			t.Fatalf("hidden or wrong type key in state batch: %s", k)
		}
	}
	// The optional batch capability may be absent; the established per-item path remains.
	svc.d.IsFavoriteMany = nil
	fallback := 0
	svc.d.IsFavorite = func(context.Context, *core.User, core.RatingKey) (bool, error) { fallback++; return true, nil }
	fallbackPage, err := svc.LibraryItems(ctx, viewer, core.Device{}, "2", q)
	if err != nil || fallbackPage.Total != page.Total || !reflect.DeepEqual(fallbackPage.Items, page.Items) || fallback != 400 {
		t.Fatalf("fallback %+v calls=%d err=%v", fallbackPage, fallback, err)
	}
	// Missing watch and favorite map entries represent false, not an error.
	unwatched, notFavorite := false, false
	falseQuery := q
	falseQuery.Watched, falseQuery.Favorite = &unwatched, &notFavorite
	svc.d.IsFavoriteMany = func(context.Context, *core.User, []core.RatingKey) (map[core.RatingKey]bool, error) {
		return map[core.RatingKey]bool{}, nil
	}
	falsePage, err := svc.LibraryItems(ctx, viewer, core.Device{}, "2", falseQuery)
	if err != nil || falsePage.Total != 402 || len(falsePage.Items) != 3 || falsePage.Items[0].Key != core.EpisodeKey(1, 1, 5) {
		t.Fatalf("false predicates: %+v %v", falsePage, err)
	}
	// Nil predicates must not read state or favorites.
	beforeBatches, beforeGets := st.batches, st.gets
	st.err = errors.New("unexpected state access")
	svc.d.IsFavoriteMany = func(context.Context, *core.User, []core.RatingKey) (map[core.RatingKey]bool, error) {
		t.Error("unexpected favorite access")
		return nil, errors.New("unexpected")
	}
	plainQuery := q
	plainQuery.Watched, plainQuery.Favorite = nil, nil
	plain, err := svc.LibraryItems(ctx, viewer, core.Device{}, "2", plainQuery)
	if err != nil || plain.Total != 802 || st.batches != beforeBatches || st.gets != beforeGets {
		t.Fatalf("nil predicates: %+v %v", plain, err)
	}
	st.err = nil
	failure := errors.New("state failed")
	st.err = failure
	if got, err := svc.LibraryItems(ctx, viewer, core.Device{}, "2", q); !errors.Is(err, failure) || len(got.Items) != 0 {
		t.Fatalf("state failure: %+v %v", got, err)
	}
	st.err = nil
	favoriteErr = errors.New("favorite failed")
	svc.d.IsFavoriteMany = func(context.Context, *core.User, []core.RatingKey) (map[core.RatingKey]bool, error) {
		return nil, favoriteErr
	}
	if got, err := svc.LibraryItems(ctx, viewer, core.Device{}, "2", q); !errors.Is(err, favoriteErr) || len(got.Items) != 0 {
		t.Fatalf("favorite failure: %+v %v", got, err)
	}
	favoriteErr = nil
	missingParent = true
	if got, err := svc.LibraryItems(ctx, viewer, core.Device{}, "2", q); err != nil || got.Total != 0 {
		t.Fatalf("missing parent: %+v %v", got, err)
	}
	active = false
	if _, err := svc.LibraryItems(ctx, viewer, core.Device{}, "2", q); err == nil {
		t.Fatal("revoked viewer accepted")
	}
}

func TestRecursiveBrowseCachedParentsFailClosed(t *testing.T) {
	ctx := context.Background()
	viewer := &core.User{ID: 7, MaxContentRating: "PG"}
	active := true
	show := core.Title{Key: core.ShowKey(10), Type: core.TypeShow, ContentRating: "TV-G"}
	denied := core.Title{Key: core.ShowKey(11), Type: core.TypeShow, ContentRating: "TV-MA"}
	children := []core.Title{{Key: core.EpisodeKey(10, 1, 1), Type: core.TypeEpisode, ShowKey: show.Key, ContentRating: "TV-G"}, {Key: core.EpisodeKey(11, 1, 1), Type: core.TypeEpisode, ShowKey: denied.Key, ContentRating: "TV-G"}, {Key: core.EpisodeKey(12, 1, 1), Type: core.TypeEpisode, ShowKey: core.ShowKey(12), ContentRating: "TV-G"}}
	batchCalls, individualCalls := 0, 0
	fail := error(nil)
	svc := &Service{d: ServiceDeps{Active: func(context.Context, int64) (*core.User, error) {
		if !active {
			return nil, core.ErrNotFound
		}
		return viewer, nil
	}, Libraries: []Library{{ID: 2, Type: core.TypeShow}}, Visibility: Visibility{SectionFor: func(core.MediaType) (int, bool) { return 2, true }, GetTitle: func(context.Context, core.RatingKey) (*core.Title, error) {
		individualCalls++
		return nil, core.ErrNotFound
	}}, CachedParents: func(_ context.Context, keys []core.RatingKey) (map[core.RatingKey]core.Title, error) {
		batchCalls++
		if len(keys) > 400 {
			t.Fatal("unbounded parent batch")
		}
		if fail != nil {
			return nil, fail
		}
		return map[core.RatingKey]core.Title{show.Key: show, denied.Key: denied}, nil
	}, Browse: func(context.Context, core.BrowseQuery) (core.Page, error) {
		return core.Page{Items: []core.Title{show}, Total: 1}, nil
	}, Grandchildren: func(context.Context, *core.User, core.RatingKey) ([]core.Title, error) { return children, nil }}}
	q := frontend.ItemQuery{Recursive: true, Types: []core.MediaType{core.TypeEpisode}, Limit: 20}
	page, err := svc.LibraryItems(ctx, viewer, core.Device{}, "2", q)
	if err != nil || page.Total != 1 || len(page.Items) != 1 || page.Items[0].Key != children[0].Key || batchCalls != 1 || individualCalls != 0 {
		t.Fatalf("page=%+v err=%v batches=%d individual=%d", page, err, batchCalls, individualCalls)
	}
	fail = errors.New("cache read failed")
	if got, err := svc.LibraryItems(ctx, viewer, core.Device{}, "2", q); !errors.Is(err, fail) || len(got.Items) != 0 {
		t.Fatalf("cache failure %+v %v", got, err)
	}
	active = false
	if _, err := svc.LibraryItems(ctx, viewer, core.Device{}, "2", q); err == nil {
		t.Fatal("revoked viewer accepted")
	}
}
