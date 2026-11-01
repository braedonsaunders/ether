package experience

import (
	"context"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

// Recent owns recent-list selection. Unrestricted is a trusted host policy
// capability; nil enforces inherited visibility and availability.
type Recent struct {
	Visibility   Visibility
	Unrestricted func(*core.User) bool
}

// RecentlyAdded loads the catalogue candidates for a library in added order.
func RecentlyAdded(ctx context.Context, viewer *core.User, kind core.MediaType, count int, visibility Visibility, browse func(context.Context, core.BrowseQuery) (core.Page, error)) ([]core.Title, bool, error) {
	return (Recent{Visibility: visibility}).Added(ctx, viewer, kind, count, browse)
}

func (r Recent) Added(ctx context.Context, viewer *core.User, kind core.MediaType, count int, browse func(context.Context, core.BrowseQuery) (core.Page, error)) ([]core.Title, bool, error) {
	if r.Unrestricted != nil && r.Unrestricted(viewer) {
		q := core.BrowseQuery{Filter: core.Filter{Type: kind, KnownAddedDate: true}, Sort: core.SortAdded, Desc: true, Limit: count + 1}
		if viewer != nil {
			q.Filter.MaxContentRating = viewer.MaxContentRating
		}
		page, err := browse(ctx, q)
		if err != nil {
			return nil, false, err
		}
		if len(page.Items) > count {
			return page.Items[:count], true, nil
		}
		return page.Items, false, nil
	}
	// Browse clamps a page to 500 entries; advance offsets rather than
	// requesting ever-larger prefixes. Bound total candidate work at 1000.
	fetch := min(max(count+1, 1), 500)
	offset := 0
	var items []core.Title
	for offset < 1000 {
		pageSize := min(fetch, 1000-offset)
		q := core.BrowseQuery{Filter: core.Filter{Type: kind, KnownAddedDate: true}, Sort: core.SortAdded, Desc: true, Offset: offset, Limit: pageSize}
		if viewer != nil {
			q.Filter.MaxContentRating = viewer.MaxContentRating
		}
		page, err := browse(ctx, q)
		if err != nil {
			return nil, false, err
		}
		for i := range page.Items {
			if r.Visibility.Visible(ctx, viewer, &page.Items[i]) {
				items = append(items, page.Items[i])
				if len(items) > count {
					return items[:count], true, nil
				}
			}
		}
		offset += len(page.Items)
		if len(page.Items) < pageSize || ctx.Err() != nil {
			return items, false, nil
		}
		fetch = min(fetch*2, 500)
	}
	return items, false, nil
}

// RecentlyViewed loads unique history entries in their most recent order.
func RecentlyViewed(ctx context.Context, viewer *core.User, libraryID int, visibility Visibility, history func(context.Context, int64, time.Time, int) ([]core.HistoryEntry, error), titles func(context.Context, []core.RatingKey) []core.Title, libraryFor func(core.MediaType) (int, bool)) ([]core.Title, error) {
	return (Recent{Visibility: visibility}).Viewed(ctx, viewer, libraryID, history, titles, libraryFor)
}

func (r Recent) Viewed(ctx context.Context, viewer *core.User, libraryID int, history func(context.Context, int64, time.Time, int) ([]core.HistoryEntry, error), titles func(context.Context, []core.RatingKey) []core.Title, libraryFor func(core.MediaType) (int, bool)) ([]core.Title, error) {
	if viewer == nil || history == nil {
		return nil, nil
	}
	unrestricted := r.Unrestricted != nil && r.Unrestricted(viewer)
	fetch := 100
	for {
		entries, err := history(ctx, viewer.ID, time.Time{}, fetch)
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
			if id, ok := libraryFor(title.Type); ok && id == libraryID && (unrestricted || r.Visibility.Visible(ctx, viewer, &title)) {
				out = append(out, title)
			}
		}
		if unrestricted {
			return out, nil
		}
		if len(out) >= 100 {
			return out[:100], nil
		}
		if len(entries) < fetch || fetch >= 1000 || ctx.Err() != nil {
			return out, nil
		}
		fetch = min(fetch*2, 1000)
	}
}
