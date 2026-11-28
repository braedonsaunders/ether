package availability

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/releaseparse"
	"github.com/braedonsaunders/ether/internal/store"
)

const hiddenMigration = `
CREATE TABLE availability_hidden (
 kind TEXT NOT NULL, generation TEXT NOT NULL, rating_key INTEGER NOT NULL,
 created_at INTEGER NOT NULL, PRIMARY KEY(kind,generation,rating_key)
);
CREATE TABLE availability_hidden_current (
 kind TEXT PRIMARY KEY, generation TEXT NOT NULL, policy TEXT NOT NULL
);`

func init() {
	// Ephemeral derived paging sets: never exported in logical backups, and
	// rebuilt on demand after restore.
	store.RegisterLocalTable("availability_hidden")
	store.RegisterLocalTable("availability_hidden_current")
	store.RegisterLocalTable("availability_hidden_retired")
	store.RegisterRestore("availability_hidden", func(context.Context, *sql.Tx) (func(context.Context, *sql.Tx) error, error) {
		return func(ctx context.Context, tx *sql.Tx) error {
			if _, err := tx.ExecContext(ctx, `DELETE FROM availability_hidden`); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `DELETE FROM availability_hidden_retired`); err != nil {
				return err
			}
			_, err := tx.ExecContext(ctx, `DELETE FROM availability_hidden_current`)
			return err
		}, nil
	}, "availability_hidden", "availability_hidden_current", "availability_hidden_retired")

	// Rebuild availability_hidden as WITHOUT ROWID on SQLite and drop accumulated
	// 64-char-digest generations from 0.3.6. Own component so availability stays
	// pinned at version 6.
	store.RegisterAfter("availability_hidden_slim", "availability")
	store.Register("availability_hidden_slim", 1, "SELECT 1;")
	store.RegisterSQLiteApply("availability_hidden_slim", 1, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
DELETE FROM availability_hidden_current;
DROP TABLE availability_hidden;
CREATE TABLE availability_hidden (
 kind TEXT NOT NULL, generation TEXT NOT NULL, rating_key INTEGER NOT NULL,
 created_at INTEGER NOT NULL, PRIMARY KEY(kind,generation,rating_key)
) WITHOUT ROWID;`)
		return err
	})
	store.RegisterDialect("availability_hidden_slim", 1, store.Postgres, `DELETE FROM availability_hidden_current; DELETE FROM availability_hidden;`)

	// Interval membership: a key is hidden in snapshot N iff
	// from_seq<=N<to_seq (to_seq=max int64 while current), so publishing
	// writes only the delta instead of a full new generation. Converts the
	// current generation to seq 1 and drops retired generations. Own
	// component, like availability_hidden_slim, so availability stays at 6.
	store.RegisterAfter("availability_hidden_intervals", "availability_hidden_slim")
	store.Register("availability_hidden_intervals", 1, "SELECT 1;")
	store.RegisterSQLiteApply("availability_hidden_intervals", 1, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
CREATE TABLE availability_hidden_new (
 kind TEXT NOT NULL, rating_key INTEGER NOT NULL, from_seq INTEGER NOT NULL,
 to_seq INTEGER NOT NULL, PRIMARY KEY(kind,rating_key,from_seq)
) WITHOUT ROWID;
INSERT INTO availability_hidden_new(kind,rating_key,from_seq,to_seq)
 SELECT h.kind,h.rating_key,1,9223372036854775807 FROM availability_hidden h
 JOIN availability_hidden_current c ON c.kind=h.kind AND c.generation=h.generation;
DROP TABLE availability_hidden;
ALTER TABLE availability_hidden_new RENAME TO availability_hidden;
CREATE INDEX availability_hidden_toseq ON availability_hidden(kind,to_seq,rating_key);
CREATE TABLE availability_hidden_current_new (
 kind TEXT PRIMARY KEY, seq INTEGER NOT NULL, policy TEXT NOT NULL
);
INSERT INTO availability_hidden_current_new(kind,seq,policy) SELECT kind,1,policy FROM availability_hidden_current;
DROP TABLE availability_hidden_current;
ALTER TABLE availability_hidden_current_new RENAME TO availability_hidden_current;
CREATE TABLE availability_hidden_retired (
 kind TEXT NOT NULL, seq INTEGER NOT NULL, retired_at INTEGER NOT NULL,
 PRIMARY KEY(kind,seq)
) WITHOUT ROWID;`)
		return err
	})
	store.RegisterDialect("availability_hidden_intervals", 1, store.Postgres, `
CREATE TABLE availability_hidden_new (
 kind TEXT NOT NULL, rating_key BIGINT NOT NULL, from_seq BIGINT NOT NULL,
 to_seq BIGINT NOT NULL, PRIMARY KEY(kind,rating_key,from_seq)
);
INSERT INTO availability_hidden_new(kind,rating_key,from_seq,to_seq)
 SELECT h.kind,h.rating_key,1,9223372036854775807 FROM availability_hidden h
 JOIN availability_hidden_current c ON c.kind=h.kind AND c.generation=h.generation;
DROP TABLE availability_hidden;
ALTER TABLE availability_hidden_new RENAME TO availability_hidden;
CREATE INDEX availability_hidden_toseq ON availability_hidden(kind,to_seq,rating_key);
CREATE TABLE availability_hidden_current_new (
 kind TEXT PRIMARY KEY, seq BIGINT NOT NULL, policy TEXT NOT NULL
);
INSERT INTO availability_hidden_current_new(kind,seq,policy) SELECT kind,1,policy FROM availability_hidden_current;
DROP TABLE availability_hidden_current;
ALTER TABLE availability_hidden_current_new RENAME TO availability_hidden_current;
CREATE TABLE availability_hidden_retired (
 kind TEXT NOT NULL, seq BIGINT NOT NULL, retired_at BIGINT NOT NULL,
 PRIMARY KEY(kind,seq)
);`)

	store.RegisterAfter("availability_checks_retention", "availability_coverage")
	store.Register("availability_checks_retention", 1, `CREATE INDEX availability_title_checks_prune ON availability_title_checks(next_check, checked_at);`)

	// Retention runs every feed cycle (once a minute). Without these, its
	// "anything expired?" probes scanned availability_releases (2 KB rows)
	// and an availability_feed_items index whose leading column is the
	// title, every minute. Own component: availability is pinned to the
	// delivered schema boundary.
	store.RegisterAfter("availability_retention", "availability")
	store.Register("availability_retention", 1, `CREATE INDEX availability_feed_items_seen ON availability_feed_items(seen_at); CREATE INDEX availability_releases_last_seen ON availability_releases(last_seen);`)
	// Invalidate and the console evidence projection look releases up by
	// checked_id; without an index every call scanned the whole table.
	store.Register("availability_retention", 2, `CREATE INDEX availability_releases_checked_id ON availability_releases(checked_id);`)
	store.Register("availability", 7, `ALTER TABLE availability_feeds ADD COLUMN last_new_at BIGINT NOT NULL DEFAULT 0;`)
	store.Register("availability", 6, hiddenMigration)
	// covered_since: since when a feed's head polls have overlapped without a
	// gap, so it has shown every release posted since then. full_at: a
	// title's last probe across every indexer (feed coverage aside). Its own
	// component, so the availability versions upgrade fixtures pin stay put.
	store.RegisterAfter("availability_coverage", "availability")
	store.Register("availability_coverage", 1, `
ALTER TABLE availability_feeds ADD COLUMN covered_since INTEGER NOT NULL DEFAULT 0;
ALTER TABLE availability_title_checks ADD COLUMN full_at INTEGER NOT NULL DEFAULT 0;`)
	// How often each feedless source (a scraper or addon) returned anything
	// when every source was searched, so routine checks ask the best one.
	store.Register("availability_coverage", 2, `
CREATE TABLE availability_source_yield (source_id TEXT PRIMARY KEY, searches INTEGER NOT NULL DEFAULT 0, found INTEGER NOT NULL DEFAULT 0);`)
	// Title probes became event-driven (first look, digital date, an
	// unplayable release) and ask sources one at a time in priority order,
	// so feed coverage, full-search times and scraper yields are gone, and
	// a recorded next_check now means only an unfinished search. Cadence
	// times from earlier versions would each trigger one more search.
	// polled_at is a feed's last scheduled head poll; checked_at also moves
	// with the gap pages between polls.
	store.Register("availability_coverage", 3, `
DROP TABLE availability_source_yield;
ALTER TABLE availability_feeds DROP COLUMN covered_since;
ALTER TABLE availability_title_checks DROP COLUMN full_at;
ALTER TABLE availability_feeds ADD COLUMN polled_at INTEGER NOT NULL DEFAULT 0;
UPDATE availability_feeds SET polled_at=checked_at;
UPDATE availability_title_checks SET next_check=0 WHERE reason<>'Search could not be completed';`)
	store.Register("availability", 5, `
ALTER TABLE availability_budget ADD COLUMN ceiling INTEGER NOT NULL DEFAULT 0;
ALTER TABLE availability_feeds ADD COLUMN catchup_oldest INTEGER NOT NULL DEFAULT 0;
ALTER TABLE availability_feeds ADD COLUMN catchup_started INTEGER NOT NULL DEFAULT 0;
CREATE TABLE availability_title_checks (
 rating_key INTEGER PRIMARY KEY,
 checked_at INTEGER NOT NULL DEFAULT 0,
 next_check INTEGER NOT NULL DEFAULT 0,
 state TEXT NOT NULL DEFAULT '',
 reason TEXT NOT NULL DEFAULT ''
);`)
	store.Register("availability", 4, `ALTER TABLE availability_releases ADD COLUMN policy_digest TEXT NOT NULL DEFAULT ''; ALTER TABLE availability_feed_items ADD COLUMN policy_digest TEXT NOT NULL DEFAULT '';`)
	store.Register("availability", 3, `
 ALTER TABLE availability_feeds ADD COLUMN catchup_target TEXT NOT NULL DEFAULT '';
 ALTER TABLE availability_feeds ADD COLUMN catchup_tail TEXT NOT NULL DEFAULT '';
 ALTER TABLE availability_feeds ADD COLUMN catchup_offset INTEGER NOT NULL DEFAULT 0;
 -- Earlier monitors only seeded the newest page and discarded gap boundaries.
 -- Recover their recent history once, within the normal catch-up allowance.
 UPDATE availability_feeds SET catchup_target='__ether_recent_history__',gap=1,
 message='Recent feed history is queued for bounded catch-up';
 `)
	store.Register("availability", 2, `
 CREATE TABLE availability_feed_items (
 id TEXT NOT NULL, source_id TEXT NOT NULL, generation TEXT NOT NULL,
 match_title TEXT NOT NULL, payload TEXT NOT NULL, seen_at INTEGER NOT NULL,
 PRIMARY KEY(id,source_id));
 CREATE INDEX availability_feed_match ON availability_feed_items(match_title,seen_at);
 `)

	store.Register("availability", 1, `
CREATE TABLE availability_feeds (
 source_id TEXT PRIMARY KEY, generation TEXT NOT NULL, anchor TEXT NOT NULL DEFAULT '',
 checked_at INTEGER NOT NULL DEFAULT 0, synced_at INTEGER NOT NULL DEFAULT 0,
 gap INTEGER NOT NULL DEFAULT 0, message TEXT NOT NULL DEFAULT '', discovered INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE availability_releases (
 id TEXT NOT NULL, rating_key INTEGER NOT NULL, source_id TEXT NOT NULL, generation TEXT NOT NULL,
 payload TEXT NOT NULL, title TEXT NOT NULL, name TEXT NOT NULL, protocol TEXT NOT NULL,
 first_seen INTEGER NOT NULL, last_seen INTEGER NOT NULL, first_verified INTEGER NOT NULL DEFAULT 0,
 verified_at INTEGER NOT NULL DEFAULT 0, policy TEXT NOT NULL DEFAULT '', state TEXT NOT NULL DEFAULT 'observed',
 reason TEXT NOT NULL DEFAULT '', checked_id TEXT NOT NULL DEFAULT '', next_check INTEGER NOT NULL DEFAULT 0, attempts INTEGER NOT NULL DEFAULT 0,
 revision INTEGER NOT NULL DEFAULT 0, priority REAL NOT NULL DEFAULT 0, PRIMARY KEY(id,rating_key,source_id)
);
CREATE INDEX availability_queue ON availability_releases(next_check,priority);
CREATE INDEX availability_title ON availability_releases(rating_key,verified_at);
CREATE TABLE availability_budget(day TEXT NOT NULL, resource TEXT NOT NULL, used INTEGER NOT NULL DEFAULT 0, PRIMARY KEY(day,resource));
`)
}
func (s *Service) spend(ctx context.Context, kind string, n, limit int64) error {
	if n <= 0 {
		return nil
	}
	if limit <= 0 {
		return core.ErrBudgetExhausted
	}
	day := s.now().UTC().Format("2006-01-02")
	if _, err := s.db.ExecContext(ctx, `INSERT INTO availability_budget(day,resource) VALUES(?,?) ON CONFLICT(day,resource) DO NOTHING`, day, kind); err != nil {
		return err
	}
	// ceiling remembers the highest allowance that admitted a request today.
	// Lowering the allowance later leaves used above the new number; it does
	// not mean the cap was bypassed.
	result, err := s.db.ExecContext(ctx, `UPDATE availability_budget SET used=used+?, ceiling=CASE WHEN ceiling>=? THEN ceiling ELSE ? END WHERE day=? AND resource=? AND used<=?`, n, limit, limit, day, kind, limit-n)
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows == 0 {
		return core.ErrBudgetExhausted
	}
	return nil
}
func (s *Service) remember(ctx context.Context, f core.ReleaseFeed, t core.Title, r core.Release, policy string) error {
	defer s.clearVisibility()
	if r.Info.Title == "" && r.Name != "" {
		r.Info = releaseparse.Parse(r.Name)
	}
	raw, err := s.encode(r)
	if err != nil {
		return err
	}
	now := s.now().Unix()
	_, err = s.db.ExecContext(ctx, `INSERT INTO availability_releases(id,rating_key,source_id,generation,payload,title,name,protocol,first_seen,last_seen,priority,policy,policy_digest) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)
 ON CONFLICT(id,rating_key,source_id) DO UPDATE SET last_seen=excluded.last_seen,payload=CASE WHEN availability_releases.policy_digest=excluded.policy_digest THEN availability_releases.payload ELSE excluded.payload END,policy_digest=excluded.policy_digest,priority=excluded.priority,
 verified_at=CASE WHEN availability_releases.generation<>excluded.generation THEN 0 ELSE availability_releases.verified_at END,
 first_verified=CASE WHEN availability_releases.generation<>excluded.generation THEN 0 ELSE availability_releases.first_verified END,
 next_check=CASE WHEN availability_releases.generation<>excluded.generation THEN 0 ELSE availability_releases.next_check END,
 state=CASE WHEN availability_releases.generation<>excluded.generation THEN 'observed' ELSE availability_releases.state END,
 generation=excluded.generation`, r.ID, t.Key, f.ID, f.Generation, raw, t.Title, r.Name, r.Protocol, now, now, t.Popularity, policy, releasePolicyDigest(r))
	if err == nil {
		s.publishConsoleEvidence(ctx, t.Key, r, f.ID, policy, -1)
		_ = s.maybeOpenGateFromRelease(ctx, f, t, r, s.now(), false)
	}
	return err
}

// Prune removes expired observations, retired hidden generations, and aged
// completed title-check records during scheduled maintenance.
func (s *Service) Prune(ctx context.Context) (int64, error) {
	if err := s.prune(ctx); err != nil {
		return 0, err
	}
	var total int64
	now := s.now().Unix()
	avail := s.cfg().Availability
	if avail.ModeFor(hiddenMovies) != config.ModeDigital && avail.DisableMovieEvidenceGate {
		if res, err := s.db.ExecContext(ctx, `DELETE FROM availability_hidden WHERE kind=?`, hiddenMovies); err == nil {
			n, _ := res.RowsAffected()
			total += n
		}
		_, _ = s.db.ExecContext(ctx, `DELETE FROM availability_hidden_retired WHERE kind=?`, hiddenMovies)
		_, _ = s.db.ExecContext(ctx, `DELETE FROM availability_hidden_current WHERE kind=?`, hiddenMovies)
	}
	for _, kind := range []string{hiddenMovies, hiddenShows, hiddenEpisodes} {
		var cur int64
		err := s.db.QueryRowContext(ctx, `SELECT seq FROM availability_hidden_current WHERE kind=?`, kind).Scan(&cur)
		if errors.Is(err, sql.ErrNoRows) {
			// No reader can pin a seq without a current row.
			res, err := s.db.ExecContext(ctx, `DELETE FROM availability_hidden WHERE kind=?`, kind)
			if err != nil {
				return total, err
			}
			n, _ := res.RowsAffected()
			total += n
			if _, err := s.db.ExecContext(ctx, `DELETE FROM availability_hidden_retired WHERE kind=?`, kind); err != nil {
				return total, err
			}
			continue
		}
		if err != nil {
			return total, err
		}
		n, err := pruneHiddenRetention(ctx, s.db, kind, cur, now)
		if err != nil {
			return total, err
		}
		total += n
	}
	// First-look completion is durable. Pruning it would queue old titles again.
	return total, nil
}

func (s *Service) prune(ctx context.Context) error {
	if c, ok := s.d.Catalog.(interface {
		PruneConsoleReleaseEvidence(context.Context, int64) error
	}); ok {
		if err := c.PruneConsoleReleaseEvidence(ctx, s.now().Unix()); err != nil {
			s.log.Debug("prune catalogue release evidence", "error", err)
		}
	}
	cutoff := s.retentionCutoff().Unix()
	var hasExpiredFeedItems bool
	if err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM availability_feed_items WHERE seen_at<?)`, cutoff).Scan(&hasExpiredFeedItems); err != nil {
		return err
	}
	retainedChanged := hasExpiredFeedItems
	if hasExpiredFeedItems {
		if _, err := s.db.ExecContext(ctx, `DELETE FROM availability_feed_items WHERE seen_at<?`, cutoff); err != nil {
			return err
		}
	}
	var hasOverflowFeedItems bool
	if err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM availability_feed_items LIMIT 1 OFFSET 30000)`).Scan(&hasOverflowFeedItems); err != nil {
		return err
	}
	retainedChanged = retainedChanged || hasOverflowFeedItems
	if hasOverflowFeedItems {
		if _, err := s.db.ExecContext(ctx, `DELETE FROM availability_feed_items WHERE (id,source_id) IN (SELECT id,source_id FROM availability_feed_items ORDER BY seen_at DESC LIMIT 2147483647 OFFSET 30000)`); err != nil {
			return err
		}
	}

	var hasExpiredReleases bool
	if err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM availability_releases WHERE last_seen<?)`, cutoff).Scan(&hasExpiredReleases); err != nil {
		return err
	}
	retainedChanged = retainedChanged || hasExpiredReleases
	if hasExpiredReleases {
		if _, err := s.db.ExecContext(ctx, `DELETE FROM availability_releases WHERE last_seen<?`, cutoff); err != nil {
			return err
		}
	}
	// Bound even very busy feeds; retaining recent/high-priority work is preferable
	// to letting monitoring consume an unbounded database.
	var hasOverflowReleases bool
	if err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM availability_releases LIMIT 1 OFFSET 30000)`).Scan(&hasOverflowReleases); err != nil {
		return err
	}
	retainedChanged = retainedChanged || hasOverflowReleases
	if hasOverflowReleases {
		if _, err := s.db.ExecContext(ctx, `DELETE FROM availability_releases WHERE (id,rating_key,source_id) IN (SELECT id,rating_key,source_id FROM availability_releases ORDER BY last_seen DESC LIMIT 2147483647 OFFSET 30000)`); err != nil {
			// Retention failures are surfaced rather than silently growing the table.
			return err
		}
	}
	if err := s.pruneVerdicts(ctx, retainedChanged); err != nil {
		return err
	}
	budgetCutoff := s.now().Add(-35 * 24 * time.Hour).UTC().Format("2006-01-02")
	var hasExpiredBudget bool
	if err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM availability_budget WHERE day<?)`, budgetCutoff).Scan(&hasExpiredBudget); err != nil {
		return err
	}
	if !hasExpiredBudget {
		return nil
	}
	_, err := s.db.ExecContext(ctx, `DELETE FROM availability_budget WHERE day<?`, budgetCutoff)
	return err
}
func benign(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, core.ErrBudgetExhausted)
}

// feedState is a feed's last head release, last scheduled head poll, last
// request of any kind, and whether a gap is still open.
type feedState struct {
	anchor                   string
	polled, checked, lastNew int64
	gap                      bool
}

func (s *Service) feedState(ctx context.Context, f core.ReleaseFeed) (feedState, error) {
	var st feedState
	var gap int
	err := s.db.QueryRowContext(ctx, `SELECT anchor,polled_at,checked_at,gap,last_new_at FROM availability_feeds WHERE source_id=? AND generation=?`, f.ID, f.Generation).Scan(&st.anchor, &st.polled, &st.checked, &gap, &st.lastNew)
	if errors.Is(err, sql.ErrNoRows) {
		return feedState{}, nil
	}
	st.gap = gap != 0
	return st, err
}

// markPolled records a scheduled head poll, made or denied: the next one
// waits a full interval either way.
func (s *Service) markPolled(ctx context.Context, f core.ReleaseFeed) error {
	_, err := s.db.ExecContext(ctx, `UPDATE availability_feeds SET polled_at=? WHERE source_id=? AND generation=?`, s.now().Unix(), f.ID, f.Generation)
	return err
}
func safeReason(err error) string {
	switch {
	case errors.Is(err, core.ErrBudgetExhausted):
		return "Background allowance reached; playback capacity is reserved"
	case errors.Is(err, core.ErrArticleMissing):
		return "Sampled articles are missing on the configured providers"
	case errors.Is(err, core.ErrNotCached):
		return "Not cached by the configured debrid providers"
	case errors.Is(err, core.ErrReleaseRejected):
		return "Source was rejected by media validation"
	case errors.Is(err, context.DeadlineExceeded):
		return "Validation timed out; availability remains unknown"
	default:
		return "Source could not be checked; it will be retried"
	}
}

// Keep enough history for the largest supported window even if settings change later.
func (s *Service) retentionCutoff() time.Time { return s.now().Add(-367 * 24 * time.Hour) }
