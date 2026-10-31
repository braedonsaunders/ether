package experience

import (
	"context"
	"strconv"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
)

// ColdHomeRows fills a neutral Home that has no ranked shelves left after policy
// filtering. Plex does not call it. Ordering follows date added: a movie or
// episode uses its own release or air date, and a series or season uses its
// latest aired episode. The page defaults to 50 and never exceeds 100.
func ColdHomeRows(ctx context.Context, viewer *core.User, libraries []Library, visibility Visibility, limit int, excluded []core.RatingKey, browse func(context.Context, core.BrowseQuery) (core.Page, error)) ([]frontend.Row, error) {
	if viewer == nil || browse == nil {
		return nil, core.ErrNotAvailable
	}
	if limit <= 0 {
		limit = 50
	}
	limit = min(limit, 100)
	filtered := func(ctx context.Context, q core.BrowseQuery) (core.Page, error) {
		q.Filter.ExcludeKeys = excluded
		return browse(ctx, q)
	}
	var rows []frontend.Row
	for _, library := range libraries {
		if !SectionAllowed(viewer, library.ID) {
			continue
		}
		items, more, err := RecentlyAdded(ctx, viewer, library.Type, limit, visibility, filtered)
		if err != nil {
			return nil, err
		}
		if len(items) == 0 {
			continue
		}
		row := frontend.Row{ID: "library.recent." + strconv.Itoa(library.ID), Kind: "recentlyAdded", Purpose: frontend.RowRecentlyAdded, Title: "Recently Added " + library.Name, More: more}
		for i := range items {
			item := items[i]
			row.Entries = append(row.Entries, frontend.RowEntry{Kind: frontend.RowEntryTitle, Title: &item})
		}
		rows = append(rows, row)
	}
	return rows, nil
}
