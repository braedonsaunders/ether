// Package segcache is Ether's on-disk cache of decoded Usenet segments. It
// is a disk budget with segment-granular LRU eviction, pinning and a
// protection callback, not a library: the stream engine stores segments here
// so rewatches, seeks and the next episode are instant.
//
// Concurrency model: a single mutex guards the in-memory index (O(1) map
// lookups plus an intrusive LRU list); segment file IO happens outside it.
// Per-key striped RW locks order the file-level operations on one key: Get
// reads under the read lock, while the rename of a Put and the unlink of an
// eviction/Remove run under the write lock and re-check the index, so an
// eviction never deletes a newer file put concurrently and a reader never
// observes a file being replaced underneath it.
//
// A single background worker starts evicting segments at
// the 95% high watermark and evicts down to the 90% low watermark. If writers
// outrun the worker past 110% of budget, Put drops the oldest unprotected
// entries from the in-memory index and queues their files for the background
// worker to unlink.
//
// With a file pool attached, writes reserve space against all retained media
// before IO. Pinned releases and active file readers survive pool pressure.
// A segment larger than the whole budget is rejected by Put with ErrTooLarge.
package segcache

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/maphash"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

const (
	numStripes         = 256
	defaultTouchEvery  = time.Minute
	defaultProtectTTL  = 5 * time.Second
	overBudgetLogEvery = time.Minute

	evictHighPct = 95  // background worker starts evicting at 95% of budget
	evictLowPct  = 90  // background worker evicts down to 90% of budget
	evictHardPct = 110 // Put drops oldest entries in-memory above 110% of budget
)

var (
	// ErrTooLarge is returned by Put for a segment bigger than the whole budget.
	ErrTooLarge = errors.New("segcache: segment larger than cache budget")
	// ErrClosed is returned by mutating calls after Close.
	ErrClosed = errors.New("segcache: cache closed")
)

type protSnapshot struct {
	set map[string]bool
	at  time.Time
	gen uint64
}

// Cache implements core.SegmentCache.
type Cache struct {
	// Disk-usage and live-protection callbacks may query Usage while the
	// file store's allocation lock is held.
	poolUsage       atomic.Pointer[int64]
	files           FilePool   // protected by mu
	segmentReserved int64      // local segments still being written
	batchID         string     // shared segcache_writes row id for the active batch
	batchBytes      int64      // pre-verified segment headroom remaining in the current batch
	batchAt         time.Time  // when batchBytes was computed
	indexMu         sync.Mutex // serializes initial local disk accounting
	indexed         atomic.Bool
	ownerMu         sync.RWMutex // orders owner clearing against writes and new pins
	blockCheck      func(string) bool

	shared  *sharedCache
	dir     string
	dataDir string
	logger  *slog.Logger
	seed    maphash.Seed
	stripes [numStripes]sync.RWMutex

	mu                 sync.Mutex // guards everything below up to saveMu
	closed             bool
	budget             int64
	used               int64
	segCount           int
	rels               map[string]*release
	pins               map[string]map[string]struct{}
	lru                lruList  // evictable candidates, head = most recent
	parked             lruList  // entries found pinned/protected during eviction
	parkDirty          bool     // protection may have shrunk; merge parked back into lru
	pendingUnlinks     []*entry // entries dropped in Put awaiting background unlink
	pendingUnlinkBytes int64    // bytes in pendingUnlinks not yet unlinked from disk
	floorEnabled       bool
	floorOKSnap        bool
	floorCapSnap       int64
	floorAtNs          int64
	beforeUnlink       func() // tests: runs between victim selection and file unlinks
	lastOverLog        time.Time
	policy             Policy
	metrics            Metrics

	saveMu sync.Mutex // serialises pins.json writes

	// pinNote asks the writer to publish the current pin set. The play path
	// updates memory and returns; the file catches up, and Close publishes
	// whatever memory holds so a restart still sees the last set.
	pinNote chan struct{}
	pinStop chan struct{}
	pinDone chan struct{}
	pinOnce sync.Once

	// Background eviction worker: signaled non-blockingly by Put when usage
	// reaches the high watermark (or exceeds the hard cap / floor).
	evictNote      chan struct{}
	evictStop      chan struct{}
	evictDone      chan struct{}
	evictStartOnce sync.Once
	evictStopOnce  sync.Once
	evictReq       atomic.Uint64
	evictWaitMu    sync.Mutex
	evictWaitCond  *sync.Cond
	evictDoneSeq   uint64

	expMu      sync.Mutex
	expCond    *sync.Cond
	expPending map[string][]byte
	expWriting bool

	evictMu sync.Mutex // serialises passes that free space; taken before floorMu
	floorMu sync.Mutex // guards diskFn and floor; taken before mu
	diskFn  DiskUsageFunc
	floor   floorStat

	protMu     sync.Mutex
	protFn     func() map[string]bool
	protSet    map[string]bool
	protAt     time.Time
	protValid  bool
	protGen    atomic.Uint64
	protShrank atomic.Bool
	hasProtFn  atomic.Bool
	protSnap   atomic.Pointer[protSnapshot]

	// Tunables, overridden by tests.
	now        func() time.Time
	touchEvery time.Duration
	protectTTL time.Duration

	ev cacheEventState
}

var _ core.SegmentCache = (*Cache)(nil)

// release is the index record of one release directory. id and dir are
// immutable; the rest is guarded by Cache.mu. Records are never deleted while
// the cache is open (an emptied release keeps its directory and metadata).
type release struct {
	id         string
	dir        string
	segs       map[int]*entry
	head, tail *entry
	bytes      int64
	lastUsed   int64
	deleting   map[int]int // count of in-flight unlinks per key; never rediscover before unlink completes
	metaOK     bool        // directory and release.json exist on disk
	// metaMu serializes writing release.json: concurrent Puts for a new
	// release would otherwise race their atomic renames onto the same
	// file, which Windows refuses ("Access is denied").
	metaMu     sync.Mutex
	experience experience
}

// ReleaseUsage describes one release in the cache, for the admin UI.
type ReleaseUsage struct {
	ReleaseID  string
	Bytes      int64
	Segments   int
	LastUsed   time.Time
	Pins       []string
	Title      string
	Size       int64
	Plays      int
	LastPlayed time.Time

	// Parsed provenance, rendered into Reason once retention is known.
	file      string
	source    string
	indexer   string
	origin    string
	ratingKey core.RatingKey
	cachedAt  time.Time
	causes    []core.CacheCause

	Reason *ReleaseReason `json:"reason,omitempty"`
}

// Open opens (creating) a cache rooted at dir with a byte budget (<=0 means
// unlimited). Existing entries are discovered on access; Run accounts for the
// remaining disk contents after listening. logger may be nil (slog.Default()).
func Open(dir string, budget int64, logger *slog.Logger) (*Cache, error) {
	return openLocal(dir, budget, logger, time.Now)
}

func openLocal(dir string, budget int64, logger *slog.Logger, now func() time.Time) (*Cache, error) {
	if logger == nil {
		logger = slog.Default()
	}
	if dir == "" {
		return nil, errors.New("segcache: open: empty directory")
	}
	c := &Cache{
		dir:        dir,
		dataDir:    filepath.Join(dir, dataDirName),
		logger:     logger.With("component", "segcache"),
		seed:       maphash.MakeSeed(),
		budget:     max(budget, 0),
		rels:       make(map[string]*release),
		pins:       make(map[string]map[string]struct{}),
		now:        now,
		touchEvery: defaultTouchEvery,
		protectTTL: defaultProtectTTL,
		pinNote:    make(chan struct{}, 1),
		pinStop:    make(chan struct{}),
		pinDone:    make(chan struct{}),
	}
	if err := os.MkdirAll(c.dataDir, 0o755); err != nil {
		return nil, fmt.Errorf("segcache: open %s: %w", dir, err)
	}
	if err := c.loadPins(); err != nil {
		return nil, err
	}

	c.expCond = sync.NewCond(&c.expMu)
	c.initEvictWorker(true)
	go c.writePinsLoop()
	used, b := c.Usage()
	c.logger.Info("segment cache opened", "dir", dir, "segments", c.segCountSafe(),
		"used", used, "budget", b)
	return c, nil
}

func (c *Cache) segCountSafe() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.segCount
}

// Get returns the cached payload (a fresh buffer owned by the caller) and marks it as
// recently used. A missing, vanished or corrupt file is a miss.
func (c *Cache) Get(releaseID string, segment int) ([]byte, bool) {
	if c.shared != nil {
		return c.shared.get(releaseID, segment)
	}
	st := c.stripe(releaseID, segment)
	st.RLock()
	defer st.RUnlock()

	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, false
	}
	r := c.discoverLocked(releaseID, segment)
	if r == nil {
		c.metrics.Misses++
		c.mu.Unlock()
		return nil, false
	}
	e := r.segs[segment]
	if e == nil {
		c.metrics.Misses++
		c.mu.Unlock()
		return nil, false
	}
	size := e.size
	c.mu.Unlock()

	path := filepath.Join(r.dir, segName(segment))
	data, err := readSegment(path, size)
	if err != nil {
		// Holding the stripe read lock excludes our own renames/unlinks of this
		// key, so the file genuinely vanished or was damaged externally.
		c.mu.Lock()
		c.metrics.Misses++
		dropped := r.segs[segment] == e
		if dropped {
			c.dropLocked(e)
		}
		c.mu.Unlock()
		if errors.Is(err, fs.ErrNotExist) {
			c.logger.Warn("cached segment vanished from disk", "release", releaseID, "segment", segment)
		} else {
			c.logger.Warn("dropping unreadable cached segment", "release", releaseID,
				"segment", segment, "err", err)
			if rerr := os.Remove(path); rerr != nil && !errors.Is(rerr, fs.ErrNotExist) {
				c.logger.Warn("remove corrupt segment", "path", path, "err", rerr)
			}
		}
		if dropped {
			c.mu.Lock()
			c.finishDeletingLocked(r, segment)
			c.mu.Unlock()
		}
		return nil, false
	}

	now := c.now()
	nowNs := now.UnixNano()
	touch := false
	c.mu.Lock()
	c.metrics.Hits++
	c.metrics.ServedBytes += int64(len(data))
	if r.segs[segment] == e { // may have been evicted while we read; data is still valid
		c.listOf(e).remove(e)
		e.parked = false
		c.lru.pushFront(e)
		r.removeEntry(e)
		r.pushEntry(e)
		e.lastUsed = nowNs
		r.lastUsed = max(r.lastUsed, nowNs)
		if time.Duration(nowNs-e.touched) >= c.touchEvery {
			e.touched = nowNs
			touch = true
		}
	}
	c.mu.Unlock()
	if touch {
		if err := os.Chtimes(path, now, now); err != nil && !errors.Is(err, fs.ErrNotExist) {
			c.logger.Debug("touch segment", "path", path, "err", err)
		}
	}
	return data, true
}

// Peek returns the first n payload bytes of a cached segment and its
// payload size, reading only the header and that prefix: no checksum, no
// recency update, no mtime touch. It answers "is this block still on disk,
// and whose is it" for preload verification, which used Get and so read
// and checksummed every cached block of every shelved title on each pass
// (gigabytes of garbage an hour on an idle server). A damaged payload is
// still caught by Get when the block is actually read.
func (c *Cache) Peek(releaseID string, segment, n int) ([]byte, int64, bool) {
	if c.shared != nil {
		data, ok := c.shared.get(releaseID, segment)
		if !ok {
			return nil, 0, false
		}
		return data[:min(n, len(data))], int64(len(data)), true
	}
	st := c.stripe(releaseID, segment)
	st.RLock()
	defer st.RUnlock()
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, 0, false
	}
	r := c.discoverLocked(releaseID, segment)
	var e *entry
	if r != nil {
		e = r.segs[segment]
	}
	if e == nil {
		c.mu.Unlock()
		return nil, 0, false
	}
	size, dir := e.size, r.dir
	c.mu.Unlock()
	prefix, err := peekSegment(filepath.Join(dir, segName(segment)), size, n)
	if err != nil {
		c.mu.Lock()
		dropped := r.segs[segment] == e
		if dropped {
			c.dropLocked(e)
		}
		c.mu.Unlock()
		_ = os.Remove(filepath.Join(dir, segName(segment)))
		if dropped {
			c.mu.Lock()
			c.finishDeletingLocked(r, segment)
			c.mu.Unlock()
		}
		return nil, 0, false
	}
	return prefix, size, true
}

// Has reports whether the segment exists, discovering one disk entry if needed.
func (c *Cache) Has(releaseID string, segment int) bool {
	if c.shared != nil {
		return c.shared.has(releaseID, segment)
	}
	st := c.stripe(releaseID, segment)
	st.RLock()
	defer st.RUnlock()
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return false
	}
	r := c.discoverLocked(releaseID, segment)
	return r != nil && r.segs[segment] != nil
}

// Put stores (or replaces) a segment crash-safely (temp file + rename) and
// signals the background eviction worker when usage reaches the high watermark.
// A payload larger than the whole budget is rejected with ErrTooLarge and
// nothing is stored. With plugin media attached, admission may reclaim eligible
// media before writing the segment.
func (c *Cache) Put(releaseID string, segment int, data []byte) error {
	c.ownerMu.RLock()
	defer c.ownerMu.RUnlock()
	if c.blockCheck != nil && c.blockCheck(releaseID) {
		return nil // playback may continue without retaining blocked bytes
	}
	if err := validKey(releaseID, segment); err != nil {
		return err
	}
	c.mu.Lock()
	pool, off, closed := c.files, c.policy.Mode == "off", c.closed
	c.mu.Unlock()
	if closed {
		return ErrClosed
	}
	if off {
		return nil
	}
	lease := ""
	if pool != nil {
		var err error
		lease, err = c.reserveSegment(pool, int64(len(data)))
		if err != nil {
			return err
		}
	}
	if c.shared != nil {
		return c.shared.put(releaseID, segment, data, lease)
	}
	reserved := pool != nil
	if reserved {
		defer func() {
			if reserved {
				c.endReservation("", int64(len(data)))
			}
		}()
	}
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return ErrClosed
	}
	if c.policy.Mode == "off" {
		c.mu.Unlock()
		return nil
	}
	if c.budget > 0 && int64(len(data)) > c.budget {
		b := c.budget
		c.mu.Unlock()
		return fmt.Errorf("segcache: put %q segment %d (%d bytes, budget %d): %w",
			releaseID, segment, len(data), b, ErrTooLarge)
	}
	r := c.releaseLocked(releaseID)
	metaOK := r.metaOK
	c.mu.Unlock()

	if !metaOK {
		if err := c.ensureReleaseDir(r, false); err != nil {
			return fmt.Errorf("segcache: put %q segment %d: %w", releaseID, segment, err)
		}
	}
	tmp, err := writeSegmentTemp(r.dir, segment, data)
	if errors.Is(err, fs.ErrNotExist) {
		// The release directory was removed behind our back; recreate it once.
		if err = c.ensureReleaseDir(r, true); err == nil {
			tmp, err = writeSegmentTemp(r.dir, segment, data)
		}
	}
	if err != nil {
		return fmt.Errorf("segcache: put %q segment %d: write: %w", releaseID, segment, err)
	}

	final := filepath.Join(r.dir, segName(segment))
	st := c.stripe(releaseID, segment)
	st.Lock()
	if err := os.Rename(tmp, final); err != nil {
		st.Unlock()
		_ = os.Remove(tmp)
		return fmt.Errorf("segcache: put %q segment %d: rename: %w", releaseID, segment, err)
	}
	nowNs := c.now().UnixNano()
	var pending pendingCacheEvents
	c.mu.Lock()
	c.insertLocked(r, segment, int64(len(data)), nowNs)
	if reserved {
		c.segmentReserved -= int64(len(data))
		reserved = false
	}
	c.checkBudgetTransitionLocked(&pending)
	overHard := c.overHardBudgetLocked()
	wake := overHard || c.shouldWakeEvictLocked(nowNs)
	c.mu.Unlock()
	st.Unlock()
	c.publishPendingEvents(pending)

	if overHard && pool == nil {
		c.dropOverHardBudget()
	}
	if wake {
		c.signalEvict()
	}
	return nil
}

// Pin protects every segment of a release from eviction while it has at
// least one pin reason. Pins are persisted to pins.json. If persisting fails
// the pin still applies in memory and the error is returned.
func (c *Cache) Pin(releaseID string, reason string) error {
	c.ownerMu.RLock()
	defer c.ownerMu.RUnlock()
	if c.shared != nil {
		return c.shared.pin(releaseID, reason, true)
	}
	if releaseID == "" || reason == "" {
		return errors.New("segcache: pin: empty release id or reason")
	}
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return ErrClosed
	}
	set := c.pins[releaseID]
	if set == nil {
		set = make(map[string]struct{})
		c.pins[releaseID] = set
	}
	if _, ok := set[reason]; ok {
		c.mu.Unlock()
		return nil
	}
	set[reason] = struct{}{}
	c.mu.Unlock()
	c.notePins()
	return nil
}

// Unpin removes one pin reason. When the last reason goes the release becomes
// evictable again and the cache is evicted down to budget immediately.
func (c *Cache) Unpin(releaseID string, reason string) error {
	if c.shared != nil {
		return c.shared.pin(releaseID, reason, false)
	}
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return ErrClosed
	}
	set := c.pins[releaseID]
	if _, ok := set[reason]; !ok {
		c.mu.Unlock()
		return nil
	}
	delete(set, reason)
	if len(set) == 0 {
		delete(c.pins, releaseID)
		c.parkDirty = true
	}
	c.mu.Unlock()
	c.notePins()
	c.evict()
	return nil
}

// Pins returns a copy: releaseID -> sorted pin reasons.
func (c *Cache) Pins() map[string][]string {
	if c.shared != nil {
		return c.shared.pins()
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.pinsSnapshotLocked()
}

// Usage returns the payload bytes stored and the budget (0 = unlimited).
func (c *Cache) Usage() (used, budget int64) {
	c.mu.Lock()
	files := c.files
	used, budget = c.used, c.budget
	c.mu.Unlock()
	if c.shared != nil {
		used, budget = c.shared.usage()
	}
	if files != nil {
		if retained := c.poolUsage.Load(); retained != nil {
			return used + *retained, budget
		}
		retained, _, err := files.FileUsage()
		if err == nil {
			used += retained
		}
	}
	return used, budget
}

// UsageLimit is the most the cache may hold right now: the budget, or the
// free-space floor's last measured cap when that is lower. -1 means
// unbounded; 0 means the policy retains no media bytes (mode off).
func (c *Cache) UsageLimit() int64 {
	c.floorMu.Lock()
	floorCap, floorOK := c.floor.cap, c.floor.ok
	c.floorMu.Unlock()
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.policy.Mode == "off" {
		return 0
	}
	return usageLimit(c.budget, floorCap, floorOK)
}

// ReleaseBytes reports how many bytes of a release are currently cached.
func (c *Cache) ReleaseBytes(releaseID string) int64 {
	if c.shared != nil {
		return c.shared.releaseBytes(releaseID)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if r := c.rels[releaseID]; r != nil {
		return r.bytes
	}
	return 0
}

// SetBudget changes the budget (<=0 = unlimited) and evicts immediately if
// the cache is now over it.
func (c *Cache) SetBudget(budget int64) {
	c.releaseBatch()
	var pending pendingCacheEvents
	c.mu.Lock()
	c.budget = max(budget, 0)
	c.checkBudgetTransitionLocked(&pending)
	c.mu.Unlock()
	c.publishPendingEvents(pending)
	c.evict()
}

// Protect registers a callback returning release IDs that must not be
// evicted right now. It is only called when an eviction is needed and its
// result is cached for a few seconds. A nil fn clears it. fn must not call
// Protect.
func (c *Cache) Protect(fn func() map[string]bool) {
	c.protMu.Lock()
	c.protFn = fn
	c.protSet = nil
	c.protValid = false
	c.protGen.Add(1)
	c.hasProtFn.Store(fn != nil)
	c.protSnap.Store(nil)
	c.protMu.Unlock()
	c.mu.Lock()
	c.parkDirty = true
	c.mu.Unlock()
	c.evict()
}

// Remove drops all segments of a release from the index and disk. Pins are
// kept.
func (c *Cache) Remove(releaseID string) error {
	if c.shared != nil {
		return c.shared.remove(releaseID)
	}
	c.evictMu.Lock() // a floor measurement must not see these bytes half-freed
	defer c.evictMu.Unlock()
	c.drainPendingUnlinks()
	if validKey(releaseID, 0) != nil {
		return nil
	}
	dir := filepath.Join(c.dataDir, releaseDirRel(releaseID))
	if entries, err := os.ReadDir(dir); err == nil {
		for _, de := range entries {
			seg, ok := parseSegName(de.Name())
			if !ok || de.IsDir() {
				continue
			}
			st := c.stripe(releaseID, seg)
			st.RLock()
			c.mu.Lock()
			c.discoverLocked(releaseID, seg)
			c.mu.Unlock()
			st.RUnlock()
		}
	}

	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return ErrClosed
	}
	r := c.rels[releaseID]
	if r == nil {
		c.mu.Unlock()
		return nil
	}
	victims := make([]*entry, 0, len(r.segs))
	for _, e := range r.segs {
		victims = append(victims, e)
	}
	for _, e := range victims {
		c.dropLocked(e)
	}
	var pending pendingCacheEvents
	c.checkBudgetTransitionLocked(&pending)
	c.mu.Unlock()
	c.publishPendingEvents(pending)
	if err := c.deleteFiles(victims, false); err != nil {
		return fmt.Errorf("segcache: remove %q: %w", releaseID, err)
	}
	return nil
}

// ReleaseDetail summarizes the cached segment spans for one release. It touches
// only that release's in-memory index (or indexed rows on a shared pool).
func (c *Cache) ReleaseDetail(releaseID string) (ReleaseDetail, bool) {
	if releaseID == "" {
		return ReleaseDetail{}, false
	}
	if c.shared != nil {
		return c.shared.releaseDetail(releaseID)
	}
	c.mu.Lock()
	r := c.rels[releaseID]
	if r == nil || (len(r.segs) == 0 && len(c.pins[releaseID]) == 0) {
		c.mu.Unlock()
		return ReleaseDetail{}, false
	}
	keys := make([]int, 0, len(r.segs))
	var earliestNs int64
	for seg, e := range r.segs {
		keys = append(keys, seg)
		ts := e.touched
		if ts <= 0 || (e.lastUsed > 0 && e.lastUsed < ts) {
			ts = e.lastUsed
		}
		if ts > 0 && (earliestNs == 0 || ts < earliestNs) {
			earliestNs = ts
		}
	}
	bytes, size := r.bytes, r.experience.Size
	first := earliestTime(r.experience.CachedAt, r.experience.Causes)
	c.mu.Unlock()
	if earliestNs > 0 {
		t := time.Unix(0, earliestNs)
		if first.IsZero() || t.Before(first) {
			first = t
		}
	}
	slices.Sort(keys)
	return buildReleaseDetail(releaseID, keys, bytes, size, first), true
}

// Releases lists every release that has cached segments or pins, most
// recently used first.
func (c *Cache) Releases() []ReleaseUsage {
	if c.shared != nil {
		return c.shared.releases()
	}
	c.mu.Lock()
	out := make([]ReleaseUsage, 0, len(c.rels))
	seen := make(map[string]bool, len(c.rels))
	for id, r := range c.rels {
		if len(r.segs) == 0 && len(c.pins[id]) == 0 {
			continue
		}
		seen[id] = true
		u := ReleaseUsage{Title: r.experience.Title, Size: r.experience.Size, Plays: r.experience.Plays, LastPlayed: r.experience.LastPlayed, ReleaseID: id, Bytes: r.bytes, Segments: len(r.segs), Pins: sortedReasons(c.pins[id]), file: r.experience.File, source: r.experience.Source, indexer: r.experience.Indexer, origin: r.experience.Origin, ratingKey: r.experience.RatingKey, cachedAt: r.experience.CachedAt, causes: append([]core.CacheCause(nil), r.experience.Causes...)}
		if r.lastUsed > 0 {
			u.LastUsed = time.Unix(0, r.lastUsed)
		}
		u.Reason = reasonFor(&u, c.policy, c.now())
		out = append(out, u)
	}
	for id, set := range c.pins {
		if !seen[id] {
			u := ReleaseUsage{ReleaseID: id, Pins: sortedReasons(set)}
			u.Reason = reasonFor(&u, c.policy, c.now())
			out = append(out, u)
		}
	}
	c.mu.Unlock()
	sort.Slice(out, func(i, j int) bool {
		if !out[i].LastUsed.Equal(out[j].LastUsed) {
			return out[i].LastUsed.After(out[j].LastUsed)
		}
		return out[i].ReleaseID < out[j].ReleaseID
	})
	return out
}

// Close marks the cache closed, stops background workers and publishes the
// in-memory pin set. It is idempotent.
func (c *Cache) Close() error {
	c.mu.Lock()
	c.closed = true
	c.mu.Unlock()
	c.releaseBatch()
	if c.shared != nil {
		c.shared.unregisterPool()
	}
	if c.evictStop != nil {
		c.evictStopOnce.Do(func() {
			close(c.evictStop)
			c.startEvictWorker()
			<-c.evictDone
		})
	}
	if c.shared == nil {
		c.evictMu.Lock()
		c.drainPendingUnlinks()
		c.evictMu.Unlock()
	}
	if c.pinStop == nil {
		return nil
	}
	c.pinOnce.Do(func() {
		close(c.pinStop)
		<-c.pinDone
		_ = c.writePinsFile()
	})
	c.flushExperience()
	return nil
}

// ---------------------------------------------------------------------------
// internals

func scalePercent(n, pct int64) int64 {
	if n <= 0 {
		return 0
	}
	return (n/100)*pct + ((n%100)*pct)/100
}

func highWatermark(budget int64) int64 { return scalePercent(budget, evictHighPct) }
func lowWatermark(budget int64) int64  { return scalePercent(budget, evictLowPct) }
func hardWatermark(budget int64) int64 { return budget + scalePercent(budget, evictHardPct-100) }

func validKey(releaseID string, segment int) error {
	if releaseID == "" {
		return errors.New("segcache: empty release id")
	}
	if segment < 0 {
		return fmt.Errorf("segcache: negative segment %d", segment)
	}
	return nil
}

func (c *Cache) stripe(releaseID string, segment int) *sync.RWMutex {
	h := maphash.String(c.seed, releaseID) ^ (uint64(segment) * 0x9e3779b97f4a7c15)
	h ^= h >> 29
	return &c.stripes[h%numStripes]
}

func (c *Cache) releaseLocked(releaseID string) *release {
	r := c.rels[releaseID]
	if r == nil {
		r = &release{
			id:   releaseID,
			dir:  filepath.Join(c.dataDir, releaseDirRel(releaseID)),
			segs: make(map[int]*entry),
		}
		c.rels[releaseID] = r
	}
	return r
}

func (c *Cache) ensureReleaseDir(r *release, force bool) error {
	r.metaMu.Lock()
	defer r.metaMu.Unlock()
	if !force {
		c.mu.Lock()
		done := r.metaOK
		c.mu.Unlock()
		if done { // another Put wrote it while we waited
			return nil
		}
	}
	if err := writeReleaseMeta(r.dir, r.id); err != nil {
		return fmt.Errorf("create release dir: %w", err)
	}
	c.mu.Lock()
	r.metaOK = true
	c.mu.Unlock()
	return nil
}

func (c *Cache) listOf(e *entry) *lruList {
	if e.parked {
		return &c.parked
	}
	return &c.lru
}

func (c *Cache) insertLocked(r *release, seg int, size, nowNs int64) {
	e := r.segs[seg]
	if e != nil {
		c.used -= e.size
		r.bytes -= e.size
		c.listOf(e).remove(e)
		r.removeEntry(e)
	} else {
		e = &entry{rel: r, seg: seg}
		r.segs[seg] = e
		c.segCount++
	}
	e.size = size
	e.lastUsed = nowNs
	e.touched = nowNs
	e.parked = false
	c.used += size
	r.bytes += size
	r.lastUsed = max(r.lastUsed, nowNs)
	if nowNs > 0 {
		ts := time.Unix(0, nowNs)
		if r.experience.CachedAt.IsZero() || ts.Before(r.experience.CachedAt) {
			r.experience.CachedAt = ts
		}
	}
	c.lru.pushFront(e)
	r.pushEntry(e)
}

// dropLocked removes e from the index and accounting (not from disk).
func (c *Cache) dropLocked(e *entry) {
	if e.rel.deleting == nil {
		e.rel.deleting = make(map[int]int)
	}
	e.rel.deleting[e.seg]++
	c.listOf(e).remove(e)
	e.parked = false
	e.rel.removeEntry(e)
	delete(e.rel.segs, e.seg)
	c.used -= e.size
	e.rel.bytes -= e.size
	c.segCount--
}

func (c *Cache) finishDeletingLocked(r *release, seg int) {
	if n := r.deleting[seg]; n <= 1 {
		delete(r.deleting, seg)
	} else {
		r.deleting[seg] = n - 1
	}
}

func (c *Cache) queueUnlinkLocked(e *entry) {
	c.pendingUnlinks = append(c.pendingUnlinks, e)
	c.pendingUnlinkBytes += e.size
}

func (c *Cache) overBudgetLocked() bool     { return c.budget > 0 && c.used > c.budget }
func (c *Cache) overHardBudgetLocked() bool { return c.budget > 0 && c.used > hardWatermark(c.budget) }

// overLimitLocked reports whether usage is above limit (see limitLocked).
func (c *Cache) overLimitLocked(limit int64) bool { return limit >= 0 && c.used > limit }

// limitLocked is the usage eviction aims for: the budget, or the floor's cap
// when that is lower. -1 means no limit.
func (c *Cache) limitLocked(floorCap int64, floorOK bool) int64 {
	return usageLimit(c.budget, floorCap, floorOK)
}

func usageLimit(budget, floorCap int64, floorOK bool) int64 {
	limit := int64(-1)
	if budget > 0 {
		limit = budget
	}
	if floorOK && (limit < 0 || floorCap < limit) {
		limit = floorCap
	}
	return limit
}

func (c *Cache) pinnedLocked(releaseID string) bool { return len(c.pins[releaseID]) > 0 }

func (c *Cache) initEvictWorker(eager bool) {
	c.evictNote = make(chan struct{}, 1)
	c.evictStop = make(chan struct{})
	c.evictDone = make(chan struct{})
	c.evictWaitCond = sync.NewCond(&c.evictWaitMu)
	if eager {
		c.startEvictWorker()
	}
}

func (c *Cache) startEvictWorker() {
	if c.evictNote == nil {
		return
	}
	c.evictStartOnce.Do(func() {
		select {
		case <-c.evictStop:
			close(c.evictDone)
			return
		default:
		}
		go c.evictLoop()
	})
}

func (c *Cache) signalEvict() {
	if c.evictNote == nil {
		return
	}
	c.startEvictWorker()
	c.evictReq.Add(1)
	select {
	case c.evictNote <- struct{}{}:
	default:
	}
}

// waitEvict blocks until the background eviction worker has completed a pass
// covering all signals sent before waitEvict was called (used by tests).
func (c *Cache) waitEvict() {
	if c.evictNote == nil {
		return
	}
	target := c.evictReq.Load()
	if target == 0 {
		return
	}
	c.startEvictWorker()
	select {
	case c.evictNote <- struct{}{}:
	default:
	}
	c.evictWaitMu.Lock()
	defer c.evictWaitMu.Unlock()
	for c.evictDoneSeq < target {
		select {
		case <-c.evictDone:
			return
		default:
		}
		c.evictWaitCond.Wait()
	}
}

func (c *Cache) evictLoop() {
	defer func() {
		close(c.evictDone)
		c.evictWaitMu.Lock()
		if c.evictWaitCond != nil {
			c.evictWaitCond.Broadcast()
		}
		c.evictWaitMu.Unlock()
	}()
	for {
		select {
		case <-c.evictStop:
			return
		case <-c.evictNote:
		}
		for {
			c.mu.Lock()
			closed := c.closed
			c.mu.Unlock()
			if closed {
				return
			}
			seq := c.evictReq.Load()
			c.evictWorkerPass()
			c.evictWaitMu.Lock()
			c.evictDoneSeq = seq
			if c.evictWaitCond != nil {
				c.evictWaitCond.Broadcast()
			}
			c.evictWaitMu.Unlock()
			if c.evictReq.Load() == seq {
				break
			}
		}
	}
}

func (c *Cache) evictWorkerPass() {
	if c.trimPoolWatermarked(true) {
		return
	}
	if c.shared != nil {
		c.shared.sweepWatermarked()
		return
	}
	c.evictLocal(true)
}

func (c *Cache) shouldWakeEvictLocked(nowNs int64) bool {
	if c.budget > 0 {
		high := highWatermark(c.budget)
		low := lowWatermark(c.budget)
		if c.used >= high && c.used > low && !(c.used <= c.budget && c.segCount <= 1) {
			return true
		}
	}
	if c.floorEnabled {
		if !c.floorOKSnap || nowNs-c.floorAtNs >= int64(floorStatEvery) {
			return true
		}
		if c.used > c.floorCapSnap {
			return true
		}
	}
	return len(c.pendingUnlinks) > 0
}

func (c *Cache) shouldWakeSharedEvict(used int64) bool {
	nowNs := c.now().UnixNano()
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return false
	}
	if c.budget > 0 {
		high := highWatermark(c.budget)
		low := lowWatermark(c.budget)
		if used >= high && used > low {
			return true
		}
	}
	if c.floorEnabled {
		if !c.floorOKSnap || nowNs-c.floorAtNs >= int64(floorStatEvery) {
			return true
		}
		if used > c.floorCapSnap {
			return true
		}
	}
	return false
}

// dropOverHardBudget cheaply drops the oldest unprotected entries in memory
// when writers push usage past 110% of budget, queuing file unlinks for the
// background worker so Put never unlinks files or waits on evictMu.
func (c *Cache) dropOverHardBudget() {
	prot, gen, ok := c.tryProtected()
	if !ok {
		return
	}
	var pending pendingCacheEvents
	c.mu.Lock()
	if c.closed || !c.overHardBudgetLocked() || c.protGen.Load() != gen {
		c.mu.Unlock()
		return
	}
	if c.protShrank.Swap(false) {
		c.parkDirty = true
	}
	if c.parkDirty {
		c.unparkLocked()
		c.parkDirty = false
	}
	target := c.budget
	var evictedBytes int64
	evictedEntries := 0
	for c.used > target {
		e := c.victimLocked(prot)
		if e == nil {
			break
		}
		if c.pinnedLocked(e.rel.id) || prot[e.rel.id] {
			c.lru.remove(e)
			e.parked = true
			c.parked.pushFront(e)
			continue
		}
		c.metrics.EvictedBytes += e.size
		evictedBytes += e.size
		evictedEntries++
		c.dropLocked(e)
		c.queueUnlinkLocked(e)
	}
	c.recordEvictionLocked(evictedBytes, evictedEntries, &pending)
	c.checkBudgetTransitionLocked(&pending)
	c.mu.Unlock()
	c.publishPendingEvents(pending)
}

// drainPendingUnlinks unlinks any entries previously dropped in Put. Callers
// hold evictMu so floor measurements see consistent disk space.
func (c *Cache) drainPendingUnlinks() {
	for {
		c.mu.Lock()
		if len(c.pendingUnlinks) == 0 {
			c.mu.Unlock()
			return
		}
		batch := c.pendingUnlinks
		c.pendingUnlinks = nil
		c.mu.Unlock()
		if err := c.deleteFiles(batch, true); err != nil {
			c.logger.Warn("evict segments", "err", err)
		}
	}
}

func (c *Cache) evictThresholdsLocked(floorCap int64, floorOK bool, watermarked bool) (trigger bool, target, limit int64) {
	limit = c.limitLocked(floorCap, floorOK)
	if limit < 0 {
		return false, -1, -1
	}
	if !watermarked {
		return c.used > limit, limit, limit
	}
	target = limit
	if c.budget > 0 {
		high := highWatermark(c.budget)
		low := lowWatermark(c.budget)
		if low < target {
			target = low
		}
		if c.used >= high && !(c.used <= limit && c.segCount <= 1) {
			trigger = true
		}
	}
	if floorOK && c.used > floorCap {
		trigger = true
	}
	if c.used <= target {
		return false, target, limit
	}
	return trigger, target, limit
}

// evict brings usage down to the limit synchronously (for SetBudget, Unpin,
// Protect, SetDiskUsage, Sweep and indexDisk), skipping pinned and protected
// releases.
func (c *Cache) evict() {
	if c.trimPool() {
		return
	}
	if c.shared != nil {
		c.shared.sweep(false)
		return
	}
	c.evictLocal(false)
}

// evictLocal runs one eviction pass under evictMu. When watermarked is true
// (background worker), budget eviction starts at 95% and evicts down to 90%.
func (c *Cache) evictLocal(watermarked bool) { c.evictLocalTarget(watermarked, -1, time.Time{}) }
func (c *Cache) evictLocalTarget(watermarked bool, need int64, cutoff time.Time) (int64, error) {
	c.evictMu.Lock()
	defer c.evictMu.Unlock()
	c.drainPendingUnlinks()
	var floorCap int64
	var floorOK bool
	if need < 0 {
		floorCap, floorOK = c.floorCap(c.localUsed)
	}
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return 0, nil
	}
	trigger, _, _ := c.evictThresholdsLocked(floorCap, floorOK, watermarked)
	if !trigger && need < 0 {
		c.mu.Unlock()
		return 0, nil
	}
	c.mu.Unlock()

	prot, shrank := c.protected() // user callback; never under c.mu

	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return 0, nil
	}
	if shrank || c.protShrank.Swap(false) {
		c.parkDirty = true
	}
	if c.parkDirty {
		c.unparkLocked()
		c.parkDirty = false
	}
	trigger, target, limit := c.evictThresholdsLocked(floorCap, floorOK, watermarked)
	if need >= 0 {
		trigger = true
		target = max(c.used-need, 0)
	}
	var victims []*entry
	var evictedBytes int64
	if trigger {
		for c.used > target && !(watermarked && limit >= 0 && c.used <= limit && c.segCount <= 1) {
			e := c.victimLocked(prot)
			if e == nil {
				break
			}
			if c.pinnedLocked(e.rel.id) || prot[e.rel.id] {
				c.lru.remove(e)
				e.parked = true
				c.parked.pushFront(e)
				continue
			}
			if !cutoff.IsZero() && c.victimAgeLocked(e) > cutoff.UnixNano() {
				break
			}
			c.metrics.EvictedBytes += e.size
			evictedBytes += e.size
			c.dropLocked(e)
			victims = append(victims, e)
		}
	}
	var pending pendingCacheEvents
	c.recordEvictionLocked(evictedBytes, len(victims), &pending)
	c.checkBudgetTransitionLocked(&pending)
	if c.overLimitLocked(limit) {
		if now := c.now(); now.Sub(c.lastOverLog) >= overBudgetLogEvery {
			c.lastOverLog = now
			if c.overBudgetLocked() {
				c.logger.Warn("segment cache over budget: everything left is pinned or protected",
					"used", c.used, "budget", c.budget, "segments", c.segCount)
			} else {
				c.logger.Warn("segment cache volume below its free-space floor: everything left is pinned or protected",
					"used", c.used, "shortBytes", c.used-limit, "segments", c.segCount)
			}
		}
	}
	hook := c.beforeUnlink
	c.mu.Unlock()
	c.publishPendingEvents(pending)

	if hook != nil && len(victims) > 0 {
		hook()
	}
	err := c.deleteFiles(victims, false)
	if err != nil {
		c.logger.Warn("evict segments", "err", err)
	}
	c.drainPendingUnlinks()
	return evictedBytes, err
}

// unparkLocked merges the parked list back into the LRU list, ordered by
// lastUsed (both lists are already roughly in that order).
func (c *Cache) unparkLocked() {
	if c.parked.n == 0 {
		return
	}
	var merged lruList
	for c.lru.head != nil || c.parked.head != nil {
		a, b := c.lru.head, c.parked.head
		var pick *entry
		if b == nil || (a != nil && a.lastUsed >= b.lastUsed) {
			pick = a
			c.lru.remove(a)
		} else {
			pick = b
			c.parked.remove(b)
		}
		pick.parked = false
		merged.pushBack(pick)
	}
	c.lru = merged
}

// deleteFiles unlinks the files of entries already dropped from the index,
// unless a concurrent Put re-added the key meanwhile (then the file on disk
// is the new version and must stay).
func (c *Cache) deleteFiles(victims []*entry, fromPending bool) error {
	var errs []error
	for _, e := range victims {
		st := c.stripe(e.rel.id, e.seg)
		st.Lock()
		c.mu.Lock()
		_, readded := e.rel.segs[e.seg]
		c.mu.Unlock()
		if !readded {
			path := filepath.Join(e.rel.dir, segName(e.seg))
			if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
				errs = append(errs, err)
			}
		}
		c.mu.Lock()
		c.finishDeletingLocked(e.rel, e.seg)
		if fromPending {
			c.pendingUnlinkBytes -= e.size
		}
		c.mu.Unlock()
		st.Unlock()
	}
	return errors.Join(errs...)
}

// protected returns the current protected set (cached for protectTTL) and
// whether it shrank compared to the previous one.
func (c *Cache) protected() (map[string]bool, bool) {
	c.protMu.Lock()
	defer c.protMu.Unlock()
	return c.protectedLocked()
}

func (c *Cache) protectedLocked() (map[string]bool, bool) {
	if c.protFn == nil {
		return nil, false
	}
	gen := c.protGen.Load()
	now := c.now()
	if c.protValid && now.Sub(c.protAt) < c.protectTTL && !now.Before(c.protAt) {
		return c.protSet, false
	}
	raw := c.protFn()
	set := make(map[string]bool, len(raw))
	for id, ok := range raw {
		if ok {
			set[id] = true
		}
	}
	shrank := false
	if c.protValid {
		for id := range c.protSet {
			if !set[id] {
				shrank = true
				break
			}
		}
	}
	if shrank {
		c.protShrank.Store(true)
	}
	c.protSet, c.protAt, c.protValid = set, now, true
	c.protSnap.Store(&protSnapshot{set: set, at: now, gen: gen})
	return set, shrank
}

// tryProtected returns the protected set without blocking if another goroutine
// is currently inside the protection callback.
func (c *Cache) tryProtected() (set map[string]bool, gen uint64, ok bool) {
	if !c.hasProtFn.Load() {
		return nil, c.protGen.Load(), true
	}
	gen = c.protGen.Load()
	now := c.now()
	if snap := c.protSnap.Load(); snap != nil && snap.gen == gen && now.Sub(snap.at) < c.protectTTL && !now.Before(snap.at) {
		return snap.set, gen, true
	}
	if !c.protMu.TryLock() {
		return nil, 0, false
	}
	defer c.protMu.Unlock()
	set, _ = c.protectedLocked()
	return set, c.protGen.Load(), true
}

func sortedReasons(set map[string]struct{}) []string {
	if len(set) == 0 {
		return nil
	}
	out := make([]string, 0, len(set))
	for r := range set {
		out = append(out, r)
	}
	slices.Sort(out)
	return out
}

func (c *Cache) pinsSnapshotLocked() map[string][]string {
	out := make(map[string][]string, len(c.pins))
	for id, set := range c.pins {
		if len(set) > 0 {
			out[id] = sortedReasons(set)
		}
	}
	return out
}

func (c *Cache) notePins() {
	if c.pinNote == nil {
		_ = c.writePinsFile()
		return
	}
	select {
	case c.pinNote <- struct{}{}:
	default:
	}
}

func (c *Cache) writePinsLoop() {
	defer close(c.pinDone)
	for {
		select {
		case <-c.pinStop:
			return
		case <-c.pinNote:
			_ = c.writePinsFile()
		}
	}
}

func (c *Cache) writePinsFile() error {
	c.saveMu.Lock()
	defer c.saveMu.Unlock()
	c.mu.Lock()
	snap := c.pinsSnapshotLocked() // taken under saveMu so the last writer wins with the newest state
	c.mu.Unlock()
	b, err := json.MarshalIndent(pinsFile{Version: 1, Pins: snap}, "", "  ")
	if err != nil {
		return fmt.Errorf("segcache: encode pins: %w", err)
	}
	// Rename publishes the new pin set. Fsyncing this small manifest on every
	// play and stop dominated time-to-first-byte; a crash may observe the
	// previous set, and the next playback re-pins from the live session.
	if err := writeFileAtomic(filepath.Join(c.dir, pinsFileName), b, false); err != nil {
		return fmt.Errorf("segcache: save pins: %w", err)
	}
	return nil
}

// loadPins reads pins.json. A
// corrupt pins file is logged and ignored (the cache stays usable).
func (c *Cache) loadPins() error {
	path := filepath.Join(c.dir, pinsFileName)
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("segcache: read pins: %w", err)
	}
	var pf pinsFile
	if err := json.Unmarshal(b, &pf); err != nil {
		c.logger.Warn("ignoring corrupt pins file", "path", path, "err", err)
		return nil
	}
	for id, reasons := range pf.Pins {
		for _, r := range reasons {
			if id == "" || r == "" {
				continue
			}
			if c.pins[id] == nil {
				c.pins[id] = make(map[string]struct{})
			}
			c.pins[id][r] = struct{}{}
		}
	}
	return nil
}

// discoverLocked checks only the requested key. The caller holds mu and a
// stripe lock; no cache or library traversal is allowed on this path.
func (c *Cache) discoverLocked(id string, seg int) *release {
	if c.closed || validKey(id, seg) != nil {
		return nil
	}
	r := c.rels[id]
	if r != nil && (r.segs[seg] != nil || r.deleting[seg] > 0) {
		return r
	}
	dir := filepath.Join(c.dataDir, releaseDirRel(id))
	path := filepath.Join(dir, segName(seg))
	info, err := os.Stat(path)
	if err != nil {
		return r
	}
	stored, err := readReleaseMeta(dir)
	if err != nil || stored != id || info.Size() < headerSize {
		_ = os.Remove(path)
		return r
	}
	if _, err := peekSegment(path, info.Size()-headerSize, 0); err != nil {
		_ = os.Remove(path)
		return r
	}
	if r == nil {
		r = c.releaseLocked(id)
		if b, err := os.ReadFile(filepath.Join(dir, "experience.json")); err == nil {
			_ = json.Unmarshal(b, &r.experience)
		}
	}
	r.metaOK = true
	c.insertLocked(r, seg, info.Size()-headerSize, info.ModTime().UnixNano())
	return r
}

// indexDisk accounts for existing local segments once. Run starts it after
// listening. The first allocation may also need it to avoid spending space
// occupied before a restart. Cached reads continue to discover data on demand.
func (c *Cache) indexDisk(ctx context.Context) error {
	if c.indexed.Load() {
		return nil
	}
	c.indexMu.Lock()
	defer c.indexMu.Unlock()
	if c.indexed.Load() {
		return nil
	}
	var scanErr error
	var walk func(string, int)
	walk = func(dir string, depth int) {
		f, err := os.Open(dir)
		if err != nil {
			if !errors.Is(err, fs.ErrNotExist) {
				scanErr = err
			}
			return
		}
		defer f.Close()
		for {
			entries, err := f.ReadDir(32)
			for _, de := range entries {
				if ctx.Err() != nil {
					return
				}
				path := filepath.Join(dir, de.Name())
				if strings.HasSuffix(de.Name(), tmpExt) && !de.IsDir() {
					if info, err := de.Info(); err == nil && time.Since(info.ModTime()) > time.Hour {
						_ = os.Remove(path)
					}
					continue
				}
				if depth == -1 && de.Name() != dataDirName {
					continue
				}

				if depth < 2 && de.IsDir() {
					walk(path, depth+1)
					continue
				}
				if depth != 2 || de.IsDir() {
					continue
				}
				seg, ok := parseSegName(de.Name())
				if !ok {
					continue
				}
				id, e := readReleaseMeta(dir)
				if e != nil || filepath.Join(c.dataDir, releaseDirRel(id)) != dir {
					continue
				}
				st := c.stripe(id, seg)
				st.RLock()
				c.mu.Lock()
				c.discoverLocked(id, seg)
				c.mu.Unlock()
				st.RUnlock()
			}
			if ctx.Err() != nil {
				return
			}
			c.mu.Lock()
			pooled := c.files != nil
			c.mu.Unlock()
			if !pooled {
				c.evictLocal(false)
			}
			if err != nil {
				if err != io.EOF {
					scanErr = err
				}
				return
			}
		}
	}
	walk(c.dir, -1)
	if err := ctx.Err(); err != nil {
		return err
	}
	if scanErr != nil {
		return scanErr
	}
	c.mu.Lock()
	if c.files != nil && c.segCount > 1 {
		c.restoreRecencyLocked()
	}
	c.mu.Unlock()
	c.indexed.Store(true)
	return nil
}

// Directory enumeration order need not reflect the last read of a segment.
// Restore both LRU indexes before comparing cached files across a restart.
func (c *Cache) restoreRecencyLocked() {
	entries := make([]*entry, 0, c.segCount)
	for _, r := range c.rels {
		for _, e := range r.segs {
			entries = append(entries, e)
		}
		r.head, r.tail = nil, nil
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].lastUsed < entries[j].lastUsed })
	c.lru, c.parked = lruList{}, lruList{}
	for _, e := range entries {
		c.listOf(e).pushFront(e)
		e.rel.pushEntry(e)
	}
}

// SetBlockCheck prevents background writers from refilling owner-blocked releases.
func (c *Cache) SetBlockCheck(check func(string) bool) {
	c.ownerMu.Lock()
	defer c.ownerMu.Unlock()
	c.blockCheck = check
}

// ClearUnpinned refuses protected data on both backends and waits for local
// writers before removing bytes. Shared pins are also checked transactionally.
func (c *Cache) ClearUnpinned(id string) error {
	c.ownerMu.Lock()
	defer c.ownerMu.Unlock()
	if len(c.Pins()[id]) > 0 {
		return errors.New("release still pinned")
	}
	if protected, _ := c.protected(); protected[id] {
		return errors.New("release still in use")
	}
	if err := c.Remove(id); err != nil {
		return err
	}
	if c.ReleaseBytes(id) != 0 {
		return errors.New("cached bytes remain")
	}
	return nil
}
