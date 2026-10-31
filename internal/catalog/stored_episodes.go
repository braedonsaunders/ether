package catalog

import (
	"context"
	"fmt"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

// episodeKeyEnd is the first key past the episode key range.
var episodeKeyEnd = int64(core.KindEpisode+1) * 1_000_000_000_000_000

// storedEpisodesWhere selects stored, aired, non-adult episodes (e) whose
// series (t) passes the browse filter show.
func (c *Catalog) storedEpisodesWhere(show core.Filter) (string, []any, error) {
	show.Type = core.TypeShow
	where, args, err := c.browseWhere(show)
	if err != nil {
		return "", nil, err
	}
	return `e.type='episode' AND e.adult=0 AND e.release_known=1 AND e.released_ts<=? AND ` + where, append([]any{c.now().Unix()}, args...), nil
}

// StoredEpisodesByKey returns up to limit stored, aired, non-adult episodes
// after the key after, in key order (series, season, episode), of the
// series the browse filter show admits. It reads the key range and probes
// each episode's series, so a page costs the same however large the
// library; it refreshes nothing.
func (c *Catalog) StoredEpisodesByKey(ctx context.Context, show core.Filter, after core.RatingKey, limit int) ([]core.Title, error) {
	where, args, err := c.storedEpisodesWhere(show)
	if err != nil {
		return nil, err
	}
	lo := max(int64(after), int64(core.KindEpisode)*1_000_000_000_000_000-1)
	q := `SELECT ` + prefixedCols("e") + ` FROM catalog_titles e JOIN catalog_titles t ON t.key=e.show_key WHERE e.key>? AND e.key<? AND ` + where + ` ORDER BY e.key LIMIT ?`
	return c.queryTitles(ctx, q, append(append([]any{lo, episodeKeyEnd}, args...), limit)...)
}

// CountStoredEpisodes counts StoredEpisodesByKey's whole set. Like a browse
// total, a large count is served from the last one while it recounts.
func (c *Catalog) CountStoredEpisodes(ctx context.Context, show core.Filter) (int, error) {
	where, args, err := c.storedEpisodesWhere(show)
	if err != nil {
		return 0, err
	}
	ckey := fmt.Sprintf("stored-episodes:%+v", show)
	gen, minute := c.writeGen.Load(), c.minuteBucket()
	c.cacheMu.RLock()
	ent, ok := c.countCache[ckey]
	c.cacheMu.RUnlock()
	count := func(ctx context.Context) (int, error) {
		var total int
		if err := c.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM catalog_titles e JOIN catalog_titles t ON t.key=e.show_key WHERE e.key>? AND e.key<? AND `+where,
			append([]any{int64(core.KindEpisode)*1_000_000_000_000_000 - 1, episodeKeyEnd}, args...)...).Scan(&total); err != nil {
			return 0, err
		}
		c.cacheMu.Lock()
		if len(c.countCache) >= 1024 {
			c.countCache = map[string]cachedCount{}
		}
		c.countCache[ckey] = cachedCount{total: total, gen: gen, minute: minute, counted: time.Now()}
		c.cacheMu.Unlock()
		return total, nil
	}
	switch {
	case ok && ent.gen == gen && ent.minute == minute:
		return ent.total, nil
	case ok && ent.total >= staleCountMin:
		if time.Since(ent.counted) >= recountEvery {
			_ = c.hydrate(ctx, "count:"+ckey, 0, func(ctx context.Context) error {
				_, err := count(ctx)
				return err
			})
		}
		return ent.total, nil
	}
	return count(ctx)
}

// EpisodesUnranked reports that no stored episode has a popularity, so a
// popularity order of episodes is their key order. TMDB gives episodes no
// popularity; this checks the index's two ends rather than assuming it.
func (c *Catalog) EpisodesUnranked(ctx context.Context) (bool, error) {
	// Separate MIN and MAX subqueries, so each is one index seek.
	var ranked bool
	err := c.db.QueryRowContext(ctx, `SELECT COALESCE((SELECT MIN(popularity) FROM catalog_titles WHERE type='episode' AND browsable=0),0)<>0
		OR COALESCE((SELECT MAX(popularity) FROM catalog_titles WHERE type='episode' AND browsable=0),0)<>0
		OR COALESCE((SELECT MIN(popularity) FROM catalog_titles WHERE type='episode' AND browsable=1),0)<>0
		OR COALESCE((SELECT MAX(popularity) FROM catalog_titles WHERE type='episode' AND browsable=1),0)<>0`).Scan(&ranked)
	return !ranked, err
}
