package playback

import (
	"context"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/releasepolicy"
)

const preResolveTTL = 60 * time.Minute

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
	profileKey string // profileKey of profileObj
	profileObj core.QualityProfile
	caps       core.ClientCaps
	done       chan struct{}
	ready      sync.Once
	foreground chan context.Context
	started    time.Time
	expires    time.Time
	cancelMu   sync.Mutex
	cancel     context.CancelFunc
	runtime    time.Duration // title runtime, for size rules without a probe
	res        *core.Resolution
	probe      *probeCache
	// originalAudio is set when the warmed profile compares audio with
	// the catalogue language. originalLanguage is the Title.Language
	// that Resolve saw. A later catalogue correction must not reuse it.
	originalAudio    bool
	originalLanguage string
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

// PreResolve schedules detail-page warming; it is the core.PrewarmDetail
// form of Prewarm, kept for existing callers.
func (m *Manager) PreResolve(ctx context.Context, user *core.User, key core.RatingKey, caps core.ClientCaps) bool {
	return m.Prewarm(ctx, user, key, caps, core.PrewarmOptions{Reason: core.PrewarmDetail})
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
func (m *Manager) Prewarm(_ context.Context, user *core.User, key core.RatingKey, caps core.ClientCaps, opts core.PrewarmOptions) bool {
	if user == nil || caps.ClientID == "" || caps.Product == "" {
		return false
	}
	reason := opts.Reason
	if reason != core.PrewarmTrailer && reason != core.PrewarmPostPlay {
		reason = core.PrewarmDetail
	}
	cfg := m.config().Cache
	if cfg.DisableSpeculation || cfg.Mode == "off" || (reason == core.PrewarmDetail && cfg.DetailWarmup == "off") {
		return false
	}
	automaticReserve := reason == core.PrewarmDetail && opts.PrefetchBytes <= 0 && cfg.DetailBytes <= 0
	prefetchBytes := opts.PrefetchBytes
	if reason == core.PrewarmDetail && cfg.DetailBytes > 0 {
		prefetchBytes = cfg.DetailBytes
	}
	if prefetchBytes <= 0 {
		if reason == core.PrewarmDetail {
			prefetchBytes = detailPrefetchBytes
		} else {
			prefetchBytes = bigWarmPrefetchBytes
		}
	}
	if prefetchBytes > maxWarmPrefetchBytes {
		prefetchBytes = maxWarmPrefetchBytes
	}
	timeout := 45 * time.Second
	id := identity{userID: user.ID, clientID: caps.ClientID, key: key, surface: playbackSurface(string(caps.Protocol))}
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
	if m.warm[id] != nil {
		m.mu.Unlock()
		return false
	}
	active := 0
	for ident, e := range m.warm {
		select {
		case <-e.done:
			continue
		default:
		}
		if reason == core.PrewarmDetail && ident.userID == user.ID && ident.clientID == caps.ClientID && ident.key != key && now.Sub(e.started) >= 2*time.Second {
			delete(m.warm, ident)
			e.abort()
			continue
		}
		active++
		if ident.userID == user.ID {
			m.mu.Unlock()
			return false
		}
	}
	if active >= 2 {
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
	if !opts.DuringPlayback && m.hasActivePlaybackLocked() {
		m.mu.Unlock()
		return false
	}
	budgetReason := "speculative"
	rateLimit := 6
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
	profileObj := m.deps.SelectProfile(caps, user)
	e := &warmEntry{profileKey: profileKey(profileObj), profileObj: profileObj, caps: caps, done: make(chan struct{}), foreground: make(chan context.Context, 1), started: now, expires: now.Add(preResolveTTL)}
	m.warm[id] = e
	u := *user
	m.wg.Add(1)
	m.mu.Unlock()
	go func() {
		defer m.wg.Done()
		defer func() {
			m.mu.Lock()
			if e.res == nil && m.warm[id] == e {
				delete(m.warm, id)
			}
			m.mu.Unlock()
			e.ready.Do(func() { close(e.done) })
		}()
		ctx, cancel := warmPreparationContext(m.ctx, timeout, e.foreground)
		e.setCancel(cancel)
		defer cancel()
		title, err := m.deps.Catalog.Get(ctx, key)
		if err != nil || title == nil || (!title.Released.IsZero() && title.Released.After(m.now())) || title.Type == core.TypeShow || title.Type == core.TypeSeason {
			return
		}
		// Warming must not override the source that actually survived the
		// previous playback with the original, slower top-ranked release.
		res := m.resumeResolution(ctx, u.ID, key, e.profileObj, caps, strings.TrimSpace(title.Language))
		if releasepolicy.UsesOriginalAudio(e.profileObj.ReleaseRules) {
			e.originalAudio = true
			e.originalLanguage = strings.TrimSpace(title.Language)
		}
		if res == nil || !releaseMatchesTitle(res.Release, title) {
			res, err = m.deps.Resolver.Resolve(ctx, core.ResolveRequest{Title: title, Client: caps, User: &u})
		} else if reason == core.PrewarmDetail {
			// Reusing the remembered source skips Resolve/search. Recover only
			// the picker candidates, without delaying or changing that source.
			if refill, ok := m.deps.Resolver.(core.SearchCacheRefiller); ok {
				refill.RefillSearchCache(title)
			}
		}
		if err != nil || res == nil {
			return
		}
		session := newSession(randomID(16), id, &u, caps, title, e.profileKey, m.now())
		defer session.sources.seal()
		src, finish, err := m.openSource(ctx, session.sources, res)
		if err != nil {
			return
		}
		generation, err := m.probeGeneration(ctx, session, res, src, finish, func(size int64) core.MediaInfo {
			info := EstimateInfo(title, caps, m.deps.SelectProfile(caps, &u))
			info.Size = size
			return info
		})
		if err != nil {
			return
		}
		defer m.dropGeneration(generation)
		facts := &probeCache{releaseID: res.Release.ID, info: generation.infoSnapshot(), points: generation.timeMapSnapshot().Points}
		if facts.info.Estimated {
			facts = nil
		}
		if automaticReserve {
			info := generation.infoSnapshot()
			if info.Duration > 0 {
				prefetchBytes = min(int64(256<<20), max(int64(8<<20), int64(float64(src.Size())*playableReserve.Seconds()/info.Duration.Seconds())))
			}
		}
		// Publish before prefetch so a play request need not wait for background IO.
		m.mu.Lock()
		e.res, e.probe, e.runtime = res, facts, title.Runtime
		m.mu.Unlock()
		e.ready.Do(func() { close(e.done) })
		m.log.Debug("prewarm ready", "key", key, "reason", reason, "release", res.Release.Name, "prefetch", prefetchBytes)
		if err := m.deps.Engine.Prefetch(ctx, res.Release.ID, res.NZB, 0, prefetchBytes); err != nil {
			if permanentSourceFailure(err) {
				// Probe success is not proof that the opening window is intact.
				// Invalidate the published warm result before a later Play uses it.
				m.mu.Lock()
				if m.warm[id] == e {
					delete(m.warm, id)
				}
				m.mu.Unlock()
				m.markBad(res.Release.ID, "opening prefetch: "+err.Error())
				m.log.Warn("prepared opening rejected", "key", key, "release", res.Release.ID, "err", err)
			} else if ctx.Err() == nil {
				m.log.Debug("opening prefetch incomplete", "key", key, "err", err)
			}
			return
		}
		m.log.Info("opening prefetch completed", "key", key, "release", res.Release.ID, "bytes", prefetchBytes)
		// Seek indexes are optional for starting playback. Keep filling one
		// under the warm-up budget, but never hold the ready signal for it.
		select {
		case <-generation.indexReady:
		case <-ctx.Done():
			return
		}
		complete := &probeCache{releaseID: res.Release.ID, info: generation.infoSnapshot(), points: generation.timeMapSnapshot().Points}
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
			f.height, f.codec = v.Height, v.Codec
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
	if !reflect.DeepEqual(e.profileObj.ReleaseRules, profile.ReleaseRules) {
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
		if e := m.warm[identity{userID: user.ID, clientID: caps.ClientID, key: key}]; e != nil && now.Before(e.expires) {
			return e
		}
	}
	if m.deps.SelectProfile == nil {
		return nil
	}
	pk := profileKey(m.deps.SelectProfile(caps, user))
	for ident, e := range m.warm {
		if ident.userID == user.ID && ident.key == key && now.Before(e.expires) && e.profileKey == pk {
			return e
		}
	}
	return nil
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

// takeWarmResolution consumes the warm entry for id when its release is
// acceptable for the Play-time profile and caps (and, for a variant,
// is the picked release); an unacceptable entry is discarded.
func (m *Manager) takeWarmResolution(ctx context.Context, id identity, profile core.QualityProfile, caps core.ClientCaps, prefer, originalLanguage string) *warmEntry {
	// Warm facts are keyed by viewer/device/title, then checked against the
	// selected release below; a plan's variant identity is not a warm key.
	id.variant, id.exact = "", false
	m.mu.Lock()
	e := m.warm[id]
	if e == nil {
		m.mu.Unlock()
		return nil
	}
	if !m.now().Before(e.expires) {
		delete(m.warm, id)
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
	select {
	case <-e.done:
	case <-ctx.Done():
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.warm[id] != e {
		return nil
	}
	if prefer != "" && (e.res == nil || e.res.Release.ID != prefer) {
		return nil // keep it: a plain Prepare may still use it
	}
	if !warmAcceptable(e, profile, caps, originalLanguage) || (e.res != nil && m.releaseBlocked(ctx, e.res.Release.ID)) {
		delete(m.warm, id)
		return nil
	}
	if e.probe == nil {
		delete(m.warm, id)
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
// forgets a title's search. Stored source choices are invalidated too;
// watch progress and container indexes remain intact. A show or season key
// also drops its episodes' entries. A warm-up still in flight finishes into
// nothing. It returns how many warm entries were dropped.
func (m *Manager) InvalidateTitle(key core.RatingKey) int {
	if key == 0 {
		return 0
	}
	m.mu.Lock()
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
		if err := m.resume.invalidateTitle(ctx, key); err != nil {
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
