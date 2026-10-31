package collections

import (
	"context"
	"hash/fnv"
	"math/rand/v2"
	"sort"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/schedule"
)

// rotate picks the subset of keys shown in the current period. Over one
// cycle (ceil(n/size) periods) every member is shown exactly once; each
// cycle uses a fresh deterministic shuffle, and within a window titles keep
// the collection's order. The result depends only on (id, keys, period),
// so every request in a period sees the same subset.
func rotate(keys []core.RatingKey, size int, every, id string, now time.Time) []core.RatingKey {
	n := len(keys)
	if size <= 0 || n <= size {
		return keys
	}
	period := periodIndex(every, now)
	windows := (n + size - 1) / size
	cycle := period / int64(windows)
	slot := int(period % int64(windows))

	h := fnv.New64a()
	h.Write([]byte(id))
	seed := h.Sum64() ^ uint64(cycle)*0x9e3779b97f4a7c15
	perm := rand.New(rand.NewPCG(seed, uint64(cycle))).Perm(n)

	start := slot * size
	end := start + size
	if end > n {
		// The last window of a cycle is topped up from the start so every
		// period shows a full set.
		end = n
	}
	picked := append([]int(nil), perm[start:end]...)
	for i := 0; len(picked) < size && i < start; i++ {
		picked = append(picked, perm[i])
	}
	sort.Ints(picked) // keep the collection's own order
	out := make([]core.RatingKey, len(picked))
	for i, p := range picked {
		out[i] = keys[p]
	}
	return out
}

// periodIndex counts days or weeks since the Unix epoch in local time.
func periodIndex(every string, now time.Time) int64 {
	_, off := now.Zone()
	days := (now.Unix() + int64(off)) / 86400
	if every == "weekly" {
		// Weeks start on Monday (1970-01-01 was a Thursday).
		return (days + 3) / 7
	}
	return days
}

// ---------------------------------------------------------------------------
// Scheduler

// Run refreshes scheduled collections until ctx ends: anything never built
// or edited since its last build right away, then hourly/daily/weekly per
// spec. Live collections are evaluated on request instead.
func (s *Service) Run(ctx context.Context) {
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		s.RunDue(ctx)
		timer.Reset(s.tick)
	}
}

// RunDue rebuilds every collection that is due and returns how many were
// built.
func (s *Service) RunDue(ctx context.Context) int {
	if err := s.seedLibraryDefaults(ctx); err != nil && ctx.Err() == nil {
		s.log.Warn("collections: default facets", "err", err)
	}
	rows, err := s.loadRows(ctx)
	if err != nil {
		s.log.Warn("collections: scheduler", "err", err)
		return 0
	}
	built := 0
	for _, row := range rows {
		if ctx.Err() != nil {
			break
		}
		if !row.builtin && row.owner > 0 && len(row.spec.Sources) == 1 && row.spec.Sources[0].Kind == "titles" && len(row.spec.Sources[0].Keys) == 0 {
			_, _ = s.db.ExecContext(ctx, `DELETE FROM collections_specs WHERE id=? AND builtin=0`, row.id)
			continue
		}
		if !s.due(row) {
			continue
		}
		bctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
		_, err := s.build(bctx, row)
		cancel()
		if err != nil {
			if ctx.Err() == nil {
				s.log.Warn("collections: refresh failed", "collection", row.spec.Name, "err", err)
			}
			continue
		}
		built++
	}
	return built
}

func (s *Service) due(row *specRow) bool {
	if _, ok := localFacetSpec(row.spec); ok {
		return false
	}
	if !row.enabled {
		return false
	}
	if row.lastBuilt.IsZero() || row.buildHash == "" {
		return true // includes live collections, so the Studio has a count
	}
	if row.spec.Refresh == RefreshLive {
		return false
	}
	return schedule.Due(s.now(), row.lastBuilt, refreshInterval(row.spec.Refresh))
}
