package sessions

import (
	"context"
	"crypto/rand"
	"net/http"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
)

// LiveBackend adapts the existing channel playout service. Serve receives a
// grant-validated request and a path relative to the scoped live stream.
type LiveBackend interface {
	Allowed(context.Context, *core.Viewer, string) error
	Tune(context.Context, *core.Viewer, string, core.ClientCaps, string) (string, error)
	Serve(http.ResponseWriter, *http.Request, *core.Viewer, string, string, string)
	Stop(context.Context, *core.Viewer, string) error
}

// LiveMediaBackend optionally returns the live session's probed or output MediaInfo.
type LiveMediaBackend interface {
	LiveBackend
	TuneMedia(context.Context, *core.Viewer, string, core.ClientCaps, string) (string, *core.MediaInfo, error)
}

func (b *Broker) planLive(ctx context.Context, req frontend.PlanRequest, viewer *core.Viewer) (frontend.PlayPlan, error) {
	if b.deps.Live == nil {
		return frontend.PlayPlan{}, core.ErrNotAvailable
	}
	if err := b.deps.Live.Allowed(ctx, viewer, req.Channel); err != nil {
		return frontend.PlayPlan{}, err
	}
	if req.Intent.AllowTranscoding != nil && !*req.Intent.AllowTranscoding {
		return frontend.PlayPlan{}, core.ErrNotAvailable
	}
	info := core.MediaInfo{Container: "mpegts", Estimated: true}
	decision := core.Decision{Kind: core.DecisionRemux, Container: "mpegts", VideoCodec: "h264", AudioCodec: "aac", VideoCopy: true, AudioCopy: true, AudioChannels: 2, AudioStream: -1, SubtitleStream: -1, Reason: "live_remux_hls"}
	if req.Intent.Estimate {
		return frontend.PlayPlan{Estimated: true, Media: info, Decision: decision, Transcoding: true, HLS: true, HLSContainer: "mpegts"}, nil
	}
	if !b.reservePlan(viewer.ID) {
		return frontend.PlayPlan{}, core.ErrStreamLimit
	}
	defer b.releasePlanReservation(viewer.ID)
	id := rand.Text()
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return frontend.PlayPlan{}, core.ErrStreamLimit
	}
	b.plans[id] = brokerPlan{request: req, decision: decision, created: b.deps.Now(), live: true}
	return frontend.PlayPlan{ID: id, Media: info, Decision: decision, Transcoding: true, HLS: true, HLSContainer: "mpegts"}, nil
}

func (b *Broker) openLive(ctx context.Context, req frontend.OpenRequest, p brokerPlan, viewer *core.Viewer) (frontend.Session, error) {
	if b.deps.Live == nil {
		return frontend.Session{}, core.ErrNotAvailable
	}
	if err := b.deps.Live.Allowed(ctx, viewer, p.request.Channel); err != nil {
		return frontend.Session{}, err
	}
	id, err := sessionID(req.Protocol)
	if err != nil {
		return frontend.Session{}, err
	}
	var liveID string
	var tunedMedia *core.MediaInfo
	if mb, ok := b.deps.Live.(LiveMediaBackend); ok {
		liveID, tunedMedia, err = mb.TuneMedia(ctx, viewer, p.request.Channel, p.request.Caps, p.request.Intent.ClientSessionID)
	} else {
		liveID, err = b.deps.Live.Tune(ctx, viewer, p.request.Channel, p.request.Caps, p.request.Intent.ClientSessionID)
	}
	if err != nil {
		return frontend.Session{}, capacityError(err)
	}
	liveMedia := normalizeLiveMedia(tunedMedia)
	grant := rand.Text()
	session := frontend.Session{ID: id, MediaPath: "/sessions/media/" + id + "/" + grant + "/live/index.m3u8", Protocol: req.Protocol, ViewerID: viewer.ID, Device: p.request.Device, Channel: p.request.Channel, Media: &liveMedia, StartedAt: b.deps.Now()}
	if req.Protocol == "silo" {
		session.ClientSessionID = p.request.Intent.ClientSessionID
	}
	if err := b.enrichCapabilities(ctx, &session); err != nil {
		_ = b.deps.Live.Stop(ctx, viewer, liveID)
		return frontend.Session{}, err
	}
	s := &brokerSession{public: copySession(session), grant: grant, playID: liveID, clientID: p.request.Caps.ClientID, caps: p.request.Caps, opts: p.request.Intent.Options, decision: p.decision, live: true}
	if b.cluster != nil && session.Protocol != "plex" {
		if err := b.cluster.store.put(ctx, session, liveID, *viewer, b.deps.Now()); err != nil {
			_ = b.deps.Live.Stop(ctx, viewer, liveID)
			return frontend.Session{}, err
		}
	}
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		if b.cluster != nil {
			_ = b.cluster.store.remove(ctx, id)
		}
		_ = b.deps.Live.Stop(ctx, viewer, liveID)
		return frontend.Session{}, core.ErrNotAvailable
	}
	superseded := b.collectSupersededLocked(s)
	b.sessions[id] = s
	b.mu.Unlock()
	b.finishSuperseded(ctx, superseded, liveID)
	if b.cluster != nil {
		b.cluster.ownershipChanged(ctx)
	}
	b.publish(ctx, sessionEvent("SessionStarted", session, nil))
	return copySession(session), nil
}

func normalizeLiveMedia(in *core.MediaInfo) core.MediaInfo {
	var out core.MediaInfo
	if in != nil {
		out = publicMedia(*in)
	}
	out.Container = "mpegts"
	out.Duration = 0
	out.Size = 0
	out.Chapters = nil
	out.Keyframes = nil
	out.Estimated = false
	kept := make([]core.StreamInfo, 0, len(out.Streams))
	haveAudioDefault := false
	for _, s := range out.Streams {
		if s.Type != "video" && s.Type != "audio" {
			continue
		}
		if s.Type == "video" {
			if s.Codec == "" {
				s.Codec = "h264"
			}
			if s.Width <= 0 {
				s.Width = 1920
			}
			if s.Height <= 0 {
				s.Height = 1080
			}
			if s.BitDepth <= 0 {
				s.BitDepth = 8
			}
			if s.PixelFormat == "" {
				s.PixelFormat = "yuv420p"
			}
			if !s.DolbyVision && s.DolbyVisionProfile == 0 {
				no := false
				s.DolbyVisionConfigurationPresent = &no
				if s.HDR10Plus == nil && s.ColorTransfer != "smpte2084" && s.ColorTransfer != "arib-std-b67" {
					s.HDR10Plus = &no
				}
			}
		}
		if s.Type == "audio" {
			if s.Codec == "" {
				s.Codec = "aac"
			}
			if s.Channels <= 0 {
				s.Channels = 2
			}
			if s.SampleRate <= 0 {
				s.SampleRate = 48000
			}
			if s.Default || (s.DefaultDisposition != nil && *s.DefaultDisposition) {
				haveAudioDefault = true
			}
		}
		s.Index = len(kept)
		kept = append(kept, s)
	}
	if len(kept) == 0 {
		no := false
		eight := 8
		kept = []core.StreamInfo{
			{Index: 0, Type: "video", Codec: "h264", Width: 1920, Height: 1080, BitDepth: 8, BitsPerRawSample: &eight, PixelFormat: "yuv420p", ColorTransfer: "bt709", ColorSpace: "bt709", ColorPrimaries: "bt709", SampleAspectRatio: "1:1", DisplayAspectRatio: "16:9", Default: true, DolbyVisionConfigurationPresent: &no, HDR10Plus: &no},
			{Index: 1, Type: "audio", Codec: "aac", Channels: 2, SampleRate: 48000, ChannelLayout: "stereo", Default: true},
		}
		haveAudioDefault = true
	}
	if !haveAudioDefault {
		for i := range kept {
			if kept[i].Type == "audio" {
				kept[i].Default = true
				break
			}
		}
	}
	out.Streams = kept
	return out
}
