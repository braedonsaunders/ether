package playback

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/releasepolicy"
	"github.com/braedonsaunders/ether/internal/vault"
)

const (
	preResolveTTL = 60 * time.Minute
	// warmMissTTL is how long a warm-up that found no source keeps the
	// same viewer, device and title from warming again. Clients re-request
	// a detail page many times while it is open; each warm-up searches
	// every indexer and may grab NZBs, so a title whose top releases are
	// broken would otherwise walk further down its list on every refresh
	// (#224). Play is unaffected: it always resolves in full.
	warmMissTTL   = 15 * time.Minute
	maxWarmMisses = 1024
	// takeWarmBudget is how long Play waits for an in-flight warm-up
	// before resolving itself. A usenet RAR can sit in probe/prefetch
	// for tens of seconds; a cached debrid remux must not wait behind it.
	takeWarmBudget = 400 * time.Millisecond
)

// parityRetainWarm is nil in production, so warm-up eviction is unchanged.
// A plexparity build points it at a function that keeps fixture probes, which
// otherwise get dropped once thirty-two warm entries are recorded.
var parityRetainWarm func(identity) bool

func warmRetained(id identity) bool {
	return parityRetainWarm != nil && parityRetainWarm(id)
}

const (
	detailPrefetchBytes  = 2 << 20
	bigWarmPrefetchBytes = 48 << 20
	maxWarmPrefetchBytes = 64 << 20
)

type warmEntry struct {
	offset        time.Duration
	windowAt      time.Time
	windowRunning bool
	profileKey    string // profileKey of profileObj
	profileObj    core.QualityProfile
	caps          core.ClientCaps
	done          chan struct{}
	ready         sync.Once
	foreground    chan context.Context
	started       time.Time
	expires       time.Time
	cancelMu      sync.Mutex
	cancel        context.CancelFunc
	runtime       time.Duration // title runtime, for size rules without a probe
	res           *core.Resolution
	sourceReady   bool // mu; a warm source is resolved before its probe completes
	probe         *probeCache
	// originalAudio is set when the warmed profile compares audio with
	// the catalogue language. originalLanguage is the Title.Language
	// that Resolve saw. A later catalogue correction must not reuse it.
	originalAudio    bool
	originalLanguage string
	// opened is closed once the opening window is proven. For debrid/HTTP
	// that is the probe; for usenet it is a successful opening prefetch.
	// nil means the entry predates this field and is treated as proven.
	opened chan struct{}
	openDo sync.Once
	// detailOnly marks the viewer's last played release, warmed for a
	// detail page's tracks only: it is never prefetched and Play never
	// opens it.
	detailOnly bool
	// playProbe marks a warm-up started directly for explicit play-time
	// metadata probing (foreground playback rules, zero prefetch).
	playProbe bool
	// promoted is closed when a play-intent wait promotes an already
	// in-flight speculative warm-up without tying its lifetime to the
	// metadata request.
	promoted   chan struct{}
	promoteDo  sync.Once
	promotedAt atomic.Bool
}

func (e *warmEntry) markPromoted() {
	if e == nil {
		return
	}
	e.promotedAt.Store(true)
	e.promoteDo.Do(func() {
		if e.promoted != nil {
			close(e.promoted)
		}
	})
}

func (e *warmEntry) isPromoted() bool {
	return e != nil && e.promotedAt.Load()
}

func (e *warmEntry) markOpeningReady() {
	if e == nil {
		return
	}
	e.openDo.Do(func() {
		if e.opened != nil {
			close(e.opened)
		}
	})
}

func usenetWarm(res *core.Resolution) bool {
	return res != nil && res.Release.Protocol == core.ProtocolUsenet
}

func (e *warmEntry) setCancel(cancel context.CancelFunc) {
	e.cancelMu.Lock()
	e.cancel = cancel
	e.cancelMu.Unlock()
}

func (e *warmEntry) abort() {
	e.cancelMu.Lock()
	cancel := e.cancel
	e.cancelMu.Unlock()
	if cancel != nil {
		cancel()
	}
}

type probeCache struct {
	warmReads *core.WarmReadWindow
	releaseID string
	info      core.MediaInfo
	points    []IndexPoint
	// needsAugment marks facts rebuilt from the persisted probe, which
	// strips per-viewer sidecar enrichment: AugmentMedia must run again
	// before the generation is published. Warm in-memory facts were
	// already augmented when they were probed.
	needsAugment bool
	// nzbHash binds persisted facts to the exact NZB payload they were
	// probed from; release ID and file size alone do not prove the
	// opened source is the same media. Empty for in-memory warm facts.
	nzbHash string
}

// Prewarm implements core.Prewarmer: it resolves, opens, probes (including
// the container keyframe index) and pre-buffers a title ahead of Play, so
// the later Prepare for the same user, client and title needs no resolver
// or probe round trip. Request cancellation does not cancel a warm-up;
// manager shutdown always does. It neither reserves a live stream nor
// writes resume/history. False means skipped.
//
// Budgets: two warm-ups in flight server-wide, one per user, thirty detail
// (or six speculative) requests/minute/user. A detail warm-up runs at most 45s,
// prefetches up to 40s of source media (256 MiB cap) and is skipped while
// anything is playing. Trailer/post-play warm-ups may run during playback
// (DuringPlayback), prefetch 48 MiB by default (hard cap 64 MiB) and get 45s.
// Prefetch goes through Engine.Prefetch, which runs at core.PriorityPrefetch
// and yields to live streams.
func (m *Manager) Prewarm(ctx context.Context, user *core.User, key core.RatingKey, caps core.ClientCaps, opts core.PrewarmOptions) bool {
	return m.prewarm(ctx, user, key, caps, opts, false, false)
}

// RevealDetail triggers a background season-drop check for a show, season, or
// episode detail open unless the client is reading pages like a crawler.
func (m *Manager) RevealDetail(user *core.User, clientID string, key core.RatingKey) {
	if m == nil || m.deps.SeasonRevealer == nil || user == nil || !isTVDetailKey(key) {
		return
	}
	if core.DetailBurst(user.ID, clientID, key) {
		return
	}
	m.deps.SeasonRevealer(key)
}

func isTVDetailKey(key core.RatingKey) bool {
	if key == 0 || key.IsPluginKey() || key.TMDB() <= 0 {
		return false
	}
	switch key.Kind() {
	case core.KindShow:
		return true
	case core.KindSeason, core.KindEpisode:
		return key.Season() > 0
	default:
		return false
	}
}

// ProbeMetadata resolves and probes the detail page's source without creating a
// playback session or prefetching video. Owner cache/warm-up settings and
// admission limits apply. Slow probes finish in the background; the caller
// can render estimated metadata until exact facts become available.
func (m *Manager) ProbeMetadata(ctx context.Context, user *core.User, key core.RatingKey, caps core.ClientCaps) error {
	// A caller deadline bounds only the wait: one already past still
	// schedules the warm-up. Cancellation stops before any work.
	if err := ctx.Err(); err != nil && !errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	if _, _, ok := m.ResumedTracks(ctx, user, key, caps); ok {
		m.RevealDetail(user, caps.ClientID, key)
		m.scheduleVersionProbes(user, key, caps)
		return nil
	}
	if _, ok := m.WarmProbeInfo(user, caps, key); ok {
		m.RevealDetail(user, caps.ClientID, key)
		m.scheduleVersionProbes(user, key, caps)
		return nil
	}
	if user != nil && core.DetailBurst(user.ID, caps.ClientID, key) {
		return core.ErrPrepareSlow
	}
	if user != nil && m.deps.SeasonRevealer != nil && isTVDetailKey(key) {
		m.deps.SeasonRevealer(key)
	}
	if core.LocalResponse(ctx) {
		if user == nil {
			return core.ErrPrepareSlow
		}
		m.mu.Lock()
		entry := m.findWarmEntryLocked(user, caps, key, m.now())
		ready := entry != nil && (entry.sourceReady || entry.res != nil)
		m.mu.Unlock()
		if !ready {
			// An already stored source may need its first probe after restart.
			// Without such a source the page must not resolve/search to wait.
			point, err := m.resumeFor(ctx, user, key, caps, false)
			if err != nil || point == nil {
				return core.ErrPrepareSlow
			}
		}
	}
	// Viewing the detail page admits one bounded on-demand open, just as
	// playback intent does, while metadata mode still forbids video prefetch.
	m.prewarm(ctx, user, key, caps, core.PrewarmOptions{Reason: core.PrewarmDetail, PlayIntent: true}, true, false)
	if _, ok := m.WaitWarmProbe(ctx, user, caps, key, probeMetadataWait(ctx)); ok {
		return nil
	}
	if err := ctx.Err(); err != nil && !errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return core.ErrPrepareSlow
}

// defaultProbeMetadataWait bounds ProbeMetadata when the caller sets no
// deadline of its own.
const defaultProbeMetadataWait = 1500 * time.Millisecond

// maxProbeMetadataWait caps a caller-declared deadline: the Bedrock detail
// allowance is 4 s, and no caller may hold a response longer for a probe.
const maxProbeMetadataWait = 4 * time.Second

// maxPlayProbeMetadataWait caps the bounded wait for explicit play-intent
// metadata re-asks (such as Plezy's first play), where a cold remux resolve
// can exceed the 4 s browsing allowance. Moving that wait onto the metadata
// re-ask costs no time to first frame because the following stream request
// adopts the same warm entry.
const maxPlayProbeMetadataWait = 12 * time.Second

// probeMetadataWait is how long ProbeMetadata holds the caller for the probe.
// A caller that declares a deadline owns the allowance: Plex Android and iOS
// build their subtitle picker from the response being written, so the probe
// gets the whole budget the caller granted. Without a deadline the wait stays
// short. The warm-up itself is never cancelled; a probe that finishes later is
// still adopted by later reads.
func probeMetadataWait(ctx context.Context) time.Duration {
	if dl, ok := ctx.Deadline(); ok {
		return min(time.Until(dl), maxProbeMetadataWait)
	}
	return defaultProbeMetadataWait
}

func playProbeMetadataWait(ctx context.Context) time.Duration {
	if dl, ok := ctx.Deadline(); ok {
		return min(time.Until(dl), maxPlayProbeMetadataWait)
	}
	return maxPlayProbeMetadataWait
}

var _ core.PlayMetadataProber = (*Manager)(nil)

// ProbePlayMetadata joins or starts the shared resolve+probe flight for a
// play-time metadata re-ask (e.g. Plezy's first play) and waits bounded by
// ctx (capped at maxPlayProbeMetadataWait) until probed tracks are ready.
//
// Unlike browsing ProbeMetadata, this carries explicit play intent:
//   - LocalResponse, speculation/detail-warmup settings, DetailBurst, and a
//     prior speculative warmMiss never short-circuit it.
//   - An already in-flight speculative warm-up is promoted and joined; if that
//     speculative attempt finishes without a probe while wait budget remains,
//     one foreground play-intent flight is started in its place.
//   - A flight started here runs under foreground playback resolution rules
//     with zero video prefetch, publishing into m.warm so the following stream
//     request adopts the same resolved source and probe without repeating work.
//   - Caller cancellation or deadline expiry ends only the wait; the shared
//     flight keeps running for the stream request.
func (m *Manager) ProbePlayMetadata(ctx context.Context, user *core.User, key core.RatingKey, caps core.ClientCaps) error {
	if err := ctx.Err(); err != nil && !errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	if user == nil || caps.ClientID == "" {
		return core.ErrPrepareSlow
	}
	if ps, ok := m.SessionFor(user.ID, caps.ClientID, key); ok && ps != nil && !ps.Info.Estimated && len(ps.Info.Streams) > 0 {
		return nil
	}
	if _, _, ok := m.ResumedTracks(ctx, user, key, caps); ok {
		return nil
	}
	if _, ok := m.WarmProbeInfo(user, caps, key); ok {
		m.mu.Lock()
		if e := m.findWarmEntryLocked(user, caps, key, m.now()); e != nil {
			e.markPromoted()
		}
		m.mu.Unlock()
		return nil
	}
	budget := playProbeMetadataWait(ctx)
	deadline := time.Now().Add(budget)
	id := identity{userID: user.ID, clientID: caps.ClientID, key: key, surface: m.playbackSurface(string(caps.Protocol))}

	for attempt := 0; attempt < 2; attempt++ {
		now := m.now()
		m.mu.Lock()
		if m.closed {
			m.mu.Unlock()
			return core.ErrPrepareSlow
		}
		e := m.warm[id]
		if e != nil && !now.Before(e.expires) {
			delete(m.warm, id)
			e = nil
		}
		var inFlight bool
		var wasSpeculative bool
		if e != nil {
			select {
			case <-e.done:
				if e.probe == nil || e.probe.info.Estimated || len(e.probe.info.Streams) == 0 {
					delete(m.warm, id)
					e = nil
				}
			default:
				inFlight = true
				wasSpeculative = !e.playProbe
			}
		}
		m.mu.Unlock()

		if e != nil && !inFlight {
			if _, ok := m.WarmProbeInfo(user, caps, key); ok {
				return nil
			}
			return core.ErrPrepareSlow
		}

		if !inFlight {
			m.prewarm(ctx, user, key, caps, core.PrewarmOptions{Reason: core.PrewarmDetail, PlayIntent: true}, true, true)
			now = m.now()
			m.mu.Lock()
			e = m.warm[id]
			if e != nil && !now.Before(e.expires) {
				e = nil
			}
			m.mu.Unlock()
			if e == nil {
				if _, ok := m.WarmProbeInfo(user, caps, key); ok {
					return nil
				}
				return core.ErrPrepareSlow
			}
			wasSpeculative = !e.playProbe
		}

		if wasSpeculative {
			e.markPromoted()
		}

		wait := time.Until(deadline)
		if wait <= 0 {
			if _, ok := m.WarmProbeInfo(user, caps, key); ok {
				return nil
			}
			return core.ErrPrepareSlow
		}
		timer := time.NewTimer(wait)
		select {
		case <-e.done:
			timer.Stop()
		case <-ctx.Done():
			timer.Stop()
			if err := ctx.Err(); err != nil && !errors.Is(err, context.DeadlineExceeded) {
				return err
			}
			return core.ErrPrepareSlow
		case <-timer.C:
			return core.ErrPrepareSlow
		}

		if _, ok := m.WarmProbeInfo(user, caps, key); ok {
			return nil
		}
		if !wasSpeculative {
			return core.ErrPrepareSlow
		}
	}
	return core.ErrPrepareSlow
}

func (m *Manager) prewarm(requestCtx context.Context, user *core.User, key core.RatingKey, caps core.ClientCaps, opts core.PrewarmOptions, metadata, playProbe bool) bool {
	cachedMetadata := metadata && !playProbe && core.LocalResponse(requestCtx)
	if user == nil || caps.ClientID == "" || (!playProbe && caps.Product == "") {
		return false
	}
	reason := opts.Reason
	if reason != core.PrewarmTrailer && reason != core.PrewarmPostPlay && reason != core.PrewarmBrowse && reason != core.PrewarmNextEpisode {
		reason = core.PrewarmDetail
	}
	if !playProbe && reason == core.PrewarmDetail && !opts.DuringPlayback {
		if core.DetailBurst(user.ID, caps.ClientID, key) {
			return false
		}
		if m.deps.SeasonRevealer != nil && isTVDetailKey(key) {
			m.deps.SeasonRevealer(key)
		}
	}
	cfg := m.config().Cache
	if !playProbe && (cfg.DisableSpeculation || cfg.Mode == "off" || (cfg.SpeculativeWarming == "off" && reason != core.PrewarmNextEpisode && !metadata) || (reason == core.PrewarmDetail && cfg.DetailWarmup == "off")) {
		return false
	}
	automaticReserve := reason == core.PrewarmDetail && opts.PrefetchBytes <= 0 && cfg.DetailBytes <= 0
	prefetchBytes := opts.PrefetchBytes
	if reason == core.PrewarmDetail && cfg.DetailBytes > 0 {
		prefetchBytes = cfg.DetailBytes
	}
	if prefetchBytes <= 0 {
		switch reason {
		case core.PrewarmDetail:
			prefetchBytes = detailPrefetchBytes
		case core.PrewarmBrowse:
			// Resolve + open the cached/debrid link. Do not pull opening
			// bytes from a browse row — that is the expensive part.
			prefetchBytes = 0
		default:
			prefetchBytes = bigWarmPrefetchBytes
		}
	}
	if cachedMetadata || playProbe {
		prefetchBytes = 0
	}
	if prefetchBytes > maxWarmPrefetchBytes {
		prefetchBytes = maxWarmPrefetchBytes
	}
	timeout := 45 * time.Second
	id := identity{userID: user.ID, clientID: caps.ClientID, key: key, surface: m.playbackSurface(string(caps.Protocol))}
	now := m.now()
	m.mu.Lock()
	if m.closed || m.byIdent[id] != nil || m.hasFlightLocked(id) {
		m.mu.Unlock()
		return false
	}
	for ident, e := range m.warm {
		if !now.Before(e.expires) && !warmRetained(ident) {
			delete(m.warm, ident)
		}
	}
	if existing := m.warm[id]; existing != nil {
		accepted := m.scheduleChannelWindowLocked(existing, opts.StartOffset)
		m.mu.Unlock()
		if reason == core.PrewarmDetail && !opts.DuringPlayback {
			m.scheduleVersionProbes(user, key, caps)
		}
		return accepted
	}
	if playProbe {
		delete(m.warmMisses, id)
	} else if at, ok := m.warmMisses[id]; ok {
		if now.Sub(at) < warmMissTTL {
			m.mu.Unlock()
			return false
		}
		delete(m.warmMisses, id)
	}
	active := 0
	for ident, e := range m.warm {
		select {
		case <-e.done:
			continue
		default:
		}
		if reason == core.PrewarmDetail && ident.userID == user.ID && ident.clientID == caps.ClientID && ident.key != key && (playProbe || now.Sub(e.started) >= 2*time.Second) {
			delete(m.warm, ident)
			e.abort()
			continue
		}
		active++
		if !playProbe && ident.userID == user.ID {
			m.mu.Unlock()
			return false
		}
	}
	if !playProbe && active >= 2 {
		m.mu.Unlock()
		return false
	}
	if len(m.warm) >= 128 {
		var oldest identity
		var expiry time.Time
		for ident, e := range m.warm {
			if warmRetained(ident) {
				continue
			}
			select {
			case <-e.done:
				if expiry.IsZero() || e.expires.Before(expiry) {
					oldest, expiry = ident, e.expires
				}
			default:
			}
		}
		if expiry.IsZero() {
			m.mu.Unlock()
			return false
		}
		delete(m.warm, oldest)
	}
	// Active playback takes priority over speculation, unless the caller
	// is warming the next thing while the viewer watches (a trailer or
	// the end of a film).
	if !metadata && !opts.DuringPlayback && m.hasActivePlaybackLocked() {
		m.mu.Unlock()
		return false
	}
	if !playProbe {
		budgetReason := "speculative"
		rateLimit := 6
		if reason == core.PrewarmNextEpisode {
			budgetReason = core.PrewarmNextEpisode
		}
		if reason == core.PrewarmDetail {
			budgetReason = core.PrewarmDetail
			rateLimit = 30
		}
		bk := warmBudgetKey{userID: user.ID, reason: budgetReason}
		requests := m.warmRequests[bk]
		recent := requests[:0]
		for _, at := range requests {
			if now.Sub(at) < time.Minute {
				recent = append(recent, at)
			}
		}
		if len(recent) >= rateLimit {
			m.mu.Unlock()
			return false
		}
		m.warmRequests[bk] = append(recent, now)
	}
	profileObj := m.selectProfile(caps, user, key)
	var fg chan context.Context
	var promoted chan struct{}
	if !playProbe {
		fg = make(chan context.Context, 1)
		promoted = make(chan struct{})
	}
	e := &warmEntry{offset: opts.StartOffset, profileKey: profileKey(profileObj), profileObj: profileObj, caps: caps, done: make(chan struct{}), opened: make(chan struct{}), foreground: fg, playProbe: playProbe, promoted: promoted, started: now, expires: now.Add(preResolveTTL)}
	m.warm[id] = e
	u := *user
	wallStarted := time.Now()
	m.wg.Add(1)
	m.mu.Unlock()
	go func() {
		defer m.wg.Done()
		defer func() {
			m.mu.Lock()
			if e.res == nil && m.warm[id] == e {
				delete(m.warm, id)
				// A warm-up superseded by another title was removed above
				// and is not a miss; neither is one cut short by shutdown.
				if m.ctx.Err() == nil && !playProbe {
					m.recordWarmMissLocked(id)
				}
			}
			m.mu.Unlock()
			e.ready.Do(func() { close(e.done) })
		}()
		var ctx context.Context
		var cancel context.CancelFunc
		if playProbe {
			// Explicit play intent (e.g. Plezy's play-time metadata re-ask):
			// resolve under foreground playback rules so the immediately
			// following stream request adopts the same source and probe.
			ctx, cancel = context.WithTimeout(core.WithIndexerPurpose(m.ctx, core.IndexerPurposePlayback), timeout)
		} else {
			ctx, cancel = warmPreparationContextWithPromote(core.WithIndexerPurpose(m.ctx, core.IndexerPurposeWarmup), timeout, e.foreground, e.promoted)
		}
		e.setCancel(cancel)
		defer cancel()
		if !playProbe && (opts.PlayIntent || reason == core.PrewarmPostPlay || reason == core.PrewarmNextEpisode) {
			hold := time.Duration(0)
			if reason == core.PrewarmDetail {
				hold = onDemandWarmHold
			}
			ctx = core.WithOnDemandWarm(ctx, m.onDemandWarmAdmit(ctx, id, e, wallStarted, hold))
		}
		var title *core.Title
		var err error
		if cachedMetadata {
			cached, ok := m.deps.Catalog.(interface {
				CachedGet(context.Context, core.RatingKey) (*core.Title, error)
			})
			if !ok {
				return
			}
			title, err = cached.CachedGet(ctx, key)
		} else {
			title, err = m.deps.Catalog.Get(ctx, key)
		}
		if err != nil || title == nil || title.IsFutureRelease(m.now()) || title.Type == core.TypeShow || title.Type == core.TypeSeason {
			return
		}
		if reason == core.PrewarmDetail && !opts.DuringPlayback {
			defer m.scheduleVersionProbes(&u, key, caps)
		}
		// Warming must not override the source that actually survived the
		// previous playback with the original, slower top-ranked release.
		profile := e.profileObj
		res := m.resumeResolution(ctx, u.ID, key, profile, caps, strings.TrimSpace(title.Language))
		if releasepolicy.UsesOriginalAudio(profile.ReleaseRules) {
			e.originalAudio = true
			e.originalLanguage = strings.TrimSpace(title.Language)
		}
		fromResume := false
		detailOnly := false
		if res != nil && m.rememberedReleaseMatchesTitle(ctx, res.Release, title) && payloadMatchesTitle(res.NZB, title) {
			fromResume = true
			if reason == core.PrewarmDetail && !cachedMetadata {
				// Reusing the remembered source skips Resolve/search. Recover only
				// the picker candidates, without delaying or changing that source.
				if refill, ok := m.deps.Resolver.(core.WarmupSearchCacheRefiller); ok {
					refill.RefillWarmupSearchCache(title)
				}
			}
		} else {
			res = nil
			if cachedMetadata {
				return
			}
		}
		session := newSession(randomID(16), id, &u, caps, title, e.profileKey, m.now())
		defer session.stopCancel()
		defer session.sources.seal()
		maxAttempts := m.opts.MaxAttempts
		if maxAttempts <= 0 {
			maxAttempts = 4
		}
		if cachedMetadata {
			maxAttempts = 1
		}
		var (
			tried              []string
			alternates         []core.Release
			unknownAudioProbes int
			fallbackGen        *generation
			fallbackRes        *core.Resolution
			fallbackBurned     bool
			chosenGen          *generation
			chosenRes          *core.Resolution
			lastRel            core.Release
			lastErr            error
		)
		rt := newRuntimeTracker(func(g *generation, res *core.Resolution) bool {
			info := g.infoSnapshot()
			if m.knownHardSubbed(ctx, g) && m.hardSubtitleBlocked(ctx, res.Release) {
				return false
			}
			if probedRequiredAudioRejection(profile, title, res.Release, info, false) != "" {
				return false
			}
			if caps.BroadcastBitrateKbps > 0 {
				file := res.Release
				file.Size, file.Info.FullSeason = info.Size, false
				if video := info.Video(); video != nil && video.Height > file.Info.Height {
					file.Info.Height = video.Height
				}
				if !m.channelSourceAllowed(session, file) {
					return false
				}
			}
			return true
		})
		defer rt.close(m)
		defer func() {
			if fallbackGen != nil {
				m.dropGeneration(fallbackGen)
			}
		}()
		fallbackInfo := func(size int64) core.MediaInfo {
			info := EstimateInfo(title, caps, profile)
			info.Size = size
			return info
		}
		for attempt := 0; attempt < maxAttempts; attempt++ {
			if res == nil {
				fromResume = false
				if len(tried) == 0 {
					res, err = m.deps.Resolver.Resolve(ctx, core.ResolveRequest{Title: title, Client: caps, User: &u})
					if (err != nil || res == nil) && reason == core.PrewarmDetail && caps.BroadcastBitrateKbps == 0 {
						// A detail page for a title the viewer played keeps that
						// file's stored tracks while no search can answer. The
						// entry serves the page only; Play selects its own source.
						if last := m.lastPlayedResolution(ctx, u.ID, title, profile, caps); last != nil {
							res, err, detailOnly = last, nil, true
						}
					}
				} else {
					res, err = m.nextWarmResolution(ctx, session, &tried, alternates)
				}
				if err != nil || res == nil {
					lastErr = err
					break
				}
			}
			m.mu.Lock()
			e.sourceReady = true
			m.mu.Unlock()
			if len(res.NZB) > 0 {
				res.Release.SourceHash = fmt.Sprintf("%x", sha256.Sum256(res.NZB))
			}
			if !cachedMetadata && !detailOnly {
				if m.titleReleaseRejected(ctx, key, res.Release) {
					tried = append(tried, res.Release.ID)
					if len(res.Alternates) > 0 {
						alternates = res.Alternates
					}
					res = nil
					continue
				}
				if m.releaseNamesakeRejected(ctx, session, res.Release) {
					m.markBad(res.Release.ID, "release names the other title")
					tried = append(tried, res.Release.ID)
					if len(res.Alternates) > 0 {
						alternates = res.Alternates
					}
					res = nil
					continue
				}
			}
			needsAudioProbe := false
			if !cachedMetadata && !detailOnly && m.ownerPinAllows(session, "") {
				if langs, unverified := candidateNeedsAudioVerification(profile, title, res.Release); unverified {
					if unknownAudioProbes >= maxUnknownAudioProbes {
						lastRel, lastErr = res.Release, fmt.Errorf("audio is not in the profile's language (%s)", langs[0])
						tried = append(tried, res.Release.ID)
						if len(res.Alternates) > 0 {
							alternates = res.Alternates
						}
						res = nil
						continue
					}
					unknownAudioProbes++
					needsAudioProbe = true
				}
			}
			src, finish, err := m.openSource(ctx, session.sources, res)
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				lastRel, lastErr = res.Release, err
				if fromResume && reason == core.PrewarmNextEpisode && permanentSourceFailure(err) {
					_ = m.ReplaceWarmSource(ctx, &u, key, caps, res.Release, opts.StartOffset, title.Runtime, err)
					return
				}
				if cachedMetadata || detailOnly {
					break
				}
				tried = append(tried, res.Release.ID)
				if len(res.Alternates) > 0 {
					alternates = res.Alternates
				}
				res = nil
				continue
			}
			m.noteWarmCause(res, src, reason, cacheTitle(*title))
			probeCtx := withDeferredGenerationFills(ctx)
			if !cachedMetadata && !detailOnly {
				probeCtx = withRuntimeTracker(probeCtx, rt)
			}
			generation, err := m.probeGeneration(probeCtx, session, res, src, finish, fallbackInfo, m.persistedProbe(ctx, u.ID, key, res.Release.ID, res.NZB))
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				lastRel, lastErr = res.Release, err
				if fromResume && reason == core.PrewarmNextEpisode && permanentSourceFailure(err) {
					_ = m.ReplaceWarmSource(ctx, &u, key, caps, res.Release, opts.StartOffset, title.Runtime, err)
					return
				}
				if cachedMetadata || detailOnly {
					break
				}
				tried = append(tried, res.Release.ID)
				if len(res.Alternates) > 0 {
					alternates = res.Alternates
				}
				res = nil
				continue
			}
			m.RecordReleaseProbe(ctx, res.Release, res.NZB, generation.infoSnapshot())
			burned := !cachedMetadata && !detailOnly && m.knownHardSubbed(ctx, generation)
			if burned && m.hardSubtitleBlocked(ctx, res.Release) {
				m.dropGeneration(generation)
				tried = append(tried, res.Release.ID)
				if len(res.Alternates) > 0 {
					alternates = res.Alternates
				}
				res = nil
				continue
			}
			if reqReason := probedRequiredAudioRejection(profile, title, res.Release, generation.infoSnapshot(), fromResume); reqReason != "" {
				m.log.Debug("prewarm skipped: required audio absent", "key", key, "release", res.Release.Name, "reason", reqReason)
				m.dropGeneration(generation)
				lastRel, lastErr = res.Release, errors.New(reqReason)
				if cachedMetadata || detailOnly {
					break
				}
				if needsAudioProbe && unknownAudioProbes < maxUnknownAudioProbes {
					attempt--
				}
				tried = append(tried, res.Release.ID)
				if len(res.Alternates) > 0 {
					alternates = res.Alternates
				}
				res = nil
				continue
			}
			if !cachedMetadata && !detailOnly && !fromResume && (burned || !m.probedAudioAcceptable(session, profile, generation.infoSnapshot())) {
				skipReason := "probed release lacks viewer audio language"
				if burned {
					skipReason = "burned-in subtitles detected"
				}
				m.log.Debug("prewarm trying next candidate", "key", key, "release", res.Release.Name, "reason", skipReason)
				if fallbackGen == nil || (fallbackBurned && !burned) {
					if fallbackGen != nil {
						m.dropGeneration(fallbackGen)
					}
					fallbackGen, fallbackRes, fallbackBurned = generation, res, burned
				} else {
					m.dropGeneration(generation)
				}
				tried = append(tried, res.Release.ID)
				if len(res.Alternates) > 0 {
					alternates = res.Alternates
				}
				res = nil
				continue
			}
			if caps.BroadcastBitrateKbps > 0 {
				file := res.Release
				file.Size, file.Info.FullSeason = generation.src.Size(), false
				if video := generation.infoSnapshot().Video(); video != nil && video.Height > file.Info.Height {
					file.Info.Height = video.Height
				}
				if !m.channelSourceAllowed(session, file) {
					m.dropGeneration(generation)
					if cachedMetadata || detailOnly {
						break
					}
					tried = append(tried, res.Release.ID)
					if len(res.Alternates) > 0 {
						alternates = res.Alternates
					}
					res = nil
					continue
				}
			}
			chosenGen, chosenRes = generation, res
			break
		}
		if chosenGen == nil && fallbackGen != nil {
			chosenGen, chosenRes = fallbackGen, fallbackRes
			fallbackGen = nil
		}
		if chosenGen == nil && !cachedMetadata && !detailOnly && ctx.Err() == nil {
			if g, fRes, med, disagree, ok := rt.takeFallback(); ok {
				m.recordConsensusRuntime(m.ctx, title, med)
				for _, r := range disagree {
					if err := m.RejectRelease(m.ctx, title.Key, r); err != nil {
						m.log.Debug("persist runtime rejection failed", "key", title.Key, "release", r.ID, "err", err)
					}
					m.rememberOpenFailure(session, r, core.ErrReleaseRejected)
				}
				m.log.Info("probed runtime consensus overrides catalogue runtime",
					"key", key, "release", fRes.Release.Name, "consensus", med, "catalogue", title.Runtime, "fallback", true)
				chosenGen, chosenRes = g, fRes
			}
		}
		if ctx.Err() == nil {
			rt.commitPendingRejections(m.ctx, m, session)
		}
		if chosenGen == nil {
			if reason == core.PrewarmNextEpisode && lastRel.ID != "" && lastErr != nil {
				_ = m.ReplaceWarmSource(ctx, &u, key, caps, lastRel, opts.StartOffset, title.Runtime, lastErr)
			}
			return
		}
		m.startGenerationFills(chosenGen)
		generation, res := chosenGen, chosenRes
		defer m.dropGeneration(generation)
		facts := &probeCache{releaseID: res.Release.ID, info: generation.infoSnapshot(), warmReads: generation.warmReads, points: generation.timeMapSnapshot().Points}
		if facts.info.Estimated {
			facts = nil
		}
		if !cachedMetadata && !playProbe && (automaticReserve || reason == core.PrewarmDetail || reason == core.PrewarmNextEpisode) {
			info := generation.infoSnapshot()
			if info.Duration > 0 {
				prefetchBytes = min(int64(256<<20), max(int64(8<<20), int64(float64(generation.src.Size())*25/info.Duration.Seconds())))
			}
		}

		// Publish before prefetch so a play request need not wait for
		// background IO on debrid/HTTP. Usenet still has to prove the
		// opening articles exist; takeWarm will not consume it until then.
		m.mu.Lock()
		e.res, e.probe, e.runtime, e.detailOnly = res, facts, title.Runtime, detailOnly
		m.mu.Unlock()
		e.ready.Do(func() { close(e.done) })
		if !usenetWarm(res) {
			e.markOpeningReady()
		}
		if facts != nil && !facts.info.Estimated {
			m.RecordReleaseProbe(ctx, res.Release, res.NZB, facts.info)
		}
		m.log.Debug("prewarm ready", "key", key, "reason", reason, "release", res.Release.Name, "prefetch", prefetchBytes, "detailOnly", detailOnly)
		if prefetchBytes <= 0 || detailOnly {
			e.markOpeningReady()
			return
		}
		resumeOffset := opts.StartOffset
		if reason == core.PrewarmDetail && resumeOffset <= 0 && m.deps.State != nil {
			if status, err := m.deps.State.Get(ctx, u.ID, key); err == nil {
				resumeOffset = status.ViewOffset
			}
		}
		warmOffset := int64(0)
		if resumeOffset > 0 && facts != nil {
			tm := TimeMap{Size: generation.src.Size(), Duration: facts.info.Duration, Points: facts.points}
			lo, hi := warmSpanFor(tm, resumeOffset, resumeOffset+25*time.Second)
			warmOffset = lo
			prefetchBytes = min(hi-lo, int64(256<<20))
		}
		if cfg.BudgetBytes > 0 {
			prefetchBytes = min(prefetchBytes, cfg.BudgetBytes/10)
		}
		if err := m.deps.Engine.Prefetch(core.WithWarmReadWindow(core.WithReadOrigin(ctx, "playback.prewarm", string(reason)), generation.warmReads), res.Release.ID, res.NZB, warmOffset, prefetchBytes); err != nil {
			if permanentSourceFailure(err) {
				// Probe success is not proof that the opening window is intact.
				// Invalidate the published warm result before a later Play uses it.
				m.mu.Lock()
				if m.warm[id] == e {
					delete(m.warm, id)
				}
				m.mu.Unlock()
				m.rememberOpenFailure(session, res.Release, err)
				if reason == core.PrewarmNextEpisode {
					_ = m.ReplaceWarmSource(ctx, &u, key, caps, res.Release, resumeOffset, title.Runtime, err)
				} else {
					m.markBad(res.Release.ID, "opening prefetch: "+err.Error())
				}
				m.log.Warn("prepared opening rejected", "key", key, "release", res.Release.ID, "err", err)
			} else if ctx.Err() == nil {
				m.log.Debug("opening prefetch incomplete", "key", key, "err", err)
			}
			return
		}
		e.markOpeningReady()
		m.log.Info("opening prefetch completed", "key", key, "release", res.Release.ID, "bytes", prefetchBytes)
		// Seek indexes are optional for starting playback. Keep filling one
		// under the warm-up budget, but never hold the ready signal for it.
		select {
		case <-generation.indexReady:
		case <-ctx.Done():
			return
		}
		complete := &probeCache{releaseID: res.Release.ID, info: generation.infoSnapshot(), warmReads: generation.warmReads, points: generation.timeMapSnapshot().Points}
		if !complete.info.Estimated {
			m.mu.Lock()
			if m.warm[id] == e {
				e.probe = complete
			}
			m.mu.Unlock()
		}
	}()
	return true
}

func (m *Manager) nextWarmResolution(ctx context.Context, s *session, tried *[]string, alternates []core.Release) (*core.Resolution, error) {
	if m.deps.Vault == nil || len(alternates) == 0 {
		return nil, core.ErrNotAvailable
	}
	exclude := append(slices.Clone(*tried), s.caps.BroadcastExclude...)
	m.mu.Lock()
	for id := range m.openFailures[s.ident.key] {
		if !slices.Contains(exclude, id) {
			exclude = append(exclude, id)
		}
	}
	m.mu.Unlock()
	for i, alt := range alternates {
		if alt.ID == "" || !m.ownerPinAllows(s, alt.ID) || !m.channelSourceAllowed(s, alt) || m.titleReleaseRejected(ctx, s.ident.key, alt) || m.releaseNamesakeRejected(ctx, s, alt) || slices.Contains(exclude, alt.ID) || !releaseMatchesTitle(alt, s.title) {
			continue
		}
		vk := vault.KeyOf(alt)
		if vk == "" && strings.HasPrefix(alt.ContentID, "nzb:") {
			vk = vault.Key(alt.ContentID)
		}
		if vk == "" {
			continue
		}
		rec, err := m.deps.Vault.Get(ctx, vk)
		if err != nil || rec == nil || len(rec.XML) == 0 {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			continue
		}
		return &core.Resolution{
			Title:      s.ident.key,
			Release:    alt,
			NZB:        rec.XML,
			Alternates: slices.Clone(alternates[i+1:]),
			ResolvedAt: m.now(),
		}, nil
	}
	return nil, core.ErrNotAvailable
}

// lastPlayedResolution is the release that last served the viewer's title,
// for a detail warm-up whose Resolve found no source. Unlike
// resumeResolution it does not require the profile selection key the
// release was chosen under, so a page opened with other capabilities (home
// versus away) still lists that file's stored tracks. The release must
// still match the title, be allowed by the source preference, carry no open
// failure or title rejection, satisfy the current profile's release rules
// and original-audio language, and fit these caps and the profile's height,
// codec, HDR and size bounds as far as its name and size tell.
// resumePoint already drops a blocked release and an episode payload for
// another episode. The warm entry built from it is detailOnly: Play
// discards it and selects under its own profile.
func (m *Manager) lastPlayedResolution(ctx context.Context, userID int64, title *core.Title, profile core.QualityProfile, caps core.ClientCaps) *core.Resolution {
	rp, err := m.resumePoint(ctx, userID, title.Key, true)
	if err != nil || rp == nil || len(rp.NZB) == 0 || !m.resumeSourceAllowed(rp.Release) ||
		!m.rememberedReleaseMatchesTitle(ctx, rp.Release, title) || m.titleReleaseRejected(ctx, title.Key, rp.Release) {
		return nil
	}
	lang := strings.TrimSpace(title.Language)
	if !rememberedOriginalAudioCurrent(profile, rp.originalLanguage, lang, rp.Release) {
		return nil
	}
	if pol, err := releasepolicy.Compile(profile.ReleaseRules); err != nil || pol.Evaluate(rp.Release, releasepolicy.Context{OriginalLanguage: lang}).Rejected {
		return nil
	}
	res := &core.Resolution{Title: title.Key, Release: rp.Release, NZB: rp.NZB, ResolvedAt: rp.UpdatedAt}
	if !warmAcceptable(&warmEntry{res: res, profileObj: profile, runtime: title.Runtime}, profile, caps, lang) {
		return nil
	}
	return res
}

const (
	// onDemandWarmHold is how long a detail page must stay the device's
	// current title before its warm-up may open an add-on stream: opening
	// another title's page aborts the warm-up first.
	onDemandWarmHold = 2500 * time.Millisecond
	// One add-on stream per viewer per onDemandWarmViewerGap, and at most
	// onDemandWarmAddonLimit per add-on per onDemandWarmAddonWindow.
	onDemandWarmViewerGap   = time.Minute
	onDemandWarmAddonLimit  = 6
	onDemandWarmAddonWindow = 10 * time.Minute
)

// onDemandWarmAdmit is the budget behind core.WithOnDemandWarm for one
// warm-up: after the hold, the entry must still be the device's current
// warm-up, the viewer must not have warmed an add-on stream within the gap,
// and the add-on must be under its window limit.
func (m *Manager) onDemandWarmAdmit(ctx context.Context, id identity, e *warmEntry, started time.Time, hold time.Duration) func(core.Release) bool {
	return func(rel core.Release) bool {
		if !e.isPromoted() {
			if wait := time.Until(started.Add(hold)); wait > 0 {
				t := time.NewTimer(wait)
				select {
				case <-t.C:
				case <-e.promoted:
					t.Stop()
				case <-ctx.Done():
					t.Stop()
					return false
				}
			}
		}
		if ctx.Err() != nil || !core.IsSpeculative(ctx) || e.isPromoted() {
			return ctx.Err() == nil // Play took the warm-up over: normal rules apply
		}
		addon := rel.IndexerID
		if addon == "" {
			addon = rel.Indexer
		}
		now := m.now()
		m.mu.Lock()
		defer m.mu.Unlock()
		if m.closed || m.warm[id] != e {
			return false
		}
		if at, ok := m.onDemandWarmAt[id.userID]; ok && now.Sub(at) < onDemandWarmViewerGap {
			return false
		}
		recent := m.onDemandWarmByAddon[addon][:0]
		for _, at := range m.onDemandWarmByAddon[addon] {
			if now.Sub(at) < onDemandWarmAddonWindow {
				recent = append(recent, at)
			}
		}
		if len(recent) >= onDemandWarmAddonLimit {
			m.onDemandWarmByAddon[addon] = recent
			return false
		}
		if m.onDemandWarmAt == nil {
			m.onDemandWarmAt = map[int64]time.Time{}
		}
		if m.onDemandWarmByAddon == nil {
			m.onDemandWarmByAddon = map[string][]time.Time{}
		}
		m.onDemandWarmAt[id.userID] = now
		m.onDemandWarmByAddon[addon] = append(recent, now)
		m.log.Info("warm-up opens one add-on stream", "key", id.key, "release", rel.Name, "addon", rel.Indexer)
		return true
	}
}

// warmFacts are the release properties a Play-time profile is checked
// against, from the cached probe when there is one, else the parsed
// release name.
type warmFacts struct {
	height   int
	codec    string
	hdr      bool
	dv       bool
	size     int64
	duration time.Duration
	bitrate  int
	known    bool // at least one property was known
}

func (e *warmEntry) facts() warmFacts {
	var f warmFacts
	if e.probe != nil {
		info := e.probe.info
		f.size, f.duration = info.Size, info.Duration
		f.bitrate = info.BitrateKbps
		if v := info.Video(); v != nil {
			f.height, f.codec = warmResolutionHeight(v.Width, v.Height), v.Codec
			f.hdr = v.ColorTransfer == "smpte2084" || v.ColorTransfer == "arib-std-b67"
			f.dv = v.DolbyVision
		}
		f.known = f.height > 0 || f.codec != "" || f.size > 0
		return f
	}
	if e.res != nil {
		ri := e.res.Release.Info
		f.height, f.codec, f.hdr, f.dv = ri.Height, ri.VideoCodec, ri.HDR10, ri.DolbyVision
		f.size, f.duration = e.res.Release.Size, e.runtime
		f.known = f.height > 0 || f.codec != "" || f.size > 0
	}
	return f
}

// warmAcceptable reports whether the warm release is acceptable for the
// Play-time profile and caps. The detail request that produced the warm
// entry derived caps without decision query params or the client-profile
// extra, so its profile name frequently differs from the Play-time one
// for the same device; equality is sufficient but not required. Unknown
// properties never reject; with no facts at all, the Play-time profile
// must be at least as permissive as the warm one.
func warmAcceptable(e *warmEntry, profile core.QualityProfile, caps core.ClientCaps, originalLanguage string) bool {
	if e == nil || e.res == nil {
		return false
	}
	if !reflect.DeepEqual(e.profileObj.ReleaseRules, profile.ReleaseRules) ||
		!reflect.DeepEqual(e.profileObj.PluginScores, profile.PluginScores) {
		return false
	}
	if e.originalAudio && e.originalLanguage != strings.TrimSpace(originalLanguage) {
		return false
	}
	f := e.facts()
	if !sourceFitsBitrate(f.size, f.duration, f.bitrate, caps.MaxBitrateKbps) {
		return false
	}
	if !f.known {
		w := e.profileObj
		heightOK := profile.MaxHeight == 0 || w.MaxHeight == 0 || profile.MaxHeight >= w.MaxHeight
		return heightOK && profile.MinHeight <= w.MinHeight &&
			(profile.AllowHDR || !w.AllowHDR) && (profile.AllowDolbyVision || !w.AllowDolbyVision)
	}
	capHeight := profile.MaxHeight
	if caps.MaxHeight > 0 && (capHeight == 0 || caps.MaxHeight < capHeight) {
		capHeight = caps.MaxHeight
	}
	if capHeight > 0 && f.height > capHeight {
		return false
	}
	if profile.MinHeight > 0 && f.height > 0 && f.height < profile.MinHeight {
		return false
	}
	if f.codec != "" && len(caps.VideoCodecs) > 0 && !caps.SupportsVideo(f.codec) {
		return false
	}
	if f.hdr && !(profile.AllowHDR && caps.HDR10) {
		return false
	}
	if f.dv && !(profile.AllowDolbyVision && caps.DolbyVision) {
		return false
	}
	if f.size > 0 && f.duration > 0 {
		mbPerMin := float64(f.size) / (1024 * 1024) / f.duration.Minutes()
		minMBPerMin, maxMBPerMin := profile.SizeBoundsForKey(e.res.Title)
		if maxMBPerMin > 0 && mbPerMin > maxMBPerMin {
			return false
		}
		if minMBPerMin > 0 && mbPerMin < minMBPerMin {
			return false
		}
	}
	return true
}

func (m *Manager) findWarmEntryLocked(user *core.User, caps core.ClientCaps, key core.RatingKey, now time.Time) *warmEntry {
	if caps.ClientID != "" {
		if surf := m.playbackSurface(string(caps.Protocol)); surf != "" {
			if e := m.warm[identity{userID: user.ID, clientID: caps.ClientID, key: key, surface: surf}]; e != nil && now.Before(e.expires) {
				return e
			}
		}
		if e := m.warm[identity{userID: user.ID, clientID: caps.ClientID, key: key}]; e != nil && now.Before(e.expires) {
			return e
		}
	}
	if m.deps.SelectProfile == nil {
		return nil
	}
	pk := profileKey(m.selectProfile(caps, user, key))
	for ident, e := range m.warm {
		if ident.userID == user.ID && ident.key == key && now.Before(e.expires) && e.profileKey == pk {
			return e
		}
	}
	return nil
}

// DiscardWarm drops and cancels the warm entry for this user/client/key, so
// the next Prepare resolves afresh. The broker calls it when a device's caps
// widened after a detail page warmed the title with its earlier caps.
func (m *Manager) DiscardWarm(user *core.User, caps core.ClientCaps, key core.RatingKey) {
	if user == nil || caps.ClientID == "" {
		return
	}
	id := identity{userID: user.ID, clientID: caps.ClientID, key: key, surface: m.playbackSurface(string(caps.Protocol))}
	m.mu.Lock()
	e := m.warm[id]
	if e != nil {
		delete(m.warm, id)
	}
	m.mu.Unlock()
	if e != nil {
		e.abort()
	}
}

// WarmProbeInfo returns the probed media info of a completed warm-up for
// this user/client/key, for metadata/play-queue rendering before Play
// (core.WarmProber). It never blocks on a warm-up still in flight and never
// consumes the entry: the later Play still takes it via takeWarmResolution.
func (m *Manager) WarmProbeInfo(user *core.User, caps core.ClientCaps, key core.RatingKey) (core.MediaInfo, bool) {
	if user == nil || caps.ClientID == "" {
		return core.MediaInfo{}, false
	}
	now := m.now()
	m.mu.Lock()
	e := m.findWarmEntryLocked(user, caps, key, now)
	m.mu.Unlock()
	if e == nil {
		return core.MediaInfo{}, false
	}
	select {
	case <-e.done:
	default:
		return core.MediaInfo{}, false
	}
	m.mu.Lock()
	probe := e.probe
	m.mu.Unlock()
	if probe == nil || probe.info.Estimated || len(probe.info.Streams) == 0 {
		return core.MediaInfo{}, false
	}
	return probe.info, true
}

// WaitWarmProbe waits up to budget for an already-scheduled warm-up for this
// user/client/key to finish probing, returning its probed media info when
// ready (core.WarmProbeWaiter). It never cancels the background warm-up when
// budget expires.
func (m *Manager) WaitWarmProbe(ctx context.Context, user *core.User, caps core.ClientCaps, key core.RatingKey, budget time.Duration) (core.MediaInfo, bool) {
	if user == nil || caps.ClientID == "" {
		return core.MediaInfo{}, false
	}
	if info, ok := m.WarmProbeInfo(user, caps, key); ok || budget <= 0 {
		return info, ok
	}
	now := m.now()
	m.mu.Lock()
	e := m.findWarmEntryLocked(user, caps, key, now)
	m.mu.Unlock()
	if e == nil {
		return core.MediaInfo{}, false
	}
	timer := time.NewTimer(budget)
	defer timer.Stop()
	select {
	case <-e.done:
	case <-ctx.Done():
		return core.MediaInfo{}, false
	case <-timer.C:
		return core.MediaInfo{}, false
	}
	return m.WarmProbeInfo(user, caps, key)
}

func waitWarmChan(ctx context.Context, ch <-chan struct{}, deadline time.Time) bool {
	if ch == nil {
		return true
	}
	select {
	case <-ch:
		return true
	default:
	}
	d := time.Until(deadline)
	if d <= 0 {
		return false
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ch:
		return true
	case <-ctx.Done():
		return false
	case <-timer.C:
		return false
	}
}

// takeWarmResolution consumes the warm entry for id when its release is
// acceptable for the Play-time profile and caps (and, for a variant,
// is the picked release); an unacceptable entry is discarded.
func (m *Manager) takeWarmResolution(ctx context.Context, id identity, profile core.QualityProfile, caps core.ClientCaps, prefer, originalLanguage string) *warmEntry {
	// Warm facts are keyed by viewer/device/title, then checked against the
	// selected release below; a plan's variant identity is not a warm key.
	id.variant, id.exact = "", false
	now := m.now()
	m.mu.Lock()
	warmID := id
	e := m.warm[id]
	if e != nil && !now.Before(e.expires) {
		delete(m.warm, id)
		e = nil
	}
	if e == nil {
		// Clientless native players (such as Plezy's libmpv) drop
		// X-Plex-Client-Identifier on /library/parts reads and receive a
		// synthetic native-* device ID. Let Play join a play-intent or
		// promoted warm flight started by the viewer's metadata re-ask so
		// file.mkv reuses the same resolution and probe without repeating work.
		pk := profileKey(profile)
		for candID, cand := range m.warm {
			if (!cand.playProbe && !cand.isPromoted()) || candID.userID != id.userID || candID.key != id.key || candID.surface != id.surface || !now.Before(cand.expires) {
				continue
			}
			if e == nil || (cand.profileKey == pk && e.profileKey != pk) || (cand.profileKey == e.profileKey && cand.expires.After(e.expires)) {
				warmID = candID
				e = cand
			}
		}
	}
	if e == nil {
		m.mu.Unlock()
		return nil
	}
	m.mu.Unlock()
	// Once Play is waiting, this is foreground preparation. Keep its
	// existing resolve/open/probe work instead of cancelling it at the
	// speculative deadline and repeating it on Play's critical path.
	if e.foreground != nil {
		select {
		case e.foreground <- ctx:
		default:
		}
	}
	if (e.playProbe || e.isPromoted()) && prefer == "" {
		select {
		case <-e.done:
		case <-ctx.Done():
			return nil
		}
		if e.opened != nil {
			select {
			case <-e.opened:
			case <-ctx.Done():
				return nil
			}
		}
	} else {
		deadline := time.Now().Add(takeWarmBudget)
		if !waitWarmChan(ctx, e.done, deadline) {
			return nil
		}
		if e.opened != nil && !waitWarmChan(ctx, e.opened, deadline) {
			// Probe published but the usenet opening is still unproven.
			// Let Play resolve a cached debrid remux instead of waiting.
			return nil
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.warm[warmID] != e {
		return nil
	}
	if e.detailOnly {
		// The last played release, kept for the detail page's tracks;
		// Play selects its own source.
		if warmID == id {
			delete(m.warm, warmID)
		}
		return nil
	}
	if prefer != "" && (e.res == nil || !e.res.MatchesVersion(prefer)) {
		return nil // keep it: a plain Prepare may still use it
	}
	blocked := e.res != nil && m.releaseBlocked(ctx, e.res.Release.ID)
	if !warmAcceptable(e, profile, caps, originalLanguage) || blocked {
		if warmID == id || blocked {
			delete(m.warm, warmID)
		}
		return nil
	}
	if e.probe == nil {
		if warmID == id {
			delete(m.warm, warmID)
		}
	} else {
		e.expires = m.now().Add(preResolveTTL)
	}
	// Return an immutable snapshot: the background index fill may replace
	// e.probe after the ready signal.
	return &warmEntry{res: e.res, probe: e.probe}
}

// InvalidateTitle drops the warm (pre-resolved) entries for a title, from
// both the detail and speculative budgets, so the next Play resolves
// afresh — after the owner pins, unpins, blocks or unblocks a release, or
// forgets a title's search. Stored source choices are invalidated too,
// with their keyframe indexes and probes; watch progress remains intact.
// A show or season key
// also drops its episodes' entries. A warm-up still in flight finishes into
// nothing. It returns how many warm entries were dropped.
func (m *Manager) InvalidateTitle(key core.RatingKey) int {
	if key == 0 {
		return 0
	}
	m.mu.Lock()
	m.forgetStalePrefersLocked(key)
	n := 0
	for id, e := range m.warm {
		if titleIncludes(key, id.key) {
			delete(m.warm, id)
			e.abort()
			n++
		}
	}
	m.mu.Unlock()
	if m.resume != nil {
		ctx, cancel := context.WithTimeout(m.ctx, 5*time.Second)
		defer cancel()
		sessions := map[resumeKey]bool{}
		m.mu.Lock()
		for _, s := range m.sessions {
			sessions[resumeKey{s.ident.userID, s.ident.key}] = true
		}
		m.mu.Unlock()
		if err := m.resume.invalidateTitle(ctx, key, sessions); err != nil {
			m.log.Warn("invalidate remembered source", "key", key, "err", err)
		}
	}
	return n
}

func titleIncludes(parent, key core.RatingKey) bool {
	switch {
	case key == parent:
		return true
	case key.Kind() != core.KindEpisode:
		return false
	case parent.Kind() == core.KindShow:
		return key.TMDB() == parent.TMDB()
	case parent.Kind() == core.KindSeason:
		return key.Parent() == parent
	}
	return false
}

func (m *Manager) releaseBlocked(ctx context.Context, id string) bool {
	if local, ok := m.deps.Resolver.(interface{ LocalReleaseBlocked(string) bool }); ok && local.LocalReleaseBlocked(id) {
		return true
	}
	if blocked, ok := preloadedReleaseBlocked(ctx, id); ok {
		return blocked
	}
	if checker, ok := m.deps.Resolver.(core.ReleaseBlockChecker); ok {
		blocked, err := checker.ReleaseBlocked(ctx, id)
		return blocked || err != nil
	}
	return false
}

func (m *Manager) hasActivePlaybackLocked() bool {
	if m.pendingN > 0 {
		return true
	}
	for _, s := range m.sessions {
		s.mu.Lock()
		active := s.stopAt.IsZero() && s.viewerState != "paused"
		s.mu.Unlock()
		if active {
			return true
		}
	}
	return false
}

// warmResolutionHeight normalizes cinemascope/anamorphic dimensions (e.g.
// 1920x800 or 3840x1600) to their nominal vertical tier so profile MinHeight
// checks (such as 1080p) do not reject wide-aspect 1080p/4K releases.
func warmResolutionHeight(w, h int) int {
	tier := core.ResolutionTier(w, h)
	if tier >= 720 {
		return max(tier, h)
	}
	return h
}

// recordWarmMissLocked remembers a warm-up that ended without a source.
// The memo is bounded: expired entries go first, then the oldest.
func (m *Manager) recordWarmMissLocked(id identity) {
	now := m.now()
	if len(m.warmMisses) >= maxWarmMisses {
		var oldest identity
		var at time.Time
		for ident, t := range m.warmMisses {
			if now.Sub(t) >= warmMissTTL {
				delete(m.warmMisses, ident)
				continue
			}
			if at.IsZero() || t.Before(at) {
				oldest, at = ident, t
			}
		}
		if len(m.warmMisses) >= maxWarmMisses {
			delete(m.warmMisses, oldest)
		}
	}
	m.warmMisses[id] = now
}
