package vela

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/kit/caps"
	"github.com/braedonsaunders/ether/internal/frontend/kit/ids"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

type client struct {
	OriginalPlayback *frontend.OriginalPlaybackCapabilities `json:"originalPlayback,omitempty"`
	Device           device                                 `json:"device"`
	ProtocolVersions []int                                  `json:"protocolVersions"`
	Containers       []string                               `json:"containers"`
	VideoCodecs      []string                               `json:"videoCodecs"`
	AudioCodecs      []string                               `json:"audioCodecs"`
	SubtitleFormats  []string                               `json:"subtitleFormats"`
	HLS              bool                                   `json:"hls"`
	MSE              bool                                   `json:"mse"`
	MaxWidth         int                                    `json:"maxWidth,omitempty"`
	MaxHeight        int                                    `json:"maxHeight,omitempty"`
	MaxBitrate       int                                    `json:"maxBitrate,omitempty"`
	HDR              []string                               `json:"hdr,omitempty"`
}
type playSession struct {
	scope string

	mu                               sync.Mutex
	viewer                           int64
	device                           string
	plan                             object
	source, seek, revision, sequence uint64
	position                         time.Duration
	cursor                           uint64
	events                           []object
	signal                           chan struct{}
	stream                           context.CancelFunc
	ended                            time.Time
	previewExpires                   time.Time
}

func seconds(n float64) (time.Duration, bool) {
	if math.IsNaN(n) || math.IsInf(n, 0) || n < 0 || n > float64(math.MaxInt64)/float64(time.Second) {
		return 0, false
	}
	return time.Duration(n * float64(time.Second)), true
}
func (i *instance) access(r *http.Request, v *frontend.Viewer) frontend.SessionAccess {
	return frontend.SessionAccess{Viewer: v, Device: requestDevice(r), Protocol: "vela"}
}
func (i *instance) playback(w http.ResponseWriter, r *http.Request) {
	v, token, e := i.viewer(r)
	if e != nil {
		fail(w, e)
		return
	}
	var b struct {
		TitleID, ChannelID, ExtraID string
		LiveMode                    string
		Client                      client
		PositionSeconds             *float64
		AllowTranscode              *bool
		AudioTrackID                string
		SubtitleTrackID             json.RawMessage
	}
	if body(r, &b) != nil || b.Client.Device.ID != requestDevice(r).ID {
		problem(w, 400, "request", "invalid_target")
		return
	}
	targets := 0
	for _, id := range []string{b.TitleID, b.ChannelID, b.ExtraID} {
		if id != "" {
			targets++
		}
	}
	if targets != 1 {
		problem(w, 400, "request", "invalid_target")
		return
	}
	if b.ExtraID != "" {
		if _, err := ids.ParseRatingKeyGUID(b.ExtraID); err != nil {
			problem(w, 400, "request", "invalid_target")
			return
		}
		i.unavailable(w, r)
		return
	}
	if b.LiveMode != "" && b.LiveMode != "adaptive" && b.LiveMode != "low-delay" {
		problem(w, 400, "request", "invalid_live_mode")
		return
	}
	var pos time.Duration
	if b.PositionSeconds != nil {
		var ok bool
		pos, ok = seconds(*b.PositionSeconds)
		if !ok {
			problem(w, 400, "request", "invalid_position")
			return
		}
	}
	scope, e := i.grantScope(r.Context(), token, requestDevice(r))
	if e != nil {
		fail(w, e)
		return
	}
	options := frontend.DecideOptions{DirectPlay: true, DirectStream: b.Client.HLS, AudioStream: -1, SubtitleStream: -1}
	audioSet, subtitleSet := false, false
	if b.AudioTrackID != "" {
		n, err := strconv.Atoi(b.AudioTrackID)
		if err != nil || n < 0 {
			problem(w, 400, "request", "invalid_audio_track")
			return
		}
		options.AudioStream = n
		audioSet = true
	}
	if len(b.SubtitleTrackID) > 0 {
		subtitleSet = true
		if string(b.SubtitleTrackID) != "null" {
			var id string
			if json.Unmarshal(b.SubtitleTrackID, &id) != nil {
				problem(w, 400, "request", "invalid_subtitle_track")
				return
			}
			n, err := strconv.Atoi(id)
			if err != nil || n < 0 {
				problem(w, 400, "request", "invalid_subtitle_track")
				return
			}
			options.SubtitleStream = n
		}
	}
	if i.host.Capabilities == nil {
		i.unavailable(w, r)
		return
	}
	profile := caps.DeviceProfile{MaxStreamingBitrate: b.Client.MaxBitrate, DirectPlayProfiles: []caps.DirectPlayProfile{{Type: "Video", Container: strings.Join(b.Client.Containers, ","), VideoCodec: strings.Join(b.Client.VideoCodecs, ","), AudioCodec: strings.Join(b.Client.AudioCodecs, ",")}}}
	if b.Client.HLS {
		profile.TranscodingProfiles = []caps.TranscodingProfile{{Type: "Video", Protocol: "hls", Container: "ts", VideoCodec: strings.Join(b.Client.VideoCodecs, ","), AudioCodec: strings.Join(b.Client.AudioCodecs, ",")}}
	}
	for _, format := range b.Client.SubtitleFormats {
		profile.SubtitleProfiles = append(profile.SubtitleProfiles, caps.SubtitleProfile{Format: format, Method: "External"})
	}
	declared, e := i.host.Capabilities.Build(i.identityContext(r), frontend.CapabilityRequest{Viewer: v, Device: caps.DeviceInfo{Protocol: "vela", Product: "Vela", Platform: b.Client.Device.Platform, Version: b.Client.Device.AppVersion, ClientID: b.Client.Device.ID, DeviceName: b.Client.Device.Name, VideoCodecs: b.Client.VideoCodecs, AudioCodecs: b.Client.AudioCodecs, Containers: b.Client.Containers, SubtitleCodecs: b.Client.SubtitleFormats, MaxHeight: b.Client.MaxHeight, MaxBitrateKbps: b.Client.MaxBitrate / 1000}, Profile: profile, UserAgent: r.UserAgent()})
	if e != nil {
		fail(w, e)
		return
	}
	var item frontend.RatingKey
	if b.TitleID != "" {
		item, e = ids.ParseRatingKeyGUID(b.TitleID)
		if e != nil {
			problem(w, 404, "not-found", "not_found")
			return
		}
		if item.Kind() == frontend.KindShow || item.Kind() == frontend.KindSeason {
			problem(w, 400, "request", "not_playable")
			return
		}
		// Omission delegates resume to current server-owned watch state. An
		// explicit zero is an independent start-over choice and skips this read.
		if b.PositionSeconds == nil {
			states, err := i.host.Experience.UserData(r.Context(), v, []frontend.RatingKey{item})
			if err != nil {
				fail(w, err)
				return
			}
			pos = states[item].ViewOffset
			if pos < 0 {
				problem(w, 503, "transient", "invalid_resume_position")
				return
			}
		}
	}
	plan, e := i.host.Sessions.Plan(r.Context(), frontend.PlanRequest{Protocol: "vela", Viewer: v, Device: requestDevice(r), Item: item, Channel: b.ChannelID, Caps: declared.Client, Intent: frontend.PlayIntent{Options: options, AudioSet: audioSet, SubtitleSet: subtitleSet, Position: pos, AllowTranscoding: b.AllowTranscode, DeclaredCaps: true, OriginalPlayback: b.Client.OriginalPlayback}})
	if e != nil {
		fail(w, e)
		return
	}
	if plan.Estimated {
		problem(w, 502, "source", "source_unresolved")
		return
	}
	s, e := i.host.Sessions.Open(r.Context(), frontend.OpenRequest{Protocol: "vela", PlanID: plan.ID, Viewer: v, Device: requestDevice(r)})
	if e != nil {
		fail(w, e)
		return
	}
	if b.ChannelID != "" && s.Media != nil && !s.Media.Estimated {
		plan.Media = *s.Media
	}
	format := frontend.StreamDirect
	transport := "file"
	mime := sourceMIME(plan.Media.Container)
	if plan.Decision.Kind != frontend.DecisionDirectPlay {
		if !b.Client.HLS || !plan.HLS {
			_ = i.host.Sessions.Stop(r.Context(), i.access(r, v), s.ID)
			problem(w, 422, "decoder", "unsupported_transport")
			return
		}
		format = frontend.StreamHLS
		transport = "hls"
		mime = "application/vnd.apple.mpegurl"
	}
	url, e := i.host.Sessions.StartStream(r.Context(), i.access(r, v), s.ID, frontend.StreamRequest{Format: format, Position: pos})
	if e != nil {
		_ = i.host.Sessions.Stop(r.Context(), i.access(r, v), s.ID)
		fail(w, e)
		return
	}
	kind := "direct"
	if plan.Decision.Kind == frontend.DecisionRemux {
		kind = "remux"
	}
	if plan.Decision.Kind == frontend.DecisionTranscode {
		kind = "transcode"
	}
	original := b.ChannelID == "" && plan.Decision.Reason == "original_runtime"
	path := object{"id": "primary", "kind": kind, "transport": transport, "url": url, "mimeType": mime, "readiness": "ready", "timelineOriginSeconds": float64(0), "qualities": []object{}, "boundaries": []object{}}
	if transport == "hls" {
		path["timelineOriginSeconds"] = pos.Seconds()
	}
	if plan.ETag != "" {
		path["timelineId"] = plan.ETag
	}
	if original {
		if facts := originalVideo(plan.Media); facts != nil {
			path["originalVideo"] = facts
		}
		path["original"], path["range"] = true, true
		if plan.Media.Size > 0 {
			path["byteLength"] = plan.Media.Size
		}
	}
	if plan.ETag != "" {
		path["sourceId"] = plan.ETag
	}
	audio, subs := []object{}, []object{}
	var selectedSub any
	selectedAudio := ""
	for _, track := range plan.Media.Streams {
		if track.Type != "audio" && track.Type != "subtitle" {
			continue
		}
		t := object{"id": strconv.Itoa(track.Index), "streamIndex": track.Index, "kind": track.Type, "language": track.Language, "name": track.Title, "codec": track.Codec, "default": track.Default, "forced": track.Forced}
		if track.Type == "audio" {
			audio = append(audio, t)
			if track.Index == plan.Decision.AudioStream {
				selectedAudio = t["id"].(string)
			}
		} else {
			url, err := i.host.Sessions.SubtitleURL(r.Context(), i.access(r, v), s.ID, track.Index, "vtt")
			if err != nil && !original {
				continue
			}
			if err == nil {
				t["url"] = url
			}
			if !original {
				t["codec"] = "webvtt"
			}
			subs = append(subs, t)
			if track.Index == plan.Decision.SubtitleStream {
				selectedSub = t["id"]
			}
		}
	}
	p := object{"sessionId": s.ID, "revision": uint64(1), "sourceGeneration": uint64(1), "seekGeneration": uint64(1), "state": "ready", "positionSeconds": pos.Seconds(), "durationSeconds": plan.Media.Duration.Seconds(), "paths": []object{path}, "selectedPathId": "primary", "audioTracks": audio, "subtitleTracks": subs, "selectedSubtitleTrackId": selectedSub, "chapters": chapters(plan.Media), "fallback": object{"state": "unsupported"}}
	if plan.Media.Duration <= 0 {
		delete(p, "durationSeconds")
	}
	if selectedAudio != "" {
		p["selectedAudioTrackId"] = selectedAudio
	}
	if b.TitleID != "" {
		p["titleId"] = b.TitleID
	} else {
		p["channelId"] = b.ChannelID
		mode := b.LiveMode
		if mode == "" {
			mode = "adaptive"
		}
		p["live"] = object{"mode": mode}
		delete(p, "durationSeconds")
	}
	if original && b.Client.HLS && plan.HLS {
		fallbackKind := "transcode"
		if plan.DirectStream {
			fallbackKind = "remux"
		}
		p["paths"] = []object{path, {"id": "fallback-hls", "kind": fallbackKind, "transport": "hls", "mimeType": "application/vnd.apple.mpegurl", "readiness": "on-demand", "sourceId": plan.ETag, "timelineId": plan.ETag, "qualities": []object{}, "boundaries": []object{}}}
	}
	ps := &playSession{scope: scope, viewer: v.ID, device: requestDevice(r).ID, plan: p, source: 1, seek: 1, revision: 1, cursor: 1, signal: make(chan struct{}), position: pos}
	i.mu.Lock()
	for id, old := range i.sessions {
		old.mu.Lock()
		expired := !old.ended.IsZero() && time.Since(old.ended) > 10*time.Minute
		old.mu.Unlock()
		if expired {
			delete(i.sessions, id)
		}
	}
	i.sessions[s.ID] = ps
	i.mu.Unlock()
	send(w, 200, p)
	i.watchSession(v, requestDevice(r), s.ID, scope, ps)
}
func (i *instance) session(w http.ResponseWriter, r *http.Request) (*frontend.Viewer, *playSession) {
	v, token, e := i.viewer(r)
	if e != nil {
		fail(w, e)
		return nil, nil
	}
	scope, e := i.grantScope(r.Context(), token, requestDevice(r))
	if e != nil {
		fail(w, e)
		return nil, nil
	}
	i.mu.Lock()
	s := i.sessions[r.PathValue("id")]
	if s != nil {
		s.mu.Lock()
		expired := (!s.ended.IsZero() && time.Since(s.ended) >= 10*time.Minute) || (!s.previewExpires.IsZero() && !s.previewExpires.After(time.Now()))
		s.mu.Unlock()
		if expired {
			delete(i.sessions, r.PathValue("id"))
			s = nil
		}
	}
	i.mu.Unlock()
	if s == nil || s.viewer != v.ID || s.device != requestDevice(r).ID || s.scope != scope {
		problem(w, 404, "not-found", "session_not_found")
		return nil, nil
	}
	return v, s
}
func (s *playSession) snapshot() object {
	return object{"cursor": strconv.FormatUint(s.cursor, 10), "revision": s.revision, "plan": s.plan, "publishedRanges": []object{}, "lastObservationSequence": s.sequence}
}
func (s *playSession) publish(kind string) {
	s.cursor++
	s.revision++
	s.plan["revision"] = s.revision
	s.plan["sourceGeneration"] = s.source
	s.plan["seekGeneration"] = s.seek
	s.plan["positionSeconds"] = s.position.Seconds()
	b, _ := json.Marshal(s.plan)
	var p object
	_ = json.Unmarshal(b, &p)
	event := object{"type": kind, "cursor": strconv.FormatUint(s.cursor, 10), "revision": s.revision, "sessionId": s.plan["sessionId"]}
	if kind == "ended" {
		event["reason"] = "stopped"
	} else {
		event["plan"] = p
	}
	s.events = append(s.events, event)
	if len(s.events) > 64 {
		s.events = s.events[len(s.events)-64:]
	}
	close(s.signal)
	s.signal = make(chan struct{})
}
func (i *instance) snapshot(w http.ResponseWriter, r *http.Request) {
	_, s := i.session(w, r)
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	send(w, 200, s.snapshot())
}
func stale(w http.ResponseWriter, s *playSession, source, seek uint64) bool {
	if source == s.source && seek == s.seek {
		return false
	}
	send(w, 409, object{"error": object{"class": "conflict", "code": "stale_generation", "message": "stale generation", "retry": object{"allowed": false}, "currentGenerations": object{"sourceGeneration": s.source, "seekGeneration": s.seek}}})
	return true
}
func (i *instance) command(w http.ResponseWriter, r *http.Request) {
	v, s := i.session(w, r)
	if s == nil {
		return
	}
	var b struct {
		Kind, PathID, AudioTrackID       string
		AtSeconds                        *float64
		SubtitleTrackID                  json.RawMessage
		PositionSeconds                  float64
		SourceGeneration, SeekGeneration uint64
	}
	if body(r, &b) != nil {
		problem(w, 400, "request", "invalid_request")
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if stale(w, s, b.SourceGeneration, b.SeekGeneration) {
		return
	}
	if !s.ended.IsZero() {
		problem(w, 409, "conflict", "session_ended")
		return
	}
	if s.plan["purpose"] == "preview" && b.Kind != "stop" {
		problem(w, 400, "request", "preview_command_unsupported")
		return
	}
	id := r.PathValue("id")
	switch b.Kind {
	case "stop":
		if e := i.host.Sessions.Stop(r.Context(), i.access(r, v), id); e != nil {
			fail(w, e)
			return
		}
		s.ended = time.Now()
		s.plan["state"] = "ended"
	case "switch":
		pos, ok := seconds(b.PositionSeconds)
		if b.AtSeconds != nil {
			pos, ok = seconds(*b.AtSeconds)
		}
		if !ok {
			problem(w, 400, "request", "invalid_position")
			return
		}
		paths := s.plan["paths"].([]object)
		selected := -1
		for n, path := range paths {
			if path["id"] == b.PathID {
				selected = n
				break
			}
		}
		if selected < 0 {
			problem(w, 400, "request", "invalid_path")
			return
		}
		path := paths[selected]
		format := frontend.StreamDirect
		if path["transport"] == "hls" {
			format = frontend.StreamHLS
		}
		url, err := i.host.Sessions.StartStream(r.Context(), i.access(r, v), id, frontend.StreamRequest{Format: format, Position: pos})
		if err != nil {
			fail(w, err)
			return
		}
		for _, previous := range paths {
			if previous["id"] != b.PathID {
				delete(previous, "url")
				previous["readiness"] = "on-demand"
			}
		}
		path["url"], path["readiness"] = url, "ready"
		path["timelineOriginSeconds"] = float64(0)
		if format == frontend.StreamHLS {
			path["timelineOriginSeconds"] = pos.Seconds()
		}
		s.plan["selectedPathId"] = b.PathID
		s.position = pos
		s.seek++
	case "seek":
		pos, ok := seconds(b.PositionSeconds)
		if b.AtSeconds != nil {
			pos, ok = seconds(*b.AtSeconds)
		}
		if !ok {
			problem(w, 400, "request", "invalid_position")
			return
		}
		paths := s.plan["paths"].([]object)
		path := selectedPlaybackPath(s.plan, paths)
		if path["transport"] == "hls" {
			url, e := i.host.Sessions.StartStream(r.Context(), i.access(r, v), id, frontend.StreamRequest{Format: frontend.StreamHLS, Position: pos})
			if e != nil {
				fail(w, e)
				return
			}
			path["url"] = url
			path["timelineOriginSeconds"] = pos.Seconds()
		}
		s.seek++
		s.position = pos
	case "pause", "resume":
		state := "paused"
		if b.Kind == "resume" {
			state = "playing"
		}
		if e := i.host.Sessions.Report(r.Context(), i.access(r, v), id, frontend.Timeline{Position: s.position, State: state}); e != nil {
			fail(w, e)
			return
		}
		s.plan["state"] = state
	default:
		i.unavailable(w, r)
		return
	}
	kind := "plan"
	if b.Kind == "stop" {
		kind = "ended"
	}
	s.publish(kind)
	send(w, 200, s.plan)
}
func (i *instance) observe(w http.ResponseWriter, r *http.Request) {
	v, s := i.session(w, r)
	if s == nil {
		return
	}
	var b struct {
		SourceGeneration, SeekGeneration, Sequence uint64
		PositionSeconds                            float64
		State, SelectedPathID                      string
		AudioTrackID                               *string
		SubtitleTrackID                            json.RawMessage
	}
	if body(r, &b) != nil {
		problem(w, 400, "request", "invalid_request")
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.plan["purpose"] == "preview" {
		problem(w, 400, "request", "preview_observation_unsupported")
		return
	}
	if stale(w, s, b.SourceGeneration, b.SeekGeneration) {
		return
	}
	if !s.ended.IsZero() {
		send(w, 200, object{"accepted": true})
		return
	}
	if b.Sequence <= s.sequence {
		send(w, 200, object{"accepted": true})
		return
	}
	pos, ok := seconds(b.PositionSeconds)
	if !ok || b.SelectedPathID != s.plan["selectedPathId"] {
		problem(w, 400, "request", "invalid_observation")
		return
	}
	switch b.State {
	case "playing", "paused", "buffering", "ended":
	default:
		problem(w, 400, "request", "invalid_state")
		return
	}
	var audioIndex, subtitleIndex *int
	if b.AudioTrackID != nil {
		n, ok := offeredTrack(s.plan, "audioTracks", *b.AudioTrackID)
		if !ok {
			problem(w, 400, "request", "invalid_audio_track")
			return
		}
		audioIndex = &n
	}
	var subtitleID any
	if len(b.SubtitleTrackID) > 0 {
		n := -1
		if string(b.SubtitleTrackID) != "null" {
			var id string
			if json.Unmarshal(b.SubtitleTrackID, &id) != nil {
				problem(w, 400, "request", "invalid_subtitle_track")
				return
			}
			var ok bool
			n, ok = offeredTrack(s.plan, "subtitleTracks", id)
			if !ok {
				problem(w, 400, "request", "invalid_subtitle_track")
				return
			}
			subtitleID = id
		}
		subtitleIndex = &n
	}
	if e := i.host.Sessions.Report(r.Context(), i.access(r, v), r.PathValue("id"), frontend.Timeline{Sequence: b.Sequence, Position: pos, State: b.State, AudioStreamIndex: audioIndex, SubtitleStreamIndex: subtitleIndex}); e != nil {
		fail(w, e)
		return
	}
	s.sequence = b.Sequence
	s.position = pos
	if b.AudioTrackID != nil {
		s.plan["selectedAudioTrackId"] = *b.AudioTrackID
	}
	if len(b.SubtitleTrackID) > 0 {
		s.plan["selectedSubtitleTrackId"] = subtitleID
	}
	if b.State == "ended" {
		if e := i.host.Sessions.Stop(r.Context(), i.access(r, v), r.PathValue("id")); e != nil {
			fail(w, e)
			return
		}
		s.ended = time.Now()
		s.plan["state"] = "ended"
		s.publish("ended")
	} else {
		state := b.State
		if state == "buffering" {
			state = "recovering"
		}
		s.plan["state"] = state
		s.publish("plan")
	}
	send(w, 200, object{"accepted": true})
}
func (i *instance) events(w http.ResponseWriter, r *http.Request) {
	_, s := i.session(w, r)
	if s == nil {
		return
	}
	s.mu.Lock()
	preview := s.plan["purpose"] == "preview"
	s.mu.Unlock()
	if preview {
		problem(w, 400, "request", "preview_events_unsupported")
		return
	}
	flush, ok := w.(http.Flusher)
	if !ok {
		i.unavailable(w, r)
		return
	}
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	s.mu.Lock()
	if s.stream != nil {
		s.stream()
	}
	s.stream = cancel
	s.mu.Unlock()
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	cursor := r.Header.Get("Last-Event-ID")
	if cursor == "" {
		cursor = r.URL.Query().Get("cursor")
	}
	last, _ := strconv.ParseUint(cursor, 10, 64)
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		identity, ok := i.host.Identity.(frontend.GrantScopes)
		if !ok {
			return
		}
		if _, err := identity.GrantScopeCurrent(r.Context(), s.scope, requestDevice(r)); err != nil {
			return
		}
		s.mu.Lock()
		events := []object{}
		snapshot := last == 0 || last > s.cursor || (len(s.events) > 0 && last+uint64(len(s.events)) < s.cursor)
		if snapshot {
			events = append(events, object{"type": "snapshot", "cursor": strconv.FormatUint(s.cursor, 10), "revision": s.revision, "sessionId": s.plan["sessionId"], "snapshot": s.snapshot()})
			last = s.cursor
		} else {
			for _, e := range s.events {
				n, _ := strconv.ParseUint(e["cursor"].(string), 10, 64)
				if n > last {
					events = append(events, e)
					last = n
				}
			}
		}
		// Serialize while holding the plan lock; the snapshot shares the plan map.
		chunks := []string{}
		for _, e := range events {
			b, _ := json.Marshal(e)
			chunks = append(chunks, fmt.Sprintf("id: %s\nevent: %s\ndata: %s\n\n", e["cursor"], e["type"], b))
		}
		signal := s.signal
		ended := !s.ended.IsZero()
		s.mu.Unlock()
		for _, chunk := range chunks {
			if _, e := fmt.Fprint(w, chunk); e != nil {
				return
			}
		}
		flush.Flush()
		if ended {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-i.ctx.Done():
			return
		case <-signal:
		case <-ticker.C:
			if _, e := fmt.Fprint(w, ": heartbeat\n\n"); e != nil {
				return
			}
			flush.Flush()
		}
	}
}

func chapters(media frontend.MediaInfo) []object {
	out := []object{}
	for _, c := range media.Chapters {
		out = append(out, object{"id": strconv.FormatInt(c.ID, 10), "name": c.Title, "startSeconds": c.Start.Seconds(), "endSeconds": c.End.Seconds()})
	}
	return out
}

func offeredTrack(plan object, key, id string) (int, bool) {
	tracks, _ := plan[key].([]object)
	for _, track := range tracks {
		if track["id"] == id {
			n, err := strconv.Atoi(id)
			return n, err == nil && n >= 0
		}
	}
	return 0, false
}

func selectedPlaybackPath(plan object, paths []object) object {
	for _, p := range paths {
		if p["id"] == plan["selectedPathId"] {
			return p
		}
	}
	return object{}
}

func sourceMIME(container string) string {
	switch strings.ToLower(container) {
	case "mkv", "matroska":
		return "video/x-matroska"
	case "mp4", "mov", "m4v":
		return "video/mp4"
	case "webm":
		return "video/webm"
	case "mpegts":
		return "video/mp2t"
	case "avi":
		return "video/x-msvideo"
	}
	return "application/octet-stream"
}
