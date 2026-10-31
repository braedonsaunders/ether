// Package stream is Ether's stream engine: it turns an NZB into a seekable
// core.MediaSource without writing the full file anywhere.
//
// Open parses the NZB, picks the main video (a plain post, or the file
// inside a store-mode RAR/7z set or a plain split), and builds a
// byte-offset index from the yEnc part headers so any byte range maps to
// exact articles. ReadAt fetches those articles in parallel through the
// core.ArticleFetcher, reads ahead of sequential readers, coalesces
// concurrent requests for the same article, keeps a bounded in-memory hot
// buffer and spills decoded segments to the core.SegmentCache.
//
// Open touches as few articles as possible: the first article of the video
// (plain posts), or the first article of the first, second and last
// volumes of an archive set, assuming the uniform volume layout that
// archivers produce. That assumption is checked against the archive's own
// totals at Open and verified per volume, lazily, before any byte of that
// volume is served; a mismatch rebuilds the layout from every volume's
// headers.
package stream

import (
	"container/list"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/nzb"
)

// Options tune the engine. Zero values take the defaults noted.
type Options struct {
	Repair config.Repair
	// HotBufferBytes bounds decoded segments kept in memory across all
	// sources (default 256 MiB).
	HotBufferBytes int64
	// ReadAheadMin is the initial read-ahead window of a sequential reader
	// (default 4 MiB); it grows with the bytes read up to ReadAheadMax
	// (default 64 MiB).
	ReadAheadMin int64
	ReadAheadMax int64
	// ReadAheadParallel caps concurrent read-ahead fetches per source
	// (default 8).
	ReadAheadParallel int
	// ReadParallel caps concurrent fetches serving one ReadAt (default 16).
	ReadParallel int
	// PrefetchParallel caps concurrent fetches of one Prefetch call
	// (default 4).
	PrefetchParallel int
	// HealthSamples is how many articles Check STATs (default 12, from
	// config Resolver.HealthSamples).
	HealthSamples int
	// HealthParallel caps concurrent STATs of one Check (default 16).
	HealthParallel int
	// OpenParallel caps concurrent header fetches while opening (default 16).
	OpenParallel int
	// ReadTimeout bounds one ReadAt (default 90 s); ReadAt has no context.
	ReadTimeout time.Duration
	// MaxReleases is how many parsed release indexes are kept for reuse
	// (default 32). Releases with open sources are never dropped.
	MaxReleases int
	// Retries is how often a failed article fetch (other than missing or
	// cancelled) is retried (default 2).
	Retries int
	// MaxProbes bounds articles fetched to identify obfuscated files
	// (default 64).
	MaxProbes int
}

// OptionsFromConfig derives options from the owner settings.
func OptionsFromConfig(cfg config.Config) Options {
	return Options{HealthSamples: cfg.Resolver.HealthSamples, Repair: cfg.Usenet.Repair}
}

func (o Options) withDefaults() Options {
	def := func(v *int64, d int64) {
		if *v <= 0 {
			*v = d
		}
	}
	defi := func(v *int, d int) {
		if *v <= 0 {
			*v = d
		}
	}
	def(&o.HotBufferBytes, 256<<20)
	def(&o.ReadAheadMin, 4<<20)
	def(&o.ReadAheadMax, 64<<20)
	if o.ReadAheadMax < o.ReadAheadMin {
		o.ReadAheadMax = o.ReadAheadMin
	}
	defi(&o.ReadAheadParallel, 8)
	defi(&o.ReadParallel, 16)
	defi(&o.PrefetchParallel, 4)
	defi(&o.HealthSamples, 12)
	defi(&o.HealthParallel, 16)
	defi(&o.OpenParallel, 16)
	defi(&o.MaxReleases, 32)
	defi(&o.MaxProbes, 64)
	if o.Retries < 0 {
		o.Retries = 0
	} else if o.Retries == 0 {
		o.Retries = 2
	}
	if o.ReadTimeout <= 0 {
		o.ReadTimeout = 90 * time.Second
	}
	return o
}

// Stats are engine counters for diagnostics.
type Stats struct {
	RepairLastReason   string
	RepairAttempts     int64
	RepairSuccesses    int64
	RepairFailures     int64
	RepairInputBytes   int64
	RepairSummaryBytes int64
	Fetches            int64 // articles fetched from Usenet
	FetchedBytes       int64 // decoded bytes fetched from Usenet
	HotHits            int64 // segments served from memory
	CacheHits          int64 // segments served from the disk cache
	Missing            int64 // articles missing on every provider
	Coalesced          int64 // requests that joined an in-flight fetch
	HotBytes           int64 // bytes currently in the hot buffer
	OpenReleases       int   // release indexes held
	ActiveSources      int64

	// PAR2 (see par2.go): index files parsed, unusable (fetched but not
	// parseable) and skipped as oversize; obfuscated files named from PAR2;
	// head probes of obfuscated files avoided thanks to it; first volumes
	// verified against it and found not matching.
	Par2Loaded      int64
	Par2Failed      int64
	Par2Skipped     int64
	Par2Named       int64
	Par2ProbesSaved int64
	Par2Verified    int64
	Par2Mismatches  int64
}

// hasCache is optionally implemented by a SegmentCache (segcache does) to
// test presence without reading the segment.
type hasCache interface {
	Has(releaseID string, segment int) bool
}

// Engine implements core.Engine and core.HealthChecker.
type Engine struct {
	repairRate         repairRate
	repairNotice       atomic.Uint64
	repairConfig       atomic.Pointer[config.Repair]
	repairLast         atomic.Pointer[string]
	repairGate         chan struct{}
	repairPersist      chan struct{}
	repairMemory       atomic.Int64
	repairAttempts     atomic.Int64
	repairSuccesses    atomic.Int64
	repairFailures     atomic.Int64
	repairInputBytes   atomic.Int64
	repairSummaryBytes atomic.Int64

	fetcher    core.ArticleFetcher
	cache      core.SegmentCache
	opts       Options
	log        *slog.Logger
	hot        *hotBuffer
	hedgeDelay time.Duration // bounded live wait on an existing speculative flight

	ctx    context.Context // cancelled by Close; parent of every fetch
	cancel context.CancelFunc

	mu          sync.Mutex
	healthMu    sync.RWMutex
	healthTrace HealthTrace
	releases    map[string]*list.Element // digest -> *release
	relLRU      *list.List
	flights     map[flightKey]*flight
	wg          sync.WaitGroup // fetch goroutines
	closed      bool

	fetches, fetchedBytes, hotHits, cacheHits, missing, coalesced, active atomic.Int64

	par2Loaded, par2Failed, par2Skipped, par2Named, par2Saved, par2Verified, par2Mismatches atomic.Int64
}

// HealthTrace describes the most recent release validation phases.
type HealthTrace struct {
	Warm    time.Duration `json:"warm"`
	Parse   time.Duration `json:"parse"`
	Plan    time.Duration `json:"plan"`
	STAT    time.Duration `json:"stat"`
	Samples int           `json:"samples"`
	// BODY probe results: the mid-file segments verified with a real
	// BODY (yEnc header) read, since STAT 223 does not prove a BODY
	// would not be taken down (451).
	Body       time.Duration `json:"body"`
	BodyProbes int           `json:"bodyProbes"`
	BodyOK     int           `json:"bodyOk"`
	// FailFast is set when the check bailed out on the first confirmed
	// missing article instead of waiting for every sample; FailReason
	// says which probe decided it.
	FailFast   bool   `json:"failFast"`
	FailReason string `json:"failReason,omitempty"`
}

// HealthTimings returns timing for the most recent Check call.
func (e *Engine) HealthTimings() HealthTrace {
	e.healthMu.RLock()
	defer e.healthMu.RUnlock()
	return e.healthTrace
}

var (
	_ core.Engine        = (*Engine)(nil)
	_ core.HealthChecker = (*Engine)(nil)
)

// New returns an engine. cache may be nil (memory only). logger may be nil.
func New(fetcher core.ArticleFetcher, cache core.SegmentCache, opts Options, logger *slog.Logger) *Engine {
	if logger == nil {
		logger = slog.Default()
	}
	opts = opts.withDefaults()
	ctx, cancel := context.WithCancel(context.Background())
	return &Engine{
		repairGate:    make(chan struct{}, 1),
		repairPersist: make(chan struct{}, 1),
		fetcher:       fetcher,
		cache:         cache,
		opts:          opts,
		log:           logger.With("component", "stream"),
		hot:           newHotBuffer(opts.HotBufferBytes),
		hedgeDelay:    liveHedgeDelay,
		ctx:           ctx,
		cancel:        cancel,
		releases:      map[string]*list.Element{},
		relLRU:        list.New(),
		flights:       map[flightKey]*flight{},
	}
}

// Close cancels every in-flight fetch, makes open sources fail and waits
// for background work (fetches, cache writes) to stop.
func (e *Engine) Close() error {
	e.mu.Lock()
	e.closed = true
	e.mu.Unlock()
	e.cancel()
	e.wg.Wait()
	return nil
}

// Stats returns a snapshot of the counters.
func (e *Engine) Stats() Stats {
	e.mu.Lock()
	n := len(e.releases)
	e.mu.Unlock()
	var reason string
	if p := e.repairLast.Load(); p != nil {
		reason = *p
	}
	return Stats{
		RepairLastReason:   reason,
		RepairAttempts:     e.repairAttempts.Load(),
		RepairSuccesses:    e.repairSuccesses.Load(),
		RepairFailures:     e.repairFailures.Load(),
		RepairInputBytes:   e.repairInputBytes.Load(),
		RepairSummaryBytes: e.repairSummaryBytes.Load(),
		Fetches:            e.fetches.Load(),
		FetchedBytes:       e.fetchedBytes.Load(),
		HotHits:            e.hotHits.Load(),
		CacheHits:          e.cacheHits.Load(),
		Missing:            e.missing.Load(),
		Coalesced:          e.coalesced.Load(),
		HotBytes:           e.hot.usage(),
		OpenReleases:       n,
		ActiveSources:      e.active.Load(),

		Par2Loaded:      e.par2Loaded.Load(),
		Par2Failed:      e.par2Failed.Load(),
		Par2Skipped:     e.par2Skipped.Load(),
		Par2Named:       e.par2Named.Load(),
		Par2ProbesSaved: e.par2Saved.Load(),
		Par2Verified:    e.par2Verified.Load(),
		Par2Mismatches:  e.par2Mismatches.Load(),
	}
}

// Open builds the byte-offset index for the main video file of an NZB.
func (e *Engine) Open(ctx context.Context, releaseID string, nzbData []byte) (core.MediaSource, error) {
	if err := e.ctx.Err(); err != nil {
		return nil, ErrClosed
	}
	rel, err := e.acquire(nzbData)
	if err != nil {
		return nil, err
	}
	// The plan probe is the cold-start head of line: its reads are one stream.
	p, err := rel.ensurePlan(core.WithStreamKey(ctx, rel), core.PriorityLive, releaseID)
	if err != nil {
		e.release(rel)
		if IsRejection(err) {
			err = fmt.Errorf("%w: %w", core.ErrReleaseRejected, err)
		}
		return nil, err
	}
	// A health check that found the content not matching its PAR2 index
	// rejects the release here too (never waiting for PAR2 at Open).
	if done, err := p.par2Verdict(); done && err != nil {
		e.release(rel)
		return nil, err
	}
	if e.repairOptions().Enabled {
		warmCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		if rel.repairMu.TryLock() {
			e.prepareRepair(warmCtx, rel, core.PriorityReadahead, fetchOpts{cacheID: releaseID, noRepair: true})
			rel.repairMu.Unlock()
		}
		cancel()
	}
	return newSource(e, rel, p, releaseID), nil
}

// Prefetch warms [offset, offset+length) of the main video into the
// segment cache at background priority.
func (e *Engine) Prefetch(ctx context.Context, releaseID string, nzbData []byte, offset, length int64) error {
	if err := e.ctx.Err(); err != nil {
		return ErrClosed
	}
	rel, err := e.acquire(nzbData)
	if err != nil {
		return err
	}
	defer e.release(rel)
	p, err := rel.ensurePlan(ctx, core.PriorityPrefetch, releaseID)
	if err != nil {
		return err
	}
	return e.prefetchRange(ctx, rel, p, releaseID, offset, length)
}

// acquire returns the release state for an NZB (parsing it once) and takes
// a reference that release must drop.
func (e *Engine) acquire(nzbData []byte) (*release, error) {
	sum := sha256.Sum256(nzbData)
	digest := hex.EncodeToString(sum[:12])
	e.mu.Lock()
	if el, ok := e.releases[digest]; ok {
		rel := el.Value.(*release)
		rel.refs++
		e.relLRU.MoveToFront(el)
		e.mu.Unlock()
		return rel, nil
	}
	e.mu.Unlock()

	doc, err := nzb.Parse(nzbData)
	if err != nil {
		return nil, err
	}
	rel := newRelease(e, digest, doc)

	e.mu.Lock()
	defer e.mu.Unlock()
	if el, ok := e.releases[digest]; ok { // lost a race; use the winner
		other := el.Value.(*release)
		other.refs++
		e.relLRU.MoveToFront(el)
		return other, nil
	}
	rel.refs = 1
	e.releases[digest] = e.relLRU.PushFront(rel)
	e.trimReleasesLocked()
	return rel, nil
}

func (e *Engine) release(rel *release) {
	e.mu.Lock()
	defer e.mu.Unlock()
	rel.refs--
	e.trimReleasesLocked()
}

func (e *Engine) trimReleasesLocked() {
	for el := e.relLRU.Back(); el != nil && e.relLRU.Len() > e.opts.MaxReleases; {
		prev := el.Prev()
		rel := el.Value.(*release)
		if rel.refs <= 0 && rel.repairMu.TryLock() {
			e.relLRU.Remove(el)
			delete(e.releases, rel.digest)
			if rel.repair != nil {
				e.repairMemory.Add(-rel.repair.reserved)
				rel.repair = nil
			}
			rel.repairMu.Unlock()
			// Drop synchronously (hot.mu never waits on e.mu): a deferred
			// drop could run after the same NZB was acquired again and
			// evict the new release state's segments.
			e.hot.dropRelease(rel.digest)
		}
		el = prev
	}
}

// errorsJoinMissing builds an error that is both ErrIncomplete and
// core.ErrArticleMissing.
func incompleteError(format string, args ...any) error {
	return fmt.Errorf("%w: %s: %w", ErrIncomplete, fmt.Sprintf(format, args...), core.ErrArticleMissing)
}

func isMissing(err error) bool { return errors.Is(err, core.ErrArticleMissing) }
