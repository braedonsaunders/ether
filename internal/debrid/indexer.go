package debrid

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/flight"
)

// TorrentResolver fills a torrent release's info hash and magnet (by
// downloading its .torrent when the indexer gave neither). Implemented by
// *newznab.Manager.
type TorrentResolver interface {
	ResolveTorrent(ctx context.Context, r core.Release) (core.Release, error)
}

// Indexer wraps the aggregate indexer: torrent results are kept only while
// a debrid provider is enabled (annotated with cache availability where a
// provider offers a bulk check), and FetchNZB returns a debrid payload for
// torrents while Usenet releases go to the inner indexer unchanged.
//
// A Stremio addon's own HTTP stream links (core.ProtocolHTTP) need no
// debrid account. Without one, a torrent the addon also linked plays
// through that link instead; with cached-only on, links the addon labels
// only as uncached are left out (the label is a hint, never proof: the
// probe in FetchNZB decides). Usenet results are left out while no Usenet
// provider is enabled.
type Indexer struct {
	inner   core.Indexer
	svc     *Service
	flights flight.Group[string, []byte]
	// usenetReady reports whether a Usenet provider is enabled; nil means
	// unknown (no debrid-only source check).
	usenetReady func() bool
}

var _ core.Indexer = (*Indexer)(nil)

// NewIndexer wraps inner.
func NewIndexer(inner core.Indexer, svc *Service) *Indexer { return &Indexer{inner: inner, svc: svc} }

// SetUsenetReady wires the "is a Usenet provider enabled" check used by
// MissingSources. Call before the indexer is used.
func (x *Indexer) SetUsenetReady(fn func() bool) { x.usenetReady = fn }

// sourceInventory is what MissingSources needs from the inner indexer
// (implemented by *newznab.Manager).
type sourceInventory interface {
	MissingSources() error
	HasTorrentIndexer() bool
}

// MissingSources reports core.ErrNoSources when no indexer is enabled, and
// core.ErrNoTorrentSources when debrid is the only way to play (no Usenet
// provider) but no enabled indexer returns torrents.
func (x *Indexer) MissingSources() error {
	inv, ok := x.inner.(sourceInventory)
	if !ok {
		return nil
	}
	if err := inv.MissingSources(); err != nil {
		return err
	}
	if x.usenetReady != nil && !x.usenetReady() && x.svc != nil && x.svc.Enabled() && !inv.HasTorrentIndexer() {
		return core.ErrNoTorrentSources
	}
	return nil
}

// Inner returns the wrapped indexer.
func (x *Indexer) Inner() core.Indexer { return x.inner }

// Name implements core.Indexer.
func (x *Indexer) Name() string { return x.inner.Name() }

// Search implements core.Indexer.
func (x *Indexer) Search(ctx context.Context, req core.SearchRequest) ([]core.Release, error) {
	return x.SearchWith(ctx, req, nil)
}

// SearchWith exposes arrivals from both sources while cache discovery continues.
// The resolver targets and validates speculative torrents before selecting one.
func (x *Indexer) SearchWith(ctx context.Context, req core.SearchRequest, arrived func([]core.Release)) ([]core.Release, error) {
	started := time.Now()
	early := x.earlyCacheChecks(ctx)
	if early != nil {
		defer func() { early.cancel(); early.wg.Wait() }()
	}
	var results []core.Release
	var err error
	emit := func(batch []core.Release) {
		x.svc.ApplyHints(ctx, batch)
		if early != nil {
			early.add(batch)
		}
		if arrived != nil {
			var eligible []core.Release
			for _, r := range batch {
				if r, ok := x.playable(r); ok {
					eligible = append(eligible, r)
				}
			}
			if len(eligible) > 0 {
				arrived(eligible)
			}
		}
	}
	if streaming, ok := x.inner.(core.StreamingIndexer); ok && (early != nil || arrived != nil) {
		results, err = streaming.SearchWith(ctx, req, emit)
	} else {
		results, err = x.inner.Search(ctx, req)
	}
	searchDuration := time.Since(started)
	var cacheDuration time.Duration
	var hashCount int
	defer func() {
		x.svc.log.Info("source search completed", "indexers", searchDuration,
			"cacheDiscovery", cacheDuration, "torrentHashes", hashCount,
			"results", len(results), "took", time.Since(started), "success", err == nil)
	}()
	if err != nil {
		return results, err
	}
	enabled := x.svc.Enabled()
	if enabled {
		x.svc.ApplyHints(ctx, results)
	}
	var torrents []string
	out := results[:0:0]
	for _, r := range results {
		r, ok := x.playable(r)
		if !ok {
			continue
		}
		if r.Protocol != core.ProtocolTorrent {
			out = append(out, r)
			continue
		}
		if r.InfoHash != "" {
			torrents = append(torrents, r.InfoHash)
		}
		out = append(out, r)
	}
	if len(torrents) == 0 {
		return out, nil
	}
	// One bulk availability call per provider that offers it: torrents
	// known to be uncached everywhere are dropped (they would only burn
	// health-check attempts), and known-cached torrents move ahead of
	// unknown ones of equal rank.
	cacheStart := time.Now()
	var cached map[string]bool
	var definitive bool
	if early != nil {
		// Most hashes were checked while slower indexers were still
		// searching; only stragglers are checked now.
		cached, definitive = early.finish(ctx, torrents)
	} else {
		cctx, cancel := context.WithTimeout(ctx, 8*time.Second)
		cached, definitive = x.svc.CheckCached(cctx, torrents)
		cancel()
	}
	cacheDuration, hashCount = time.Since(cacheStart), len(torrents)
	kept := out[:0]
	anyCached := false
	// With cached-only off an uncached torrent is still playable later
	// (playback sends it to download), so it stays, ranked after cached ones.
	dropUncached := definitive && x.svc.Config().CachedOnly
	for _, r := range out {
		if r.Protocol == core.ProtocolTorrent && r.InfoHash != "" && dropUncached && !cached[r.InfoHash] {
			continue
		}
		if r.Protocol == core.ProtocolTorrent && (cached[r.InfoHash] || x.svc.KnownCached(r.InfoHash)) {
			anyCached = true
		}
		kept = append(kept, r)
	}
	// When an episode search returned only uncached single-episode torrents
	// (preventing the indexer's own empty-result fallback from querying
	// season packs), search for season packs (Episode = 0) and keep cached
	// packs so older shows whose episodes live only in cached season packs
	// still resolve.
	if enabled && !anyCached && len(torrents) > 0 && req.Type == core.TypeEpisode && req.Season > 0 && req.Episode > 0 && ctx.Err() == nil {
		packCached, packs := x.searchSeasonPacks(ctx, req)
		for h, ok := range packCached {
			if ok {
				if cached == nil {
					cached = map[string]bool{}
				}
				cached[h] = true
			}
		}
		seenID := make(map[string]bool, len(kept))
		for _, r := range kept {
			seenID[r.ID] = true
		}
		for _, r := range packs {
			if !seenID[r.ID] {
				seenID[r.ID] = true
				kept = append(kept, r)
			}
		}
	}
	rank := func(r core.Release) int {
		if r.Protocol == core.ProtocolTorrent && (cached[r.InfoHash] || x.svc.KnownCached(r.InfoHash)) {
			return 0
		}
		return 1
	}
	sort.SliceStable(kept, func(i, j int) bool { return rank(kept[i]) < rank(kept[j]) })
	return kept, nil
}

// playable adapts a search result to what this server can stream, or
// reports false: torrents need an enabled debrid provider (or else play
// through the addon link they came with), Usenet needs a Usenet provider,
// and with cached-only on an HTTP link the addon labels only as uncached
// is left out.
func (x *Indexer) playable(r core.Release) (core.Release, bool) {
	switch r.Protocol {
	case core.ProtocolTorrent:
		if x.svc.Enabled() {
			return r, true
		}
		if r.StreamURL == "" {
			return r, false
		}
		r.Protocol = core.ProtocolHTTP
		fallthrough
	case core.ProtocolHTTP:
		if r.StreamURL == "" {
			return r, false
		}
		if x.svc.Config().CachedOnly && len(r.DebridUncached) > 0 && len(r.DebridCached) == 0 {
			return r, false
		}
		return r, true
	case core.ProtocolUsenet:
		return r, x.usenetReady == nil || x.usenetReady()
	}
	return r, true
}

// searchSeasonPacks queries inner for season-level releases (Episode = 0)
// and bulk-checks their debrid cache availability.
func (x *Indexer) searchSeasonPacks(ctx context.Context, req core.SearchRequest) (map[string]bool, []core.Release) {
	packReq := req
	packReq.Episode = 0
	early := x.earlyCacheChecks(ctx)
	if early != nil {
		defer func() { early.cancel(); early.wg.Wait() }()
	}
	var results []core.Release
	var err error
	if streaming, ok := x.inner.(core.StreamingIndexer); ok && early != nil {
		results, err = streaming.SearchWith(ctx, packReq, early.add)
	} else {
		results, err = x.inner.Search(ctx, packReq)
	}
	if err != nil || len(results) == 0 {
		return nil, nil
	}
	x.svc.ApplyHints(ctx, results)
	var hashes []string
	var packs []core.Release
	for _, r := range results {
		if r.Protocol != core.ProtocolTorrent {
			continue
		}
		if r.InfoHash != "" {
			hashes = append(hashes, r.InfoHash)
		}
		packs = append(packs, r)
	}
	if len(packs) == 0 {
		return nil, nil
	}
	var cached map[string]bool
	var definitive bool
	if len(hashes) > 0 {
		if early != nil {
			cached, definitive = early.finish(ctx, hashes)
		} else {
			cctx, cancel := context.WithTimeout(ctx, 8*time.Second)
			cached, definitive = x.svc.CheckCached(cctx, hashes)
			cancel()
		}
	}
	kept := packs[:0]
	for _, r := range packs {
		if r.InfoHash != "" && definitive && !cached[r.InfoHash] {
			continue
		}
		kept = append(kept, r)
	}
	return cached, kept
}

// SearchRescue implements second-pass rescue search through the inner
// indexer, annotating discovered torrent releases with debrid cache status.
func (x *Indexer) SearchRescue(ctx context.Context, req core.SearchRequest) ([]core.Release, error) {
	rs, ok := x.inner.(core.RescueIndexer)
	if !ok {
		return nil, nil
	}
	early := x.earlyCacheChecks(ctx)
	if early != nil {
		defer func() { early.cancel(); early.wg.Wait() }()
	}
	var results []core.Release
	var err error
	if streaming, ok := x.inner.(core.StreamingRescueIndexer); ok && early != nil {
		results, err = streaming.SearchRescueWith(ctx, req, early.add)
	} else {
		results, err = rs.SearchRescue(ctx, req)
	}
	if err != nil || len(results) == 0 {
		return results, err
	}
	enabled := x.svc.Enabled()
	if enabled {
		x.svc.ApplyHints(ctx, results)
	}
	var torrents []string
	out := results[:0:0]
	for _, r := range results {
		r, ok := x.playable(r)
		if !ok {
			continue
		}
		if r.Protocol != core.ProtocolTorrent {
			out = append(out, r)
			continue
		}
		if r.InfoHash != "" {
			torrents = append(torrents, r.InfoHash)
		}
		out = append(out, r)
	}
	if len(torrents) == 0 {
		return out, nil
	}
	var cached map[string]bool
	var definitive bool
	if early != nil {
		cached, definitive = early.finish(ctx, torrents)
	} else {
		cctx, cancel := context.WithTimeout(ctx, 8*time.Second)
		cached, definitive = x.svc.CheckCached(cctx, torrents)
		cancel()
	}
	kept := out[:0]
	dropUncached := definitive && x.svc.Config().CachedOnly
	for _, r := range out {
		if r.Protocol == core.ProtocolTorrent && r.InfoHash != "" && dropUncached && !cached[r.InfoHash] {
			continue
		}
		kept = append(kept, r)
	}
	rank := func(r core.Release) int {
		if r.Protocol == core.ProtocolTorrent && (cached[r.InfoHash] || x.svc.KnownCached(r.InfoHash)) {
			return 0
		}
		return 1
	}
	sort.SliceStable(kept, func(i, j int) bool { return rank(kept[i]) < rank(kept[j]) })
	return kept, nil
}

// FetchNZB implements core.Indexer. For a torrent it returns the debrid
// payload of the release's main video file (or the wanted episode of a
// season pack); for anything else, the inner indexer's NZB.
func (x *Indexer) FetchNZB(ctx context.Context, r core.Release) ([]byte, error) {
	if r.Protocol == core.ProtocolHTTP {
		return x.svc.DirectPayload(ctx, r)
	}
	if r.Protocol != core.ProtocolTorrent {
		return x.inner.FetchNZB(ctx, r)
	}
	if !x.svc.Enabled() {
		return nil, fmt.Errorf("torrent release %q: %w", r.Name, ErrNoProvider)
	}
	return x.fetchTorrent(ctx, r)
}

func (x *Indexer) fetchTorrentOnce(ctx context.Context, r core.Release) (data []byte, err error) {
	started := time.Now()
	var descriptor, provider time.Duration
	reused := false
	defer func() {
		x.svc.log.Info("debrid payload prepared", "release", r.ID, "reused", reused,
			"descriptor", descriptor, "provider", provider, "took", time.Since(started), "success", err == nil)
	}()
	if p, ok := x.svc.reusablePayload(ctx, r); ok {
		reused = true
		return p.Encode(), nil
	}
	if r.InfoHash == "" {
		tr, ok := x.inner.(TorrentResolver)
		if !ok {
			return nil, fmt.Errorf("debrid: torrent %q has no info hash: %w", r.Name, core.ErrReleaseRejected)
		}
		phase := time.Now()
		resolved, err := tr.ResolveTorrent(ctx, r)
		descriptor = time.Since(phase)
		if err != nil {
			return nil, err
		}
		r = resolved
	}
	phase := time.Now()
	data, err = x.svc.Acquire(ctx, r)
	provider = time.Since(phase)
	return data, err
}

// earlyChecks runs debrid cache checks per indexer as results arrive.
type earlyChecks struct {
	x       *Indexer
	ctx     context.Context
	cancel  context.CancelFunc
	mu      sync.Mutex
	wg      sync.WaitGroup
	asked   map[string]bool
	cached  map[string]bool
	pending []string
	running bool
	sure    bool // every check so far was definitive
}

func (x *Indexer) earlyCacheChecks(ctx context.Context) *earlyChecks {
	if !x.svc.Enabled() {
		return nil
	}
	cctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	return &earlyChecks{x: x, ctx: cctx, cancel: cancel, asked: map[string]bool{}, cached: map[string]bool{}, sure: true}
}

// add starts a cache check for the torrent hashes in one indexer's results.
func (e *earlyChecks) add(rs []core.Release) {
	e.mu.Lock()
	for _, r := range rs {
		if r.Protocol == core.ProtocolTorrent && r.InfoHash != "" && !e.asked[r.InfoHash] {
			e.asked[r.InfoHash] = true
			e.pending = append(e.pending, r.InfoHash)
		}
	}
	if len(e.pending) == 0 || e.running || e.ctx.Err() != nil {
		e.mu.Unlock()
		return
	}
	e.running = true
	e.wg.Add(1)
	e.mu.Unlock()
	go func() {
		defer e.wg.Done()
		for {
			// Streaming feeds emit individual items. Coalesce those into bulk
			// provider calls, with one bounded consumer rather than one HTTP
			// request goroutine for every torrent in a large feed.
			timer := time.NewTimer(20 * time.Millisecond)
			select {
			case <-e.ctx.Done():
				timer.Stop()
			case <-timer.C:
			}
			e.mu.Lock()
			if e.ctx.Err() != nil || len(e.pending) == 0 {
				e.running = false
				e.mu.Unlock()
				return
			}
			hashes := e.pending
			e.pending = nil
			e.mu.Unlock()
			cached, definitive := e.x.svc.CheckCached(e.ctx, hashes)
			e.mu.Lock()
			for h, ok := range cached {
				if ok {
					e.cached[h] = true
				}
			}
			e.sure = e.sure && definitive
			if len(e.pending) == 0 {
				e.running = false
				e.mu.Unlock()
				return
			}
			e.mu.Unlock()
		}
	}()
}

// finish checks any hashes not yet asked about, waits for every check and
// returns the merged availability.
func (e *earlyChecks) finish(ctx context.Context, hashes []string) (map[string]bool, bool) {
	defer e.cancel()
	e.add(hashOnly(hashes))
	e.wg.Wait()
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make(map[string]bool, len(e.cached))
	for h := range e.cached {
		out[h] = true
	}
	return out, e.sure && ctx.Err() == nil
}

func hashOnly(hashes []string) []core.Release {
	out := make([]core.Release, len(hashes))
	for i, h := range hashes {
		out[i] = core.Release{Protocol: core.ProtocolTorrent, InfoHash: h}
	}
	return out
}
