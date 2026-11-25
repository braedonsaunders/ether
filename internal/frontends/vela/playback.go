package vela

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/kit/ids"
	"github.com/braedonsaunders/ether/internal/frontend/kit/reading"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
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

	// Dolby Vision variants shown as Dolby Vision ("5", "8.1", "8.4") and
	// variants the client reshapes itself. Nil is a client that does not
	// declare them; an empty list declares none.
	DolbyVisionProfiles *[]string    `json:"dolbyVisionProfiles,omitempty"`
	DolbyVisionReshape  *[]string    `json:"dolbyVisionReshape,omitempty"`
	AudioOutput         *audioOutput `json:"audioOutput,omitempty"`
}

// audioOutput is what the client's audio route delivers. It is advisory: the
// route can change after negotiation, so it never limits what is played.
type audioOutput struct {
	Passthrough   []string `json:"passthrough"`
	Immersive     []string `json:"immersive"`
	MaxChannels   int      `json:"maxChannels"`
	MaxSampleRate int      `json:"maxSampleRate,omitempty"`
	Lossless      bool     `json:"lossless"`
}
type playSession struct {
	id               string
	item             frontend.RatingKey
	titleID          string
	channelID        string
	liveProgrammeKey string
	liveEvent        *frontend.LiveEventRef
	client           client
	declaredCaps     frontend.ClientCaps
	// active is the admitted version handle and the release the session
	// reads now; pollPlayingRelease moves it after an in-session failover.
	active activeVersion
	// versionsCold: the version menu was built before the title's search
	// finished; snapshots rebuild it from the cached results until it is not.
	versionsCold      bool
	qualityRungs      map[string]string
	scope             string
	media             frontend.MediaInfo
	subtitleFormats   []string
	canBurn           bool
	mse               bool
	preparation       []frontend.HLSPreparationEvent
	diagnostics       any
	observedRecords   []object
	diagnosticHistory []object
	clientErrors      []object
	// observedState and observedAt are the client's last reported player
	// state, logged on change so a rebuffer shows beside the server's log.
	observedState string
	observedAt    time.Time

	mediaReads       atomic.Int32
	lastActivityNano atomic.Int64

	commandActive                    bool
	stopping                         bool
	commandMu                        sync.Mutex
	mu                               sync.Mutex
	viewer                           int64
	device                           string
	accessViewer                     *frontend.Viewer
	accessDevice                     frontend.Device
	plan                             object
	source, seek, revision, sequence uint64
	observedSource                   uint64
	position                         time.Duration
	cursor                           uint64
	events                           []object
	signal                           chan struct{}
	stream                           context.CancelFunc
	streamSeq                        uint64
	streamOpen                       bool
	disconnectTimer                  *time.Timer
	ended                            time.Time
	previewExpires                   time.Time
	multiviewGroup                   string
	multiviewTile                    int
	trickplayCurrent                 trickplayOfferKey
	trickplayPublished               trickplayOfferKey
	trickplayLastPublish             time.Time
	trickplayGenerating              bool
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
		fail(w, e, r)
		return
	}
	var b struct {
		TitleID, ChannelID, ExtraID string
		VersionID                   string
		IdempotencyKey              string
		LiveMode                    string
		Client                      client
		PositionSeconds             *float64
		AllowTranscode              *bool
		AudioTrackID                string
		SubtitleTrackID             json.RawMessage
		Multiview                   *multiviewPlaybackRequest `json:"multiview,omitempty"`
		// Context: what this play started from (features.playContexts).
		Context *playContextRequest `json:"context,omitempty"`
		// Queued: the next item of the device's play queue, opened ahead of
		// time for gapless audio (features.queuedPlayback).
		Queued bool `json:"queued,omitempty"`
	}
	if body(r, &b) != nil || b.Client.Device.ID != requestDevice(r).ID {
		problem(w, 400, "request", "invalid_target")
		return
	}
	if b.Context != nil && b.TitleID != "" {
		i.recordPlayContext(r, v, *b.Context)
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
	if b.VersionID != "" {
		if b.TitleID == "" {
			problem(w, 400, "request", "invalid_request")
			return
		}
		if !i.versionsAvailable() {
			i.unavailable(w, r)
			return
		}
	}
	if b.ExtraID != "" {
		if _, err := ids.ParseRatingKeyGUID(b.ExtraID); err != nil {
			problem(w, 400, "request", "invalid_target")
			return
		}
		i.extraPlayback(w, r, v, token, b.ExtraID, b.IdempotencyKey, b.Client)
		return
	}
	if b.LiveMode != "" && b.LiveMode != "adaptive" && b.LiveMode != "low-delay" {
		problem(w, 400, "request", "invalid_live_mode")
		return
	}
	var mvTarget *frontend.MultiviewTarget
	if b.Multiview != nil {
		if b.ChannelID == "" {
			problem(w, 400, "request", "invalid_request")
			return
		}
		if !i.multiviewAvailable() {
			i.unavailable(w, r)
			return
		}
		var ok bool
		mvTarget, ok = parseMultiviewPlayback(b.Multiview)
		if !ok {
			problem(w, 400, "request", "invalid_multiview")
			return
		}
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
		fail(w, e, r)
		return
	}
	options := frontend.DecideOptions{MaxWidth: max(b.Client.MaxWidth, 0), MaxHeight: max(b.Client.MaxHeight, 0), MaxBitrateKbps: clientBitrateKbps(b.Client.MaxBitrate), DirectPlay: true, DirectStream: b.Client.HLS || b.Client.MSE, AudioStream: -1, SubtitleStream: -1}
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
	declared, e := i.clientCapabilities(r, v, b.Client)
	if e != nil {
		fail(w, e, r)
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
		if item.IsPluginKey() && i.isBookTitle(r, v, item) {
			problemMessage(w, 400, "request", "not_playable", "This title is a book and opens via reading rather than playback.")
			return
		}
		// Omission delegates resume to current server-owned watch state. An
		// explicit zero is an independent start-over choice and skips this read.
		if b.PositionSeconds == nil {
			states, err := i.host.Experience.UserData(r.Context(), v, []frontend.RatingKey{item})
			if err != nil {
				fail(w, err, r)
				return
			}
			pos = states[item].ViewOffset
			if pos < 0 {
				problem(w, 503, "transient", "invalid_resume_position")
				return
			}
		}
	}
	plan, e := i.host.Sessions.Plan(r.Context(), frontend.PlanRequest{Protocol: "vela", Viewer: v, Device: requestDevice(r), Item: item, Channel: b.ChannelID, Multiview: mvTarget, Version: b.VersionID, Caps: declared.Client, Intent: frontend.PlayIntent{Options: options, AudioSet: audioSet, SubtitleSet: subtitleSet, Position: pos, ExactVersion: b.VersionID != "", AllowTranscoding: b.AllowTranscode, DeclaredCaps: true, OriginalPlayback: b.Client.OriginalPlayback, Queued: b.Queued && b.TitleID != ""}})
	if e != nil {
		if b.VersionID != "" && errors.Is(e, frontend.ErrNotFound) {
			if _, itemErr := i.host.Experience.Item(frontend.WithColdDetail(r.Context(), item), frontend.ItemRequest{Viewer: v, Device: requestDevice(r), Key: item}); itemErr == nil {
				problem(w, 400, "request", "invalid_version")
				return
			}
		}
		i.failFor(w, v, e, r)
		return
	}
	if plan.Estimated {
		problem(w, 502, "source", "source_unresolved")
		return
	}
	s, e := i.host.Sessions.Open(r.Context(), frontend.OpenRequest{Protocol: "vela", PlanID: plan.ID, Viewer: v, Device: requestDevice(r)})
	if e != nil {
		var capErr *frontend.ProviderConnectionCapError
		if errors.As(e, &capErr) {
			sendProviderConnectionCap(w, capErr)
			return
		}
		if b.ChannelID != "" {
			if errors.Is(e, frontend.ErrTooManyTranscodes) || errors.Is(e, frontend.ErrTranscodeCapacity) || errors.Is(e, frontend.ErrStreamLimit) || errors.Is(e, frontend.ErrSessionBusy) {
				i.failFor(w, v, e, r)
				return
			}
			var mvErr *frontend.MultiviewError
			if mvTarget != nil && (errors.As(e, &mvErr) || errors.Is(e, frontend.ErrInvalidMultiview)) {
				i.failFor(w, v, e, r)
				return
			}
			detail := frontend.DiagnoseLiveTV("tune", e)
			send(w, 503, object{"error": object{"class": "source", "code": detail.Code, "message": detail.Message, "retry": object{"allowed": false}}})
		} else {
			i.failFor(w, v, e, r)
		}
		return
	}
	if b.ChannelID != "" && s.Media != nil && !s.Media.Estimated {
		plan.Media = *s.Media
	}
	audioOnly := b.ChannelID == "" && plan.Media.Video() == nil && plan.Media.HasPlayableMedia()
	format := frontend.StreamDirect
	transport := "file"
	mime := sourceMIME(plan.Media.Container)
	if audioOnly {
		mime = audioSourceMIME(plan.Media.Container, plan.Decision.AudioCodec)
	}
	if plan.Decision.Kind != frontend.DecisionDirectPlay {
		if audioOnly && plan.HTTP {
			format = frontend.StreamHTTP
			transport = "file"
			container := plan.HTTPContainer
			if container == "" {
				container = plan.Decision.Container
			}
			mime = audioSourceMIME(container, plan.Decision.AudioCodec)
		} else if (b.Client.HLS || b.Client.MSE) && plan.HLS {
			format = frontend.StreamHLS
			transport = "hls"
			mime = "application/vnd.apple.mpegurl"
		} else {
			_ = i.host.Sessions.Stop(r.Context(), i.access(r, v), s.ID)
			problem(w, 422, "decoder", "unsupported_transport")
			return
		}
	}
	url, e := i.host.Sessions.StartStream(r.Context(), i.access(r, v), s.ID, frontend.StreamRequest{Format: format, Position: pos, LazyStart: true, Continuous: true})
	if e != nil {
		_ = i.host.Sessions.Stop(r.Context(), i.access(r, v), s.ID)
		i.failFor(w, v, e, r)
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
		if !audioOnly {
			if facts := originalVideo(plan.Media); facts != nil {
				path["originalVideo"] = facts
			}
		}
		path["original"], path["range"] = true, true
		if plan.Media.Size > 0 {
			path["byteLength"] = plan.Media.Size
		}
	} else if audioOnly && transport == "file" {
		if format == frontend.StreamDirect {
			path["range"] = true
			if plan.Media.Size > 0 {
				path["byteLength"] = plan.Media.Size
			}
		} else if pos == 0 && plan.Media.Duration > 0 && plan.Media.Duration <= 30*time.Minute {
			path["range"] = true
		}
	}
	path["decisionReason"] = plan.Decision.Reason
	if plan.ETag != "" {
		path["sourceId"] = plan.ETag
	}
	_, trackChanges := i.host.Sessions.(frontend.StreamDecisionReader)
	canBurn := !audioOnly && trackChanges && plan.Transcoding && !plan.VideoTranscodingOff && plan.HLS && (b.Client.HLS || b.Client.MSE)
	selectedSubIndex := plan.SubtitleStream()
	if subtitleSet {
		selectedSubIndex = options.SubtitleStream
	}
	audio, subs, selectedAudio, selectedSub := i.playbackTracks(r.Context(), i.access(r, v), s.ID, plan.Media, plan.Decision, selectedSubIndex, original, b.Client.SubtitleFormats, canBurn)
	p := object{"sessionId": s.ID, "revision": uint64(1), "sourceGeneration": uint64(1), "seekGeneration": uint64(1), "state": "ready", "positionSeconds": pos.Seconds(), "durationSeconds": plan.Media.Duration.Seconds(), "paths": []object{path}, "selectedPathId": "primary", "audioTracks": audio, "subtitleTracks": subs, "selectedSubtitleTrackId": selectedSub, "chapters": chapters(plan.Media), "fallback": object{"state": "unsupported"}}
	if audioOnly {
		p["media"] = "audio"
		if q := audioQualityWire(plan.Media); q != nil {
			p["audioQuality"] = q
		}
		if i.host.Loudness != nil && s.Item != 0 {
			if l, ok := i.host.Loudness.ForSource(r.Context(), s.Item, frontend.LoudnessSource(plan.Media.Size)); ok {
				p["loudness"] = loudnessWire(l)
			}
		}
	}
	if plan.Media.Duration <= 0 {
		delete(p, "durationSeconds")
	}
	if selectedAudio != "" {
		p["selectedAudioTrackId"] = selectedAudio
	}
	if b.TitleID != "" {
		p["titleId"] = b.TitleID
		if !audioOnly {
			setSkipMarkers(p, i.skipMarkers(r.Context(), item, plan.Media))
		}
	} else {
		p["channelId"] = b.ChannelID
		mode := b.LiveMode
		if mode == "" {
			mode = "adaptive"
		}
		p["live"] = object{"mode": mode}
		if s.Multiview != nil {
			mvDoc := object{
				"groupId": s.Multiview.GroupID,
				"tile":    s.Multiview.Tile,
			}
			if s.Multiview.MaxHeight > 0 {
				mvDoc["maxHeight"] = s.Multiview.MaxHeight
			}
			p["multiview"] = mvDoc
		}
		delete(p, "durationSeconds")
	}
	if (original || (audioOnly && transport == "file")) && (b.Client.HLS || b.Client.MSE) && plan.HLS {
		fallbackKind := "transcode"
		if plan.DirectStream {
			fallbackKind = "remux"
		}
		p["paths"] = []object{path, {"id": "fallback-hls", "kind": fallbackKind, "transport": "hls", "mimeType": "application/vnd.apple.mpegurl", "readiness": "on-demand", "sourceId": plan.ETag, "timelineId": plan.ETag, "qualities": []object{}, "boundaries": []object{}}}
	}
	if b.TitleID != "" && !original {
		if choice := originalChoicePath(plan, audioOnly, b.Client); choice != nil {
			p["paths"] = append(p["paths"].([]object), choice)
		}
	}
	dev := requestDevice(r)
	mvGroup := ""
	mvTile := 0
	if s.Multiview != nil {
		mvGroup = s.Multiview.GroupID
		mvTile = s.Multiview.Tile
	}
	ps := &playSession{id: s.ID, item: item, titleID: b.TitleID, channelID: b.ChannelID, client: b.Client, declaredCaps: declared.Client, active: planActiveVersion(plan), scope: scope, canBurn: canBurn, media: plan.Media, subtitleFormats: append([]string(nil), b.Client.SubtitleFormats...), mse: b.Client.MSE, viewer: v.ID, device: dev.ID, accessViewer: v, accessDevice: dev, plan: p, source: 1, seek: 1, revision: 1, cursor: 1, signal: make(chan struct{}), position: pos, multiviewGroup: mvGroup, multiviewTile: mvTile}
	if b.ChannelID != "" && i.host.LiveEventLink != nil {
		if progKey, ref, err := i.host.LiveEventLink.LiveEventForChannel(r.Context(), v, b.ChannelID); err == nil {
			ps.liveProgrammeKey = progKey
			ps.liveEvent = cloneLiveEventRef(ref)
			if ref != nil {
				p["liveEvent"] = liveEventObject(ref)
			}
		}
	}
	ps.noteActivity()

	if !plan.SourceChosenAt.IsZero() {
		ps.preparation = append(ps.preparation, frontend.HLSPreparationEvent{Stage: "source-chosen", At: plan.SourceChosenAt})
	}
	if !plan.DecisionAt.IsZero() {
		ps.preparation = append(ps.preparation, frontend.HLSPreparationEvent{Stage: "decision", At: plan.DecisionAt})
	}
	if transport == "hls" {
		i.refreshTrackDelivery(r.Context(), i.access(r, v), s.ID, ps)
	}
	i.addQualityPaths(r.Context(), i.access(r, v), s.ID, ps)
	i.refreshOutput(r.Context(), s.ID, ps)
	if b.ChannelID != "" {
		alignUnobservedLivePosition(ps)
	}
	if b.TitleID != "" && i.versionsAvailable() {
		ps.versionsCold = i.attachPlaybackVersions(r.Context(), v, dev, item, declared.Client, planActiveVersion(plan), p)
	}
	i.refreshInitialTrickplay(r.Context(), i.access(r, v), s.ID, ps)
	i.mu.Lock()
	for id, old := range i.sessions {
		old.mu.Lock()
		expired := !old.ended.IsZero() && time.Since(old.ended) > 10*time.Minute
		if expired {
			old.clearDisconnectTimerLocked()
		}
		old.mu.Unlock()
		if expired {
			delete(i.sessions, id)
		}
	}
	i.sessions[s.ID] = ps
	i.mu.Unlock()
	send(w, 200, p)
	i.watchSession(v, dev, s.ID, scope, ps)
}
func (i *instance) session(w http.ResponseWriter, r *http.Request) (*frontend.Viewer, *playSession) {
	v, token, e := i.viewer(r)
	if e != nil {
		fail(w, e, r)
		return nil, nil
	}
	dev := requestDevice(r)
	scope, e := i.grantScope(r.Context(), token, dev)
	if e != nil {
		fail(w, e, r)
		return nil, nil
	}
	id := r.PathValue("id")
	i.mu.Lock()
	s := i.sessions[id]
	if s != nil {
		s.mu.Lock()
		expired := (!s.ended.IsZero() && time.Since(s.ended) >= 10*time.Minute) || (!s.previewExpires.IsZero() && !s.previewExpires.After(time.Now()))
		if expired {
			s.clearDisconnectTimerLocked()
		}
		s.mu.Unlock()
		if expired {
			delete(i.sessions, id)
			s = nil
		}
	}
	i.mu.Unlock()
	if s == nil || s.viewer != v.ID || s.device != dev.ID || s.scope != scope {
		problem(w, 404, "not-found", "session_not_found")
		return nil, nil
	}
	s.noteActivity()
	s.mu.Lock()
	if s.id == "" {
		s.id = id
	}
	s.accessViewer = v
	s.accessDevice = dev
	s.mu.Unlock()
	return v, s
}
func (s *playSession) snapshot() object {
	out := object{"cursor": strconv.FormatUint(s.cursor, 10), "revision": s.revision, "plan": s.plan, "publishedRanges": []object{}, "lastObservationSequence": s.sequence, "diagnostics": object{"encoder": s.diagnostics, "media": s.observedRecords, "history": s.diagnosticHistory, "clientErrors": s.clientErrors}}
	if s.channelID != "" {
		out["liveEvent"] = liveEventObject(s.liveEvent)
	}
	return out
}
func (s *playSession) publish(kind string) {
	s.cursor++
	s.revision++
	s.plan["revision"] = s.revision
	s.plan["sourceGeneration"] = s.source
	s.plan["seekGeneration"] = s.seek
	s.plan["positionSeconds"] = s.position.Seconds()
	if kind == "plan" && s.trickplayCurrent != s.trickplayPublished {
		s.trickplayPublished = s.trickplayCurrent
		s.trickplayLastPublish = time.Now()
	}
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
	v, s := i.session(w, r)
	if s == nil {
		return
	}
	i.refreshColdVersions(r, v, s)
	i.pollPlayingRelease(r.Context(), v, requestDevice(r), r.PathValue("id"), s)
	i.refreshSessionLiveEvent(r.Context(), v, s)
	i.refreshSessionTrickplay(r.Context(), i.access(r, v), r.PathValue("id"), s)
	s.mu.Lock()
	defer s.mu.Unlock()
	send(w, 200, s.snapshot())
}

// refreshColdVersions rebuilds a version menu that was built before the
// title's search finished. It reads the cached search only; it never starts
// one.
func (i *instance) refreshColdVersions(r *http.Request, v *frontend.Viewer, s *playSession) {
	s.mu.Lock()
	cold, item, caps, active := s.versionsCold, s.item, s.declaredCaps, s.active
	s.mu.Unlock()
	if !cold || item == 0 || v == nil {
		return
	}
	versionPlan := object{}
	stillCold := i.attachPlaybackVersions(r.Context(), v, requestDevice(r), item, caps, active, versionPlan)
	if _, ok := versionPlan["versions"]; !ok {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.active.release.ID != active.release.ID {
		return // a failover moved the session meanwhile; its refresh wins
	}
	applyVersionPlan(s.plan, versionPlan)
	s.versionsCold = stillCold
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
		VersionID                        string
		MaxBitrate                       int
		MaxHeight                        *int
		AtSeconds                        *float64
		SubtitleTrackID                  json.RawMessage
		PositionSeconds                  *float64
		SourceGeneration, SeekGeneration *uint64
	}
	if body(r, &b) != nil {
		problem(w, 400, "request", "invalid_request")
		return
	}
	s.commandMu.Lock()
	defer s.commandMu.Unlock()
	s.mu.Lock()
	s.commandActive = true
	defer func() { s.commandActive = false; s.mu.Unlock() }()
	var srcGen, seekGen uint64
	if b.SourceGeneration != nil {
		srcGen = *b.SourceGeneration
	}
	if b.SeekGeneration != nil {
		seekGen = *b.SeekGeneration
	}
	if !(b.Kind == "replan" && b.SourceGeneration == nil && b.SeekGeneration == nil) {
		if stale(w, s, srcGen, seekGen) {
			return
		}
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
	if _, extra := s.plan["extraId"]; extra {
		if b.Kind != "stop" && b.Kind != "seek" && b.Kind != "pause" && b.Kind != "resume" {
			problem(w, 400, "request", "extra_command_unsupported")
			return
		}
	}
	switch b.Kind {
	case "stop":
		s.stopping = true
		defer func() { s.stopping = false }()
		var e error
		if !s.stopPort(w, func() { e = i.host.Sessions.Stop(r.Context(), i.access(r, v), id) }) {
			return
		}
		if e != nil {
			fail(w, e, r)
			return
		}
		s.ended = time.Now()
		s.clearDisconnectTimerLocked()
		s.plan["state"] = "ended"
	case "prepare-fallback":
		if !i.prepareFallback(w, r, v, s, id, b.MaxBitrate) {
			return
		}
	case "switch":
		var pos time.Duration
		ok := true
		if b.PositionSeconds != nil {
			pos, ok = seconds(*b.PositionSeconds)
		}
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
		if rung := s.rungFor(path); rung != "" {
			allowed := false
			if !s.port(w, func() { allowed = i.qualityAllowed(r.Context(), i.access(r, v), id, rung) }) {
				return
			}
			if !allowed {
				problem(w, 403, "policy", "quality_unavailable")
				return
			}
		}
		format := frontend.StreamDirect
		if path["transport"] == "hls" {
			format = frontend.StreamHLS
		} else if path["transport"] == "file" && (path["kind"] == "transcode" || path["kind"] == "remux") {
			format = frontend.StreamHTTP
		}
		origin := pos
		if fallback, ok := s.plan["fallback"].(object); ok && fallback["state"] == "ready" && fallback["pathId"] == b.PathID {
			if at, ok := path["timelineOriginSeconds"].(float64); ok {
				if prepared, valid := seconds(at); valid && prepared <= pos {
					origin = prepared
				}
			}
		}
		var url string
		var err error
		if !s.port(w, func() {
			url, err = i.host.Sessions.StartStream(r.Context(), i.access(r, v), id, frontend.StreamRequest{Format: format, Position: origin, LazyStart: true, Continuous: true})
		}) {
			return
		}
		if err != nil {
			i.failFor(w, v, err, r)
			return
		}
		for _, previous := range paths {
			if previous["id"] != b.PathID && previous["original"] != true {
				delete(previous, "url")
				previous["qualities"] = []object{}
				delete(previous, "initialRenditionId")
				previous["readiness"] = "on-demand"
			}
		}
		if rung := s.qualityRungs[b.PathID]; rung != "" {
			url = strings.TrimSuffix(url, "master.m3u8") + rung + "/index.m3u8"
		}
		path["url"], path["readiness"] = url, "ready"
		path["timelineOriginSeconds"] = float64(0)
		if format == frontend.StreamHLS {
			path["timelineOriginSeconds"] = origin.Seconds()
		}
		s.plan["selectedPathId"] = b.PathID
		if format == frontend.StreamHLS {
			if !s.portSnapshot(w, func(copy *playSession) { i.refreshTrackDelivery(r.Context(), i.access(r, v), id, copy) }) {
				return
			}
		}
		s.position = pos
		s.retainDiagnostics()
		s.source++
		s.seek++
		s.resetTrickplayForSourceLocked()
		s.plan["fallback"] = object{"state": "idle"}
		if !s.portSnapshot(w, func(copy *playSession) { i.refreshOutput(r.Context(), id, copy) }) {
			return
		}
	case "seek":
		var pos time.Duration
		ok := true
		if b.PositionSeconds != nil {
			pos, ok = seconds(*b.PositionSeconds)
		}
		if b.AtSeconds != nil {
			pos, ok = seconds(*b.AtSeconds)
		}
		if !ok {
			problem(w, 400, "request", "invalid_position")
			return
		}
		if _, extra := s.plan["extraId"]; extra {
			var e error
			if !s.port(w, func() {
				e = i.host.Sessions.Report(r.Context(), i.access(r, v), id, frontend.Timeline{Position: pos, State: "playing"})
			}) {
				return
			}
			if e != nil {
				fail(w, e, r)
				return
			}
			s.position = pos
			s.seek++
			break
		}
		paths := s.plan["paths"].([]object)
		path := selectedPlaybackPath(s.plan, paths)
		if path["transport"] == "hls" {
			var url string
			var e error
			if !s.port(w, func() {
				url, e = i.host.Sessions.StartStream(r.Context(), i.access(r, v), id, frontend.StreamRequest{Format: frontend.StreamHLS, Position: pos, LazyStart: true, Continuous: true})
			}) {
				return
			}
			if e != nil {
				i.failFor(w, v, e, r)
				return
			}
			if rung := s.rungFor(path); rung != "" {
				url = strings.TrimSuffix(url, "master.m3u8") + rung + "/index.m3u8"
			}
			path["url"] = url
			path["timelineOriginSeconds"] = pos.Seconds()
		}
		s.invalidatePrepared(path)
		s.retainDiagnostics()
		s.seek++
		s.position = pos
		if !s.portSnapshot(w, func(copy *playSession) { i.refreshOutput(r.Context(), id, copy) }) {
			return
		}

	case "tracks":
		if _, ok := i.host.Sessions.(frontend.StreamDecisionReader); !ok {
			i.unavailable(w, r)
			return
		}
		if _, live := s.plan["live"]; live {
			i.unavailable(w, r)
			return
		}
		var audio, sub *int
		if b.AudioTrackID != "" {
			n, ok := offeredTrack(s.plan, "audioTracks", b.AudioTrackID)
			if !ok {
				problem(w, 400, "request", "invalid_audio_track")
				return
			}
			audio = &n
		}
		var subID any = s.plan["selectedSubtitleTrackId"]
		if len(b.SubtitleTrackID) > 0 {
			n := -1
			if string(b.SubtitleTrackID) != "null" {
				var track string
				if json.Unmarshal(b.SubtitleTrackID, &track) != nil {
					problem(w, 400, "request", "invalid_subtitle_track")
					return
				}
				var ok bool
				n, ok = offeredTrack(s.plan, "subtitleTracks", track)
				if !ok {
					problem(w, 400, "request", "invalid_subtitle_track")
					return
				}
				subID = track
			} else {
				subID = nil
			}
			sub = &n
		}
		if audio == nil && sub == nil {
			problem(w, 400, "request", "invalid_request")
			return
		}
		position := s.position
		var url string
		var err error
		if !s.port(w, func() {
			url, err = i.host.Sessions.StartStream(r.Context(), i.access(r, v), id, frontend.StreamRequest{Format: frontend.StreamHLS, Position: position, LazyStart: true, Continuous: true, AudioStream: audio, SubtitleStream: sub, BurnImageSubtitles: true})
		}) {
			return
		}
		if err != nil {
			i.failFor(w, v, err, r)
			return
		}
		paths := s.plan["paths"].([]object)
		path := selectedPlaybackPath(s.plan, paths)
		if path["original"] == true || path["id"] == "original" {
			// Track changes are HLS: leave the original (a fallback or the
			// client-cap choice) intact and move to its converted path.
			for _, candidate := range paths {
				if candidate["id"] == "fallback-hls" || (candidate["id"] == "primary" && candidate["transport"] == "hls") {
					delete(path, "url")
					path["readiness"] = "on-demand"
					path = candidate
					s.plan["selectedPathId"] = path["id"]
					break
				}
			}
		}
		path["readiness"] = "ready"
		delete(path, "original")
		delete(path, "range")
		delete(path, "originalVideo")
		delete(path, "byteLength")
		if rung := s.rungFor(path); rung != "" {
			url = strings.TrimSuffix(url, "master.m3u8") + rung + "/index.m3u8"
		}
		path["url"], path["transport"], path["mimeType"] = url, "hls", "application/vnd.apple.mpegurl"
		path["qualities"] = []object{}
		path["timelineOriginSeconds"] = s.position.Seconds()
		if reader, ok := i.host.Sessions.(frontend.StreamDecisionReader); ok {
			var d frontend.Decision
			var e error
			if !s.port(w, func() { d, e = reader.StreamDecision(r.Context(), i.access(r, v), id) }) {
				return
			}
			if e == nil {
				kind := "remux"
				// A quality rung is always re-encoded, whatever the
				// session's main decision is.
				if d.Kind == frontend.DecisionTranscode || s.rungFor(path) != "" {
					kind = "transcode"
				}
				path["kind"] = kind
				path["decisionReason"] = d.Reason
				if len(s.media.Streams) > 0 {
					selected := -1
					if subID != nil {
						selected, _ = strconv.Atoi(subID.(string))
					}
					var audioTracks, subtitleTracks []object
					media, formats, burn := s.media, s.subtitleFormats, s.canBurn
					if !s.port(w, func() {
						audioTracks, subtitleTracks, _, _ = i.playbackTracks(r.Context(), i.access(r, v), id, media, d, selected, false, formats, burn)
					}) {
						return
					}
					s.plan["audioTracks"], s.plan["subtitleTracks"] = audioTracks, subtitleTracks
				}
			}
		}
		if audio != nil {
			s.plan["selectedAudioTrackId"] = b.AudioTrackID
		}
		if sub != nil {
			s.plan["selectedSubtitleTrackId"] = subID
		}
		s.invalidatePrepared(path)
		s.retainDiagnostics()
		s.source++
		s.seek++
		s.resetTrickplayForSourceLocked()
		s.preparation = []frontend.HLSPreparationEvent{{Stage: "decision", At: time.Now().UTC()}}
		if !s.portSnapshot(w, func(copy *playSession) { i.refreshOutput(r.Context(), id, copy) }) {
			return
		}
	case "version":
		switcher, canSwitch := i.host.Sessions.(frontend.VersionSwitcher)
		if !canSwitch || !i.versionsAvailable() {
			i.unavailable(w, r)
			return
		}
		if _, live := s.plan["live"]; live {
			i.unavailable(w, r)
			return
		}
		if b.VersionID == "" {
			problem(w, 400, "request", "invalid_version")
			return
		}
		pos := s.position
		ok := true
		if b.PositionSeconds != nil {
			pos, ok = seconds(*b.PositionSeconds)
		}
		if b.AtSeconds != nil {
			pos, ok = seconds(*b.AtSeconds)
		}
		if !ok {
			problem(w, 400, "request", "invalid_position")
			return
		}
		var plan frontend.PlayPlan
		var err error
		if !s.port(w, func() {
			plan, err = switcher.SwitchVersion(r.Context(), i.access(r, v), id, b.VersionID, pos)
		}) {
			return
		}
		if err != nil {
			switch {
			case errors.Is(err, frontend.ErrPreparing):
				versionPreparing(w, err)
			case errors.Is(err, frontend.ErrVersionUnavailable):
				problem(w, 409, "source", "version_unavailable")
			case errors.Is(err, frontend.ErrNotFound):
				problem(w, 400, "request", "invalid_version")
			default:
				i.failFor(w, v, err, r)
			}
			return
		}
		format := frontend.StreamDirect
		transport := "file"
		mime := sourceMIME(plan.Media.Container)
		if plan.Decision.Kind != frontend.DecisionDirectPlay {
			if (!s.client.HLS && !s.client.MSE) || !plan.HLS {
				problem(w, 422, "decoder", "unsupported_transport")
				return
			}
			format = frontend.StreamHLS
			transport = "hls"
			mime = "application/vnd.apple.mpegurl"
		}
		var url string
		if !s.port(w, func() {
			url, err = i.host.Sessions.StartStream(r.Context(), i.access(r, v), id, frontend.StreamRequest{Format: format, Position: pos, LazyStart: true, Continuous: true})
		}) {
			return
		}
		if err != nil {
			i.failFor(w, v, err, r)
			return
		}
		kind := "direct"
		if plan.Decision.Kind == frontend.DecisionRemux {
			kind = "remux"
		}
		if plan.Decision.Kind == frontend.DecisionTranscode {
			kind = "transcode"
		}
		original := plan.Decision.Reason == "original_runtime"
		path := object{"id": "primary", "kind": kind, "transport": transport, "url": url, "mimeType": mime, "readiness": "ready", "timelineOriginSeconds": float64(0), "qualities": []object{}, "boundaries": []object{}}
		if transport == "hls" {
			path["timelineOriginSeconds"] = pos.Seconds()
		}
		if plan.ETag != "" {
			path["timelineId"] = plan.ETag
			path["sourceId"] = plan.ETag
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
		path["decisionReason"] = plan.Decision.Reason
		paths := []object{path}
		if original && (s.client.HLS || s.client.MSE) && plan.HLS {
			fallbackKind := "transcode"
			if plan.DirectStream {
				fallbackKind = "remux"
			}
			paths = append(paths, object{"id": "fallback-hls", "kind": fallbackKind, "transport": "hls", "mimeType": "application/vnd.apple.mpegurl", "readiness": "on-demand", "sourceId": plan.ETag, "timelineId": plan.ETag, "qualities": []object{}, "boundaries": []object{}})
		}
		if !original {
			audioOnly := plan.Media.Video() == nil && plan.Media.HasPlayableMedia()
			if choice := originalChoicePath(plan, audioOnly, s.client); choice != nil {
				paths = append(paths, choice)
			}
		}
		_, trackChanges := i.host.Sessions.(frontend.StreamDecisionReader)
		canBurn := trackChanges && plan.Transcoding && !plan.VideoTranscodingOff && plan.HLS && (s.client.HLS || s.client.MSE)
		formats := append([]string(nil), s.subtitleFormats...)
		var audioTracks, subtitleTracks []object
		var selectedAudio string
		var selectedSub any
		var markers []object
		markerItem := s.item
		if !s.port(w, func() {
			audioTracks, subtitleTracks, selectedAudio, selectedSub = i.playbackTracks(r.Context(), i.access(r, v), id, plan.Media, plan.Decision, plan.Decision.SubtitleStream, original, formats, canBurn)
			if plan.Media.Video() != nil || !plan.Media.HasPlayableMedia() {
				markers = i.skipMarkers(r.Context(), markerItem, plan.Media)
			}
		}) {
			return
		}
		s.media = plan.Media
		s.canBurn = canBurn
		s.qualityRungs = nil
		s.active = planActiveVersion(plan)
		s.plan["paths"] = paths
		s.plan["selectedPathId"] = "primary"
		s.plan["audioTracks"] = audioTracks
		s.plan["subtitleTracks"] = subtitleTracks
		if selectedAudio != "" {
			s.plan["selectedAudioTrackId"] = selectedAudio
		} else {
			delete(s.plan, "selectedAudioTrackId")
		}
		s.plan["selectedSubtitleTrackId"] = selectedSub
		s.plan["chapters"] = chapters(plan.Media)
		setSkipMarkers(s.plan, markers)
		s.plan["fallback"] = object{"state": "unsupported"}
		delete(s.plan, "qualityLimitations")
		if plan.Media.Duration > 0 {
			s.plan["durationSeconds"] = plan.Media.Duration.Seconds()
		} else {
			delete(s.plan, "durationSeconds")
		}
		s.position = pos
		s.retainDiagnostics()
		s.source++
		s.seek++
		s.resetTrickplayForSourceLocked()
		s.preparation = nil
		if !plan.SourceChosenAt.IsZero() {
			s.preparation = append(s.preparation, frontend.HLSPreparationEvent{Stage: "source-chosen", At: plan.SourceChosenAt})
		}
		if !plan.DecisionAt.IsZero() {
			s.preparation = append(s.preparation, frontend.HLSPreparationEvent{Stage: "decision", At: plan.DecisionAt})
		}
		if transport == "hls" {
			if !s.portSnapshot(w, func(copy *playSession) { i.refreshTrackDelivery(r.Context(), i.access(r, v), id, copy) }) {
				return
			}
		}
		qSnap := s.outputSnapshot()
		if !s.port(w, func() { i.addQualityPaths(r.Context(), i.access(r, v), id, qSnap) }) {
			return
		}
		s.qualityRungs = qSnap.qualityRungs
		s.plan["paths"] = qSnap.plan["paths"]
		if fb, ok := qSnap.plan["fallback"]; ok {
			s.plan["fallback"] = fb
		}
		if limits, ok := qSnap.plan["qualityLimitations"]; ok {
			s.plan["qualityLimitations"] = limits
		}
		if !s.portSnapshot(w, func(copy *playSession) { i.refreshOutput(r.Context(), id, copy) }) {
			return
		}
		if s.item != 0 {
			item, caps, dev := s.item, s.declaredCaps, requestDevice(r)
			active := planActiveVersion(plan)
			versionPlan := object{}
			cold := false
			if !s.port(w, func() {
				cold = i.attachPlaybackVersions(r.Context(), v, dev, item, caps, active, versionPlan)
			}) {
				return
			}
			applyVersionPlan(s.plan, versionPlan)
			s.versionsCold = cold
		}
	case "replan":
		ok, changed := i.replanLiveSession(w, r, v, s, id, b.MaxHeight)
		if !ok {
			return
		}
		if !changed {
			send(w, 200, s.plan)
			return
		}
	case "pause", "resume":
		state := "paused"
		if b.Kind == "resume" {
			state = "playing"
		}
		position := s.position
		var e error
		if !s.port(w, func() {
			e = i.host.Sessions.Report(r.Context(), i.access(r, v), id, frontend.Timeline{Position: position, State: state})
		}) {
			return
		}
		if e != nil {
			fail(w, e, r)
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
	if kind == "plan" && s.isTrickplayEligibleLocked() {
		var key trickplayOfferKey
		var generating, ok bool
		if !s.port(w, func() {
			key, generating, ok = i.queryTrickplay(r.Context(), i.access(r, v), id)
		}) {
			return
		}
		if ok && s.isTrickplayEligibleLocked() {
			s.trickplayGenerating = generating
			s.applyTrickplayLocked(key)
		}
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
		Error                                      *struct{ Class, Message string }
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
	if !s.ended.IsZero() || s.stopping {
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
		if b.AudioTrackID != nil && *b.AudioTrackID != s.plan["selectedAudioTrackId"] {
			problem(w, 409, "conflict", "track_command_required")
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
	s.mu.Unlock()
	err := func() error {
		defer s.mu.Lock()
		return i.host.Sessions.Report(r.Context(), i.access(r, v), r.PathValue("id"), frontend.Timeline{Sequence: b.Sequence, Position: pos, State: b.State, AudioStreamIndex: audioIndex, SubtitleStreamIndex: subtitleIndex})
	}()
	if stale(w, s, b.SourceGeneration, b.SeekGeneration) {
		return
	}
	if b.Sequence <= s.sequence || !s.ended.IsZero() || s.stopping {
		send(w, 200, object{"accepted": true})
		return
	}
	if e := err; e != nil {
		fail(w, e, r)
		return
	}

	if b.Error != nil {
		switch b.Error.Class {
		case "source", "decoder", "transient":
			// Keep correlation without retaining arbitrary client text containing URLs.
			digest := sha256.Sum256([]byte(b.Error.Message))
			s.clientErrors = append(s.clientErrors, object{"at": time.Now().UTC(), "sourceGeneration": s.source, "seekGeneration": s.seek, "class": b.Error.Class, "messageHash": fmt.Sprintf("%x", digest), "positionSeconds": pos.Seconds()})
			if len(s.clientErrors) > 8 {
				s.clientErrors = s.clientErrors[len(s.clientErrors)-8:]
			}
		}
	}
	s.sequence = b.Sequence
	s.observedSource = s.source
	s.position = pos
	if b.State != s.observedState {
		attrs := []any{"session", s.id, "state", b.State, "previous", s.observedState, "position", pos.Round(time.Millisecond)}
		if !s.observedAt.IsZero() {
			attrs = append(attrs, "after", time.Since(s.observedAt).Round(time.Millisecond))
		}
		if b.Error != nil {
			attrs = append(attrs, "errorClass", b.Error.Class)
		}
		slog.Info("vela player state", attrs...)
		s.observedState, s.observedAt = b.State, time.Now()
	}
	if b.AudioTrackID != nil {
		s.plan["selectedAudioTrackId"] = *b.AudioTrackID
	}
	if len(b.SubtitleTrackID) > 0 {
		s.plan["selectedSubtitleTrackId"] = subtitleID
	}
	if b.State == "ended" {
		s.mu.Unlock()
		s.commandMu.Lock()
		s.mu.Lock()
		defer s.commandMu.Unlock()
		if stale(w, s, b.SourceGeneration, b.SeekGeneration) {
			return
		}
		if b.Sequence != s.sequence || !s.ended.IsZero() {
			send(w, 200, object{"accepted": true})
			return
		}
		s.stopping = true
		defer func() { s.stopping = false }()
		var stopErr error
		if !s.stopPort(w, func() { stopErr = i.host.Sessions.Stop(r.Context(), i.access(r, v), r.PathValue("id")) }) {
			return
		}
		if e := stopErr; e != nil {
			fail(w, e, r)
			return
		}
		s.ended = time.Now()
		s.clearDisconnectTimerLocked()
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
	frontend.EnableFastDeadPeer(r.Context(), i.deadPeerTimeout())
	writeTimeout := i.streamWriteTimeout()
	rc := http.NewResponseController(unwrapResponseWriter(w))
	defer func() { _ = rc.SetWriteDeadline(time.Time{}) }()

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	id := r.PathValue("id")
	s.mu.Lock()
	if id == "" {
		id = s.id
	}
	if id == "" {
		id, _ = s.plan["sessionId"].(string)
	}
	if s.stream != nil {
		s.stream()
	}
	s.clearDisconnectTimerLocked()
	s.streamSeq++
	seq := s.streamSeq
	s.stream = cancel
	s.streamOpen = true
	s.mu.Unlock()
	s.noteActivity()

	defer func() {
		s.mu.Lock()
		if s.streamSeq == seq {
			s.stream = nil
			s.streamOpen = false
			if s.ended.IsZero() && !s.stopping && id != "" && (i.ctx == nil || i.ctx.Err() == nil) {
				grace := i.streamDisconnectGrace()
				s.armDisconnectTimerLocked(grace, func() { i.checkDisconnectedSession(id, s) })
			}
		}
		s.mu.Unlock()
	}()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	cursor := r.Header.Get("Last-Event-ID")
	if cursor == "" {
		cursor = r.URL.Query().Get("cursor")
	}
	last, _ := strconv.ParseUint(cursor, 10, 64)
	ticker := time.NewTicker(i.heartbeatInterval())
	defer ticker.Stop()
	keepAlive := true // connect, then on every heartbeat tick
	for {
		identity, ok := i.host.Identity.(frontend.GrantScopes)
		if !ok {
			return
		}
		viewer, err := identity.GrantScopeCurrent(ctx, s.scope, requestDevice(r))
		if err != nil {
			return
		}
		s.mu.Lock()
		ended := !s.ended.IsZero()
		s.mu.Unlock()
		if keepAlive && !ended {
			// Backends without activity-only renewal retain their existing SSE behavior.
			if err := i.host.Sessions.KeepAlive(ctx, i.access(r, viewer), id); err != nil && !errors.Is(err, frontend.ErrNotAvailable) {
				if errors.Is(err, frontend.ErrNotFound) {
					s.mu.Lock()
					if s.ended.IsZero() {
						s.ended = time.Now()
						s.clearDisconnectTimerLocked()
						if s.plan != nil {
							s.plan["state"] = "ended"
						}
						if s.signal != nil {
							s.publish("ended")
						}
					}
					s.mu.Unlock()
				} else {
					return
				}
			}
		}
		keepAlive = false
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
			var payload any = e
			if raw, ok := e["data"]; ok {
				payload = raw
			}
			b, _ := json.Marshal(payload)
			chunks = append(chunks, fmt.Sprintf("id: %s\nevent: %s\ndata: %s\n\n", e["cursor"], e["type"], b))
		}
		signal := s.signal
		ended = !s.ended.IsZero()
		s.mu.Unlock()
		if len(chunks) > 0 {
			_ = rc.SetWriteDeadline(time.Now().Add(writeTimeout))
			for _, chunk := range chunks {
				if _, e := fmt.Fprint(w, chunk); e != nil {
					return
				}
			}
			if err := flushSSE(rc, flush, writeTimeout); err != nil {
				return
			}
			s.noteActivity()
		}
		if ended {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-i.done():
			return
		case <-signal:
		case <-ticker.C:
			keepAlive = true
			_ = rc.SetWriteDeadline(time.Now().Add(writeTimeout))
			if _, e := fmt.Fprint(w, ": heartbeat\n\n"); e != nil {
				return
			}
			if err := flushSSE(rc, flush, writeTimeout); err != nil {
				return
			}
			s.noteActivity()
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

// originalChoicePath is the original file the client's own streaming cap
// (client.maxBitrate, together with maxWidth/maxHeight) kept from being
// selected. It is offered on demand, as an explicit choice; nothing starts
// until the client switches to it.
func originalChoicePath(plan frontend.PlayPlan, audioOnly bool, c client) object {
	d := plan.OriginalOverClientCap
	if d == nil || d.Kind != frontend.DecisionDirectPlay {
		return nil
	}
	mime := sourceMIME(plan.Media.Container)
	if audioOnly {
		mime = audioSourceMIME(plan.Media.Container, d.AudioCodec)
	}
	path := object{"id": "original", "kind": "direct", "transport": "file", "mimeType": mime, "readiness": "on-demand", "timelineOriginSeconds": float64(0), "qualities": []object{}, "boundaries": []object{}, "range": true, "exceedsClientCap": true, "decisionReason": d.Reason}
	if d.Reason == "original_runtime" {
		path["original"] = true
		if !audioOnly {
			if facts := originalVideo(plan.Media); facts != nil {
				path["originalVideo"] = facts
			}
		}
	}
	if plan.Media.Size > 0 {
		path["byteLength"] = plan.Media.Size
	}
	sourceKbps := plan.Media.BitrateKbps
	if sourceKbps <= 0 && plan.Media.Size > 0 && plan.Media.Duration > 0 {
		sourceKbps = int(float64(plan.Media.Size*8) / plan.Media.Duration.Seconds() / 1000)
	}
	if sourceKbps > 0 {
		path["bitrate"] = sourceKbps * 1000
		if capKbps := clientBitrateKbps(c.MaxBitrate); capKbps > 0 && sourceKbps > capKbps {
			path["exceedsMaxBitrate"] = true
		}
	}
	if plan.ETag != "" {
		path["sourceId"], path["timelineId"] = plan.ETag, plan.ETag
	}
	return path
}

func selectedPlaybackPath(plan object, paths []object) object {
	for _, p := range paths {
		if p["id"] == plan["selectedPathId"] {
			return p
		}
	}
	return object{}
}

func (i *instance) isBookTitle(r *http.Request, v *frontend.Viewer, key frontend.RatingKey) bool {
	if i == nil || i.host.Experience == nil || v == nil || key == 0 {
		return false
	}
	d, err := i.host.Experience.Item(frontend.WithColdDetail(r.Context(), key), frontend.ItemRequest{Viewer: v, Device: requestDevice(r), Key: key})
	if err != nil {
		return false
	}
	return reading.IsBookTitle(d.Title)
}

func audioSourceMIME(container, codec string) string {
	switch strings.ToLower(strings.TrimSpace(container)) {
	case "mp4", "m4a", "m4b", "fmp4", "mov":
		return "audio/mp4"
	case "webm", "weba":
		return "audio/webm"
	case "mkv", "matroska", "mka":
		return "audio/x-matroska"
	}
	if ct := sourceMIME(container); ct != "application/octet-stream" {
		return ct
	}
	switch strings.ToLower(strings.TrimSpace(codec)) {
	case "aac", "alac":
		return "audio/mp4"
	case "mp3":
		return "audio/mpeg"
	case "flac":
		return "audio/flac"
	case "opus", "vorbis":
		return "audio/ogg"
	}
	return "application/octet-stream"
}

func sourceMIME(container string) string {
	switch strings.ToLower(container) {
	case "mkv", "matroska":
		return "video/x-matroska"
	case "mka":
		return "audio/x-matroska"
	case "mp4", "mov", "m4v":
		return "video/mp4"
	case "webm":
		return "video/webm"
	case "weba":
		return "audio/webm"
	case "mpegts":
		return "video/mp2t"
	case "avi":
		return "video/x-msvideo"
	case "flac":
		return "audio/flac"
	case "mp3":
		return "audio/mpeg"
	case "aac", "adts":
		return "audio/aac"
	case "m4a", "m4b":
		return "audio/mp4"
	case "ogg", "oga", "opus":
		return "audio/ogg"
	case "wav":
		return "audio/wav"
	case "aif", "aiff":
		return "audio/aiff"
	case "wma":
		return "audio/x-ms-wma"
	case "epub":
		return "application/epub+zip"
	case "pdf":
		return "application/pdf"
	case "cbz":
		return "application/vnd.comicbook+zip"
	}
	return "application/octet-stream"
}
