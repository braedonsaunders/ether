package resolver

import (
	"context"
	"time"
)

// Retention and in-memory bounds for the resolver's caches. Everything here
// is derived data: dropping an entry only costs a repeat search, lookup or
// health check.
const (
	// badRetention is how long a release stays blacklisted after it was
	// last marked bad. Bad marks come from failed health checks and
	// mid-stream failover (missing articles, takedowns), which are almost
	// always permanent; if the release is still broken after this long it
	// fails one more check and is marked again.
	badRetention = 90 * 24 * time.Hour

	maxSearchCache     = 2000   // indexer result sets (each holds a release list)
	maxAvailEntries    = 50_000 // title availability, mirrored in resolver_availability
	maxVerifiedEntries = 20_000 // passed health checks, mirrored in resolver_verified
)

// boundMap keeps m below max entries before an insert: it first drops
// expired entries and, if that is not enough, arbitrary ones until the map
// is at three quarters of max (map iteration order is random, so this is a
// cheap random eviction amortised over many inserts). Callers hold the lock
// guarding m.
func boundMap[K comparable, V any](m map[K]V, max int, expired func(V) bool) {
	if len(m) < max {
		return
	}
	for k, v := range m {
		if expired(v) {
			delete(m, k)
		}
	}
	target := max * 3 / 4
	for k := range m {
		if len(m) <= target {
			break
		}
		delete(m, k)
	}
}

// unavailableRetention is how long Prune keeps an expired unavailable
// entry (in memory and in resolver_availability) for the recheck peek.
// It must outlive any search-cache result that was rejected alongside
// that entry: searchCached (resolver.go) caches a non-empty result for
// the full searchTTL (only an empty result is capped at
// freshReleaseCooloff, and searchRescue uses searchTTL too), so dropping
// the marker earlier would let Resolve — without lastUnavailableChecked —
// reuse that still-live stale result instead of fresh-searching. Hence
// the ceiling is max(unavailableRecheck, searchTTL) from checked; past it
// no associated search result can still suppress recovery.
func (s *Service) unavailableRetention() time.Duration {
	return max(unavailableRecheck, s.searchTTL())
}

// Prune drops expired entries from the in-memory caches and deletes
// resolver rows that can no longer be served: expired availability, health
// passes older than the health TTL and automatic bad marks not re-marked
// within badRetention (owner blocks never expire).
// Indexer reputation is a handful of counters and is kept. It is meant to be
// called periodically (the app runs it daily).
func (s *Service) Prune(ctx context.Context) (int64, error) {
	now := s.now()
	s.mu.Lock()
	for k, e := range s.searchCache {
		if !now.Before(e.expires) {
			delete(s.searchCache, k)
		}
	}
	// Keep unavailable entries past expiry while their stale search
	// result can still suppress recovery: the recheck peek reads checked
	// even expired. getAvailability still never serves these expired
	// entries; only lastUnavailableChecked peeks at them.
	for k, e := range s.avail {
		if now.After(e.expires) && (e.available || now.Sub(e.checked) > s.unavailableRetention()) {
			delete(s.avail, k)
		}
	}
	for k, at := range s.verified {
		if s.healthTTL <= 0 || now.Sub(at) >= s.healthTTL {
			delete(s.verified, k)
		}
	}
	for k, m := range s.bad {
		if !m.live(now) {
			delete(s.bad, k)
		}
	}
	s.mu.Unlock()
	if s.db == nil {
		return 0, nil
	}
	verifiedCutoff := now.Unix() + 1 // health cache disabled: nothing is trusted
	if s.healthTTL > 0 {
		verifiedCutoff = now.Add(-s.healthTTL).Unix()
	}
	var total int64
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`DELETE FROM resolver_availability WHERE expires_at<? AND (available=1 OR checked_at<?)`, []any{now.Unix(), now.Add(-s.unavailableRetention()).Unix()}},
		{`DELETE FROM resolver_verified WHERE checked_at<?`, []any{verifiedCutoff}},
		{`DELETE FROM resolver_bad WHERE manual=0 AND updated_at<?`, []any{now.Add(-badRetention).Unix()}},
	} {
		res, err := s.db.ExecContext(ctx, q.sql, q.args...)
		if err != nil {
			return total, err
		}
		n, _ := res.RowsAffected()
		total += n
	}
	return total, nil
}
