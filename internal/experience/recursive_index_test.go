package experience

import (
	"context"
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
)

// A recursive page's work is bounded by its requested window, independently
// of the number of series. Re-check parent policy without hydrating children.
func TestRecursiveIndexWindow(t *testing.T) {
	v := &core.User{ID: 1, IsOwner: true}
	show := core.Title{Key: core.ShowKey(9), Type: core.TypeShow, Title: "Series"}
	ep := core.Title{Key: core.EpisodeKey(9, 1, 1), ShowKey: show.Key, Type: core.TypeEpisode, Title: "Episode"}
	calls := 0
	var query core.BrowseQuery
	s := &Service{d: ServiceDeps{
		Active:     func(context.Context, int64) (*core.User, error) { return v, nil },
		Libraries:  []Library{{ID: 2, Type: core.TypeShow}},
		Visibility: Visibility{SectionFor: func(core.MediaType) (int, bool) { return 2, true }},
		Browse: func(_ context.Context, q core.BrowseQuery) (core.Page, error) {
			calls++
			query = q
			if q.Filter.Type != core.TypeEpisode {
				t.Fatalf("expanded series instead of querying episodes: %+v", q)
			}
			return core.Page{Items: []core.Title{ep}, Total: 100000}, nil
		},
		PageExclusions: func(context.Context, *core.User, core.MediaType) LibraryPageExclusions {
			return LibraryPageExclusions{}
		},
		ParentTitles: func(context.Context, []core.RatingKey) (map[core.RatingKey]core.Title, error) {
			return map[core.RatingKey]core.Title{show.Key: show}, nil
		},
		Grandchildren: func(context.Context, *core.User, core.RatingKey) ([]core.Title, error) {
			t.Fatal("must never hydrate seasons")
			return nil, nil
		},
		StoredEpisodes: StoredEpisodeDeps{
			Page: func(context.Context, core.Filter, core.RatingKey, int) ([]core.Title, error) {
				t.Fatal("must preserve SQL sort")
				return nil, nil
			},
			Count:      func(context.Context, core.Filter) (int, error) { return 0, nil },
			Unranked:   func(context.Context) (bool, error) { return true, nil },
			Customized: func(context.Context, *core.User) (bool, error) { return false, nil },
		},
	}}
	q := frontend.ItemQuery{Recursive: true, Types: []core.MediaType{core.TypeEpisode}, Sort: core.SortTitle, Offset: 2500, Limit: 26, NoTotal: true}
	page, err := s.LibraryItems(context.Background(), v, core.Device{}, "2", q)
	if err != nil || len(page.Items) != 1 || calls != 1 || query.Offset != 2500 || query.Limit != 26 || query.Sort != core.SortTitle || !query.NoTotal {
		t.Fatalf("calls=%d query=%+v page=%+v err=%v", calls, query, page, err)
	}
	// A parent that no longer passes current visibility must not leak a child
	// or trigger an unbounded fallback scan to fill the page.
	s.d.ParentTitles = func(context.Context, []core.RatingKey) (map[core.RatingKey]core.Title, error) {
		return map[core.RatingKey]core.Title{}, nil
	}
	page, err = s.LibraryItems(context.Background(), v, core.Device{}, "2", q)
	if err != nil || len(page.Items) != 0 || calls != 2 {
		t.Fatalf("policy race calls=%d page=%+v err=%v", calls, page, err)
	}
}
