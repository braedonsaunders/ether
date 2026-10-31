package silo

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/kit/caps"
	"github.com/braedonsaunders/ether/internal/frontend/kit/quality"
)

func (i *instance) installationID() string {
	return i.id[:8] + "-" + i.id[8:12] + "-" + "4" + i.id[13:16] + "-" + "8" + i.id[17:20] + "-" + i.id[20:]
}

func installationChanged(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(http.StatusConflict)
	_ = json.NewEncoder(w).Encode(map[string]any{"type": "/problems/installation_changed", "title": "Playback installation changed", "status": http.StatusConflict, "detail": "Refresh playback capabilities before retrying."})
}

type startRequest struct {
	Context struct {
		Deliveries map[string]struct {
			Enabled   bool `json:"enabled"`
			Supported bool `json:"supported_on_device"`
		} `json:"deliveries"`
	} `json:"client_playback_context"`
	FileID         string  `json:"file_id"`
	ProfileID      string  `json:"profile_id"`
	InstallationID string  `json:"installation_id"`
	Attempt        string  `json:"playback_attempt_id"`
	Protocol       int     `json:"protocol_version"`
	Quality        string  `json:"quality_preference"`
	Position       float64 `json:"start_position"`
	Audio          *int    `json:"audio_track_index"`
	Subtitle       *int    `json:"subtitle_track_index"`
	Capabilities   struct {
		Video      []string `json:"codecs_video"`
		Audio      []string `json:"codecs_audio"`
		Containers []string `json:"containers"`
		HDR        bool     `json:"hdr"`
		Resolution string   `json:"max_resolution"`
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
	var b startRequest
	if decode(r, &b) != nil || b.Protocol != 3 || b.InstallationID == "" || b.Attempt == "" || b.Position < 0 || math.IsNaN(b.Position) || math.IsInf(b.Position, 0) || b.Position > float64((1<<63-1)/int64(time.Second)) {
		http.Error(w, "Bad Request", 400)
		return
	}
	if b.InstallationID != i.installationID() {
		installationChanged(w)
		return
	}
	if b.ProfileID != "" && b.ProfileID != strconv.FormatInt(v.ID, 10) {
		http.Error(w, "Forbidden", 403)
		return
	}
	d := device(r)
	height, _ := strconv.Atoi(strings.TrimSuffix(b.Capabilities.Resolution, "p"))
	if h, e := strconv.Atoi(strings.TrimSuffix(b.Quality, "p")); e == nil && h > 0 && (height == 0 || h < height) {
		height = h
	}
	cap, err := i.host.Capabilities.Build(i.identityContext(r), frontend.CapabilityRequest{Viewer: v, Device: caps.DeviceInfo{Protocol: "silo", ClientID: d.ID, Product: d.Client, Version: d.Version, DeviceName: d.DeviceName, VideoCodecs: b.Capabilities.Video, AudioCodecs: b.Capabilities.Audio, Containers: b.Capabilities.Containers, MaxHeight: height, HDR10: b.Capabilities.HDR}, UserAgent: r.UserAgent()})
	if err != nil {
		http.Error(w, "Unavailable", 503)
		return
	}
	scope := frontend.VersionScope{Viewer: v, Device: d, Protocol: "silo", Caps: cap.Client}
	if i.host.SourceOffers == nil {
		versionUnavailable(w)
		return
	}
	offer, err := i.host.SourceOffers.Resolve(r.Context(), scope, b.FileID)
	if err != nil || offer.Target.Item == 0 || (offer.Target.VersionHandle == "") == (offer.Target.DetailSource == "") {
		versionUnavailable(w)
		return
	}
	key, versionHandle := offer.Target.Item, offer.Target.VersionHandle
	planner, hasDetailPlanner := i.host.Sessions.(frontend.DetailPlanner)
	if (versionHandle != "" && i.host.VersionHandles == nil) || (offer.Target.DetailSource != "" && (!hasDetailPlanner || offer.Target.ETag == "")) {
		versionUnavailable(w)
		return
	}
	direct, hls := true, true
	if b.Context.Deliveries != nil {
		d, ok := b.Context.Deliveries["original_http"]
		direct = ok && d.Enabled && d.Supported
		h, ok := b.Context.Deliveries["hls"]
		hls = ok && h.Enabled && h.Supported
	}
	if !direct && !hls {
		http.Error(w, "Playback unavailable", 422)
		return
	}
	options := frontend.DecideOptions{DirectPlay: direct, DirectStream: hls, AudioStream: -1, SubtitleStream: -1}
	if (b.Audio != nil && *b.Audio < 0) || (b.Subtitle != nil && *b.Subtitle < -1) {
		http.Error(w, "Bad Request", 400)
		return
	}
	pos := time.Duration(b.Position * float64(time.Second))
	pos = i.skipIntroAtStart(r, v, scope, key, versionHandle, pos)
	plan, err := i.host.Sessions.Plan(r.Context(), frontend.PlanRequest{Viewer: v, Device: d, Protocol: "silo", Item: key, Version: versionHandle, DetailSource: offer.Target.DetailSource, ExpectedSourceETag: offer.Target.ETag, Caps: cap.Client, Intent: frontend.PlayIntent{ExactVersion: versionHandle != "", Options: options, AllowTranscoding: &hls, AudioOrdinal: b.Audio, SubtitleStreamIndex: b.Subtitle, Position: pos, PlaybackID: b.Attempt, ClientSessionID: b.InstallationID}})
	if err != nil {
		if !errors.Is(err, frontend.ErrSessionBusy) && !errors.Is(err, frontend.ErrTranscodeCapacity) && !errors.Is(err, frontend.ErrTrackNotFound) && !errors.Is(err, frontend.ErrTrackSourceUnavailable) {
			versionUnavailable(w)
			return
		}
		playbackError(w, err)
		return
	}
	session, err := i.host.Sessions.Open(r.Context(), frontend.OpenRequest{Protocol: "silo", PlanID: plan.ID, Viewer: v, Device: d})
	if err != nil {
		if !errors.Is(err, frontend.ErrSessionBusy) && !errors.Is(err, frontend.ErrTranscodeCapacity) && !errors.Is(err, frontend.ErrTrackNotFound) && !errors.Is(err, frontend.ErrTrackSourceUnavailable) {
			versionUnavailable(w)
			return
		}
		playbackError(w, err)
		return
	}
	access := frontend.SessionAccess{Viewer: v, Device: d, Protocol: "silo"}
	matchOffer := func() error {
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
		versionUnavailable(w)
		return
	}

	format := frontend.StreamDirect
	delivery := "original_http"
	proto := "http_progressive"
	mime := "video/" + plan.Media.Container
	if plan.Decision.Kind != "directplay" {
		format = frontend.StreamHLS
		delivery = "server_transcode_hls"
		if plan.Decision.Kind == "copy" {
			delivery = "server_remux_hls"
		}
		proto = "hls"
		mime = "application/vnd.apple.mpegurl"
	}
	media, err := i.host.Sessions.StartStream(r.Context(), access, session.ID, frontend.StreamRequest{Format: format, Position: pos})
	if err != nil {
		i.stopFailedAdmission(r.Context(), access, session.ID)
		playbackError(w, err)
		return
	}
	// Admission finishes only after stream preparation and one last exact
	// selector check. Later expiry does not revoke the independent session.
	if err = matchOffer(); err != nil {
		i.stopFailedAdmission(r.Context(), access, session.ID)
		versionUnavailable(w)
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
	selected := map[string]any{}
	inventory := []any{}
	subtitle := map[string]any{"mode": "off"}
	ordinal := 0
	audioOrdinal := 0
	for _, track := range plan.Media.Streams {
		if track.Type == "audio" {
			if track.Index == plan.Decision.AudioStream {
				selected["audio"] = map[string]any{"id": "file:" + b.FileID + ":audio:" + strconv.Itoa(audioOrdinal), "index": audioOrdinal}
			}
			audioOrdinal++
		}
		if track.Type != "subtitle" {
			continue
		}
		trackID := "file:" + b.FileID + ":subtitle:" + strconv.Itoa(ordinal)
		entry := map[string]any{"track_id": trackID, "combined_index": ordinal, "source": "embedded", "codec": track.Codec, "language": track.Language, "label": track.Title, "forced": track.Forced, "default": track.Default, "hearing_impaired": false, "delivery": "burn_in_only"}
		if track.External {
			entry["source"] = "external"
		}
		url, e := i.host.Sessions.SubtitleURL(r.Context(), access, session.ID, track.Index, "vtt")
		if e == nil && strings.HasPrefix(url, "/sessions/media/") {
			entry["delivery"] = "sidecar"
			entry["url"] = "/api/v2/stream/" + session.ID + "/subtitles/" + strconv.Itoa(track.Index) + ".vtt"
			if sync := i.subtitleSyncMS(r, v); sync != 0 {
				entry["url"] = entry["url"].(string) + "?sync_ms=" + strconv.Itoa(sync)
			}
		}
		inventory = append(inventory, entry)
		if track.Index == plan.Decision.SubtitleStream {
			selected["subtitle"] = map[string]any{"id": trackID, "index": ordinal}
			if plan.Decision.BurnSubtitles {
				subtitle["mode"] = "burn_in"
				subtitle["track_id"] = trackID
			} else if url, e := entry["url"].(string); e {
				subtitle["mode"] = "convert"
				subtitle["track_id"] = trackID
				subtitle["artifact"] = map[string]any{"url": url, "format": "vtt", "mime_type": "text/vtt", "timing_origin_seconds": 0}
			} else {
				i.stopFailedAdmission(r.Context(), access, session.ID)
				http.Error(w, "Subtitle unavailable", 503)
				return
			}
		}
		ordinal++
	}
	subtitle["inventory"] = inventory
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
	output := map[string]any{"protocol_version": 3, "plan_id": plan.ID, "session_id": session.ID, "delivery": delivery, "plan_attempt_key": plan.ID, "stream": stream, "timeline": timeline, "selected_tracks": selected, "effective_recipe": recipe, "claims": map[string]any{"video": map[string]any{"hdr10": hdr10, "hdr10_plus": hdr10Plus, "hlg": hlg, "dolby_vision": dv}, "audio": audioClaim, "subtitles": map[string]any{"ass_styling_preserved": false, "bitmap_overlay": false, "bitmap_sidecar": false}}, "subtitle": subtitle, "transformations": siloTransformations(plan), "applied_quirks": []any{}, "runtime_corrections": []any{}, "degradation_warnings": []any{}, "decision_reason": plan.Decision.Reason, "requested_media_file_id": b.FileID, "effective_media_file_id": b.FileID, "source": source, "subtitle_fidelity_policy": "allow_simplified_rendering", "available_qualities": qualities}
	if active := siloActiveQuality(b.Quality, qualities, plan.Decision.VideoCopy, encodeHeight); active != "" {
		output["active_quality"] = active
	}
	document := map[string]any{"outcome": "playable", "protocol_version": 3, "server_features": []string{"playback_plan_v3", "neutral_playback_v3_contract_v1", "header_authenticated_media_v1", "sequenced_progress_v1", "plan_source_duration_v1"}, "session_id": session.ID, "playback_plan": output}
	i.rememberPlan(session.ID, format, document)
	reply(w, 201, document)
}

// siloMediaURL is the only stream address the Apple TV client will play.
// Direct play is the session itself. HLS, including remux and transcode, is
// the session master. A grant in the path is rejected before the request.
func siloMediaURL(sessionID string, format frontend.StreamFormat) string {
	if format == frontend.StreamHLS {
		return "/api/v2/playback/transcode/" + sessionID + "/master.m3u8"
	}
	return "/api/v2/stream/" + sessionID
}

type playbackPlan struct {
	format frontend.StreamFormat
	body   map[string]any
}

func (i *instance) rememberPlan(id string, format frontend.StreamFormat, body map[string]any) {
	i.plansMu.Lock()
	defer i.plansMu.Unlock()
	if i.plans == nil {
		i.plans = map[string]playbackPlan{}
	}
	i.plans[id] = playbackPlan{format: format, body: body}
}

func (i *instance) rememberedPlan(id string) (playbackPlan, bool) {
	i.plansMu.Lock()
	defer i.plansMu.Unlock()
	plan, ok := i.plans[id]
	return plan, ok
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
	if err := i.host.Sessions.Stop(cleanup, access, sessionID); err != nil && i.host.Log != nil {
		i.host.Log.Error("failed playback admission cleanup", "session", sessionID, "err", err)
	}
}

func versionUnavailable(w http.ResponseWriter) {
	requestID, err := controlCommandID()
	if err != nil {
		http.Error(w, "Playback unavailable", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("X-Request-ID", requestID)
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(http.StatusServiceUnavailable)
	_ = json.NewEncoder(w).Encode(map[string]any{"type": "urn:ether:problem:version-unavailable", "title": "Selected version unavailable", "status": http.StatusServiceUnavailable, "detail": "The selected version cannot be served for this playback request.", "instance": "urn:silo:request:" + requestID})
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
	hasSample := b.Sequence != 0 || b.Position != nil || b.Paused != nil
	if !hasSample && stop {
		if err = i.host.Sessions.Stop(r.Context(), access, sessionID); err != nil {
			http.Error(w, "Unavailable", 503)
			return
		}
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
	if stop && (outcome == frontend.ReportApplied || outcome == frontend.ReportReplayed) {
		response["outcome"] = "stopped"
	}
	if b.StopID != "" {
		response["stop_id"] = b.StopID
	}
	reply(w, 200, response)
}

func playbackError(w http.ResponseWriter, err error) {
	if errors.Is(err, frontend.ErrTrackNotFound) {
		http.Error(w, "Selected track unavailable", http.StatusUnprocessableEntity)
		return
	}
	if errors.Is(err, frontend.ErrTrackSourceUnavailable) {
		http.Error(w, "Track source unavailable", http.StatusServiceUnavailable)
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
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_ = json.NewEncoder(w).Encode(map[string]any{"type": "about:blank", "title": "Playback temporarily unavailable", "status": 503, "detail": "Playback admission is busy; retry later.", "instance": "urn:silo:request:" + requestID})
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
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_ = json.NewEncoder(w).Encode(map[string]any{"type": "https://siloserver.org/docs/api/v2/problems/transcode_capacity", "title": "Transcode capacity unavailable", "status": 503, "detail": "The server is busy; retry later.", "instance": "urn:silo:request:" + requestID})
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
	features := []string{"playback_plan_v3", "neutral_playback_v3_contract_v1", "header_authenticated_media_v1", "sequenced_progress_v1", "plan_source_duration_v1"}
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
	Attempt        string   `json:"playback_attempt_id"`
	ReplanID       string   `json:"replan_request_id"`
	FailedPlan     string   `json:"failed_plan_id"`
	PlanAttempt    string   `json:"plan_attempt_id"`
	PlanKey        string   `json:"plan_attempt_key"`
	AttemptCount   int      `json:"attempt_count"`
	Quality        string   `json:"quality_preference"`
	Position       float64  `json:"position_seconds"`
	Attempted      []string `json:"attempted_plan_keys"`
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
	var b replanRequest
	if decode(r, &b) != nil || b.Protocol != 3 || b.InstallationID == "" || b.Attempt == "" || b.ReplanID == "" || b.FailedPlan == "" || b.PlanAttempt == "" || b.PlanKey == "" || b.Quality == "" || b.AttemptCount < 1 || len(b.Attempted) == 0 || b.Position < 0 || math.IsNaN(b.Position) || math.IsInf(b.Position, 0) {
		problem(w, http.StatusBadRequest, "urn:ether:problem:validation-failed", "Validation failed", "The replan request is missing a required field.")
		return
	}
	if b.InstallationID != i.installationID() {
		installationChanged(w)
		return
	}
	id := r.PathValue("session")
	if !i.ownsSession(r.Context(), v, device(r), id) {
		problem(w, http.StatusNotFound, "urn:ether:problem:playback-session-not-found", "Playback session not found", "No playback session matches this device.")
		return
	}
	snap, ok := i.rememberedPlan(id)
	if !ok {
		problem(w, http.StatusNotFound, "urn:ether:problem:playback-session-not-found", "Playback session not found", "This server has no plan to revise for the session.")
		return
	}
	access := frontend.SessionAccess{Viewer: v, Device: device(r), Protocol: "silo"}
	pos := time.Duration(b.Position * float64(time.Second))
	if _, err = i.host.Sessions.StartStream(r.Context(), access, id, frontend.StreamRequest{Format: snap.format, Position: pos}); err != nil {
		playbackError(w, err)
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
		problem(w, http.StatusNotFound, "urn:ether:problem:playback-session-not-found", "Playback session not found", "This server has no plan to revise for the session.")
		return
	}
	stream, _ := plan["stream"].(map[string]any)
	if stream == nil {
		stream = map[string]any{}
		plan["stream"] = stream
	}
	stream["url"] = siloMediaURL(id, snap.format)
	timeline, _ := plan["timeline"].(map[string]any)
	if timeline == nil {
		timeline = map[string]any{}
		plan["timeline"] = timeline
	}
	timeline["source_start_seconds"] = b.Position
	if snap.format == frontend.StreamHLS {
		timeline["stream_origin_seconds"] = b.Position
		timeline["player_start_seconds"] = 0
		timeline["timeline_offset_seconds"] = b.Position
	} else {
		timeline["stream_origin_seconds"] = 0
		timeline["player_start_seconds"] = b.Position
		timeline["timeline_offset_seconds"] = 0
	}
	document["session_id"] = id
	reply(w, http.StatusOK, document)
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
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"type": typ, "title": title, "status": status, "detail": detail, "instance": "urn:silo:request:" + requestID})
}

type sessionMedia interface {
	ServeSessionMedia(w http.ResponseWriter, r *http.Request, a frontend.SessionAccess, sessionID, resource string)
}

func (i *instance) serveSessionMedia(w http.ResponseWriter, r *http.Request, sessionID, resource string) {
	if i.host.Sessions == nil {
		http.Error(w, "Unavailable", 503)
		return
	}
	viewer, _, err := i.deviceViewer(r)
	if err != nil || viewer == nil {
		http.Error(w, "Unauthorized", 401)
		return
	}
	media, ok := i.host.Sessions.(sessionMedia)
	if !ok {
		http.Error(w, "Unavailable", 503)
		return
	}
	media.ServeSessionMedia(w, r, frontend.SessionAccess{Viewer: viewer, Device: device(r), Protocol: "silo"}, sessionID, resource)
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
			i.serveSessionMedia(w, r, r.PathValue("session"), r.PathValue("path"))
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
