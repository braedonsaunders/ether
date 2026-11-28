// Disk admission shared by complete plugin files and playback segments.
// The plugin store supplies a short, pool-wide allocation lock. Segment IO
// runs outside it. A reservation covers the bytes between admission and
// publication, including writes in progress on another NAS node.
package segcache

import (
	"context"
	"errors"
	"math"
	"time"
)

// FileCandidate is an idle whole file or a resumable partial. Evict rechecks
// the object's lease and returns the bytes it actually reclaimed.
type FileCandidate struct {
	LastUsed time.Time
	Evict    func() (int64, error)
}

// FileCandidates lists the oldest eligible files first. It is called only
// when a write or maintenance pass needs to reclaim space.
type FileCandidates func() ([]FileCandidate, error)

var ErrPoolFull = errors.New("media cache has insufficient evictable space")

const (
	headroomRefreshEvery = time.Second
	segmentBatchMaxBytes = 16 << 20
)

// FilePool supplies reservations and eviction for whole plugin files.
type FilePool interface {
	// accounted includes retained files, partials and the unwritten portion of
	// outstanding reservations.
	WithAllocation(func(accounted, unwritten int64, files FileCandidates) error) error
	FileUsage() (retained, reserved int64, err error)
}

// SetFilePool attaches the file store to the media cache budget.
func (c *Cache) SetFilePool(files FilePool) {
	c.releaseBatch()
	c.mu.Lock()
	c.files = files
	c.mu.Unlock()
	if c.shared != nil || c.indexed.Load() {
		c.trimPool()
	}
}

func (c *Cache) releaseBatch() { c.releaseBatchIf("") }

// releaseBatchIf releases the current batch, or only the batch with the given
// lease when one is named (an expiry timer must not end a newer batch).
func (c *Cache) releaseBatchIf(lease string) {
	c.mu.Lock()
	if lease != "" && c.batchID != lease {
		c.mu.Unlock()
		return
	}
	id, unused := c.batchID, c.batchBytes
	c.batchID, c.batchBytes, c.batchAt = "", 0, time.Time{}
	c.mu.Unlock()
	if c.shared != nil && id != "" && unused > 0 {
		c.endReservation(id, unused)
	}
}

func (c *Cache) releasePoolBatches() {
	c.releaseBatch()
	if c.shared == nil {
		return
	}
	sharedPoolsMu.Lock()
	peers := append([]*Cache(nil), sharedPools[c.shared.token]...)
	sharedPoolsMu.Unlock()
	for _, peer := range peers {
		if peer != c {
			peer.releaseBatch()
		}
	}
}

func (c *Cache) logPoolFull(shortage int64) {
	now := c.now()
	c.mu.Lock()
	shouldLog := now.Sub(c.lastOverLog) >= overBudgetLogEvery
	if shouldLog {
		c.lastOverLog = now
	}
	budget := c.budget
	c.mu.Unlock()
	if shouldLog {
		c.logger.Warn("media cache full: insufficient evictable space", "shortBytes", shortage, "budget", budget)
	}
}

// MediaFileCapacity bounds one object before admission accounts for other
// media and the volume's free-space floor. Zero budget means unlimited.
func (c *Cache) MediaFileCapacity() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.policy.Mode == "off" {
		return 0
	}
	if c.budget == 0 {
		return math.MaxInt64
	}
	return c.budget
}

func (c *Cache) trimPool() bool { return c.trimPoolWatermarked(false) }
func (c *Cache) trimPoolWatermarked(watermarked bool) bool {
	c.mu.Lock()
	files := c.files
	c.mu.Unlock()
	if files == nil {
		return false
	}
	err := files.WithAllocation(func(accounted, unwritten int64, candidates FileCandidates) error {
		// Retain the segment worker's 95%/90% hysteresis and its last-
		// segment exception when there is no other class to compare.
		if watermarked && accounted == 0 {
			defer c.publishFileUsage(0)()
			if c.shared != nil {
				c.shared.sweepWatermarked()
			} else {
				c.evictLocal(true)
			}
			return nil
		}
		_, err := c.makeRoom(accounted, unwritten, 0, candidates, watermarked)
		return err
	})
	if err != nil {
		c.logger.Debug("media pool sweep", "err", err)
	}
	return true
}

// MakeFileRoom is called under the pool allocation lock. Playback and
// plugin-file admissions use the same order when pressure requires eviction.
func (c *Cache) MakeFileRoom(accounted, unwritten, additional int64, candidates FileCandidates) error {
	_, err := c.makeRoom(accounted, unwritten, additional, candidates, false)
	return err
}

func (c *Cache) makeRoom(accounted, unwritten, additional int64, candidates FileCandidates, watermarked bool) (int64, error) {
	if accounted < 0 || unwritten < 0 || additional < 0 || unwritten > accounted {
		return 0, errors.New("invalid media allocation")
	}
	c.releasePoolBatches()
	defer c.publishFileUsage(accounted - unwritten)()
	cap := c.MediaFileCapacity()
	if additional > cap {
		return 0, ErrTooLarge
	}
	if c.shared == nil && !c.indexed.Load() {
		// Maintenance runs again when the background index completes. A
		// writer must know about retained segments before spending its bytes.
		if additional == 0 {
			return 0, nil
		}
		if err := c.indexDisk(context.Background()); err != nil {
			return 0, err
		}
	}
	used, pending, err := c.segmentAllocation()
	if err != nil {
		return 0, err
	}
	total, err := sumSizes(used, pending, accounted, additional)
	if err != nil {
		return 0, err
	}
	shortage := max(total-cap, 0)
	if watermarked && cap > 0 && used >= highWatermark(cap) {
		shortage = max(shortage, total-lowWatermark(cap))
	}
	diskShort, diskHeadroom, err := c.volumeShortfall(used, accounted-unwritten, unwritten, pending, additional)
	if err != nil {
		return 0, err
	}
	shortage = max(shortage, diskShort)
	if shortage <= 0 {
		budgetLimit := cap
		if budgetLimit < math.MaxInt64 {
			budgetLimit = highWatermark(budgetLimit)
		}
		budgetHeadroom := max(budgetLimit-total, 0)
		return min(budgetHeadroom, diskHeadroom), nil
	}
	var files []FileCandidate
	if accounted > 0 && candidates != nil {
		files, err = candidates()
		if err != nil {
			return 0, err
		}
	}
	// Walk the sorted file list. At each boundary, evict only playback
	// segments ranked older than that file. Its whole size is reclaimed
	// only if the older eligible segments did not meet the shortfall.
	for _, file := range files {
		if used > 0 {
			reclaimed, err := c.evictSegmentsBefore(shortage, file.LastUsed)
			if err != nil {
				return 0, err
			}
			shortage -= reclaimed
			used -= reclaimed
		}
		if shortage <= 0 {
			return 0, nil
		}
		reclaimed, err := file.Evict()
		if err != nil {
			return 0, err
		}
		shortage -= reclaimed
		if shortage <= 0 {
			return 0, nil
		}
	}
	if used > 0 {
		reclaimed, err := c.evictSegmentsBefore(shortage, time.Time{})
		if err != nil {
			return 0, err
		}
		shortage -= reclaimed
	}
	if shortage > 0 {
		c.logPoolFull(shortage)
		return 0, ErrPoolFull
	}
	return 0, nil
}

func (c *Cache) publishFileUsage(retained int64) func() {
	c.poolUsage.Store(&retained)
	return func() { c.poolUsage.CompareAndSwap(&retained, nil) }
}

func (c *Cache) evictSegmentsBefore(need int64, cutoff time.Time) (int64, error) {
	if c.shared == nil {
		return c.evictLocalTarget(false, need, cutoff)
	}
	var freed int64
	for freed < need {
		more, n, err := c.shared.evictTarget("", false, false, need-freed, cutoff)
		freed += n
		if err != nil || !more {
			return freed, err
		}
	}
	return freed, nil
}

// A volume measurement is refreshed at most once per headroomRefreshEvery when
// free headroom is known and no new file was committed. Publish the segment
// headroom to the playback planners as well.
func (c *Cache) volumeShortfall(used, retained, unwritten, pending, additional int64) (int64, int64, error) {
	c.floorMu.Lock()
	defer c.floorMu.Unlock()
	if c.diskFn == nil {
		return 0, math.MaxInt64, nil
	}
	now := c.now()
	var size, free, floor int64
	cached := false
	if c.floor.ok && retained == c.floor.retainedAt && now.Sub(c.floor.at) < headroomRefreshEvery && !now.Before(c.floor.at) {
		estFree := c.floor.free - max(used-c.floor.usedAt, 0)
		req, rerr := sumSizes(c.floor.floor, unwritten, pending, additional)
		if rerr == nil && estFree >= req {
			free = estFree
			floor = c.floor.floor
			cached = true
		}
	}
	if !cached {
		var err error
		size, free, err = c.diskFn()
		if err != nil {
			return 0, 0, err
		}
		if size <= 0 || free < 0 {
			return 0, 0, errors.New("cannot measure media cache volume")
		}
		floor = MinFreeBytes(size)
	}
	required, err := sumSizes(floor, unwritten, pending, additional)
	if err != nil {
		return 0, 0, err
	}
	headroom := free - (required - additional)
	capacity := max(used+min(headroom, math.MaxInt64-used), 0)
	if !cached {
		c.floor = floorStat{ok: true, at: now, cap: capacity, floor: floor, free: free, usedAt: used, retainedAt: retained}
	} else {
		c.floor.cap = capacity
	}
	c.mu.Lock()
	c.floorEnabled = true
	c.floorOKSnap = true
	c.floorCapSnap = capacity
	if !cached {
		c.floorAtNs = now.UnixNano()
	}
	c.mu.Unlock()
	return max(required-free, 0), max(free-required, 0), nil
}

func sumSizes(values ...int64) (int64, error) {
	var total int64
	for _, n := range values {
		if n < 0 || n > math.MaxInt64-total {
			return 0, ErrTooLarge
		}
		total += n
	}
	return total, nil
}

func (c *Cache) segmentAllocation() (used, pending int64, err error) {
	if c.shared == nil {
		c.mu.Lock()
		defer c.mu.Unlock()
		return c.used, c.segmentReserved, nil
	}
	s := c.shared
	if err = s.checkMount(); err != nil {
		return
	}
	ctx, cancel := s.context()
	defer cancel()
	err = s.db.QueryRowContext(ctx, `SELECT used, (SELECT COALESCE(SUM(bytes),0) FROM segcache_writes WHERE expires>`+s.nowSQL()+`) FROM segcache_pool WHERE id=1`).Scan(&used, &pending)
	return
}

func (c *Cache) reserveSegment(files FilePool, size int64) (string, error) {
	now := c.now()
	c.mu.Lock()
	if !c.closed && c.policy.Mode != "off" && (c.shared != nil || c.indexed.Load()) &&
		size > 0 && size <= c.batchBytes && now.Sub(c.batchAt) < headroomRefreshEvery && !now.Before(c.batchAt) {
		c.batchBytes -= size
		lease := c.batchID
		if c.shared == nil {
			c.segmentReserved += size
		}
		c.mu.Unlock()
		return lease, nil
	}
	c.mu.Unlock()

	var lease string
	err := files.WithAllocation(func(accounted, unwritten int64, candidates FileCandidates) error {
		headroom, err := c.makeRoom(accounted, unwritten, size, candidates, false)
		if err != nil {
			return err
		}
		var extra int64
		if size > 0 && unwritten == 0 && headroom >= 7*size {
			extra = min(max(int64(segmentBatchMaxBytes)-size, 0), headroom/2)
		}
		at := c.now()
		if c.shared == nil {
			c.mu.Lock()
			c.segmentReserved += size
			c.batchID = ""
			c.batchBytes = extra
			c.batchAt = at
			c.mu.Unlock()
			return nil
		}
		s := c.shared
		lease = randomName()
		ctx, cancel := s.context()
		defer cancel()
		if _, err := s.db.ExecContext(ctx, `INSERT INTO segcache_writes(id,bytes,expires) VALUES(?,?,`+s.nowSQL()+`+600)`, lease, size+extra); err != nil {
			return err
		}
		c.mu.Lock()
		c.batchID = lease
		c.batchBytes = extra
		c.batchAt = at
		c.mu.Unlock()
		if extra > 0 {
			// Hand unused batch headroom back to the pool as soon as the batch
			// stops being usable instead of holding it until the row expires.
			id := lease
			time.AfterFunc(headroomRefreshEvery, func() { c.releaseBatchIf(id) })
		}
		return nil
	})
	return lease, err
}

func (c *Cache) endReservation(lease string, size int64) {
	if c.shared == nil {
		c.mu.Lock()
		c.segmentReserved -= size
		c.mu.Unlock()
		return
	}
	s := c.shared
	ctx, cancel := s.context()
	defer cancel()
	_, err := s.db.ExecContext(ctx, `UPDATE segcache_writes SET bytes=bytes-? WHERE id=? AND bytes>=?`, size, lease, size)
	s.log(err)
	_, err = s.db.ExecContext(ctx, `DELETE FROM segcache_writes WHERE id=? AND bytes<=0`, lease)
	s.log(err)
}
