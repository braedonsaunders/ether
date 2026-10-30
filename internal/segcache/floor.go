package segcache

import "time"

// Free-space floor. The budget is the most the owner lets the cache use, but
// the volume it lives on is shared with everything else (the system drive, a
// NAS share) and may be smaller than the budget or fill up from other
// writers. Eviction therefore also keeps a floor of the volume free: the
// smaller of 10 GiB and 5% of the volume. Pinned and protected releases are
// never evicted for it; if only they remain, the cache logs and waits.
//
// A measurement turns into an absolute cap on the cache's usage (usage then
// plus free space then, minus the floor) that every pass compares with the
// usage it sees under the index lock. Bytes another pass already freed are
// therefore never freed twice, and the cache's own writes count against the
// cap at once. On one node, passes that measure, evict and unlink are
// serialised (evictMu), so a measurement never sees space that an earlier
// pass dropped from the index but has not unlinked yet. Across the nodes of
// a shared cache, each eviction records its bytes in segcache_reclaim until
// they are unlinked, and measurements count them as free. What remains is
// segment writes in flight while the volume is measured: they can lower one
// cap by at most their size until the next measurement.
const (
	floorMaxBytes  = 10 << 30
	floorFraction  = 20 // 5%
	floorStatEvery = 2 * time.Second
)

// DiskUsageFunc reports the size and the free bytes of the cache's volume.
type DiskUsageFunc func() (total, free int64, err error)

// MinFreeBytes is the free space the cache leaves on a volume of total bytes.
func MinFreeBytes(total int64) int64 { return min(floorMaxBytes, max(total, 0)/floorFraction) }

// floorStat is one measurement of the volume.
type floorStat struct {
	ok      bool
	at      time.Time
	cap     int64 // most the cache may use while this measurement holds
	floor   int64
	free    int64
	failing bool // the last measurement failed (logged once)
}

// SetDiskUsage enables the free-space floor; nil disables it. It applies
// immediately.
func (c *Cache) SetDiskUsage(fn DiskUsageFunc) {
	c.floorMu.Lock()
	c.diskFn = fn
	c.floor = floorStat{}
	c.floorMu.Unlock()
	c.evict()
}

// FreeFloor reports the floor and the free space last measured (0, 0 while
// the floor is off or unmeasured).
func (c *Cache) FreeFloor() (floor, free int64) {
	c.floorMu.Lock()
	defer c.floorMu.Unlock()
	if !c.floor.ok {
		return 0, 0
	}
	return c.floor.floor, c.floor.free
}

// floorCap returns the usage cap the floor imposes, measuring the volume at
// most every floorStatEvery; ok is false while the floor is off or the volume
// cannot be measured. used reads the usage the cap is expressed in (the
// local index, or the shared pool plus bytes still being unlinked). Callers
// hold evictMu.
func (c *Cache) floorCap(used func() (int64, error)) (int64, bool) {
	c.floorMu.Lock()
	defer c.floorMu.Unlock()
	if c.diskFn == nil {
		return 0, false
	}
	now := c.now()
	if c.floor.ok && now.Sub(c.floor.at) < floorStatEvery && !now.Before(c.floor.at) {
		return c.floor.cap, true
	}
	u, uerr := used()
	total, free, err := c.diskFn()
	if err == nil {
		err = uerr
	}
	if err != nil || total <= 0 {
		if !c.floor.failing {
			c.logger.Warn("segment cache: cannot read free space; the free-space floor is paused", "err", err)
		}
		c.floor = floorStat{failing: true}
		return 0, false
	}
	floor := MinFreeBytes(total)
	c.floor = floorStat{ok: true, at: now, cap: max(u+free-floor, 0), floor: floor, free: free}
	return c.floor.cap, true
}

// localUsed is the local index's usage, for floorCap.
func (c *Cache) localUsed() (int64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.used, nil
}

// sharedUsed is the shared pool's usage plus the bytes any node has evicted
// but not yet unlinked, for floorCap: the volume does not show those as free
// yet, so counting them keeps two nodes from freeing the same shortfall. It
// is read before the volume is measured, so an unlink finishing in between
// can only make the cap too generous until the next measurement.
func (s *sharedCache) sharedUsed() (int64, error) {
	ctx, cancel := s.context()
	defer cancel()
	var used int64
	err := s.db.QueryRowContext(ctx, `SELECT used + COALESCE((SELECT SUM(bytes) FROM segcache_reclaim WHERE at > `+s.nowSQL()+`-?),0) FROM segcache_pool WHERE id=1`, reclaimStale).Scan(&used)
	return used, err
}
