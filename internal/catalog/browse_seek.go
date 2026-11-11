package catalog

import (
	"context"
	"database/sql"
	"errors"

	"github.com/braedonsaunders/ether/internal/core"
)

// ErrSeekUnavailable reports that a seek's title is gone from the page's
// order (deleted, hidden or filtered out); the caller pages by offset.
var ErrSeekUnavailable = core.ErrSeekUnavailable

// seekableBrowse is core.BrowseSeekable.
func seekableBrowse(q core.BrowseQuery) bool { return core.BrowseSeekable(q) }

// seekColumn is the column Browse orders q by.
func seekColumn(q core.BrowseQuery) string {
	if q.Sort == core.SortAdded && q.Filter.Type == core.TypeShow {
		return "t.show_added_ts"
	}
	return sortColumn("t", q.Sort)
}

// seekPage reads the page that starts offset rows after AfterKey in the
// sort order, without visiting the rows before it: the rest of AfterKey's
// tie (same sort value, larger key) and then the rows past its value, each
// an index range of the sort column.
func (c *Catalog) seekPage(ctx context.Context, q core.BrowseQuery, where string, args []any, offset, limit int) ([]core.Title, error) {
	col := seekColumn(q)
	var value any
	err := c.db.QueryRowContext(ctx, `SELECT `+col+` FROM catalog_titles t WHERE t.key=? AND `+where, append([]any{int64(q.AfterKey)}, args...)...).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) || err == nil && value == nil {
		return nil, ErrSeekUnavailable
	}
	if err != nil {
		return nil, err
	}
	dir, past := "ASC", ">"
	if q.Desc {
		dir, past = "DESC", "<"
	}
	order := col + " " + dir + ", t.key ASC"
	if q.Sort == core.SortTitle {
		where = c.genreProbe(where, q.Filter)
		where = c.studioProbe(where, q.Filter)
	}
	want := offset + limit
	ranges := []seekRange{
		{" AND " + col + "=? AND t.key>?", []any{value, int64(q.AfterKey)}},
		{" AND " + col + past + "?", []any{value}},
	}
	if years, ok := pageYears(q.Filter); ok && q.Sort == core.SortTitle && walkableFilter(q.Filter) && want <= mergedPageMax {
		// A year range has no index in title order; each year's does.
		keys, err := c.seekYearKeys(ctx, q, where, args, years, ranges, offset, limit)
		if err != nil {
			return nil, err
		}
		items, err := c.titlesInKeyOrder(ctx, keys)
		if items == nil && err == nil {
			items = []core.Title{}
		}
		return items, err
	}
	var items []core.Title
	for _, r := range ranges {
		if len(items) >= want {
			break
		}
		page, err := c.queryPagedTitles(ctx, `SELECT `+prefixedCols("t")+` FROM catalog_titles t WHERE `+where+r.pred+` ORDER BY `+order+` LIMIT ?`, append(append(append([]any{}, args...), r.args...), want-len(items))...)
		if err != nil {
			return nil, err
		}
		items = append(items, page...)
	}
	if offset >= len(items) {
		return []core.Title{}, nil
	}
	items = items[offset:min(want, len(items))]
	if q.Sort == core.SortAdded {
		if err := applyAirAddedTimes(ctx, c.db, items, c.now()); err != nil {
			return nil, err
		}
	}
	return items, nil
}

type seekRange struct {
	pred string
	args []any
}

// seekYearKeys is seekPage's keys for a title-sorted page over a few
// years: each year's rows past AfterKey, from its (year, sort_title)
// index range, merged.
func (c *Catalog) seekYearKeys(ctx context.Context, q core.BrowseQuery, where string, args []any, years []int, ranges []seekRange, offset, limit int) ([]core.RatingKey, error) {
	dir := "ASC"
	if q.Desc {
		dir = "DESC"
	}
	want := offset + limit
	var all []sortedKey
	for _, y := range years {
		n := 0
		for _, r := range ranges {
			if n >= want {
				break
			}
			got, err := c.sortedKeys(ctx, `SELECT t.key, t.sort_title FROM catalog_titles t WHERE `+where+` AND t.year=?`+r.pred+` ORDER BY t.sort_title `+dir+`, t.key ASC LIMIT ?`, append(append(append(append([]any{}, args...), y), r.args...), want-n)...)
			if err != nil {
				return nil, err
			}
			n += len(got)
			all = append(all, got...)
		}
	}
	return mergedPage(all, q.Desc, offset, limit), nil
}

// CountWatched counts the titles matching f that userID has played (a watch
// row with a view), driven from the viewer's watch rows: its cost follows
// the viewer's history, not the library.
func (c *Catalog) CountWatched(ctx context.Context, f core.Filter, userID int64) (int, error) {
	f = c.viewerPaging(ctx, f)
	where, args, err := c.browseWhere(f)
	if err != nil {
		return 0, err
	}
	var n int
	err = c.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM state_watch w CROSS JOIN catalog_titles t WHERE w.user_id=? AND w.view_count>0 AND t.key=w.rating_key AND `+where, append([]any{userID}, args...)...).Scan(&n)
	return n, err
}
