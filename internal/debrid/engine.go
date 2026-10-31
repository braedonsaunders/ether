package debrid

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptrace"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/lru"
)

// Streaming parameters. The segment cache stores fixed blocks; HTTP range
// requests span runs of blocks whose length grows from one block (fast
// first byte) towards ChunkBytes.
const (
	defaultBlockSize    = 512 << 10 // cache unit and first-request size
	defaultMaxRunBytes  = 8 << 20   // config.Debrid.ChunkBytes default
	freshRunBlocks      = 8         // first request at a new position: 4 MiB of 512 KiB blocks
	defaultConnections  = 4         // config.Debrid.MaxConnections default
	minReadAheadBytes   = 8 << 20
	maxReadAheadBytes   = 96 << 20
	readAheadSeconds    = 10 // adaptive window: seconds of measured throughput
	memBlocksPerFile    = 32 // in-memory blocks per open file (16 MiB)
	defaultHedgeDelay   = 400 * time.Millisecond
	idleFileTTL         = 15 * time.Minute
	tailPrefetchBytes   = 2 << 20 // container index (MKV Cues / MP4 moov) at the end
	maxRangeAttempts    = 4
	maxLinkFailures     = 3
	readAtDefaultBudget = 2 * time.Minute
	// bodyStallTimeout bounds a media read that receives no bytes at all.
	// The transport only times out headers; a CDN connection that stays
	// open but stops sending would otherwise hold a block's flight (and
	// every reader waiting on it) for the file's lifetime.
	bodyStallTimeout = 15 * time.Second
)

// errRunCancelled marks blocks whose read-ahead run was cancelled by a
// seek; a reader waiting on one simply fetches it again.
var errRunCancelled = errors.New("debrid: read-ahead cancelled")

// errBodyStalled is a media response that delivered no bytes for
// bodyStallTimeout; the read is retried from the block it stopped in.
var errBodyStalled = errors.New("debrid: media response stalled")

// EngineOptions configure an Engine.
type EngineOptions struct {
	// Inner serves every non-debrid payload (the Usenet stream engine).
	Inner core.Engine
	// Health checks non-debrid payloads; nil when Inner does not implement
	// core.HealthChecker.
	Health core.HealthChecker
	// Cache stores fetched blocks on disk (nil keeps them in memory only).
	Cache core.SegmentCache
	// HTTPClient and HedgeClient read media; they must use separate
	// connection pools so a hedged request opens a fresh connection.
	HTTPClient  *http.Client
	HedgeClient *http.Client
	Logger      *slog.Logger
	Now         func() time.Time
	HedgeDelay  time.Duration // 0 = 400ms; negative disables hedging
	BlockSize   int64         // tests; 0 = 512 KiB
}

// Engine implements core.Engine and core.HealthChecker: debrid payloads
// are served from HTTPS range requests; anything else goes to Inner.
type Engine struct {
	svc   *Service
	inner core.Engine
	inHC  core.HealthChecker
	cache core.SegmentCache
	hc    *http.Client
	hedge *http.Client
	log   *slog.Logger
	now   func() time.Time
	delay time.Duration
	block int64
	stall time.Duration // bodyStallTimeout; tests shorten it

	// limit caps connections per media host across both owned transports;
	// backoff spaces requests to a host whose connections keep failing.
	limit   *connCap
	backoff *hostBackoff

	ctx    context.Context
	cancel context.CancelFunc

	mu           sync.Mutex
	files        map[string]*remoteFile
	mediaPauseMu sync.Mutex
	mediaPauses  map[string]mediaPause

	stats engineCounters

	// Streaming leases (cluster.go), clustered only.
	leaseOnce sync.Once
	leaseKick chan struct{}
	leaseDone chan struct{}
}

type engineCounters struct {
	memHits, diskHits, fetched, bytes, hedges, hedgeWins, refreshes, errors atomic.Int64
}

// EngineStats is the engine part of the debrid admin status.
type EngineStats struct {
	OpenFiles     int   `json:"openFiles"`
	BlockBytes    int64 `json:"blockBytes"`
	MemoryHits    int64 `json:"memoryHits"`
	DiskHits      int64 `json:"diskHits"`
	BlocksFetched int64 `json:"blocksFetched"`
	BytesFetched  int64 `json:"bytesFetched"`
	Hedges        int64 `json:"hedges"`
	HedgeWins     int64 `json:"hedgeWins"`
	LinkRefreshes int64 `json:"linkRefreshes"`
	Errors        int64 `json:"errors"`
}

var (
	_ core.Engine        = (*Engine)(nil)
	_ core.HealthChecker = (*Engine)(nil)
)

// NewStreamingTransport is an HTTP transport tuned for media streaming:
// keep-alive, HTTP/2 when offered, many idle connections per host and
// connect/TLS timeouts that tolerate a busy CDN edge.
func NewStreamingTransport() *http.Transport {
	return newStreamingTransport(nil)
}

// Busy debrid CDN edges can take several seconds to accept a connection and
// finish the TLS handshake (5.08s was observed). Failing at 5s threw away
// a handshake about to complete and started another, which is slower again
// on the same node. The generous connect and TLS limits only cover setting
// up the connection; ResponseHeaderTimeout still bounds a server that
// accepts and then never answers, and bodyStallTimeout a stalled body.
const (
	mediaDialTimeout = 10 * time.Second
	mediaTLSTimeout  = 15 * time.Second
)

func newStreamingTransport(limit *connCap) *http.Transport {
	d := &net.Dialer{Timeout: mediaDialTimeout, KeepAlive: 30 * time.Second}
	dial := d.DialContext
	if limit != nil {
		dial = limit.wrap(dial)
	}
	return &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           dial,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          64,
		MaxIdleConnsPerHost:   16,
		IdleConnTimeout:       120 * time.Second,
		TLSHandshakeTimeout:   mediaTLSTimeout,
		ResponseHeaderTimeout: 15 * time.Second,
		ExpectContinueTimeout: time.Second,
		ReadBufferSize:        64 << 10,
		WriteBufferSize:       16 << 10,
	}
}

// NewEngine wraps inner with debrid streaming.
func NewEngine(svc *Service, opts EngineOptions) *Engine {
	e := &Engine{svc: svc, inner: opts.Inner, inHC: opts.Health, cache: opts.Cache, hc: opts.HTTPClient, hedge: opts.HedgeClient,
		log: opts.Logger, now: opts.Now, delay: opts.HedgeDelay, block: opts.BlockSize, stall: bodyStallTimeout, files: map[string]*remoteFile{}}
	if e.inHC == nil {
		e.inHC, _ = opts.Inner.(core.HealthChecker)
	}
	// The primary and hedge pools share one per-host connection cap: the
	// CDN counts every open connection from this server.
	limit := newConnCap(defaultHostConnections)
	var owned []*http.Transport
	if e.hc == nil {
		t := newStreamingTransport(limit)
		owned = append(owned, t)
		e.hc = &http.Client{Transport: t}
	}
	if e.hedge == nil {
		t := newStreamingTransport(limit)
		owned = append(owned, t)
		e.hedge = &http.Client{Transport: t}
	}
	limit.closeIdle = func() {
		for _, t := range owned {
			t.CloseIdleConnections()
		}
	}
	if e.log == nil {
		e.log = slog.Default()
	}
	limit.log = e.log
	e.limit = limit
	e.backoff = newHostBackoff(e.log)
	if e.now == nil {
		e.now = time.Now
	}
	if e.delay == 0 {
		e.delay = defaultHedgeDelay
	}
	if e.block <= 0 {
		e.block = defaultBlockSize
	}
	e.ctx, e.cancel = context.WithCancel(context.Background())
	return e
}

// Inner returns the wrapped engine.
func (e *Engine) Inner() core.Engine { return e.inner }

// Close cancels in-flight fetches and drops this node's streaming leases.
// The inner engine is closed by its owner.
func (e *Engine) Close() error {
	e.cancel()
	e.leaseOnce.Do(func() {}) // no worker may start after Close
	if e.leaseDone != nil {
		select {
		case <-e.leaseDone:
		case <-time.After(10 * time.Second):
		}
	}
	return nil
}

// Stats reports engine counters.
func (e *Engine) Stats() EngineStats {
	e.mu.Lock()
	open := 0
	for _, f := range e.files {
		if f.refs > 0 {
			open++
		}
	}
	e.mu.Unlock()
	return EngineStats{OpenFiles: open, BlockBytes: e.block, MemoryHits: e.stats.memHits.Load(), DiskHits: e.stats.diskHits.Load(),
		BlocksFetched: e.stats.fetched.Load(), BytesFetched: e.stats.bytes.Load(), Hedges: e.stats.hedges.Load(),
		HedgeWins: e.stats.hedgeWins.Load(), LinkRefreshes: e.stats.refreshes.Load(), Errors: e.stats.errors.Load()}
}

// tuning reads the live config knobs.
func (e *Engine) tuning() (maxRunBlocks int64, conns int, fixedAhead int64) {
	cfg := e.svc.Config()
	run := cfg.ChunkBytes
	if run <= 0 {
		run = defaultMaxRunBytes
	}
	maxRunBlocks = max(1, run/e.block)
	conns = cfg.MaxConnections
	if conns <= 0 {
		conns = defaultConnections
	}
	switch {
	case cfg.ReadAheadChunks < 0:
		fixedAhead = -1
	case cfg.ReadAheadChunks > 0:
		fixedAhead = int64(cfg.ReadAheadChunks) * run
	}
	return maxRunBlocks, conns, fixedAhead
}

// Open implements core.Engine. With a fresh link already known (the usual
// case after a detail-page prewarm) it makes no provider API call and no
// request: the first read is served from the cache or one range GET.
func (e *Engine) Open(ctx context.Context, releaseID string, payload []byte) (core.MediaSource, error) {
	if !IsPayload(payload) {
		return e.inner.Open(ctx, releaseID, payload)
	}
	f, err := e.acquireFile(ctx, releaseID, payload)
	if err != nil {
		return nil, err
	}
	s := &httpSource{f: f, last: -2}
	s.raCtx, s.raCancel = context.WithCancel(f.ctx)
	return s, nil
}

// Prefetch implements core.Engine. For a debrid payload it pulls
// [offset, offset+length) into the segment cache; from offset 0 it also
// pulls the file's tail, where MKV Cues and MP4 moov usually live, so the
// container index is warm too.
func (e *Engine) Prefetch(ctx context.Context, releaseID string, payload []byte, offset, length int64) error {
	if !IsPayload(payload) {
		return e.inner.Prefetch(ctx, releaseID, payload, offset, length)
	}
	f, err := e.acquireFile(ctx, releaseID, payload)
	if err != nil {
		return err
	}
	defer e.releaseFile(f)
	if offset < 0 {
		offset = 0
	}
	end := min(offset+length, f.size)
	ranges := [][2]int64{{offset, end}}
	if offset == 0 && f.size > end {
		ranges = append(ranges, [2]int64{max(end, f.size-tailPrefetchBytes), f.size})
	}
	return f.prefetch(ctx, ranges)
}

// Check implements core.HealthChecker. For a debrid payload it confirms a
// direct link is obtainable and that a range request answers 206 with the
// expected size, caching the head block it read; others go to the inner
// checker.
func (e *Engine) Check(ctx context.Context, payload []byte) (bool, error) {
	return e.CheckRelease(ctx, "", payload)
}

// CheckRelease is Check with the release ID, passed through to the inner
// checker for Usenet payloads so its reads are cached under that ID.
// Implements core.ReleaseHealthChecker.
func (e *Engine) CheckRelease(ctx context.Context, releaseID string, payload []byte) (bool, error) {
	if !IsPayload(payload) {
		if e.inHC == nil {
			return true, nil
		}
		return core.CheckReleaseHealth(ctx, e.inHC, releaseID, payload)
	}
	p, err := DecodePayload(payload)
	if err != nil {
		return false, fmt.Errorf("%w: %v", core.ErrReleaseRejected, err)
	}
	// Health checks have no release id; the payload carries it so the head
	// block lands where Open will look for it.
	f, err := e.acquireFile(ctx, healthKey(p), payload)
	if err != nil {
		return false, err
	}
	defer e.releaseFile(f)
	if core.IsBackgroundCheck(ctx) {
		// Keep budget and cancellation on this request instead of starting a
		// shared durable block fetch with the engine lifetime.
		body, _, err := f.rangeGet(ctx, 0, min(f.size-1, 65535), false)
		if err != nil {
			return false, err
		}
		defer body.Close()
		_, err = io.CopyN(io.Discard, body, min(f.size, 65536))
		return err == nil, err
	}
	// With the head already cached, a 1-byte range request still proves the
	// link serves this file (Content-Range carries its size), so a check
	// never passes on cached bytes alone.
	f.bmu.Lock()
	_, inFlight := f.flights[0]
	cached := !inFlight && f.presentLocked(0)
	f.bmu.Unlock()
	if cached {
		body, _, err := f.rangeGet(ctx, 0, 0, true)
		if err != nil {
			return false, err
		}
		body.Close()
		return true, nil
	}
	if _, err := f.block(ctx, 0, fetchDurable, 1, nil); err != nil {
		return false, err
	}
	return true, nil
}

// healthKey is the file key used without a caller-supplied release id.
func healthKey(p Payload) string {
	if p.ReleaseID != "" {
		return p.ReleaseID
	}
	return "debrid-" + p.Key
}

// acquireFile returns the shared state for a release's file, refreshing
// its link only when the known one is stale.
func (e *Engine) acquireFile(ctx context.Context, releaseID string, payload []byte) (*remoteFile, error) {
	p, err := DecodePayload(payload)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", core.ErrReleaseRejected, err)
	}
	if releaseID == "" {
		releaseID = healthKey(p)
	}
	e.mu.Lock()
	e.trimLocked()
	f := e.files[releaseID]
	if f == nil || f.tag != p.fileTag() {
		if f != nil && f.refs <= 0 {
			f.cancel()
		}
		f = e.newFile(releaseID, p)
		e.files[releaseID] = f
	} else {
		f.adopt(p)
	}
	f.refs++
	first := f.refs == 1
	f.lastUsed = e.now()
	e.mu.Unlock()
	if first {
		e.kickLeases()
	}
	if _, _, err := f.link(ctx, false); err != nil {
		e.releaseFile(f)
		return nil, err
	}
	return f, nil
}

func (e *Engine) releaseFile(f *remoteFile) {
	e.mu.Lock()
	f.refs--
	f.lastUsed = e.now()
	idle := f.refs <= 0
	// A file replaced while in use (its payload named another file) is no
	// longer in e.files, so trimming never reaches it: end it here.
	replaced := idle && e.files[f.id] != f
	e.mu.Unlock()
	if replaced {
		f.cancel()
	}
	if idle {
		f.dropMemory()
		e.kickLeases()
	}
}

// trimLocked forgets files idle for a while (their blocks stay on disk).
func (e *Engine) trimLocked() {
	now := e.now()
	for id, f := range e.files {
		if f.refs <= 0 && now.Sub(f.lastUsed) > idleFileTTL {
			f.cancel()
			delete(e.files, id)
		}
	}
}

// ---------------------------------------------------------------------------
// Shared per-file state.

type fetchMode int

const (
	fetchUrgent    fetchMode = iota // a reader is waiting: hedged, may use every connection
	fetchReadAhead                  // speculative; cancelled by seeks
	fetchPrefetch                   // background warm-up; disk only
	fetchDurable                    // urgent, but published only once on disk (health checks)
)

type blockFlight struct {
	done chan struct{}
	data []byte
	err  error
}

type remoteFile struct {
	e       *Engine
	id      string
	tag     [8]byte
	size    int64
	nblocks int64
	ctx     context.Context
	cancel  context.CancelFunc

	// guarded by Engine.mu
	refs     int
	lastUsed time.Time

	linkMu           sync.Mutex // serializes refreshes
	mu               sync.Mutex
	pl               Payload
	gen              int
	linkFails        int       // link generations that failed in a row
	failedGen        int       // last generation counted in linkFails (-1 = none)
	transportRetryAt time.Time // bound link regeneration during a CDN outage
	mediaHealthyKey  string    // a shared cooldown must not interrupt another healthy file
	mediaPauseKey    string
	mediaPause       mediaPause

	bmu     sync.Mutex
	flights map[int64]*blockFlight
	mem     *lru.Cache[int64, []byte]
	stored  map[int64]bool
	tput    float64 // bytes/s EWMA

	conns   chan struct{} // all requests
	raSlots chan struct{} // read-ahead and prefetch (leaves one connection for urgent reads)
}

func (e *Engine) newFile(id string, p Payload) *remoteFile {
	_, conns, _ := e.tuning()
	f := &remoteFile{e: e, id: id, tag: p.fileTag(), size: p.Size, pl: p,
		flights: map[int64]*blockFlight{}, mem: lru.New(lru.Options[int64, []byte]{MaxEntries: memBlocksPerFile}), stored: map[int64]bool{}, failedGen: -1,
		conns: make(chan struct{}, conns), raSlots: make(chan struct{}, max(1, conns-1))}
	f.nblocks = (p.Size + e.block - 1) / e.block
	f.ctx, f.cancel = context.WithCancel(e.ctx)
	return f
}

// adopt takes a fresher link from a payload handed in by a caller.
func (f *remoteFile) adopt(p Payload) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if p.URL != "" && p.Expires.After(f.pl.Expires) {
		// The newer link may come from another provider or a re-added
		// torrent: take its identity with it, so refreshes and timings go
		// to the right place.
		f.pl.URL, f.pl.Expires = p.URL, p.Expires
		f.pl.Provider, f.pl.Kind, f.pl.TorrentID, f.pl.FileID, f.pl.Link = p.Provider, p.Kind, p.TorrentID, p.FileID, p.Link
		f.gen++
	}
}

func (f *remoteFile) name() string { p, _ := f.payload(); return p.Name() }

func (f *remoteFile) payload() (Payload, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.pl, f.gen
}

// link returns a usable URL, refreshing when the current one is stale or
// (force) failed at generation gen.
func (f *remoteFile) link(ctx context.Context, force bool, gen ...int) (string, int, error) {
	p, g := f.payload()
	if !force && f.e.svc.fresh(p) {
		return p.URL, g, nil
	}
	f.linkMu.Lock()
	defer f.linkMu.Unlock()
	p, g = f.payload()
	if force && len(gen) > 0 && g != gen[0] {
		return p.URL, g, nil // someone else already refreshed
	}
	if !force && f.e.svc.fresh(p) {
		return p.URL, g, nil
	}
	out, err := f.e.svc.Refresh(ctx, p, force)
	if err != nil {
		f.e.stats.errors.Add(1)
		return "", g, err
	}
	f.e.stats.refreshes.Add(1)
	f.mu.Lock()
	if out.Size != f.size {
		f.mu.Unlock()
		return "", g, fmt.Errorf("debrid: %s changed size: %w", p.Name(), core.ErrArticleCorrupt)
	}
	moved := out.TorrentID != p.TorrentID || out.Provider != p.Provider
	f.pl = out
	f.gen++
	g = f.gen
	f.mu.Unlock()
	if moved {
		f.e.kickLeases() // lease the torrent the file now streams from
	}
	return out.URL, g, nil
}

func (f *remoteFile) blockLen(idx int64) int64 {
	if idx == f.nblocks-1 {
		return f.size - idx*f.e.block
	}
	return f.e.block
}

func (f *remoteFile) memGetLocked(idx int64) []byte {
	data, _ := f.mem.Get(idx)
	return data
}

func (f *remoteFile) memPutLocked(idx int64, data []byte) { f.mem.Add(idx, data) }

func (f *remoteFile) dropMemory() {
	f.bmu.Lock()
	f.mem.Clear()
	f.bmu.Unlock()
}

// present reports whether a block needs no fetch.
func (f *remoteFile) presentLocked(idx int64) bool {
	if f.mem.Contains(idx) {
		return true
	}
	if _, ok := f.flights[idx]; ok {
		return true
	}
	// The segment cache evicts on its own, so a block stored earlier may be
	// gone: ask the cache when it can say.
	return f.presentOnDiskLocked(idx)
}

// presentOnDiskLocked reports a block the segment cache still holds.
func (f *remoteFile) presentOnDiskLocked(idx int64) bool {
	if h, ok := f.e.cache.(core.SegmentPresence); ok {
		if h.Has(f.id, int(idx)) {
			f.stored[idx] = true
			return true
		}
		delete(f.stored, idx)
		return false
	}
	return f.stored[idx]
}

func (f *remoteFile) diskGet(idx int64) ([]byte, bool) {
	if f.e.cache == nil {
		return nil, false
	}
	raw, ok := f.e.cache.Get(f.id, int(idx))
	if !ok || int64(len(raw)) != 8+f.blockLen(idx) || !bytes.Equal(raw[:8], f.tag[:]) {
		// Evicted, or another file's block: not present for this file.
		f.bmu.Lock()
		delete(f.stored, idx)
		f.bmu.Unlock()
		return nil, false
	}
	return raw[8:], true
}

func (f *remoteFile) diskPut(idx int64, data []byte) {
	if f.e.cache == nil {
		return
	}
	buf := make([]byte, 8+len(data))
	copy(buf, f.tag[:])
	copy(buf[8:], data)
	if err := f.e.cache.Put(f.id, int(idx), buf); err == nil {
		f.bmu.Lock()
		f.stored[idx] = true
		f.bmu.Unlock()
	}
}

// block returns one block, fetching a run of up to runBlocks starting at
// it when it is neither in memory, on disk nor in flight. runCtx scopes a
// read-ahead run (nil = the file's lifetime).
func (f *remoteFile) block(ctx context.Context, idx int64, mode fetchMode, runBlocks int64, runCtx context.Context) ([]byte, error) {
	if idx < 0 || idx >= f.nblocks {
		return nil, io.EOF
	}
	for attempt := 0; attempt < 3; attempt++ {
		f.bmu.Lock()
		if d := f.memGetLocked(idx); d != nil {
			f.bmu.Unlock()
			f.e.stats.memHits.Add(1)
			return d, nil
		}
		fl := f.flights[idx]
		f.bmu.Unlock()
		if fl == nil {
			if d, ok := f.diskGet(idx); ok {
				f.e.stats.diskHits.Add(1)
				if mode != fetchPrefetch {
					f.bmu.Lock()
					f.memPutLocked(idx, d)
					f.bmu.Unlock()
				}
				return d, nil
			}
			if runCtx == nil {
				runCtx = f.ctx
			}
			fl, _ = f.startRun(runCtx, idx, runBlocks, mode, false)
		}
		select {
		case <-fl.done:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		if fl.err == nil {
			return fl.data, nil
		}
		if !errors.Is(fl.err, errRunCancelled) {
			return nil, fl.err
		}
		runCtx, mode = nil, fetchUrgent
	}
	return nil, fmt.Errorf("debrid: block %d: %w", idx, core.ErrSourceUnavailable)
}

// startRun registers flights for a run of missing blocks starting at idx
// and fetches them with one range request. It returns idx's flight and
// whether this call started the run. slotHeld means the caller already
// holds a read-ahead slot for the run (released when it ends).
func (f *remoteFile) startRun(ctx context.Context, idx, n int64, mode fetchMode, slotHeld bool) (*blockFlight, bool) {
	f.bmu.Lock()
	if fl := f.flights[idx]; fl != nil {
		f.bmu.Unlock()
		return fl, false
	}
	var fls []*blockFlight
	for i := idx; i < idx+max(n, 1) && i < f.nblocks; i++ {
		if i > idx && f.presentLocked(i) {
			break
		}
		fl := &blockFlight{done: make(chan struct{})}
		f.flights[i] = fl
		fls = append(fls, fl)
	}
	f.bmu.Unlock()
	go f.fetchRun(ctx, idx, fls, mode, slotHeld)
	return fls[0], true
}

func (f *remoteFile) finish(idx int64, fl *blockFlight, data []byte, err error, mode fetchMode) {
	f.bmu.Lock()
	if err == nil && mode != fetchPrefetch {
		f.memPutLocked(idx, data)
	}
	if f.flights[idx] == fl {
		delete(f.flights, idx)
	}
	f.bmu.Unlock()
	fl.data, fl.err = data, err
	close(fl.done)
}

// fetchRun reads blocks [first, first+len(fls)) with one range request,
// publishing each block as soon as its bytes arrive.
func (f *remoteFile) fetchRun(ctx context.Context, first int64, fls []*blockFlight, mode fetchMode, slotHeld bool) {
	i := 0
	fail := func(err error) {
		if ctx.Err() != nil {
			err = errRunCancelled // the run was abandoned; waiters fetch again
		}
		// Retire the entire failed tail before waking any reader. A caller
		// can immediately retry its first block; publishing failures one
		// block at a time lets that retry join later stale flights.
		f.bmu.Lock()
		for j := i; j < len(fls); j++ {
			idx := first + int64(j)
			if f.flights[idx] == fls[j] {
				delete(f.flights, idx)
			}
		}
		f.bmu.Unlock()
		for ; i < len(fls); i++ {
			fls[i].err = err
			close(fls[i].done)
		}
	}
	if slotHeld {
		defer func() { <-f.raSlots }()
	} else if mode == fetchReadAhead || mode == fetchPrefetch {
		select {
		case f.raSlots <- struct{}{}:
			defer func() { <-f.raSlots }()
		case <-ctx.Done():
			fail(ctx.Err())
			return
		}
	}
	start := first * f.e.block
	end := min((first+int64(len(fls)))*f.e.block, f.size) - 1
	began := f.e.now()
	body, tm, err := f.rangeGet(ctx, start, end, mode == fetchUrgent || mode == fetchDurable)
	if err != nil {
		if ctx.Err() == nil {
			f.e.log.Warn("debrid range failed", "release", f.id, "offset", start, "err", sanitize(err))
		}
		fail(err)
		return
	}
	defer func() {
		if body != nil {
			body.Close()
		}
	}()
	var got int64
	bodyRetries := 0
	for ; i < len(fls); i++ {
		idx := first + int64(i)
		buf := make([]byte, f.blockLen(idx))
		for {
			_, err = io.ReadFull(body, buf)
			if err == nil || ctx.Err() != nil || bodyRetries >= maxRangeAttempts-1 {
				break
			}
			// Headers and the first byte can succeed before the CDN drops
			// the body. Keep completed blocks; refetch the incomplete block
			// from its exact offset instead of replacing the whole release.
			body.Close()
			bodyRetries++
			if isConnReset(err) {
				p, _ := f.payload()
				f.e.reduceHostCap(p.URL)
			}
			f.e.log.Warn("debrid body interrupted; retrying same file", "release", f.id,
				"offset", idx*f.e.block, "attempt", bodyRetries, "err", sanitize(err))
			if bodyRetries == 1 {
				_, gen := f.payload()
				if _, _, refreshErr := f.link(ctx, true, gen); refreshErr != nil {
					err = refreshErr
					break
				}
			}
			body, tm, err = f.rangeGet(ctx, idx*f.e.block, end, mode == fetchUrgent || mode == fetchDurable)
			if err != nil {
				break
			}
		}
		if err != nil {
			if ctx.Err() != nil {
				err = ctx.Err()
			} else {
				err = fmt.Errorf("debrid: read %s: %w: %v", f.name(), core.ErrSourceUnavailable, sanitize(err))
				f.e.log.Warn("debrid range failed", "release", f.id, "offset", idx*f.e.block, "err", err)
			}
			fail(err)
			break
		}
		got += int64(len(buf))
		f.e.stats.fetched.Add(1)
		f.e.stats.bytes.Add(int64(len(buf)))
		if mode == fetchPrefetch || mode == fetchDurable {
			// Publish only once on disk: prefetched blocks skip memory, and
			// a checked head block must outlive the check's handle.
			f.diskPut(idx, buf)
			f.finish(idx, fls[i], buf, nil, mode)
		} else {
			f.finish(idx, fls[i], buf, nil, mode)
			f.diskPut(idx, buf)
		}
	}
	elapsed := f.e.now().Sub(began)
	if got > 0 && elapsed > 0 {
		f.bmu.Lock()
		f.tput = ewma(f.tput, float64(got)/elapsed.Seconds(), f.tput == 0)
		f.bmu.Unlock()
	}
	p, _ := f.payload()
	f.e.svc.RecordStream(p.Provider, tm.connect, tm.ttfb, got, elapsed)
}

// prefetch warms byte ranges into the cache with bounded concurrency.
func (f *remoteFile) prefetch(ctx context.Context, ranges [][2]int64) error {
	maxRun, _, _ := f.e.tuning()
	var wg sync.WaitGroup
	errc := make(chan error, 1)
	for _, r := range ranges {
		if r[1] <= r[0] {
			continue
		}
		for idx := r[0] / f.e.block; idx*f.e.block < r[1] && idx < f.nblocks; {
			f.bmu.Lock()
			present := f.presentLocked(idx)
			f.bmu.Unlock()
			if present {
				idx++
				continue
			}
			n := min(maxRun, (r[1]-idx*f.e.block+f.e.block-1)/f.e.block, f.nblocks-idx)
			wg.Add(1)
			go func(idx, n int64) {
				defer wg.Done()
				// Wait for the whole run, not just its first block.
				for i := idx; i < idx+n; i++ {
					f.bmu.Lock()
					fl, stored := f.flights[i], f.presentOnDiskLocked(i)
					f.bmu.Unlock()
					if stored {
						continue
					}
					if fl != nil {
						select {
						case <-fl.done:
						case <-ctx.Done():
							select {
							case errc <- ctx.Err():
							default:
							}
							return
						}
						if fl.err == nil {
							continue
						}
					}
					if _, err := f.block(ctx, i, fetchPrefetch, idx+n-i, ctx); err != nil {
						select {
						case errc <- err:
						default:
						}
						return
					}
				}
			}(idx, n)
			idx += n
		}
	}
	wg.Wait()
	select {
	case err := <-errc:
		return err
	default:
		return nil
	}
}

// ---------------------------------------------------------------------------
// HTTP.

type timing struct{ connect, ttfb time.Duration }

// rangeGet requests bytes [start, end] and returns the body positioned at
// start, retrying transient failures and refreshing the link on 401, 403,
// 404, 410 or expiry. Errors wrap core.ErrSourceUnavailable (transient),
// core.ErrArticleMissing (the link keeps failing) or core.ErrArticleCorrupt
// (the file is not what the payload describes).
func (f *remoteFile) rangeGet(ctx context.Context, start, end int64, urgent bool) (io.ReadCloser, timing, error) {
	var lastErr error
	for attempt := 0; attempt < maxRangeAttempts; attempt++ {
		u, gen, err := f.link(ctx, false)
		if err != nil {
			return nil, timing{}, err
		}
		payload, _ := f.payload()
		if err := f.checkMediaPause(payload.Provider, u); err != nil {
			return nil, timing{}, err
		}
		if attempt > 0 {
			if err := sleep(ctx, time.Duration(attempt*attempt)*150*time.Millisecond); err != nil {
				return nil, timing{}, err
			}
		}
		rctx := ctx
		if payload.IsDirect() {
			rctx = withDirect(ctx, payload)
		}
		resp, tm, err := f.e.get(rctx, u, start, end, urgent, f.conns)
		if err != nil {
			if errors.Is(err, core.ErrBudgetExhausted) {
				return nil, tm, err
			}
			if ctx.Err() != nil {
				return nil, tm, ctx.Err()
			}
			lastErr = err
			// A still-valid signed URL can point at a dead CDN node. One
			// bounded regeneration gives the provider a chance to route us
			// elsewhere instead of retrying the dead endpoint four times.
			// A network outage never establishes that the file is missing.
			if attempt == 0 && urgent {
				f.mu.Lock()
				refresh := !f.e.now().Before(f.transportRetryAt)
				if refresh {
					f.transportRetryAt = f.e.now().Add(5 * time.Second)
				}
				f.mu.Unlock()
				if refresh {
					if _, _, refreshErr := f.link(ctx, true, gen); refreshErr != nil {
						return nil, tm, refreshErr
					}
				}
			}
			continue
		}
		switch {
		case resp.StatusCode == http.StatusPartialContent:
			if err := checkContentRange(resp.Header.Get("Content-Range"), start, f.size); err != nil {
				resp.Body.Close()
				return nil, tm, directMediaError(payload, err)
			}
			f.mu.Lock()
			if final := resp.Request.URL.String(); payload.IsDirect() && final != u && f.gen == gen {
				// Read the addon's redirect target directly until it fails.
				f.pl.URL = final
			}
			f.linkFails = 0
			f.mediaHealthyKey = mediaPauseKey(payload.Provider, u)
			f.mu.Unlock()
			return resp.Body, tm, nil
		case resp.StatusCode == http.StatusOK && start == 0 && resp.ContentLength == f.size:
			// A server that ignores Range but serves exactly this file.
			f.mu.Lock()
			f.mediaHealthyKey = mediaPauseKey(payload.Provider, u)
			f.mu.Unlock()
			return readCloser{Reader: io.LimitReader(resp.Body, end+1), Closer: resp.Body}, tm, nil
		case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden ||
			resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone || resp.StatusCode == http.StatusOK:
			// A 200 that is not this file (hosters answer dead links with an
			// HTML page) is a dead link too, not corrupt media.
			drain(resp)
			// Count each link generation once: concurrent requests that
			// failed on a link someone is already replacing just retry.
			f.mu.Lock()
			if gen != f.failedGen {
				f.failedGen = gen
				f.linkFails++
			}
			fails := f.linkFails
			f.mu.Unlock()
			if fails > maxLinkFailures {
				return nil, tm, directMediaError(payload, fmt.Errorf("debrid: link for %s keeps failing (HTTP %d): %w", f.name(), resp.StatusCode, core.ErrArticleMissing))
			}
			if _, _, err := f.link(ctx, true, gen); err != nil {
				return nil, tm, err
			}
			lastErr = fmt.Errorf("HTTP %d", resp.StatusCode)
		case resp.StatusCode == http.StatusRequestedRangeNotSatisfiable:
			drain(resp)
			return nil, tm, fmt.Errorf("debrid: %s is shorter than expected: %w", f.name(), core.ErrArticleCorrupt)
		case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500:
			ra := retryAfter(resp.Header.Get("Retry-After"))
			lastErr = fmt.Errorf("HTTP %d", resp.StatusCode)
			if resp.StatusCode == http.StatusTooManyRequests || ra > 0 {
				// Error bodies can stall too. Headers establish the pause;
				// do not wait to drain an unneeded body on the Play path.
				resp.Body.Close()
				if resp.StatusCode == http.StatusTooManyRequests {
					// The host counts our connections: open fewer for a while.
					f.e.reduceHostCap(resp.Request.URL.String())
				}
				// A rate-limited CDN cannot become a healthy candidate by
				// retrying each block four times before Retry-After expires.
				// Share its pause across this account's files, return promptly
				// to source selection, and preserve transient classification.
				return nil, tm, f.pauseMedia(payload.Provider, u, resp.StatusCode, ra)
			}
			drain(resp)
		default:
			drain(resp)
			lastErr = fmt.Errorf("HTTP %d", resp.StatusCode)
		}
	}
	f.e.stats.errors.Add(1)
	return nil, timing{}, fmt.Errorf("debrid: read %s: %w: %v", f.name(), core.ErrSourceUnavailable, lastErr)
}

type readCloser struct {
	io.Reader
	io.Closer
}

func drain(resp *http.Response) {
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	resp.Body.Close()
}

func checkContentRange(v string, start, size int64) error {
	// "bytes 0-1023/4096"
	v = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(v), "bytes"))
	rng, total, ok := strings.Cut(v, "/")
	if !ok {
		return nil // tolerate a missing header; the body length is checked on read
	}
	from, _, _ := strings.Cut(strings.TrimSpace(rng), "-")
	if n, err := strconv.ParseInt(strings.TrimSpace(from), 10, 64); err == nil && n != start {
		return fmt.Errorf("debrid: range starts at %d, asked %d: %w", n, start, core.ErrSourceUnavailable)
	}
	if t := strings.TrimSpace(total); t != "*" {
		if n, err := strconv.ParseInt(t, 10, 64); err == nil && n != size {
			return fmt.Errorf("debrid: file is %d bytes, expected %d: %w", n, size, core.ErrArticleCorrupt)
		}
	}
	return nil
}

// get issues one range request. Urgent requests are hedged: when the first
// byte has not arrived within the hedge delay, a duplicate request goes out
// on a separate connection pool and the first to deliver a byte wins.
func (e *Engine) get(ctx context.Context, u string, start, end int64, urgent bool, slots chan struct{}) (*http.Response, timing, error) {
	// Count actual requests through body close, including probes and hedges.
	// A speculative duplicate may only use a spare slot; it must never
	// block the primary or exceed the provider's media connection budget.
	acquire := func(wait bool) bool {
		if !wait {
			select {
			case slots <- struct{}{}:
				return true
			default:
				return false
			}
		}
		select {
		case slots <- struct{}{}:
			return true
		case <-ctx.Done():
			return false
		}
	}
	send := func(c context.Context, hc *http.Client, w *connWatch) (*http.Response, timing, error) {
		var once sync.Once
		release := func() { once.Do(func() { <-slots }) }
		c, stop := context.WithCancel(c)
		resp, tm, err := e.send(c, hc, u, start, end, w)
		if err != nil {
			stop()
			release()
		} else {
			resp.Body = newStallBody(resp.Body, e.stall, stop, release)
		}
		return resp, tm, err
	}
	if !acquire(true) {
		return nil, timing{}, ctx.Err()
	}
	if !urgent || e.delay < 0 {
		return send(ctx, e.hc, nil)
	}
	type result struct {
		resp   *http.Response
		tm     timing
		err    error
		cancel context.CancelFunc
		hedge  bool
	}
	results := make(chan result, 2)
	var cancels []context.CancelFunc
	launch := func(hc *http.Client, hedge bool, w *connWatch) {
		rctx, cancel := context.WithCancel(ctx)
		cancels = append(cancels, cancel)
		go func() {
			resp, tm, err := send(rctx, hc, w)
			if err == nil && resp.StatusCode/100 == 2 {
				br := newPeekBody(resp.Body)
				if _, perr := br.peek(); perr != nil && perr != io.EOF {
					resp.Body.Close()
					err = perr
				} else {
					resp.Body = br
				}
			}
			results <- result{resp: resp, tm: tm, err: err, cancel: cancel, hedge: hedge}
		}()
	}
	// The hedge covers a slow first byte on a live connection. While the
	// primary is still dialing or handshaking, a duplicate would only add
	// another handshake to the same slow edge: wait for the primary's
	// connection, then give it the full hedge delay.
	watch := newConnWatch()
	launch(e.hc, false, watch)
	timer := time.NewTimer(e.delay)
	defer timer.Stop()
	pending, hedged := 1, false
	var connected <-chan struct{} // set while a hedge waits on the primary's connection
	var firstErr *result
	tryHedge := func() {
		hedged = true
		if ctx.Err() == nil && acquire(false) {
			e.stats.hedges.Add(1)
			launch(e.hedge, true, nil)
			pending++
		}
	}
	cleanup := func(n int) {
		go func() {
			for ; n > 0; n-- {
				l := <-results
				if l.err == nil {
					l.resp.Body.Close()
				}
				l.cancel()
			}
		}()
	}
	for pending > 0 {
		select {
		case r := <-results:
			pending--
			if r.err == nil {
				// Cancel the loser now, not after its stalled body returns.
				for i, cancel := range cancels {
					if (i == 1) != r.hedge {
						cancel()
					}
				}
				if pending > 0 {
					cleanup(pending)
				}
				if r.hedge {
					e.stats.hedgeWins.Add(1)
				}
				r.resp.Body = cancelOnClose{ReadCloser: r.resp.Body, cancel: r.cancel}
				return r.resp, r.tm, nil
			}
			r.cancel()
			if firstErr == nil {
				firstErr = &r
			}
			if !hedged {
				tryHedge()
			}
		case <-timer.C:
			if hedged {
				break
			}
			if watch.connecting() {
				connected = watch.ready
				break
			}
			tryHedge()
		case <-connected:
			connected = nil
			if !hedged {
				timer.Reset(e.delay)
			}
		case <-ctx.Done():
			for _, cancel := range cancels {
				cancel()
			}
			cleanup(pending)
			return nil, timing{}, ctx.Err()
		}
	}
	return nil, firstErr.tm, firstErr.err
}

// stallBody ends a response whose body delivers no bytes for d by
// cancelling its request, so a stalled CDN connection fails like a dropped
// one (and is resumed) instead of blocking its reader forever. Close also
// returns the request's connection slot. Close may run concurrently with
// Read, so the timer is created up front and only touched through its
// (concurrency-safe) methods.
type stallBody struct {
	rc      io.ReadCloser
	stop    context.CancelFunc
	release func()
	d       time.Duration
	timer   *time.Timer // nil when d <= 0
	fired   atomic.Bool
}

func newStallBody(rc io.ReadCloser, d time.Duration, stop context.CancelFunc, release func()) *stallBody {
	b := &stallBody{rc: rc, stop: stop, release: release, d: d}
	if d > 0 {
		b.timer = time.AfterFunc(d, func() { b.fired.Store(true); b.stop() })
		b.timer.Stop()
	}
	return b
}

func (b *stallBody) Read(p []byte) (int, error) {
	if b.timer == nil {
		return b.rc.Read(p)
	}
	b.timer.Reset(b.d)
	n, err := b.rc.Read(p)
	b.timer.Stop()
	if err != nil && b.fired.Load() {
		err = errBodyStalled
	}
	return n, err
}

func (b *stallBody) Close() error {
	if b.timer != nil {
		b.timer.Stop()
	}
	err := b.rc.Close()
	b.stop()
	b.release()
	return err
}

type cancelOnClose struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (c cancelOnClose) Close() error {
	err := c.ReadCloser.Close()
	c.cancel()
	return err
}

// peekBody buffers the first byte of a response body.
type peekBody struct {
	rc   io.ReadCloser
	head []byte
	err  error
}

func newPeekBody(rc io.ReadCloser) *peekBody { return &peekBody{rc: rc} }

func (p *peekBody) peek() ([]byte, error) {
	if p.head == nil && p.err == nil {
		b := make([]byte, 1)
		n, err := p.rc.Read(b)
		for n == 0 && err == nil {
			n, err = p.rc.Read(b)
		}
		p.head, p.err = b[:n], err
	}
	return p.head, p.err
}

func (p *peekBody) Read(b []byte) (int, error) {
	if len(p.head) > 0 {
		n := copy(b, p.head)
		p.head = p.head[n:]
		return n, nil
	}
	if p.err != nil {
		return 0, p.err
	}
	return p.rc.Read(b)
}

func (p *peekBody) Close() error { return p.rc.Close() }

// connWatch reports whether a request is still setting up its connection
// (DNS, dial, TLS) or has one it can send on.
type connWatch struct {
	dialing atomic.Bool
	ready   chan struct{} // closed once the request has a connection
	once    sync.Once
}

func newConnWatch() *connWatch { return &connWatch{ready: make(chan struct{})} }

func (w *connWatch) setDialing() { w.dialing.Store(true) }
func (w *connWatch) gotConn()    { w.once.Do(func() { close(w.ready) }) }

// connecting is true while a dial or handshake is under way. A transport
// that reports no connection events (tests' fake round trippers) never is.
func (w *connWatch) connecting() bool {
	select {
	case <-w.ready:
		return false
	default:
		return w.dialing.Load()
	}
}

// reduceHostCap lowers the connection cap for raw's host for a cooldown
// after it reset a connection or answered 429.
func (e *Engine) reduceHostCap(raw string) {
	if e.limit != nil {
		e.limit.reduce(dialAddr(raw))
	}
}

func (e *Engine) send(ctx context.Context, hc *http.Client, u string, start, end int64, w *connWatch) (*http.Response, timing, error) {
	if err := core.SpendBackground(ctx, "bytes", "debrid", max(1, end-start+1)); err != nil {
		return nil, timing{}, err
	}
	var began time.Time
	var connStart, connect, ttfb, gotConn atomic.Int64
	reused := false
	dialing := func() {
		if w != nil {
			w.setDialing()
		}
	}
	trace := &httptrace.ClientTrace{
		GotConn: func(info httptrace.GotConnInfo) {
			gotConn.Store(int64(time.Since(began)))
			reused = info.Reused
			if w != nil {
				w.gotConn()
			}
		},
		DNSStart:          func(httptrace.DNSStartInfo) { dialing() },
		TLSHandshakeStart: dialing,
		ConnectStart: func(string, string) {
			dialing()
			connStart.CompareAndSwap(0, int64(time.Since(began)))
		},
		ConnectDone: func(_, _ string, err error) {
			if err == nil {
				connect.Store(int64(time.Since(began)) - connStart.Load())
			}
		},
		TLSHandshakeDone: func(tls.ConnectionState, error) {
			connect.Store(int64(time.Since(began)) - connStart.Load())
		},
		GotFirstResponseByte: func() { ttfb.Store(int64(time.Since(began))) },
	}
	req, err := http.NewRequestWithContext(httptrace.WithClientTrace(ctx, trace), http.MethodGet, u, nil)
	if err != nil {
		return nil, timing{}, fmt.Errorf("debrid: bad media URL: %w", core.ErrSourceUnavailable)
	}
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", start, end))
	req.Header.Set("User-Agent", "Ether")
	if p, ok := ctx.Value(directKey{}).(Payload); ok {
		// Addon links go through the SSRF guard, never the CDN pools.
		e.svc.direct.init()
		if hc == e.hedge {
			hc = e.svc.direct.hedge
		} else {
			hc = e.svc.direct.primary
		}
		req = prepareDirect(req, p)
	}
	host := req.URL.Host
	if err := e.backoff.wait(ctx, host); err != nil {
		return nil, timing{}, err
	}
	began = time.Now()
	resp, err := hc.Do(req)
	switch {
	case err == nil:
		e.backoff.success(host)
	case ctx.Err() == nil:
		// A connect, TLS or transport failure, not a cancelled request
		// (hedge losers and seeks cancel theirs).
		e.backoff.failure(host)
		if isConnReset(err) {
			e.reduceHostCap(u)
		}
	}
	tm := timing{connect: time.Duration(connect.Load()), ttfb: time.Duration(ttfb.Load())}
	if took := time.Since(began); took > time.Second {
		// A slow first byte stalls ffmpeg at a start or seek; say whether
		// the time went to waiting for a connection, dialing or the CDN.
		e.log.Info("slow media response", "took", took, "connWait", time.Duration(gotConn.Load())-tm.connect,
			"connect", tm.connect, "ttfb", tm.ttfb, "reused", reused, "bytes", end-start+1, "err", err != nil)
	}
	if err != nil {
		return nil, tm, fmt.Errorf("debrid: media request: %w: %v", core.ErrSourceUnavailable, sanitize(err))
	}
	return resp, tm, nil
}

// ---------------------------------------------------------------------------
// MediaSource.

// httpSource is one open handle on a debrid file. It implements
// core.MediaSource plus the context-aware read variants the playback layer
// type-asserts (ReadAtContext, ReadAtPrefetchContext, ReadMetadataAtContext).
type httpSource struct {
	f *remoteFile

	mu       sync.Mutex
	last     int64 // last block a live read touched
	raCtx    context.Context
	raCancel context.CancelFunc
	closed   bool
}

func (s *httpSource) Size() int64       { return s.f.size }
func (s *httpSource) Name() string      { p, _ := s.f.payload(); return p.Name() }
func (s *httpSource) ReleaseID() string { return s.f.id }

// Origin implements core.OriginSource with the debrid service's name.
func (s *httpSource) Origin() string {
	p, _ := s.f.payload()
	if p.IsDirect() {
		return "HTTP"
	}
	if label, ok := kindLabels[p.Kind]; ok {
		return label
	}
	if p.Kind != "" {
		return p.Kind
	}
	return "Debrid"
}

// RetryDirectPlay keeps the file representation stable across a temporary CDN
// failure: native clients retry the same Range URL without a new decision.
func (s *httpSource) RetryDirectPlay() bool { return true }

func (s *httpSource) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	s.raCancel()
	s.mu.Unlock()
	s.f.e.releaseFile(s.f)
	return nil
}

func (s *httpSource) ReadAt(p []byte, off int64) (int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), readAtDefaultBudget)
	defer cancel()
	return s.read(ctx, p, off, fetchUrgent, true)
}

func (s *httpSource) ReadAtContext(ctx context.Context, p []byte, off int64) (int, error) {
	return s.read(ctx, p, off, fetchUrgent, true)
}

// ReadAtPrefetchContext reads at background priority without read-ahead.
func (s *httpSource) ReadAtPrefetchContext(ctx context.Context, p []byte, off int64) (int, error) {
	return s.read(ctx, p, off, fetchPrefetch, false)
}

// ReadMetadataAtContext serves container-index reads: urgent, but they jump
// around the file, so they neither count as seeks nor trigger read-ahead.
func (s *httpSource) ReadMetadataAtContext(ctx context.Context, p []byte, off int64) (int, error) {
	return s.read(ctx, p, off, fetchUrgent, false)
}

func (s *httpSource) read(ctx context.Context, p []byte, off int64, mode fetchMode, live bool) (int, error) {
	f := s.f
	if off < 0 {
		return 0, fmt.Errorf("debrid: negative offset")
	}
	if off >= f.size {
		return 0, io.EOF
	}
	if len(p) == 0 {
		return 0, nil
	}
	end := min(off+int64(len(p)), f.size)
	first, lastBlock := off/f.e.block, (end-1)/f.e.block
	maxRun, _, _ := f.e.tuning()
	// Blocks are published as their bytes arrive, so a long run costs no
	// first-byte latency. Debrid CDNs rate-limit requests, not bytes: one
	// request that covers what the reader will want next beats a ramp of
	// small ones (TorBox answers bursts with a five-minute 429).
	runLen := min(maxRun, freshRunBlocks)
	if live {
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			return 0, errors.New("debrid: read on closed source")
		}
		// A short forward skip onto blocks read-ahead already holds or is
		// fetching keeps those runs; anything else is a seek.
		sequential := s.last >= 0 && first >= s.last && first <= s.last+2
		if !sequential && s.last >= 0 && first > s.last {
			f.bmu.Lock()
			sequential = f.presentLocked(first)
			f.bmu.Unlock()
		}
		if !sequential && s.last >= 0 {
			// A seek: abandon speculative reads for the old position and
			// serve the new one with a fresh request immediately.
			s.raCancel()
			s.raCtx, s.raCancel = context.WithCancel(f.ctx)
		}
		if sequential {
			runLen = min(maxRun, 4)
		}
		s.last = lastBlock
		s.mu.Unlock()
		// Start read-ahead past this read before blocking, so the burst
		// runs in parallel with the request for the bytes asked for.
		s.readAhead(lastBlock)
	}
	n := 0
	for idx := first; idx <= lastBlock; idx++ {
		data, err := f.block(ctx, idx, mode, min(maxRun, max(runLen, lastBlock-idx+1)), nil)
		if err != nil {
			return n, err
		}
		from := int64(0)
		if idx == first {
			from = off - idx*f.e.block
		}
		n += copy(p[n:], data[from:])
	}
	if int64(n) < int64(len(p)) {
		return n, io.EOF
	}
	return n, nil
}

// readAhead keeps a throughput-scaled window ahead of the reader in flight,
// in runs that start at freshRunBlocks and double up to the chunk size.
func (s *httpSource) readAhead(from int64) {
	f := s.f
	maxRun, _, fixed := f.e.tuning()
	if fixed < 0 {
		return
	}
	window := fixed
	if window == 0 {
		f.bmu.Lock()
		tput := f.tput
		f.bmu.Unlock()
		window = min(maxReadAheadBytes, max(minReadAheadBytes, int64(tput*readAheadSeconds)))
	}
	s.mu.Lock()
	ctx := s.raCtx
	closed := s.closed
	s.mu.Unlock()
	if closed || ctx.Err() != nil {
		return
	}
	lastIdx := min(f.nblocks-1, from+window/f.e.block)
	run := min(maxRun, freshRunBlocks)
	for idx := from + 1; idx <= lastIdx; {
		f.bmu.Lock()
		present := f.presentLocked(idx)
		f.bmu.Unlock()
		if present {
			idx++
			continue
		}
		// Read-ahead runs never queue: a queued run would hold flights a
		// reader may soon wait on. Without a free slot, the next read
		// tops the window up.
		select {
		case f.raSlots <- struct{}{}:
		default:
			return
		}
		n := min(run, lastIdx-idx+1)
		if _, started := f.startRun(ctx, idx, n, fetchReadAhead, true); !started {
			<-f.raSlots
		}
		idx += n
		run = min(run*2, maxRun)
	}
}
