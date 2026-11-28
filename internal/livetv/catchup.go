package livetv

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/transcode"
)

type CatchUpRequest = frontend.CatchUpRequest
type CatchUpInfo = frontend.CatchUpInfo
type CatchUpError = frontend.CatchUpError

var ErrInvalidCatchUpRange = frontend.ErrInvalidCatchUpRange

func archiveUnavailable() error { return &CatchUpError{Code: "catchup_unavailable"} }

type catchUpSession struct {
	provider     *providerChange
	previous     []*catchUpSession
	retryAt      time.Time
	inputMu      sync.Mutex
	input        *catchUpInput
	inputClaimed bool
	inputClosed  bool
	inputPaused  bool
	info         CatchUpInfo
	hls          core.HLSSession
}

func (s *Service) catchUpSelection(ctx context.Context, u *core.User, c Channel, req CatchUpRequest, now time.Time) (CatchUpInfo, error) {
	info := CatchUpInfo{ChannelID: c.ID, Title: c.Name, Start: req.Start, StartOver: req.StartOver, DASH: req.DASH}
	if req.DASH && req.StartOver {
		return info, ErrInvalidCatchUpRange
	}
	if !s.allowedProgramme(ctx, u, c, nil) {
		return info, core.ErrUnauthorized
	}
	if c.Archive == nil || c.Archive.Days <= 0 {
		return info, &CatchUpError{Code: "catchup_not_supported"}
	}
	days := min(c.Archive.Days, 30)
	earliest := now.Add(-time.Duration(days) * 24 * time.Hour)
	if req.ProgrammeID != "" {
		stamp, ok := strings.CutPrefix(req.ProgrammeID, c.ID+"-")
		startMS, err := strconv.ParseInt(stamp, 10, 64)
		if !ok || err != nil {
			return info, core.ErrNotFound
		}
		at := time.UnixMilli(startMS)
		if at.Before(earliest) || at.After(now) {
			return info, &CatchUpError{Code: "catchup_outside_archive", ArchiveDays: days, EarliestAt: earliest.UTC()}
		}
		p, found, err := s.programmeAt(ctx, c, at)
		if err != nil {
			return info, err
		}
		if !found || p.ID != req.ProgrammeID {
			return info, core.ErrNotFound
		}
		if !s.allowedProgramme(ctx, u, c, &p) {
			return info, core.ErrUnauthorized
		}
		info.ProgrammeID, info.Title, info.Start, info.ProgrammeEnd = p.ID, p.Item.Title, p.Start, p.End
		if info.ProgrammeEnd.After(now) != req.StartOver {
			return info, ErrInvalidCatchUpRange
		}
		info.End = info.ProgrammeEnd
	} else {
		if req.Start.IsZero() || req.Duration <= 0 || req.StartOver {
			return info, ErrInvalidCatchUpRange
		}
		info.End = req.Start.Add(req.Duration)
		if req.ClampStart && info.Start.Before(earliest) && info.End.After(earliest) {
			info.Start = earliest
		}
		// Explicit padding still obeys the programme policy at the selected time.
		p, ok, err := s.programmeAt(ctx, c, info.Start)
		if err != nil {
			return info, err
		}
		if ok && !s.allowedProgramme(ctx, u, c, &p) {
			return info, core.ErrUnauthorized
		}
		if ok && p.Item.Title != "" {
			info.Title = p.Item.Title
		}
	}
	if info.Start.Before(earliest) || info.Start.After(now) {
		return info, &CatchUpError{Code: "catchup_outside_archive", ArchiveDays: days, EarliestAt: earliest.UTC()}
	}
	if info.End.After(now) {
		info.End = now
	}
	if !info.End.After(info.Start) {
		return info, archiveUnavailable()
	}
	if req.InitialOffset < 0 || req.InitialOffset >= info.End.Sub(info.Start) || req.StartOver && req.InitialOffset > 0 {
		return info, ErrInvalidCatchUpRange
	}
	info.Origin = req.InitialOffset
	return info, nil
}

// StartCatchUp opens a provider archive through the IPTV input relay and
// returns a session ID for ServePlaylist, ServeRelaySegment, SeekCatchUp and Stop.
func (s *Service) StartCatchUp(ctx context.Context, u *core.User, req CatchUpRequest, caps core.ClientCaps, clientSessionID string) (CatchUpInfo, error) {
	if !s.Enabled() {
		return CatchUpInfo{}, core.ErrNotAvailable
	}
	now := s.clockNow()
	if u == nil || s.o.CanPlay != nil && !s.o.CanPlay(u, caps.Remote, now) {
		return CatchUpInfo{}, core.ErrUnauthorized
	}
	if s.o.ConfirmAdmission != nil {
		var err error
		u, err = s.o.ConfirmAdmission(ctx, u, caps.Remote, now)
		if err != nil {
			return CatchUpInfo{}, err
		}
	}
	ctx, policy, err := s.withPolicy(ctx, u)
	if err != nil {
		return CatchUpInfo{}, err
	}
	c, err := s.Channel(ctx, req.ChannelID)
	if err != nil {
		return CatchUpInfo{}, err
	}
	info, err := s.catchUpSelection(ctx, u, c, req, now)
	if err != nil {
		return CatchUpInfo{}, err
	}
	if req.ProgrammeID == "" && (u.MaxContentRating != "" || policy.MaxRating != "") {
		programmes, err := s.programmes(ctx, c, info.Start, info.End)
		if err != nil {
			return CatchUpInfo{}, err
		}
		for _, p := range programmes {
			if !s.allowedProgramme(ctx, u, c, &p) {
				return CatchUpInfo{}, core.ErrUnauthorized
			}
		}
	}
	c.Transcode = c.Transcode || policy.Transcode
	info.ID = liveSessionID()
	if caps.Remote && s.o.RemoteUploadCap != nil {
		if limit, fromBudget := s.o.RemoteUploadCap(ctx, info.ID, true, true, caps.MaxBitrateKbps, 0); fromBudget && limit > 0 && (caps.MaxBitrateKbps == 0 || limit < caps.MaxBitrateKbps) {
			caps.MaxBitrateKbps = limit
		}
	}
	if clientSessionID == "" {
		clientSessionID = caps.ClientID
	}
	handover := core.LiveHandover(ctx)
	previous := s.supersedeBeforeTune(u.ID, caps, info.ID, handover)
	var playing []*core.PlaySession
	if s.o.Playback != nil {
		playing = s.o.Playback.Sessions()
	}
	s.mu.Lock()
	limit := u.StreamLimit
	if s.o.StreamLimit != nil {
		limit = s.o.StreamLimit(u)
	}
	if limit > 0 && s.activeStreamCountLocked(u.ID, caps.MultiviewGroup, caps.ClientID, playing, previous) >= limit {
		s.mu.Unlock()
		return CatchUpInfo{}, core.ErrStreamLimit
	}
	s.nextSubscription++
	ss := &session{
		tuneStartedAt: now, archiveInput: true, ephemeral: true, streamGrant: randomID(), user: *u, caps: caps, last: now,
		info: SessionInfo{ID: info.ID, SubscriptionID: s.nextSubscription, ActivityID: liveSessionID(),
			ClientSessionID: clientSessionID, UserID: u.ID, Channel: c, StartedAt: now},
		view: core.PlaySession{ID: info.ID, PlaybackGroup: info.ID, UserID: u.ID, Username: u.Username,
			ClientID: caps.ClientID, Client: caps, TitleName: info.Title, StartedAt: now, Origin: "Catch-up"},
	}
	ss.allowCheck = func(ctx context.Context, viewer *core.User) error {
		ch, err := s.Channel(ctx, c.ID)
		if err != nil {
			return err
		}
		ctx, _, err = s.withPolicy(ctx, viewer)
		if err != nil {
			return err
		}
		p, found, err := s.programmeAt(ctx, ch, info.Start)
		if err != nil {
			return err
		}
		var programme *Programme
		if found {
			programme = &p
		}
		if !s.allowedProgramme(ctx, viewer, ch, programme) {
			return core.ErrUnauthorized
		}
		return nil
	}
	ss.ctx, ss.cancel = context.WithCancel(s.serviceContext())
	s.sessions[info.ID] = ss
	s.mu.Unlock()
	started := false
	defer func() {
		if !started {
			_ = s.Stop(ctx, info.ID, u)
		}
	}()
	s.notifySessionsChanged()
	if limiter, ok := s.o.Playback.(channelLimiter); ok {
		if err := limiter.BeginChannel(u, caps, info.ID); err != nil {
			return CatchUpInfo{}, err
		}
		ss.mu.Lock()
		if ss.closed {
			ss.mu.Unlock()
			limiter.EndChannel(info.ID)
			return CatchUpInfo{}, core.ErrNotFound
		}
		ss.reserved = true
		ss.mu.Unlock()
	}
	opened, err := s.openCatchUp(ctx, ss, info)
	if err != nil {
		return CatchUpInfo{}, err
	}
	ss.mu.Lock()
	if ss.closed {
		ss.mu.Unlock()
		opened.close(s)
		return CatchUpInfo{}, core.ErrNotFound
	}
	ss.catchUp, ss.ready = opened, true
	ss.mu.Unlock()
	if !handover {
		s.finishHandover(previous, info.ID)
	}
	started = true
	return opened.info, nil
}

type catchUpInput struct {
	stage *session
	url   string
	trim  time.Duration
}

func (input *catchUpInput) release(s *Service) {
	s.revokeInput(input.stage)
}

// openCatchUpInput prepares a single range for the probe and the encoder to
// share. The requested start includes zero additional pre-roll. Minute-based
// URLs still round down, with the encoder trimming the fractional minute.
func (s *Service) openCatchUpInput(ctx context.Context, ss *session, info CatchUpInfo, offset time.Duration) (*catchUpInput, error) {
	ctx, cancel := ss.bindContext(ctx)
	defer cancel()
	ctx, timeout := context.WithTimeout(ctx, 20*time.Second+s.stabilityReserve(ss.info.Channel))
	defer timeout()
	c := ss.info.Channel
	duration := info.End.Sub(info.Start) - info.Origin - offset
	start := info.Start.Add(info.Origin + offset)
	raw, err := s.ArchiveURL(ctx, c, start, duration)
	var trim time.Duration
	if err == nil && (c.Archive.Mode == "xtream" || c.Archive.Mode == "xc") && strings.Contains(raw, "/timeshift/") {
		// The provider accepts minute timestamps. Ask for the enclosing
		// range and discard the leading partial minute at the encoder.
		rounded := start.Truncate(time.Minute)
		trim = start.Sub(rounded)
		if trim > 0 {
			raw, err = s.ArchiveURL(ctx, c, rounded, duration+trim)
		}
	}
	if err != nil {
		return nil, archiveUnavailable()
	}
	src := StreamSource{ID: stableID(c.ID, "playlist"), AuthSourceID: c.SourceID, URL: raw}
	headers, err := s.upstreamHeaders(ctx, c, src)
	if err != nil {
		return nil, archiveUnavailable()
	}
	stage := &session{archiveInput: true, info: ss.info, caps: ss.caps, ctx: ss.ctx, inputSource: src, inputURL: raw, inputHeaders: headers}
	opened := &catchUpInput{stage: stage, trim: trim}
	ready := false
	defer func() {
		if !ready {
			opened.release(s)
		}
	}()
	if err := s.withProviderCapacity(ctx, c, src, func(admitCtx context.Context) error {
		return s.grantInputForContext(admitCtx, stage, src, headers, true, s.latencyPolicy().TargetLatency())
	}); err != nil {
		return nil, s.catchUpSourceError(ctx, c.SourceID, err)
	}
	prefix, _, err := s.sniffInput(ctx, stage, 7)
	if err != nil || len(prefix) == 0 {
		return nil, s.catchUpSourceError(ctx, c.SourceID, err)
	}
	opened.url, err = s.relayInput(stage)
	if err != nil {
		return nil, s.catchUpSourceError(ctx, c.SourceID, err)
	}
	ready = true
	return opened, nil
}

func (s *Service) enableCatchUpPlayback(input *catchUpInput) {
	s.inputRelay.mu.Lock()
	grant := s.inputRelay.bySess[input.stage]
	s.inputRelay.mu.Unlock()
	if grant != nil {
		grant.mu.Lock()
		tap := grant.tap
		grant.mu.Unlock()
		if tap != nil {
			tap.mu.Lock()
			tap.finitePlayback = true
			tap.cond.Broadcast()
			tap.mu.Unlock()
		}
	}
}

func (s *Service) openCatchUp(ctx context.Context, ss *session, info CatchUpInfo) (*catchUpSession, error) {
	tc := s.transcoder()
	if tc == nil {
		return nil, core.ErrNotAvailable
	}
	ctx, cancel := ss.bindContext(ctx)
	defer cancel()
	ctx, timeout := context.WithTimeout(ctx, 20*time.Second+s.stabilityReserve(ss.info.Channel))
	defer timeout()
	input, err := s.openCatchUpInput(ctx, ss, info, 0)
	if err != nil {
		return nil, err
	}
	opened := &catchUpSession{info: info, input: input}
	if g := s.sessionGrant(input.stage); g != nil {
		opened.provider = &providerChange{active: g.provider}
	}
	ready := false
	defer func() {
		if !ready {
			opened.close(s)
		}
	}()
	probe, err := s.probeSessionInput(ctx, input.stage)
	if err != nil || probe == nil || !probe.HasPlayableMedia() {
		return nil, s.catchUpSourceError(ctx, ss.info.Channel.SourceID, err)
	}
	s.enableCatchUpPlayback(input)
	probe.Duration = info.End.Sub(info.Start) - info.Origin
	probe.Keyframes = nil
	dec, cut, _ := s.relayPlan(input.stage, *probe)
	info.MediaGeneration = randomID()
	info.Decision = dec
	opened.info = info
	protocol := core.ProtocolHLS
	if info.DASH {
		protocol = core.ProtocolDASH
		dec.Container = transcode.ContainerFMP4
		opened.info.Decision = dec
	}
	opened.hls, err = tc.StartHLS(ss.ctx, core.HLSRequest{
		SessionID: info.ID + "-" + info.MediaGeneration, MediaGeneration: info.MediaGeneration, InputURL: input.url,
		Info: *probe, Decision: dec, SegmentDuration: cut, Protocol: protocol, ArchiveInput: true, OpenArchiveRange: opened.openRange(s, ss), NoVideoTranscode: ss.caps.NoVideoTranscode,
	})
	if err != nil {
		if errors.Is(err, frontend.ErrTooManyTranscodes) || errors.Is(err, frontend.ErrTranscodeCapacity) || errors.Is(err, frontend.ErrVideoTranscodingDisabled) {
			return nil, err
		}
		return nil, Diagnose("catchup_output", err)
	}
	ready = true
	return opened, nil
}

// openRange lets an HLS segment seek re-tune without changing the playlist
// or its media generation. Each run owns exactly one relay grant.
func (c *catchUpSession) openRange(s *Service, ss *session) core.ArchiveRangeOpener {
	return func(ctx context.Context, offset time.Duration) (core.ArchiveRangeInput, error) {
		c.inputMu.Lock()
		defer c.inputMu.Unlock()
		if c.inputClosed || c.inputPaused || ctx.Err() != nil {
			return core.ArchiveRangeInput{}, core.ErrNotAvailable
		}
		input := c.input
		if input == nil || c.inputClaimed || offset != 0 {
			if input != nil {
				input.release(s)
				c.input = nil
			}
			var err error
			input, err = s.openCatchUpInput(ctx, ss, c.info, offset)
			if err != nil {
				return core.ArchiveRangeInput{}, err
			}
			s.enableCatchUpPlayback(input)
			c.input = input
		}
		c.inputClaimed = true
		return core.ArchiveRangeInput{URL: input.url, Trim: input.trim, Release: func() {
			c.inputMu.Lock()
			defer c.inputMu.Unlock()
			if c.input == input {
				input.release(s)
				c.input = nil
			}
		}}, nil
	}
}

func (s *Service) catchUpSourceError(ctx context.Context, source string, err error) error {
	if errors.Is(err, ErrProviderBusy) {
		return s.wrapProviderConnectionCap(source, err)
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return archiveUnavailable()
}

func (c *catchUpSession) close(s *Service) {
	if c == nil {
		return
	}
	for _, previous := range c.previous {
		previous.closeRange(s)
	}
	c.closeRange(s)
}

func (c *catchUpSession) releaseInput(s *Service) {
	c.inputMu.Lock()
	defer c.inputMu.Unlock()
	c.inputClosed = true
	c.releaseInputLocked(s)
}

func (c *catchUpSession) releaseInputLocked(s *Service) {
	if c.input != nil {
		c.input.release(s)
		c.input = nil
	}
}

func (c *catchUpSession) closeRange(s *Service) {
	c.releaseInput(s)
	if c.hls != nil {
		c.hls.Close()
	}
}

// SeekCatchUp returns the current output when the target segment is buffered.
// A seek outside it opens the provider at the requested programme offset.
func (s *Service) SeekCatchUp(ctx context.Context, u *core.User, id string, offset time.Duration) (CatchUpInfo, error) {
	ss, err := s.getSession(ctx, id, u)
	if err != nil {
		return CatchUpInfo{}, err
	}
	ss.prepareMu.Lock()
	defer ss.prepareMu.Unlock()
	ss.mu.Lock()
	current := ss.catchUp
	if ss.closed || current == nil {
		ss.mu.Unlock()
		return CatchUpInfo{}, core.ErrNotFound
	}
	info := current.info
	if info.StartOver {
		info.End = minTime(s.clockNow(), info.ProgrammeEnd)
	}
	if offset < 0 || offset > info.End.Sub(info.Start) {
		ss.mu.Unlock()
		return CatchUpInfo{}, ErrInvalidCatchUpRange
	}
	if offset == info.End.Sub(info.Start) {
		if len(current.previous) > 0 {
			info.Origin = current.previous[0].info.Origin
			info.MediaGeneration = current.previous[0].info.MediaGeneration
		}
		ss.mu.Unlock()
		return info, nil
	}
	for _, chunk := range append(append([]*catchUpSession(nil), current.previous...), current) {
		buffer, ok := chunk.hls.(interface{ HasBufferedPosition(time.Duration) bool })
		if ok && offset >= chunk.info.Origin && offset < chunk.info.End.Sub(info.Start) && buffer.HasBufferedPosition(offset-chunk.info.Origin) {
			if len(current.previous) > 0 {
				info.Origin = current.previous[0].info.Origin
				info.MediaGeneration = current.previous[0].info.MediaGeneration
			}
			ss.mu.Unlock()
			return info, nil
		}
	}
	ss.mu.Unlock()
	// Release the current provider connection before opening a new range;
	// even single-connection providers must support seeking.
	current.inputMu.Lock()
	current.inputPaused = true
	current.releaseInputLocked(s)
	current.inputMu.Unlock()
	info.Origin = offset
	next, err := s.openCatchUp(ctx, ss, info)
	if err != nil {
		// Keep the old playlist usable if the new range could not open.
		// A concurrent Stop still prevents reopening through inputClosed.
		current.inputMu.Lock()
		current.inputPaused = false
		current.inputMu.Unlock()
		return CatchUpInfo{}, err
	}
	ss.mu.Lock()
	if ss.closed {
		ss.mu.Unlock()
		next.close(s)
		return CatchUpInfo{}, core.ErrNotFound
	}
	ss.catchUp = next
	ss.mu.Unlock()
	current.close(s)
	return next.info, nil
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

func (s *Service) serveCatchUp(w http.ResponseWriter, r *http.Request, ss *session, archive *catchUpSession, n *int) {
	if n == nil {
		first := archive
		if len(archive.previous) > 0 {
			first = archive.previous[0]
		}
		if generation := r.URL.Query().Get("archive"); generation != "" && generation != first.info.MediaGeneration {
			http.NotFound(w, r)
			return
		}
		if archive.info.StartOver {
			s.extendStartOver(ss, archive)
			s.serveStartOverPlaylist(w, r, archive)
			return
		}
		archive.hls.ServePlaylist(w, r)
		return
	}
	for _, previous := range archive.previous {
		if strings.HasPrefix(r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:], "g-"+previous.info.MediaGeneration+"-") {
			archive = previous
			break
		}
	}
	ss.mu.Lock()
	ss.consumed, ss.mediaStarted, ss.last = true, true, s.clockNow()
	ss.mu.Unlock()
	s.activeCatchUpRead(w, r, ss, archive, *n)
}

// Extend only as the player reloads its playlist and the current range has
// finished producing. A single provider connection works even at boundaries.
func (s *Service) extendStartOver(ss *session, archive *catchUpSession) {
	if !ss.prepareMu.TryLock() {
		return
	}
	ss.mu.Lock()
	current := ss.catchUp
	closed := ss.closed
	ss.mu.Unlock()
	if closed || current != archive {
		ss.prepareMu.Unlock()
		return
	}
	now := s.clockNow()
	end := minTime(now.Truncate(10*time.Second), archive.info.ProgrammeEnd)
	if !now.Before(archive.info.ProgrammeEnd) {
		end = archive.info.ProgrammeEnd
	}
	complete, ok := archive.hls.(interface{ ArchiveRangeComplete() bool })
	if !end.After(archive.info.End) || now.Before(archive.retryAt) || !ok || !complete.ArchiveRangeComplete() {
		ss.prepareMu.Unlock()
		return
	}
	go func() {
		defer ss.prepareMu.Unlock()
		archive.releaseInput(s)
		info := archive.info
		info.Origin = info.End.Sub(info.Start)
		info.End = end
		next, err := s.openCatchUp(ss.ctx, ss, info)
		if err != nil {
			archive.retryAt = now.Add(30 * time.Second)
			return
		}
		next.previous = append(append([]*catchUpSession(nil), archive.previous...), archive)
		ss.mu.Lock()
		if ss.closed {
			ss.mu.Unlock()
			next.closeRange(s)
			return
		}
		ss.catchUp = next
		ss.mu.Unlock()
	}()
}

func (s *Service) serveStartOverPlaylist(w http.ResponseWriter, r *http.Request, archive *catchUpSession) {
	w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	w.Header().Set("Cache-Control", "no-store")
	if r.Method == http.MethodHead {
		return
	}
	var content strings.Builder
	target := 1
	complete := false
	chunks := append(append([]*catchUpSession(nil), archive.previous...), archive)
	for n, chunk := range chunks {
		chunk.touchOutput()
		playlist := httptest.NewRecorder()
		if growing, ok := chunk.hls.(interface {
			ServeArchiveEventPlaylist(http.ResponseWriter, *http.Request)
		}); ok {
			growing.ServeArchiveEventPlaylist(playlist, r)
		} else {
			chunk.hls.ServePlaylist(playlist, r)
		}
		if playlist.Code != http.StatusOK {
			http.Error(w, "Archive output unavailable", http.StatusBadGateway)
			return
		}
		if n > 0 {
			content.WriteString("#EXT-X-DISCONTINUITY\n")
		}
		complete = strings.HasSuffix(playlist.Body.String(), "#EXT-X-ENDLIST\n")
		for _, line := range strings.Split(playlist.Body.String(), "\n") {
			if value, ok := strings.CutPrefix(line, "#EXT-X-TARGETDURATION:"); ok {
				duration, _ := strconv.Atoi(value)
				target = max(target, duration)
			}
			if strings.HasPrefix(line, "#EXTINF:") || strings.HasPrefix(line, "#EXT-X-MAP:") || strings.HasPrefix(line, "#EXT-X-DISCONTINUITY") || line != "" && !strings.HasPrefix(line, "#") {
				content.WriteString(line + "\n")
			}
		}
	}
	fmt.Fprintf(w, "#EXTM3U\n#EXT-X-VERSION:7\n#EXT-X-TARGETDURATION:%d\n#EXT-X-MEDIA-SEQUENCE:0\n#EXT-X-PLAYLIST-TYPE:EVENT\n#EXT-X-START:TIME-OFFSET=0,PRECISE=YES\n", target)
	_, _ = w.Write([]byte(content.String()))
	if complete && !archive.info.End.Before(archive.info.ProgrammeEnd) {
		_, _ = w.Write([]byte("#EXT-X-ENDLIST\n"))
	}
}

func (s *Service) activeCatchUpRead(w http.ResponseWriter, r *http.Request, ss *session, archive *catchUpSession, n int) {
	ss.activeReads.Add(1)
	defer ss.activeReads.Add(-1)
	observed := s.observeContinuousMedia(w, ss)
	defer observed.done()
	archive.hls.ServeSegment(observed, r, n)
}

// TouchLiveSession renews an authorized playback lease without moving its position.
func (s *Service) TouchLiveSession(ctx context.Context, u *core.User, id string) error {
	ss, err := s.getSession(ctx, id, u)
	if err != nil {
		return err
	}
	ss.mu.Lock()
	archive := ss.catchUp
	if !ss.closed {
		ss.lastControl = s.clockNow()
		if archive != nil {
			ss.last = ss.lastControl
		}
	}
	ss.mu.Unlock()
	if archive != nil {
		archive.touchOutput()
		for _, prior := range archive.previous {
			prior.touchOutput()
		}
	}
	return nil
}

func (c *catchUpSession) touchOutput() {
	if keep, ok := c.hls.(interface{ Touch() }); ok {
		keep.Touch()
	}
}

// ServeCatchUpTS streams the finite MPEG-TS rendition to Xtream clients.
func (s *Service) ServeCatchUpTS(w http.ResponseWriter, r *http.Request, id string, u *core.User) {
	ss, err := s.getSession(r.Context(), id, u)
	if err != nil {
		writeStreamError(w, err)
		return
	}
	ss.mu.Lock()
	archive := ss.catchUp
	ss.mu.Unlock()
	if archive == nil || archive.info.Decision.Container != transcode.ContainerMPEGTS {
		writeStreamError(w, core.ErrNotAvailable)
		return
	}
	w.Header().Set("Content-Type", "video/mp2t")
	if r.Method == http.MethodHead {
		return
	}
	playlist := httptest.NewRecorder()
	archive.hls.ServePlaylist(playlist, r)
	if playlist.Code != http.StatusOK {
		writeStreamError(w, core.ErrNotAvailable)
		return
	}
	for _, line := range strings.Split(playlist.Body.String(), "\n") {
		if r.Context().Err() != nil {
			return
		}
		n, ok := transcode.ParseSegmentName(line)
		if !ok || n < 0 {
			continue
		}
		part := r.Clone(r.Context())
		part.URL.Path = "/" + line
		part.Header.Del("Range")
		writer := &archiveTSWriter{ResponseWriter: w, header: make(http.Header)}
		s.serveCatchUp(writer, part, ss, archive, &n)
		if writer.failed || writer.err != nil {
			return
		}
		_ = http.NewResponseController(w).Flush()
	}
}

// Each segment supplies its own HTTP headers. Only the bytes belong in the
// continuous response.
type archiveTSWriter struct {
	http.ResponseWriter
	header http.Header
	failed bool
	err    error
}

func (w *archiveTSWriter) Header() http.Header    { return w.header }
func (w *archiveTSWriter) WriteHeader(status int) { w.failed = status >= 400 }
func (w *archiveTSWriter) Write(p []byte) (int, error) {
	if w.failed {
		return len(p), nil
	}
	n, err := w.ResponseWriter.Write(p)
	w.err = err
	return n, err
}
