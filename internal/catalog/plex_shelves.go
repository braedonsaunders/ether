package catalog

import (
	"context"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
)

func init() {
	store.Register("catalog_plex_shelf_order", 1, `CREATE INDEX catalog_plex_shelf_order ON catalog_titles(type,released_ts DESC,key ASC); CREATE INDEX catalog_plex_show_order ON catalog_titles(show_key,key);`)
}

// PlexRecentPage merges independently indexed movie and episode streams in SQL.
// Only the requested page is hydrated; totals use the catalogue's count cache.
func (c *Catalog) PlexRecentPage(ctx context.Context, q core.BrowseQuery, kinds []core.MediaType) (core.Page, error) {
	var branches []string
	var args []any
	total := 0
	for _, kind := range kinds {
		f := q.Filter
		f.Type = kind
		f = c.viewerPaging(ctx, f)
		counted, err := c.Browse(ctx, core.BrowseQuery{Filter: f, Sort: core.SortAdded, Desc: true, Limit: 1})
		if err != nil {
			return core.Page{}, err
		}
		total += counted.Total
		var where string
		var a []any
		if kind == core.TypeEpisode {
			where, a, err = c.storedEpisodesWhere(f)
			branches = append(branches, `SELECT e.key AS key,e.released_ts AS stamp FROM catalog_titles e CROSS JOIN catalog_titles t WHERE t.key=e.show_key AND e.type='episode' AND `+where)
		} else {
			where, a, err = c.browseWhere(f)
			branches = append(branches, `SELECT t.key AS key,t.released_ts AS stamp FROM catalog_titles t WHERE `+where)
		}
		if err != nil {
			return core.Page{}, err
		}
		args = append(args, a...)
	}
	if len(branches) == 0 {
		return core.Page{}, nil
	}
	limit := min(max(q.Limit, 1), 10000)
	if q.Limit == 0 {
		limit = 0
	}
	args = append(args, limit, max(q.Offset, 0))
	keys := `SELECT key,stamp FROM (` + strings.Join(branches, ` UNION ALL `) + `) ORDER BY stamp DESC,key ASC LIMIT ? OFFSET ?`
	rows, err := c.queryTitles(ctx, `SELECT `+prefixedCols("t")+` FROM (`+keys+`) page JOIN catalog_titles t ON t.key=page.key ORDER BY page.stamp DESC,t.key ASC`, args...)
	return core.Page{Items: rows, Total: total}, err
}

// PlexOnDeckPage starts with the viewer's indexed watch rows, chooses one
// candidate per show, applies catalogue policy, and pages before hydration.
func (c *Catalog) PlexOnDeckPage(ctx context.Context, uid int64, offset, limit int, f core.Filter, episodesOnly bool) (core.WatchPage, error) {
	if episodesOnly && f.Type == core.TypeMovie {
		return core.WatchPage{}, nil
	}
	movie := f
	movie.Type = core.TypeMovie
	movie = c.viewerPaging(ctx, movie)
	mw, ma, err := c.browseWhere(movie)
	if err != nil {
		return core.WatchPage{}, err
	}
	episode := f
	episode.Type = core.TypeEpisode
	episode = c.viewerPaging(ctx, episode)
	ew, ea, err := c.storedEpisodesWhere(episode)
	if err != nil {
		return core.WatchPage{}, err
	}
	const cte = `WITH active AS (
 SELECT w.*,CASE WHEN w.rating_key>=4000000000000000 AND w.rating_key<5000000000000000 THEN 2000000000000000+(w.rating_key-4000000000000000)/10000000 ELSE w.rating_key END AS entity
 FROM state_watch w WHERE w.user_id=? AND ((w.rating_key>=1000000000000000 AND w.rating_key<2000000000000000) OR (w.rating_key>=4000000000000000 AND w.rating_key<5000000000000000)) AND (w.view_offset_ns>0 OR w.view_count>0) AND /*activeVisible*/
), ranked AS (
 SELECT active.*,ROW_NUMBER() OVER(PARTITION BY entity ORDER BY CASE WHEN view_offset_ns>0 AND view_count=0 THEN 0 ELSE 1 END,last_viewed_at DESC,rating_key DESC) AS n
 FROM active
), chosen AS (
 SELECT CASE WHEN entity<2000000000000000 OR (view_offset_ns>0 AND view_count=0) THEN rating_key ELSE (
 SELECT e.key FROM catalog_titles e CROSS JOIN catalog_titles t WHERE t.key=e.show_key AND e.show_key=ranked.entity AND e.type='episode' AND e.season>0 AND e.episode>0 AND e.key>ranked.rating_key
 AND (e.released_ts=0 OR e.released_ts<=?)
 AND NOT EXISTS(SELECT 1 FROM state_watch watched WHERE watched.user_id=? AND watched.rating_key=e.key AND watched.view_count>0)
 AND /*nextVisible*/ ORDER BY e.key LIMIT 1) END AS key,last_viewed_at AS stamp,updated_at AS updated,entity
 FROM ranked WHERE n=1 AND (entity>=2000000000000000 OR view_offset_ns>0)
 AND NOT EXISTS(SELECT 1 FROM state_continue_hidden hidden WHERE hidden.user_id=? AND hidden.item_key=ranked.entity)
 AND NOT EXISTS(SELECT 1 FROM personalize_discovery_hidden hidden WHERE hidden.user_id=? AND hidden.item_key=ranked.entity)
), qualified AS (`
	branches := []string{}
	args := []any{uid}
	args = append(args, ma...)
	args = append(args, ea...)
	args = append(args, c.now().Unix(), uid)
	args = append(args, ea...)
	args = append(args, uid)
	personalArgIndex := len(args)
	args = append(args, uid)
	if !episodesOnly && f.Type != core.TypeEpisode {
		branches = append(branches, `SELECT chosen.key,chosen.stamp,chosen.updated FROM chosen CROSS JOIN catalog_titles t WHERE t.key=chosen.key AND `+mw)
		args = append(args, ma...)
	}
	if f.Type != core.TypeMovie {
		branches = append(branches, `SELECT chosen.key,chosen.stamp,chosen.updated FROM chosen CROSS JOIN catalog_titles e CROSS JOIN catalog_titles t WHERE e.key=chosen.key AND t.key=e.show_key AND e.type='episode' AND `+ew)
		args = append(args, ea...)
	}
	prefix := cte + strings.Join(branches, ` UNION ALL `) + `) `
	prefix = strings.Replace(prefix, "/*activeVisible*/", `(EXISTS(SELECT 1 FROM catalog_titles t WHERE t.key=w.rating_key AND `+mw+`) OR EXISTS(SELECT 1 FROM catalog_titles e CROSS JOIN catalog_titles t WHERE e.key=w.rating_key AND t.key=e.show_key AND `+ew+`))`, 1)
	prefix = strings.Replace(prefix, "/*nextVisible*/", ew, 1)
	var personal bool
	schemaQuery := `SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='table' AND name='personalize_discovery_hidden')`
	if store.DialectOf(c.db) == store.Postgres {
		schemaQuery = `SELECT to_regclass('personalize_discovery_hidden') IS NOT NULL`
	}
	if err := c.db.QueryRowContext(ctx, schemaQuery).Scan(&personal); err != nil {
		return core.WatchPage{}, err
	}
	if !personal {
		prefix = strings.Replace(prefix, ` AND NOT EXISTS(SELECT 1 FROM personalize_discovery_hidden hidden WHERE hidden.user_id=? AND hidden.item_key=ranked.entity)`, "", 1)
		args = append(args[:personalArgIndex], args[personalArgIndex+1:]...)
	}
	var total int
	if err := c.db.QueryRowContext(ctx, prefix+`SELECT COUNT(*) FROM qualified`, args...).Scan(&total); err != nil {
		return core.WatchPage{}, err
	}
	args = append(args, min(max(limit, 0), 10000), max(offset, 0))
	clocks, err := c.db.QueryContext(ctx, prefix+`SELECT key,stamp,updated FROM qualified ORDER BY stamp DESC,key ASC LIMIT ? OFFSET ?`, args...)
	if err != nil {
		return core.WatchPage{}, err
	}
	anchors := map[core.RatingKey]core.WatchStatus{}
	for clocks.Next() {
		var key core.RatingKey
		var stamp, updated int64
		if err := clocks.Scan(&key, &stamp, &updated); err != nil {
			clocks.Close()
			return core.WatchPage{}, err
		}
		anchor := core.WatchStatus{Key: key}
		if stamp > 0 {
			anchor.LastViewedAt = time.Unix(0, stamp)
		}
		if updated > 0 {
			anchor.UpdatedAt = time.Unix(0, updated)
		}
		anchors[key] = anchor
	}
	if err := clocks.Err(); err != nil {
		clocks.Close()
		return core.WatchPage{}, err
	}
	clocks.Close()
	rows, err := c.queryTitles(ctx, prefix+`SELECT `+prefixedCols("t")+` FROM (SELECT key,stamp FROM qualified ORDER BY stamp DESC,key ASC LIMIT ? OFFSET ?) page JOIN catalog_titles t ON t.key=page.key ORDER BY page.stamp DESC,t.key ASC`, args...)
	return core.WatchPage{Page: core.Page{Items: rows, Total: total}, Anchors: anchors}, err
}
