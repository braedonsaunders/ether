package availability

import (
	"context"
	"github.com/braedonsaunders/ether/internal/core"
)

type consoleEvidenceCatalog interface {
	RecordConsoleReleaseEvidence(context.Context, core.RatingKey, core.Release, string, string, int64, int64, int) error
	InvalidateConsoleReleaseEvidence(context.Context, string)
}

// All reads are keyed local rows. Only definitive provider answers enter cached;
// addon-provided markers remain hints. Absence and stale evidence stay unknown.
func (s *Service) publishConsoleEvidence(ctx context.Context, key core.RatingKey, r core.Release, source, policy string, cached int) {
	c, ok := s.d.Catalog.(consoleEvidenceCatalog)
	if !ok {
		return
	}
	var verified int64
	var state, storedPolicy string
	if err := s.db.QueryRowContext(ctx, `SELECT verified_at,state,policy FROM availability_releases WHERE id=? AND rating_key=? AND source_id=?`, r.ID, int64(key), source).Scan(&verified, &state, &storedPolicy); err != nil {
		return
	}
	until := int64(0)
	if state == "verified" && storedPolicy == policy && verified > 0 {
		until = verified + 21600
	}
	cacheUntil := int64(0)
	if cached >= 0 {
		cacheUntil = s.now().Unix() + 3600
	}
	if err := c.RecordConsoleReleaseEvidence(ctx, key, r, source, policy, until, cacheUntil, cached); err != nil {
		s.log.Debug("catalogue evidence projection", "error", err)
	}
}
func (s *Service) invalidateConsoleEvidence(ctx context.Context, id string) {
	c, ok := s.d.Catalog.(consoleEvidenceCatalog)
	if !ok {
		return
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM availability_releases WHERE id=? UNION SELECT id FROM availability_releases WHERE checked_id=?`, id, id)
	if err != nil {
		return
	}
	var ids []string
	for rows.Next() {
		var release string
		if rows.Scan(&release) == nil {
			ids = append(ids, release)
		}
	}
	rows.Close()
	for _, release := range ids {
		c.InvalidateConsoleReleaseEvidence(ctx, release)
	}
}

// Backfill is a bounded startup task over the retained release queue (capped at
// 30k records), decoding at most 500 descriptors at a time, never on HTTP reads.
func (s *Service) backfillConsoleEvidence(ctx context.Context) {
	if _, ok := s.d.Catalog.(consoleEvidenceCatalog); !ok {
		return
	}
	type observation struct {
		id, source, generation, raw string
		key                         core.RatingKey
	}
	generations := map[string]string{}
	for _, feed := range s.d.Feeds.FeedSources() {
		generations[feed.ID] = feed.Generation
	}
	lastID, lastSource := "", ""
	var lastKey int64
	for scanned := 0; scanned < 30000 && ctx.Err() == nil; scanned += 500 {
		rows, err := s.db.QueryContext(ctx, `SELECT id,rating_key,source_id,generation,payload FROM availability_releases WHERE (id,rating_key,source_id)>(?,?,?) AND state='verified' AND policy=? AND verified_at>? ORDER BY id,rating_key,source_id LIMIT 500`, lastID, lastKey, lastSource, s.policy(), s.now().Unix()-21600)
		if err != nil {
			return
		}
		batch := []observation{}
		for rows.Next() {
			var o observation
			if rows.Scan(&o.id, &o.key, &o.source, &o.generation, &o.raw) != nil {
				rows.Close()
				return
			}
			batch = append(batch, o)
		}
		err = rows.Err()
		rows.Close()
		if err != nil || len(batch) == 0 {
			return
		}
		for _, o := range batch {
			if ctx.Err() != nil {
				return
			}
			lastID, lastKey, lastSource = o.id, int64(o.key), o.source
			if generations[o.source] != o.generation {
				continue
			}
			r, err := s.decode(o.raw)
			if err != nil {
				continue
			}
			r.ID = o.id
			s.publishConsoleEvidence(ctx, o.key, r, o.source, s.policy(), -1)
		}
		if len(batch) < 500 {
			return
		}
	}
}
