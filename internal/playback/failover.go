package playback

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/releasepolicy"
	"github.com/braedonsaunders/ether/internal/store"
)

// errSourceBroken marks a release whose articles went missing while it was
// being opened or probed.
var errSourceBroken = fmt.Errorf("playback: source broken: %w", core.ErrArticleMissing)

// errRequireRemux reports a completed failover that swapped the session to
// a healthy release whose bytes cannot continue the in-flight direct-play
// response: the response must end and the next decision/start is a remux.
var errRequireRemux = errors.New("playback: failover requires remux")

// openInitial finds and opens the first release of a session: the stored
// resume release when it was resolved for the same profile, else the
// resolver's answer, moving to the next candidate when a release fails.
func (m *Manager) openInitial(ctx context.Context, s *session, profile core.QualityProfile) (*generation, error) {
	var res *core.Resolution
	var cached *probeCache
	fromResume := false
	original := ""
	if s.title != nil {
		original = s.title.Language
	}
	if warm := m.takeWarmResolution(ctx, s.ident, profile, s.caps, s.prefer, original); warm != nil {
		res, cached = warm.res, warm.probe
	}
	if res == nil {
		res = m.resumeResolution(ctx, s.ident.userID, s.ident.key, profile, s.caps, original)
		if res != nil && s.prefer != "" && res.Release.ID != s.prefer {
			res = nil // a picked version wins over the remembered release
		}
		if res != nil {
			fromResume = true
			if cached == nil {
				// A restart drops the in-memory warm probe, but the resume
				// record's persisted probe and keyframe index survive it.
				cached = m.persistedProbe(ctx, s.ident.userID, s.ident.key, res.Release.ID, res.NZB)
			}
			if cached != nil && !rememberedInfoCodecCurrent(cached.info, s.caps) {
				res, cached, fromResume = nil, nil, false
			}
		}
	}
	if res != nil && (!releaseMatchesTitle(res.Release, s.title) || !payloadMatchesTitle(res.NZB, s.title)) {
		// An older stored/warmed selection may predate the series identity
		// gate. Do not reuse a same-named reboot for this show's episode.
		res, cached, fromResume = nil, nil, false
	}
	var (
		tried      []string
		alternates []core.Release
		lastErr    error
		// allUncached: every release that failed to open was a torrent
		// no debrid account could serve (uncached, blocked, add refused).
		allUncached = true
	)
	fallback := func(size int64) core.MediaInfo {
		info := EstimateInfo(s.title, s.caps, profile)
		info.Size = size
		return info
	}
	for attempt := 0; attempt < m.opts.MaxAttempts; attempt++ {
		if res == nil {
			fromResume = false
			var err error
			res, err = m.nextResolution(ctx, s, tried, alternates)
			if err != nil {
				if lastErr != nil && errors.Is(err, core.ErrNotAvailable) {
					if errors.Is(lastErr, core.ErrDebridAuth) {
						return nil, fmt.Errorf("%w (last failure: %v): %w", err, lastErr, core.ErrDebridAuth)
					}
					return nil, fmt.Errorf("%w (last failure: %v)", err, lastErr)
				}
				return nil, err
			}
		}
		if s.exact && res.Release.ID != s.prefer {
			return nil, fmt.Errorf("%w: %w", core.ErrNotAvailable, core.ErrSourceChanged)
		}
		g, err := m.openGeneration(ctx, s, res, fallback, cached)
		if err == nil && fromResume && !s.exact && !rememberedInfoCodecCurrent(g.infoSnapshot(), s.caps) {
			m.dropGeneration(g)
			tried = append(tried, res.Release.ID)
			res, cached, fromResume = nil, nil, false
			continue
		}
		if err == nil {
			s.mu.Lock()
			s.tried = append(s.tried, tried...)
			s.mu.Unlock()
			return g, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		lastErr = err
		if s.exact {
			if errors.Is(err, core.ErrNotFound) {
				return nil, fmt.Errorf("%w: %w", core.ErrNotAvailable, core.ErrSourceChanged)
			}
			return nil, fmt.Errorf("%w: selected release could not be opened", core.ErrNotAvailable)
		}
		allUncached = allUncached && errors.Is(err, core.ErrNotCached)
		m.log.Warn("release failed to open", "release", res.Release.Name, "err", err)
		tried = append(tried, res.Release.ID)
		if len(res.Alternates) > 0 {
			alternates = res.Alternates
		}
		res = nil
	}
	return nil, releasesFailed(len(tried), lastErr, allUncached)
}

// releasesFailed is the error after every attempted release failed to open.
// It always wraps core.ErrNotAvailable and keeps the owner-facing cause
// classifiable: a debrid service rejecting its API key (a configuration
// problem) or every release uncached on the debrid accounts. Other release
// errors stay text only, so release-level sentinels (ErrNotFound and the
// like) cannot change how the failure is reported to the client.
func releasesFailed(n int, lastErr error, allUncached bool) error {
	switch {
	case errors.Is(lastErr, core.ErrDebridAuth):
		return fmt.Errorf("%w: %d releases failed (last: %v): %w", core.ErrNotAvailable, n, lastErr, core.ErrDebridAuth)
	case allUncached && n > 0:
		return fmt.Errorf("%w: %d releases failed (last: %v)", core.ErrNotCached, n, lastErr)
	}
	return fmt.Errorf("%w: %d releases failed (last: %v)", core.ErrNotAvailable, n, lastErr)
}

// nextResolution returns the next release to try: an untried alternate when
// NZBs can be fetched directly, else a fresh Resolve excluding tried ones.
func (m *Manager) nextResolution(ctx context.Context, s *session, tried []string, alternates []core.Release) (*core.Resolution, error) {
	if m.deps.FetchNZB != nil {
		for i, alt := range alternates {
			if alt.ID == "" || slices.Contains(tried, alt.ID) || !releaseMatchesTitle(alt, s.title) {
				continue
			}
			nzb, err := m.deps.FetchNZB(ctx, alt)
			if err != nil {
				if ctx.Err() != nil {
					return nil, ctx.Err()
				}
				m.log.Warn("alternate nzb fetch failed", "release", alt.Name, "err", err)
				continue
			}
			return &core.Resolution{
				Title:      s.ident.key,
				Release:    alt,
				NZB:        nzb,
				Alternates: slices.Clone(alternates[i+1:]),
				ResolvedAt: m.now(),
			}, nil
		}
	}
	u := s.user
	res, err := m.deps.Resolver.Resolve(ctx, core.ResolveRequest{
		Title:              s.title,
		Client:             s.caps,
		User:               &u,
		Exclude:            slices.Clone(tried),
		Prefer:             s.prefer,
		ExactHEVCTranscode: s.exact && s.ident.surface != "" && allowsH264Transcode(s.caps),
	})
	if err != nil {
		return nil, err
	}
	if res == nil || res.Release.ID == "" || len(res.NZB) == 0 {
		return nil, fmt.Errorf("%w: resolver returned no usable release", core.ErrNotAvailable)
	}
	if slices.Contains(tried, res.Release.ID) {
		return nil, fmt.Errorf("%w: resolver returned an excluded release", core.ErrNotAvailable)
	}
	return res, nil
}

// This is only permission to inspect the selected source. The broker's
// decision still checks output delivery, audio, conditions and capacity.
func allowsH264Transcode(c core.ClientCaps) bool {
	if c.Download || c.Profiles == nil {
		return false
	}
	for _, p := range c.Profiles.Transcoding {
		if !strings.EqualFold(p.MediaType, "video") {
			continue
		}
		for _, codec := range p.VideoCodecs {
			if strings.EqualFold(codec, "h264") {
				return true
			}
		}
	}
	return false
}

// openGeneration opens a release through the engine, exposes it on the
// loopback server and probes it. A release whose articles are missing is
// marked bad. When the probe fails for another reason, fallback supplies the
// media info.
func (m *Manager) openGeneration(ctx context.Context, s *session, res *core.Resolution, fallback func(size int64) core.MediaInfo, cached ...*probeCache) (*generation, error) {
	// Recheck at the shared open boundary, including failover and old
	// cached alternates. A mismatch is request-specific, not a bad release.
	if !releaseMatchesTitle(res.Release, s.title) || !payloadMatchesTitle(res.NZB, s.title) {
		return nil, fmt.Errorf("%w: release does not match requested episode", core.ErrNotAvailable)
	}
	src, finish, err := m.openSource(ctx, s.sources, res)
	if err != nil {
		if permanentSourceFailure(err) {
			m.markBad(res.Release.ID, "invalid source on open")
		}
		return nil, fmt.Errorf("open %s: %w", res.Release.Name, err)
	}
	if src.Size() <= 0 {
		finish(src.Close())
		return nil, fmt.Errorf("open %s: empty media file", res.Release.Name)
	}
	return m.probeGeneration(ctx, s, res, src, finish, fallback, cached...)
}

func (m *Manager) probeGeneration(ctx context.Context, s *session, res *core.Resolution, src core.MediaSource, finish func(error), fallback func(int64) core.MediaInfo, cached ...*probeCache) (*generation, error) {
	inner, closeShared := unwrapShared(src)
	// Probes, revisions and optional source interfaces must see the backend,
	// not the refcount wrapper. Close stays on the wrapper.
	share := src
	src = inner
	openedRevision := core.SourceRevision(&core.PlaySession{Key: s.ident.key, Source: src})
	g := &generation{sess: s, n: s.nextGen(), token: randomID(16), src: src, share: share, sharedClose: closeShared, finishSource: finish, res: res, indexReady: make(chan struct{})}
	g.ctx, g.cancel = context.WithCancel(m.ctx)
	m.mu.Lock()
	m.tokens[s.id+"/"+g.token] = g
	m.mu.Unlock()

	probeStarted := time.Now()
	pctx, cancel := context.WithTimeout(ctx, m.opts.ProbeTimeout)
	var info *core.MediaInfo
	var perr error
	var facts *probeCache
	prober := "cache"
	if len(cached) > 0 && cached[0] != nil && cached[0].releaseID == res.Release.ID && cached[0].info.Size == src.Size() &&
		(cached[0].nzbHash == "" || (len(res.NZB) > 0 && cached[0].nzbHash == nzbHash(res.NZB))) {
		facts = cached[0]
		copy := facts.info
		info = &copy
	} else {
		info, prober, perr = m.probeMedia(pctx, g, src)
	}
	cancel()
	probeDuration := time.Since(probeStarted)
	var mi core.MediaInfo
	switch {
	case g.sourceFailure() != nil || g.articleMissing.Load():
		failure := g.sourceFailure()
		if failure == nil {
			failure = errSourceBroken
		}
		m.dropGeneration(g)
		if permanentSourceFailure(failure) {
			m.markBad(res.Release.ID, "invalid source while probing")
		}
		return nil, failure
	case perr != nil && ctx.Err() != nil:
		m.dropGeneration(g)
		return nil, ctx.Err()
	case prober == "native" && permanentSourceFailure(perr):
		// The native probe read the source directly, so the loopback
		// server never saw the failure.
		m.dropGeneration(g)
		m.markBad(res.Release.ID, "invalid source while probing")
		return nil, perr
	case perr != nil || info == nil:
		m.log.Warn("probe failed; using estimated media info", "release", res.Release.Name, "err", perr)
		mi = core.CloneMediaInfo(fallback(src.Size()))
		mi.Estimated = true
	default:
		mi = core.CloneMediaInfo(*info)
	}
	if mi.Size <= 0 {
		mi.Size = src.Size()
	}
	if mi.Container == "" {
		mi.Container = containerFromName(src.Name())
	}
	// A cached warm probe was already augmented when it was made; running
	// AugmentMedia again would put its cost back on the Play path. Facts
	// rebuilt from the persisted probe had their per-viewer sidecars
	// stripped before they reached disk, so they augment again.
	augmentStarted := time.Now()
	if (facts == nil || facts.needsAugment) && m.deps.AugmentMedia != nil {
		m.deps.AugmentMedia(ctx, s.title, &s.user, res, src, &mi)
	}
	m.log.Debug("media preparation completed", "session", s.id, "release", res.Release.ID,
		"probe", probeDuration, "probeCached", facts != nil, "prober", prober, "estimated", mi.Estimated,
		"augment", time.Since(augmentStarted))
	var points []IndexPoint
	if facts != nil {
		points = facts.points
		if len(points) == 0 {
			points = indexPointsOf(facts.info.Keyframes)
		}
	} else if len(mi.Keyframes) > 0 {
		points = indexPointsOf(mi.Keyframes)
	}
	mi.Keyframes = keyframesOf(points)
	needIndex := len(points) == 0 && !mi.Estimated && mi.Duration > 0
	// A historical open observation is never fresh authorization. Validation
	// belongs to source opening/probing, outside manager/session locks; a path
	// replacement or unsupported identity leaves all source facts unknown.
	if openedRevision != core.SourceRevision(&core.PlaySession{Key: s.ident.key, Source: src}) {
		openedRevision = ""
	}
	observation := core.PlaybackSourceObservation{}
	if openedRevision != "" {
		observation.Revision, observation.ReleaseID = openedRevision, res.Release.ID
		if !mi.Estimated {
			if video := mi.Video(); video != nil {
				observation.VideoCodec, observation.Height = video.Codec, video.Height
				observation.HDR = observedHDR(*video)
			}
		}
	}
	// The loopback server may already hold g; publish under its lock.
	g.publishInfo(mi, observation, TimeMap{Size: src.Size(), Duration: mi.Duration, Points: points})
	if needIndex {
		// Play never waits for the container index: publish now and fill
		// TimeMap.Points / MediaInfo.Keyframes in the background.
		m.startIndexFill(g)
	} else {
		g.markIndexReady()
	}
	if prober == "native" || (facts != nil && facts.needsAugment) {
		// Persisted facts predate any tail fill; it only fills facts
		// that are still unknown, so a published decision never changes.
		m.startTrailerFill(g)
	}
	return g, nil
}

// probeMedia reads stream parameters from the container headers when the
// native prober can vouch for them, and otherwise runs ffprobe through the
// loopback URL. A native probe costs no process and, for a validated Usenet
// source, usually no network read: the first article is already cached.
func (m *Manager) probeMedia(ctx context.Context, g *generation, src core.MediaSource) (*core.MediaInfo, string, error) {
	if m.deps.NativeProbe != nil {
		started := time.Now()
		info, err := m.deps.NativeProbe(ctx, src)
		if err == nil && info != nil {
			if m.opts.VerifyNativeProbe && g.res != nil {
				m.startProbeVerify(g.sess.sources, g.res, *info)
			}
			return info, "native", nil
		}
		if ctx.Err() != nil || permanentSourceFailure(err) {
			return nil, "native", err
		}
		m.log.Debug("native probe deferred to ffprobe", "release", g.releaseID(), "took", time.Since(started), "err", err)
	}
	info, err := m.deps.Transcoder.Probe(ctx, m.urlFor(g))
	return info, "ffprobe", err
}

// startTrailerFill reads Matroska facts kept after the media (tail
// chapters, per-track statistics) off the Play path and merges only
// facts that were unknown, so a running decision never changes.
func (m *Manager) startTrailerFill(g *generation) {
	if m.deps.NativeTrailer == nil || !g.acquire() {
		return
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		g.release()
		return
	}
	m.wg.Add(1)
	m.mu.Unlock()
	go func() {
		// Release can block in a backend Close. Leave the manager wait group
		// first so shutdown is not pinned behind that Close.
		defer g.release()
		defer m.wg.Done()
		// The trailer usually sits next to the Cues the index fill reads;
		// waiting lets it share that fetch instead of competing with it.
		select {
		case <-g.indexReady:
		case <-g.ctx.Done():
			return
		}
		ctx, cancel := context.WithTimeout(g.ctx, m.opts.IndexTimeout)
		defer cancel()
		info := g.infoSnapshot()
		if !m.deps.NativeTrailer(ctx, g.src, &info) {
			return
		}
		g.mu.Lock()
		if len(g.info.Chapters) == 0 {
			g.info.Chapters = slices.Clone(info.Chapters)
		}
		streams := slices.Clone(g.info.Streams)
		for i := range streams {
			if streams[i].BitrateKbps == 0 && i < len(info.Streams) && info.Streams[i].Index == streams[i].Index {
				streams[i].BitrateKbps = info.Streams[i].BitrateKbps
			}
		}
		g.info.Streams = streams
		g.mu.Unlock()
	}()
}

// startProbeVerify runs ffprobe after a native probe and logs any
// decision-relevant disagreement. It is a soak-test aid, off by default.
// The check opens its own source and serves it on a private loopback
// listener, because the generation that was probed may be retired (a
// decision-only session) long before ffprobe finishes.
func (m *Manager) startProbeVerify(sources *sourceLifetime, res *core.Resolution, native core.MediaInfo) {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return
	}
	m.wg.Add(1)
	m.mu.Unlock()
	go func() {
		defer m.wg.Done()
		ctx, cancel := context.WithTimeout(m.ctx, 2*m.opts.ProbeTimeout)
		defer cancel()
		src, finish, err := m.openSource(ctx, sources, res)
		if err != nil {
			return
		}
		inner, closeShared := unwrapShared(src)
		verification := &generation{src: inner, sharedClose: closeShared, finishSource: finish}
		defer verification.retire()
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return
		}
		srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !verification.acquire() {
				http.Error(w, "gone", http.StatusGone)
				return
			}
			defer verification.release()
			http.ServeContent(w, r, src.Name(), time.Time{}, io.NewSectionReader(src, 0, src.Size()))
		})}
		go srv.Serve(ln)
		defer srv.Close()
		ref, err := m.deps.Transcoder.Probe(ctx, "http://"+ln.Addr().String()+"/media")
		if err != nil || ref == nil {
			m.log.Debug("native probe verification skipped", "release", res.Release.Name, "err", err)
			return
		}
		if diff := probeDifference(native, *ref); diff != "" {
			m.log.Warn("native probe disagrees with ffprobe", "release", res.Release.Name, "diff", diff)
			return
		}
		m.log.Info("native probe verified", "release", res.Release.Name, "streams", len(native.Streams))
	}()
}

// probeDifference compares the facts playback decisions use.
func probeDifference(native, ref core.MediaInfo) string {
	var diffs []string
	add := func(format string, args ...any) { diffs = append(diffs, fmt.Sprintf(format, args...)) }
	if d := native.Duration - ref.Duration; d > time.Second || d < -time.Second {
		add("duration %v/%v", native.Duration, ref.Duration)
	}
	if len(native.Streams) != len(ref.Streams) {
		add("streams %d/%d", len(native.Streams), len(ref.Streams))
		return strings.Join(diffs, "; ")
	}
	for i, n := range native.Streams {
		r := ref.Streams[i]
		if r.ColorTransfer == "unknown" {
			r.ColorTransfer = ""
		}
		if n.Index != r.Index || n.Type != r.Type || n.Codec != r.Codec || n.Language != r.Language || n.Default != r.Default || n.Forced != r.Forced {
			add("stream %d identity %d/%s/%s/%s vs %d/%s/%s/%s", i, n.Index, n.Type, n.Codec, n.Language, r.Index, r.Type, r.Codec, r.Language)
		}
		switch n.Type {
		case "video":
			if !strings.EqualFold(n.Profile, r.Profile) || n.Level != r.Level || n.Width != r.Width || n.Height != r.Height || n.BitDepth != r.BitDepth ||
				n.ColorTransfer != r.ColorTransfer || n.DolbyVision != r.DolbyVision || n.DolbyVisionProfile != r.DolbyVisionProfile ||
				n.DolbyVisionCompatibilityID != r.DolbyVisionCompatibilityID || n.CodecString != r.CodecString {
				add("video %+v vs %+v", n, r)
			}
		case "audio":
			if n.Channels != r.Channels || n.SampleRate != r.SampleRate || (n.Profile != "" && !strings.EqualFold(n.Profile, r.Profile)) {
				add("audio %d %s %dch %dHz vs %s %dch %dHz", n.Index, n.Profile, n.Channels, n.SampleRate, r.Profile, r.Channels, r.SampleRate)
			}
		}
	}
	return strings.Join(diffs, "; ")
}

// startIndexFill reads the container keyframe index off the critical path
// and installs it on the generation (and the session's virtual map when
// this generation defines it) once read.
func (m *Manager) startIndexFill(g *generation) {
	if !g.acquire() {
		g.markIndexReady()
		return
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		g.release()
		g.markIndexReady()
		return
	}
	m.wg.Add(1)
	m.mu.Unlock()
	go func() {
		// Release can block in a backend Close. Leave the manager wait group
		// first so shutdown is not pinned behind that Close.
		defer g.release()
		defer m.wg.Done()
		defer g.markIndexReady()
		ctx, cancel := context.WithTimeout(g.ctx, m.opts.IndexTimeout)
		defer cancel()
		points := containerIndex(ctx, g.src, g.src.Size())
		if len(points) == 0 {
			return
		}
		g.setIndex(points)
		s := g.sess
		// Seeking uses the index at once; persisting it for later resumes
		// can take seconds (or wait out a database outage).
		s.mu.Lock()
		if s.virtualGen == g {
			s.virtual.Points = points
		}
		s.mu.Unlock()
		if m.resume != nil {
			ctx, cancel := context.WithTimeout(m.ctx, 5*time.Second)
			if err := m.resume.saveIndex(ctx, s.ident.userID, s.ident.key, g.releaseID(), keyframesOf(points)); err != nil {
				if store.IsUnavailable(err) {
					// Only an optimisation for later resumes; the stream
					// itself never needs it.
					m.log.Debug("save resume keyframe index skipped: database unreachable", "key", s.ident.key, "err", err)
				} else {
					m.log.Warn("save resume keyframe index failed", "key", s.ident.key, "err", err)
				}
			}
			cancel()
		}
	}()
}

// dropGeneration discards a generation that never became current.
func (m *Manager) dropGeneration(g *generation) {
	m.mu.Lock()
	delete(m.tokens, g.sess.id+"/"+g.token)
	m.mu.Unlock()
	g.retire()
}

func (m *Manager) markBad(releaseID, reason string) {
	ctx, cancel := context.WithTimeout(m.ctx, 10*time.Second)
	defer cancel()
	if err := m.deps.Resolver.MarkBad(ctx, releaseID, reason); err != nil {
		m.log.Warn("mark release bad failed", "release", releaseID, "err", err)
	}
}

// Failover swaps the session to the next candidate release, keeping the
// timestamp at. The current release is marked bad. When an HLS session
// reads from this playback session it is restarted on the new input at the
// same position. Returns the updated session.
func (m *Manager) Failover(ctx context.Context, sessionID string, at time.Duration) (*core.PlaySession, error) {
	s := m.lookup(sessionID)
	if s == nil {
		return nil, fmt.Errorf("playback: session %s: %w", sessionID, core.ErrNotFound)
	}
	if _, err := m.failover(ctx, s, s.current(), at, "failover requested"); err != nil && !errors.Is(err, errRequireRemux) {
		return nil, err
	}
	return s.snapshot(m.now()), nil
}

// FailoverInput handles an exhausted encoder input. The input URL identifies
// the exact source generation, so an old encoder's delayed exit cannot discard
// a newer source. Exhausted recovery closes the session and its resources.
func (m *Manager) FailoverInput(ctx context.Context, sessionID, inputURL string, at time.Duration) error {
	s := m.lookup(sessionID)
	if s == nil {
		return nil // the client already stopped
	}
	g := s.current()
	if g == nil || m.urlFor(g) != inputURL {
		return nil // another reader already recovered this input
	}
	_, err := m.failoverWith(ctx, s, g, at, failoverOpts{reason: "encoder input failed", stopOnFailure: true})
	return err
}

// failoverOpts distinguishes a broken-source failover (markBad) from a
// speed-driven down-failover (down: pick a lower-bitrate candidate, never
// mark the slow release bad).
type failoverOpts struct {
	stillNeeded    func() bool // speculative recovery rechecks before publishing a swap
	quietFailure   bool
	preserveBuffer bool
	reason         string
	markBad        bool
	down           bool
	stopOnFailure  bool
	// followPlayer: at was mapped from a failed source read, which may be
	// a container tail/index read or predate a seek; resume at the
	// player's latest segment request when the HLS session reports it.
	followPlayer bool
}

func (m *Manager) failover(ctx context.Context, s *session, observed *generation, at time.Duration, reason string) (*generation, error) {
	return m.failoverWith(ctx, s, observed, at, failoverOpts{reason: reason, markBad: true})
}

// failoverWith replaces generation observed. Concurrent callers that
// observed the same failing generation share one swap: the first does the
// work and the others return the generation it installed.
func (m *Manager) failoverWith(ctx context.Context, s *session, observed *generation, at time.Duration, fo failoverOpts) (result *generation, retErr error) {
	if err := lock(ctx, s.failSem); err != nil {
		return nil, err
	}
	defer unlock(s.failSem)

	s.mu.Lock()
	cur, closed, failovers, pinned := s.cur, s.closed, s.failovers, s.sourcePins > 0
	direct := s.decision == core.DecisionDirectPlay && !spliceable(s.container, s.name)
	s.mu.Unlock()
	if closed {
		return nil, fmt.Errorf("playback: session %s ended: %w", s.id, core.ErrNotFound)
	}
	if cur != observed {
		return cur, nil // someone else already swapped
	}
	if fo.stillNeeded != nil && !fo.stillNeeded() {
		return cur, nil
	}
	if fo.stopOnFailure {
		defer func() {
			if retErr != nil && !errors.Is(retErr, errRequireRemux) {
				m.Stop(s.id)
			}
		}()
	}
	if s.exact || pinned {
		return nil, fmt.Errorf("%w: selected release cannot be replaced", core.ErrNotAvailable)
	}
	if failovers >= m.opts.MaxFailovers {
		return nil, fmt.Errorf("%w: session %s gave up after %d failovers", core.ErrNotAvailable, s.id, failovers)
	}
	log := m.log.With("session", s.id, "key", s.ident.key, "at", at)
	log.Warn("failing over", "release", cur.res.Release.Name, "reason", fo.reason, "down", fo.down)
	// A provider outage is not evidence that the release itself is invalid.
	if failure := cur.sourceFailure(); permanentSourceFailure(failure) {
		m.markBad(cur.releaseID(), fo.reason)
	}

	s.mu.Lock()
	s.tried = append(s.tried, cur.releaseID())
	tried := slices.Clone(s.tried)
	alternates := s.alternates
	s.mu.Unlock()

	oldInfo := cur.infoSnapshot()
	if direct {
		// Identical-size reposts can continue the client's byte stream;
		// try them before opening candidates in rank order.
		alternates = orderExactSizeFirst(alternates, cur.res.Release)
	} else if fo.down {
		alternates = orderLowerBitrateFirst(alternates, cur.res.Release)
	}
	fallback := func(size int64) core.MediaInfo {
		info := oldInfo
		info.Size = size
		info.Estimated = true
		info.Keyframes = nil
		return info
	}
	curBps := mediaBps(oldInfo, cur.src.Size())
	var (
		g           *generation
		fallbackGen *generation // healthy but not spliceable into direct play
		remuxSwitch bool
		lastErr     error
	)
	if fo.down {
		g = m.joinSpeedAlternate(ctx, cur, tried)
		if g != nil && m.preflightSwap(ctx, s, g) != nil {
			m.dropGeneration(g)
			g = nil
		}
	}
	prepared := cur.takeDamageResolution(tried)
	if g == nil && !direct && prepared == nil {
		// Any healthy alternate continues an encoder session, so open
		// several at once instead of stalling through each broken one.
		var failed []string
		width := failoverRaceWidth
		var accept func(*generation) error
		if fo.down {
			width = 2
			accept = func(next *generation) error {
				bps := mediaBps(next.infoSnapshot(), next.src.Size())
				if curBps > 0 && (bps <= 0 || bps >= curBps) {
					return fmt.Errorf("down-failover candidate is not lower bitrate: %w", core.ErrNotAvailable)
				}
				return nil
			}
		}
		g, failed, lastErr = m.raceAlternates(ctx, s, tried, alternates, fallback, width, accept)
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		tried = append(tried, failed...)
	}
	for attempt := 0; g == nil && attempt < m.opts.MaxAttempts; attempt++ {
		res := prepared
		prepared = nil
		var err error
		if res == nil {
			res, err = m.nextResolution(ctx, s, tried, alternates)
		}
		if err != nil {
			lastErr = err
			break
		}
		g, err = m.openGeneration(ctx, s, res, fallback)
		if err == nil {
			if perr := m.preflightSwap(ctx, s, g); perr != nil {
				m.dropGeneration(g)
				g, err = nil, perr
			}
		}
		// Byte-range clients retain the original container headers and
		// offsets. Only an exact-size repost may replace a non-spliceable
		// direct source in place; a healthy mismatched candidate is kept
		// as a remux-switch fallback instead of being spliced in.
		if err == nil && direct && g.src.Size() != cur.src.Size() {
			keep := fallbackGen == nil
			if !keep && fo.down {
				// A down-failover's remux fallback only helps at a lower
				// bitrate, so a lower mismatch supersedes an earlier
				// non-lower one instead of being dropped behind it. The
				// existing veto below still rejects a fallback that is
				// not lower when no lower mismatch was found.
				bps := mediaBps(g.infoSnapshot(), g.src.Size())
				fallbackBps := mediaBps(fallbackGen.infoSnapshot(), fallbackGen.src.Size())
				if curBps > 0 && bps > 0 && bps < curBps && !(fallbackBps > 0 && fallbackBps < curBps) {
					m.dropGeneration(fallbackGen)
					fallbackGen, keep = nil, true
				}
			}
			if keep {
				fallbackGen = g
			} else {
				m.dropGeneration(g)
			}
			g = nil
			err = fmt.Errorf("incompatible direct-play replacement size: %w", core.ErrNotAvailable)
		}
		if err == nil && fo.down {
			if bps := mediaBps(g.infoSnapshot(), g.src.Size()); curBps > 0 && bps > 0 && bps >= curBps {
				m.dropGeneration(g)
				g = nil
				err = fmt.Errorf("down-failover candidate is not lower bitrate: %w", core.ErrNotAvailable)
			}
		}
		if err == nil {
			break
		}
		if ctx.Err() != nil {
			if fallbackGen != nil {
				m.dropGeneration(fallbackGen)
			}
			return nil, ctx.Err()
		}
		lastErr = err
		log.Warn("failover candidate failed", "release", res.Release.Name, "err", err)
		tried = append(tried, res.Release.ID)
		if len(res.Alternates) > 0 {
			alternates = res.Alternates
		}
		if direct {
			alternates = orderExactSizeFirst(alternates, cur.res.Release)
		} else if fo.down {
			alternates = orderLowerBitrateFirst(alternates, cur.res.Release)
		}
	}
	if g == nil && fallbackGen != nil && fo.down {
		// A down-failover only helps with a lower-bitrate fallback.
		if bps := mediaBps(fallbackGen.infoSnapshot(), fallbackGen.src.Size()); curBps > 0 && bps > 0 && bps >= curBps {
			m.dropGeneration(fallbackGen)
			fallbackGen = nil
		}
	}
	if g == nil && fallbackGen != nil {
		// No exact-size repost: do not splice. Swap anyway and restart
		// the client as a zero-encode remux at the mapped time.
		g, fallbackGen, remuxSwitch = fallbackGen, nil, true
		log.Warn("no exact-size direct-play replacement; switching to remux", "release", g.res.Release.Name)
	}
	if fallbackGen != nil {
		m.dropGeneration(fallbackGen)
	}
	if g == nil {
		log.Error("failover found no working release", "err", lastErr)
		if lastErr == nil {
			lastErr = core.ErrNotAvailable
		}
		if !fo.quietFailure {
			m.emitEvent("error", s, lastErr)
		}
		if fo.down {
			m.noteFailedDownFailover(s, tried)
		}
		return nil, fmt.Errorf("playback: failover: %w", lastErr)
	}

	if fo.stillNeeded != nil && !fo.stillNeeded() {
		m.dropGeneration(g)
		return cur, nil
	}
	// An HLS session restarts ffmpeg on fresh headers; anything else that
	// swaps to a different-size file must be remuxed. Mark that in the same
	// critical section as the swap, so no request sees the new release
	// without the remux requirement.
	hls, hasHLS := m.deps.HLSLookup(s.id)
	hasHLS = hasHLS && hls != nil
	if hasHLS {
		remuxSwitch = false
	}

	// Pin before publishing g: a Stop right after the swap unpins s.cur,
	// and must find this pin rather than leave one taken later behind.
	m.pinLive(g.releaseID())
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		m.dropGeneration(g)
		m.unpinLive(g.releaseID())
		return nil, fmt.Errorf("playback: session %s ended: %w", s.id, core.ErrNotFound)
	}
	s.cur = g
	if remuxSwitch {
		s.requiresRemux = true
	}
	s.failovers++
	if fo.down {
		s.downFailovers++
	}
	// Racers that failed after the winner may already have been recorded.
	for _, id := range tried {
		if !slices.Contains(s.tried, id) {
			s.tried = append(s.tried, id)
		}
	}
	if len(g.res.Alternates) > 0 {
		s.alternates = g.res.Alternates
	} else {
		s.alternates = withoutTried(alternates, append(slices.Clone(s.tried), g.releaseID()))
	}
	s.mu.Unlock()

	m.mu.Lock()
	delete(m.tokens, s.id+"/"+cur.token)
	m.mu.Unlock()
	cur.retire()
	m.unpinLive(cur.releaseID())
	m.saveResume(ctx, s, g)
	s.resetSpeed(m.now(), m.opts.SpeedGrace)

	newInfo := g.infoSnapshot()
	if hasHLS {
		if position, ok := hls.(core.PlaybackPosition); ok && fo.followPlayer {
			// Read only now: the player may have sought while candidates
			// were being opened.
			requested := position.RequestedPosition()
			log.Info("failover follows player", "requested", requested)
			at = requested
		}
		target := m.alignForFailover(ctx, s, cur, g, at)
		swap := hls.SwapInput
		if buffered, ok := hls.(core.BufferedInputSwapper); ok && fo.preserveBuffer && target == at {
			if source, ok := hls.(core.RecoveryBufferSource); ok {
				boundary := source.RecoveryBuffer().ReadyUntil
				// Check the handoff itself, not only a zero playhead where PAL
				// speed corrections and different edits can happen to agree.
				if boundary > at && boundary < newInfo.Duration && m.alignForFailover(ctx, s, cur, g, boundary) == boundary {
					swap = buffered.SwapInputBuffered
				}
			}
		}
		if err := swap(ctx, m.urlFor(g), newInfo, target); err != nil {
			log.Error("hls input swap failed", "err", err)
			return g, fmt.Errorf("playback: swap hls input: %w", err)
		}
	}
	if fo.down {
		m.emitEvent("downfailover", s, nil)
	} else {
		m.emitEvent("failover", s, nil)
	}
	log.Info("failed over", "release", g.res.Release.Name, "size", g.src.Size(), "duration", newInfo.Duration, "remuxSwitch", remuxSwitch)
	if remuxSwitch {
		return g, errRequireRemux
	}
	return g, nil
}

// alignForFailover maps a failover timestamp between releases: through
// the optional dialogue TimelineAligner (300ms budget) when wired, else
// duration-based AlignTime (PAL rule).
func (m *Manager) alignForFailover(ctx context.Context, s *session, from, to *generation, at time.Duration) time.Duration {
	fromDur := from.infoSnapshot().Duration
	toDur := to.infoSnapshot().Duration
	if m.deps.Aligner != nil && from.res != nil && to.res != nil {
		req := core.AlignRequest{
			Key:          s.ident.key,
			From:         from.res.Release,
			To:           to.res.Release,
			FromDuration: fromDur,
			ToDuration:   toDur,
		}
		type result struct {
			mapped time.Duration
			ok     bool
		}
		ch := make(chan result, 1)
		actx, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
		go func() {
			mapped, ok := m.deps.Aligner.AlignTime(actx, req, at)
			ch <- result{mapped, ok}
		}()
		select {
		case r := <-ch:
			cancel()
			if r.ok {
				if r.mapped < 0 {
					return 0
				}
				if toDur > 0 && r.mapped > toDur {
					return toDur
				}
				return r.mapped
			}
		case <-actx.Done():
			cancel()
			m.log.Debug("timeline aligner timed out; using duration alignment", "session", s.id)
		}
	}
	return AlignTime(at, fromDur, toDur)
}

// orderExactSizeFirst stably moves candidates whose known size matches
// the failing release (identical reposts — same size, then same name,
// then same group) ahead of rank order for direct-play failover.
func orderExactSizeFirst(alts []core.Release, cur core.Release) []core.Release {
	if cur.Size <= 0 || len(alts) < 2 {
		return alts
	}
	out := slices.Clone(alts)
	rank := func(r core.Release) int {
		switch {
		case r.Size == cur.Size && r.Name == cur.Name:
			return 0
		case r.Size == cur.Size && r.Info.Group != "" && r.Info.Group == cur.Info.Group:
			return 1
		case r.Size == cur.Size:
			return 2
		}
		return 3
	}
	sortStableByRank(out, rank)
	return out
}

// orderLowerBitrateFirst stably moves candidates of known smaller size
// (lower bitrate at equal duration) ahead for down-failover, keeping the
// resolver's ranking within each group.
func orderLowerBitrateFirst(alts []core.Release, cur core.Release) []core.Release {
	if cur.Size <= 0 || len(alts) < 2 {
		return alts
	}
	out := slices.Clone(alts)
	rank := func(r core.Release) int {
		if r.Size > 0 && r.Size < cur.Size {
			return 0
		}
		return 1
	}
	sortStableByRank(out, rank)
	return out
}

func sortStableByRank(rs []core.Release, rank func(core.Release) int) {
	for i := 1; i < len(rs); i++ {
		for j := i; j > 0 && rank(rs[j]) < rank(rs[j-1]); j-- {
			rs[j], rs[j-1] = rs[j-1], rs[j]
		}
	}
}

func withoutTried(rs []core.Release, tried []string) []core.Release {
	var out []core.Release
	for _, r := range rs {
		if !slices.Contains(tried, r.ID) {
			out = append(out, r)
		}
	}
	return out
}

func containerFromName(name string) string {
	switch ct := contentTypeFor("", name); ct {
	case "video/x-matroska":
		return "mkv"
	case "video/mp4":
		return "mp4"
	case "video/mp2t":
		return "mpegts"
	case "video/x-msvideo":
		return "avi"
	case "video/webm":
		return "webm"
	}
	return ""
}

// ---------------------------------------------------------------------------
// Resume records

// resumeResolution rebuilds a Resolution from the stored resume record when
// it was resolved for the same profile selection key (configured
// definition, pre-clamp) and, when the profile compares original-language
// audio, the same catalogue language. Legacy records store a plain name,
// never match, and are replaced by the fresh resolution's record. A
// language mismatch drops the memory for this new selection; it does not
// change a stream that is already open.
func (m *Manager) resumeResolution(ctx context.Context, userID int64, key core.RatingKey, profile core.QualityProfile, caps core.ClientCaps, originalLanguage string) *core.Resolution {
	if m.resume == nil || m.deps.Availability.Down() {
		return nil
	}
	if res := m.resume.rememberedResolution(userID, key, profile, profileKey(profile), originalLanguage, caps); res != nil && !m.releaseBlocked(ctx, res.Release.ID) && m.resumeSourceAllowed(res.Release) {
		return res
	}
	rec, err := m.resume.get(ctx, userID, key)
	if err != nil {
		m.log.Warn("resume lookup failed", "err", err)
		return nil
	}
	if rec == nil || m.releaseBlocked(ctx, rec.releaseID) || rec.profile != profileKey(profile) || !rememberedOriginalAudioCurrent(profile, rec.originalLanguage, originalLanguage, rec.release) || !rememberedCodecCurrent(rec.release, caps) || !m.resumeSourceAllowed(rec.release) || !sourceFitsBitrate(rec.size, rec.duration, 0, caps.MaxBitrateKbps) {
		return nil
	}
	nzb, err := m.resume.readResumeNZB(ctx, rec)
	if err != nil {
		m.log.Warn("stored nzb unreadable; resolving afresh", "release", rec.releaseID, "err", err)
		return nil
	}
	m.log.Debug("reusing stored release", "key", key, "release", rec.release.Name)
	return &core.Resolution{Title: key, Release: rec.release, NZB: nzb, ResolvedAt: rec.updatedAt}
}

// saveResume remembers the release now serving a session.
func (m *Manager) saveResume(ctx context.Context, s *session, g *generation) {
	if m.resume == nil || g.res == nil || len(g.res.NZB) == 0 {
		return
	}
	info := g.infoSnapshot()
	rec := resumeRecord{
		userID:           s.ident.userID,
		key:              s.ident.key,
		release:          g.res.Release,
		profile:          s.profileKey,
		originalLanguage: s.originalLanguage,
		size:             g.src.Size(),
		duration:         info.Duration,
		updatedAt:        m.now(),
	}
	nzb := append([]byte(nil), g.res.NZB...)
	points := append([]core.KeyframePoint(nil), info.Keyframes...)
	var liveProbe *probeCache
	if facts, ok := persistedProbeFacts(info); ok {
		liveProbe = &probeCache{
			releaseID:    rec.release.ID,
			info:         facts,
			points:       indexPointsOf(points),
			needsAugment: true,
			nzbHash:      nzbHash(nzb),
		}
	}
	// The next prepare reads this copy. The durable row, including its NZB
	// rename, follows after the response so a slow disk cannot stall it.
	m.resume.noteLive(rec, nzb, liveProbe)
	m.beginResumeSave()
	go func() {
		defer m.endResumeSave()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := m.resume.putWithProbe(ctx, rec, nzb, points, info); err != nil {
			m.log.Warn("save resume record failed", "key", rec.key, "err", err)
		}
	}()
}

// ResumePoint returns the release that last served (or was prefetched for)
// a user's title, with its NZB; nil when there is none.
func (m *Manager) ResumePoint(ctx context.Context, userID int64, key core.RatingKey) (*ResumePoint, error) {
	if m.resume == nil {
		return nil, nil
	}
	if live := m.resume.livePoint(userID, key); live != nil {
		if m.releaseBlocked(ctx, live.Release.ID) {
			return nil, nil
		}
		if pts, err := m.resume.loadIndex(ctx, userID, key, live.Release.ID); err == nil {
			live.Keyframes = pts
		}
		return live, nil
	}
	rec, err := m.resume.get(ctx, userID, key)
	if err != nil || rec == nil {
		return nil, err
	}
	if m.releaseBlocked(ctx, rec.releaseID) {
		return nil, nil
	}
	nzb, err := m.resume.readResumeNZB(ctx, rec)
	if err != nil {
		return nil, err
	}
	point := rec.point(nzb)
	point.Keyframes, err = m.resume.loadIndex(ctx, userID, key, rec.releaseID)
	if err != nil {
		return nil, err
	}
	return point, nil
}

// Remember stores a resolution for a title the user has not played yet
// (a prefetched next episode) so the eventual Prepare opens the same,
// already warm release. An existing record for the same profile is kept.
func (m *Manager) Remember(ctx context.Context, user *core.User, key core.RatingKey, caps core.ClientCaps, res *core.Resolution) error {
	if m.resume == nil || user == nil || res == nil || len(res.NZB) == 0 {
		return nil
	}
	prof := m.deps.SelectProfile(caps, user)
	profile := profileKey(prof)
	// Profiles that do not compare original-language audio keep the
	// previous resume path: no catalogue read, and the language column
	// stays blank.
	current := ""
	if releasepolicy.UsesOriginalAudio(prof.ReleaseRules) {
		lang, err := m.catalogueLanguage(ctx, key)
		if err != nil {
			return err
		}
		current = lang
	}
	rec, err := m.resume.get(ctx, user.ID, key)
	if err != nil {
		return err
	}
	if rec != nil && rec.profile == profile && rememberedOriginalAudioCurrent(prof, rec.originalLanguage, current, rec.release) && rememberedCodecCurrent(rec.release, caps) && m.resumeSourceAllowed(rec.release) && sourceFitsBitrate(rec.size, rec.duration, 0, caps.MaxBitrateKbps) {
		return nil
	}
	return m.resume.save(ctx, resumeRecord{
		userID:           user.ID,
		key:              key,
		release:          res.Release,
		profile:          profile,
		originalLanguage: current,
		updatedAt:        m.now(),
	}, res.NZB)
}

// ResumeFor is ResumePoint restricted to a record resolved for the quality
// profile this client and user would get, i.e. the release a Prepare from
// that client would open.
func (m *Manager) ResumeFor(ctx context.Context, user *core.User, key core.RatingKey, caps core.ClientCaps) (*ResumePoint, error) {
	if user == nil {
		return nil, nil
	}
	rp, err := m.ResumePoint(ctx, user.ID, key)
	if err != nil || rp == nil {
		return nil, err
	}
	prof := m.deps.SelectProfile(caps, user)
	current := ""
	if releasepolicy.UsesOriginalAudio(prof.ReleaseRules) {
		lang, err := m.catalogueLanguage(ctx, key)
		if err != nil {
			return nil, nil
		}
		current = lang
	}
	if rp.profileKey != profileKey(prof) || !rememberedOriginalAudioCurrent(prof, rp.originalLanguage, current, rp.Release) || !rememberedCodecCurrent(rp.Release, caps) || !m.resumeSourceAllowed(rp.Release) || !sourceFitsBitrate(rp.Size, rp.Duration, 0, caps.MaxBitrateKbps) {
		return nil, nil
	}
	if probe := m.persistedProbe(ctx, user.ID, key, rp.Release.ID, rp.NZB); probe != nil && !rememberedInfoCodecCurrent(probe.info, caps) {
		return nil, nil
	}
	rp.Profile = prof.Name // name from the profile object, not the stored column
	return rp, nil
}

func titleLanguage(t *core.Title) string {
	if t == nil {
		return ""
	}
	return strings.TrimSpace(t.Language)
}

func (m *Manager) catalogueLanguage(ctx context.Context, key core.RatingKey) (string, error) {
	if m.deps.Catalog == nil {
		return "", fmt.Errorf("playback: catalogue unavailable: %w", core.ErrNotFound)
	}
	t, err := m.deps.Catalog.Get(ctx, key)
	if err != nil {
		return "", err
	}
	if t == nil {
		return "", fmt.Errorf("playback: title %s: %w", key, core.ErrNotFound)
	}
	return titleLanguage(t), nil
}

// rememberedOriginalAudioCurrent reports whether a remembered release may
// open a new selection. Profiles that do not compare original-language
// audio ignore catalogue language, including a blank column left by
// migration. When they do, a blank language is the pre-migration row and
// is not reused. A known language has to be the one this selection sees,
// and the release has to still satisfy Require and Avoid. Prefer-only rows
// are dropped when that language changed so the new selection can rank
// again. An already open stream is not rewritten. The stored profile
// column stays Name|<16 hex>.
func rememberedOriginalAudioCurrent(profile core.QualityProfile, stored, current string, rel core.Release) bool {
	if !releasepolicy.UsesOriginalAudio(profile.ReleaseRules) {
		return true
	}
	current = strings.TrimSpace(current)
	stored = strings.TrimSpace(stored)
	if stored == "" || stored != current {
		return false
	}
	pol, err := releasepolicy.Compile(profile.ReleaseRules)
	if err != nil {
		return false
	}
	return !pol.Evaluate(rel, releasepolicy.Context{OriginalLanguage: current}).Rejected
}

func rememberedCodecCurrent(rel core.Release, caps core.ClientCaps) bool {
	if caps.Protocol == "" || caps.Protocol == "plex" || len(caps.VideoCodecs) == 0 {
		return true
	}
	if rel.Info.VideoCodec != "" && !caps.SupportsVideo(rel.Info.VideoCodec) {
		return false
	}
	return true
}

func rememberedInfoCodecCurrent(info core.MediaInfo, caps core.ClientCaps) bool {
	if caps.Protocol == "" || caps.Protocol == "plex" || len(caps.VideoCodecs) == 0 {
		return true
	}
	if v := info.Video(); v != nil && v.Codec != "" && !caps.SupportsVideo(v.Codec) {
		return false
	}
	return true
}

func permanentSourceFailure(err error) bool {
	return errors.Is(err, core.ErrArticleMissing) || errors.Is(err, core.ErrArticleCorrupt) || errors.Is(err, core.ErrReleaseRejected)
}
func recoverableSourceFailure(err error) bool {
	if permanentSourceFailure(err) || errors.Is(err, core.ErrSourceUnavailable) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.ErrNoProgress) {
		return true
	}
	var ne net.Error
	return errors.As(err, &ne) || errors.Is(err, context.DeadlineExceeded)
}
