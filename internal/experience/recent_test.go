package experience

import (
	"context"
	"errors"
	"github.com/braedonsaunders/ether/internal/core"
	"testing"
	"time"
)

func TestRecentTrustedSourceErrors(t *testing.T) {
	failure := errors.New("source failed")
	r := Recent{Unrestricted: func(*core.User) bool { return true }}
	if _, _, err := r.Added(context.Background(), &core.User{}, core.TypeMovie, 2, func(context.Context, core.BrowseQuery) (core.Page, error) { return core.Page{}, failure }); !errors.Is(err, failure) {
		t.Fatalf("browse error %v", err)
	}
	if _, err := r.Viewed(context.Background(), &core.User{}, 1, func(context.Context, int64, time.Time, int) ([]core.HistoryEntry, error) { return nil, failure }, func(context.Context, []core.RatingKey) []core.Title {
		t.Fatal("titles loaded after history error")
		return nil
	}, func(core.MediaType) (int, bool) { return 0, false }); !errors.Is(err, failure) {
		t.Fatalf("history error %v", err)
	}
}

func TestRecentlyAddedQueryAndMore(t *testing.T) {
	viewer := &core.User{MaxContentRating: "PG"}
	items, more, err := RecentlyAdded(context.Background(), viewer, core.TypeMovie, 1, Visibility{}, func(_ context.Context, q core.BrowseQuery) (core.Page, error) {
		if q.Filter.Type != core.TypeMovie || q.Filter.MaxContentRating != "PG" || q.Limit != 2 || !q.Desc || q.Sort != core.SortAdded {
			t.Fatalf("query: %+v", q)
		}
		return core.Page{Items: []core.Title{{Key: 1, ContentRating: "PG"}, {Key: 2, ContentRating: "PG"}}}, nil
	})
	if err != nil || !more || len(items) != 1 || items[0].Key != 1 {
		t.Fatalf("items %+v more %v err %v", items, more, err)
	}
}
func TestRecentlyViewedDedupeAndSection(t *testing.T) {
	items, err := RecentlyViewed(context.Background(), &core.User{ID: 7}, 1, Visibility{}, func(_ context.Context, id int64, _ time.Time, limit int) ([]core.HistoryEntry, error) {
		if id != 7 || limit != 100 {
			t.Fatal("wrong history query")
		}
		return []core.HistoryEntry{{Key: 1}, {Key: 1}, {Key: 2}}, nil
	}, func(_ context.Context, keys []core.RatingKey) []core.Title {
		if len(keys) != 2 || keys[0] != 1 || keys[1] != 2 {
			t.Fatalf("keys %+v", keys)
		}
		return []core.Title{{Key: 1, Type: core.TypeMovie}, {Key: 2, Type: core.TypeShow}}
	}, func(kind core.MediaType) (int, bool) {
		if kind == core.TypeMovie {
			return 1, true
		}
		return 2, true
	})
	if err != nil || len(items) != 1 || items[0].Key != 1 {
		t.Fatalf("items %+v err %v", items, err)
	}
}

func TestRecentlyAddedBackfillsHidden(t *testing.T) {
	all := []core.Title{{Key: 1, Adult: true}, {Key: 2, Adult: true}, {Key: 3}}
	calls := 0
	got, more, err := RecentlyAdded(context.Background(), &core.User{}, core.TypeMovie, 1, Visibility{}, func(_ context.Context, q core.BrowseQuery) (core.Page, error) {
		calls++
		start := min(q.Offset, len(all))
		return core.Page{Items: all[start:min(start+q.Limit, len(all))]}, nil
	})
	if err != nil || more || len(got) != 1 || got[0].Key != 3 || calls != 2 {
		t.Fatalf("got %+v more %v err %v calls %d", got, more, err, calls)
	}
}

func TestRecentlyAddedPagesPastCatalogueCap(t *testing.T) {
	all := make([]core.Title, 502)
	for i := range all {
		all[i] = core.Title{Key: core.RatingKey(i + 1), Adult: i < 500}
	}
	var offsets []int
	got, more, err := RecentlyAdded(context.Background(), &core.User{}, core.TypeMovie, 1, Visibility{}, func(_ context.Context, q core.BrowseQuery) (core.Page, error) {
		if q.Limit > 500 {
			t.Fatalf("page exceeds catalogue cap: %d", q.Limit)
		}
		offsets = append(offsets, q.Offset)
		start := min(q.Offset, len(all))
		return core.Page{Items: all[start:min(start+q.Limit, len(all))], Total: len(all)}, nil
	})
	if err != nil || !more || len(got) != 1 || got[0].Key != 501 {
		t.Fatalf("got %+v more %v err %v", got, more, err)
	}
	if len(offsets) < 2 || offsets[len(offsets)-1] == 0 {
		t.Fatalf("missing offset pagination: %v", offsets)
	}
}

func TestRecentTrustedLegacySelection(t *testing.T) {
	ctx := context.Background()
	viewer := &core.User{ID: 9, MaxContentRating: "PG"}
	hidden := core.Title{Key: core.MovieKey(1), Type: core.TypeMovie, Adult: true}
	other := core.Title{Key: core.ShowKey(2), Type: core.TypeShow}
	r := Recent{Unrestricted: func(*core.User) bool { return true }, Visibility: Visibility{FilterAvailable: func(context.Context, []core.Title) []core.Title {
		t.Fatal("unexpected availability lookup")
		return nil
	}}}
	calls := 0
	items, more, err := r.Added(ctx, viewer, core.TypeMovie, 1, func(_ context.Context, q core.BrowseQuery) (core.Page, error) {
		calls++
		if q.Limit != 2 || q.Filter.MaxContentRating != "PG" || q.Sort != core.SortAdded || !q.Desc {
			t.Fatalf("query %+v", q)
		}
		return core.Page{Items: []core.Title{hidden, other}}, nil
	})
	if err != nil || calls != 1 || !more || len(items) != 1 || items[0].Key != hidden.Key {
		t.Fatalf("added=%+v more=%v calls=%d err=%v", items, more, calls, err)
	}
	calls = 0
	items, err = r.Viewed(ctx, viewer, 1, func(_ context.Context, id int64, _ time.Time, limit int) ([]core.HistoryEntry, error) {
		calls++
		if id != viewer.ID || limit != 100 {
			t.Fatal("history query")
		}
		return []core.HistoryEntry{{Key: hidden.Key}, {Key: hidden.Key}, {Key: other.Key}}, nil
	}, func(_ context.Context, keys []core.RatingKey) []core.Title {
		if len(keys) != 2 {
			t.Fatalf("dedupe %v", keys)
		}
		return []core.Title{hidden, other}
	}, func(kind core.MediaType) (int, bool) {
		if kind == core.TypeMovie {
			return 1, true
		}
		return 2, true
	})
	if err != nil || calls != 1 || len(items) != 1 || items[0].Key != hidden.Key {
		t.Fatalf("viewed=%+v calls=%d err=%v", items, calls, err)
	}
}
