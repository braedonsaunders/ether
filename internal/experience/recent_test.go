package experience

import (
	"context"
	"github.com/braedonsaunders/ether/internal/core"
	"testing"
	"time"
)

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
