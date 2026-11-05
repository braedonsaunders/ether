package livetv

import (
	"context"
	"strconv"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/onair/raster"
	"github.com/braedonsaunders/ether/internal/onair/signal"
	"github.com/braedonsaunders/ether/internal/transcode"
)

func (s *Service) screenDefaults() config.LiveTVScreens {
	if s.o.Screens != nil {
		defaults := s.o.Screens()
		if defaults.NoSignal == "" {
			defaults.NoSignal = "error"
		}
		if defaults.OffAir == "" {
			defaults.OffAir = "card"
		}
		if defaults.ApplyTo == "" {
			defaults.ApplyTo = "converted"
		}
		return defaults
	}
	return config.LiveTVScreens{NoSignal: "error", OffAir: "card", ApplyTo: "converted"}
}
func (s *Service) screenStyle(c Channel, offair bool) string {
	defaults := s.screenDefaults()
	id := ""
	if s.o.DefaultStyle != nil {
		id = s.o.DefaultStyle()
	}
	if c.Look != nil && c.Look.LookID != "" {
		id = c.Look.LookID
	}
	if s.o.StyleScreens != nil {
		if v := s.o.StyleScreens(id); v != nil {
			defaults = *v
		}
	}
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
func (s *Service) useBroadcast(c Channel) bool {
	if c.Kind == "virtual" {
		return true
	}
	return c.Kind == "iptv" && s.screenStyle(c, false) != "error" && (c.Transcode || s.screenDefaults().ApplyTo == "all")
}
func (s *Service) screenRequest(c Channel, reason string) core.ClipRequest {
	offair := reason == offAirSlate
	style := s.screenStyle(c, offair)
	if style == "error" {
		style = "card"
	}
	if reason != offAirSlate && reason != "busy" {
		reason = "reconnecting"
	}
	station := "0"
	stationID := s.screenDefaults().StationID
	id := ""
	if s.o.DefaultStyle != nil {
		id = s.o.DefaultStyle()
	}
	if c.Look != nil && c.Look.LookID != "" {
		id = c.Look.LookID
	}
	if s.o.StyleScreens != nil {
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
	return core.ClipRequest{LookID: lookID, Moment: "screen." + style, ChannelID: c.ID, Bind: map[string]string{"screen.reason": reason, "screen.back": "", "channel.name": c.Name, "channel.number": c.Number, "screen.stationId": station}}
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
				return &signalInputDecoder{engine: signal.NewStatic(format.Width, format.Height, int64Hash(request.ChannelID+request.LookID)), overlay: overlay, format: format, tick: s.o.Now().Unix() * 30, audio: make([]int16, 3200), closed: make(chan struct{})}, nil, nil
			}
		}
	}
	clip, ok := s.o.Graphics.Clip(ctx, request)
	if !ok {
		s.o.Graphics.Ensure(ctx, request)
		return nil, nil, core.ErrNotAvailable
	}
	decoder, err := runner.StartDecoder(ctx, transcode.DecoderRequest{SessionID: "graphics-" + clip.Hash, InputURL: clip.Path, Format: format, Loop: loop})
	return decoder, nil, err
}
func (p *scheduleBroadcastPlanner) screen(time.Time) broadcastPlan {
	return p.s.screenPlan(p.channel, p.runner, p.format)
}
func (p *providerBroadcastPlanner) screen(time.Time) broadcastPlan {
	return p.s.screenPlan(p.channel, p.runner, p.format)
}
func (s *Service) screenPlan(c Channel, runner transcode.ContinuousRunner, format transcode.RawFormat) broadcastPlan {
	return s.signalPlan(c, "reconnecting", "", format)
}
func (s *Service) startBroadcast(ctx context.Context, ss *session) error {
	runner, ok := s.o.Transcoder.(transcode.ContinuousRunner)
	if !ok {
		return core.ErrNotAvailable
	}
	ss.mu.Lock()
	c, user, caps := ss.info.Channel, ss.user, ss.caps
	ss.mu.Unlock()
	height := 720
	if s.o.Transcoder != nil {
		hw := s.o.Transcoder.HWAccel()
		if hw.Name != "" && hw.Name != "none" {
			height = 1080
		}
	}
	format := transcode.RawFormat{Width: height * 16 / 9, Height: height, FPSNum: 30, FPSDen: 1, SampleRate: 48000, Channels: 2, PixelFormat: "yuv420p"}
	outputHeight := height
	if caps.MaxHeight > 0 && caps.MaxHeight < height {
		outputHeight = 480
		if height == 1080 && caps.MaxHeight >= 720 {
			outputHeight = 720
		}
	}
	bitrate := 3000
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
	sourceInfo := core.MediaInfo{Container: "mp4", Streams: []core.StreamInfo{{Index: 0, Type: "video", Codec: "h264", Width: format.Width, Height: format.Height, FrameRate: 30}, {Index: 1, Type: "audio", Codec: "aac", Channels: 2, SampleRate: 48000}}}
	outputCaps := caps
	outputCaps.VideoCodecs = []string{"h264"}
	outputCaps.AudioCodecs = []string{"aac"}
	outputCaps.MaxHeight = outputHeight
	decision := s.o.Transcoder.Decide(sourceInfo, outputCaps, core.DecideOptions{AudioStream: 1, SubtitleStream: -1})
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

	info := core.MediaInfo{Container: "mpegts", Streams: []core.StreamInfo{{Index: 0, Type: "video", Codec: "h264", Profile: "High", Width: outputHeight * 16 / 9, Height: outputHeight, FrameRate: 30}, {Index: 1, Type: "audio", Codec: "aac", Channels: 2, SampleRate: 48000}}}
	// Resolve a shared catalogue source as the owner, then enforce every viewer's
	// channel/title policy again at playlist and segment boundaries.
	inputUser := user
	if c.Personal == nil && s.o.InputUser != nil {
		owner, err := s.o.InputUser(ctx)
		if err != nil {
			return err
		}
		inputUser = *owner
	}
	create := func() (*broadcast, error) {
		var planner broadcastPlanner
		if c.Kind == "virtual" {
			planner = &scheduleBroadcastPlanner{s: s, channel: c, user: inputUser, caps: core.ClientCaps{MaxHeight: height, VideoCodecs: []string{"h264"}, AudioCodecs: []string{"aac"}}, format: format, runner: runner}
		} else {
			planner = &providerBroadcastPlanner{s: s, channel: c, format: format, runner: runner}
		}
		b, err := newBroadcast(s.ctx, runner, planner, format)
		if err != nil {
			return nil, err
		}
		if c.DVRMinutes > 0 {
			b.dvr = time.Duration(c.DVRMinutes) * time.Minute
		}
		return b, nil
	}
	id := "channel-" + c.ID + "-" + strconv.Itoa(outputHeight) + "-" + strconv.Itoa(bitrate)
	key := broadcastKey{channelID: c.ID}
	if c.Personal != nil {
		key.viewerID = user.ID
		id += "-viewer-" + strconv.FormatInt(user.ID, 10)
	}
	lease, err := s.broadcasts.acquire(ctx, key, id, decision, create)
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
	return nil
}

func (p *scheduleBroadcastPlanner) introPlan(now time.Time) (broadcastPlan, time.Duration) {
	c := p.s.personalChannel(p.s.ctx, p.channel, &p.user)
	if welcome, ok := p.s.personalWelcome(c, now); ok {
		p.joinAt = welcome.End
		return p.programme(welcome), welcome.End.Sub(now)
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
