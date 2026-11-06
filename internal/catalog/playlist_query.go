package catalog

import (
	"context"
	"fmt"
	"strings"

	"github.com/braedonsaunders/ether/internal/core"
)

// playlistSelectionSQL builds only identities and their source rank. The
// saved limit applies before parent deduplication and request filters; no
// source title is hydrated until its final page has been selected.
func (c *Catalog) playlistSelectionSQL(source core.BrowseQuery, cap int) (string, []any, error) {
	alias, from := "t", "catalog_titles t"
	var where string
	var args []any
	var err error
	switch source.Filter.Type {
	case core.TypeEpisode:
		alias = "e"
		from = "catalog_titles e JOIN catalog_titles t ON t.key=e.show_key"
		where, args, err = c.storedEpisodesWhere(source.Filter)
	case core.TypeSeason:
		alias = "s"
		from = "catalog_titles s JOIN catalog_titles t ON t.key=s.show_key"
		where, args, err = c.storedSeasonsWhere(source.Filter, false)
	default:
		where, args, err = c.browseWhere(source.Filter)
	}
	if err != nil {
		return "", nil, err
	}
	order := browseOrder(source)
	if alias != "t" {
		order = storedChildOrder(alias, source)
	}
	if source.SortLastViewed {
		dir := "ASC"
		if source.Desc {
			dir = "DESC"
		}
		order = fmt.Sprintf("CASE WHEN EXISTS(SELECT 1 FROM state_watch w WHERE w.user_id=%d AND w.rating_key=%s.key AND w.last_viewed_at>0) THEN 0 ELSE 1 END, (SELECT w.last_viewed_at FROM state_watch w WHERE w.user_id=%d AND w.rating_key=%s.key) %s, ", source.SortViewerID, alias, source.SortViewerID, alias, dir) + order
	}
	selectSQL := "SELECT " + alias + ".key, " + alias + ".show_key, " + alias + ".season, ROW_NUMBER() OVER(ORDER BY " + order + ", " + alias + ".key ASC) AS ordinal FROM " + from + " WHERE " + where + " ORDER BY " + order + ", " + alias + ".key ASC"
	if cap > 0 {
		selectSQL += " LIMIT ?"
		args = append(args, cap)
	}
	return selectSQL, args, nil
}

func (*Catalog) BrowsePlaylistInSQL() bool { return true }

func (c *Catalog) browsePlaylist(ctx context.Context, q core.BrowseQuery) (core.Page, error) {
	source := *q.PlaylistSource
	source.Filter = c.viewerPaging(ctx, source.Filter)
	sql, args, err := c.playlistSelectionSQL(source, q.PlaylistSourceLimit)
	if err != nil {
		return core.Page{}, err
	}
	projection := "key"
	if source.Filter.Type == core.TypeEpisode {
		switch q.Filter.Type {
		case core.TypeShow:
			projection = "show_key"
		case core.TypeSeason:
			projection = "CAST(" + fmt.Sprint(int64(core.KindSeason)*1_000_000_000_000_000) + " + (show_key % 1000000000000000)*10000 + season AS INTEGER)"
		}
	}
	alias, from := "t", "catalog_titles t"
	q.Filter = c.viewerPaging(ctx, q.Filter)
	var where string
	var vals []any
	switch q.Filter.Type {
	case core.TypeEpisode:
		alias = "e"
		from = "catalog_titles e JOIN catalog_titles t ON t.key=e.show_key"
		where, vals, err = c.storedEpisodesWhere(q.Filter)
	case core.TypeSeason:
		alias = "s"
		from = "catalog_titles s JOIN catalog_titles t ON t.key=s.show_key"
		where, vals, err = c.storedSeasonsWhere(q.Filter, false)
	default:
		where, vals, err = c.browseWhere(q.Filter)
	}
	if err != nil {
		return core.Page{}, err
	}
	prefix := "WITH members AS (" + sql + "), projected AS (SELECT " + projection + " AS selected_key, MIN(ordinal) AS ordinal FROM members GROUP BY " + projection + ") "
	join := " FROM projected p JOIN " + strings.Replace(from, "catalog_titles "+alias, "catalog_titles "+alias+" ON "+alias+".key=p.selected_key", 1) + " WHERE " + where
	args = append(args, vals...)
	var total int
	if err = c.db.QueryRowContext(ctx, prefix+"SELECT COUNT(*)"+join, args...).Scan(&total); err != nil {
		return core.Page{}, err
	}
	limit := q.Limit
	if limit <= 0 {
		limit = 50
	}
	if limit > 1000 {
		limit = 1000
	}
	items, err := c.queryPagedTitles(ctx, prefix+"SELECT "+prefixedCols(alias)+join+" ORDER BY p.ordinal LIMIT ? OFFSET ?", append(append([]any{}, args...), limit, max(q.Offset, 0))...)
	return core.Page{Items: items, Total: total}, err
}
