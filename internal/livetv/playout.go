package livetv

import (
	"context"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/sessions"
)

const segmentDuration = 4 * time.Second

type channelLimiter interface {
	BeginChannel(*core.User, core.ClientCaps, string) error
	EndChannel(string)
}

type track struct {
	programme  Programme
	hls        core.HLSSession
	cancel     context.CancelFunc
	playbackID string
	offset     time.Duration
	sequence   int64
	count      int
}
type session struct {
	inputURL        string // selected upstream, immutable once the tune is ready
	inputSource     StreamSource
	inputHeaders    http.Header
	consumed        bool
	mediaStarted    bool
	startupCancel   context.CancelFunc // immutable after publication; cancels I/O before locking
	view            core.PlaySession   // immutable monitoring snapshot, including pending tunes
	reserved        bool
	ready           bool
	nextSequence    int64
	discontinuities int64
	relay           core.HLSSession
	relayCancel     context.CancelFunc
	relayInfo       *core.MediaInfo
	dash            core.DASHSession
	// mu guards the fields above and below it. It is never held across
	// Playback.Prepare/Failover, HLS SwapInput, programme lookups, probing
	// or upstream I/O: preparers work on a staged
	// snapshot (prepareMu serializes them) and publish under mu only after
	// validating generation. getSession/ServeSegment/close therefore stay
	// responsive while a tune or playlist preparation is in flight.
	mu sync.Mutex
	// generation is guarded by mu and bumped by close; a preparer that
	// snapshotted an older generation discards its staged result.
	generation int64
	// prepareMu serializes tune/playlist preparation for this session. It
	// is deliberately separate from mu so that slow preparation blocks
	// neither media serving nor close.
	prepareMu sync.Mutex
	// failoverGate serializes source changes without blocking close or readers.
	failoverGate chan struct{} // initialized under mu
	// ctx is the session lifetime, cancelled by close; preparation I/O is
	// bound to it (bindContext) so Stop cancels a slow Prepare/probe.
	ctx         context.Context
	cancel      context.CancelFunc
	info        SessionInfo
	streamGrant string
	user        core.User
	caps        core.ClientCaps
	last        time.Time
	tracks      []*track
	resources   map[string]string
	closed      bool
}

// bindContext derives a context cancelled when parent ends or when the
// session is closed, whichever comes first.
func (ss *session) bindContext(parent context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(parent)
	if ss.ctx == nil {
		return ctx, cancel
	}
	stop := context.AfterFunc(ss.ctx, cancel)
	return ctx, func() { stop(); cancel() }
}

// startOwnedHLS ties startup to the request and the returned encoder to the
// session. Once startup completes, request cancellation must not end playback.
func (s *Service) startOwnedHLS(request context.Context, ss *session, req core.HLSRequest) (core.HLSSession, context.CancelFunc, error) {
	base := ss.ctx
	if base == nil {
		base = s.ctx
	}
	if base == nil {
		base = context.Background()
	}
	lifetime, cancel := context.WithCancel(base)
	stop := context.AfterFunc(request, cancel)
	hls, err := s.o.Transcoder.StartHLS(lifetime, req)
	stop()
	if err == nil && request.Err() != nil {
		err = request.Err()
	}
	if err == nil && lifetime.Err() != nil {
		err = lifetime.Err()
	}
	if err != nil {
		cancel()
		if hls != nil {
			hls.Close()
		}
		return nil, nil, err
	}
	return hls, cancel, nil
}

func (ss *session) close(s *Service) {
	if ss.startupCancel != nil {
		ss.startupCancel()
	}
	if ss.cancel != nil {
		ss.cancel()
	}
	ss.mu.Lock()
	if ss.closed {
		ss.mu.Unlock()
		return
	}
	ss.closed = true
	ss.generation++
	tracks := ss.tracks
	relay := ss.relay
	relayCancel := ss.relayCancel
	dash := ss.dash
	reserved := ss.reserved
	ss.mu.Unlock()
	s.revokeInput(ss)
	if relayCancel != nil {
		relayCancel()
	}
	if relay != nil {
		relay.Close()
	}
	if dash != nil {
		dash.Close()
	}
	for _, t := range tracks {
		s.stopTrack(t)
	}
	if reserved {
		if limiter, ok := s.o.Playback.(channelLimiter); ok {
			limiter.EndChannel(ss.info.ID)
		}
	}
}

func (s *Service) Tune(ctx context.Context, u *core.User, channelID string, caps core.ClientCaps) (SessionInfo, error) {
	return s.TuneWithSession(ctx, u, channelID, caps, caps.ClientID)
}

// TuneWithSession preserves the client's playback-session identity independently
// of the server's stream, subscription and activity identities.
func (s *Service) TuneWithSession(ctx context.Context, u *core.User, channelID string, caps core.ClientCaps, clientSessionID string) (SessionInfo, error) {
	if clientSessionID == "" {
		clientSessionID = caps.ClientID
	}
	if u == nil || (s.o.CanPlay != nil && !s.o.CanPlay(u, caps.Remote, s.o.Now())) {
		return SessionInfo{}, core.ErrUnauthorized
	}
	c, err := s.Channel(ctx, channelID)
	if err != nil {
		return SessionInfo{}, err
	}
	ps, err := s.programmes(ctx, c, s.o.Now(), s.o.Now().Add(time.Second))
	if err != nil {
		return SessionInfo{}, err
	}
	var p *Programme
	if len(ps) > 0 {
		p = &ps[0]
	}
	if !s.allowed(ctx, u, c, p) {
		return SessionInfo{}, core.ErrUnauthorized
	}
	if c.Kind == "virtual" && p == nil {
		return SessionInfo{}, core.ErrNotAvailable
	}
	// A client can abandon a successful tune before requesting any media.
	// Reclaim only its unused session; other viewers/active players stay intact.
	if caps.ClientID != "" {
		var abandoned []*session
		s.mu.Lock()
		for id, old := range s.sessions {
			if !old.mu.TryLock() {
				continue
			}
			unused := old.ready && !old.consumed && old.info.UserID == u.ID && old.caps.ClientID == caps.ClientID
			old.mu.Unlock()
			if unused {
				delete(s.sessions, id)
				abandoned = append(abandoned, old)
			}
		}
		s.mu.Unlock()
		for _, old := range abandoned {
			old.close(s)
		}
	}
	var playbackSessions []*core.PlaySession
	if s.o.Playback != nil {
		playbackSessions = s.o.Playback.Sessions()
	}
	s.mu.Lock()
	count := 0
	for _, ss := range s.sessions {
		if ss.info.UserID == u.ID {
			count++
		}
	}
	limit := u.StreamLimit
	if s.o.StreamLimit != nil {
		limit = s.o.StreamLimit(u)
	}
	if s.o.Playback != nil {
		for _, ps := range playbackSessions {
			if ps.UserID == u.ID && ps.PlaybackGroup == "" {
				count++
			}
		}
	}
	if limit > 0 && count >= limit {
		s.mu.Unlock()
		return SessionInfo{}, core.ErrStreamLimit
	}
	s.nextSubscription++
	ss := &session{
		info: SessionInfo{
			ID: liveSessionID(), SubscriptionID: s.nextSubscription,
			ActivityID: liveSessionID(), ClientSessionID: clientSessionID,
			UserID: u.ID, Channel: c, Programme: p, StartedAt: s.o.Now(),
		},
		streamGrant: randomID(), user: *u, caps: caps, last: s.o.Now(), resources: map[string]string{},
	}
	base := s.ctx
	if base == nil {
		base = context.Background()
	}
	ss.ctx, ss.cancel = context.WithCancel(base)
	// Upstream retries and probing share one startup budget. A stalled public
	// source must not occupy a viewer slot for three HTTP timeouts.
	var cancel context.CancelFunc
	if c.Kind == "iptv" {
		ctx, cancel = context.WithTimeout(ctx, 10*time.Second)
	} else {
		ctx, cancel = context.WithCancel(ctx)
	}
	defer cancel()
	ss.startupCancel = cancel
	ss.view = core.PlaySession{ID: ss.info.ID, PlaybackGroup: ss.info.ID, UserID: u.ID, Username: u.Username, ClientID: caps.ClientID, Client: caps, TitleName: c.Name, StartedAt: ss.info.StartedAt, Decision: core.DecisionRemux, DecisionReason: "Live channel playout"}
	if p != nil {
		ss.view.Key = p.Item.Key
		ss.view.TitleName = p.Item.Title
		ss.view.Info.Duration = p.End.Sub(p.Start)
	}
	s.sessions[ss.info.ID] = ss
	s.mu.Unlock()
	if limiter, ok := s.o.Playback.(channelLimiter); ok {
		if err = limiter.BeginChannel(u, caps, ss.info.ID); err != nil {
			s.Stop(ctx, ss.info.ID, u)
			return SessionInfo{}, err
		}
		ss.mu.Lock()
		if ss.closed {
			ss.mu.Unlock()
			limiter.EndChannel(ss.info.ID)
			return SessionInfo{}, core.ErrNotFound
		}
		ss.reserved = true
		ss.mu.Unlock()
	}

	// Preparation does its I/O without ss.mu (see prepareTrack/startIPTVSession);
	// close can therefore cancel it through the session context instead of
	// waiting behind up to ~10s of upstream work.
	if c.Kind == "virtual" {
		_, err = s.prepareTrack(ctx, ss, *p)
		if err != nil {
			s.Stop(ctx, ss.info.ID, u)
			return SessionInfo{}, err
		}
	} else {
		err = s.startIPTVSession(ctx, ss)
		if err != nil {
			s.Stop(ctx, ss.info.ID, u)
			return SessionInfo{}, err
		}
	}
	ss.mu.Lock()
	if ss.closed {
		ss.mu.Unlock()
		return SessionInfo{}, core.ErrNotFound
	}
	ss.ready = true
	info := ss.info
	if ss.relayInfo != nil {
		info.MediaInfo = ss.relayInfo
	}
	ss.mu.Unlock()
	info.Channel.StreamURL = ""
	info.Channel.StreamSources = nil
	return info, nil
}
func (s *Service) getSession(ctx context.Context, id string, u *core.User) (*session, error) {
	s.mu.Lock()
	ss := s.sessions[id]
	s.mu.Unlock()
	if ss == nil {
		return nil, core.ErrNotFound
	}
	ctx, cancel := ss.bindContext(ctx)
	defer cancel()
	if u == nil || ss.info.UserID != u.ID || s.o.CanPlay != nil && !s.o.CanPlay(u, ss.caps.Remote, s.o.Now()) {
		return nil, core.ErrUnauthorized
	}
	c, err := s.Channel(ctx, ss.info.Channel.ID)
	if err != nil {
		return nil, err
	}
	ps, err := s.programmes(ctx, c, s.o.Now(), s.o.Now().Add(time.Second))
	if err != nil {
		return nil, err
	}
	var p *Programme
	if len(ps) > 0 {
		p = &ps[0]
	}
	if !s.allowed(ctx, u, c, p) {
		return nil, core.ErrUnauthorized
	}
	ss.mu.Lock()
	defer ss.mu.Unlock()
	if ss.closed {
		return nil, core.ErrNotFound
	}
	if !ss.ready {
		return nil, core.ErrNotAvailable
	}
	ss.user = *u
	return ss, nil
}

// Only media consumption renews the lease. Subscription/timeline polling and
// a repeatedly requested manifest can otherwise preserve a failed tune forever.
func (s *Service) touchMedia(ss *session) {
	ss.mu.Lock()
	ss.consumed = true
	ss.mediaStarted = true
	ss.last = s.o.Now()
	ss.mu.Unlock()
	s.recordSourceHealth(ss.info.Channel.ID, ss.inputSource, nil, true)
}
func (s *Service) Session(ctx context.Context, id string, u *core.User) (SessionInfo, error) {
	ss, err := s.getSession(ctx, id, u)
	if err != nil {
		return SessionInfo{}, err
	}
	// Snapshot under mu, then run the programme lookup (DB/upstream)
	// without the lock so getSession/ServeSegment/close stay responsive
	// while it is in flight.
	ss.mu.Lock()
	if ss.closed {
		ss.mu.Unlock()
		return SessionInfo{}, core.ErrNotFound
	}
	gen := ss.generation
	info := ss.info
	if ss.relayInfo != nil {
		info.MediaInfo = ss.relayInfo
	}
	channel := ss.info.Channel
	ss.mu.Unlock()
	info.Channel.StreamURL = ""
	info.Channel.StreamSources = nil
	lookupCtx, cancel := ss.bindContext(ctx)
	defer cancel()
	ps, err := s.programmes(lookupCtx, channel, s.o.Now(), s.o.Now().Add(time.Second))
	// Discard the snapshot if the session died while the lookup ran.
	ss.mu.Lock()
	stale := ss.closed || ss.generation != gen
	ss.mu.Unlock()
	if stale {
		return SessionInfo{}, core.ErrNotFound
	}
	if err == nil && len(ps) > 0 {
		info.Programme = &ps[0]
	}
	return info, err
}
func (s *Service) Sessions(ctx context.Context, u *core.User) []SessionInfo {
	s.mu.Lock()
	var ids []string
	for id := range s.sessions {
		ids = append(ids, id)
	}
	s.mu.Unlock()
	out := []SessionInfo{}
	for _, id := range ids {
		if info, err := s.Session(ctx, id, u); err == nil {
			out = append(out, info)
		}
	}
	return out
}
func (s *Service) Stop(ctx context.Context, id string, u *core.User) error {
	s.mu.Lock()
	ss := s.sessions[id]
	if ss == nil {
		s.mu.Unlock()
		return core.ErrNotFound
	}
	if u == nil || ss.info.UserID != u.ID && !u.IsOwner {
		s.mu.Unlock()
		return core.ErrUnauthorized
	}
	delete(s.sessions, id)
	s.mu.Unlock()
	ss.close(s)
	return nil
}
func trackCount(span time.Duration) int {
	n := int(span / segmentDuration)
	if rem := span % segmentDuration; n == 0 || rem >= segmentDuration/4 {
		n++
	}
	return n
}

// prepareTrack prepares one programme without holding ss.mu across
// Playback.Prepare/StartHLS. ss.prepareMu serializes preparers of this
// session; the I/O itself runs on a staged copy of the session and is
// published under ss.mu only if the session is unchanged (same generation,
// not closed, programme not already prepared). A stale staged track is
// stopped and discarded instead of leaking into a closed session.
func (s *Service) prepareTrack(ctx context.Context, ss *session, p Programme) (*track, error) {
	ss.prepareMu.Lock()
	defer ss.prepareMu.Unlock()
	return s.prepareTrackStaged(ctx, ss, p)
}

// prepareTrackStaged is prepareTrack for a caller already holding
// ss.prepareMu (ServePlaylist prepares up to three programmes as one unit).
func (s *Service) prepareTrackStaged(ctx context.Context, ss *session, p Programme) (*track, error) {
	ss.mu.Lock()
	if ss.closed {
		ss.mu.Unlock()
		return nil, core.ErrNotFound
	}
	for _, t := range ss.tracks {
		if t.programme.ID == p.ID {
			ss.mu.Unlock()
			return t, nil
		}
	}
	gen := ss.generation
	stage := &session{
		info: ss.info, user: ss.user, caps: ss.caps,
		tracks: append([]*track(nil), ss.tracks...), nextSequence: ss.nextSequence, ctx: ss.ctx,
	}
	stagedFrom := len(stage.tracks)
	ss.mu.Unlock()

	ctx, cancel := ss.bindContext(ctx)
	defer cancel()
	tr, err := s.prepareTrackOnStage(ctx, stage, p)
	if err != nil {
		return nil, err
	}
	ss.mu.Lock()
	if ss.closed || ss.generation != gen {
		ss.mu.Unlock()
		for _, stale := range stage.tracks[stagedFrom:] {
			s.stopTrack(stale)
		}
		return nil, core.ErrNotFound
	}
	for _, t := range ss.tracks {
		if t.programme.ID == p.ID {
			ss.mu.Unlock()
			for _, stale := range stage.tracks[stagedFrom:] {
				s.stopTrack(stale)
			}
			return t, nil
		}
	}
	ss.info.MediaInfo = stage.info.MediaInfo
	ss.nextSequence = stage.nextSequence
	ss.tracks = stage.tracks
	ss.mu.Unlock()
	return tr, nil
}

// startIPTVSession runs startIPTV on a staged copy of ss, so probing,
// upstream fetches and relay startup never hold ss.mu. On success the
// staged selection is published under ss.mu (including the input-relay
// grant, which is re-keyed from the stage to ss); if the session closed or
// changed generation meanwhile, the staged relay/grant are discarded.
func (s *Service) startIPTVSession(ctx context.Context, ss *session) error {
	ss.prepareMu.Lock()
	defer ss.prepareMu.Unlock()
	ss.mu.Lock()
	if ss.closed {
		ss.mu.Unlock()
		return core.ErrNotFound
	}
	gen := ss.generation
	stage := &session{info: ss.info, user: ss.user, caps: ss.caps, ctx: ss.ctx}
	ss.mu.Unlock()

	ctx, cancel := ss.bindContext(ctx)
	defer cancel()
	if err := s.startIPTV(ctx, stage); err != nil {
		return err
	}
	ss.mu.Lock()
	if ss.closed || ss.generation != gen {
		relay := stage.relay
		ss.mu.Unlock()
		if relay != nil {
			relay.Close()
		}
		if stage.relayCancel != nil {
			stage.relayCancel()
		}
		s.revokeInput(stage)
		return core.ErrNotFound
	}
	ss.inputURL, ss.inputSource, ss.inputHeaders = stage.inputURL, stage.inputSource, stage.inputHeaders
	ss.info.MediaInfo, ss.info.ActiveSourceID = stage.info.MediaInfo, stage.info.ActiveSourceID
	ss.relay, ss.relayInfo, ss.resources = stage.relay, stage.relayInfo, stage.resources
	ss.relayCancel = stage.relayCancel
	s.transferInput(stage, ss)
	ss.mu.Unlock()
	return nil
}

// prepareTrackOnStage performs the actual (slow) preparation against a
// staged session copy owned exclusively by the calling goroutine; it must
// only be called from prepareTrackStaged.
func (s *Service) prepareTrackOnStage(ctx context.Context, ss *session, p Programme) (*track, error) {
	if ss.closed {
		return nil, core.ErrNotFound
	}
	for _, t := range ss.tracks {
		if t.programme.ID == p.ID {
			return t, nil
		}
	}
	if s.o.Playback == nil || s.o.Transcoder == nil {
		return nil, core.ErrNotAvailable
	}
	if !s.allowed(ctx, &ss.user, ss.info.Channel, &p) {
		return nil, core.ErrUnauthorized
	}
	offset := time.Duration(0)
	if len(ss.tracks) == 0 {
		offset = (p.Offset(s.o.Now()) / segmentDuration) * segmentDuration
	}
	caps := ss.caps
	caps.ClientID = "livetv-" + ss.info.ID + "-" + strconv.FormatInt(p.Start.UnixMilli(), 10)
	var ps *core.PlaySession
	var err error
	if grouped, ok := s.o.Playback.(core.ChannelPreparer); ok {
		ps, err = grouped.PrepareChannel(ctx, &ss.user, p.Item.Key, caps, ss.info.ID)
	} else {
		ps, err = s.o.Playback.Prepare(ctx, &ss.user, p.Item.Key, caps)
	}
	if err != nil {
		return nil, err
	}
	info := ps.Info
	ss.info.MediaInfo = &info
	info.Duration = p.End.Sub(p.Start)
	decision := s.o.Transcoder.Decide(info, caps, core.DecideOptions{DirectStream: true, AudioStream: -1, SubtitleStream: -1})
	if decision.Kind == core.DecisionDirectPlay {
		decision.Kind = core.DecisionRemux
	}
	decision.Container = "mpegts"
	hs, lifetimeCancel, err := s.startOwnedHLS(ctx, ss, core.HLSRequest{SessionID: "livetv-" + ss.info.ID + "-" + strconv.FormatInt(p.Start.UnixMilli(), 10), InputURL: s.o.Playback.InputURL(ps.ID), Info: info, Decision: decision, StartOffset: offset, SegmentDuration: segmentDuration})
	if err != nil {
		s.o.Playback.Stop(ps.ID)
		return nil, err
	}
	tr := &track{programme: p, hls: hs, cancel: lifetimeCancel, playbackID: ps.ID, offset: offset, count: trackCount(info.Duration - offset)}
	tr.sequence = ss.nextSequence
	ss.nextSequence += int64(tr.count)
	s.transMu.Lock()
	s.transcodes[ps.ID] = hs
	s.transMu.Unlock()
	ss.tracks = append(ss.tracks, tr)
	return tr, nil
}
func writeStreamError(w http.ResponseWriter, err error) {
	code := http.StatusBadGateway
	switch err {
	case core.ErrNotFound:
		code = 404
	case core.ErrUnauthorized:
		code = 403
	case core.ErrStreamLimit:
		code = 429
	case core.ErrNotAvailable:
		code = 503
	}
	http.Error(w, http.StatusText(code), code)
}

// ServePlaylist creates a sliding live playlist. Each programme has its own
// transcoder, with a discontinuity at the exact programme boundary. Offsets are
// measured in programme time and never in release byte positions.
func (s *Service) ServePlaylist(w http.ResponseWriter, r *http.Request, id string, u *core.User) {
	ss, err := s.getSession(r.Context(), id, u)
	if err != nil {
		writeStreamError(w, err)
		return
	}
	ss.mu.Lock()
	ss.consumed = true
	relay, channel := ss.relay, ss.info.Channel
	ss.mu.Unlock()
	if relay != nil {
		s.serveRelayPlaylist(w, r, ss)
		return
	}
	if channel.Kind == "iptv" {
		s.ServeResource(w, r, id, "index.m3u8", u)
		return
	}
	// ss.mu is only taken for brief snapshots below; the (potentially ~10s)
	// programme preparation runs under prepareMu on staged state, so
	// ServeSegment/getSession/close of this same session stay responsive.
	ss.prepareMu.Lock()
	defer ss.prepareMu.Unlock()
	now := s.o.Now()
	ps, err := s.programmes(r.Context(), channel, now, now.Add(24*time.Second))
	if err != nil {
		writeStreamError(w, err)
		return
	}
	if len(ps) > 3 {
		ps = ps[:3]
	}
	// At most three source handles belong to one viewing session. Trim old
	// programme handles before reserving the next ones, retaining recent handles
	// when longer programmes leave room for lagging clients.
	desired := map[string]bool{}
	for _, p := range ps {
		desired[p.ID] = true
	}
	ss.mu.Lock()
	if ss.closed {
		ss.mu.Unlock()
		writeStreamError(w, core.ErrNotFound)
		return
	}
	needed := 0
	for _, p := range ps {
		exists := false
		for _, tr := range ss.tracks {
			if tr.programme.ID == p.ID {
				exists = true
			}
		}
		if !exists {
			needed++
		}
	}
	var removed []*track
	for len(ss.tracks) > 0 && (len(ss.tracks)+needed > 3 || ss.tracks[0].programme.End.Before(now.Add(-24*time.Second))) {
		old := ss.tracks[0]
		if desired[old.programme.ID] {
			break
		}
		ss.tracks = ss.tracks[1:]
		ss.discontinuities++
		removed = append(removed, old)
	}
	ss.mu.Unlock()
	for _, old := range removed {
		s.stopTrack(old)
	}
	for _, p := range ps {
		if _, err = s.prepareTrackStaged(r.Context(), ss, p); err != nil {
			if err == core.ErrUnauthorized && p.Start.After(now) {
				break
			}
			writeStreamError(w, err)
			return
		}
	}

	type seg struct {
		seq  int64
		at   time.Time
		dur  time.Duration
		uri  string
		disc bool
	}
	var segments []seg
	target := 4
	ss.mu.Lock()
	tracks := append([]*track(nil), ss.tracks...)
	discSequence := ss.discontinuities
	ss.mu.Unlock()
	for ti, tr := range tracks {
		for n := 0; n < tr.count; n++ {
			at := tr.programme.Start.Add(tr.offset + time.Duration(n)*segmentDuration)
			dur := segmentDuration
			if n == tr.count-1 {
				dur = tr.programme.End.Sub(at)
			}
			if at.Before(now.Add(-24*time.Second)) || at.After(now.Add(20*time.Second)) {
				continue
			}
			if int(math.Ceil(dur.Seconds())) > target {
				target = int(math.Ceil(dur.Seconds()))
			}
			uri := fmt.Sprintf("program/%d/%d.ts", tr.programme.Start.UnixMilli(), n)
			uri = withAuth(uri, ss.streamGrant)
			segments = append(segments, seg{tr.sequence + int64(n), at, dur, uri, ti > 0 && n == 0})
		}
		if tr.programme.End.Before(now.Add(-24 * time.Second)) {
			discSequence++
		}
	}
	if len(segments) == 0 {
		writeStreamError(w, core.ErrNotAvailable)
		return
	}
	var b strings.Builder
	fmt.Fprintf(&b, "#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:%d\n#EXT-X-MEDIA-SEQUENCE:%d\n#EXT-X-DISCONTINUITY-SEQUENCE:%d\n", target, segments[0].seq, discSequence)
	for _, seg := range segments {
		if seg.disc {
			b.WriteString("#EXT-X-DISCONTINUITY\n")
		}
		fmt.Fprintf(&b, "#EXT-X-PROGRAM-DATE-TIME:%s\n#EXTINF:%.6f,\n%s\n", seg.at.UTC().Format(time.RFC3339Nano), seg.dur.Seconds(), seg.uri)
	}
	w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != "HEAD" {
		fmt.Fprint(w, b.String())
	}
}
func withAuth(raw, grant string) string {
	return sessions.GrantURL(raw, grant)
}
func (s *Service) ServeSegment(w http.ResponseWriter, r *http.Request, id string, start int64, n int, u *core.User) {
	ss, err := s.getSession(r.Context(), id, u)
	if err != nil {
		writeStreamError(w, err)
		return
	}
	ss.mu.Lock()
	var tr *track
	for _, t := range ss.tracks {
		if t.programme.Start.UnixMilli() == start {
			tr = t
			break
		}
	}
	if tr == nil || n < 0 || n >= tr.count {
		ss.mu.Unlock()
		writeStreamError(w, core.ErrNotFound)
		return
	}
	if !s.allowed(r.Context(), u, ss.info.Channel, &tr.programme) {
		ss.mu.Unlock()
		writeStreamError(w, core.ErrUnauthorized)
		return
	}
	hs := tr.hls
	ss.mu.Unlock()
	s.touchMedia(ss)
	hs.ServeSegment(w, r, n)
}

// Failover keeps the channel timestamp when Playback resolves another release.
//
// Playback.Failover and HLSSession.SwapInput do I/O, so they run without
// ss.mu (getSession/ServeSegment/close stay responsive): the current track
// is snapshotted under mu and the swap is only kept if, once it finishes,
// the session is still alive and still carries that same track. A stale
// swap is discarded — the replacement playback session (when it is a new
// one) is stopped and the swapped HLS is closed — and reported as
// ErrNotFound, matching a session/track that is already gone.
func (s *Service) Failover(ctx context.Context, id string, u *core.User) error {
	ss, err := s.getSession(ctx, id, u)
	if err != nil {
		return err
	}
	ctx, cancel := ss.bindContext(ctx)
	defer cancel()
	ss.mu.Lock()
	if ss.failoverGate == nil {
		ss.failoverGate = make(chan struct{}, 1)
	}
	gate := ss.failoverGate
	ss.mu.Unlock()
	select {
	case gate <- struct{}{}:
		defer func() { <-gate }()
	case <-ctx.Done():
		return ctx.Err()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	now := s.o.Now()
	ss.mu.Lock()
	if ss.closed {
		ss.mu.Unlock()
		return core.ErrNotFound
	}
	gen := ss.generation
	var current *track
	for _, tr := range ss.tracks {
		if !now.Before(tr.programme.Start) && now.Before(tr.programme.End) {
			current = tr
			break
		}
	}
	if current == nil {
		ss.mu.Unlock()
		return core.ErrNotAvailable
	}
	hls, playbackID := current.hls, current.playbackID
	at := current.programme.Offset(now)
	ss.mu.Unlock()

	stillCurrent := func() bool {
		ss.mu.Lock()
		defer ss.mu.Unlock()
		if ss.closed || ss.generation != gen {
			return false
		}
		for _, tr := range ss.tracks {
			if tr == current {
				return true
			}
		}
		return false
	}
	discard := func(ps *core.PlaySession, swapped bool) {
		if ps != nil && ps.ID != playbackID && s.o.Playback != nil {
			s.o.Playback.Stop(ps.ID)
		}
		if swapped && hls != nil {
			hls.Close()
		}
	}

	ps, err := s.o.Playback.Failover(ctx, playbackID, at)
	if err != nil {
		return err
	}
	if ctx.Err() != nil || !stillCurrent() {
		discard(ps, false)
		return core.ErrNotFound
	}
	swapErr := hls.SwapInput(ctx, s.o.Playback.InputURL(ps.ID), ps.Info, at)
	if !stillCurrent() {
		discard(ps, swapErr == nil)
		return core.ErrNotFound
	}
	return swapErr
}

// startIPTV recognizes HLS without assuming URL suffixes (many owners use
// signed extensionless URLs). Other HTTP streams are remuxed through ffmpeg,
// whose existing input layer reconnects on network errors with bounded delay.
func (s *Service) startIPTVSource(ctx context.Context, ss *session) error {
	if ss.closed {
		return core.ErrNotFound
	}
	c := ss.info.Channel
	c.StreamURL = ss.inputURL
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, c.StreamURL, nil)
	res, err := s.upstreamWithHeaders(ctx, c.StreamURL, req, ss.inputHeaders)
	if err != nil {
		return err
	}
	prefix := make([]byte, 7)
	n, readErr := io.ReadFull(res.Body, prefix)
	res.Body.Close()
	if res.StatusCode != 200 {
		return core.ErrNotAvailable
	}
	if readErr != nil && readErr != io.EOF && readErr != io.ErrUnexpectedEOF {
		return readErr
	}
	isHLS := strings.HasPrefix(strings.TrimSpace(string(prefix[:n])), "#EXTM3U") || strings.Contains(strings.ToLower(res.Header.Get("Content-Type")), "mpegurl")
	var info *core.MediaInfo
	if isHLS && !c.Transcode {
		analyze := len(ss.caps.VideoCodecs) > 0 || len(ss.caps.AudioCodecs) > 0 || strings.Contains(strings.ToLower(ss.caps.Product), "apple") || strings.EqualFold(ss.caps.Platform, "tvos") || strings.EqualFold(ss.caps.Platform, "ios")
		if s.o.Transcoder != nil && analyze {
			info, err = s.probeSessionInput(ctx, ss)
			if err != nil {
				return err
			}
			if info == nil || info.Video() == nil {
				return core.ErrNotAvailable
			}
			copy := *info
			copy.Duration = 0
			info = &copy
			ss.info.MediaInfo = info
			// A playlist is a transport, not evidence that its codecs are
			// playable. MPEG-2/MP2 HLS needs conversion on Apple TV too.
			dec := s.o.Transcoder.Decide(*info, ss.caps, core.DecideOptions{DirectPlay: true, DirectStream: true, AudioStream: -1, SubtitleStream: -1})
			if !dec.VideoCopy || !dec.AudioCopy {
				return s.startIPTVRelay(ctx, ss, *info)
			}
		}
		ss.resources["index.m3u8"] = c.StreamURL
		return nil
	}
	if s.o.Transcoder == nil {
		return core.ErrNotAvailable
	}
	info, err = s.probeSessionInput(ctx, ss)
	if err != nil {
		return err
	}
	if info == nil {
		return core.ErrNotAvailable
	}
	copy := *info
	copy.Duration = 0
	ss.info.MediaInfo = &copy
	return s.startIPTVRelay(ctx, ss, copy)
}
func (s *Service) serveRelayPlaylist(w http.ResponseWriter, r *http.Request, ss *session) {
	ss.mu.Lock()
	relay := ss.relay
	ss.mu.Unlock()
	rec := httptest.NewRecorder()
	relay.ServePlaylist(rec, r)
	if rec.Code != 200 {
		http.Error(w, "IPTV playout unavailable", rec.Code)
		return
	}
	// An unknown-duration transcoder initially has no completed segments.
	// Native players can reject that empty EVENT playlist immediately. Use
	// the session's bounded segment wait before returning the first playlist.
	if !relayPlaylistHasMedia(rec.Body.String()) {
		ready := &relayReadinessWriter{header: make(http.Header)}
		relay.ServeSegment(ready, r, 0)
		if ready.status >= 400 {
			http.Error(w, "IPTV playout unavailable", ready.status)
			return
		}
		rec = httptest.NewRecorder()
		relay.ServePlaylist(rec, r)
		if rec.Code != 200 || !relayPlaylistHasMedia(rec.Body.String()) {
			http.Error(w, "IPTV playout unavailable", http.StatusBadGateway)
			return
		}
	}
	lines := strings.Split(rec.Body.String(), "\n")
	for i, line := range lines {
		if line != "" && !strings.HasPrefix(line, "#") {
			base := path.Base(strings.SplitN(line, "?", 2)[0])
			n, err := strconv.Atoi(strings.TrimSuffix(base, ".ts"))
			if err != nil {
				http.Error(w, "invalid relay segment", 502)
				return
			}
			lines[i] = withAuth(fmt.Sprintf("/livetv/sessions/%s/0/relay/%d.ts", ss.info.ID, n), ss.streamGrant)
		}
	}
	w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != "HEAD" {
		fmt.Fprint(w, strings.Join(lines, "\n"))
	}
}
func (s *Service) ServeRelaySegment(w http.ResponseWriter, r *http.Request, id string, n int, u *core.User) {
	ss, err := s.getSession(r.Context(), id, u)
	if err != nil {
		writeStreamError(w, err)
		return
	}
	ss.mu.Lock()
	relay := ss.relay
	ss.mu.Unlock()
	if relay == nil || n < 0 {
		writeStreamError(w, core.ErrNotFound)
		return
	}
	s.touchMedia(ss)
	relay.ServeSegment(w, r, n)
}

// TranscodeFor lets the playback manager's existing release failover locate
// programme sessions, preserving the same media timestamp after a source swap.
func (s *Service) TranscodeFor(playbackID string) (core.HLSSession, bool) {
	s.transMu.RLock()
	defer s.transMu.RUnlock()
	hs, ok := s.transcodes[playbackID]
	return hs, ok
}

// ViewingSessions reports one trusted reservation unit per live viewer. It is
// used by ordinary playback's limit accounting, including HLS passthrough
// viewers that do not own an ffmpeg/source session.
func (s *Service) ViewingSessions() []*core.PlaySession {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*core.PlaySession, 0, len(s.sessions))
	for _, ss := range s.sessions {
		p := ss.view
		out = append(out, &p)
	}
	return out
}
