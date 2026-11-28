package sessions

import (
	"context"
	"crypto/rand"
	"fmt"
	"net/http"
	"strings"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
)

func ownerGateKey(viewerID int64, clientID string) string {
	return fmt.Sprintf("broker/owner/%d/%s", viewerID, clientID)
}

func tileGateKey(viewerID int64, clientID, groupID string, tile int) string {
	return fmt.Sprintf("broker/owner/%d/%s/mv/%s/tile/%d", viewerID, clientID, groupID, tile)
}

// Caller holds b.mu.
func (b *Broker) bumpNonGroupEpochLocked(viewerID int64, clientID string) {
	b.pruneExpiredMultiviewLocked(b.deps.Now())
	if len(b.multiviewGroups) == 0 {
		b.nonGroupEpoch = nil
		return
	}
	key := ownerGateKey(viewerID, clientID)
	active := false
	for _, g := range b.multiviewGroups {
		if g.viewerID == viewerID && g.clientID == clientID {
			active = true
			break
		}
	}
	if !active {
		delete(b.nonGroupEpoch, key)
		return
	}
	if b.nonGroupEpoch == nil {
		b.nonGroupEpoch = make(map[string]uint64)
	}
	b.nonGroupEpoch[key]++
}

func livePlanContainer(caps core.ClientCaps) string {
	wantsFMP4 := false
	for _, c := range caps.HLSContainers {
		switch strings.ToLower(strings.TrimSpace(c)) {
		case "ts", "mpegts":
			return "mpegts"
		case "fmp4", "mp4", "m4s", "m4a":
			wantsFMP4 = true
		}
	}
	if wantsFMP4 {
		return "fmp4"
	}
	return "mpegts"
}

func liveVideoHeight(info *core.MediaInfo) int {
	if info == nil {
		return 0
	}
	if v := info.Video(); v != nil && v.Height > 0 {
		return v.Height
	}
	return 0
}

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

// DirectLiveBackend optionally supports tuning direct-URL live streams (such as
// plugin library live tabs) without registering a Live TV channel.
type DirectLiveBackend interface {
	LiveBackend
	DirectAllowed(context.Context, *core.Viewer, *frontend.DirectLiveTarget) error
	TuneDirect(context.Context, *core.Viewer, *frontend.DirectLiveTarget, core.ClientCaps, string) (string, *core.MediaInfo, error)
}

func (b *Broker) planLive(ctx context.Context, req frontend.PlanRequest, viewer *core.Viewer) (frontend.PlayPlan, error) {
	// Live TV is unaffected by the per-viewer "no video transcoding" switch,
	// matching the server-wide switch.
	req.Caps.NoVideoTranscode = false
	if b.deps.Live == nil {
		return frontend.PlayPlan{}, core.ErrNotAvailable
	}
	liveCtx := core.WithLiveApp(ctx, string(req.Protocol))
	if req.DirectLive != nil {
		db, ok := b.deps.Live.(DirectLiveBackend)
		if !ok {
			return frontend.PlayPlan{}, core.ErrNotAvailable
		}
		if err := db.DirectAllowed(liveCtx, viewer, req.DirectLive); err != nil {
			return frontend.PlayPlan{}, err
		}
	} else {
		if err := b.deps.Live.Allowed(liveCtx, viewer, req.Channel); err != nil {
			return frontend.PlayPlan{}, err
		}
	}
	if req.Intent.AllowTranscoding != nil && !*req.Intent.AllowTranscoding {
		return frontend.PlayPlan{}, core.ErrNotAvailable
	}
	baseMaxHeight := req.Caps.MaxHeight
	if req.Multiview != nil {
		if req.DirectLive != nil {
			return frontend.PlayPlan{}, frontend.ErrInvalidMultiview
		}
		b.mu.Lock()
		_, tileCount, err := b.validateMultiviewLocked(viewer.ID, req.Protocol, req.Device, req.Multiview, b.deps.Now())
		b.mu.Unlock()
		if err != nil {
			return frontend.PlayPlan{}, err
		}
		tileHeight := multiviewTileMaxHeight(baseMaxHeight, tileCount, req.Multiview.MaxHeight)
		req.Caps.MultiviewGroup = req.Multiview.GroupID
		req.Caps.MultiviewTile = req.Multiview.Tile
		req.Caps.MaxHeight = tileHeight
		req.Caps.ExplicitMaxHeight = tileHeight > 0
		req.Intent.Options.MaxHeight = tileHeight
	}
	container := livePlanContainer(req.Caps)
	info := core.MediaInfo{Container: container, Estimated: true}
	decision := core.Decision{Kind: core.DecisionRemux, Container: container, VideoCodec: "h264", AudioCodec: "aac", VideoCopy: true, AudioCopy: true, AudioChannels: 2, AudioStream: -1, SubtitleStream: -1, Reason: "live_remux_hls"}
	if req.Intent.Estimate {
		return frontend.PlayPlan{Estimated: true, Media: info, Decision: decision, Transcoding: true, HLS: true, HLSContainer: container}, nil
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
	b.plans[id] = brokerPlan{request: req, decision: decision, created: b.deps.Now(), live: true, baseMaxHeight: baseMaxHeight}
	return frontend.PlayPlan{ID: id, Media: info, Decision: decision, Transcoding: true, HLS: true, HLSContainer: container}, nil
}

func (b *Broker) openLive(ctx context.Context, req frontend.OpenRequest, p brokerPlan, viewer *core.Viewer, openEpoch uint64) (frontend.Session, error) {
	if b.deps.Live == nil {
		return frontend.Session{}, core.ErrNotAvailable
	}
	var mvSession *frontend.MultiviewTarget
	if p.request.Multiview != nil {
		b.mu.Lock()
		_, tileCount, err := b.validateMultiviewLocked(viewer.ID, req.Protocol, p.request.Device, p.request.Multiview, b.deps.Now())
		b.mu.Unlock()
		if err != nil {
			return frontend.Session{}, err
		}
		tileHeight := multiviewTileMaxHeight(p.baseMaxHeight, tileCount, p.request.Multiview.MaxHeight)
		p.request.Caps.MultiviewGroup = p.request.Multiview.GroupID
		p.request.Caps.MultiviewTile = p.request.Multiview.Tile
		p.request.Caps.MaxHeight = tileHeight
		p.request.Caps.ExplicitMaxHeight = tileHeight > 0
		p.request.Intent.Options.MaxHeight = tileHeight
		mvSession = &frontend.MultiviewTarget{
			GroupID:   p.request.Multiview.GroupID,
			Tile:      p.request.Multiview.Tile,
			TileCount: tileCount,
			MaxHeight: tileHeight,
		}
	}
	ctx = core.WithLiveApp(ctx, string(req.Protocol))
	if mvSession != nil {
		ctx = core.WithLiveHandover(ctx)
	}
	var liveID string
	var tunedMedia *core.MediaInfo
	var err error
	var directRef *frontend.DirectLiveTarget
	if p.request.DirectLive != nil {
		db, ok := b.deps.Live.(DirectLiveBackend)
		if !ok {
			return frontend.Session{}, core.ErrNotAvailable
		}
		if err = db.DirectAllowed(ctx, viewer, p.request.DirectLive); err != nil {
			return frontend.Session{}, err
		}
		liveID, tunedMedia, err = db.TuneDirect(ctx, viewer, p.request.DirectLive, p.request.Caps, p.request.Intent.ClientSessionID)
		if err != nil {
			return frontend.Session{}, capacityError(err)
		}
		// Retain only non-secret identity fields for ongoing policy re-checks.
		directRef = &frontend.DirectLiveTarget{
			LibraryID: p.request.DirectLive.LibraryID,
			Tab:       p.request.DirectLive.Tab,
			ItemID:    p.request.DirectLive.ItemID,
			Title:     p.request.DirectLive.Title,
		}
	} else {
		if err = b.deps.Live.Allowed(ctx, viewer, p.request.Channel); err != nil {
			return frontend.Session{}, err
		}
		if mb, ok := b.deps.Live.(LiveMediaBackend); ok {
			liveID, tunedMedia, err = mb.TuneMedia(ctx, viewer, p.request.Channel, p.request.Caps, p.request.Intent.ClientSessionID)
		} else {
			liveID, err = b.deps.Live.Tune(ctx, viewer, p.request.Channel, p.request.Caps, p.request.Intent.ClientSessionID)
		}
		if err != nil {
			return frontend.Session{}, capacityError(err)
		}
	}
	id, err := sessionID(req.Protocol)
	if err != nil {
		_ = b.deps.Live.Stop(ctx, viewer, liveID)
		return frontend.Session{}, err
	}
	liveMedia := normalizeLiveMedia(tunedMedia, livePlanContainer(p.request.Caps))
	grant := rand.Text()
	session := frontend.Session{ID: id, MediaPath: "/sessions/media/" + id + "/" + grant + "/live/index.m3u8", Protocol: req.Protocol, ViewerID: viewer.ID, Device: p.request.Device, Channel: p.request.Channel, Multiview: mvSession, Media: &liveMedia, StartedAt: b.deps.Now(), PlayMethod: "Transcode"}
	if req.Protocol == "silo" {
		session.ClientSessionID = p.request.Intent.ClientSessionID
	}
	if err := b.enrichCapabilities(ctx, &session); err != nil {
		_ = b.deps.Live.Stop(ctx, viewer, liveID)
		return frontend.Session{}, err
	}
	mvGroup := ""
	mvTile := 0
	if mvSession != nil {
		mvGroup = mvSession.GroupID
		mvTile = mvSession.Tile
		// Acquire the device owner gate only for the fast commit phase so
		// sibling tiles tune concurrently without blocking each other.
		ownerUnlock, gerr := b.gates.Acquire(ctx, ownerGateKey(viewer.ID, p.request.Caps.ClientID))
		if gerr != nil {
			_ = b.deps.Live.Stop(ctx, viewer, liveID)
			return frontend.Session{}, gerr
		}
		defer ownerUnlock()
	}
	s := &brokerSession{public: copySession(session), grant: grant, playID: liveID, clientID: p.request.Caps.ClientID, caps: p.request.Caps, opts: p.request.Intent.Options, decision: p.decision, live: true, directLive: directRef, multiviewGroup: mvGroup, multiviewTile: mvTile, baseMaxHeight: p.baseMaxHeight, knownVideoHeight: liveVideoHeight(tunedMedia)}
	if b.cluster != nil && !b.policy(session.Protocol).ExternalAdmission {
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
	if mvGroup != "" {
		ownerKey := ownerGateKey(viewer.ID, p.request.Caps.ClientID)
		if b.multiviewGroups[mvGroup] == nil || b.nonGroupEpoch[ownerKey] != openEpoch {
			b.mu.Unlock()
			if b.cluster != nil {
				_ = b.cluster.store.remove(ctx, id)
			}
			_ = b.deps.Live.Stop(ctx, viewer, liveID)
			return frontend.Session{}, &frontend.MultiviewError{Cause: core.ErrNotFound}
		}
	} else {
		b.bumpNonGroupEpochLocked(viewer.ID, s.clientID)
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

func normalizeLiveMedia(in *core.MediaInfo, defaultContainer string) core.MediaInfo {
	var out core.MediaInfo
	if in != nil {
		out = publicMedia(*in)
	}
	if out.Container == "" {
		if defaultContainer != "" {
			out.Container = defaultContainer
		} else {
			out.Container = "mpegts"
		}
	}
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

// LiveWindow forwards a previously authorized session's existing output clock.
func (b *Broker) LiveWindow(ctx context.Context, id string) (frontend.LiveWindow, error) {
	b.mu.Lock()
	s := b.sessions[id]
	if s == nil || !s.live || b.closed {
		b.mu.Unlock()
		return frontend.LiveWindow{}, core.ErrNotAvailable
	}
	playID := s.playID
	b.mu.Unlock()
	reader, ok := b.deps.Live.(frontend.LiveWindowReader)
	if !ok {
		return frontend.LiveWindow{}, core.ErrNotAvailable
	}
	return reader.LiveWindow(ctx, playID)
}
