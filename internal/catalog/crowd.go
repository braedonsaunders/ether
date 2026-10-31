package catalog

import (
	"context"
	"database/sql"
	"errors"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
	"github.com/braedonsaunders/ether/internal/tmdb"
)

// Crowd co-engagement lists. TMDB's /recommendations for a title is what
// people who engaged with it also engaged with: public collaborative
// filtering over millions of users. Personalisation aggregates these lists
// over a viewer's favourites (internal/personalize, crowd.go), so they are
// persisted here and reused for days: they change slowly, and a viewer's
// favourites recur from build to build. Lists also arrive for free with
// every title detail fetch (append_to_response), which is harvested.

func init() {
	store.Register("catalog", 4, `CREATE TABLE catalog_crowd (
  key INTEGER PRIMARY KEY,
  fetched_at INTEGER NOT NULL,
  pages INTEGER NOT NULL,
  recs TEXT NOT NULL
);`)
}

// DefaultCrowdTTL is how long a stored co-engagement list is fresh.
const DefaultCrowdTTL = 72 * time.Hour

// maxCrowdPages bounds the pages fetched per title (20 results each).
const maxCrowdPages = 2

// WithCrowdTTL overrides how long co-engagement lists stay fresh.
func WithCrowdTTL(d time.Duration) Option { return func(c *Catalog) { c.crowdTTL = d } }

// crowdKey maps seasons and episodes onto their show; films and shows map
// to themselves. Other kinds have no list.
func crowdKey(key core.RatingKey) (core.RatingKey, bool) {
	switch key.Kind() {
	case core.KindMovie, core.KindShow:
		return key, true
	case core.KindSeason, core.KindEpisode:
		return core.ShowKey(key.TMDB()), true
	}
	return 0, false
}

// CrowdRecommendations returns TMDB's crowd co-engagement list for a title
// (/recommendations only: never metadata similarity), best first, from up
// to pages pages (1–2). A stored list younger than the crowd TTL with
// enough pages is served from the local mirror. Otherwise, with fetch set,
// TMDB is asked and fetched reports it (callers budget those calls);
// without fetch, or when TMDB fails, a stale list is served when there is
// one and nil otherwise. Titles not yet released are dropped.
func (c *Catalog) CrowdRecommendations(ctx context.Context, key core.RatingKey, pages int, fetch bool) (titles []core.Title, fetched bool, err error) {
	ck, ok := crowdKey(key)
	if !ok {
		return nil, false, core.ErrNotFound
	}
	pages = max(1, min(pages, maxCrowdPages))
	keys, at, have, err := c.loadCrowd(ctx, ck, pages)
	if err != nil {
		return nil, false, err
	}
	if have && c.now().Sub(at) < c.crowdTTLOrDefault() {
		out, err := c.titlesByKeys(ctx, keys)
		return out, false, err
	}
	if !fetch || c.tmdb() == nil {
		if keys == nil {
			return nil, false, nil
		}
		out, err := c.titlesByKeys(ctx, keys)
		return out, false, err
	}
	fresh, ferr := c.fetchCrowd(ctx, ck, pages)
	if ferr != nil {
		if keys != nil {
			out, err := c.titlesByKeys(ctx, keys)
			return out, true, err
		}
		return nil, true, ferr
	}
	if err := c.storeCrowd(ctx, ck, fresh, pages); err != nil {
		return nil, true, err
	}
	out, err := c.titlesByKeys(ctx, keyList(fresh))
	return out, true, err
}

func (c *Catalog) crowdTTLOrDefault() time.Duration {
	if c.crowdTTL > 0 {
		return c.crowdTTL
	}
	return DefaultCrowdTTL
}

// loadCrowd reads a stored list. have reports a list with at least pages
// pages; keys is any stored list (for stale serving).
func (c *Catalog) loadCrowd(ctx context.Context, key core.RatingKey, pages int) (keys []core.RatingKey, at time.Time, have bool, err error) {
	var fetchedAt int64
	var stored int
	var recs string
	err = c.db.QueryRowContext(ctx, `SELECT fetched_at, pages, recs FROM catalog_crowd WHERE key=?`, int64(key)).Scan(&fetchedAt, &stored, &recs)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, time.Time{}, false, nil
	}
	if err != nil {
		return nil, time.Time{}, false, err
	}
	keys = []core.RatingKey{}
	for _, s := range strings.Split(recs, ",") {
		if n, perr := strconv.ParseInt(s, 10, 64); perr == nil && n != 0 {
			keys = append(keys, core.RatingKey(n))
		}
	}
	return keys, time.Unix(fetchedAt, 0), stored >= pages, nil
}

// fetchCrowd asks TMDB for up to pages pages of recommendations.
func (c *Catalog) fetchCrowd(ctx context.Context, key core.RatingKey, pages int) ([]core.Title, error) {
	id := key.TMDB()
	var out []core.Title
	for p := 1; p <= pages; p++ {
		total := 0
		switch key.Kind() {
		case core.KindMovie:
			res, err := c.tmdb().MovieRecommendations(ctx, id, p)
			if err != nil {
				if p > 1 {
					return out, nil // page 1 is the list that matters
				}
				return nil, err
			}
			for _, m := range res.Results {
				out = append(out, c.movieToTitle(ctx, m))
			}
			total = res.TotalPages
		default:
			res, err := c.tmdb().TVRecommendations(ctx, id, p)
			if err != nil {
				if p > 1 {
					return out, nil
				}
				return nil, err
			}
			for _, s := range res.Results {
				out = append(out, c.tvToTitle(ctx, s))
			}
			total = res.TotalPages
		}
		if p >= total {
			break
		}
	}
	return out, nil
}

// storeCrowd mirrors a fetched list's titles and stores the list.
func (c *Catalog) storeCrowd(ctx context.Context, key core.RatingKey, titles []core.Title, pages int) error {
	seen := map[core.RatingKey]bool{key: true}
	kept := titles[:0:0]
	for _, t := range titles {
		if t.Key == 0 || t.Adult || seen[t.Key] {
			continue
		}
		seen[t.Key] = true
		kept = append(kept, t)
	}
	var browsable, mirrorOnly []core.Title
	for _, t := range kept {
		if c.meetsThreshold(t) {
			browsable = append(browsable, t)
		} else {
			mirrorOnly = append(mirrorOnly, t)
		}
	}
	if err := c.upsertMany(ctx, browsable, upsertOpts{browsable: true}); err != nil {
		return err
	}
	if err := c.upsertMany(ctx, mirrorOnly, upsertOpts{}); err != nil {
		return err
	}
	parts := make([]string, len(kept))
	for i, t := range kept {
		parts[i] = strconv.FormatInt(int64(t.Key), 10)
	}
	_, err := c.db.ExecContext(ctx, `INSERT INTO catalog_crowd(key, fetched_at, pages, recs) VALUES(?,?,?,?)
		ON CONFLICT(key) DO UPDATE SET fetched_at=excluded.fetched_at, pages=excluded.pages, recs=excluded.recs`,
		int64(key), c.now().Unix(), pages, strings.Join(parts, ","))
	return err
}

// harvestCrowd keeps the first page of recommendations a detail fetch
// carried, unless a fresh list with more pages is already stored. Errors
// are ignored: this is a free bonus, never a reason for Get to fail.
func (c *Catalog) harvestCrowd(ctx context.Context, key core.RatingKey, titles []core.Title) {
	if _, at, have, err := c.loadCrowd(ctx, key, maxCrowdPages); err != nil || have && c.now().Sub(at) < c.crowdTTLOrDefault() {
		return
	}
	if err := c.storeCrowd(ctx, key, titles, 1); err != nil {
		c.log.Debug("catalog: storing harvested recommendations failed", "key", key, "err", err)
	}
}

func (c *Catalog) harvestMovieCrowd(ctx context.Context, key core.RatingKey, p *tmdb.Paged[tmdb.Movie]) {
	if p == nil {
		return
	}
	titles := make([]core.Title, 0, len(p.Results))
	for _, m := range p.Results {
		titles = append(titles, c.movieToTitle(ctx, m))
	}
	c.harvestCrowd(ctx, key, titles)
}

func (c *Catalog) harvestTVCrowd(ctx context.Context, key core.RatingKey, p *tmdb.Paged[tmdb.TVShow]) {
	if p == nil {
		return
	}
	titles := make([]core.Title, 0, len(p.Results))
	for _, s := range p.Results {
		titles = append(titles, c.tvToTitle(ctx, s))
	}
	c.harvestCrowd(ctx, key, titles)
}

func keyList(titles []core.Title) []core.RatingKey {
	out := make([]core.RatingKey, 0, len(titles))
	seen := map[core.RatingKey]bool{}
	for _, t := range titles {
		if t.Key != 0 && !t.Adult && !seen[t.Key] {
			seen[t.Key] = true
			out = append(out, t.Key)
		}
	}
	return out
}

// titlesByKeys loads mirrored titles in the given order (released, not
// adult), in batches, consulting the in-memory movie cache first.
func (c *Catalog) titlesByKeys(ctx context.Context, keys []core.RatingKey) ([]core.Title, error) {
	if err := c.admitFallbacks(ctx); err != nil {
		return nil, err
	}
	out := make([]core.Title, 0, len(keys))
	if len(keys) == 0 {
		return out, nil
	}
	gen := c.writeGen.Load()
	minBucket := c.minuteBucket()
	byKey := make(map[core.RatingKey]core.Title, len(keys))
	var missing []core.RatingKey
	seenMissing := map[core.RatingKey]bool{}
	c.cacheMu.RLock()
	for _, k := range keys {
		if _, ok := byKey[k]; ok {
			continue
		}
		if k.Kind() == core.KindMovie {
			if ent, ok := c.titleCache[k]; ok && ent.gen == gen && ent.minute == minBucket {
				if !ent.st.t.Adult && ent.st.t.IsReleased(c.now()) && (!c.config().GateMoviesByHomeRelease || homeReleaseVisible(&ent.st.t, c.now())) {
					byKey[k] = cloneTitle(ent.st.t)
				}
				continue
			}
		}
		if !seenMissing[k] {
			seenMissing[k] = true
			missing = append(missing, k)
		}
	}
	c.cacheMu.RUnlock()

	const batch = 200
	for i := 0; i < len(missing); i += batch {
		part := missing[i:min(i+batch, len(missing))]
		args := make([]any, len(part))
		for j, k := range part {
			args[j] = int64(k)
		}
		ts, err := c.queryTitles(ctx, `SELECT `+titleCols+` FROM catalog_titles WHERE key IN (`+placeholders(len(part))+`)`, args...)
		if err != nil {
			return nil, err
		}
		if c.writeGen.Load() == gen {
			c.cacheMu.Lock()
			if len(c.titleCache) >= 8192 {
				c.titleCache = map[core.RatingKey]cachedStoredTitle{}
			}
			for _, t := range ts {
				if t.Key.Kind() == core.KindMovie {
					c.titleCache[t.Key] = cachedStoredTitle{st: storedTitle{t: cloneTitle(t)}, gen: gen, minute: minBucket}
				}
			}
			c.cacheMu.Unlock()
		}
		for _, t := range ts {
			byKey[t.Key] = t
		}
	}
	for _, k := range keys {
		if t, ok := byKey[k]; ok {
			out = append(out, t)
		}
	}
	return out, nil
}

// CrowdGraphBatch reads stored co-engagement lists from catalog_crowd for a
// batch of keys without contacting TMDB, and resolves up to maxTargets of the
// referenced titles from the local mirror. Used for 2-hop crowd expansion.
func (c *Catalog) CrowdGraphBatch(ctx context.Context, keys []core.RatingKey, maxPerKey, maxTargets int) (map[core.RatingKey][]core.RatingKey, map[core.RatingKey]core.Title, error) {
	edges := make(map[core.RatingKey][]core.RatingKey, len(keys))
	if len(keys) == 0 {
		return edges, nil, nil
	}
	if maxPerKey <= 0 {
		maxPerKey = 20
	}
	if maxTargets <= 0 {
		maxTargets = 300
	}
	args := make([]any, 0, len(keys))
	seen := map[core.RatingKey]bool{}
	for _, k := range keys {
		if ck, ok := crowdKey(k); ok && !seen[ck] {
			seen[ck] = true
			args = append(args, int64(ck))
		}
	}
	if len(args) == 0 {
		return edges, nil, nil
	}
	// The original query had no guaranteed row order. Sort source keys and
	// query each chunk in that order so the target cap is independent of
	// input permutation and database row order. Each recommendation list
	// retains its stored ranking.
	sort.Slice(args, func(i, j int) bool { return args[i].(int64) < args[j].(int64) })
	var targetKeys []core.RatingKey
	seenTarget := map[core.RatingKey]bool{}
	const batch = 200
	for start := 0; start < len(args); start += batch {
		part := args[start:min(start+batch, len(args))]
		rows, err := c.db.QueryContext(ctx, `SELECT key, recs FROM catalog_crowd WHERE key IN (`+placeholders(len(part))+`) ORDER BY key`, part...)
		if err != nil {
			return nil, nil, err
		}
		for rows.Next() {
			var k int64
			var recs string
			if err := rows.Scan(&k, &recs); err != nil {
				rows.Close()
				return nil, nil, err
			}
			src := core.RatingKey(k)
			var list []core.RatingKey
			for _, s := range strings.Split(recs, ",") {
				if len(list) >= maxPerKey {
					break
				}
				if n, perr := strconv.ParseInt(s, 10, 64); perr == nil && n != 0 {
					tk := core.RatingKey(n)
					list = append(list, tk)
					if !seenTarget[tk] && len(targetKeys) < maxTargets {
						seenTarget[tk] = true
						targetKeys = append(targetKeys, tk)
					}
				}
			}
			if len(list) > 0 {
				edges[src] = list
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, nil, err
		}
	}
	loaded, err := c.titlesByKeys(ctx, targetKeys)
	if err != nil {
		return edges, nil, err
	}
	titles := make(map[core.RatingKey]core.Title, len(loaded))
	for _, t := range loaded {
		titles[t.Key] = t
	}
	return edges, titles, nil
}
