package mediabrowser

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/kit/caps"
	"github.com/braedonsaunders/ether/internal/frontend/kit/ids"
	"github.com/braedonsaunders/ether/internal/frontend/kit/route"
)

type playbackRequest struct {
	MediaSourceId       string             `json:"MediaSourceId"`
	DeviceProfile       caps.DeviceProfile `json:"DeviceProfile"`
	IsPlayback          *bool              `json:"IsPlayback"`
	EnableDirectPlay    *bool              `json:"EnableDirectPlay"`
	EnableDirectStream  *bool              `json:"EnableDirectStream"`
	EnableTranscoding   *bool              `json:"EnableTranscoding"`
	StartTimeTicks      int64              `json:"StartTimeTicks"`
	AudioStreamIndex    *int               `json:"AudioStreamIndex"`
	SubtitleStreamIndex *int               `json:"SubtitleStreamIndex"`
}

func boolValue(p *bool, def bool) bool {
	if p == nil {
		return def
	}
	return *p
}

func streamError(w http.ResponseWriter, err error) {
	var capacity *frontend.TranscodeCapacityError
	var busy *frontend.SessionBusyError
	var retryAfter time.Duration
	if errors.Is(err, frontend.ErrTranscodeCapacity) && errors.As(err, &capacity) && capacity != nil {
		retryAfter = capacity.RetryAfter
	}
	if errors.Is(err, frontend.ErrSessionBusy) && errors.As(err, &busy) && busy != nil {
		retryAfter = busy.RetryAfter
	}
	if retryAfter > 0 {
		seconds := retryAfter / time.Second
		if retryAfter%time.Second != 0 {
			seconds++
		}
		w.Header().Set("Retry-After", strconv.FormatInt(int64(seconds), 10))
	}
	http.Error(w, "Playback unavailable", http.StatusServiceUnavailable)
}
func queryBool(r *http.Request, key string, def bool) bool {
	values := route.Query(r.URL.Query(), key)
	if len(values) != 1 {
		return def
	}
	v, err := strconv.ParseBool(values[0])
	if err != nil {
		return def
	}
	return v
}
func (i *instance) playbackInfo(w http.ResponseWriter, r *http.Request) {
	v, credential, err := i.viewer(r)
	if err != nil || v == nil {
		http.Error(w, "Unauthorized", 401)
		return
	}
	if i.host.Sessions == nil || i.host.Capabilities == nil {
		http.Error(w, "Unavailable", 503)
		return
	}
	wireID := r.PathValue("id")
	key, err := ids.ParseRatingKeyGUID(wireID)
	channel := ""
	itemIDError := err
	var body playbackRequest
	if r.Body != nil && r.ContentLength != 0 {
		if decode(r, &body) != nil {
			http.Error(w, "Bad Request", 400)
			return
		}
	}
	dev := device(r)
	if values := route.Query(r.URL.Query(), "MaxStreamingBitrate"); len(values) == 1 {
		n, e := strconv.Atoi(values[0])
		if e != nil || n < 0 {
			http.Error(w, "Bad Request", 400)
			return
		}
		if body.DeviceProfile.MaxStreamingBitrate == 0 || n < body.DeviceProfile.MaxStreamingBitrate {
			body.DeviceProfile.MaxStreamingBitrate = n
		}
	}
	capability, err := i.host.Capabilities.Build(i.identityContext(r), frontend.CapabilityRequest{Viewer: v, Device: caps.DeviceInfo{Protocol: i.protocol, ClientID: dev.ID, Product: dev.Client, DeviceName: dev.DeviceName, Version: dev.Version}, Profile: body.DeviceProfile, UserAgent: r.UserAgent()})
	if err != nil {
		http.Error(w, "Unavailable", 503)
		return
	}
	scope := frontend.VersionScope{Viewer: v, Device: dev, Protocol: i.protocol, Caps: capability.Client}
	versionFromItem := ""
	if itemIDError != nil {
		if target, _, _, e := i.resolveSource(r, scope, wireID); e == nil && target != 0 {
			key, versionFromItem = target, wireID
		}
		if versionFromItem == "" {
			if i.host.LiveTV == nil {
				http.NotFound(w, r)
				return
			}
			ch, e := i.host.LiveTV.ResolveChannelGUID(r.Context(), v, wireID)
			if e != nil || ch.ID == "" {
				http.NotFound(w, r)
				return
			}
			channel, key = ch.ID, 0
		}
	}
	// Native clients commonly omit IsPlayback when starting a stream. Only an
	// explicit false is an estimate; opening details does not resolve releases.
	playback := boolValue(body.IsPlayback, true)
	if values := route.Query(r.URL.Query(), "IsPlayback"); len(values) > 1 {
		http.Error(w, "Bad Request", http.StatusBadRequest)
		return
	} else if len(values) == 1 {
		var e error
		playback, e = strconv.ParseBool(values[0])
		if e != nil {
			http.Error(w, "Bad Request", http.StatusBadRequest)
			return
		}
	}
	opts := frontend.DecideOptions{DirectPlay: queryBool(r, "EnableDirectPlay", boolValue(body.EnableDirectPlay, true)), DirectStream: queryBool(r, "EnableDirectStream", boolValue(body.EnableDirectStream, true)), AudioStream: -1, SubtitleStream: -1}
	if body.AudioStreamIndex != nil {
		opts.AudioStream = *body.AudioStreamIndex
	}
	if body.SubtitleStreamIndex != nil {
		opts.SubtitleStream = *body.SubtitleStreamIndex
	}
	audioSet, subtitleSet := body.AudioStreamIndex != nil, body.SubtitleStreamIndex != nil
	for _, field := range []struct {
		name string
		dest *int
		set  *bool
	}{{"AudioStreamIndex", &opts.AudioStream, &audioSet}, {"SubtitleStreamIndex", &opts.SubtitleStream, &subtitleSet}} {
		values := route.Query(r.URL.Query(), field.name)
		if len(values) > 1 {
			http.Error(w, "Bad Request", 400)
			return
		}
		if len(values) == 1 {
			n, e := strconv.Atoi(values[0])
			if e != nil || n < -1 {
				http.Error(w, "Bad Request", 400)
				return
			}
			*field.dest = n
			*field.set = true
		}
	}
	if values := route.Query(r.URL.Query(), "StartTimeTicks"); len(values) > 1 {
		http.Error(w, "Bad Request", 400)
		return
	} else if len(values) == 1 {
		n, e := strconv.ParseInt(values[0], 10, 64)
		if e != nil {
			http.Error(w, "Bad Request", 400)
			return
		}
		body.StartTimeTicks = n
	}
	if body.StartTimeTicks < 0 || body.StartTimeTicks > (1<<63-1)/100 {
		http.Error(w, "Bad Request", 400)
		return
	}
	allowed := queryBool(r, "EnableTranscoding", boolValue(body.EnableTranscoding, true))
	intent := frontend.PlayIntent{Estimate: !playback, Options: opts, Position: time.Duration(body.StartTimeTicks) * 100 * time.Nanosecond, AllowTranscoding: &allowed, AudioSet: audioSet, SubtitleSet: subtitleSet}
	sourceID := body.MediaSourceId
	idsQuery := route.Query(r.URL.Query(), "MediaSourceId")
	if len(idsQuery) > 1 {
		http.Error(w, "Bad Request", 400)
		return
	}
	if len(idsQuery) == 1 {
		sourceID = idsQuery[0]
	}
	version := versionFromItem
	if sourceID != "" && !strings.EqualFold(sourceID, wireID) {
		if parsed, e := ids.ParseRatingKeyGUID(sourceID); e != nil || parsed != key || channel != "" {
			version = sourceID
		}
	}
	detailSource, expectedETag := "", ""
	if version != "" {
		if channel != "" {
			http.NotFound(w, r)
			return
		}
		target, kind, tag, e := i.resolveSource(r, scope, version)
		if e != nil || target != key || (versionFromItem != "" && !sameVersionHandle(version, versionFromItem)) {
			http.NotFound(w, r)
			return
		}
		if kind == "detail" {
			detailSource, expectedETag, version = version, tag, ""
		} else {
			intent.ExactVersion = true
		}
	}
	planRequest := frontend.PlanRequest{Viewer: v, Device: dev, Protocol: i.protocol, Item: key, Channel: channel, Version: version, DetailSource: detailSource, ExpectedSourceETag: expectedETag, Caps: capability.Client, Intent: intent}
	plan, err := i.host.Sessions.Plan(r.Context(), planRequest)
	if err != nil {
		http.Error(w, "Playback unavailable", 503)
		return
	}
	if playback && (plan.Estimated || (channel == "" && plan.Media.Estimated)) {
		http.Error(w, "Playback unavailable", 503)
		return
	}
	if !allowed && plan.Decision.Kind == "transcode" {
		http.Error(w, "Playback unavailable", 422)
		return
	}
	sourceID = ids.RatingKeyGUID(key)
	if channel != "" {
		sourceID = wireID
	} else if version != "" {
		sourceID = version
	} else if detailSource != "" {
		sourceID = detailSource
	}
	media := plan.Media
	media.Estimated = media.Estimated || plan.Estimated
	source := i.detailMediaSourceDTO(sourceID, media)
	if channel != "" || plan.Decision.Kind == "transcode" {
		// These streams are produced by Ether, even when their input is remote.
		source["IsRemote"] = false
	}
	if plan.ETag != "" {
		source["ETag"] = plan.ETag
	}
	source["SupportsDirectPlay"], source["SupportsDirectStream"], source["SupportsTranscoding"] = plan.DirectPlay, plan.DirectStream, plan.Transcoding
	source["IsInfiniteStream"] = channel != ""
	for _, stream := range plan.Media.Streams {
		if media.Estimated {
			break
		}
		if stream.Type == "audio" && stream.Default {
			source["DefaultAudioStreamIndex"] = stream.Index
			break
		}
	}

	playID := ""
	if !playback && channel == "" && opts.DirectPlay {
		if path := i.lazySourceURL(r, credential, planRequest); path != "" {
			source["Path"] = path
		}
	}
	if playback {
		session, err := i.host.Sessions.Open(r.Context(), frontend.OpenRequest{Protocol: i.protocol, PlanID: plan.ID, Viewer: v, Device: dev})
		if err != nil {
			http.Error(w, "Playback unavailable", 503)
			return
		}
		format := frontend.StreamDirect
		if channel != "" || plan.Decision.Kind != "directplay" {
			format = frontend.StreamHLS
		}
		url, err := i.host.Sessions.StartStream(r.Context(), frontend.SessionAccess{Viewer: v, Device: dev, Protocol: i.protocol}, session.ID, frontend.StreamRequest{Format: format, Position: intent.Position})
		if err != nil {
			_ = i.host.Sessions.Stop(r.Context(), frontend.SessionAccess{Viewer: v, Device: dev, Protocol: i.protocol}, session.ID)
			streamError(w, err)
			return
		}
		scopedURL := url
		if prefix, ok := r.Context().Value(prefixKey{}).(string); ok {
			url = prefix + url
		}
		if subtitleService := i.host.Sessions; subtitleService != nil {
			streams := i.mediaStreams(plan.Media.Streams)
			for n, stream := range plan.Media.Streams {
				if stream.Type != "subtitle" {
					continue
				}
				subtitleURL, e := subtitleService.SubtitleURL(r.Context(), frontend.SessionAccess{Viewer: v, Device: dev, Protocol: i.protocol}, session.ID, stream.Index, "vtt")
				if e != nil || !strings.HasPrefix(subtitleURL, "/sessions/media/") || strings.ContainsAny(subtitleURL, "\r\n") {
					continue
				}
				if prefix, ok := r.Context().Value(prefixKey{}).(string); ok {
					subtitleURL = prefix + subtitleURL
				}
				dto := streams[n].(map[string]any)
				dto["DeliveryUrl"] = subtitleURL
				dto["DeliveryMethod"] = "External"
				dto["IsTextSubtitleStream"] = true
				dto["SupportsExternalStream"] = true
			}
			source["MediaStreams"] = streams
		}
		playID = session.ID
		if format == frontend.StreamDirect {
			if path := i.absoluteMediaURL(r, scopedURL); path != "" {
				source["Path"] = path
			} else if i.host.SurfaceURL == nil {
				// Older embedded hosts without a URL facade can still use the
				// broker's same-origin scoped path. Production uses verified URLs.
				source["Path"] = url
			}
		} else {
			source["TranscodingUrl"] = url
			source["TranscodingSubProtocol"] = "hls"
			if container := hlsContainerDTO(plan); container != "" {
				source["TranscodingContainer"] = container
			}
		}
	}
	i.mediaSourceDialect(source)
	if i.protocol == "emby" {
		i.embyMediaSourceDTO(source, wireID, media)
	}
	jsonReply(w, map[string]any{"MediaSources": []any{source}, "PlaySessionId": playID})
}

func (i *instance) mediaSourceDialect(source map[string]any) {
	if i.protocol != "emby" {
		return
	}
	// These Jellyfin properties are absent from Emby's pinned DTO.
	for _, key := range []string{"ETag", "GenPtsInput", "HasSegments", "IgnoreDts", "IgnoreIndex", "MediaAttachments"} {
		delete(source, key)
	}
	if streams, ok := source["MediaStreams"].([]any); ok {
		for _, stream := range streams {
			if dto, ok := stream.(map[string]any); ok {
				delete(dto, "IsOriginal")
			}
		}
	}
}

// mediaSourceDTO describes the operations this frontend offers, not an
// upstream URL. Compatibility is filled in only from the broker's plan.
func mediaSourceDTO(id string, media frontend.MediaInfo) map[string]any {
	source := map[string]any{
		"Id": id, "Container": media.Container, "RunTimeTicks": media.Duration.Nanoseconds() / 100, "Size": media.Size, "Bitrate": media.BitrateKbps * 1000,
		"Protocol": "Http", "Type": "Default", "IsRemote": true, "ReadAtNativeFramerate": false, "IgnoreDts": false, "IgnoreIndex": false, "GenPtsInput": false,
		"SupportsDirectPlay": false, "SupportsDirectStream": false, "SupportsTranscoding": false, "IsInfiniteStream": false,
		// Jellyfin's strict SDK requires a boolean. Until the broker exposes
		// version-scoped marker status, use the conservative false fallback.
		"RequiresOpening": false, "RequiresClosing": false, "RequiresLooping": false, "SupportsProbing": !media.Estimated && len(media.Streams) > 0, "TranscodingSubProtocol": "http", "HasSegments": false,
		"Formats": []string{}, "MediaAttachments": []any{}, "RequiredHttpHeaders": map[string]string{}, "MediaStreams": mediaStreams(media.Streams),
	}
	addChapters(source, media)
	return source
}

func (i *instance) mediaSourceDTO(id string, media frontend.MediaInfo) map[string]any {
	source := mediaSourceDTO(id, media)
	source["MediaStreams"] = i.mediaStreams(media.Streams)
	return source
}

func (i *instance) mediaStreams(streams []frontend.StreamInfo) []any {
	out := mediaStreams(streams)
	for n, stream := range streams {
		if i.protocol == "emby" {
			i.embyMediaStreamDTO(out[n].(map[string]any), stream)
		} else if i.protocol == "jellyfin" {
			i.jellyfinStreamDTO(out[n].(map[string]any), stream)
		}
	}
	return out
}

func mediaStreams(streams []frontend.StreamInfo) []any {
	out := make([]any, 0, len(streams))
	for _, s := range streams {
		kind := map[string]string{"video": "Video", "audio": "Audio", "subtitle": "Subtitle"}[s.Type]
		m := map[string]any{"Index": s.Index, "Type": kind, "Codec": s.Codec, "Language": s.Language, "IsDefault": s.Default, "IsForced": s.Forced, "IsExternal": s.External, "Width": s.Width, "Height": s.Height, "Channels": s.Channels, "IsInterlaced": s.Interlaced, "IsHearingImpaired": false, "IsOriginal": false, "IsTextSubtitleStream": false, "SupportsExternalStream": false}
		if s.DefaultDisposition != nil {
			m["IsDefault"] = *s.DefaultDisposition
		}
		if s.ForcedDisposition != nil {
			m["IsForced"] = *s.ForcedDisposition
		}
		if s.HearingImpaired != nil {
			m["IsHearingImpaired"] = *s.HearingImpaired
		}
		if s.Original != nil {
			m["IsOriginal"] = *s.Original
		}
		if s.Profile != "" {
			m["Profile"] = s.Profile
		}
		if s.Title != "" {
			m["Title"] = s.Title
		}
		if s.Level > 0 {
			m["Level"] = s.Level
		}
		if s.FrameRate > 0 {
			m["RealFrameRate"] = s.FrameRate
		}
		if s.BitDepth > 0 {
			m["BitDepth"] = s.BitDepth
		}
		if s.SampleRate > 0 {
			m["SampleRate"] = s.SampleRate
		}
		if s.BitrateKbps > 0 {
			m["BitRate"] = s.BitrateKbps * 1000
		}
		if s.ChannelLayout != "" {
			m["ChannelLayout"] = s.ChannelLayout
		}
		out = append(out, m)
	}
	return out
}

func addChapters(source map[string]any, media frontend.MediaInfo) {
	if len(media.Chapters) == 0 {
		return
	}
	chapters := make([]any, 0, len(media.Chapters))
	for _, chapter := range media.Chapters {
		chapters = append(chapters, map[string]any{"Name": chapter.Title, "StartPositionTicks": chapter.Start.Nanoseconds() / 100})
	}
	source["Chapters"] = chapters
}
func (i *instance) playbackRoutes() error {
	if err := i.router.Handle("GET", "/Playback/BitrateTest", http.HandlerFunc(i.bitrateTest)); err != nil {
		return err
	}
	for _, method := range []string{"GET", "HEAD"} {
		if err := i.router.Handle(method, "/Videos/{id}/{name}", http.HandlerFunc(i.streamAlias)); err != nil {
			return err
		}
	}
	if err := i.router.Handle("GET", "/MediaSegments/{itemId}", http.HandlerFunc(i.mediaSegments)); err != nil {
		return err
	}
	for _, entry := range []struct {
		path    string
		stopped bool
	}{{"/Sessions/Playing", false}, {"/Sessions/Playing/Progress", false}, {"/Sessions/Playing/Stopped", true}} {
		stop := entry.stopped
		if err := i.router.Handle("POST", entry.path, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { i.timeline(w, r, stop) })); err != nil {
			return err
		}
	}
	if err := i.router.Handle("POST", "/Items/{id}/PlaybackInfo", http.HandlerFunc(i.playbackInfo)); err != nil {
		return err
	}
	for _, method := range []string{"GET", "HEAD"} {
		if err := i.router.Handle(method, "/sessions/lazy/{path...}", http.HandlerFunc(i.lazyMedia)); err != nil {
			return err
		}
		if err := i.router.Handle(method, "/sessions/media/{path...}", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if i.host.Sessions == nil {
				http.Error(w, "Unavailable", 503)
				return
			}
			if i.protocol == "emby" {
				i.embyMediaHeaders(w, r)
			}
			i.host.Sessions.ServeMedia(w, r)
		})); err != nil {
			return err
		}
	}
	return nil
}

type timelineRequest struct {
	PlaySessionId, ItemId string
	PositionTicks         json.Number
	IsPaused              bool
}

func (i *instance) timeline(w http.ResponseWriter, r *http.Request, stopped bool) {
	v, _, err := i.viewer(r)
	if err != nil || v == nil {
		http.Error(w, "Unauthorized", 401)
		return
	}
	if i.host.Sessions == nil {
		http.Error(w, "Unavailable", 503)
		return
	}
	var body timelineRequest
	if decode(r, &body) != nil || body.PlaySessionId == "" {
		http.Error(w, "Bad Request", 400)
		return
	}
	ticks := int64(0)
	if body.PositionTicks != "" {
		ticks, err = body.PositionTicks.Int64()
		if err != nil || ticks < 0 || ticks > (1<<63-1)/100 {
			http.Error(w, "Bad Request", 400)
			return
		}
	}
	access := frontend.SessionAccess{Viewer: v, Device: device(r), Protocol: i.protocol}
	state := "playing"
	if body.IsPaused {
		state = "paused"
	}
	if stopped {
		state = "stopped"
	}
	err = i.host.Sessions.Report(r.Context(), access, body.PlaySessionId, frontend.Timeline{PlaybackID: body.PlaySessionId, Position: time.Duration(ticks) * 100 * time.Nanosecond, State: state})
	if err != nil {
		http.Error(w, "Unavailable", 503)
		return
	}
	if stopped {
		if err = i.host.Sessions.Stop(r.Context(), access, body.PlaySessionId); err != nil {
			http.Error(w, "Unavailable", 503)
			return
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

// streamAlias translates client-generated URLs into broker-owned, scoped URLs.
func (i *instance) streamAlias(w http.ResponseWriter, r *http.Request) {
	viewer, credential, err := i.viewer(r)
	if err != nil || viewer == nil {
		http.Error(w, "Unauthorized", 401)
		return
	}
	if i.host.Sessions == nil {
		http.Error(w, "Unavailable", 503)
		return
	}
	key, err := ids.ParseRatingKeyGUID(r.PathValue("id"))
	channel := ""
	version := ""
	detailSource, expectedETag := "", ""
	var versionScope frontend.VersionScope
	if err != nil {
		versionScope, err = i.versionScope(r, viewer)
		if err == nil {
			if target, kind, tag, e := i.resolveSource(r, versionScope, r.PathValue("id")); e == nil && target != 0 {
				key = target
				if kind == "detail" {
					detailSource, expectedETag = r.PathValue("id"), tag
				} else {
					version = r.PathValue("id")
				}
			}
		}
		if version == "" && detailSource == "" {
			if i.host.LiveTV == nil {
				http.NotFound(w, r)
				return
			}
			ch, e := i.host.LiveTV.ResolveChannelGUID(r.Context(), viewer, r.PathValue("id"))
			if e != nil || ch.ID == "" {
				http.NotFound(w, r)
				return
			}
			channel, key = ch.ID, 0
		}
	}
	name := strings.ToLower(r.PathValue("name"))
	format := frontend.StreamDirect
	switch {
	case name == "stream", strings.HasPrefix(name, "stream."):
	case name == "master.m3u8":
		format = frontend.StreamHLS
	default:
		http.NotFound(w, r)
		return
	}
	vals := route.Query(r.URL.Query(), "PlaySessionId")
	if len(vals) > 1 {
		http.Error(w, "Bad Request", 400)
		return
	}
	sourceIDs := route.Query(r.URL.Query(), "MediaSourceId")
	if len(sourceIDs) > 1 {
		http.Error(w, "Bad Request", 400)
		return
	}
	if len(sourceIDs) == 1 && sourceIDs[0] != "" && !strings.EqualFold(sourceIDs[0], r.PathValue("id")) {
		if sourceKey, e := ids.ParseRatingKeyGUID(sourceIDs[0]); e != nil || sourceKey != key || channel != "" {
			if channel != "" || (version != "" && !sameVersionHandle(version, sourceIDs[0])) || (detailSource != "" && !sameVersionHandle(detailSource, sourceIDs[0])) {
				http.NotFound(w, r)
				return
			}
			if version == "" && detailSource == "" {
				versionScope, err = i.versionScope(r, viewer)
				if err != nil {
					http.NotFound(w, r)
					return
				}
				target, kind, tag, e := i.resolveSource(r, versionScope, sourceIDs[0])
				if e != nil || target != key {
					http.NotFound(w, r)
					return
				}
				if kind == "detail" {
					detailSource, expectedETag = sourceIDs[0], tag
				} else {
					version = sourceIDs[0]
				}
			}
		}
	}
	if (len(vals) == 0 || vals[0] == "") && channel == "" && format == frontend.StreamDirect && queryBool(r, "Static", false) {
		lazy, ok := i.host.LazyStreams.(frontend.AuthenticatedLazyStreams)
		if !ok || i.host.Capabilities == nil {
			http.Error(w, "Unavailable", http.StatusServiceUnavailable)
			return
		}
		dev := device(r)
		capability, e := i.host.Capabilities.Build(i.identityContext(r), frontend.CapabilityRequest{Viewer: viewer, Device: caps.DeviceInfo{Protocol: i.protocol, ClientID: dev.ID, Product: dev.Client, DeviceName: dev.DeviceName, Version: dev.Version}, UserAgent: r.UserAgent()})
		if e != nil {
			http.Error(w, "Unavailable", http.StatusServiceUnavailable)
			return
		}
		request := frontend.PlanRequest{Viewer: viewer, Device: dev, Protocol: i.protocol, Item: key, Version: version, DetailSource: detailSource, ExpectedSourceETag: expectedETag, Caps: capability.Client, Intent: frontend.PlayIntent{ExactVersion: version != "", Options: frontend.DecideOptions{DirectPlay: true}}}
		lazy.ServeAuthenticated(w, r, credential, request)
		return
	}
	access := frontend.SessionAccess{Viewer: viewer, Device: device(r), Protocol: i.protocol}
	if access.Device.ID == "" {
		http.Error(w, "Unauthorized", 401)
		return
	}
	sessions, err := i.host.Sessions.Sessions(r.Context(), access)
	if err != nil {
		http.Error(w, "Unavailable", 503)
		return
	}
	sessionID := ""
	for _, session := range sessions {
		if session.Item != key || session.Channel != channel || session.Protocol != i.protocol || session.Device.ID != access.Device.ID {
			continue
		}
		if len(vals) == 1 && vals[0] != "" && session.ID != vals[0] {
			continue
		}
		if sessionID != "" {
			http.Error(w, "Ambiguous session", 409)
			return
		}
		sessionID = session.ID
	}
	if sessionID == "" {
		http.NotFound(w, r)
		return
	}
	if version != "" && i.host.VersionHandles.MatchSession(i.identityContext(r), versionScope, version, sessionID) != nil {
		http.NotFound(w, r)
		return
	}
	if detailSource != "" {
		planner, ok := i.host.Sessions.(frontend.DetailPlanner)
		if !ok || planner.MatchDetailSession(i.identityContext(r), versionScope, detailSource, sessionID) != nil {
			http.NotFound(w, r)
			return
		}
	}
	url, err := i.host.Sessions.StartStream(r.Context(), access, sessionID, frontend.StreamRequest{Format: format})
	if err != nil {
		streamError(w, err)
		return
	}
	if !strings.HasPrefix(url, "/sessions/media/") || strings.ContainsAny(url, "\r\n") {
		http.Error(w, "Unavailable", 503)
		return
	}
	if prefix, ok := r.Context().Value(prefixKey{}).(string); ok {
		url = prefix + url
	}
	http.Redirect(w, r, url, http.StatusTemporaryRedirect)
}
