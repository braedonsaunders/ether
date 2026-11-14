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
	Device           device   `json:"device"`
	ProtocolVersions []int    `json:"protocolVersions"`
	Containers       []string `json:"containers"`
	VideoCodecs      []string `json:"videoCodecs"`
	AudioCodecs      []string `json:"audioCodecs"`
	SubtitleFormats  []string `json:"subtitleFormats"`
	HLS              bool     `json:"hls"`
	MSE              bool     `json:"mse"`
	MaxWidth         int      `json:"maxWidth"`
	MaxHeight        int      `json:"maxHeight"`
	MaxBitrate       int      `json:"maxBitrate"`
	HDR              []string `json:"hdr"`
}
type playSession struct {
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
	v, _, e := i.viewer(r)
	if e != nil {
		fail(w, e)
		return
	}
	var b struct {
		TitleID, ChannelID, ExtraID string
		Client                      client
		PositionSeconds             float64
		AllowTranscode              *bool
		AudioTrackID                string
		SubtitleTrackID             json.RawMessage
	}
	if body(r, &b) != nil || b.Client.Device.ID != requestDevice(r).ID || (b.TitleID == "") == (b.ChannelID == "") || b.ExtraID != "" {
		problem(w, 400, "request", "invalid_target")
		return
	}
	pos, ok := seconds(b.PositionSeconds)
	if !ok {
		problem(w, 400, "request", "invalid_position")
		return
	}
	if b.AudioTrackID != "" || len(b.SubtitleTrackID) > 0 {
		i.unavailable(w, r)
		return
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
	}
	plan, e := i.host.Sessions.Plan(r.Context(), frontend.PlanRequest{Protocol: "vela", Viewer: v, Device: requestDevice(r), Item: item, Channel: b.ChannelID, Caps: declared.Client, Intent: frontend.PlayIntent{Position: pos, AllowTranscoding: b.AllowTranscode, DeclaredCaps: true}})
	if e != nil {
		fail(w, e)
		return
	}
	if plan.Estimated {
		problem(w, 503, "source", "source_unresolved")
		return
	}
	s, e := i.host.Sessions.Open(r.Context(), frontend.OpenRequest{Protocol: "vela", PlanID: plan.ID, Viewer: v, Device: requestDevice(r)})
	if e != nil {
		fail(w, e)
		return
	}
	format := frontend.StreamDirect
	transport := "file"
	mime := "video/" + plan.Media.Container
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
	path := object{"id": "primary", "kind": kind, "transport": transport, "url": url, "mimeType": mime, "readiness": "ready", "qualities": []object{}, "boundaries": []object{}}
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
		t := object{"id": strconv.Itoa(track.Index), "kind": track.Type, "language": track.Language, "name": track.Title, "codec": track.Codec, "default": track.Default, "forced": track.Forced}
		if track.Type == "audio" {
			audio = append(audio, t)
			if track.Index == plan.Decision.AudioStream {
				selectedAudio = t["id"].(string)
			}
		} else {
			url, err := i.host.Sessions.SubtitleURL(r.Context(), i.access(r, v), s.ID, track.Index, "vtt")
			if err != nil {
				continue
			}
			t["url"] = url
			subs = append(subs, t)
			if track.Index == plan.Decision.SubtitleStream {
				selectedSub = t["id"]
			}
		}
	}
	p := object{"sessionId": s.ID, "revision": uint64(1), "sourceGeneration": uint64(1), "seekGeneration": uint64(1), "state": "ready", "positionSeconds": pos.Seconds(), "durationSeconds": plan.Media.Duration.Seconds(), "paths": []object{path}, "selectedPathId": "primary", "audioTracks": audio, "subtitleTracks": subs, "selectedSubtitleTrackId": selectedSub, "chapters": []object{}, "fallback": object{"state": "unsupported"}}
	if selectedAudio != "" {
		p["selectedAudioTrackId"] = selectedAudio
	}
	if b.TitleID != "" {
		p["titleId"] = b.TitleID
	} else {
		p["channelId"] = b.ChannelID
	}
	ps := &playSession{viewer: v.ID, device: requestDevice(r).ID, plan: p, source: 1, seek: 1, revision: 1, cursor: 1, signal: make(chan struct{}), position: pos}
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
}
func (i *instance) session(w http.ResponseWriter, r *http.Request) (*frontend.Viewer, *playSession) {
	v, _, e := i.viewer(r)
	if e != nil {
		fail(w, e)
		return nil, nil
	}
	i.mu.Lock()
	s := i.sessions[r.PathValue("id")]
	i.mu.Unlock()
	if s == nil || s.viewer != v.ID || s.device != requestDevice(r).ID {
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
	id := r.PathValue("id")
	switch b.Kind {
	case "stop":
		if e := i.host.Sessions.Stop(r.Context(), i.access(r, v), id); e != nil {
			fail(w, e)
			return
		}
		s.ended = time.Now()
		s.plan["state"] = "ended"
	case "seek":
		pos, ok := seconds(b.PositionSeconds)
		if !ok {
			problem(w, 400, "request", "invalid_position")
			return
		}
		paths := s.plan["paths"].([]object)
		if paths[0]["transport"] == "hls" {
			url, e := i.host.Sessions.StartStream(r.Context(), i.access(r, v), id, frontend.StreamRequest{Format: frontend.StreamHLS, Position: pos})
			if e != nil {
				fail(w, e)
				return
			}
			paths[0]["url"] = url
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
	if b.Sequence <= s.sequence {
		send(w, 200, object{"accepted": true})
		return
	}
	pos, ok := seconds(b.PositionSeconds)
	if !ok || b.SelectedPathID != "primary" {
		problem(w, 400, "request", "invalid_observation")
		return
	}
	switch b.State {
	case "playing", "paused", "buffering", "ended":
	default:
		problem(w, 400, "request", "invalid_state")
		return
	}
	if e := i.host.Sessions.Report(r.Context(), i.access(r, v), r.PathValue("id"), frontend.Timeline{Sequence: b.Sequence, Position: pos, State: b.State}); e != nil {
		fail(w, e)
		return
	}
	s.sequence = b.Sequence
	s.position = pos
	if b.State == "ended" {
		if e := i.host.Sessions.Stop(r.Context(), i.access(r, v), r.PathValue("id")); e != nil {
			fail(w, e)
			return
		}
		s.ended = time.Now()
		s.plan["state"] = "ended"
		s.publish("ended")
	} else if b.State != "buffering" {
		s.plan["state"] = b.State
		s.publish("plan")
	}
	send(w, 200, object{"accepted": true})
}
func (i *instance) events(w http.ResponseWriter, r *http.Request) {
	_, s := i.session(w, r)
	if s == nil {
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
		if _, _, err := i.viewer(r); err != nil {
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
