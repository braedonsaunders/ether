package catalog

import (
	"context"
	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/libraryfilter"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

// CachedCreditItems pages existing cached credit membership, including titles
// opened on demand. It never fetches metadata or hydrates filmographies.
// Viewer and inherited-title policy belongs to the consuming Experience.
func (c *Catalog) CachedCreditItems(ctx context.Context, after core.RatingKey, limit int) ([]core.RatingKey, error) {
	if after < 0 || limit < 1 || limit > 500 {
		return nil, core.ErrNotAvailable
	}
	rows, err := c.db.QueryContext(ctx, `SELECT DISTINCT c.key FROM catalog_credits c JOIN catalog_titles t ON t.key=c.key WHERE c.person_tmdb>0 AND c.name<>'' AND c.key>? AND t.type IN ('movie','show','season','episode') ORDER BY c.key LIMIT ?`, int64(after), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	keys := make([]core.RatingKey, 0, limit)
	for rows.Next() {
		var key core.RatingKey
		if err = rows.Scan(&key); err != nil {
			return nil, err
		}
		keys = append(keys, key)
	}
	return keys, rows.Err()
}

// CachedCreditTitles is a read-only People snapshot. Existing cached-read gates
// may conservatively omit a title; every returned title is additionally checked
// against the caller's immutable config and time, including inherited parents.
// The caller must hold the config lease and fence durable changes separately.
func (c *Catalog) CachedCreditTitles(ctx context.Context, keys []core.RatingKey, cfg config.Catalog, now time.Time) (map[core.RatingKey]core.Title, error) {
	filter, err := libraryfilter.Compile(cfg.Exclusions)
	if err != nil {
		return nil, err
	}
	// Credit proofs must see a credit written by another process during this
	// cache minute. The minute snapshot stays in place for ordinary reads.
	titles, err := c.cachedGetMany(ctx, keys, false)
	if err != nil {
		return nil, err
	}
	counts := make([]*core.Title, 0, len(titles))
	for key := range titles {
		title := titles[key]
		counts = append(counts, &title)
	}
	if err := applyAiredCounts(ctx, c.db, now, counts); err != nil {
		return nil, err
	}
	for _, title := range counts {
		if title.Adult || !title.IsReleased(now) || filter.Hidden(title) || (cfg.GateMoviesByHomeRelease && !homeReleaseVisible(title, now)) || (title.Type == core.TypeSeason && !listableSeason(title, now)) {
			delete(titles, title.Key)
		} else {
			titles[title.Key] = *title
		}
	}
	return titles, nil
}
