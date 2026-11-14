package livetv

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/streamgrant"
)

const segmentDuration = 4 * time.Second

type channelLimiter interface {
	BeginChannel(*core.User, core.ClientCaps, string) error
	EndChannel(string)
}

type session struct {
	broadcastPolicy     broadcastViewerPolicy
	broadcast           *broadcastLease
	inputURL            string // selected upstream, immutable once the tune is ready
	inputSource         StreamSource
	inputHeaders        http.Header
	firstMediaDelivered bool
	lastMediaWarning    time.Time
	consumed            bool
	mediaStarted        bool
	startupCancel       context.CancelFunc // immutable after publication; cancels I/O before locking
	view                core.PlaySession   // immutable monitoring snapshot, including pending tunes
	reserved            bool
	ready               bool
	relayInfo           *core.MediaInfo
	steadyRefused       string // why Steady refused this tune; Direct must not escalate back into it
	// owned is the Ether-owned client timeline of a Direct (passthrough or
	// relay) session; ServePlaylist and ServeRelaySegment read it.
	owned *ownedLease
	dash  core.DASHSession
	http  core.HTTPSession
	// httpStart is closed when an in-flight ensureHTTP StartHLS finishes.
	// Waiters re-check liveHTTP instead of starting a second encoder.
	httpStart chan struct{}
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
	// ctx is the session lifetime, cancelled by close; preparation I/O is
	// bound to it (bindContext) so Stop cancels a slow Prepare/probe.
	ctx    context.Context
	cancel context.CancelFunc
	// bound holds the cancels of live bindContext contexts. close cancels
	// them synchronously, so every bound context (a Playback.Failover or
	// Prepare in flight, including its internal retries) is cancelled
	// before Stop returns; context.AfterFunc alone runs in a new goroutine
	// and can lag.
	boundMu     sync.Mutex
	bound       map[uint64]context.CancelFunc
	boundSeq    uint64
	info        SessionInfo
	streamGrant string
	user        core.User
	caps        core.ClientCaps
	last        time.Time
	closed      bool
}

// bindContext derives a context cancelled when parent ends or when the
// session is closed, whichever comes first. A context bound after close
// began is returned already cancelled.
func (ss *session) bindContext(parent context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(parent)
	if ss.ctx == nil {
		return ctx, cancel
	}
	ss.boundMu.Lock()
	if ss.ctx.Err() != nil {
		ss.boundMu.Unlock()
		cancel()
		return ctx, cancel
	}
	if ss.bound == nil {
		ss.bound = map[uint64]context.CancelFunc{}
	}
	ss.boundSeq++
	id := ss.boundSeq
	ss.bound[id] = cancel
	ss.boundMu.Unlock()
	// Service shutdown cancels ss.ctx without close; that path may lag.
	stop := context.AfterFunc(ss.ctx, cancel)
	return ctx, func() {
		stop()
		cancel()
		ss.boundMu.Lock()
		delete(ss.bound, id)
		ss.boundMu.Unlock()
	}
}

// live reports why I/O for ctx must not start: ctx itself ended, or close
// has begun (it cancels ss.ctx before anything else, so any effect of a
// Stop — such as a failover gate it freed — is ordered after this check).
func (ss *session) live(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if ss.ctx != nil && ss.ctx.Err() != nil {
		return core.ErrNotFound
	}
	return nil
}

// cancelBound cancels every bound context; ss.ctx must already be
// cancelled so bindContext cannot register another one afterwards.
func (ss *session) cancelBound() {
	ss.boundMu.Lock()
	bound := ss.bound
	ss.bound = nil
	ss.boundMu.Unlock()
	for _, cancel := range bound {
		cancel()
	}
}

// startOwnedHLS ties startup to the request and the returned encoder to the
// session. Once startup completes, request cancellation must not end playback.
func (s *Service) startOwnedHLS(request context.Context, ss *session, req core.HLSRequest) (core.HLSSession, context.CancelFunc, error) {
	base := ss.ctx
	if base == nil {
		base = s.serviceContext()
	}
	if base == nil {
		base = context.Background()
	}
	lifetime, cancel := context.WithCancel(base)
	stop := context.AfterFunc(request, cancel)
	hls, err := s.transcoder().StartHLS(lifetime, req)
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
	defer s.onChannelSessionClosed(ss.info.Channel.ID)
	defer s.endPersonal(ss.info.Channel, ss.info.UserID)
	if ss.startupCancel != nil {
		ss.startupCancel()
	}
	if ss.cancel != nil {
		ss.cancel()
		ss.cancelBound()
	}
	ss.mu.Lock()
	if ss.closed {
		ss.mu.Unlock()
		return
	}
	ss.closed = true
	ss.generation++
	broadcast := ss.broadcast
	ss.broadcast = nil
	owned := ss.owned
	ss.owned = nil
	dash := ss.dash
	httpStream := ss.http
	ss.dash = nil
	ss.http = nil
	reserved := ss.reserved
	ready, requested, delivered := ss.ready, ss.consumed, ss.firstMediaDelivered
	ss.mu.Unlock()
	if broadcast != nil {
		broadcast.close()
	}
	s.revokeInput(ss)
	owned.close()
	if dash != nil {
		dash.Close()
	}
	if httpStream != nil {
		httpStream.Close()
	}
	if reserved {
		if limiter, ok := s.o.Playback.(channelLimiter); ok {
			limiter.EndChannel(ss.info.ID)
		}
	}
	s.sessionLog(ss).Info("live session resources released", "ready", ready, "media_requested", requested, "media_delivered", delivered, "age", s.o.Now().Sub(ss.info.StartedAt), "viewer_reservation_released", reserved)
}

func (s *Service) Tune(ctx context.Context, u *core.User, channelID string, caps core.ClientCaps) (SessionInfo, error) {
	return s.TuneWithSession(ctx, u, channelID, caps, caps.ClientID)
}

// TuneWithSession preserves the client's playback-session identity independently
// of the server's stream, subscription and activity identities.
func (s *Service) TuneWithSession(ctx context.Context, u *core.User, channelID string, caps core.ClientCaps, clientSessionID string) (result SessionInfo, resultErr error) {
	if !s.Enabled() {
		return SessionInfo{}, core.ErrNotAvailable
	}
	started := time.Now()
	attemptID := liveSessionID()
	log := s.logger().With("session", attemptID, "channel", channelID, "client", caps.ClientID, "product", caps.Product)
	log.Info("live tune started")
	defer func() {
		attrs := []any{"elapsed", time.Since(started)}
		if resultErr != nil {
			detail := Diagnose("tune", resultErr)
			attrs = append(attrs, detail.LogAttrs()...)
			if detail.Code == "cancelled" {
				log.Debug("live tune cancelled", attrs...)
			} else {
				log.Warn("live tune failed", attrs...)
			}
		} else {
			log.Info("live tune ready", append(attrs, "source", result.Channel.SourceID, "analyzed", result.MediaInfo != nil)...)
		}
	}()

	if clientSessionID == "" {
		clientSessionID = caps.ClientID
	}
	if u == nil || (s.o.CanPlay != nil && !s.o.CanPlay(u, caps.Remote, s.o.Now())) {
		return SessionInfo{}, core.ErrUnauthorized
	}
	ctx, policy, err := s.withPolicy(ctx, u)
	if err != nil {
		return SessionInfo{}, err
	}
	c, err := s.Channel(ctx, channelID)
	if err != nil {
		return SessionInfo{}, err
	}
	c = s.personalChannel(ctx, c, u)
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
	// Apply the viewer override only to this tune's channel copy. A channel
	// already requiring conversion remains converted for every viewer.
	if c.Kind == "iptv" && policy.Transcode {
		c.Transcode = true
	}
	if c.Kind == "virtual" && p == nil {
		return SessionInfo{}, core.ErrNotAvailable
	}
	// A single client device watches one live channel at a time. When it
	// retunes without explicitly stopping the previous channel (common on
	// Fire TV / Android TV), supersede its prior live session immediately
	// while leaving other devices and viewers intact.
	if caps.ClientID != "" {
		var superseded []*session
		s.mu.Lock()
		for id, old := range s.sessions {
			if old.info.UserID == u.ID && old.caps.ClientID == caps.ClientID {
				delete(s.sessions, id)
				superseded = append(superseded, old)
			}
		}
		s.mu.Unlock()
		for _, old := range superseded {
			s.sessionLog(old).Info("live session superseded", "replacement_session", attemptID)
			old.close(s)
		}
		if len(superseded) > 0 {
			s.notifySessionsChanged()
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
	// A provider that allows N connections can carry N different channels;
	// viewers of a channel already playing share its connection.
	if c.Kind == "iptv" && c.SourceID != "" {
		if max := s.sourceConnectionLimit(c.SourceID); max > 0 {
			playing := map[string]bool{}
			for _, other := range s.sessions {
				if other.info.Channel.SourceID == c.SourceID {
					playing[other.info.Channel.ID] = true
				}
			}
			if !playing[c.ID] && len(playing)+s.inputRelay.taps.externalCount(c.SourceID) >= max {
				s.mu.Unlock()
				return SessionInfo{}, ErrProviderBusy
			}
		}
	}
	s.nextSubscription++
	ss := &session{
		info: SessionInfo{
			ID: attemptID, SubscriptionID: s.nextSubscription,
			ActivityID: liveSessionID(), ClientSessionID: clientSessionID,
			UserID: u.ID, Channel: c, Programme: p, StartedAt: s.o.Now(),
		},
		streamGrant: randomID(), user: *u, caps: caps, last: s.o.Now(),
	}
	base := s.serviceContext()
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
	ss.view.LiveChannel = &core.LiveChannel{ID: c.ID, Name: c.Name, Number: c.Number}
	if p != nil {
		ss.view.Key = p.Item.Key
		ss.view.TitleName = p.Item.Title
		ss.view.Info.Duration = p.End.Sub(p.Start)
	}
	s.sessions[ss.info.ID] = ss
	s.noteWatchedLocked(c.ID)
	s.mu.Unlock()
	s.notifySessionsChanged()
	s.beginPersonal(c, u)
	if limiter, ok := s.o.Playback.(channelLimiter); ok {
		if joined, ok := s.o.Playback.(interface {
			JoinChannel(*core.User, core.ClientCaps, string, string) error
		}); ok && s.useBroadcast(c) {
			group := "broadcast-" + c.ID
			if c.Personal != nil {
				group += "-viewer-" + strconv.FormatInt(u.ID, 10)
			}
			err = joined.JoinChannel(u, caps, ss.info.ID, group)
			if errors.Is(err, core.ErrStreamLimit) && s.broadcasts.yieldIdle(nil) {
				deadline := time.NewTimer(2 * time.Second)
				tick := time.NewTicker(25 * time.Millisecond)
				defer deadline.Stop()
				defer tick.Stop()
			retryJoin:
				for {
					select {
					case <-ctx.Done():
						break retryJoin
					case <-deadline.C:
						break retryJoin
					case <-tick.C:
						err = joined.JoinChannel(u, caps, ss.info.ID, group)
						if !errors.Is(err, core.ErrStreamLimit) {
							break retryJoin
						}
					}
				}
			}
		} else {
			err = limiter.BeginChannel(u, caps, ss.info.ID)
		}
		if err != nil {
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

	// Input and output startup perform their I/O without ss.mu;
	// close can therefore cancel it through the session context instead of
	// waiting behind up to ~10s of upstream work.
	if s.useBroadcast(c) {
		err = s.startBroadcast(ctx, ss)
		if errors.Is(err, errSteadyRefused) {
			// A refused tune is an ordinary direct session: it holds its own
			// viewer reservation instead of a share of the channel output.
			err = s.reserveDirectViewer(u, caps, ss)
			if err == nil {
				err = s.startIPTVSession(ctx, ss)
			}
			if err == nil {
				err = s.awaitOwnedStart(ctx, ss)
			}
		}
		if err != nil {
			s.Stop(ctx, ss.info.ID, u)
			return SessionInfo{}, err
		}
	} else {
		err = s.startIPTVSession(ctx, ss)
		if err == nil {
			err = s.awaitOwnedStart(ctx, ss)
		}
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
	ss.mu.Lock()
	shared := ss.broadcast != nil
	ss.mu.Unlock()
	if shared {
		ctx, err := s.broadcastPolicyContext(ctx, ss, u)
		if err != nil {
			return nil, err
		}
		if !s.broadcastTagsAllowed(ctx, ss, nil) {
			return nil, core.ErrUnauthorized
		}
	} else {
		c, err := s.Channel(ctx, ss.info.Channel.ID)
		if err != nil {
			return nil, err
		}
		c = s.personalChannel(ctx, c, u)
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
	first := !ss.consumed
	ss.consumed = true
	ss.mediaStarted = true
	ss.last = s.o.Now()
	// A shared passthrough timeline may have left the source this tune
	// selected; its producer records the health of what actually plays.
	shared := ss.owned != nil && ss.owned.relay == nil
	src := ss.inputSource
	ss.mu.Unlock()
	if first {
		s.sessionLog(ss).Info("live media first requested", "since_tune", s.o.Now().Sub(ss.info.StartedAt))
	}
	if !shared {
		s.recordSourceHealth(ss.info.Channel.ID, src, nil, true)
	}
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
	if lease := ss.owned; lease != nil && lease.source != nil {
		if id := lease.source(); id != "" {
			info.ActiveSourceID = id
		}
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
	s.notifySessionsChanged()
	return nil
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
	stage := &session{info: ss.info, user: ss.user, caps: ss.caps, ctx: ss.ctx, steadyRefused: ss.steadyRefused}
	ss.mu.Unlock()

	ctx, cancel := ss.bindContext(ctx)
	defer cancel()
	if err := ss.live(ctx); err != nil {
		return err
	}
	if err := s.startIPTV(ctx, stage); err != nil {
		return err
	}
	ss.mu.Lock()
	if ss.closed || ss.generation != gen {
		if stage.broadcast != nil {
			stage.broadcast.close()
		}
		ss.mu.Unlock()
		stage.owned.close()
		s.revokeInput(stage)
		return core.ErrNotFound
	}
	ss.inputURL, ss.inputSource, ss.inputHeaders = stage.inputURL, stage.inputSource, stage.inputHeaders
	ss.info.MediaInfo, ss.info.ActiveSourceID = stage.info.MediaInfo, stage.info.ActiveSourceID
	ss.relayInfo, ss.owned = stage.relayInfo, stage.owned
	ss.broadcast = stage.broadcast
	s.transferInput(stage, ss)
	if ss.owned != nil && ss.owned.relay != nil {
		ss.owned.relay.bind(ss)
	}
	ss.mu.Unlock()
	return nil
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
		w.Header().Set("Retry-After", "1")
		code = 503
	}
	http.Error(w, http.StatusText(code), code)
}

// ServePlaylist writes the shared broadcast view or the owner's unchanged
// IPTV passthrough/relay view when continuous output is disabled.
func (s *Service) ServePlaylist(w http.ResponseWriter, r *http.Request, id string, u *core.User) {
	ss, err := s.getSession(r.Context(), id, u)
	if err != nil {
		writeStreamError(w, err)
		return
	}
	ss.mu.Lock()
	ss.consumed = true
	shared := ss.broadcast != nil
	ss.mu.Unlock()
	if shared {
		s.serveBroadcastPlaylist(w, r, ss)
		return
	}
	s.serveOwnedPlaylist(w, r, ss)
}
func withAuth(raw, grant string) string { return streamgrant.GrantURL(raw, grant) }

// Failover is owned by the shared input planner, which retries without changing
// the viewer's output clock.
func (s *Service) Failover(ctx context.Context, id string, u *core.User) error {
	ss, err := s.getSession(ctx, id, u)
	if err != nil {
		return err
	}
	ss.mu.Lock()
	lease := ss.broadcast
	ss.mu.Unlock()
	if lease == nil {
		return core.ErrNotAvailable
	}
	lease.broadcast.requestRetry()
	return nil
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
	analyze := len(ss.caps.VideoCodecs) > 0 || len(ss.caps.AudioCodecs) > 0 || ss.caps.ProbeLiveWithoutCodecs
	probe := analyze || c.Transcode || ss.caps.ProbeLiveInput
	// Codec analysis overlaps the playlist prefix read. The first tune of a
	// channel still waits on the probe; a retune inside liveProbeTTL does not.
	probeCh, probeCancel := s.startEarlyProbe(ctx, ss, probe)
	if probeCancel != nil {
		defer probeCancel()
	}
	// The sniff joins the input's provider connection, which the probe and
	// ffmpeg then read from too.
	prefix, ctype, err := s.sniffInput(ctx, ss, 7)
	if err != nil {
		return Diagnose("input_sniff", err)
	}
	isHLS := strings.HasPrefix(strings.TrimSpace(string(prefix)), "#EXTM3U") || strings.Contains(strings.ToLower(ctype), "mpegurl")
	var info *core.MediaInfo
	if isHLS && !c.Transcode {
		if s.transcoder() != nil && probe {
			probeWaitCtx := ctx
			var waitCancel context.CancelFunc
			if ss.caps.BestEffortLiveStart {
				// This caller has a < 1s startup budget. Wait briefly for a
				// cached or fast probe, then proceed with HLS passthrough if
				// live ffprobe is still sampling segments.
				probeWaitCtx, waitCancel = context.WithTimeout(ctx, 350*time.Millisecond)
				defer waitCancel()
			}
			info, err = s.takeEarlyProbe(probeWaitCtx, ss, probeCh)
			if analyze && !ss.caps.BestEffortLiveStart {
				if err != nil {
					return Diagnose("probe", err)
				}
				if info == nil || info.Video() == nil {
					return &TuneError{Code: "no_video", Stage: "probe", cause: core.ErrNotAvailable}
				}
			}
			if err == nil && info != nil && info.Video() != nil {
				copy := *info
				copy.Duration = 0
				info = &copy
				ss.info.MediaInfo = info
				if analyze {
					// A playlist is a transport, not evidence that its codecs are
					// playable. MPEG-2/MP2 HLS needs conversion on Apple TV too.
					dec := s.transcoder().Decide(*info, ss.caps, core.DecideOptions{DirectPlay: true, DirectStream: true, AudioStream: -1, SubtitleStream: -1})
					if !dec.VideoCopy || !dec.AudioCopy || info.Video().Codec == "hevc" {
						return s.startIPTVRelay(ctx, ss, *info)
					}
				}
			}
		}
		ss.owned = s.acquirePassthrough(ss)
		s.sessionLog(ss).Info("live playback path selected", "path", "hls_passthrough", "analyzed", ss.info.MediaInfo != nil)
		return nil
	}
	if s.transcoder() == nil {
		return core.ErrNotAvailable
	}
	info, err = s.takeEarlyProbe(ctx, ss, probeCh)
	if err != nil {
		return Diagnose("probe", err)
	}
	if info == nil {
		return &TuneError{Code: "no_video", Stage: "probe", cause: core.ErrNotAvailable}
	}
	copy := *info
	copy.Duration = 0
	ss.info.MediaInfo = &copy
	return s.startIPTVRelay(ctx, ss, copy)
}

type probeResult struct {
	info *core.MediaInfo
	err  error
}

// startEarlyProbe begins codec analysis before the prefix read when this
// tune will need it. Direct HLS without a codec decision does not probe.
func (s *Service) startEarlyProbe(ctx context.Context, ss *session, need bool) (<-chan probeResult, context.CancelFunc) {
	if !need || s.transcoder() == nil {
		return nil, nil
	}
	ch := make(chan probeResult, 1)
	probeCtx, cancel := context.WithCancel(ctx)
	go func() {
		info, err := s.probeSessionInput(probeCtx, ss)
		ch <- probeResult{info, err}
	}()
	return ch, cancel
}

func (s *Service) takeEarlyProbe(ctx context.Context, ss *session, ch <-chan probeResult) (*core.MediaInfo, error) {
	if ch == nil {
		return s.probeSessionInput(ctx, ss)
	}
	select {
	case r := <-ch:
		return r.info, r.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// awaitOwnedStart holds the tune until a Direct session's owned timeline has
// its first playlist. An Apple player hangs a live item for good when the
// first playlist response takes a target duration, so the wait for the
// first segments belongs to the tune, never to that response. Silo's
// playback start keeps its sub-second budget: it waits at most 700 ms
// (passthrough is usually ready by then) and leaves the rest to the
// playlist request.
func (s *Service) awaitOwnedStart(ctx context.Context, ss *session) error {
	ss.mu.Lock()
	lease := ss.owned
	ss.mu.Unlock()
	if lease == nil {
		return nil
	}
	wait := ctx
	if ss.caps.BestEffortLiveStart {
		var cancel context.CancelFunc
		wait, cancel = context.WithTimeout(ctx, 700*time.Millisecond)
		defer cancel()
	}
	err := lease.tl.waitStarted(wait)
	if errors.Is(err, errNeedsRelay) {
		if lease, err = s.passthroughToRelay(ctx, ss); err == nil {
			err = lease.tl.waitStarted(wait)
		}
	}
	if err != nil && wait.Err() != nil && ctx.Err() == nil {
		return nil // The playlist request waits for the rest
	}
	if errors.Is(err, context.DeadlineExceeded) {
		// The tune budget passed without one segment from the source.
		return &TuneError{Code: "timeout", Stage: "owned_start", cause: err}
	}
	return err
}

// serveOwnedPlaylist writes a Direct session's owned live playlist. The
// first request waits for the first segments (bounded), as players reject an
// empty live playlist.
func (s *Service) serveOwnedPlaylist(w http.ResponseWriter, r *http.Request, ss *session) {
	ss.mu.Lock()
	lease := ss.owned
	ss.mu.Unlock()
	if lease == nil {
		writeStreamError(w, core.ErrNotFound)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	err := lease.tl.waitStarted(ctx)
	if errors.Is(err, errNeedsRelay) {
		lease, err = s.passthroughToRelay(ctx, ss)
		if err == nil {
			err = lease.tl.waitStarted(ctx)
		}
	}
	if err != nil {
		if r.Context().Err() == nil {
			s.sessionLog(ss).Warn("owned live playlist unavailable", Diagnose("playlist", err).LogAttrs()...)
		}
		http.Error(w, "IPTV playout unavailable", http.StatusBadGateway)
		return
	}
	prefix := "/livetv/sessions/" + ss.info.ID + "/0/"
	body := lease.tl.render(func(seq int64, ext string) string {
		return withAuth(prefix+"relay/"+strconv.FormatInt(seq, 10)+"."+ext, ss.streamGrant)
	}, func(name string) string {
		return withAuth(prefix+"resource/"+name, ss.streamGrant)
	})
	w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodHead {
		w.Write(body)
	}
}

// passthroughToRelay moves a passthrough session whose provider variants
// cannot ride one owned timeline (demuxed audio renditions) onto the relay,
// before its first playlist is served.
func (s *Service) passthroughToRelay(ctx context.Context, ss *session) (*ownedLease, error) {
	ss.prepareMu.Lock()
	defer ss.prepareMu.Unlock()
	ss.mu.Lock()
	lease, info := ss.owned, ss.info.MediaInfo
	ss.mu.Unlock()
	if lease != nil && lease.relay != nil {
		return lease, nil
	}
	if s.transcoder() == nil {
		return nil, core.ErrNotAvailable
	}
	if info == nil {
		probed, err := s.probeSessionInput(ctx, ss)
		if err != nil {
			return nil, err
		}
		if probed == nil || probed.Video() == nil {
			return nil, &TuneError{Code: "no_video", Stage: "probe", cause: core.ErrNotAvailable}
		}
		copy := *probed
		copy.Duration = 0
		info = &copy
	}
	dec, cut, _ := s.relayPlan(ss, *info)
	next, err := s.startOwnedRelay(ctx, ss, *info, relayCaps(ss.caps), dec, cut)
	if err != nil {
		return nil, Diagnose("relay_start", err)
	}
	output := liveRelayInfo(*info, dec)
	ss.mu.Lock()
	if ss.closed {
		ss.mu.Unlock()
		next.close()
		return nil, core.ErrNotFound
	}
	old := ss.owned
	ss.owned, ss.relayInfo, ss.info.MediaInfo = next, &output, info
	ss.mu.Unlock()
	old.close()
	s.sessionLog(ss).Info("live playback path selected", "path", "relay", "reason", "provider variants need remux")
	return next, nil
}

// ServeRelaySegment serves segment n of the session's owned timeline (or of
// its shared broadcast).
func (s *Service) ServeRelaySegment(w http.ResponseWriter, r *http.Request, id string, n int, u *core.User) {
	ss, err := s.getSession(r.Context(), id, u)
	if err != nil {
		writeStreamError(w, err)
		return
	}
	ss.mu.Lock()
	shared := ss.broadcast != nil
	lease := ss.owned
	ss.mu.Unlock()
	if shared {
		if n < 0 {
			writeStreamError(w, core.ErrNotFound)
			return
		}
		s.serveBroadcastTS(w, r, ss, int64(n))
		return
	}
	if lease == nil || n < 0 {
		writeStreamError(w, core.ErrNotFound)
		return
	}
	seg, err := lease.tl.segment(r.Context(), int64(n))
	if err != nil {
		writeStreamError(w, err)
		return
	}
	if r.Method != http.MethodHead {
		s.touchMedia(ss)
	}
	ctype := "video/mp2t"
	if seg.ext == "m4s" {
		ctype = "video/mp4"
	}
	w.Header().Set("Content-Type", ctype)
	w.Header().Set("Content-Length", strconv.Itoa(len(seg.data)))
	w.Header().Set("Cache-Control", "private, max-age=60")
	armWriteDeadline(w)
	observed := s.observeMedia(w, ss)
	defer observed.done()
	observed.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = observed.Write(seg.data)
	}
}

// TranscodeFor is unused by shared broadcasts: decoder retries are owned by
// their planner and never swap a viewer's encoder input.
func (s *Service) TranscodeFor(string) (core.HLSSession, bool) { return nil, false }

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
