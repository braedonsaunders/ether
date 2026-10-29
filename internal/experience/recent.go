package experience

import (
	"context"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

// RecentlyAdded loads the catalogue candidates for a library in added order.
func RecentlyAdded(ctx context.Context, viewer *core.User, kind core.MediaType, count int, browse func(context.Context, core.BrowseQuery) (core.Page, error)) ([]core.Title, bool, error) {
	q := core.BrowseQuery{Filter: core.Filter{Type: kind}, Sort: core.SortAdded, Desc: true, Limit: count + 1}
	if viewer != nil {
		q.Filter.MaxContentRating = viewer.MaxContentRating
	}
	page, err := browse(ctx, q)
	if err != nil {
		return nil, false, err
	}
	items := page.Items
	more := len(items) > count
	if more {
		items = items[:count]
	}
	return items, more, nil
}

// RecentlyViewed loads unique history entries in their most recent order.
func RecentlyViewed(ctx context.Context, viewer *core.User, libraryID int, history func(context.Context, int64, time.Time, int) ([]core.HistoryEntry, error), titles func(context.Context, []core.RatingKey) []core.Title, libraryFor func(core.MediaType) (int, bool)) ([]core.Title, error) {
	if viewer == nil || history == nil {
		return nil, nil
	}
	entries, err := history(ctx, viewer.ID, time.Time{}, 100)
	if err != nil {
		return nil, err
	}
	seen := map[core.RatingKey]bool{}
	var keys []core.RatingKey
	for _, entry := range entries {
		if !seen[entry.Key] {
			seen[entry.Key] = true
			keys = append(keys, entry.Key)
		}
	}
	var out []core.Title
	for _, title := range titles(ctx, keys) {
		if id, ok := libraryFor(title.Type); ok && id == libraryID {
			out = append(out, title)
		}
	}
	return out, nil
}
