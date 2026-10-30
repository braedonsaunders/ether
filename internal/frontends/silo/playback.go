package silo

import (
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/kit/caps"
)

type fileReference struct {
	Key     int64  `json:"key"`
	Version string `json:"version,omitempty"`
}

func (i *instance) installationID() string {
	return i.id[:8] + "-" + i.id[8:12] + "-" + "4" + i.id[13:16] + "-" + "8" + i.id[17:20] + "-" + i.id[20:]
}

func installationChanged(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(http.StatusConflict)
	_ = json.NewEncoder(w).Encode(map[string]any{"type": "/problems/installation_changed", "title": "Playback installation changed", "status": http.StatusConflict, "detail": "Refresh playback capabilities before retrying."})
}

// A PQ stream can contain HDR10+ metadata; absent probe evidence is unknown.
// Non-PQ video cannot carry HDR10+.
func siloSourceHasKnownHDR10Plus(media frontend.MediaInfo) bool {
	for _, stream := range media.Streams {
		if stream.Type == "video" && stream.ColorTransfer == "smpte2084" && stream.HDR10Plus == nil {
			return false
		}
	}
	return true
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
	n, err := strconv.ParseInt(b.FileID, 10, 32)
	if err != nil || n <= 0 || strconv.FormatInt(n, 10) != b.FileID {
		http.Error(w, "Bad Request", 400)
		return
	}
	ref, err := i.host.IDs.Resolve(r.Context(), "silo", "file", int32(n))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	var target fileReference
	if json.Unmarshal([]byte(ref), &target) != nil || target.Key <= 0 {
		http.NotFound(w, r)
		return
	}
	key := frontend.RatingKey(target.Key)
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
	versionHandle := ""
	if target.Version != "" {
		if i.host.VersionHandles == nil || (b.Capabilities.Video == nil && b.Capabilities.Audio == nil && b.Capabilities.Containers == nil) {
			versionUnavailable(w)
			return
		}
		scope := frontend.VersionScope{Viewer: v, Device: d, Protocol: "silo", Caps: cap.Client}
		versionHandle, err = i.host.VersionHandles.Issue(r.Context(), scope, key, target.Version)
		if err != nil || versionHandle == "" {
			versionUnavailable(w)
			return
		}
		resolved, err := i.host.VersionHandles.Resolve(r.Context(), scope, versionHandle)
		if err != nil || resolved.Item != key {
			versionUnavailable(w)
			return
		}
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
	if b.Audio != nil || b.Subtitle != nil {
		if (b.Audio != nil && *b.Audio < 0) || (b.Subtitle != nil && *b.Subtitle < -1) {
			http.Error(w, "Bad Request", 400)
			return
		}
		if b.Audio != nil || (b.Subtitle != nil && *b.Subtitle >= 0) {
			if i.host.Experience == nil {
				http.Error(w, "Unavailable", 503)
				return
			}
			detail, e := i.host.Experience.Item(r.Context(), frontend.ItemRequest{Viewer: v, Device: d, Key: key})
			if e != nil {
				http.NotFound(w, r)
				return
			}
			tracks := detail.Tracks
			if target.Version != "" {
				for _, version := range detail.Versions {
					if version.ID == target.Version {
						tracks = version.Media.Streams
						break
					}
				}
			}
			resolve := func(kind string, ordinal int) (int, bool) {
				for _, track := range tracks {
					if track.Type == kind {
						if ordinal == 0 {
							return track.Index, true
						}
						ordinal--
					}
				}
				return 0, false
			}
			if b.Audio != nil {
				var ok bool
				if options.AudioStream, ok = resolve("audio", *b.Audio); !ok {
					http.Error(w, "Audio unavailable", 422)
					return
				}
			}
			if b.Subtitle != nil && *b.Subtitle >= 0 {
				var ok bool
				if options.SubtitleStream, ok = resolve("subtitle", *b.Subtitle); !ok {
					http.Error(w, "Subtitle unavailable", 422)
					return
				}
			}
		}
	}
	pos := time.Duration(b.Position * float64(time.Second))
	plan, err := i.host.Sessions.Plan(r.Context(), frontend.PlanRequest{Viewer: v, Device: d, Protocol: "silo", Item: key, Version: versionHandle, Caps: cap.Client, Intent: frontend.PlayIntent{ExactVersion: versionHandle != "", Options: options, AllowTranscoding: &hls, AudioSet: b.Audio != nil, SubtitleSet: b.Subtitle != nil, Position: pos, PlaybackID: b.Attempt, ClientSessionID: b.InstallationID}})
	if err != nil {
		if versionHandle != "" && !errors.Is(err, frontend.ErrSessionBusy) && !errors.Is(err, frontend.ErrTranscodeCapacity) {
			versionUnavailable(w)
			return
		}
		playbackError(w, err)
		return
	}
	if !siloSourceHasKnownHDR10Plus(plan.Media) {
		http.Error(w, "Source HDR metadata unavailable", http.StatusServiceUnavailable)
		return
	}
	session, err := i.host.Sessions.Open(r.Context(), frontend.OpenRequest{Protocol: "silo", PlanID: plan.ID, Viewer: v, Device: d})
	if err != nil {
		if versionHandle != "" && !errors.Is(err, frontend.ErrSessionBusy) && !errors.Is(err, frontend.ErrTranscodeCapacity) {
			versionUnavailable(w)
			return
		}
		playbackError(w, err)
		return
	}
	access := frontend.SessionAccess{Viewer: v, Device: d, Protocol: "silo"}
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
		_ = i.host.Sessions.Stop(r.Context(), access, session.ID)
		playbackError(w, err)
		return
	}
	if !strings.HasPrefix(media, "/sessions/media/") {
		_ = i.host.Sessions.Stop(r.Context(), access, session.ID)
		http.Error(w, "Playback unavailable", 503)
		return
	}
	media = "/api/v2/stream/" + strings.TrimPrefix(media, "/sessions/media/")
	source := map[string]any{"media_file_id": b.FileID, "duration_seconds": plan.Media.Duration.Seconds(), "container": plan.Media.Container, "bitrate_kbps": plan.Media.BitrateKbps, "hdr10_plus": false, "dv_enhancement_layer": "unknown"}
	recipe := map[string]any{"video_codec": plan.Decision.VideoCodec, "audio_codec": plan.Decision.AudioCodec, "bitrate_kbps": plan.Decision.BitrateKbps}
	hdr10, hdr10Plus, hlg, dv := false, false, false, false
	for _, s := range plan.Media.Streams {
		if s.Type == "video" {
			if s.HDR10Plus != nil {
				source["hdr10_plus"] = *s.HDR10Plus
			}
			if s.DolbyVisionELPresent != nil && !*s.DolbyVisionELPresent {
				source["dv_enhancement_layer"] = "none"
			}
			if plan.Decision.VideoCopy && !plan.Decision.ToneMap {
				hdr10 = s.ColorTransfer == "smpte2084"
				hdr10Plus = s.HDR10Plus != nil && *s.HDR10Plus
				hlg = s.ColorTransfer == "arib-std-b67"
				dv = s.DolbyVision && !plan.Decision.StripDolbyVision
			}
			source["video_codec"] = s.Codec
			source["width"] = s.Width
			source["height"] = s.Height
			source["bit_depth"] = s.BitDepth
			source["video_profile"] = s.Profile
			source["video_level"] = s.Level
			source["frame_rate"] = s.FrameRate
			if s.ColorRange != "" {
				source["color_range"] = s.ColorRange
			}
			rangeName := ""
			switch {
			case s.DolbyVision:
				rangeName = "dolby_vision"
			case s.ColorTransfer == "smpte2084":
				rangeName = "hdr10"
			case s.ColorTransfer == "arib-std-b67":
				rangeName = "hlg"
			case s.ColorTransfer == "bt709":
				rangeName = "sdr"
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
			} else if plan.Decision.ToneMap {
				recipe["dynamic_range"] = "sdr"
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
			entry["url"] = "/api/v2/stream/" + strings.TrimPrefix(url, "/sessions/media/")
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
				_ = i.host.Sessions.Stop(r.Context(), access, session.ID)
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
	output := map[string]any{"protocol_version": 3, "plan_id": plan.ID, "session_id": session.ID, "delivery": delivery, "plan_attempt_key": plan.ID, "stream": stream, "timeline": timeline, "selected_tracks": selected, "effective_recipe": recipe, "claims": map[string]any{"video": map[string]any{"hdr10": hdr10, "hdr10_plus": hdr10Plus, "hlg": hlg, "dolby_vision": dv}, "audio": audioClaim, "subtitles": map[string]any{"ass_styling_preserved": false, "bitmap_overlay": false, "bitmap_sidecar": false}}, "subtitle": subtitle, "transformations": []any{}, "applied_quirks": []any{}, "runtime_corrections": []any{}, "degradation_warnings": []any{}, "decision_reason": plan.Decision.Reason, "requested_media_file_id": b.FileID, "effective_media_file_id": b.FileID, "source": source, "subtitle_fidelity_policy": "allow_simplified_rendering", "available_qualities": []any{}}
	reply(w, 201, map[string]any{"outcome": "playable", "protocol_version": 3, "server_features": []string{"playback_plan_v3", "neutral_playback_v3_contract_v1", "header_authenticated_media_v1", "sequenced_progress_v1"}, "session_id": session.ID, "playback_plan": output})
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
	state := "playing"
	if *b.Paused {
		state = "paused"
	}
	if stop {
		state = "stopped"
	}
	err = i.host.Sessions.Report(r.Context(), access, sessionID, frontend.Timeline{PlaybackID: sessionID, ClientSessionID: b.InstallationID, Position: time.Duration(*b.Position * float64(time.Second)), Sequence: b.Sequence, State: state})
	if err != nil {
		http.Error(w, "Unavailable", 503)
		return
	}
	outcome := "applied"
	if stop {
		outcome = "stopped"
	}
	response := map[string]any{"outcome": outcome, "accepted": map[string]any{"position": *b.Position, "sequence": b.Sequence, "is_paused": *b.Paused}}
	if b.StopID != "" {
		response["stop_id"] = b.StopID
	}
	reply(w, 200, response)
}

func playbackError(w http.ResponseWriter, err error) {
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
	reply(w, 200, map[string]any{"revision": "ether-playback-v3", "state": state, "allowed": allowed, "installation_id": i.installationID(), "protocol_versions": []int{3}, "features": []string{"playback_plan_v3", "neutral_playback_v3_contract_v1", "header_authenticated_media_v1", "sequenced_progress_v1"}, "deliveries": []string{"original_http", "server_transcode_hls", "server_remux_hls"}})
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

func (i *instance) playbackRoutes() error {
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
	for _, method := range []string{"GET", "HEAD"} {
		if err := i.router.Handle(method, "/api/v2/stream/{path...}", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if i.host.Sessions == nil {
				http.Error(w, "Unavailable", 503)
				return
			}
			viewer, _, err := i.deviceViewer(r)
			if err != nil || viewer == nil {
				http.Error(w, "Unauthorized", 401)
				return
			}
			parts := strings.SplitN(r.PathValue("path"), "/", 2)
			if len(parts) != 2 {
				http.NotFound(w, r)
				return
			}
			media, ok := i.host.Sessions.(frontend.ScopedMedia)
			if !ok {
				http.Error(w, "Unavailable", 503)
				return
			}
			forward := r.Clone(r.Context())
			u := *r.URL
			u.Path = "/sessions/media/" + r.PathValue("path")
			u.RawPath = ""
			forward.URL = &u
			media.ServeMediaAccess(w, forward, frontend.SessionAccess{Viewer: viewer, Device: device(r), Protocol: "silo"})
		})); err != nil {
			return err
		}
	}
	return nil
}
