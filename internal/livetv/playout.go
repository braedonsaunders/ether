package livetv

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/braedonsaunders/ether/internal/clientreg"
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
	relay               core.HLSSession
	relayCancel         context.CancelFunc
	relayInfo           *core.MediaInfo
	dash                core.DASHSession
	http                core.HTTPSession
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
	// failoverGate serializes source changes without blocking close or readers.
	failoverGate chan struct{} // initialized under mu
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
	resources   map[string]string
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
	relay := ss.relay
	relayCancel := ss.relayCancel
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
	if relayCancel != nil {
		relayCancel()
	}
	if relay != nil {
		relay.Close()
	}
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
	if c.Kind == "iptv" && c.SourceID != "" && !s.useBroadcast(c) {
		if max := s.sourceConnectionLimit(c.SourceID); max > 0 {
			playing := map[string]bool{}
			for _, other := range s.sessions {
				if other.info.Channel.SourceID == c.SourceID {
					playing[other.info.Channel.ID] = true
				}
			}
			if !playing[c.ID] && len(playing) >= max {
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
	s.noteWatchedLocked(c.ID)
	s.mu.Unlock()
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
	ss.mu.Unlock()
	if first {
		s.sessionLog(ss).Info("live media first requested", "since_tune", s.o.Now().Sub(ss.info.StartedAt))
	}
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
	ss.broadcast = stage.broadcast
	ss.relayCancel = stage.relayCancel
	s.transferInput(stage, ss)
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
	relay := ss.relay
	ss.mu.Unlock()
	if shared {
		s.serveBroadcastPlaylist(w, r, ss)
		return
	}
	if relay != nil {
		s.serveRelayPlaylist(w, r, ss)
		return
	}
	s.ServeResource(w, r, id, "index.m3u8", u)
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
	analyze := len(ss.caps.VideoCodecs) > 0 || len(ss.caps.AudioCodecs) > 0 || clientreg.HasQuirk(clientreg.Identity{Product: ss.caps.Product, Platform: ss.caps.Platform, Device: ss.caps.Device, Version: ss.caps.Version}, "probe-hls-without-codecs")
	probe := analyze || c.Transcode || ss.caps.Protocol == "jellyfin" || ss.caps.Protocol == "emby"
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
		if s.o.Transcoder != nil && probe {
			probeWaitCtx := ctx
			var waitCancel context.CancelFunc
			if ss.caps.Protocol == "silo" {
				// Silo playback/start has a < 1s budget. Wait briefly for a
				// cached or fast probe, then proceed with HLS passthrough if
				// live ffprobe is still sampling segments.
				probeWaitCtx, waitCancel = context.WithTimeout(ctx, 350*time.Millisecond)
				defer waitCancel()
			}
			info, err = s.takeEarlyProbe(probeWaitCtx, ss, probeCh)
			if analyze && ss.caps.Protocol != "silo" {
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
					dec := s.o.Transcoder.Decide(*info, ss.caps, core.DecideOptions{DirectPlay: true, DirectStream: true, AudioStream: -1, SubtitleStream: -1})
					if !dec.VideoCopy || !dec.AudioCopy {
						return s.startIPTVRelay(ctx, ss, *info)
					}
				}
			}
		}
		ss.resources["index.m3u8"] = c.StreamURL
		s.sessionLog(ss).Info("live playback path selected", "path", "hls_passthrough", "analyzed", ss.info.MediaInfo != nil)
		return nil
	}
	if s.o.Transcoder == nil {
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
	if !need || s.o.Transcoder == nil {
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
	shared := ss.broadcast != nil
	relay := ss.relay
	ss.mu.Unlock()
	if shared {
		s.serveBroadcastTS(w, r, ss, int64(n))
		return
	}
	if relay == nil || n < 0 {
		writeStreamError(w, core.ErrNotFound)
		return
	}
	s.touchMedia(ss)
	observed := s.observeMedia(w, ss)
	defer observed.done()
	relay.ServeSegment(observed, r, n)
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
