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
	IsPlayback          bool               `json:"IsPlayback"`
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
	if errors.Is(err, frontend.ErrTranscodeCapacity) && errors.As(err, &capacity) && capacity.RetryAfter > 0 {
		seconds := capacity.RetryAfter / time.Second
		if capacity.RetryAfter%time.Second != 0 {
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
	v, _, err := i.viewer(r)
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
	if err != nil {
		if i.host.LiveTV == nil {
			http.NotFound(w, r)
			return
		}
		ch, e := i.host.LiveTV.ResolveChannelGUID(r.Context(), v, wireID)
		if e != nil || ch.ID == "" {
			http.NotFound(w, r)
			return
		}
		channel = ch.ID
		key = 0
	}
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
	playback := queryBool(r, "IsPlayback", body.IsPlayback)
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
	version := ""
	if sourceID != "" && !strings.EqualFold(sourceID, wireID) {
		if parsed, e := ids.ParseRatingKeyGUID(sourceID); e != nil || parsed != key || channel != "" {
			version = sourceID
		}
	}
	plan, err := i.host.Sessions.Plan(r.Context(), frontend.PlanRequest{Viewer: v, Device: dev, Protocol: i.protocol, Item: key, Channel: channel, Version: version, Caps: capability.Client, Intent: intent})
	if err != nil {
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
	}
	source := map[string]any{"Id": sourceID, "Container": plan.Media.Container, "RunTimeTicks": plan.Media.Duration.Nanoseconds() / 100, "Size": plan.Media.Size, "Bitrate": plan.Media.BitrateKbps * 1000, "SupportsDirectPlay": plan.DirectPlay, "SupportsDirectStream": plan.DirectStream, "SupportsTranscoding": plan.Transcoding, "MediaStreams": mediaStreams(plan.Media.Streams)}
	addChapters(source, plan.Media)
	source["Protocol"] = "Http"
	source["Type"] = "Default"
	source["IsRemote"] = true
	source["RequiresOpening"] = false
	source["RequiresClosing"] = false
	for _, stream := range plan.Media.Streams {
		if stream.Type == "audio" && stream.Default {
			source["DefaultAudioStreamIndex"] = stream.Index
			break
		}
	}

	playID := ""
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
		if prefix, ok := r.Context().Value(prefixKey{}).(string); ok {
			url = prefix + url
		}
		if subtitleService := i.host.Sessions; subtitleService != nil {
			streams := mediaStreams(plan.Media.Streams)
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
				dto["SupportsExternalStream"] = true
			}
			source["MediaStreams"] = streams
		}
		playID = session.ID
		if format == frontend.StreamDirect {
			source["Path"] = url
		} else {
			source["TranscodingUrl"] = url
			source["TranscodingSubProtocol"] = "hls"
			source["TranscodingContainer"] = "ts"
		}
	}
	jsonReply(w, map[string]any{"MediaSources": []any{source}, "PlaySessionId": playID})
}
func mediaStreams(streams []frontend.StreamInfo) []any {
	out := make([]any, 0, len(streams))
	for _, s := range streams {
		kind := map[string]string{"video": "Video", "audio": "Audio", "subtitle": "Subtitle"}[s.Type]
		m := map[string]any{"Index": s.Index, "Type": kind, "Codec": s.Codec, "Language": s.Language, "IsDefault": s.Default, "IsForced": s.Forced, "IsExternal": s.External, "Width": s.Width, "Height": s.Height, "Channels": s.Channels}
		if s.Type == "video" {
			m["IsInterlaced"] = s.Interlaced
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
		if err := i.router.Handle(method, "/sessions/media/{path...}", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if i.host.Sessions == nil {
				http.Error(w, "Unavailable", 503)
				return
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
	viewer, _, err := i.viewer(r)
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
	if err != nil {
		if i.host.LiveTV == nil {
			http.NotFound(w, r)
			return
		}
		ch, e := i.host.LiveTV.ResolveChannelGUID(r.Context(), viewer, r.PathValue("id"))
		if e != nil || ch.ID == "" {
			http.NotFound(w, r)
			return
		}
		channel = ch.ID
		key = 0
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
