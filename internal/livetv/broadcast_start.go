package livetv

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/onair"
	"github.com/braedonsaunders/ether/internal/onair/raster"
	"github.com/braedonsaunders/ether/internal/onair/signal"
	"github.com/braedonsaunders/ether/internal/transcode"
)

type liveMomentSource interface {
	LiveRenderAllowed(r core.ClipRequest) (allowed bool, speedX float64, reason string)
	OpenMoment(ctx context.Context, r core.ClipRequest, width, height, fpsNum, fpsDen int, loop bool, openClip func(ctx context.Context, path string, loop bool) (onair.FrameReader, error)) (*onair.MomentStream, string, error)
	CachedStandby(ctx context.Context, lookID string) (core.ClipRef, bool)
	StandbyMoment(lookID string) (string, bool)
}

type decoderFrameReader struct {
	mu      sync.Mutex
	callCtx context.Context
	dec     transcode.Decoder
}

func (r *decoderFrameReader) setCallCtx(ctx context.Context) {
	r.mu.Lock()
	r.callCtx = ctx
	r.mu.Unlock()
}

func (r *decoderFrameReader) ReadFrame(video []byte) ([]byte, error) {
	r.mu.Lock()
	ctx := r.callCtx
	r.mu.Unlock()
	if ctx == nil {
		ctx = context.Background()
	}
	f, err := r.dec.Next(ctx)
	if err != nil {
		return nil, err
	}
	copy(video, f.Video)
	return f.PCM, nil
}

func (r *decoderFrameReader) Close() error {
	return r.dec.Close()
}

type momentStreamDecoder struct {
	s          *Service
	channelID  string
	moment     string
	mode       string
	coldPlate  bool
	stream     *onair.MomentStream
	baseReader *decoderFrameReader
	format     transcode.RawFormat
}

func (d *momentStreamDecoder) startupBurstReady() bool {
	return !d.coldPlate || d.stream.AnimationReady()
}

func (d *momentStreamDecoder) Next(ctx context.Context) (transcode.RawFrame, error) {
	if err := ctx.Err(); err != nil {
		return transcode.RawFrame{}, err
	}
	if d.baseReader != nil {
		d.baseReader.setCallCtx(ctx)
	}
	video := make([]byte, d.format.Width*d.format.Height*3/2)
	audio, err := d.stream.ReadFrame(video)
	if err != nil {
		if errors.Is(err, onair.ErrLiveBelowRealtime) && d.s != nil {
			d.s.noteMomentPlayout(d.channelID, d.moment, "fallback", d.stream.SpeedX(), err.Error())
		}
		return transcode.RawFrame{}, err
	}
	transition := false
	if mode := d.stream.Mode(); mode != d.mode {
		transition = mode == "cached" || mode == "split-cached"
		d.mode = mode
		if d.s != nil {
			d.s.noteMomentPlayout(d.channelID, d.moment, mode, d.stream.SpeedX(), "")
		}
	}
	return transcode.RawFrame{Video: video, PCM: audio, GraphicsTransition: transition}, nil
}

func (d *momentStreamDecoder) Close() error {
	err := d.stream.Close()
	if d.s != nil && d.stream.SpeedX() > 0 {
		d.s.noteMomentSpeed(d.stream.SpeedX())
	}
	return err
}

func (s *Service) screenDefaults(c Channel) config.LiveTVScreens {
	defaults := config.LiveTVScreens{NoSignal: "error", OffAir: "card", ApplyTo: "converted"}
	if c.Kind == "iptv" {
		// IPTV plays through a continuous output; its visible failure mode is
		// the warm reconnecting screen.
		defaults.NoSignal, defaults.ApplyTo = "card", "all"
	}
	if c.Look != nil && c.Look.LookID != "" {
		// Match StyleScreensEditor when the style has no explicit override.
		defaults.NoSignal, defaults.StationID = "card", true
		if s.o.StyleScreens != nil {
			if v := s.o.StyleScreens(c.Look.LookID); v != nil {
				defaults = *v
			}
		}
	}
	return defaults
}
func (s *Service) screenStyle(c Channel, offair bool) string {
	defaults := s.screenDefaults(c)
	style := defaults.NoSignal
	if offair {
		style = defaults.OffAir
	}
	if c.Screens != nil {
		override := c.Screens.NoSignal
		if offair {
			override = c.Screens.OffAir
		}
		if override != "" {
			style = override
		}
	}
	if style == "" {
		if offair {
			return "card"
		}
		return "error"
	}
	return style
}

// useBroadcast reports the channels that play through a continuous output.
// IPTV defaults to it (Steady); the tune itself may still refuse a 4K or HDR
// source, or a host without encode capacity, and take a direct path. A channel
// whose owner chose no screen at all ("error") never shows one, so it plays
// direct.
func (s *Service) useBroadcast(c Channel) bool {
	if c.Kind == "virtual" {
		return true
	}
	return c.Kind == "iptv" && (c.Screens == nil || c.Screens.NoSignal != "error")
}
func (s *Service) screenRequest(c Channel, reason string) core.ClipRequest {
	offair := reason == offAirSlate
	style := s.screenStyle(c, offair)
	generic := "0"
	if style == "error" {
		style = "card"
		generic = "1"
	}
	if reason != offAirSlate && reason != "busy" {
		reason = "reconnecting"
	}
	station := "0"
	stationID := s.screenDefaults(c).StationID
	id := ""
	if c.Look != nil && c.Look.LookID != "" {
		id = c.Look.LookID
	}
	if id != "" && s.o.StyleScreens != nil {
		if v := s.o.StyleScreens(id); v != nil {
			stationID = v.StationID
		}
	}
	if stationID {
		station = "1"
	}
	lookID := id
	if c.Look != nil && c.Look.LookID != "" {
		lookID = c.Look.LookID
	}
	return core.ClipRequest{LookID: lookID, Moment: "screen." + style, ChannelID: c.ID, Bind: map[string]string{"screen.reason": reason, "screen.back": "", "channel.name": c.Name, "channel.number": c.Number, "screen.stationId": station, "screen.generic": generic}}
}
func (s *Service) openGraphicsDecoder(ctx context.Context, runner transcode.ContinuousRunner, request core.ClipRequest, format transcode.RawFormat, loop bool) (transcode.Decoder, func(), error) {
	if s.o.Graphics == nil {
		return nil, nil, core.ErrNotAvailable
	}
	// A static scene owns a live phosphor engine, never a looping cached clip.
	if source, ok := s.o.Graphics.(overlaySource); ok {
		if doc, bindings, found := source.Overlay(ctx, request); found {
			if foreground, live := doc.StaticForeground(); live {
				overlay := compactOverlay(raster.Render(foreground, bindings, doc.Poster, format.Width, format.Height), format)
				seed := int64Hash(request.ChannelID + request.LookID)
				return newSignalInputDecoder(signal.NewStatic(format.Width, format.Height, seed), overlay, format, s.o.Now(), true, seed), nil, nil
			}
		}
	}
	if lm, ok := s.o.Graphics.(liveMomentSource); ok {
		stream, mode, err := lm.OpenMoment(ctx, request, format.Width, format.Height, format.FPSNum, format.FPSDen, loop, func(cctx context.Context, path string, clipLoop bool) (onair.FrameReader, error) {
			dec, dErr := runner.StartDecoder(cctx, transcode.DecoderRequest{
				SessionID: "graphics-" + filepath.Base(path),
				InputURL:  path,
				Format:    format,
				Loop:      clipLoop,
			})
			if dErr != nil {
				return nil, dErr
			}
			return &decoderFrameReader{callCtx: cctx, dec: dec}, nil
		})
		if err != nil {
			if errors.Is(err, onair.ErrLiveBelowRealtime) {
				s.noteMomentPlayout(request.ChannelID, request.Moment, "fallback", 0, mode)
			}
			return nil, nil, err
		}
		s.noteMomentPlayout(request.ChannelID, request.Moment, mode, stream.SpeedX(), "")
		return &momentStreamDecoder{
			s:         s,
			channelID: request.ChannelID,
			moment:    request.Moment,
			mode:      mode,
			coldPlate: mode == "static-plate",
			stream:    stream,
			format:    format,
		}, nil, nil
	}
	clip, ok := s.o.Graphics.Clip(ctx, request)
	if !ok {
		s.o.Graphics.Ensure(ctx, request)
		return nil, nil, core.ErrNotAvailable
	}
	decoder, err := runner.StartDecoder(ctx, transcode.DecoderRequest{SessionID: "graphics-" + clip.Hash, InputURL: clip.Path, Format: format, Loop: loop})
	return decoder, nil, err
}

func (p *scheduleBroadcastPlanner) standbyPlan() (broadcastPlan, bool) {
	c := p.channel
	if c.Look == nil || c.Look.LookID == "" || p.s == nil || p.s.o.Graphics == nil {
		return broadcastPlan{}, false
	}
	lm, ok := p.s.o.Graphics.(liveMomentSource)
	if !ok {
		return broadcastPlan{}, false
	}
	clip, ok := p.cachedStandby(lm, c.Look.LookID)
	if !ok || clip.Path == "" {
		return broadcastPlan{}, false
	}
	return broadcastPlan{
		id:      "standby-" + c.ID + "-" + clip.Hash,
		element: "standby",
		open: func(ctx context.Context) (transcode.Decoder, func(), error) {
			d, err := p.runner.StartDecoder(ctx, transcode.DecoderRequest{
				SessionID: "standby-" + clip.Hash,
				InputURL:  clip.Path,
				Format:    p.format,
				Loop:      true,
			})
			return d, nil, err
		},
	}, true
}

// cachedStandby answers the look's standby clip from a short memo: a hit
// stays valid for 30 s, a miss is retried after 5 s so a newly rendered
// standby is picked up quickly.
func (p *scheduleBroadcastPlanner) cachedStandby(lm liveMomentSource, lookID string) (core.ClipRef, bool) {
	now := time.Now()
	p.standbyMu.Lock()
	if p.standbyLook == lookID && !p.standbyAt.IsZero() {
		ttl := 5 * time.Second
		if p.standbyOK {
			ttl = 30 * time.Second
		}
		if now.Sub(p.standbyAt) < ttl {
			ref, ok := p.standbyRef, p.standbyOK
			p.standbyMu.Unlock()
			return ref, ok
		}
	}
	p.standbyMu.Unlock()
	ref, ok := lm.CachedStandby(context.Background(), lookID)
	p.standbyMu.Lock()
	p.standbyLook, p.standbyRef, p.standbyOK, p.standbyAt = lookID, ref, ok, now
	p.standbyMu.Unlock()
	return ref, ok
}

func (p *scheduleBroadcastPlanner) screen(time.Time) broadcastPlan {
	if standby, ok := p.standbyPlan(); ok {
		standby.id = "screen-" + standby.id
		return standby
	}
	return p.s.screenPlan(p.channel, p.runner, p.format)
}

// Keep the current title ready independently of provider outage screens.
// Standby masters contain background only; they cannot air as opening cards.
func (p *scheduleBroadcastPlanner) momentRequestAt(now time.Time) (core.ClipRequest, string) {
	c := p.s.graphicsChannel(p.channel)
	p.mu.Lock()
	defer p.mu.Unlock()
	selected := Programme{Item: Item{Slate: "ident"}}
	for _, programme := range p.window {
		if now.Before(programme.Start) || !now.Before(programme.End) {
			continue
		}
		selected = programme
		if programme.Item.Key != 0 {
			// Joining mid-programme needs a title for that programme, rather than
			// a station ident or the title of an unrelated future airing.
			selected.Item = Item{Slate: "ident"}
			if tonightEnabledAt(c, now) {
				selected.Item.Slate = "tonight"
			} else if c.Look != nil && c.Look.UpNext {
				selected.Item.Slate = "upnext"
			}
			selected.Start = now
			selected.End = programme.Start // include the current title in clip bindings
		}
		break
	}
	return p.s.clipRequest(c, selected, p.window), selected.ID
}

func (p *scheduleBroadcastPlanner) momentScreenAt(now time.Time) broadcastPlan {
	// Without a graphics source there is no opening title to hold. Use the
	// configured, prewarmed signal screen while the programme opens.
	if p.s.o.Graphics == nil {
		return broadcastPlan{}
	}
	c := p.channel
	request, id := p.momentRequestAt(now)
	return broadcastPlan{id: "moment-card-" + c.ID + "-" + id + "-" + request.Moment, element: request.Moment, open: func(ctx context.Context) (transcode.Decoder, func(), error) {
		if p.s.o.Graphics == nil || c.Look == nil {
			request.Bind["screen.reason"] = "moment"
			return p.s.openSignalDecoder(ctx, request, p.format)
		}
		decoder, release, err := p.s.openGraphicsDecoder(ctx, p.runner, request, p.format, true)
		if title, ok := decoder.(*momentStreamDecoder); ok {
			title.stream.UseExternalSegmentClock()
			title.stream.StartCompleteTitle()
		}
		return decoder, release, err
	}}
}
func (p *providerBroadcastPlanner) screen(time.Time) broadcastPlan {
	return p.s.screenPlan(p.channel, p.runner, p.format)
}
func (s *Service) screenPlan(c Channel, runner transcode.ContinuousRunner, format transcode.RawFormat) broadcastPlan {
	return s.signalPlan(c, "reconnecting", "", format)
}
func (s *Service) startBroadcast(ctx context.Context, ss *session) error {
	ss.mu.Lock()
	c, user, caps, refused := ss.info.Channel, ss.user, ss.caps, ss.steadyRefused
	ss.mu.Unlock()
	runner, ok := s.transcoder().(transcode.ContinuousRunner)
	if !ok {
		if c.Kind == "iptv" && refused == "" {
			ss.mu.Lock()
			ss.steadyRefused = steadyRefusedNoTranscoder
			ss.mu.Unlock()
			return fmt.Errorf("%w: no continuous transcoder", errSteadyRefused)
		}
		return core.ErrNotAvailable
	}
	key := broadcastKey{channelID: c.ID}
	if c.Personal != nil {
		key.viewerID = user.ID
	}
	var format transcode.RawFormat
	var outputHeight, bitrate int
	var admitted *steadyAdmission
	if c.Kind == "iptv" {
		if refused != "" {
			// A direct tune that Steady already refused never escalates back.
			return fmt.Errorf("%w: %s", errSteadyRefused, refused)
		}
		full := 0
		if running := s.broadcasts.running(key); running != nil {
			format, full = running.format, running.steadyBitrate
		} else {
			var code, detail string
			admitted, code, detail = s.admitSteady(ctx, c)
			if code != "" {
				s.sessionLog(ss).Info("live playback path selected", "path", "direct", "reason", code, "detail", detail)
				ss.mu.Lock()
				ss.steadyRefused = code
				ss.mu.Unlock()
				return fmt.Errorf("%w: %s", errSteadyRefused, detail)
			}
			format, full = admitted.shape.format, admitted.shape.bitrateKbps
		}
		outputHeight, bitrate = steadyRendition(format, full, caps)
	} else {
		height := 720
		if s.steadyHardware() {
			height = 1080
		}
		format = transcode.RawFormat{Width: height * 16 / 9, Height: height, FPSNum: 30, FPSDen: 1, SampleRate: 48000, Channels: 2, PixelFormat: "yuv420p"}
		outputHeight = height
		if caps.MaxHeight > 0 && caps.MaxHeight < height {
			outputHeight = 480
			if height == 1080 && caps.MaxHeight >= 720 {
				outputHeight = 720
			}
		}
		bitrate = 3000
		if outputHeight == 1080 {
			bitrate = 6000
		}
		if outputHeight == 480 {
			bitrate = 1500
		}
		if caps.MaxBitrateKbps > 0 && caps.MaxBitrateKbps < bitrate {
			bitrate = caps.MaxBitrateKbps
			if height == 1080 {
				outputHeight = 720
			} else {
				outputHeight = 480
			}
		}
	}
	var attrs []any
	if admitted != nil {
		attrs = []any{"allocation", admitted.class}
	}
	lease, info, err := s.launchBroadcast(ctx, ss, broadcastLaunch{channel: c, user: user, caps: caps, key: key, runner: runner, format: format, outputHeight: outputHeight, bitrate: bitrate, admitted: admitted, logAttrs: attrs})
	if err != nil {
		return err
	}
	ss.mu.Lock()
	if ss.closed {
		ss.mu.Unlock()
		lease.close()
		return core.ErrNotFound
	}
	ss.broadcast = lease
	ss.info.MediaInfo = &info
	ss.mu.Unlock()
	awaitBroadcastCushion(ctx, lease, c.Kind == "iptv")
	return nil
}

// broadcastLaunch is everything a viewer's continuous output needs: the
// working format, the viewer's rendition and, for a new provider output, its
// admitted first input.
type broadcastLaunch struct {
	channel      Channel
	user         core.User
	caps         core.ClientCaps
	key          broadcastKey
	runner       transcode.ContinuousRunner
	format       transcode.RawFormat
	outputHeight int
	bitrate      int
	admitted     *steadyAdmission
	logAttrs     []any
}

// launchBroadcast starts (or joins) the output and attaches one rendition
// lease for the viewer. It owns admitted's link: consumed by a new output,
// closed otherwise.
func (s *Service) launchBroadcast(ctx context.Context, ss *session, l broadcastLaunch) (*broadcastLease, core.MediaInfo, error) {
	c, user, caps, key, runner, format, outputHeight, bitrate, admitted := l.channel, l.user, l.caps, l.key, l.runner, l.format, l.outputHeight, l.bitrate, l.admitted
	consumed := false
	defer func() {
		if admitted != nil && !consumed {
			admitted.link.close(s)
		}
	}()
	rate := float64(format.FPSNum) / float64(format.FPSDen)
	sourceInfo := core.MediaInfo{Container: "mp4", Streams: []core.StreamInfo{{Index: 0, Type: "video", Codec: "h264", Width: format.Width, Height: format.Height, FrameRate: rate}, {Index: 1, Type: "audio", Codec: "aac", Channels: 2, SampleRate: 48000}}}
	outputCaps := caps
	outputCaps.VideoCodecs = []string{"h264"}
	outputCaps.AudioCodecs = []string{"aac"}
	outputCaps.MaxHeight = outputHeight
	decision := s.transcoder().Decide(sourceInfo, outputCaps, core.DecideOptions{AudioStream: 1, SubtitleStream: -1})
	decision.Kind = core.DecisionTranscode
	decision.VideoCopy = false
	decision.AudioCopy = false
	decision.VideoCodec = "h264"
	decision.AudioCodec = "aac"
	decision.AudioChannels = 2
	decision.AudioSampleRate = 48000
	decision.MaxHeight = outputHeight
	decision.BitrateKbps = bitrate
	decision.Container = "mpegts"

	info := core.MediaInfo{Container: "mpegts", Streams: []core.StreamInfo{{Index: 0, Type: "video", Codec: "h264", Profile: "High", Width: outputHeight * 16 / 9, Height: outputHeight, FrameRate: rate}, {Index: 1, Type: "audio", Codec: "aac", Channels: 2, SampleRate: 48000}}}
	// Resolve a shared catalogue source as the owner, then enforce every viewer's
	// channel/title policy again at playlist and segment boundaries.
	inputUser := user
	if c.Personal == nil && s.o.InputUser != nil {
		owner, err := s.o.InputUser(ctx)
		if err != nil {
			return nil, core.MediaInfo{}, err
		}
		inputUser = *owner
	}
	create := func() (*broadcast, error) {
		var planner broadcastPlanner
		if c.Kind == "virtual" {
			planner = &scheduleBroadcastPlanner{s: s, channel: c, user: inputUser, caps: core.ClientCaps{MaxHeight: format.Height, VideoCodecs: []string{"h264"}, AudioCodecs: []string{"aac"}}, format: format, runner: runner}
		} else {
			provider := &providerBroadcastPlanner{s: s, channel: c, format: format, runner: runner, ladder: newSteadyLadder()}
			if admitted != nil && admitted.link != nil {
				provider.primed = admitted.link
				consumed = true
			}
			planner = provider
		}
		b, err := newBroadcast(s.serviceContext(), runner, planner, format)
		if err != nil {
			return nil, err
		}
		if admitted != nil {
			b.steadyBitrate = admitted.shape.bitrateKbps
		}
		if c.DVRMinutes > 0 {
			b.dvr = time.Duration(c.DVRMinutes) * time.Minute
		}
		return b, nil
	}
	id := "channel-" + c.ID + "-" + strconv.Itoa(outputHeight) + "-" + strconv.Itoa(bitrate)
	if c.Personal != nil {
		id += "-viewer-" + strconv.FormatInt(user.ID, 10)
	}
	if c.Kind == "iptv" {
		s.sessionLog(ss).Info("live playback path selected", append([]any{"path", "steady", "width", format.Width, "height", format.Height, "fps", rate, "rendition_height", outputHeight, "bitrate_kbps", bitrate}, l.logAttrs...)...)
	}
	lease, err := s.broadcasts.acquire(ctx, key, id, decision, create, caps)
	if err != nil {
		return nil, core.MediaInfo{}, err
	}
	return lease, info, nil
}

// steadyRendition is one viewer's rendition of a Steady output: the source
// size and quality unless the viewer's own limits ask for less.
func steadyRendition(f transcode.RawFormat, full int, caps core.ClientCaps) (int, int) {
	height := f.Height
	if full <= 0 {
		full = steadyShapeFor(nil).bitrateKbps
	}
	bitrate := full
	for _, h := range []int{720, 576, 480} {
		if caps.MaxHeight > 0 && caps.MaxHeight < height && h < height && h <= caps.MaxHeight {
			bitrate = full * h * h / (height * height)
			height = h
		}
	}
	if caps.MaxBitrateKbps > 0 && caps.MaxBitrateKbps < bitrate {
		bitrate = caps.MaxBitrateKbps
		for _, h := range []int{720, 576, 480} {
			if h < height && bitrate*height*height/(h*h) <= full {
				height = h
				break
			}
		}
	}
	return height, max(bitrate, 500)
}

func (p *scheduleBroadcastPlanner) introPlan(now time.Time) (broadcastPlan, time.Duration) {
	titleDuration := segmentDuration
	p.mu.Lock()
	for _, programme := range p.window {
		if now.Before(programme.Start) || !now.Before(programme.End) {
			continue
		}
		if interstitial(programme.Item) {
			p.mu.Unlock()
			return broadcastPlan{}, 0
		}
		titleDuration = min(titleDuration, programme.End.Sub(now))
		break
	}
	p.mu.Unlock()
	c := p.s.personalChannel(p.s.serviceContext(), p.channel, &p.user)
	if welcome, ok := p.s.personalWelcome(c, now); ok {
		p.joinAt = welcome.End
		return p.programme(welcome), welcome.End.Sub(now)
	}
	if p.s.o.Graphics != nil {
		request, _ := p.momentRequestAt(now)
		if request.Moment == "tonight" || request.Moment == "upnext" {
			title := p.momentScreenAt(now)
			title.id = "intro-" + title.id
			// Cold and cached titles own the same bounded media interval. Decoder
			// preparation can delay publication, but cannot lengthen the card.
			p.joinAt = now.Add(titleDuration)
			return title, titleDuration
		}
	}
	if c.Look == nil || !c.Look.Ident || p.s.o.Graphics == nil {
		return broadcastPlan{}, 0
	}
	request := p.s.clipRequest(c, Programme{ChannelID: c.ID, Item: Item{Slate: "ident"}})
	clip, ok := p.s.o.Graphics.Clip(context.Background(), request)
	if !ok || clip.Duration <= 0 || clip.Duration%segmentDuration != 0 {
		return broadcastPlan{}, 0
	}
	return broadcastPlan{element: "ident", id: "intro-" + c.ID, fill: true, open: func(ctx context.Context) (transcode.Decoder, func(), error) {
		return p.s.openGraphicsDecoder(ctx, p.runner, request, p.format, false)
	}}, min(clip.Duration, 4*time.Second)
}
