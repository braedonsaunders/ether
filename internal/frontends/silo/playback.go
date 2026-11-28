package silo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/kit/caps"
	"github.com/braedonsaunders/ether/internal/frontend/kit/ids"
	"github.com/braedonsaunders/ether/internal/frontend/kit/mediasafety"
	"github.com/braedonsaunders/ether/internal/frontend/kit/quality"
)

func (i *instance) installationID() string {
	return i.id[:8] + "-" + i.id[8:12] + "-" + "4" + i.id[13:16] + "-" + "8" + i.id[17:20] + "-" + i.id[20:]
}

func installationChanged(w http.ResponseWriter) {
	problemReply(w, http.StatusConflict, map[string]any{"type": "/problems/installation_changed", "title": "Playback installation changed", "status": http.StatusConflict, "detail": "Refresh playback capabilities before retrying."})
}

type siloHDRDetails struct {
	HDR10           bool  `json:"hdr10"`
	HDR10Plus       bool  `json:"hdr10_plus"`
	HLG             bool  `json:"hlg"`
	DVProfiles      []int `json:"dolby_vision_profiles"`
	DVProfileLevels []struct {
		Profile int `json:"profile"`
	} `json:"dolby_vision_profile_levels"`
}

type startRequest struct {
	Context struct {
		Deliveries map[string]struct {
			Enabled     bool     `json:"enabled"`
			Supported   bool     `json:"supported_on_device"`
			Containers  []string `json:"containers"`
			VideoCodecs []string `json:"video_codecs"`
			// AudioDecode lists codecs the player decodes itself for this
			// delivery (for example ExoPlayer/FFmpeg software TrueHD/DTS on an
			// Android tablet without an AV receiver).
			AudioDecode []string `json:"audio_decode_codecs"`
			// AudioPassthrough lists codecs the device bitstreams to its
			// audio output (TrueHD, DTS-HD to an AV receiver) without decoding
			// them; codecs_audio lists only what it decodes.
			AudioPassthrough []string        `json:"audio_passthrough_codecs"`
			HDRDetails       *siloHDRDetails `json:"hdr_details"`
			Subtitles        struct {
				ASSStyling      bool `json:"ass_styling"`
				FontAttachments bool `json:"font_attachments"`
				SidecarText     bool `json:"sidecar_text"`
				// NativeEmbedded lists the container/codec pairs the player
				// demuxes and renders itself from the original file.
				NativeEmbedded []struct {
					Container string   `json:"container"`
					Codecs    []string `json:"codecs"`
					Identity  string   `json:"track_identity"`
				} `json:"native_embedded"`
			} `json:"subtitles"`
		} `json:"deliveries"`
	} `json:"client_playback_context"`
	Features         []string `json:"client_features"`
	FileID           string   `json:"file_id"`
	ProfileID        string   `json:"profile_id"`
	InstallationID   string   `json:"installation_id"`
	Attempt          string   `json:"playback_attempt_id"`
	Protocol         int      `json:"protocol_version"`
	Quality          string   `json:"quality_preference"`
	Position         float64  `json:"start_position"`
	Audio            *int     `json:"audio_track_index"`
	Subtitle         *int     `json:"subtitle_track_index"`
	SubtitleFidelity string   `json:"subtitle_fidelity_preference"`
	Capabilities     struct {
		Video            []string `json:"codecs_video"`
		Audio            []string `json:"codecs_audio"`
		Containers       []string `json:"containers"`
		HDR              bool     `json:"hdr"`
		Resolution       string   `json:"max_resolution"`
		AudioPassthrough struct {
			PassthroughCodecs []string `json:"passthrough_codecs"`
		} `json:"audio_passthrough"`
		HDRDetails *siloHDRDetails `json:"hdr_details"`
	} `json:"client_capabilities"`
}

func (i *instance) playbackStart(w http.ResponseWriter, r *http.Request) {
	v, _, err := i.deviceViewer(r)
	if err != nil || v == nil {
		http.Error(w, "Unauthorized", 401)
		return
	}
	if i.host.Sessions == nil || i.host.Capabilities == nil {
		http.Error(w, "Unavailable", 503)
		return
	}
	raw, err := io.ReadAll(http.MaxBytesReader(nil, r.Body, 1<<20))
	if err != nil {
		http.Error(w, "Bad Request", 400)
		return
	}
	var b startRequest
	if json.Unmarshal(raw, &b) != nil || b.Protocol != 3 || b.InstallationID == "" || b.Attempt == "" || b.Position < 0 || math.IsNaN(b.Position) || math.IsInf(b.Position, 0) || b.Position > float64((1<<63-1)/int64(time.Second)) {
		http.Error(w, "Bad Request", 400)
		return
	}
	if b.InstallationID != i.installationID() {
		installationChanged(w)
		return
	}
	if b.ProfileID != "" && b.ProfileID != i.viewerWireID(r.Context(), v.ID) {
		http.Error(w, "Forbidden", 403)
		return
	}
	d := device(r)
	height, _ := strconv.Atoi(strings.TrimSuffix(b.Capabilities.Resolution, "p"))
	if h, e := strconv.Atoi(strings.TrimSuffix(b.Quality, "p")); e == nil && h > 0 && (height == 0 || h < height) {
		height = h
	}
	// The original file plays any codec the device decodes (including player
	// software decoders listed under original_http.audio_decode_codecs on
	// tablets without an AV receiver) or bitstreams to a receiver.
	appendUnique := func(dst []string, items ...string) []string {
		for _, item := range items {
			if item = strings.ToLower(strings.TrimSpace(item)); item != "" && !slices.Contains(dst, item) {
				dst = append(dst, item)
			}
		}
		return dst
	}
	videoCodecs := appendUnique(nil, b.Capabilities.Video...)
	audioCodecs := appendUnique(nil, b.Capabilities.Audio...)
	audioCodecs = appendUnique(audioCodecs, b.Capabilities.AudioPassthrough.PassthroughCodecs...)
	containers := appendUnique(nil, b.Capabilities.Containers...)
	hdr10 := b.Capabilities.HDR
	var dvProfiles []int
	mergeHDR := func(h *siloHDRDetails) {
		if h == nil {
			return
		}
		if h.HDR10 || h.HDR10Plus || h.HLG {
			hdr10 = true
		}
		for _, p := range h.DVProfiles {
			if p > 0 && !slices.Contains(dvProfiles, p) {
				dvProfiles = append(dvProfiles, p)
			}
		}
		for _, pl := range h.DVProfileLevels {
			if pl.Profile > 0 && !slices.Contains(dvProfiles, pl.Profile) {
				dvProfiles = append(dvProfiles, pl.Profile)
			}
		}
	}
	mergeHDR(b.Capabilities.HDRDetails)
	if original, ok := b.Context.Deliveries["original_http"]; ok && original.Enabled && original.Supported {
		videoCodecs = appendUnique(videoCodecs, original.VideoCodecs...)
		audioCodecs = appendUnique(audioCodecs, original.AudioDecode...)
		audioCodecs = appendUnique(audioCodecs, original.AudioPassthrough...)
		containers = appendUnique(containers, original.Containers...)
		mergeHDR(original.HDRDetails)
	}
	cap, err := i.host.Capabilities.Build(i.identityContext(r), frontend.CapabilityRequest{Viewer: v, Device: caps.DeviceInfo{Protocol: "silo", ClientID: d.ID, Product: d.Client, Platform: d.Platform, Version: d.Version, DeviceName: d.DeviceName, VideoCodecs: videoCodecs, AudioCodecs: audioCodecs, Containers: containers, MaxHeight: height, HDR10: hdr10, DolbyVision: len(dvProfiles) > 0, DVProfiles: dvProfiles}, UserAgent: r.UserAgent()})
	if err != nil {
		http.Error(w, "Unavailable", 503)
		return
	}
	// A server HLS stream carries the selected embedded text track as a WebVTT
	// rendition, read with the picture instead of extracted from the file.
	cap.Client.HLSTextRenditions = slices.Contains(b.Features, "hls_subtitle_renditions_v1")
	scope := frontend.VersionScope{Viewer: v, Device: d, Protocol: "silo", Caps: cap.Client}
	if frontend.IsCatchUpGUID(b.FileID) {
		i.startCatchUp(w, r, v, b, cap.Client)
		return
	}
	// A channel GUID or live channel file_id tunes through the same live
	// session as Jellyfin and Plex instead of a catalogue file.
	channel, liveFile, err := i.resolveLiveFile(r, v, b.FileID)
	if err != nil {
		notFound(w)
		return
	}
	var offer frontend.SourceOffer
	var key frontend.RatingKey
	versionHandle := ""
	if !liveFile {
		if i.host.SourceOffers == nil {
			i.versionUnavailable(w, errors.New("source offers are not configured"), "source offers")
			return
		}
		offer, err = i.host.SourceOffers.Resolve(r.Context(), scope, b.FileID)
		if err != nil || offer.Target.Item == 0 || (offer.Target.VersionHandle == "") == (offer.Target.DetailSource == "") {
			if errors.Is(err, frontend.ErrNotFound) {
				http.NotFound(w, r)
				return
			}
			if err == nil {
				err = fmt.Errorf("offer %q item %d version %q detail %q", b.FileID, offer.Target.Item, offer.Target.VersionHandle, offer.Target.DetailSource)
			}
			i.versionUnavailable(w, err, "resolve source offer")
			return
		}
		key, versionHandle = offer.Target.Item, offer.Target.VersionHandle
	}
	planner, hasDetailPlanner := i.host.Sessions.(frontend.DetailPlanner)
	if (versionHandle != "" && i.host.VersionHandles == nil) || (offer.Target.DetailSource != "" && (!hasDetailPlanner || offer.Target.ETag == "")) {
		i.versionUnavailable(w, fmt.Errorf("version %q detail %q etag %q planner %v", versionHandle, offer.Target.DetailSource, offer.Target.ETag, hasDetailPlanner), "version handle")
		return
	}
	direct, hls := true, true
	if b.Context.Deliveries != nil {
		d, ok := b.Context.Deliveries["original_http"]
		direct = ok && d.Enabled && d.Supported
		h, ok := b.Context.Deliveries["hls"]
		hls = ok && h.Enabled && h.Supported
	}
	if !direct && !hls || (liveFile && !hls) {
		http.Error(w, "Playback unavailable", 422)
		return
	}
	options := frontend.DecideOptions{DirectPlay: direct, DirectStream: hls, AudioStream: -1, SubtitleStream: -1}
	if (b.Audio != nil && *b.Audio < 0) || (b.Subtitle != nil && *b.Subtitle < -1) {
		http.Error(w, "Bad Request", 400)
		return
	}
	pos := time.Duration(b.Position * float64(time.Second))
	if liveFile {
		// The live edge is the only start. A file position would seek a tuner.
		pos = 0
	} else {
		pos = i.skipIntroAtStart(r, v, scope, key, versionHandle, pos)
		b.Position = float64(pos) / float64(time.Second)
	}
	// The playback start is the viewer pressing Play.
	plan, err := i.host.Sessions.Plan(r.Context(), frontend.PlanRequest{Viewer: v, Device: d, Protocol: "silo", Item: key, Channel: channel, Version: versionHandle, DetailSource: offer.Target.DetailSource, ExpectedSourceETag: offer.Target.ETag, Caps: cap.Client, Intent: frontend.PlayIntent{ExactVersion: versionHandle != "", Options: options, AllowTranscoding: &hls, AudioOrdinal: b.Audio, SubtitleOrdinal: b.Subtitle, Position: pos, PlaybackID: b.Attempt, ClientSessionID: b.InstallationID, DeclaredCaps: channel == "" && len(videoCodecs) > 0, ViewerPlay: true}})
	if err != nil {
		if writePlaybackSourceProblem(w, err, r.Header.Get("Accept-Language")) {
			return
		}
		if !errors.Is(err, frontend.ErrSessionBusy) && !errors.Is(err, frontend.ErrTranscodeCapacity) && !errors.Is(err, frontend.ErrTrackNotFound) && !errors.Is(err, frontend.ErrTrackSourceUnavailable) && !errors.Is(err, frontend.ErrVideoTranscodingDisabled) && !errors.As(err, new(*frontend.RequiredAudioLanguageError)) {
			i.versionUnavailable(w, err, "plan")
			return
		}
		i.playbackError(w, v, err, r.Header.Get("Accept-Language"))
		return
	}
	session, err := i.host.Sessions.Open(r.Context(), frontend.OpenRequest{Protocol: "silo", PlanID: plan.ID, Viewer: v, Device: d})
	if err != nil {
		if writePlaybackSourceProblem(w, err, r.Header.Get("Accept-Language")) {
			return
		}
		if !errors.Is(err, frontend.ErrSessionBusy) && !errors.Is(err, frontend.ErrTranscodeCapacity) && !errors.Is(err, frontend.ErrTrackNotFound) && !errors.Is(err, frontend.ErrTrackSourceUnavailable) && !errors.Is(err, frontend.ErrVideoTranscodingDisabled) && !errors.As(err, new(*frontend.RequiredAudioLanguageError)) {
			i.versionUnavailable(w, err, "open")
			return
		}
		i.playbackError(w, v, err, r.Header.Get("Accept-Language"))
		return
	}
	if liveFile {
		hydrateLivePlan(&plan, session.Media)
	}
	access := frontend.SessionAccess{Viewer: v, Device: d, Protocol: "silo"}
	matchOffer := func() error {
		if liveFile {
			return nil
		}
		current, err := i.host.SourceOffers.Resolve(r.Context(), scope, b.FileID)
		if err != nil {
			return err
		}
		if current.ID != offer.ID || current.Target != offer.Target {
			return frontend.ErrSourceGone
		}
		if offer.Target.DetailSource != "" {
			return planner.MatchDetailSession(r.Context(), scope, offer.Target.DetailSource, session.ID)
		}
		return i.host.VersionHandles.MatchSession(r.Context(), scope, versionHandle, session.ID)
	}
	if err = matchOffer(); err != nil {
		i.stopFailedAdmission(r.Context(), access, session.ID)
		i.versionUnavailable(w, err, "match offer")
		return
	}

	format := frontend.StreamDirect
	delivery := "original_http"
	proto := "http_progressive"
	mime := "video/" + plan.Media.Container
	if liveFile || plan.Decision.Kind != "directplay" {
		format = frontend.StreamHLS
		delivery = "server_transcode_hls"
		if liveFile || plan.Decision.Kind == "copy" {
			delivery = "server_remux_hls"
		}
		proto = "hls"
		mime = "application/vnd.apple.mpegurl"
	}
	media, err := i.host.Sessions.StartStream(r.Context(), access, session.ID, frontend.StreamRequest{Format: format, Position: pos})
	if err != nil {
		i.stopFailedAdmission(r.Context(), access, session.ID)
		i.playbackError(w, v, err, r.Header.Get("Accept-Language"))
		return
	}
	// Admission finishes only after stream preparation and one last exact
	// selector check. Later expiry does not revoke the independent session.
	if err = matchOffer(); err != nil {
		i.stopFailedAdmission(r.Context(), access, session.ID)
		i.versionUnavailable(w, err, "match offer after stream")
		return
	}
	if !strings.HasPrefix(media, "/sessions/media/") {
		i.stopFailedAdmission(r.Context(), access, session.ID)
		http.Error(w, "Playback unavailable", 503)
		return
	}
	media = siloMediaURL(session.ID, format)
	source := map[string]any{"media_file_id": b.FileID, "hdr10_plus": false, "dv_enhancement_layer": "unknown"}
	if !plan.Media.Estimated {
		if plan.Media.Duration > 0 {
			source["duration_seconds"] = plan.Media.Duration.Seconds()
		}
		if plan.Media.Container != "" {
			source["container"] = plan.Media.Container
		}
		if plan.Media.BitrateKbps > 0 {
			source["bitrate_kbps"] = plan.Media.BitrateKbps
		}
	}
	recipe := map[string]any{"video_codec": plan.Decision.VideoCodec, "audio_codec": plan.Decision.AudioCodec, "bitrate_kbps": plan.Decision.BitrateKbps}
	hdr10, hdr10Plus, hlg, dv := false, false, false, false
	sourceHeight := 0
	streams := plan.Media.Streams
	if plan.Media.Estimated {
		streams = nil
	}
	for _, s := range streams {
		if s.Type == "video" {
			rangeName := siloObservedRange(plan.Media, s)
			if !plan.Media.Estimated && s.HDR10Plus != nil {
				source["hdr10_plus"] = *s.HDR10Plus
			}
			if !plan.Media.Estimated && s.DolbyVisionELPresent != nil && !*s.DolbyVisionELPresent {
				source["dv_enhancement_layer"] = "none"
			}
			if plan.Decision.VideoCopy && !plan.Decision.ToneMap {
				hdr10 = rangeName == "hdr10" || rangeName == "hdr10_plus"
				hdr10Plus = !plan.Media.Estimated && s.HDR10Plus != nil && *s.HDR10Plus
				hlg = rangeName == "hlg"
				dv = rangeName == "dolby_vision" && !plan.Decision.StripDolbyVision
			}
			source["video_codec"] = s.Codec
			source["width"] = s.Width
			source["height"] = s.Height
			if s.Height > sourceHeight {
				sourceHeight = s.Height
			}
			source["bit_depth"] = s.BitDepth
			source["video_profile"] = s.Profile
			source["video_level"] = s.Level
			source["frame_rate"] = s.FrameRate
			source["color_range"] = "unknown"
			if s.ColorRange != "" {
				source["color_range"] = s.ColorRange
			}
			if rangeName != "" {
				source["dynamic_range"] = rangeName
			}
			if plan.Decision.VideoCopy {
				if s.Width > 0 {
					recipe["width"] = s.Width
				}
				if s.Height > 0 {
					recipe["height"] = s.Height
				}
				if s.FrameRate > 0 {
					recipe["frame_rate"] = s.FrameRate
				}
				if rangeName != "" {
					recipe["dynamic_range"] = rangeName
				}
			} else {
				if plan.Decision.MaxHeight > 0 {
					recipe["height"] = plan.Decision.MaxHeight
					if s.Width > 0 && s.Height > 0 {
						recipe["width"] = s.Width * plan.Decision.MaxHeight / s.Height
					}
				}
				if s.FrameRate > 0 {
					recipe["frame_rate"] = s.FrameRate
				}
				if rangeName != "" {
					recipe["dynamic_range"] = rangeName
				}
				if plan.Decision.ToneMap {
					recipe["dynamic_range"] = "sdr"
				}
			}
		}
		if s.Type == "audio" && s.Index == plan.Decision.AudioStream {
			source["audio_codec"] = s.Codec
			source["audio_channels"] = s.Channels
			if s.ChannelLayout != "" {
				source["audio_layout"] = s.ChannelLayout
			}
			if plan.Decision.AudioCopy {
				if s.Channels > 0 {
					recipe["audio_channels"] = s.Channels
				}
				if s.ChannelLayout != "" {
					recipe["audio_layout"] = s.ChannelLayout
				}
			} else if plan.Decision.AudioChannels > 0 {
				recipe["audio_channels"] = plan.Decision.AudioChannels
			}
		}
	}
	selected, subtitle, fidelityPolicy, requireAuthoredFidelity, assStylingPreserved := i.siloTracks(siloTrackInputs{r: r, viewer: v, access: access, sessionID: session.ID, start: b, media: plan.Media, format: format, delivery: delivery, live: liveFile, hlsRenditions: cap.Client.HLSTextRenditions, audioStream: plan.Decision.AudioStream, subtitleStream: plan.SubtitleStream(), burn: plan.Decision.BurnSubtitles})
	stream := map[string]any{"url": media, "protocol": proto, "container": plan.Media.Container, "mime_type": mime, "headers": map[string]string{"X-Silo-Device-Id": d.ID}, "header_refresh": "none"}
	if format == frontend.StreamHLS {
		stream["container"] = "hls"
	}
	timeline := map[string]any{"source_start_seconds": b.Position, "stream_origin_seconds": 0, "player_start_seconds": b.Position, "timeline_offset_seconds": 0, "can_seek_anywhere": true, "seek_restoration": "player_position"}
	if format == frontend.StreamHLS {
		timeline["stream_origin_seconds"] = b.Position
		timeline["player_start_seconds"] = 0
		timeline["timeline_offset_seconds"] = b.Position
	}
	if liveFile {
		timeline["source_start_seconds"] = float64(0)
		timeline["stream_origin_seconds"] = float64(0)
		timeline["player_start_seconds"] = float64(0)
		timeline["timeline_offset_seconds"] = float64(0)
		timeline["can_seek_anywhere"] = false
	}
	audioClaim := map[string]any{"passthrough": false, "atmos_preserved": false}
	if plan.Decision.AudioCodec != "" {
		audioClaim["codec"] = plan.Decision.AudioCodec
	}
	if plan.Decision.Reason != "" {
		audioClaim["reason"] = plan.Decision.Reason
	}
	qualities := siloQualityMenu(sourceHeight)
	encodeHeight := sourceHeight
	if !plan.Decision.VideoCopy && plan.Decision.MaxHeight > 0 {
		encodeHeight = plan.Decision.MaxHeight
	}
	output := map[string]any{"protocol_version": 3, "plan_id": plan.ID, "session_id": session.ID, "delivery": delivery, "plan_attempt_key": plan.ID, "stream": stream, "timeline": timeline, "selected_tracks": selected, "effective_recipe": recipe, "claims": map[string]any{"video": map[string]any{"hdr10": hdr10, "hdr10_plus": hdr10Plus, "hlg": hlg, "dolby_vision": dv}, "audio": audioClaim, "subtitles": map[string]any{"ass_styling_preserved": assStylingPreserved, "bitmap_overlay": false, "bitmap_sidecar": false}}, "subtitle": subtitle, "transformations": siloTransformations(plan), "applied_quirks": []any{}, "runtime_corrections": []any{}, "degradation_warnings": []any{}, "decision_reason": plan.Decision.Reason, "requested_media_file_id": b.FileID, "effective_media_file_id": b.FileID, "source": source, "subtitle_fidelity_policy": fidelityPolicy, "available_qualities": qualities}
	if active := siloActiveQuality(b.Quality, qualities, plan.Decision.VideoCopy, encodeHeight); active != "" {
		output["active_quality"] = active
	}
	if liveFile {
		live := (frontend.HostConfig{}).LiveTV
		if i.host.HostConfig != nil {
			live = i.host.HostConfig().LiveTV
		}
		output["livePolicy"] = live.LatencyPolicy()
	}
	document := map[string]any{"outcome": "playable", "protocol_version": 3, "server_features": siloServerFeatures, "session_id": session.ID, "playback_plan": output}
	i.rememberPlan(session.ID, playbackPlan{format: format, live: liveFile, baseID: plan.ID, delivery: delivery, requireAuthored: requireAuthoredFidelity, body: document, start: raw, media: plan.Media, hlsRenditions: cap.Client.HLSTextRenditions, audioCopy: plan.Decision.AudioCopy})
	reply(w, 201, document)
}

func hydrateLivePlan(plan *frontend.PlayPlan, sessionMedia *frontend.MediaInfo) {
	if sessionMedia != nil && len(sessionMedia.Streams) > 0 {
		plan.Media = *sessionMedia
	} else if len(plan.Media.Streams) == 0 {
		no := false
		eight := 8
		plan.Media.Container = "mpegts"
		plan.Media.Streams = []frontend.StreamInfo{
			{Index: 0, Type: "video", Codec: "h264", Width: 1920, Height: 1080, BitDepth: 8, BitsPerRawSample: &eight, PixelFormat: "yuv420p", ColorTransfer: "bt709", ColorSpace: "bt709", ColorPrimaries: "bt709", SampleAspectRatio: "1:1", DisplayAspectRatio: "16:9", Default: true, DolbyVisionConfigurationPresent: &no, HDR10Plus: &no},
			{Index: 1, Type: "audio", Codec: "aac", Channels: 2, SampleRate: 48000, ChannelLayout: "stereo", Default: true},
		}
	}
	plan.Media.Estimated = false
	if plan.Media.Container == "" {
		plan.Media.Container = "mpegts"
	}
	plan.Decision.VideoCopy = true
	plan.Decision.AudioCopy = true
	for _, s := range plan.Media.Streams {
		if s.Type == "video" && s.Codec != "" {
			plan.Decision.VideoCodec = s.Codec
		}
		if s.Type == "audio" && plan.Decision.AudioStream < 0 {
			plan.Decision.AudioStream = s.Index
			if s.Codec != "" {
				plan.Decision.AudioCodec = s.Codec
			}
			if s.Channels > 0 {
				plan.Decision.AudioChannels = s.Channels
			}
		}
	}
	if plan.Decision.VideoCodec == "" {
		plan.Decision.VideoCodec = "h264"
	}
	if plan.Decision.AudioCodec == "" {
		plan.Decision.AudioCodec = "aac"
	}
	if plan.Decision.Reason == "" {
		plan.Decision.Reason = "live_remux_hls"
	}
}

// siloMediaURL is the only stream address the Apple TV client will play.
// Direct play is the session itself. HLS, including remux and transcode, is
// the session master. A grant in the path is rejected before the request.
// resolveLiveFile maps an On Now play_content_id or live channel file_id to
// the neutral channel id. Any other file id is a catalogue offer. A channel id
// the viewer cannot tune is not a catalogue miss.
func (i *instance) resolveLiveFile(r *http.Request, v *frontend.Viewer, fileID string) (string, bool, error) {
	channelGUID := ""
	if _, err := ids.ParseGUID(ids.Channel, fileID); err == nil {
		channelGUID = fileID
	} else if isLiveChannelFileID(fileID) {
		if chID, guid := i.resolveLiveChannelFileID(r.Context(), v, fileID); chID != "" {
			return chID, true, nil
		} else if guid != "" {
			channelGUID = guid
		} else {
			return "", false, frontend.ErrNotFound
		}
	} else {
		return "", false, nil
	}
	if i.host.LiveTV == nil {
		return "", false, frontend.ErrNotFound
	}
	ch, err := i.host.LiveTV.ResolveChannelGUID(r.Context(), v, channelGUID)
	if err != nil || ch.ID == "" {
		if err == nil {
			err = frontend.ErrNotFound
		}
		return "", false, err
	}
	return ch.ID, true, nil
}

func (i *instance) resolveLiveChannelFileID(ctx context.Context, _ *frontend.Viewer, fileID string) (string, string) {
	n, err := ids.ParseDecimal(fileID)
	if err != nil || !isLiveChannelFileID(fileID) {
		return "", ""
	}
	guid, err := i.host.IDs.Resolve(ctx, "silo", "live-channel", int32(n-liveChannelFileBit))
	if err != nil {
		return "", ""
	}
	if _, err = ids.ParseGUID(ids.Channel, guid); err != nil {
		return "", ""
	}
	return "", guid
}

func siloMediaURL(sessionID string, format frontend.StreamFormat) string {
	if format == frontend.StreamHLS {
		return "/api/v2/playback/transcode/" + sessionID + "/master.m3u8"
	}
	return "/api/v2/stream/" + sessionID
}

const (
	maxPlaybackPlans = 4096
	playbackPlanTTL  = 24 * time.Hour
)

type playbackPlan struct {
	lastUsed        time.Time
	replacement     string
	format          frontend.StreamFormat
	live            bool
	baseID          string
	delivery        string
	requireAuthored bool
	body            map[string]any
	// start is the start request the session was admitted with. A track or
	// quality change is planned again from it with the new selection.
	start []byte
	// media, hlsRenditions and audioCopy are what a track pick re-plans
	// against within the same session.
	media         frontend.MediaInfo
	hlsRenditions bool
	audioCopy     bool
}

func (i *instance) rememberPlan(id string, plan playbackPlan) {
	i.plansMu.Lock()
	defer i.plansMu.Unlock()
	now := time.Now()
	if i.plans == nil {
		i.plans = map[string]playbackPlan{}
	}
	var oldestID string
	var oldestAt time.Time
	for cachedID, cached := range i.plans {
		if now.Sub(cached.lastUsed) >= playbackPlanTTL {
			delete(i.plans, cachedID)
		} else if oldestAt.IsZero() || cached.lastUsed.Before(oldestAt) {
			oldestID, oldestAt = cachedID, cached.lastUsed
		}
	}
	if _, exists := i.plans[id]; !exists && len(i.plans) >= maxPlaybackPlans {
		delete(i.plans, oldestID)
	}
	plan.lastUsed = now
	i.plans[id] = plan
}

func (i *instance) updatePlan(id string, plan playbackPlan) {
	i.plansMu.Lock()
	defer i.plansMu.Unlock()
	// A replan can finish after a stop has removed the session's plan.
	if _, ok := i.plans[id]; ok {
		plan.lastUsed = time.Now()
		i.plans[id] = plan
	}
}

func (i *instance) rememberedPlan(id string) (playbackPlan, bool) {
	i.plansMu.Lock()
	defer i.plansMu.Unlock()
	plan, ok := i.plans[id]
	if !ok {
		return playbackPlan{}, false
	}
	now := time.Now()
	if now.Sub(plan.lastUsed) >= playbackPlanTTL {
		delete(i.plans, id)
		return playbackPlan{}, false
	}
	plan.lastUsed = now
	i.plans[id] = plan
	return plan, true
}

func (i *instance) forgetPlan(id string) {
	i.plansMu.Lock()
	defer i.plansMu.Unlock()
	delete(i.plans, id)
}

// siloQualityMenu lists Ether's default quality profiles (config.DefaultQuality)
// strictly below the source. The source itself is not a menu entry.
func siloQualityMenu(sourceHeight int) []any {
	type rung struct {
		label  string
		height int
	}
	out := []any{}
	for _, profile := range []rung{{"4K", 2160}, {"1080p", 1080}, {"720p", 720}} {
		if sourceHeight > profile.height {
			out = append(out, map[string]any{
				"label":            profile.label,
				"display_name":     profile.label,
				"height":           profile.height,
				"bitrate_kbps":     quality.ProfileVideoKbps(profile.height),
				"preserves_source": false,
			})
		}
	}
	return out
}

// siloTransformations lists operations this encode actually runs. A direct
// copy has none. Names are Ether's, not another server's rung labels.
func siloTransformations(plan frontend.PlayPlan) []any {
	out := []any{}
	add := func(name string, claims ...string) {
		if len(claims) == 0 {
			claims = []string{name}
		}
		out = append(out, map[string]any{"name": name, "executor": "ffmpeg", "recipe_version": "1", "validated_claims": claims})
	}
	var video *frontend.StreamInfo
	for i := range plan.Media.Streams {
		stream := &plan.Media.Streams[i]
		if stream.Type == "video" && video == nil {
			video = stream
		}
	}
	if !plan.Decision.VideoCopy {
		claims := []string{}
		if video != nil && video.Codec != "" {
			claims = append(claims, video.Codec+"_decode")
		}
		if plan.Decision.ToneMap {
			claims = append(claims, "tone_mapped_to_sdr")
		}
		add("video_transcode", claims...)
	}
	if plan.Decision.BurnSubtitles {
		add("subtitle_burn_in", "subtitles_burned_in")
	}
	if !plan.Decision.AudioCopy {
		claims := []string{"audio_decode"}
		if plan.Decision.AudioChannels > 0 {
			for _, stream := range plan.Media.Streams {
				if stream.Type == "audio" && stream.Index == plan.Decision.AudioStream && stream.Channels > plan.Decision.AudioChannels {
					claims = append(claims, "fewer_channels")
				}
			}
		}
		add("audio_transcode", claims...)
	}
	return out
}

// siloActiveQuality names the menu entry this encode delivers. Auto and an
// unchanged source copy name no entry. Ether has no "720p-medium" profile.
func siloActiveQuality(preference string, menu []any, videoCopy bool, encodeHeight int) string {
	if videoCopy || preference == "" || preference == "auto" {
		return ""
	}
	for _, entry := range menu {
		row, _ := entry.(map[string]any)
		label, _ := row["label"].(string)
		height, _ := row["height"].(int)
		if label == preference && height == encodeHeight {
			return label
		}
	}
	return ""
}

func (i *instance) stopFailedAdmission(ctx context.Context, access frontend.SessionAccess, sessionID string) {
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Second)
	defer cancel()
	i.forgetPlan(sessionID)
	if err := i.host.Sessions.Stop(cleanup, access, sessionID); err != nil && i.host.Log != nil {
		i.host.Log.Error("failed playback admission cleanup", "session", sessionID, "err", err)
	}
}

func (i *instance) versionUnavailable(w http.ResponseWriter, err error, reason string) {
	if i.host.Log != nil {
		i.host.Log.Error("silo version unavailable", "reason", reason, "err", err)
	}
	versionUnavailable(w)
}

func versionUnavailable(w http.ResponseWriter) {
	requestID, err := controlCommandID()
	if err != nil {
		http.Error(w, "Playback unavailable", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("X-Request-ID", requestID)
	problemReply(w, http.StatusServiceUnavailable, map[string]any{"type": "urn:ether:problem:version-unavailable", "title": "Selected version unavailable", "status": http.StatusServiceUnavailable, "detail": "The selected version cannot be served for this playback request.", "instance": "urn:silo:request:" + requestID})
}

type progressRequest struct {
	InstallationID string   `json:"installation_id"`
	Position       *float64 `json:"position"`
	Sequence       uint64   `json:"sequence"`
	Paused         *bool    `json:"is_paused"`
	StopID         string   `json:"stop_id"`
}

func (i *instance) progress(w http.ResponseWriter, r *http.Request) {
	v, _, err := i.deviceViewer(r)
	if err != nil || v == nil {
		http.Error(w, "Unauthorized", 401)
		return
	}
	if i.host.Sessions == nil {
		http.Error(w, "Unavailable", 503)
		return
	}
	var b progressRequest
	if decode(r, &b) != nil || b.InstallationID == "" {
		http.Error(w, "Bad Request", 400)
		return
	}
	if b.InstallationID != i.installationID() {
		installationChanged(w)
		return
	}
	stop := r.Method == "DELETE"
	if stop && b.StopID == "" {
		http.Error(w, "Bad Request", 400)
		return
	}
	access := frontend.SessionAccess{Viewer: v, Device: device(r), Protocol: "silo"}
	sessionID := r.PathValue("session")
	if info, ok := i.catchUpSession(r, v, sessionID); ok {
		// The airing's watch state is saved after the tune is released or
		// kept alive; it does not need the session.
		if stop {
			if err := i.host.LiveTV.Stop(r.Context(), sessionID, v); err == nil {
				i.forgetPlan(sessionID)
			}
			i.catchUpProgress(r, v, info, b.Position, b.Paused, true)
			reply(w, 200, map[string]any{"outcome": "stopped", "stop_id": b.StopID})
		} else if b.Sequence == 0 || b.Position == nil || b.Paused == nil || *b.Position < 0 {
			http.Error(w, "Bad Request", 400)
		} else if archive, ok := i.host.LiveTV.(frontend.LiveCatchUp); ok {
			if err := archive.TouchLiveSession(r.Context(), v, sessionID); err != nil {
				i.playbackError(w, v, err, r.Header.Get("Accept-Language"))
				return
			}
			i.catchUpProgress(r, v, info, b.Position, b.Paused, false)
			reply(w, 200, map[string]any{"outcome": "applied", "accepted": map[string]any{"position": *b.Position, "sequence": b.Sequence, "is_paused": *b.Paused}})
		}
		return
	}
	hasSample := b.Sequence != 0 || b.Position != nil || b.Paused != nil
	if !hasSample && stop {
		if err = i.host.Sessions.Stop(r.Context(), access, sessionID); err != nil {
			http.Error(w, "Unavailable", 503)
			return
		}
		i.forgetPlan(sessionID)
		reply(w, 200, map[string]any{"outcome": "stopped", "stop_id": b.StopID})
		return
	}
	if b.Sequence == 0 || b.Position == nil || b.Paused == nil || *b.Position < 0 || math.IsNaN(*b.Position) || math.IsInf(*b.Position, 0) || *b.Position > float64((1<<63-1)/int64(time.Second)) {
		http.Error(w, "Bad Request", 400)
		return
	}
	reporter, ok := i.host.Sessions.(frontend.SampleReporter)
	if !ok {
		http.Error(w, "Unavailable", 503)
		return
	}
	outcome, err := reporter.ReportSample(r.Context(), access, sessionID, frontend.ProgressSample{InstallationID: b.InstallationID, Sequence: b.Sequence, PositionSeconds: *b.Position, Paused: *b.Paused, Stop: stop})
	if err != nil {
		playbackError(w, err)
		return
	}
	if outcome == frontend.ReportConflict {
		reply(w, 409, map[string]any{"outcome": outcome})
		return
	}
	response := map[string]any{"outcome": outcome}
	if outcome == frontend.ReportApplied || outcome == frontend.ReportReplayed {
		response["accepted"] = map[string]any{"position": *b.Position, "sequence": b.Sequence, "is_paused": *b.Paused}
	}
	if outcome == frontend.ReportApplied || outcome == frontend.ReportReplayed {
		if stop {
			i.forgetPlan(sessionID)
			response["outcome"] = "stopped"
		} else {
			_, _ = i.rememberedPlan(sessionID)
		}
	}
	if b.StopID != "" {
		response["stop_id"] = b.StopID
	}
	reply(w, 200, response)
}

func playbackError(w http.ResponseWriter, err error) {
	playbackErrorFor(w, nil, nil, err)
}

func (i *instance) playbackError(w http.ResponseWriter, v *frontend.Viewer, err error, languages ...string) {
	var policy any
	if i != nil {
		policy = i.host.ViewerPolicy
	}
	playbackErrorFor(w, policy, v, err, languages...)
}

func writePlaybackSourceProblem(w http.ResponseWriter, err error, languages ...string) bool {
	code, status, msg := frontend.PlaybackSourceProblem(err, languages...)
	if msg == "" {
		return false
	}
	w.Header().Set("X-Ether-Media-Error", code)
	w.Header().Set("Cache-Control", "no-store")
	problemReply(w, status, map[string]any{
		"type": "about:blank", "title": msg, "status": status,
	})
	return true
}

func playbackErrorFor(w http.ResponseWriter, policy any, viewer *frontend.Viewer, err error, languages ...string) {
	if writePlaybackSourceProblem(w, err, languages...) {
		return
	}
	if errors.Is(err, frontend.ErrVideoTranscodingDisabled) {
		http.Error(w, frontend.VideoTranscodingOffMessage(policy, viewer), http.StatusUnprocessableEntity)
		return
	}
	var reqLang *frontend.RequiredAudioLanguageError
	if errors.As(err, &reqLang) {
		http.Error(w, reqLang.Error(), http.StatusUnprocessableEntity)
		return
	}
	if errors.Is(err, frontend.ErrArchiveUnstreamable) {
		http.Error(w, "This archive format or compression method cannot be streamed.", http.StatusUnprocessableEntity)
		return
	}
	if errors.Is(err, frontend.ErrReleaseRejected) || errors.Is(err, frontend.ErrArticleMissing) || errors.Is(err, frontend.ErrArticleCorrupt) {
		http.Error(w, "The source is incomplete or cannot be played.", http.StatusUnprocessableEntity)
		return
	}
	if errors.Is(err, frontend.ErrTrackNotFound) {
		http.Error(w, "Selected track unavailable", http.StatusUnprocessableEntity)
		return
	}
	if errors.Is(err, frontend.ErrTrackSourceUnavailable) {
		http.Error(w, "Track source unavailable", http.StatusServiceUnavailable)
		return
	}
	if code, msg, seconds := frontend.PlaybackPending(err, languages...); seconds > 0 {
		frontend.SetPlaybackRetry(w, code, seconds)
		requestID, idErr := controlCommandID()
		if idErr != nil {
			http.Error(w, msg, http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("X-Request-ID", requestID)
		problemReply(w, http.StatusServiceUnavailable, map[string]any{"type": "about:blank", "title": msg, "status": http.StatusServiceUnavailable, "detail": msg, "instance": "urn:silo:request:" + requestID})
		return
	}
	if errors.Is(err, frontend.ErrSessionBusy) {
		var busy *frontend.SessionBusyError
		if errors.As(err, &busy) && busy.RetryAfter > 0 {
			w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(busy.RetryAfter.Seconds()))))
		}
		requestID, idErr := controlCommandID()
		if idErr != nil {
			http.Error(w, "Playback unavailable", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("X-Request-ID", requestID)
		problemReply(w, http.StatusServiceUnavailable, map[string]any{"type": "about:blank", "title": "Playback temporarily unavailable", "status": 503, "detail": "Playback admission is busy; retry later.", "instance": "urn:silo:request:" + requestID})
		return
	}
	if errors.Is(err, frontend.ErrTranscodeCapacity) {
		var cap *frontend.TranscodeCapacityError
		if errors.As(err, &cap) && cap.RetryAfter > 0 {
			w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(cap.RetryAfter.Seconds()))))
		}
		requestID, idErr := controlCommandID()
		if idErr != nil {
			http.Error(w, "Playback unavailable", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("X-Request-ID", requestID)
		detail := frontend.VideoEncoderCapacityMessage(err, languages...)
		if detail == "" {
			detail = "The server is busy; retry later."
		}
		problemReply(w, http.StatusServiceUnavailable, map[string]any{"type": "https://siloserver.org/docs/api/v2/problems/transcode_capacity", "title": "Transcode capacity unavailable", "status": 503, "detail": detail, "instance": "urn:silo:request:" + requestID})
		return
	}
	http.Error(w, "Playback unavailable", 503)
}

func (i *instance) playbackCapabilities(w http.ResponseWriter, r *http.Request) {
	v, _, err := i.viewer(r)
	if err != nil || v == nil {
		http.Error(w, "Unauthorized", 401)
		return
	}
	state := "available"
	_, hasDeviceIdentity := i.host.Identity.(frontend.DeviceIdentity)
	_, hasScopedMedia := i.host.Sessions.(frontend.ScopedMedia)
	allowed := i.host.Sessions != nil && i.host.Capabilities != nil && hasDeviceIdentity && hasScopedMedia
	if !allowed {
		state = "not_configured"
	}
	features := slices.Clone(siloServerFeatures)
	if i.host.WatchParty != nil {
		features = append(features, "watch_party_coordinator_v1", "fixed_media_file_v1")
	}
	reply(w, 200, map[string]any{"revision": "ether-playback-v3", "state": state, "allowed": allowed, "installation_id": i.installationID(), "protocol_versions": []int{3}, "features": features, "deliveries": []string{"original_http", "server_transcode_hls", "server_remux_hls"}})
}

func (i *instance) playbackControlCapabilities(w http.ResponseWriter, r *http.Request) {
	v, _, err := i.viewer(r)
	if err != nil || v == nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	_, hasDeviceIdentity := i.host.Identity.(frontend.DeviceIdentity)
	available := i.host.Sessions != nil && i.host.Events != nil && i.host.Tickets != nil && hasDeviceIdentity
	state, protocol := "not_configured", ""
	if available {
		state, protocol = "available", controlProtocol
	}
	reply(w, http.StatusOK, map[string]any{"available": available, "protocol": protocol, "revision": "ether-playback-control-v1", "state": state, "allowed": available})
}

type sharedPrefix struct{}

type replanRequest struct {
	InstallationID string   `json:"installation_id"`
	Protocol       int      `json:"protocol_version"`
	Operation      string   `json:"operation"`
	Attempt        string   `json:"playback_attempt_id"`
	ReplanID       string   `json:"replan_request_id"`
	FailedPlan     string   `json:"failed_plan_id"`
	PlanAttempt    string   `json:"plan_attempt_id"`
	PlanKey        string   `json:"plan_attempt_key"`
	AttemptCount   int      `json:"attempt_count"`
	Quality        string   `json:"quality_preference"`
	Position       float64  `json:"position_seconds"`
	Attempted      []string `json:"attempted_plan_keys"`
	Selected       struct {
		Audio    *replanTrack `json:"audio"`
		Subtitle *replanTrack `json:"subtitle"`
	} `json:"selected_tracks"`
}

type replanTrack struct {
	ID    string `json:"id"`
	Index *int   `json:"index"`
}

// intentReplan reports a user's choice (a track or quality pick), not a route
// failure. It carries no attempt history: the playing route stays valid.
func intentReplan(operation string) bool {
	return operation == "track_change" || operation == "quality_change"
}

// nonFailureReplan reports an operation that is not a playback route failure
// and therefore may legitimately carry an empty attempted_plan_keys list.
func nonFailureReplan(operation string) bool {
	return intentReplan(operation) || operation == "seek_reanchor" || operation == "output_change"
}

type replanRoute struct {
	delivery string
	format   frontend.StreamFormat
	key      string
	reason   string
}

func (i *instance) playbackReplan(w http.ResponseWriter, r *http.Request) {
	v, _, err := i.deviceViewer(r)
	if err != nil || v == nil {
		http.Error(w, "Unauthorized", 401)
		return
	}
	if i.host.Sessions == nil {
		http.Error(w, "Unavailable", 503)
		return
	}
	raw, err := io.ReadAll(http.MaxBytesReader(nil, r.Body, 1<<20))
	if err != nil {
		problem(w, http.StatusBadRequest, "urn:ether:problem:validation-failed", "Validation failed", "The replan request is not readable.")
		return
	}
	var b replanRequest
	if err := json.Unmarshal(raw, &b); err != nil {
		problem(w, http.StatusBadRequest, "urn:ether:problem:validation-failed", "Validation failed", "The replan request is missing a required field.")
		return
	}
	if b.Quality == "" {
		b.Quality = "auto"
	}
	if b.Protocol != 3 || b.InstallationID == "" || b.Attempt == "" || b.ReplanID == "" || b.FailedPlan == "" || b.PlanAttempt == "" || b.PlanKey == "" || b.AttemptCount < 1 || (len(b.Attempted) == 0 && !nonFailureReplan(b.Operation)) || b.Position < 0 || math.IsNaN(b.Position) || math.IsInf(b.Position, 0) {
		problem(w, http.StatusBadRequest, "urn:ether:problem:validation-failed", "Validation failed", "The replan request is missing a required field.")
		return
	}
	if b.InstallationID != i.installationID() {
		installationChanged(w)
		return
	}
	id := r.PathValue("session")
	if info, ok := i.catchUpSession(r, v, id); ok {
		i.replanCatchUp(w, r, v, info, b)
		return
	}
	if !i.ownsSession(r.Context(), v, device(r), id) {
		problem(w, http.StatusNotFound, "urn:ether:problem:playback-session-not-found", "Playback session not found", "No playback session matches this device.")
		return
	}
	snap, ok := i.rememberedPlan(id)
	if !ok {
		problem(w, http.StatusNotFound, "urn:ether:problem:playback-session-not-found", "Playback session not found", "This server has no plan to revise for the session.")
		return
	}
	if intentReplan(b.Operation) && !snap.live && len(snap.start) > 0 {
		i.replanSelection(w, r, b, snap, v, id)
		return
	}
	raw, err = json.Marshal(snap.body)
	if err != nil {
		http.Error(w, "Unavailable", 503)
		return
	}
	var document map[string]any
	if json.Unmarshal(raw, &document) != nil {
		http.Error(w, "Unavailable", 503)
		return
	}
	plan, _ := document["playback_plan"].(map[string]any)
	if plan == nil {
		problem(w, http.StatusNotFound, "urn:ether:problem:playback-session-not-found", "Playback session not found", "This server has no plan to revise for the session.")
		return
	}
	currentKey, _ := plan["plan_attempt_key"].(string)
	currentPlanID, _ := plan["plan_id"].(string)
	currentDelivery := snap.delivery
	if currentDelivery == "" {
		currentDelivery, _ = plan["delivery"].(string)
	}
	baseID := snap.baseID
	if baseID == "" {
		baseID = currentPlanID
	}

	attempted := make(map[string]bool, len(b.Attempted)+2)
	for _, k := range b.Attempted {
		if k != "" {
			attempted[k] = true
		}
	}
	if !nonFailureReplan(b.Operation) {
		if b.PlanKey != "" {
			attempted[b.PlanKey] = true
		}
		if b.FailedPlan != "" {
			attempted[b.FailedPlan] = true
		}
	}

	access := frontend.SessionAccess{Viewer: v, Device: device(r), Protocol: "silo"}
	failedKey := b.PlanKey
	if b.FailedPlan != "" {
		failedKey = b.FailedPlan
	}
	if failedKey == "" {
		failedKey = currentKey
	}
	if !snap.live && !nonFailureReplan(b.Operation) && i.replanRecoveredOriginal(w, r, access, id, failedKey, snap, document, plan) {
		return
	}
	pos := time.Duration(b.Position * float64(time.Second))
	if snap.live {
		pos = 0
	}

	nextFormat := snap.format
	nextDelivery := currentDelivery
	nextKey := currentKey
	nextReason, _ := plan["decision_reason"].(string)

	routeFailed := !nonFailureReplan(b.Operation) && (attempted[currentKey] || attempted[currentPlanID] || b.Operation == "failure_recovery")
	if routeFailed {
		var ladder []replanRoute
		switch currentDelivery {
		case "original_http":
			ladder = []replanRoute{
				{delivery: "server_remux_hls", format: frontend.StreamHLS, key: baseID + ":remux_hls", reason: "replan_remux_hls"},
				{delivery: "server_transcode_hls", format: frontend.StreamHLS, key: baseID + ":transcode_hls", reason: "replan_transcode_hls"},
			}
		case "server_remux_hls":
			ladder = []replanRoute{
				{delivery: "server_transcode_hls", format: frontend.StreamHLS, key: baseID + ":transcode_hls", reason: "replan_transcode_hls"},
			}
		}
		var chosen *replanRoute
		for idx := range ladder {
			candidate := &ladder[idx]
			if !attempted[candidate.key] {
				chosen = candidate
				break
			}
		}
		if chosen == nil {
			siloAdaptationUnavailable(w, id)
			return
		}
		// The transcode rung must encode: the copy it replaces is what the
		// player refused, and serving that stream again only fails again.
		if _, err = i.host.Sessions.StartStream(r.Context(), access, id, frontend.StreamRequest{Format: chosen.format, Position: pos, ReencodeVideo: chosen.delivery == "server_transcode_hls"}); err != nil {
			if errors.Is(err, frontend.ErrNotAvailable) && !errors.Is(err, frontend.ErrPreparing) {
				siloAdaptationUnavailable(w, id)
				return
			}
			i.playbackError(w, access.Viewer, err, r.Header.Get("Accept-Language"))
			return
		}
		nextFormat = chosen.format
		nextDelivery = chosen.delivery
		nextKey = chosen.key
		nextReason = chosen.reason
	} else {
		if _, err = i.host.Sessions.StartStream(r.Context(), access, id, frontend.StreamRequest{Format: nextFormat, Position: pos}); err != nil {
			i.playbackError(w, access.Viewer, err, r.Header.Get("Accept-Language"))
			return
		}
		if !nonFailureReplan(b.Operation) && attempted[nextKey] {
			siloAdaptationUnavailable(w, id)
			return
		}
	}

	plan["plan_id"] = nextKey
	plan["plan_attempt_key"] = nextKey
	plan["delivery"] = nextDelivery
	if nextDelivery != "original_http" {
		withoutNativeSubtitles(plan)
	}
	if nextReason != "" {
		plan["decision_reason"] = nextReason
	}
	if nextDelivery == "server_transcode_hls" {
		if transforms, _ := plan["transformations"].([]any); len(transforms) == 0 {
			plan["transformations"] = []any{map[string]any{"name": "video_transcode", "executor": "ffmpeg", "recipe_version": "1", "validated_claims": []string{"video_transcode"}}}
		}
	}
	stream, _ := plan["stream"].(map[string]any)
	if stream == nil {
		stream = map[string]any{}
		plan["stream"] = stream
	}
	stream["url"] = siloMediaURL(id, nextFormat)
	if snap.replacement != "" && nextFormat == frontend.StreamDirect {
		stream["url"] = siloMediaURL(id, nextFormat) + "?sourceRevision=" + snap.replacement
	}
	if nextFormat == frontend.StreamHLS {
		stream["protocol"] = "hls"
		stream["container"] = "hls"
		stream["mime_type"] = "application/vnd.apple.mpegurl"
	}
	timeline, _ := plan["timeline"].(map[string]any)
	if timeline == nil {
		timeline = map[string]any{}
		plan["timeline"] = timeline
	}
	if snap.live {
		timeline["source_start_seconds"] = float64(0)
		timeline["stream_origin_seconds"] = float64(0)
		timeline["player_start_seconds"] = float64(0)
		timeline["timeline_offset_seconds"] = float64(0)
		timeline["can_seek_anywhere"] = false
	} else {
		timeline["source_start_seconds"] = b.Position
		if nextFormat == frontend.StreamHLS {
			timeline["stream_origin_seconds"] = b.Position
			timeline["player_start_seconds"] = 0
			timeline["timeline_offset_seconds"] = b.Position
		} else {
			timeline["stream_origin_seconds"] = 0
			timeline["player_start_seconds"] = b.Position
			timeline["timeline_offset_seconds"] = 0
		}
	}
	document["session_id"] = id
	i.updatePlan(id, playbackPlan{replacement: snap.replacement, format: nextFormat, live: snap.live, baseID: baseID, delivery: nextDelivery, requireAuthored: snap.requireAuthored, body: document, start: snap.start, media: snap.media, hlsRenditions: snap.hlsRenditions, audioCopy: snap.audioCopy && nextDelivery != "server_transcode_hls"})
	reply(w, http.StatusOK, document)
}

func siloAdaptationUnavailable(w http.ResponseWriter, sessionID string) {
	reply(w, http.StatusOK, map[string]any{
		"protocol_version": 3,
		"server_features":  siloServerFeatures,
		"outcome":          "adaptation_unavailable",
		"session_id":       sessionID,
		"terminal": map[string]any{
			"reason":    "no_compatible_route",
			"message":   "No compatible playback route remains for this stream.",
			"retryable": true,
		},
	})
}

func (i *instance) ownsSession(ctx context.Context, v *frontend.Viewer, d frontend.Device, id string) bool {
	if i.host.Sessions == nil || id == "" || v == nil || d.ID == "" {
		return false
	}
	list, err := i.host.Sessions.Sessions(ctx, frontend.SessionAccess{Viewer: v, Device: d, Protocol: "silo"})
	if err != nil {
		return false
	}
	for _, session := range list {
		if session.ID == id && session.ViewerID == v.ID && session.Protocol == "silo" && session.Device.ID == d.ID {
			return true
		}
	}
	return false
}

type routeEventRequest struct {
	InstallationID string            `json:"installation_id"`
	EventID        string            `json:"event_id"`
	Protocol       int               `json:"protocol_version"`
	Attempt        string            `json:"playback_attempt_id"`
	Event          string            `json:"event"`
	Diagnostics    map[string]string `json:"diagnostics"`
}

func (i *instance) routeEvent(w http.ResponseWriter, r *http.Request) {
	v, _, err := i.deviceViewer(r)
	if err != nil || v == nil {
		http.Error(w, "Unauthorized", 401)
		return
	}
	var b routeEventRequest
	if decode(r, &b) != nil || b.InstallationID == "" || b.EventID == "" || b.Protocol != 3 || b.Attempt == "" || b.Event == "" || b.Diagnostics == nil {
		problem(w, http.StatusBadRequest, "urn:ether:problem:validation-failed", "Validation failed", "The route event is missing a required field.")
		return
	}
	if b.InstallationID != i.installationID() {
		installationChanged(w)
		return
	}
	reply(w, http.StatusAccepted, map[string]any{"event_id": b.EventID, "outcome": "accepted"})
}

func problem(w http.ResponseWriter, status int, typ, title, detail string) {
	requestID, err := controlCommandID()
	if err != nil {
		http.Error(w, title, status)
		return
	}
	w.Header().Set("X-Request-ID", requestID)
	problemReply(w, status, map[string]any{"type": typ, "title": title, "status": status, "detail": detail, "instance": "urn:silo:request:" + requestID})
}

type sessionMedia interface {
	ServeSessionMedia(w http.ResponseWriter, r *http.Request, a frontend.SessionAccess, sessionID, resource string)
}

func (i *instance) serveSessionMedia(w http.ResponseWriter, r *http.Request, sessionID, resource string) {
	mediasafety.SetHeaders(w)
	if i.host.Sessions == nil {
		http.Error(w, "Unavailable", 503)
		return
	}
	viewer, _, err := i.deviceViewer(r)
	if err != nil || viewer == nil {
		http.Error(w, "Unauthorized", 401)
		return
	}
	if _, ok := i.catchUpSession(r, viewer, sessionID); ok {
		i.catchUpMedia(w, r, viewer, sessionID, resource)
		return
	}
	media, ok := i.host.Sessions.(sessionMedia)
	if !ok {
		http.Error(w, "Unavailable", 503)
		return
	}
	access := frontend.SessionAccess{Viewer: viewer, Device: device(r), Protocol: "silo"}
	if i.tryServeSessionSubtitleResource(w, r, media, access, sessionID, resource) {
		return
	}
	media.ServeSessionMedia(w, r, access, sessionID, resource)
}

func (i *instance) playbackRoutes() error {
	if err := i.router.Handle("POST", "/api/v2/playback/route-events", http.HandlerFunc(i.routeEvent)); err != nil {
		return err
	}
	if err := i.router.Handle("POST", "/api/v2/playback/{session}/replan", http.HandlerFunc(i.playbackReplan)); err != nil {
		return err
	}
	for _, method := range []string{http.MethodGet, http.MethodHead} {
		if err := i.router.Handle(method, "/api/v2/playback/transcode/{session}/{path...}", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			session := r.PathValue("session")
			serveSiloHLS(w, r, r.PathValue("path"), func(w http.ResponseWriter, r *http.Request, resource string) {
				i.serveSessionMedia(w, r, session, resource)
			})
		})); err != nil {
			return err
		}
	}
	if err := i.router.Handle("GET", "/api/v2/playback/sessions/control/capabilities", http.HandlerFunc(i.playbackControlCapabilities)); err != nil {
		return err
	}
	if err := i.router.Handle("POST", "/api/v2/playback/sessions/{session}/control/ws-ticket", http.HandlerFunc(i.controlTicket)); err != nil {
		return err
	}
	if err := i.router.Handle("GET", "/api/v2/playback/sessions/{session}/control/ws", http.HandlerFunc(i.controlSocket)); err != nil {
		return err
	}
	if err := i.router.Handle("GET", "/api/v2/playback/capabilities", http.HandlerFunc(i.playbackCapabilities)); err != nil {
		return err
	}
	for _, e := range []struct{ method, path string }{{"POST", "/api/v2/playback/{session}/progress"}, {"DELETE", "/api/v2/playback/{session}"}} {
		if err := i.router.Handle(e.method, e.path, http.HandlerFunc(i.progress)); err != nil {
			return err
		}
	}
	if err := i.router.Handle("POST", "/api/v2/playback/start", http.HandlerFunc(i.playbackStart)); err != nil {
		return err
	}
	for _, method := range []string{http.MethodGet, http.MethodHead} {
		if err := i.router.Handle(method, "/api/v2/stream/{path...}", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			sessionID, resource, ok := strings.Cut(r.PathValue("path"), "/")
			if !ok {
				sessionID, resource = r.PathValue("path"), "file"
			}
			i.serveSessionMedia(w, r, sessionID, resource)
		})); err != nil {
			return err
		}
	}
	return nil
}

// nativeEmbedded reports whether the client declared that it renders codec
// from container itself during original-file playback.
func (b startRequest) nativeEmbedded(container, codec string) bool {
	d, ok := b.Context.Deliveries["original_http"]
	if !ok {
		return false
	}
	container = siloContainerName(container)
	codec = siloSubtitleCodecName(codec)
	for _, n := range d.Subtitles.NativeEmbedded {
		if (n.Identity != "" && n.Identity != "ffmpeg_stream_index") || siloContainerName(n.Container) != container {
			continue
		}
		for _, c := range n.Codecs {
			if siloSubtitleCodecName(c) == codec {
				return true
			}
		}
	}
	return false
}

func siloContainerName(name string) string {
	name = strings.ToLower(strings.TrimSpace(name))
	if name == "matroska" || strings.HasPrefix(name, "matroska,") {
		return "mkv"
	}
	return name
}

// siloSubtitleCodecName is the FFmpeg codec name the client compares with.
func siloSubtitleCodecName(codec string) string {
	switch codec = strings.ToLower(strings.TrimSpace(codec)); codec {
	case "srt":
		return "subrip"
	case "vtt":
		return "webvtt"
	case "tx3g":
		return "mov_text"
	case "pgs", "pgssub":
		return "hdmv_pgs_subtitle"
	case "dvdsub", "vobsub":
		return "dvd_subtitle"
	case "dvbsub":
		return "dvb_subtitle"
	}
	return codec
}

// withoutNativeSubtitles turns a plan's player-rendered embedded subtitle
// into its sidecar artifact when the delivery leaves the original file: only
// original-file playback demuxes embedded tracks in the player.
func withoutNativeSubtitles(plan map[string]any) {
	subtitle, _ := plan["subtitle"].(map[string]any)
	if subtitle == nil {
		return
	}
	inventory, _ := subtitle["inventory"].([]any)
	if _, embedded := subtitle["embedded"]; !embedded {
		return
	}
	delete(subtitle, "embedded")
	trackID, _ := subtitle["track_id"].(string)
	for _, raw := range inventory {
		row, _ := raw.(map[string]any)
		if url, ok := row["url"].(string); ok && row["track_id"] == trackID && strings.Contains(url, ".vtt") {
			subtitle["mode"] = "convert"
			subtitle["artifact"] = map[string]any{"url": url, "format": "vtt", "mime_type": "text/vtt", "timing_origin_seconds": 0}
			return
		}
	}
	subtitle["mode"] = "off"
	delete(subtitle, "track_id")
	if selected, ok := plan["selected_tracks"].(map[string]any); ok {
		delete(selected, "subtitle")
	}
}

// siloServerFeatures are the playback features every plan document declares.
// Each name corresponds to a contract Ether implements: native embedded tracks,
// seek re-anchoring on offset HLS streams, direct-stream resume, route event
// diagnostics, active quality reporting, and HLS text subtitle renditions.
var siloServerFeatures = []string{
	"playback_plan_v3",
	"neutral_playback_v3_contract_v1",
	"embedded_subtitles_v1",
	"playback_route_diagnostics",
	"seek_reanchor_v1",
	"direct_stream_resume_v1",
	"header_authenticated_media_v1",
	"sequenced_progress_v1",
	"plan_source_duration_v1",
	"plan_active_quality_v1",
	"hls_subtitle_renditions_v1",
	"hls_remux_subtitle_renditions_v1",
}

// replanSelection answers a track or quality pick within the playing
// session: the plan names the new audio and subtitle selection, and a server
// stream restarts at the viewer's position only when its audio or a burned
// subtitle changes. Silo Android sends every audio and subtitle pick this
// way, subtitles off included, and refuses to replace a session whose
// progress is still pending, so the session is kept.
func (i *instance) replanSelection(w http.ResponseWriter, r *http.Request, b replanRequest, snap playbackPlan, viewer *frontend.Viewer, id string) {
	var start startRequest
	if json.Unmarshal(snap.start, &start) != nil || len(snap.media.Streams) == 0 {
		siloAdaptationUnavailable(w, id)
		return
	}
	raw, err := json.Marshal(snap.body)
	if err != nil {
		http.Error(w, "Unavailable", 503)
		return
	}
	var document map[string]any
	if json.Unmarshal(raw, &document) != nil {
		http.Error(w, "Unavailable", 503)
		return
	}
	plan, _ := document["playback_plan"].(map[string]any)
	if plan == nil {
		siloAdaptationUnavailable(w, id)
		return
	}
	streamAt := func(kind string, ordinal int) (frontend.StreamInfo, bool) {
		n := 0
		for _, s := range snap.media.Streams {
			if s.Type != kind {
				continue
			}
			if n == ordinal {
				return s, true
			}
			n++
		}
		return frontend.StreamInfo{}, false
	}
	currentAudio := -1
	for _, s := range snap.media.Streams {
		if s.Type == "audio" && (currentAudio < 0 || s.Default) {
			currentAudio = s.Index
		}
	}
	if selected, _ := plan["selected_tracks"].(map[string]any); selected != nil {
		if a, _ := selected["audio"].(map[string]any); a != nil {
			if n, ok := a["index"].(float64); ok {
				if s, ok := streamAt("audio", int(n)); ok {
					currentAudio = s.Index
				}
			}
		}
	}
	audio := currentAudio
	if b.Selected.Audio != nil && b.Selected.Audio.Index != nil {
		s, ok := streamAt("audio", *b.Selected.Audio.Index)
		if !ok {
			problem(w, http.StatusUnprocessableEntity, "urn:ether:problem:track-not-found", "Track not found", "The selected audio track is not in this media.")
			return
		}
		audio = s.Index
	}
	subtitle := -1
	var subtitleTrack frontend.StreamInfo
	if b.Selected.Subtitle != nil && b.Selected.Subtitle.Index != nil && *b.Selected.Subtitle.Index >= 0 {
		s, ok := streamAt("subtitle", *b.Selected.Subtitle.Index)
		if !ok {
			problem(w, http.StatusUnprocessableEntity, "urn:ether:problem:track-not-found", "Track not found", "The selected subtitle track is not in this media.")
			return
		}
		subtitle, subtitleTrack = s.Index, s
	}
	// Server HLS cannot carry an image subtitle: it is burned into the video.
	burn := snap.format == frontend.StreamHLS && subtitle >= 0 && frontend.IsImageSubtitle(subtitleTrack.Codec)
	wasBurned := false
	if sub, _ := plan["subtitle"].(map[string]any); sub != nil {
		wasBurned = sub["mode"] == "burn_in"
	}
	access := frontend.SessionAccess{Viewer: viewer, Device: device(r), Protocol: "silo"}
	pos := time.Duration(b.Position * float64(time.Second))
	// A quality pick caps the encoded height; Auto (or a rung at least the
	// source's height) lifts the cap. The original file cannot be made
	// smaller, so a cap moves it to a server encode.
	current, _ := plan["active_quality"].(string)
	wantQuality := b.Quality
	if wantQuality == "auto" {
		wantQuality = ""
	}
	qualityChanged := b.Operation == "quality_change" && wantQuality != current
	height := 0
	if qualityChanged && wantQuality != "" {
		if rows, _ := plan["available_qualities"].([]any); rows != nil {
			for _, raw := range rows {
				row, _ := raw.(map[string]any)
				if label, _ := row["label"].(string); label == wantQuality {
					if h, ok := row["height"].(float64); ok {
						height = int(h)
					}
				}
			}
		}
		if height == 0 {
			siloAdaptationUnavailable(w, id)
			return
		}
	}
	format := snap.format
	if qualityChanged && height > 0 && format == frontend.StreamDirect {
		format = frontend.StreamHLS
	}
	restart := format == frontend.StreamHLS && (audio != currentAudio || burn || wasBurned || qualityChanged || format != snap.format)
	if restart {
		// Always name the subtitle: without it the restart kept the previous
		// selection, so a burned PGS track stayed in the picture after
		// switching to a text track (both showed) or to Off.
		req := frontend.StreamRequest{Format: frontend.StreamHLS, Position: pos, AudioStream: &audio, SubtitleStream: &subtitle, BurnImageSubtitles: burn}
		if qualityChanged {
			req.MaxHeight = &height
		}
		if _, err := i.host.Sessions.StartStream(r.Context(), access, id, req); err != nil {
			i.playbackError(w, access.Viewer, err, r.Header.Get("Accept-Language"))
			return
		}
	}
	if format != snap.format {
		snap.format, snap.delivery, snap.audioCopy = format, "server_transcode_hls", false
		plan["delivery"] = snap.delivery
		withoutNativeSubtitles(plan)
		if stream, _ := plan["stream"].(map[string]any); stream != nil {
			stream["protocol"], stream["container"], stream["mime_type"] = "hls", "hls", "application/vnd.apple.mpegurl"
		}
	}
	if qualityChanged {
		if wantQuality == "" {
			delete(plan, "active_quality")
		} else {
			plan["active_quality"] = wantQuality
		}
		if recipe, _ := plan["effective_recipe"].(map[string]any); recipe != nil && height > 0 {
			if v := snap.media.Video(); v != nil && v.Height > height && v.Width > 0 {
				recipe["height"], recipe["width"] = height, v.Width*height/v.Height
			}
		}
	}
	selected, sub, fidelityPolicy, _, assStyling := i.siloTracks(siloTrackInputs{r: r, viewer: viewer, access: access, sessionID: id, start: start, media: snap.media, format: format, delivery: snap.delivery, live: snap.live, hlsRenditions: snap.hlsRenditions, audioStream: audio, subtitleStream: subtitle, burn: burn})
	if b.Selected.Subtitle != nil && subtitle >= 0 && selected["subtitle"] == nil {
		problem(w, http.StatusServiceUnavailable, "urn:ether:problem:track-source-unavailable", "Subtitle unavailable", "The selected subtitle cannot be delivered for this stream.")
		return
	}
	plan["selected_tracks"] = selected
	plan["subtitle"] = sub
	plan["subtitle_fidelity_policy"] = fidelityPolicy
	if claims, _ := plan["claims"].(map[string]any); claims != nil {
		if subs, _ := claims["subtitles"].(map[string]any); subs != nil {
			subs["ass_styling_preserved"] = assStyling
		}
		if audioClaim, _ := claims["audio"].(map[string]any); audioClaim != nil && snap.audioCopy {
			if s, ok := streamAt("audio", ordinalOf(snap.media, "audio", audio)); ok {
				audioClaim["codec"] = s.Codec
			}
		}
	}
	if recipe, _ := plan["effective_recipe"].(map[string]any); recipe != nil && snap.audioCopy {
		for _, s := range snap.media.Streams {
			if s.Type == "audio" && s.Index == audio {
				recipe["audio_codec"] = s.Codec
				if s.Channels > 0 {
					recipe["audio_channels"] = s.Channels
				}
			}
		}
	}
	base, _ := plan["plan_id"].(string)
	if base == "" {
		base = snap.baseID
	}
	key := fmt.Sprintf("%s:tracks:a%d:s%d:q%d", base, audio, subtitle, height)
	plan["plan_attempt_key"] = key
	if restart {
		if stream, _ := plan["stream"].(map[string]any); stream != nil {
			stream["url"] = siloMediaURL(id, snap.format)
		}
		if timeline, _ := plan["timeline"].(map[string]any); timeline != nil {
			timeline["source_start_seconds"] = b.Position
			timeline["stream_origin_seconds"] = b.Position
			timeline["player_start_seconds"] = 0
			timeline["timeline_offset_seconds"] = b.Position
		}
	}
	document["session_id"] = id
	snap.body = document
	i.updatePlan(id, snap)
	reply(w, http.StatusOK, document)
}

// ordinalOf is the position of a stream among the media's streams of a kind.
func ordinalOf(media frontend.MediaInfo, kind string, index int) int {
	n := 0
	for _, s := range media.Streams {
		if s.Type != kind {
			continue
		}
		if s.Index == index {
			return n
		}
		n++
	}
	return -1
}

// siloTrackInputs are what a plan's track selection and subtitle delivery
// depend on: the start request's declared abilities, the session's media and
// delivery, and the selected stream indices.
type siloTrackInputs struct {
	r              *http.Request
	viewer         *frontend.Viewer
	access         frontend.SessionAccess
	sessionID      string
	start          startRequest
	media          frontend.MediaInfo
	format         frontend.StreamFormat
	delivery       string
	live           bool
	hlsRenditions  bool
	audioStream    int
	subtitleStream int
	burn           bool
}

// siloTracks builds a plan's selected tracks and subtitle decision, with the
// subtitle inventory, for one selection.
func (i *instance) siloTracks(in siloTrackInputs) (selected, subtitle map[string]any, fidelityPolicy string, requireAuthoredFidelity, assStylingPreserved bool) {
	deliveryClass := "original_http"
	if in.format == frontend.StreamHLS {
		deliveryClass = "hls"
	}
	var clientASSStyling bool
	if in.start.Context.Deliveries != nil {
		if delivCap, ok := in.start.Context.Deliveries[deliveryClass]; ok {
			clientASSStyling = delivCap.Subtitles.ASSStyling
		}
	}
	requireAuthoredFidelity = strings.EqualFold(in.start.SubtitleFidelity, "preserve")
	fidelityPolicy = "allow_simplified_rendering"
	if requireAuthoredFidelity {
		fidelityPolicy = "require_authored_fidelity"
	}
	_, hasSubtitleArtifacts := i.host.Sessions.(frontend.SessionSubtitleArtifacts)
	assStylingPreserved = false
	selected = map[string]any{}
	inventory := []any{}
	subtitle = map[string]any{"mode": "off"}
	ordinal := 0
	audioOrdinal := 0
	// Original-file playback demuxes every embedded track in the player
	// (embedded_subtitles_v1). A track it renders natively needs no server
	// artifact: the plan names its stream and the subtitle is on screen as
	// soon as the video is, with no whole-file extraction.
	native := func(track frontend.StreamInfo) bool {
		// The app switches by the file's own track id; without it the plan
		// fails its validation, so the track goes as a sidecar instead.
		if in.format != frontend.StreamDirect || track.External || track.ExternalURL != "" || track.ContainerTrackID <= 0 || !slices.Contains(in.start.Features, "embedded_subtitles_v1") {
			return false
		}
		return in.start.nativeEmbedded(in.media.Container, track.Codec)
	}
	vttURLs := map[int]string{}
	hlsOK := map[int]bool{}
	var subReqs []frontend.SubtitleURLRequest
	wantHLS := in.format == frontend.StreamHLS && !in.live && in.hlsRenditions && (in.delivery != "server_remux_hls" || slices.Contains(in.start.Features, "hls_remux_subtitle_renditions_v1"))
	for _, track := range in.media.Streams {
		if track.Type != "subtitle" {
			continue
		}
		subReqs = append(subReqs, frontend.SubtitleURLRequest{Index: track.Index, Format: "vtt"})
		if wantHLS && !track.External && track.Index == in.subtitleStream {
			subReqs = append(subReqs, frontend.SubtitleURLRequest{Index: track.Index, Format: "hls"})
		}
	}
	if len(subReqs) > 0 {
		var urls []string
		if batch, ok := i.host.Sessions.(frontend.SubtitleURLBatch); ok {
			if got, err := batch.SubtitleURLs(in.r.Context(), in.access, in.sessionID, subReqs); err == nil && len(got) == len(subReqs) {
				urls = got
			}
		}
		if urls == nil {
			urls = make([]string, len(subReqs))
			for idx, req := range subReqs {
				if u, err := i.host.Sessions.SubtitleURL(in.r.Context(), in.access, in.sessionID, req.Index, req.Format); err == nil {
					urls[idx] = u
				}
			}
		}
		for idx, req := range subReqs {
			if urls[idx] == "" {
				continue
			}
			if req.Format == "hls" {
				hlsOK[req.Index] = true
			} else {
				vttURLs[req.Index] = urls[idx]
			}
		}
	}
	// hls_subtitle_renditions_v1 covers a transcode; a copy remux needs
	// hls_remux_subtitle_renditions_v1 as well.
	hlsRendition := func(track frontend.StreamInfo) map[string]any {
		if !wantHLS || track.External || !hlsOK[track.Index] {
			return nil
		}
		return map[string]any{"group_id": frontend.HLSTextRenditionGroup, "name": frontend.HLSTextRenditionName(track.Title, track.Language, track.Index), "format": "vtt", "mime_type": "text/vtt"}
	}
	for _, track := range in.media.Streams {
		if track.Type == "audio" {
			if track.Index == in.audioStream {
				selected["audio"] = map[string]any{"id": "file:" + in.start.FileID + ":audio:" + strconv.Itoa(audioOrdinal), "index": audioOrdinal}
			}
			audioOrdinal++
		}
		if track.Type != "subtitle" {
			continue
		}
		trackID := "file:" + in.start.FileID + ":subtitle:" + strconv.Itoa(ordinal)
		entry := map[string]any{"track_id": trackID, "combined_index": ordinal, "source": "embedded", "codec": track.Codec, "language": track.Language, "label": track.Title, "forced": track.Forced, "default": track.Default, "hearing_impaired": false, "delivery": "burn_in_only"}
		if track.External {
			entry["source"] = "external"
		}
		isEmbeddedASS := !track.External && frontend.IsASSSubtitleCodec(track.Codec)
		// Availability is resolved when the client requests the artifact. Do
		// not scan every subtitle track or extract media during startup.
		authoredASS := isEmbeddedASS && hasSubtitleArtifacts
		preserveTrackASS := authoredASS && clientASSStyling
		url := vttURLs[track.Index]
		if strings.HasPrefix(url, "/sessions/media/") {
			entry["delivery"] = "sidecar"
			ext := ".vtt"
			if preserveTrackASS {
				ext = ".ass"
			}
			entry["url"] = "/api/v2/stream/" + in.sessionID + "/subtitles/" + strconv.Itoa(track.Index) + ext
			if sync := i.subtitleSyncMS(in.r, in.viewer); sync != 0 {
				entry["url"] = entry["url"].(string) + "?sync_ms=" + strconv.Itoa(sync)
			}
			if authoredASS {
				entry["font_bundle_url"] = "/api/v2/stream/" + in.sessionID + "/subtitles/" + strconv.Itoa(track.Index) + "/fonts"
			}
		}
		nativeTrack := native(track)
		if url, ok := entry["url"].(string); ok && nativeTrack {
			// The player switches to this stream in its own demux; the row
			// names it on the URL, where the inventory schema allows it.
			sep := "?"
			if strings.Contains(url, "?") {
				sep = "&"
			}
			entry["url"] = url + sep + "embedded_stream_index=" + strconv.Itoa(track.Index)
		}
		inventory = append(inventory, entry)
		if track.Index == in.subtitleStream {
			selected["subtitle"] = map[string]any{"id": trackID, "index": ordinal}
			if in.burn {
				subtitle["mode"] = "burn_in"
				subtitle["track_id"] = trackID
			} else if nativeTrack {
				subtitle["mode"] = "render"
				subtitle["track_id"] = trackID
				subtitle["embedded"] = map[string]any{"stream_index": track.Index, "container_track_id": strconv.Itoa(track.ContainerTrackID)}
			} else if rendition := hlsRendition(track); rendition != nil {
				subtitle["mode"] = "convert"
				subtitle["track_id"] = trackID
				subtitle["hls_rendition"] = rendition
			} else if url, e := entry["url"].(string); e {
				if preserveTrackASS {
					subtitle["mode"] = "render"
					subtitle["track_id"] = trackID
					subtitle["artifact"] = map[string]any{"url": url, "format": "ass", "mime_type": "text/x-ssa", "timing_origin_seconds": 0}
					// The artifact endpoint must still validate completeness; startup
					// cannot claim that authored bytes have already been delivered.
				} else {
					// "preserve" is the app's default preference, sent on every
					// start. Plain text (SubRip, WebVTT, mov_text) converts to
					// WebVTT without losing anything. Only authored ASS styling
					// can be simplified, and then the plan says so instead of
					// refusing to play.
					if isEmbeddedASS {
						fidelityPolicy = "allow_simplified_rendering"
					}
					subtitle["mode"] = "convert"
					subtitle["track_id"] = trackID
					subtitle["artifact"] = map[string]any{"url": url, "format": "vtt", "mime_type": "text/vtt", "timing_origin_seconds": 0}
				}
			} else {
				// No deliverable artifact for the chosen track: start without
				// subtitles rather than fail the start.
				delete(selected, "subtitle")
			}
		}
		ordinal++
	}
	subtitle["inventory"] = inventory
	return selected, subtitle, fidelityPolicy, requireAuthoredFidelity, assStylingPreserved
}
