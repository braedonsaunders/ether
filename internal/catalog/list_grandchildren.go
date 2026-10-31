package catalog

import (
	"context"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

// StoredGrandchildren returns, for each of shows it can answer from the
// mirror, exactly what Grandchildren would: the episodes of the show's
// listable seasons, in season and episode order. It answers a show only when
// Grandchildren would not wait on TMDB: the show has its details and every
// listable season is detailed with episodes stored. A stale season that
// Grandchildren would refresh behind the read is refreshed behind this read
// too. Shows it omits must go through Grandchildren. A list page counting
// many shows' episodes reads them in a few batched statements instead of a
// dozen per show.
func (c *Catalog) StoredGrandchildren(ctx context.Context, shows []core.RatingKey) (map[core.RatingKey][]core.Title, error) {
	out := map[core.RatingKey][]core.Title{}
	var need []core.RatingKey
	gen, minute := c.writeGen.Load(), c.minuteBucket()
	c.cacheMu.RLock()
	for _, k := range shows {
		if k.Kind() != core.KindShow {
			continue
		}
		if ent, ok := c.grandCache[k]; ok && ent.gen == gen && ent.minute == minute {
			items := make([]core.Title, len(ent.page.Items))
			for i := range ent.page.Items {
				items[i] = cloneTitle(ent.page.Items[i])
			}
			out[k] = items
			continue
		}
		need = append(need, k)
	}
	c.cacheMu.RUnlock()
	for start := 0; start < len(need); start += 200 {
		if err := c.storedGrandchildren(ctx, need[start:min(start+200, len(need))], gen, minute, out); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (c *Catalog) storedGrandchildren(ctx context.Context, shows []core.RatingKey, gen uint64, minute int64, out map[core.RatingKey][]core.Title) error {
	keys := make([]any, len(shows))
	for i, k := range shows {
		keys[i] = int64(k)
	}
	in := placeholders(len(shows))
	// Get and getShow wait on TMDB for a show without details.
	detailed := map[core.RatingKey]bool{}
	rows, err := c.db.QueryContext(ctx, `SELECT key, detail FROM catalog_titles WHERE type='show' AND key IN (`+in+`)`, keys...)
	if err != nil {
		return err
	}
	for rows.Next() {
		var key int64
		var detail int
		if err := rows.Scan(&key, &detail); err != nil {
			rows.Close()
			return err
		}
		detailed[core.RatingKey(key)] = detail != 0
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	// seasonsOfShow: hydrated season rows, then listableSeason.
	all, err := c.queryTitles(ctx, `SELECT `+titleCols+` FROM catalog_titles WHERE type='season' AND show_key IN (`+in+`) ORDER BY show_key ASC, season ASC`, keys...)
	if err != nil {
		return err
	}
	now := c.now()
	listed := map[core.RatingKey][]core.Title{}
	for i := range all {
		if listableSeason(&all[i], now) {
			listed[all[i].ShowKey] = append(listed[all[i].ShowKey], all[i])
		}
	}
	// loadSeasons: which listed seasons it would fetch.
	type mirrored struct{ episodes, pending int }
	have := map[core.RatingKey]map[int]mirrored{}
	rows, err = c.db.QueryContext(ctx, `SELECT show_key,season,COUNT(*),COUNT(CASE WHEN release_known=0 OR released_ts>? THEN 1 END)
		FROM catalog_titles WHERE type='episode' AND show_key IN (`+in+`) GROUP BY show_key,season`, append([]any{now.Unix()}, keys...)...)
	if err != nil {
		return err
	}
	for rows.Next() {
		var show int64
		var season int
		var m mirrored
		if err := rows.Scan(&show, &season, &m.episodes, &m.pending); err != nil {
			rows.Close()
			return err
		}
		k := core.RatingKey(show)
		if have[k] == nil {
			have[k] = map[int]mirrored{}
		}
		have[k][season] = m
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	type record struct {
		detailed, fresh bool
		episodes        int
	}
	records := map[core.RatingKey]map[int]record{}
	rows, err = c.db.QueryContext(ctx, `SELECT show_key,season,detail,detail_at,child_count FROM catalog_titles WHERE type='season' AND show_key IN (`+in+`)`, keys...)
	if err != nil {
		return err
	}
	for rows.Next() {
		var show int64
		var season, detail, child int
		var detailAt int64
		if err := rows.Scan(&show, &season, &detail, &detailAt, &child); err != nil {
			rows.Close()
			return err
		}
		st := storedTitle{detail: detail != 0}
		if detailAt != 0 {
			st.detailAt = time.Unix(detailAt, 0).UTC()
		}
		k := core.RatingKey(show)
		if records[k] == nil {
			records[k] = map[int]record{}
		}
		records[k][season] = record{detailed: st.detail && detailAt != 0, fresh: c.fresh(&st), episodes: child}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	answer := map[core.RatingKey]bool{}
	for _, show := range shows {
		if !detailed[show] {
			continue
		}
		seasons := listed[show]
		latest := 0
		for _, s := range seasons {
			latest = max(latest, s.Season)
		}
		blocking, stale := false, false
		for _, s := range seasons {
			m, ok := have[show][s.Season]
			r := records[show][s.Season]
			switch {
			case !ok || !r.detailed:
				// fetchSeason waits for a season with nothing mirrored.
				blocking = true
			case r.fresh:
			case s.Season != latest && m.pending == 0 && r.episodes <= m.episodes:
			default:
				stale = true
			}
		}
		if blocking {
			continue
		}
		answer[show] = true
		if stale {
			// Grandchildren would refresh it behind the read; so does this.
			_ = c.hydrate(ctx, "grandchildren:"+show.String(), 0, func(ctx context.Context) error {
				_, err := c.Grandchildren(ctx, show)
				return err
			})
		}
	}
	if len(answer) == 0 {
		return nil
	}
	ask := make([]any, 0, len(answer))
	for _, show := range shows {
		if answer[show] {
			ask = append(ask, int64(show))
		}
	}
	eps, err := c.queryTitles(ctx, `SELECT `+titleCols+` FROM catalog_titles WHERE type='episode' AND show_key IN (`+placeholders(len(ask))+`) ORDER BY show_key ASC, season ASC, episode ASC`, ask...)
	if err != nil {
		return err
	}
	inSeason := map[core.RatingKey]map[int]bool{}
	for show := range answer {
		inSeason[show] = map[int]bool{}
		for _, s := range listed[show] {
			inSeason[show][s.Season] = true
		}
		out[show] = []core.Title{}
	}
	for _, e := range eps {
		if inSeason[e.ShowKey][e.Season] {
			out[e.ShowKey] = append(out[e.ShowKey], e)
		}
	}
	if c.writeGen.Load() == gen {
		c.cacheMu.Lock()
		for show := range answer {
			stored := make([]core.Title, len(out[show]))
			for i := range out[show] {
				stored[i] = cloneTitle(out[show][i])
			}
			if len(c.grandCache) >= 256 {
				c.grandCache = map[core.RatingKey]cachedBrowse{}
			}
			c.grandCache[show] = cachedBrowse{page: core.Page{Items: stored, Total: len(stored)}, gen: gen, minute: minute}
		}
		c.cacheMu.Unlock()
	}
	return nil
}
