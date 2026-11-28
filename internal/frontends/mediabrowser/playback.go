package mediabrowser

import (
	"encoding/json"
	"errors"
	"github.com/braedonsaunders/ether/internal/frontend/kit/mediasafety"
	"io"
	"net/http"
	"slices"
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
	LiveStreamId        string             `json:"LiveStreamId"`
	OpenToken           string             `json:"OpenToken"`
	ItemId              string             `json:"ItemId"`
	PlaySessionId       string             `json:"PlaySessionId"`
	DeviceProfile       caps.DeviceProfile `json:"DeviceProfile"`
	IsPlayback          *bool              `json:"IsPlayback"`
	AutoOpenLiveStream  *bool              `json:"AutoOpenLiveStream"`
	EnableDirectPlay    *bool              `json:"EnableDirectPlay"`
	EnableDirectStream  *bool              `json:"EnableDirectStream"`
	EnableTranscoding   *bool              `json:"EnableTranscoding"`
	StartTimeTicks      int64              `json:"StartTimeTicks"`
	AudioStreamIndex    *int               `json:"AudioStreamIndex"`
	SubtitleStreamIndex *int               `json:"SubtitleStreamIndex"`
	// SubtitleIndexQuoted marks a SubtitleStreamIndex posted as a string,
	// the way Jellyfin Web copies its subtitle menu's value.
	SubtitleIndexQuoted bool `json:"-"`
}

// UnmarshalJSON decodes a PlaybackInfo body through the caps Flex types
// so a client that quotes a number/boolean (or sends MaxAudioChannels as
// a number inside DeviceProfile) still plays instead of 400ing (#404).
func (p *playbackRequest) UnmarshalJSON(data []byte) error {
	var raw struct {
		MediaSourceId       caps.FlexString    `json:"MediaSourceId"`
		LiveStreamId        caps.FlexString    `json:"LiveStreamId"`
		OpenToken           caps.FlexString    `json:"OpenToken"`
		ItemId              caps.FlexString    `json:"ItemId"`
		PlaySessionId       caps.FlexString    `json:"PlaySessionId"`
		DeviceProfile       caps.DeviceProfile `json:"DeviceProfile"`
		IsPlayback          *caps.FlexBool     `json:"IsPlayback"`
		AutoOpenLiveStream  *caps.FlexBool     `json:"AutoOpenLiveStream"`
		EnableDirectPlay    *caps.FlexBool     `json:"EnableDirectPlay"`
		EnableDirectStream  *caps.FlexBool     `json:"EnableDirectStream"`
		EnableTranscoding   *caps.FlexBool     `json:"EnableTranscoding"`
		StartTimeTicks      caps.FlexInt64     `json:"StartTimeTicks"`
		MaxStreamingBitrate caps.FlexInt64     `json:"MaxStreamingBitrate"`
		AudioStreamIndex    json.RawMessage    `json:"AudioStreamIndex"`
		SubtitleStreamIndex json.RawMessage    `json:"SubtitleStreamIndex"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	flexBool := func(f *caps.FlexBool) *bool {
		if f == nil {
			return nil
		}
		b := bool(*f)
		return &b
	}
	// Jellyfin Web posts "SubtitleStreamIndex":"" when no subtitle is
	// chosen; an empty string (or null) is no choice, not stream 0, which
	// is the video stream and made every plan NoCompatibleStream.
	flexInt := func(raw json.RawMessage) *int {
		t := strings.TrimSpace(string(raw))
		if t == "" || t == "null" || t == `""` {
			return nil
		}
		var f caps.FlexInt
		if json.Unmarshal(raw, &f) != nil {
			return nil
		}
		n := int(f)
		return &n
	}
	*p = playbackRequest{
		MediaSourceId:       string(raw.MediaSourceId),
		LiveStreamId:        string(raw.LiveStreamId),
		OpenToken:           string(raw.OpenToken),
		ItemId:              string(raw.ItemId),
		PlaySessionId:       string(raw.PlaySessionId),
		DeviceProfile:       raw.DeviceProfile,
		IsPlayback:          flexBool(raw.IsPlayback),
		AutoOpenLiveStream:  flexBool(raw.AutoOpenLiveStream),
		EnableDirectPlay:    flexBool(raw.EnableDirectPlay),
		EnableDirectStream:  flexBool(raw.EnableDirectStream),
		EnableTranscoding:   flexBool(raw.EnableTranscoding),
		StartTimeTicks:      int64(raw.StartTimeTicks),
		AudioStreamIndex:    flexInt(raw.AudioStreamIndex),
		SubtitleStreamIndex: flexInt(raw.SubtitleStreamIndex),
		SubtitleIndexQuoted: strings.HasPrefix(strings.TrimSpace(string(raw.SubtitleStreamIndex)), `"`),
	}
	// Jellyfin's PlaybackInfoDto carries the player's quality choice as its
	// own MaxStreamingBitrate; Jellyfin Web leaves the device profile's at the
	// device maximum. The lower of the two is the limit, as with the query.
	if limit := int64(raw.MaxStreamingBitrate); limit > 0 && limit <= 1<<31-1 {
		if p.DeviceProfile.MaxStreamingBitrate == 0 || int(limit) < p.DeviceProfile.MaxStreamingBitrate {
			p.DeviceProfile.MaxStreamingBitrate = int(limit)
		}
	}
	return nil
}

func boolValue(p *bool, def bool) bool {
	if p == nil {
		return def
	}
	return *p
}

// jellyfinStaticPlayable is Infuse/Swiftfin GET /Videos/{id}/stream?Static=true.
func jellyfinStaticPlayable(protocol frontend.Protocol, channel string, opts frontend.DecideOptions) bool {
	return protocol == "jellyfin" && channel == "" && (opts.DirectPlay || opts.DirectStream)
}

func jellyfinStaticFallback(err error) bool {
	return err != nil && !errors.Is(err, frontend.ErrSessionBusy) && !errors.Is(err, frontend.ErrTranscodeCapacity) && !retryableMediaOpen(err) && !errors.Is(err, frontend.ErrJobsBusy)
}

func offerJellyfinStatic(source map[string]any, opts frontend.DecideOptions) {
	if source == nil {
		return
	}
	if opts.DirectPlay {
		source["SupportsDirectPlay"] = true
	}
	if opts.DirectPlay || opts.DirectStream {
		source["SupportsDirectStream"] = true
	}
	source["IsRemote"] = false
	source["Protocol"] = "File"
}

const msgVideoTranscodingOff = "This title needs a video transcode to play on this device, and video transcoding is turned off."

func streamError(protocol frontend.Protocol, w http.ResponseWriter, err error, languages ...string) {
	if frontend.WritePlaybackSourceProblem(w, err, languages...) {
		return
	}
	var bedtime *frontend.ErrBedtime
	if errors.As(err, &bedtime) {
		http.Error(w, bedtime.Error(), http.StatusForbidden)
		return
	}
	if errors.Is(err, frontend.ErrVideoTranscodingDisabled) {
		http.Error(w, msgVideoTranscodingOff, http.StatusUnprocessableEntity)
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

	if frontend.WritePlaybackPending(w, err, languages...) {
		return
	}
	if errors.Is(err, frontend.ErrJobsBusy) {
		w.Header().Set("Retry-After", strconv.Itoa(frontend.JobsBusyRetrySeconds(err, 5)))
		w.Header().Set("X-Ether-Media-Error", "jobs_busy")
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write(frontend.JobsBusyErrorBody(err))
		return
	}
	if errors.Is(err, frontend.ErrNotFound) || errors.Is(err, frontend.ErrNotAvailable) || errors.Is(err, frontend.ErrSourceGone) {
		if protocol == "jellyfin" {
			w.Header().Set("Content-Type", "text/plain")
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, "Error processing request.")
			return
		}
		http.Error(w, "Not Found", http.StatusNotFound)
		return
	}
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
	if msg := frontend.VideoEncoderCapacityMessage(err, languages...); msg != "" {
		http.Error(w, msg, http.StatusServiceUnavailable)
		return
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
	if themeParentID(r.PathValue("id")) != "" {
		i.themePlaybackInfo(w, r)
		return
	}
	if i.host.Sessions == nil || i.host.Capabilities == nil {
		http.Error(w, "Unavailable", 503)
		return
	}
	wireID := r.PathValue("id")
	key, err := ids.ParseRatingKeyGUID(wireID)
	channel := ""
	channelName := ""
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
		if n > 0 && (body.DeviceProfile.MaxStreamingBitrate == 0 || n < body.DeviceProfile.MaxStreamingBitrate) {
			body.DeviceProfile.MaxStreamingBitrate = n
		}
	}
	capability, err := i.host.Capabilities.Build(i.identityContext(r), frontend.CapabilityRequest{Viewer: v, Device: caps.DeviceInfo{Protocol: i.protocol, ClientID: dev.ID, Product: dev.Client, DeviceName: dev.DeviceName, Version: dev.Version}, Profile: body.DeviceProfile, UserAgent: r.UserAgent()})
	if err != nil {
		http.Error(w, "Unavailable", 503)
		return
	}
	if frontend.IsCatchUpGUID(wireID) {
		i.catchUpPlaybackInfo(w, r, v, credential, wireID, body, capability.Client)
		return
	}
	adaptiveHLS := queryBool(r, "EnableAdaptiveBitrateStreaming", false)
	capability.Client.AwaitAdmissionIndex = capability.Client.AwaitAdmissionIndex || adaptiveHLS
	scope := frontend.VersionScope{Viewer: v, Device: dev, Protocol: i.protocol, Caps: capability.Client}
	versionFromItem := ""
	if itemIDError != nil {
		if target, _, _, e := i.resolveSource(r, scope, wireID); e == nil && target != 0 {
			key, versionFromItem = target, wireID
		} else if staleItem, ok := i.resolveStaleSource(r, scope, wireID); ok && staleItem != 0 {
			key = staleItem
		}
		if key == 0 && versionFromItem == "" {
			if i.host.LiveTV == nil {
				http.NotFound(w, r)
				return
			}
			// A program plays by tuning the channel it is on. A channel id
			// tunes that channel directly.
			if _, e := ids.ParseGUID(ids.Programme, wireID); e == nil {
				p, e := i.host.LiveTV.ResolveProgrammeGUID(r.Context(), v, wireID, programmeLookupQuery(time.Now().UTC()))
				if e != nil || p.ChannelID == "" {
					http.NotFound(w, r)
					return
				}
				channel, channelName, key = p.ChannelID, p.Title, 0
			} else {
				ch, e := i.host.LiveTV.ResolveChannelGUID(r.Context(), v, wireID)
				if e != nil || ch.ID == "" {
					http.NotFound(w, r)
					return
				}
				channel, channelName, key = ch.ID, ch.Name, 0
			}
		}
	}
	var playbackItem frontend.Title
	if channel == "" && key != 0 && key.Kind() != frontend.KindExtra {
		var ok bool
		playbackItem, ok = i.authorizePlaybackItem(r, v, key)
		if !ok {
			http.NotFound(w, r)
			return
		}
		if key.IsPluginKey() && mbIsBookItem(playbackItem) {
			i.playbackInfoFailure(w, r, v, "plan", frontend.ErrNotAvailable)
			return
		}
	}
	// Native clients commonly omit IsPlayback on POST when starting a stream.
	// GET is informational by default and must not open a release.
	playback := boolValue(body.IsPlayback, r.Method != http.MethodGet)
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
	if channel != "" && queryBool(r, "AutoOpenLiveStream", boolValue(body.AutoOpenLiveStream, false)) {
		playback = true
	}
	// A queue prefetch from a device playing another title is answered from
	// the plan alone; its stream opens the session when the player gets to it.
	queued := false
	if playback && channel == "" && i.deferQueuedPlayback(r, v, dev, key) {
		playback, queued = false, true
	}
	opts := frontend.DecideOptions{DirectPlay: queryBool(r, "EnableDirectPlay", boolValue(body.EnableDirectPlay, true)), DirectStream: queryBool(r, "EnableDirectStream", boolValue(body.EnableDirectStream, true)), AudioStream: -1, SubtitleStream: -1}
	if body.AudioStreamIndex != nil {
		opts.AudioStream = *body.AudioStreamIndex
	}
	if body.SubtitleStreamIndex != nil {
		opts.SubtitleStream = *body.SubtitleStreamIndex
	}
	audioSet, subtitleSet := body.AudioStreamIndex != nil, body.SubtitleStreamIndex != nil
	// A subtitle index Jellyfin Web copied from its menu (a quoted body
	// value); a query parameter below replaces it with an explicit one.
	subtitleFromMenu := subtitleSet && body.SubtitleIndexQuoted
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
			if field.name == "SubtitleStreamIndex" {
				subtitleFromMenu = false
			}
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
	// A posted DeviceProfile is this device's real declaration; detail pages
	// warm with it later (frontend.DetailWarmer).
	intent.DeclaredCaps = channel == "" && (len(body.DeviceProfile.DirectPlayProfiles) > 0 || len(body.DeviceProfile.TranscodingProfiles) > 0)
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
		sameLiveChannel := false
		if channel != "" {
			if chGUID, e := ids.StringGUID(ids.Channel, channel); e == nil && strings.EqualFold(sourceID, chGUID) {
				sameLiveChannel = true
			}
		}
		if !sameLiveChannel {
			if parsed, e := ids.ParseRatingKeyGUID(sourceID); e != nil || parsed != key || channel != "" {
				version = sourceID
			}
		}
	}
	detailSource, expectedETag, resolvedRelease := "", "", ""
	if version != "" {
		if channel != "" {
			http.NotFound(w, r)
			return
		}
		target, kind, tag, e := i.resolveSourceFor(r, scope, key, version)
		if e != nil || target != key || (versionFromItem != "" && !sameVersionHandle(version, versionFromItem)) {
			if versionFromItem == "" {
				if staleItem, ok := i.resolveStaleSource(r, scope, version); ok && staleItem == key {
					version = ""
				} else {
					http.NotFound(w, r)
					return
				}
			} else {
				http.NotFound(w, r)
				return
			}
		} else if kind == "detail" {
			detailSource, expectedETag, version = version, tag, ""
		} else {
			intent.ExactVersion = true
			// Selecting a version is foreground playback preparation, even
			// when a client omits IsPlayback on its PlaybackInfo request.
			// Probe this release so its own embedded tracks reach the picker.
			intent.Estimate = false
			resolvedRelease = tag
		}
	}
	if i.extraPlayback(w, r, v, key, playback) {
		return
	}
	versionRequested := version != "" || detailSource != ""
	// The ranked release variants are listed (and Auto may switch to one) only
	// when the client did not pick a version and this is a single playable
	// item, never a live channel or a plugin kind.
	var variants []frontend.ReleaseVariant
	titleLanguage, titleName := playbackItem.Language, playbackItem.Title
	if i.host.Editions != nil && !versionRequested && channel == "" && !key.IsPluginKey() && playbackItem.Title != "" {
		variants = i.releaseVariants(r, scope, playbackItem)
	}
	listVersions := len(variants) > 1
	// Jellyfin Web posts its hidden subtitle menu's "-1" (a quoted body
	// value) when the detail page it showed had no subtitle list (the release
	// was not probed yet). Nobody chose "off" from a list never sent: the
	// viewer's rules decide. Clients that send a number keep their explicit
	// choice, Emby's included: native Emby apps and Infuse send Off from list
	// endpoints that never record a list here.
	if i.protocol == "jellyfin" && subtitleFromMenu && opts.SubtitleStream < 0 && channel == "" && !i.subtitleLists.shown(v, key, time.Now()) {
		subtitleSet = false
		intent.SubtitleSet = false
	}
	autoSwitchedRelease := ""
	planBase := frontend.PlanRequest{Viewer: v, Device: dev, Protocol: i.protocol, Item: key, Channel: channel, Version: version, ResolvedRelease: resolvedRelease, DetailSource: detailSource, ExpectedSourceETag: expectedETag, Caps: capability.Client, Intent: intent}
	// Auto skips a video transcode: when the automatic pick would re-encode
	// video for this device and a ranked alternative would not, play the
	// first alternative that avoids it (up to maxAutoTranscodeTrials). The
	// choice is made from estimated plans first so the release about to be
	// skipped is never opened and probed.
	var plan frontend.PlayPlan
	err = nil
	planned, triedAlternatives := false, false
	if playback && !versionRequested && len(variants) > 1 {
		estimate := planBase
		estimate.Intent.Estimate = true
		if guess, e := i.host.Sessions.Plan(r.Context(), estimate); e == nil && videoTranscode(guess) {
			triedAlternatives = true
			if release := i.avoidVideoTranscode(r.Context(), planBase, variants); release != "" {
				if alt, p, handle, ok := i.autoSwitchPlan(r, scope, planBase, release); ok {
					planBase, plan, planned = alt, p, true
					autoSwitchedRelease, version = release, handle
				}
			}
		}
	}
	if !planned {
		plan, err = i.host.Sessions.Plan(r.Context(), planBase)
		if err != nil && errors.Is(err, frontend.ErrVideoTranscodingDisabled) && detailSource != "" && version == "" {
			detailSource = ""
			planBase.DetailSource, planBase.ExpectedSourceETag = "", ""
			planBase.Intent.AudioSet, planBase.Intent.SubtitleSet = false, false
			planBase.Intent.Options.AudioStream, planBase.Intent.Options.SubtitleStream = -1, -1
			planBase.Intent.Options.BurnSubtitles = false
			plan, err = i.host.Sessions.Plan(r.Context(), planBase)
		}
	}
	if err != nil {
		if i.protocol == "jellyfin" {
			i.playbackInfoFailure(w, r, v, "plan", err)
		} else if frontend.WritePlaybackSourceProblem(w, err, r.Header.Get("Accept-Language")) {
			return
		} else if noCompatibleStream(err) {
			i.playbackInfoFailure(w, r, v, "plan", err)
		} else if retryableMediaOpen(err) || errors.Is(err, frontend.ErrJobsBusy) {
			streamError(i.protocol, w, err, r.Header.Get("Accept-Language"))
		} else {
			http.Error(w, "Playback unavailable", 503)
		}
		return
	}
	if playback && (plan.Estimated || (channel == "" && plan.Media.Estimated)) {
		i.playbackInfoFailure(w, r, v, "plan", frontend.ErrNotAvailable)
		return
	}
	// The estimate can miss a transcode the probed media reveals: check the
	// real plan too.
	if playback && !versionRequested && !triedAlternatives && videoTranscode(plan) && len(variants) > 1 {
		if release := i.avoidVideoTranscode(r.Context(), planBase, variants); release != "" {
			if alt, p, handle, ok := i.autoSwitchPlan(r, scope, planBase, release); ok {
				planBase, plan = alt, p
				autoSwitchedRelease, version = release, handle
			}
		}
	}
	if !allowed && plan.Decision.Kind == "transcode" {
		i.playbackInfoFailure(w, r, v, "transcoding disabled", frontend.ErrNotAvailable)
		return
	}

	sourceID = ids.RatingKeyGUID(key)
	if channel != "" {
		sourceID = wireID
	} else if autoSwitchedRelease != "" {
		// Auto played a different release than it would have: identify it by
		// its scoped handle (the session was planned through it), never the
		// raw release id.
		sourceID = version
	} else if version != "" {
		sourceID = version
	} else if detailSource != "" {
		sourceID = detailSource
	}
	// A declared device profile is authoritative. Static-file clients without
	// one can use the generic static endpoint; Roku must follow the broker
	// decision instead of being told to direct-play an incompatible source.
	staticPlayable := jellyfinStaticPlayable(i.protocol, channel, opts) && !intent.DeclaredCaps
	media := plan.Media
	media.Estimated = media.Estimated || plan.Estimated
	source := i.mediaSourceDTO(sourceID, media)
	if channel != "" || plan.Decision.Kind == "transcode" {
		// These streams are produced by Ether, even when their input is remote.
		source["IsRemote"] = false
	}
	if i.protocol == "jellyfin" && channel == "" && (!media.Estimated || staticPlayable) {
		// Infuse and Swiftfin direct-play a Jellyfin file with
		// GET /Videos/{id}/stream?Static=true. They do not open Path.
		// IsRemote true would tell Infuse that Path is an upstream URL to
		// fetch itself. Ether never publishes that URL.
		source["IsRemote"] = false
		source["Protocol"] = "File"
	}
	if plan.ETag != "" {
		source["ETag"] = plan.ETag
	}
	source["SupportsDirectPlay"], source["SupportsDirectStream"], source["SupportsTranscoding"] = plan.DirectPlay, plan.DirectStream, plan.Transcoding
	if staticPlayable {
		offerJellyfinStatic(source, opts)
	}
	source["IsInfiniteStream"] = channel != ""
	if channel != "" {
		i.shapeLiveMediaSource(source, sourceID, channelName, "", "", media, false)
	}
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
	if queued {
		i.rememberQueuedPlayback(r, v, key, dev, version, detailSource, sourceID, planBase)
	}
	if !playback && channel == "" && (opts.DirectPlay || opts.DirectStream) {
		if path := i.lazySourceURL(r, credential, planBase); path != "" {
			source["Path"] = path
		}
	}
	if playback {
		session, err := i.host.Sessions.Open(r.Context(), frontend.OpenRequest{Protocol: i.protocol, PlanID: plan.ID, Viewer: v, Device: dev})
		if err != nil {
			if channel != "" {
				i.liveTuneError(w, err)
				return
			}
			if !jellyfinStaticFallback(err) || !staticPlayable {
				if i.protocol == "jellyfin" {
					i.playbackInfoFailure(w, r, v, "open", err)
				} else if frontend.WritePlaybackSourceProblem(w, err, r.Header.Get("Accept-Language")) {
					return
				} else if noCompatibleStream(err) {
					i.playbackInfoFailure(w, r, v, "open", err)
				} else if retryableMediaOpen(err) || errors.Is(err, frontend.ErrJobsBusy) {
					streamError(i.protocol, w, err, r.Header.Get("Accept-Language"))
				} else {
					http.Error(w, "Playback unavailable", 503)
				}
				return
			}
			if i.host.Log != nil {
				i.host.Log.Warn("jellyfin static PlaybackInfo open", "err", err)
			}
		} else {
			i.rememberAnonymousSession(r, v, session.ID, key, channel, dev, version, detailSource, sourceID, planBase)
			format := frontend.StreamDirect
			audioOnly := channel == "" && plan.Media.Video() == nil && plan.Media.HasPlayableMedia()
			if channel != "" || (plan.Decision.Kind != "directplay" && ((i.protocol == "jellyfin" && intent.DeclaredCaps) || plan.Decision.Kind != "copy" || !plan.Decision.AudioCopy)) {
				if audioOnly && plan.HTTP && (!plan.HLS || !mbIsHLSContainer(plan.Decision.Container)) {
					format = frontend.StreamHTTP
				} else {
					format = frontend.StreamHLS
				}
			}
			if adaptiveHLS && plan.Media.Video() != nil && plan.HLS {
				format = frontend.StreamHLS
			}
			// The progressive client requires an active playback demuxer. A
			// direct-file plan becomes HLS (copy remux where codecs allow it),
			// so all embedded text rides the same input read as the picture.
			if i.protocol == "jellyfin" && channel == "" && format == frontend.StreamDirect && (plan.Media.Size == 0 || plan.Media.Size > 32<<20) {
				progressive := false
				for _, p := range body.DeviceProfile.SubtitleProfiles {
					if p.Format == "ether-progressive-vtt" {
						progressive = true
					}
				}
				if progressive {
					for _, track := range plan.Media.Streams {
						if track.Type == "subtitle" && !track.External && (track.Codec == "subrip" || track.Codec == "srt" || track.Codec == "webvtt") {
							format = frontend.StreamHLS
							break
						}
					}
				}
			}
			// Jellyfin never offsets an HLS playlist by StartTimeTicks: the
			// playlist spans the whole title and every client seeks its player
			// to the resume point. An offset playlist puts Streamyfin, Jellyfin
			// for Android TV and others an offset away from where they think.
			fullTimeline := i.protocol == "jellyfin" && channel == "" && format == frontend.StreamHLS
			url, err := i.host.Sessions.StartStream(r.Context(), frontend.SessionAccess{Viewer: v, Device: dev, Protocol: i.protocol}, session.ID, frontend.StreamRequest{Format: format, Position: intent.Position, FullTimeline: fullTimeline, AdaptiveHLS: adaptiveHLS})
			if err != nil && format == frontend.StreamHLS && staticPlayable {
				url, err = i.host.Sessions.StartStream(r.Context(), frontend.SessionAccess{Viewer: v, Device: dev, Protocol: i.protocol}, session.ID, frontend.StreamRequest{Format: frontend.StreamDirect, Position: intent.Position})
				if err == nil {
					format = frontend.StreamDirect
				}
			}
			if err != nil {
				_ = i.host.Sessions.Stop(r.Context(), frontend.SessionAccess{Viewer: v, Device: dev, Protocol: i.protocol}, session.ID)
				if !jellyfinStaticFallback(err) || !staticPlayable {
					i.playbackInfoFailure(w, r, v, "start stream", err)
					return
				}
				if i.host.Log != nil {
					i.host.Log.Warn("jellyfin static PlaybackInfo stream", "err", err)
				}
			} else {
				scopedURL := url
				if prefix, ok := r.Context().Value(prefixKey{}).(string); ok {
					url = prefix + url
				}
				if channel != "" {
					if session.Media != nil {
						media = *session.Media
					}
					media = defaultLiveMediaInfo(media)
					source = i.mediaSourceDTO(sourceID, media)
					i.shapeLiveMediaSource(source, sourceID, channelName, session.ID, url, media, true)
				} else if subtitleService := i.host.Sessions; subtitleService != nil {
					streams := i.mediaStreams(plan.Media.Streams)
					subAccess := frontend.SessionAccess{Viewer: v, Device: dev, Protocol: i.protocol}
					type subSlot struct {
						dtoIndex  int
						stream    frontend.StreamInfo
						embed     bool
						hlsReqIdx int
						vttReqIdx int
					}
					var slots []subSlot
					var reqs []frontend.SubtitleURLRequest
					n := -1
					for _, stream := range plan.Media.Streams {
						if mediaStreamKind(stream.Type) == "" {
							continue
						}
						n++
						if stream.Type != "subtitle" {
							continue
						}
						// A direct-played file carries its embedded tracks: a
						// player that declares it embeds this format renders it
						// from the file, as Jellyfin answers. Delivering it as an
						// external file instead extracts the whole track from a
						// remote source first: Jellyfin for Android TV paused and
						// Wholphin spun until it arrived, and the added side
						// tracks broke Jellyfin for Android's track mapping.
						if format == frontend.StreamDirect && !stream.External && stream.ExternalURL == "" && embedsSubtitle(body.DeviceProfile.SubtitleProfiles, stream.Codec) {
							slots = append(slots, subSlot{dtoIndex: n, stream: stream, embed: true, hlsReqIdx: -1, vttReqIdx: -1})
							continue
						}
						slot := subSlot{dtoIndex: n, stream: stream, hlsReqIdx: -1, vttReqIdx: -1}
						if format == frontend.StreamHLS {
							slot.hlsReqIdx = len(reqs)
							reqs = append(reqs, frontend.SubtitleURLRequest{Index: stream.Index, Format: "hls"})
						}
						slot.vttReqIdx = len(reqs)
						reqs = append(reqs, frontend.SubtitleURLRequest{Index: stream.Index, Format: "vtt"})
						slots = append(slots, slot)
					}
					var urls []string
					if len(reqs) > 0 {
						if batch, ok := subtitleService.(frontend.SubtitleURLBatch); ok {
							if got, err := batch.SubtitleURLs(r.Context(), subAccess, session.ID, reqs); err == nil && len(got) == len(reqs) {
								urls = got
							}
						}
						if urls == nil {
							urls = make([]string, len(reqs))
							for idx, req := range reqs {
								if u, e := subtitleService.SubtitleURL(r.Context(), subAccess, session.ID, req.Index, req.Format); e == nil {
									urls[idx] = u
								}
							}
						}
					}
					prefix, hasPrefix := r.Context().Value(prefixKey{}).(string)
					for _, slot := range slots {
						dto := streams[slot.dtoIndex].(map[string]any)
						// A track the HLS stream carries as a rendition is
						// delivered there, as Jellyfin does for "Hls" clients.
						if slot.hlsReqIdx >= 0 && slot.hlsReqIdx < len(urls) && urls[slot.hlsReqIdx] != "" {
							dto["DeliveryMethod"] = "Hls"
							delete(dto, "DeliveryUrl")
							dto["IsTextSubtitleStream"] = true
							dto["SupportsExternalStream"] = true
							continue
						}
						if slot.embed {
							dto["DeliveryMethod"] = "Embed"
							delete(dto, "DeliveryUrl")
							dto["IsTextSubtitleStream"] = frontend.IsTextSubtitle(slot.stream.Codec)
							continue
						}
						if slot.vttReqIdx < 0 || slot.vttReqIdx >= len(urls) {
							continue
						}
						subtitleURL := urls[slot.vttReqIdx]
						if !strings.HasPrefix(subtitleURL, "/sessions/media/") || strings.ContainsAny(subtitleURL, "\r\n") {
							continue
						}
						if hasPrefix {
							subtitleURL = prefix + subtitleURL
						}
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
					if !plan.Decision.AudioCopy {
						source["SupportsDirectPlay"] = plan.DirectPlay
						source["SupportsDirectStream"] = plan.DirectStream
						delete(source, "Path")
					}
					if i.protocol == "jellyfin" && intent.DeclaredCaps {
						source["SupportsDirectPlay"] = false
						source["SupportsDirectStream"] = false
						source["Protocol"] = "Http"
						delete(source, "Path")
					}
					source["TranscodingUrl"] = url
					if format == frontend.StreamHTTP {
						source["TranscodingSubProtocol"] = "http"
						if c := plan.Decision.Container; c != "" {
							source["TranscodingContainer"] = c
						}
					} else {
						source["TranscodingSubProtocol"] = "hls"
						if channel != "" {
							source["TranscodingContainer"] = "ts"
						} else if container := hlsContainerDTO(plan); container != "" {
							source["TranscodingContainer"] = container
						}
					}
				}
			}
		}
	}
	i.mediaSourceDialect(source)
	if i.protocol == "emby" {
		i.embyMediaSourceDTO(source, wireID, media)
	}
	if i.protocol == "jellyfin" {
		i.jellyfinMediaSourceDTO(source, media, channel != "")
		if body.DeviceProfile.AlwaysUseMostCompatibleTranscodingProfile {
			source["UseMostCompatibleTranscodingProfile"] = true
		}
		if name, _ := source["Name"].(string); name == "" && key != 0 {
			if titleName == "" {
				if title, ok := i.playbackTitle(r, v, key); ok {
					titleName = title.Title
				}
			}
			if titleName != "" {
				source["Name"] = titleName
			}
		}
	}
	if !media.Estimated && channel == "" {
		applyPlannedStreamIndexes(source, media, plan.Decision, plan.SubtitleStream())
	}
	sources := []any{source}
	if listVersions && !versionRequested && autoSwitchedRelease == "" {
		if alternatives := i.releaseVersionSourcesFor(r, scope, key, titleLanguage, variants, source); alternatives != nil {
			sources = alternatives
		}
	}
	if channel == "" {
		playbackItem.Key = key
		for _, raw := range sources {
			file := raw.(map[string]any)
			i.completeFileSource(r, file, playbackItem, "")
			if i.protocol == "jellyfin" {
				delete(file, "Chapters")
			}
		}
		i.subtitleLists.note(v, key, sources, time.Now())
	}
	jsonReply(w, map[string]any{"MediaSources": sources, "PlaySessionId": playID})
}

func (i *instance) liveStreamsOpen(w http.ResponseWriter, r *http.Request) {
	v, _, err := i.viewer(r)
	if err != nil || v == nil {
		http.Error(w, "Unauthorized", 401)
		return
	}
	if i.host.Sessions == nil || i.host.Capabilities == nil || i.host.LiveTV == nil {
		http.Error(w, "Unavailable", 503)
		return
	}
	var body playbackRequest
	if r.Body != nil && r.ContentLength != 0 {
		if decode(r, &body) != nil {
			http.Error(w, "Bad Request", 400)
			return
		}
	}
	if values := route.Query(r.URL.Query(), "OpenToken"); len(values) == 1 && values[0] != "" {
		body.OpenToken = values[0]
	}
	if values := route.Query(r.URL.Query(), "ItemId"); len(values) == 1 && values[0] != "" {
		body.ItemId = values[0]
	}
	wireID := body.OpenToken
	if wireID == "" {
		wireID = body.ItemId
	}
	if wireID == "" {
		http.Error(w, "Bad Request", 400)
		return
	}
	channel, channelName := "", ""
	if _, e := ids.ParseGUID(ids.Programme, wireID); e == nil {
		p, e := i.host.LiveTV.ResolveProgrammeGUID(r.Context(), v, wireID, programmeLookupQuery(time.Now().UTC()))
		if e != nil || p.ChannelID == "" {
			http.NotFound(w, r)
			return
		}
		channel, channelName = p.ChannelID, p.Title
	} else {
		ch, e := i.host.LiveTV.ResolveChannelGUID(r.Context(), v, wireID)
		if e != nil || ch.ID == "" {
			http.NotFound(w, r)
			return
		}
		channel, channelName = ch.ID, ch.Name
	}
	dev := device(r)
	if values := route.Query(r.URL.Query(), "MaxStreamingBitrate"); len(values) == 1 {
		n, e := strconv.Atoi(values[0])
		if e != nil || n < 0 {
			http.Error(w, "Bad Request", 400)
			return
		}
		if n > 0 && (body.DeviceProfile.MaxStreamingBitrate == 0 || n < body.DeviceProfile.MaxStreamingBitrate) {
			body.DeviceProfile.MaxStreamingBitrate = n
		}
	}
	capability, err := i.host.Capabilities.Build(i.identityContext(r), frontend.CapabilityRequest{Viewer: v, Device: caps.DeviceInfo{Protocol: i.protocol, ClientID: dev.ID, Product: dev.Client, DeviceName: dev.DeviceName, Version: dev.Version}, Profile: body.DeviceProfile, UserAgent: r.UserAgent()})
	if err != nil {
		http.Error(w, "Unavailable", 503)
		return
	}
	allowed := true
	opts := frontend.DecideOptions{DirectPlay: queryBool(r, "EnableDirectPlay", boolValue(body.EnableDirectPlay, true)), DirectStream: queryBool(r, "EnableDirectStream", boolValue(body.EnableDirectStream, true)), AudioStream: -1, SubtitleStream: -1}
	intent := frontend.PlayIntent{Options: opts, AllowTranscoding: &allowed}
	plan, err := i.host.Sessions.Plan(r.Context(), frontend.PlanRequest{Viewer: v, Device: dev, Protocol: i.protocol, Channel: channel, Caps: capability.Client, Intent: intent})
	if err != nil {
		i.liveTuneError(w, err)
		return
	}
	session, err := i.host.Sessions.Open(r.Context(), frontend.OpenRequest{Protocol: i.protocol, PlanID: plan.ID, Viewer: v, Device: dev})
	if err != nil {
		i.liveTuneError(w, err)
		return
	}
	url, err := i.host.Sessions.StartStream(r.Context(), frontend.SessionAccess{Viewer: v, Device: dev, Protocol: i.protocol}, session.ID, frontend.StreamRequest{Format: frontend.StreamHLS})
	if err != nil {
		_ = i.host.Sessions.Stop(r.Context(), frontend.SessionAccess{Viewer: v, Device: dev, Protocol: i.protocol}, session.ID)
		streamError(i.protocol, w, err, r.Header.Get("Accept-Language"))
		return
	}
	if prefix, ok := r.Context().Value(prefixKey{}).(string); ok {
		url = prefix + url
	}
	var media frontend.MediaInfo
	if session.Media != nil {
		media = *session.Media
	}
	media = defaultLiveMediaInfo(media)
	source := i.mediaSourceDTO(wireID, media)
	i.shapeLiveMediaSource(source, wireID, channelName, session.ID, url, media, true)
	i.mediaSourceDialect(source)
	if i.protocol == "emby" {
		i.embyMediaSourceDTO(source, wireID, media)
	}
	if i.protocol == "jellyfin" {
		i.jellyfinMediaSourceDTO(source, media, true)
		if body.DeviceProfile.AlwaysUseMostCompatibleTranscodingProfile {
			source["UseMostCompatibleTranscodingProfile"] = true
		}
	}
	jsonReply(w, map[string]any{"MediaSource": source})
}

// liveStreamsMediaInfo returns the source of an already open, authorized live
// stream. Looking up metadata never opens another provider connection.
func (i *instance) liveStreamsMediaInfo(w http.ResponseWriter, r *http.Request) {
	v, _, err := i.viewer(r)
	if err != nil || v == nil {
		http.Error(w, "Unauthorized", 401)
		return
	}
	if i.host.Sessions == nil {
		http.Error(w, "Unavailable", 503)
		return
	}
	var body struct {
		LiveStreamId string `json:"LiveStreamId"`
	}
	if r.Body != nil && r.ContentLength != 0 {
		if err := decode(r, &body); err != nil && !errors.Is(err, io.EOF) {
			http.Error(w, "Bad Request", 400)
			return
		}
	}
	streamID := body.LiveStreamId
	if values := route.Query(r.URL.Query(), "LiveStreamId"); len(values) > 1 {
		http.Error(w, "Bad Request", 400)
		return
	} else if len(values) == 1 && values[0] != "" {
		streamID = values[0]
	}
	if streamID == "" {
		http.Error(w, "Bad Request", 400)
		return
	}
	access := frontend.SessionAccess{Viewer: v, Device: device(r), Protocol: i.protocol}
	sessions, err := i.host.Sessions.Sessions(r.Context(), access)
	if err != nil {
		http.Error(w, "Unavailable", 503)
		return
	}
	for _, session := range sessions {
		if session.ID != streamID || session.Channel == "" {
			continue
		}
		url, err := i.host.Sessions.StartStream(r.Context(), access, streamID, frontend.StreamRequest{Format: frontend.StreamHLS})
		if err != nil {
			streamError(i.protocol, w, err, r.Header.Get("Accept-Language"))
			return
		}
		if prefix, ok := r.Context().Value(prefixKey{}).(string); ok {
			url = prefix + url
		}
		id, err := ids.StringGUID(ids.Channel, session.Channel)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		var media frontend.MediaInfo
		if session.Media != nil {
			media = *session.Media
		}
		media = defaultLiveMediaInfo(media)
		source := i.mediaSourceDTO(id, media)
		i.shapeLiveMediaSource(source, id, "", streamID, url, media, true)
		i.mediaSourceDialect(source)
		if i.protocol == "jellyfin" {
			i.jellyfinMediaSourceDTO(source, media, true)
		}
		if i.protocol == "emby" {
			i.embyMediaSourceDTO(source, id, media)
		}
		jsonReply(w, source)
		return
	}
	http.NotFound(w, r)
}

func (i *instance) liveStreamsClose(w http.ResponseWriter, r *http.Request) {
	v, _, err := i.viewer(r)
	if err != nil || v == nil {
		http.Error(w, "Unauthorized", 401)
		return
	}
	if i.host.Sessions == nil {
		http.Error(w, "Unavailable", 503)
		return
	}
	var body struct {
		LiveStreamId  string `json:"LiveStreamId"`
		PlaySessionId string `json:"PlaySessionId"`
	}
	if r.Body != nil && r.ContentLength != 0 {
		if e := decode(r, &body); e != nil && !errors.Is(e, io.EOF) {
			http.Error(w, "Bad Request", 400)
			return
		}
	}
	liveStreamID := body.LiveStreamId
	if values := route.Query(r.URL.Query(), "liveStreamId"); len(values) > 1 {
		http.Error(w, "Bad Request", 400)
		return
	} else if len(values) == 1 && values[0] != "" {
		liveStreamID = values[0]
	}
	if liveStreamID == "" {
		if values := route.Query(r.URL.Query(), "PlaySessionId"); len(values) == 1 && values[0] != "" {
			liveStreamID = values[0]
		} else {
			liveStreamID = body.PlaySessionId
		}
	}
	if liveStreamID == "" {
		http.Error(w, "Bad Request", 400)
		return
	}
	access := frontend.SessionAccess{Viewer: v, Device: device(r), Protocol: i.protocol}
	if err := i.host.Sessions.Stop(r.Context(), access, liveStreamID); err != nil && !errors.Is(err, frontend.ErrNotFound) {
		http.Error(w, "Unavailable", 503)
		return
	}
	if i.protocol == "emby" {
		w.WriteHeader(http.StatusOK)
		return
	}
	w.WriteHeader(http.StatusNoContent)
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
		"Id": id, "Container": media.Container, "RunTimeTicks": media.Duration.Nanoseconds() / 100, "Bitrate": media.BitrateKbps * 1000,
		"Protocol": "Http", "Type": "Default", "IsRemote": true, "ReadAtNativeFramerate": false, "IgnoreDts": false, "IgnoreIndex": false, "GenPtsInput": false,
		"SupportsDirectPlay": false, "SupportsDirectStream": false, "SupportsTranscoding": false, "IsInfiniteStream": false,
		// Jellyfin's strict SDK requires a boolean. Until the broker exposes
		// version-scoped marker status, use the conservative false fallback.
		"RequiresOpening": false, "RequiresClosing": false, "RequiresLooping": false, "SupportsProbing": !media.Estimated && len(media.Streams) > 0, "TranscodingSubProtocol": "http", "HasSegments": false,
		"Formats": []string{}, "MediaAttachments": []any{}, "RequiredHttpHeaders": map[string]string{}, "MediaStreams": mediaStreams(media.Streams),
	}
	if size := mediaSourceSize(media); size > 0 {
		source["Size"] = size
	}
	if !media.Estimated {
		addChapters(source, media)
	}
	return source
}

func mediaSourceSize(media frontend.MediaInfo) int64 {
	if media.FileSize > 0 {
		return media.FileSize
	}
	if !media.Estimated {
		return media.Size
	}
	return 0
}

func (i *instance) mediaSourceDTO(id string, media frontend.MediaInfo) map[string]any {
	source := mediaSourceDTO(id, media)
	source["MediaStreams"] = i.mediaStreams(media.Streams)
	return source
}

func (i *instance) mediaStreams(streams []frontend.StreamInfo) []any {
	out := mediaStreams(streams)
	n := 0
	for _, stream := range streams {
		if mediaStreamKind(stream.Type) == "" {
			continue
		}
		if i.protocol == "emby" {
			i.embyMediaStreamDTO(out[n].(map[string]any), stream)
		} else if i.protocol == "jellyfin" {
			i.jellyfinStreamDTO(out[n].(map[string]any), stream)
		}
		n++
	}
	return out
}

// mediaStreamKind maps a probe stream type to its MediaBrowser kind, or
// "" for streams (data, attachments) that have none.
func mediaStreamKind(t string) string {
	return map[string]string{"video": "Video", "audio": "Audio", "subtitle": "Subtitle"}[t]
}

func mediaStreams(streams []frontend.StreamInfo) []any {
	out := make([]any, 0, len(streams))
	for _, s := range streams {
		kind := mediaStreamKind(s.Type)
		if kind == "" {
			// MediaStream.Type is a required enum in the Jellyfin SDK;
			// an empty value fails decoding of the whole item (#614).
			// Data/attachment streams have no MediaBrowser kind.
			continue
		}
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
	if len(media.Chapters) > 0 || media.ChaptersProbed {
		source["Chapters"] = chapterDTOs(media.Chapters)
	}
}

func chapterDTOs(known []frontend.Chapter) []any {
	chapters := make([]any, 0, len(known))
	for _, chapter := range known {
		entry := map[string]any{"StartPositionTicks": chapter.Start.Nanoseconds() / 100, "ImageDateModified": wireTime(time.Time{})}
		if chapter.Title != "" {
			entry["Name"] = chapter.Title
		}
		chapters = append(chapters, entry)
	}
	return chapters
}
func (i *instance) playbackRoutes() error {
	if err := i.router.Handle("GET", "/Playback/BitrateTest", http.HandlerFunc(i.bitrateTest)); err != nil {
		return err
	}
	for _, method := range []string{"GET", "HEAD"} {
		if i.protocol == "jellyfin" {
			if err := i.router.Handle(method, "/Videos/{id}/Trickplay/{width}/{name}", http.HandlerFunc(i.trickplayImage)); err != nil {
				return err
			}
		}
		if err := i.router.Handle(method, "/Videos/{id}/CatchUp/{session}/{file}", http.HandlerFunc(i.catchUpStream)); err != nil {
			return err
		}
		if err := i.router.Handle(method, "/Videos/{id}/{name}", http.HandlerFunc(i.streamAlias)); err != nil {
			return err
		}
		if err := i.router.Handle(method, "/Audio/{id}/{name}", http.HandlerFunc(i.streamAlias)); err != nil {
			return err
		}
		if err := i.router.Handle(method, "/Items/{id}/Download", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			i.serveItemFile(w, r, true)
		})); err != nil {
			return err
		}
		if err := i.router.Handle(method, "/Items/{id}/File", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			i.serveItemFile(w, r, false)
		})); err != nil {
			return err
		}
	}
	if err := i.router.Handle("GET", "/MediaSegments/{itemId}", http.HandlerFunc(i.mediaSegments)); err != nil {
		return err
	}
	if err := i.router.Handle("GET", "/Items/{id}/RemoteSearch/Subtitles/{language}", http.HandlerFunc(i.remoteSubtitles)); err != nil {
		return err
	}
	if err := i.router.Handle("POST", "/Items/{id}/RemoteSearch/Subtitles/{subtitleId}", http.HandlerFunc(i.downloadSubtitle)); err != nil {
		return err
	}
	for _, entry := range []struct {
		path    string
		stopped bool
	}{{"/Sessions/Playing", false}, {"/Sessions/Playing/Progress", false}, {"/Sessions/Playing/Stopped", true}} {
		stop := entry.stopped
		started := entry.path == "/Sessions/Playing"
		if err := i.router.Handle("POST", entry.path, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { i.timeline(w, r, stop, started) })); err != nil {
			return err
		}
	}
	if err := i.router.Handle("POST", "/Sessions/Playing/Ping", http.HandlerFunc(i.playingPing)); err != nil {
		return err
	}
	for _, path := range []string{"/Users/{userId}/PlayingItems/{itemId}", "/PlayingItems/{itemId}"} {
		itemPath := path
		if err := i.router.Handle("POST", itemPath, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { i.timeline(w, r, false, true) })); err != nil {
			return err
		}
		if err := i.router.Handle("POST", itemPath+"/Progress", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { i.timeline(w, r, false, false) })); err != nil {
			return err
		}
		if err := i.router.Handle("DELETE", itemPath, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { i.timeline(w, r, true, false) })); err != nil {
			return err
		}
	}
	if err := i.router.Handle("POST", "/Items/{id}/PlaybackInfo", http.HandlerFunc(i.playbackInfo)); err != nil {
		return err
	}
	if err := i.router.Handle("POST", "/LiveStreams/Open", http.HandlerFunc(i.liveStreamsOpen)); err != nil {
		return err
	}
	if err := i.router.Handle("POST", "/LiveStreams/MediaInfo", http.HandlerFunc(i.liveStreamsMediaInfo)); err != nil {
		return err
	}
	if err := i.router.Handle("POST", "/LiveStreams/Close", http.HandlerFunc(i.liveStreamsClose)); err != nil {
		return err
	}
	for _, method := range []string{"GET", "HEAD"} {
		if err := i.router.Handle(method, "/sessions/lazy/{path...}", http.HandlerFunc(i.lazyMedia)); err != nil {
			return err
		}
		if err := i.router.Handle(method, "/sessions/media/{path...}", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mediasafety.SetHeaders(w)
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
	AudioStreamIndex, SubtitleStreamIndex              *int
	PlaySessionId, LiveStreamId, ItemId, MediaSourceId string
	PositionTicks, RunTimeTicks                        json.Number
	IsPaused                                           bool
}

func (i *instance) playingPing(w http.ResponseWriter, r *http.Request) {
	v, _, err := i.viewer(r)
	if err != nil || v == nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	if i.host.Sessions == nil {
		http.Error(w, "Unavailable", http.StatusServiceUnavailable)
		return
	}
	var id string
	switch values := route.Query(r.URL.Query(), "PlaySessionId"); len(values) {
	case 1:
		id = values[0]
	case 0:
	default:
		http.Error(w, "Bad Request", http.StatusBadRequest)
		return
	}
	if id == "" && r.Body != nil && r.ContentLength != 0 {
		var body struct{ PlaySessionId string }
		if err := decode(r, &body); err != nil && !errors.Is(err, io.EOF) {
			http.Error(w, "Bad Request", http.StatusBadRequest)
			return
		}
		id = body.PlaySessionId
	}
	if id != "" {
		access := frontend.SessionAccess{Viewer: v, Device: device(r), Protocol: i.protocol}
		// The playback manager owns the timeout. A late ping is harmless.
		_ = i.host.Sessions.KeepAlive(r.Context(), access, id)
	}
	w.WriteHeader(http.StatusNoContent)
}

func (i *instance) timeline(w http.ResponseWriter, r *http.Request, stopped, started bool) {
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
	if r.Body != nil && r.ContentLength != 0 {
		if err := decode(r, &body); err != nil && !errors.Is(err, io.EOF) {
			http.Error(w, "Bad Request", 400)
			return
		}
	}
	if pathItem := r.PathValue("itemId"); pathItem != "" {
		body.ItemId = pathItem
	}
	if body.PlaySessionId == "" && body.LiveStreamId != "" {
		body.PlaySessionId = body.LiveStreamId
	}
	if i.catchUpTimeline(w, r, v, body, stopped) {
		return
	}
	ticks, err := timelineTicks(body.PositionTicks)
	if err != nil {
		http.Error(w, "Bad Request", 400)
		return
	}
	runtime, err := timelineTicks(body.RunTimeTicks)
	if err != nil {
		http.Error(w, "Bad Request", 400)
		return
	}
	access := frontend.SessionAccess{Viewer: v, Device: device(r), Protocol: i.protocol}
	if item, ok := timelineItem(body); ok || !stopped {
		i.noteAnonymousTimeline(r, v, access.Device, item, stopped)
	}
	state := "playing"
	if body.IsPaused {
		state = "paused"
	}
	if stopped {
		state = "stopped"
	}
	report := frontend.Timeline{PlaybackID: body.PlaySessionId, Position: time.Duration(ticks) * 100 * time.Nanosecond, Duration: time.Duration(runtime) * 100 * time.Nanosecond, State: state}
	if !started && !stopped {
		report.AudioStreamIndex = body.AudioStreamIndex
		report.SubtitleStreamIndex = body.SubtitleStreamIndex
	}
	recorded := false
	if body.PlaySessionId != "" {
		err = i.host.Sessions.Report(r.Context(), access, body.PlaySessionId, report)
		if err == nil {
			recorded = true
			if stopped {
				if err = i.host.Sessions.Stop(r.Context(), access, body.PlaySessionId); err != nil {
					http.Error(w, "Unavailable", 503)
					return
				}
			}
		} else if !errors.Is(err, frontend.ErrNotFound) {
			http.Error(w, "Unavailable", 503)
			return
		}
	}
	if !recorded {
		if timelineLiveItem(body) {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		item, ok := timelineItem(body)
		if !ok && (body.ItemId != "" || body.MediaSourceId != "") {
			// Version rows can report their scoped source id as the item.
			if scope, e := i.versionScope(r, v); e == nil {
				sourceID := body.ItemId
				if sourceID == "" {
					sourceID = body.MediaSourceId
				}
				item, _, _, e = i.resolveSource(r, scope, sourceID)
				ok = e == nil
			}
		}
		if !ok {
			http.Error(w, "Bad Request", 400)
			return
		}
		progress, ok := i.host.Sessions.(frontend.ItemProgress)
		if !ok {
			http.Error(w, "Unavailable", 503)
			return
		}
		if err = progress.ReportItemProgress(r.Context(), access, item, report); err != nil {
			if errors.Is(err, frontend.ErrNotFound) {
				http.NotFound(w, r)
				return
			}
			if errors.Is(err, frontend.ErrUnauthorized) {
				http.Error(w, "Unauthorized", 401)
				return
			}
			http.Error(w, "Unavailable", 503)
			return
		}
	}
	if started {
		i.creditPlay(r, v, body.ItemId)
	}
	w.WriteHeader(http.StatusNoContent)
}

func timelineLiveItem(body timelineRequest) bool {
	for _, value := range []string{body.ItemId, body.MediaSourceId} {
		if value == "" {
			continue
		}
		if _, err := ids.ParseGUID(ids.Channel, value); err == nil {
			return true
		}
		if _, err := ids.ParseGUID(ids.Programme, value); err == nil {
			return true
		}
	}
	return false
}

func timelineTicks(value json.Number) (int64, error) {
	if value == "" {
		return 0, nil
	}
	ticks, err := value.Int64()
	if err != nil || ticks < 0 || ticks > (1<<63-1)/100 {
		return 0, frontend.ErrNotAvailable
	}
	return ticks, nil
}

// timelineItem prefers the item id. A file-shaped MediaSourceId is the same
// rating-key GUID, which Infuse sends when it did not open a broker session.
func timelineItem(body timelineRequest) (frontend.RatingKey, bool) {
	for _, value := range []string{body.ItemId, body.MediaSourceId} {
		if value == "" {
			continue
		}
		key, err := ids.ParseRatingKeyGUID(value)
		if err == nil && key != 0 {
			return key, true
		}
	}
	return 0, false
}

func retryableMediaOpen(err error) bool {
	return errors.Is(err, frontend.ErrPreparing) || errors.Is(err, frontend.ErrPrepareSlow) || errors.Is(err, frontend.ErrDownloadQueued)
}

func noCompatibleStream(err error) bool {
	if retryableMediaOpen(err) || errors.Is(err, frontend.ErrJobsBusy) {
		return false
	}
	var noPath *frontend.NoCompatiblePathError
	return errors.As(err, &noPath) || errors.Is(err, frontend.ErrNotFound) || errors.Is(err, frontend.ErrNotAvailable) || errors.Is(err, frontend.ErrSourceGone)
}

func (i *instance) playbackInfoFailure(w http.ResponseWriter, r *http.Request, v *frontend.Viewer, stage string, err error) {
	dev := device(r)
	roku32 := isJellyfinRoku(dev) && (dev.Version == "" || strings.HasPrefix(dev.Version, "3.2."))
	if i.protocol == "jellyfin" && roku32 && noCompatibleStream(err) {
		// Roku 3.2.4's postJson parses any nonempty JSON body, then its
		// default-subtitle path indexes MediaSources[0]. An empty body
		// makes the task take its existing playback-info error branch.
		if i.host.Log != nil {
			i.host.Log.Warn("PlaybackInfo failed", "protocol", i.protocol, "stage", stage, "err", err)
		}
		w.Header().Set("X-Application-Error-Code", "NoCompatibleStream")
		w.WriteHeader(http.StatusUnprocessableEntity)
		return
	}
	i.playbackInfoError(w, v, stage, err, r.Header.Get("Accept-Language"))
}

// PlaybackInfo errors must be successful protocol responses: jellyfin-web
// reads ErrorCode only on a resolved request, and otherwise shows its generic
// network error. Admission failures retain 503 and Retry-After so clients
// can retry once capacity returns. Keep failure stages in server diagnostics.
func (i *instance) playbackInfoError(w http.ResponseWriter, v *frontend.Viewer, stage string, err error, languages ...string) {
	if i.host.Log != nil {
		i.host.Log.Warn("PlaybackInfo failed", "protocol", i.protocol, "stage", stage, "err", err)
	}
	if (i.protocol != "jellyfin" && !noCompatibleStream(err)) || errors.Is(err, frontend.ErrSessionBusy) || errors.Is(err, frontend.ErrTranscodeCapacity) || retryableMediaOpen(err) || errors.Is(err, frontend.ErrJobsBusy) {
		if errors.Is(err, frontend.ErrVideoTranscodingDisabled) {
			// Emby and the pass-through cases get the viewer's own wording too.
			http.Error(w, frontend.VideoTranscodingOffMessage(i.host.ViewerPolicy, v), http.StatusUnprocessableEntity)
			return
		}
		streamError(i.protocol, w, err, languages...)
		return
	}
	code := "NoCompatibleStream"
	if errors.Is(err, frontend.ErrUnauthorized) {
		code = "NotAllowed"
	} else if errors.Is(err, frontend.ErrStreamLimit) {
		code = "RateLimitExceeded"
	}
	body := map[string]any{"MediaSources": []any{}, "PlaySessionId": "", "ErrorCode": code}
	var bedtime *frontend.ErrBedtime
	var reqLang *frontend.RequiredAudioLanguageError
	if _, _, msg := frontend.PlaybackSourceProblem(err, languages...); msg != "" {
		body["Message"] = msg
	} else if errors.As(err, &bedtime) {
		body["Message"] = bedtime.Error()
	} else if errors.Is(err, frontend.ErrVideoTranscodingDisabled) {
		body["Message"] = frontend.VideoTranscodingOffMessage(i.host.ViewerPolicy, v)
	} else if errors.As(err, &reqLang) {
		body["Message"] = reqLang.Error()
	}
	jsonReply(w, body)
}

func (i *instance) writeNoCompatibleStream(w http.ResponseWriter) {
	jsonReply(w, map[string]any{"MediaSources": []any{}, "PlaySessionId": "", "ErrorCode": "NoCompatibleStream"})
}

func directFilePath(path string) bool {
	return strings.HasPrefix(path, "/sessions/media/") && strings.HasSuffix(path, "/file") && !strings.ContainsAny(path, "\r\n?#")
}

// streamAlias translates client-generated URLs into broker-owned, scoped URLs.
func (i *instance) streamAlias(w http.ResponseWriter, r *http.Request) {
	if themeParentID(r.PathValue("id")) != "" {
		name := strings.ToLower(r.PathValue("name"))
		if name != "stream" && name != "stream.m4a" && name != "stream.mp3" {
			http.NotFound(w, r)
			return
		}
		r.SetPathValue("id", themeParentID(r.PathValue("id")))
		i.themeStream(w, r)
		return
	}
	mediasafety.SetHeaders(w)
	name := strings.ToLower(r.PathValue("name"))
	if i.protocol == "emby" && queryBool(r, "Static", false) && (name == "stream" || strings.HasPrefix(name, "stream.")) {
		w = &embyStaticResponse{ResponseWriter: w}
	}
	viewer, credential, err := i.viewer(r)
	if err != nil || viewer == nil {
		// A stream alias with no credential at all is answered from a
		// session the viewer just opened from this peer (anonymousStreamKey
		// is set by the middleware). Anything else stays 401.
		if _, anonymous := r.Context().Value(anonymousStreamKey{}).(bool); anonymous && i.serveAnonymousStream(w, r, name) {
			return
		}
		http.Error(w, "Unauthorized", 401)
		return
	}
	if key, keyErr := ids.ParseRatingKeyGUID(r.PathValue("id")); keyErr == nil && key.Kind() == frontend.KindExtra {
		streamName := strings.ToLower(r.PathValue("name"))
		if streamName == "stream" || strings.HasPrefix(streamName, "stream.") {
			i.serveExtra(w, r, viewer, key)
			return
		}
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
			} else if staleItem, ok := i.resolveStaleSource(r, versionScope, r.PathValue("id")); ok && staleItem != 0 {
				key = staleItem
			}
		}
		if key == 0 && version == "" && detailSource == "" {
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
	format := frontend.StreamDirect
	switch {
	case name == "stream", strings.HasPrefix(name, "stream."), name == "universal":
	case name == "master.m3u8", name == "main.m3u8":
		format = frontend.StreamHLS
	default:
		http.NotFound(w, r)
		return
	}
	if channel == "" && key != 0 && key.Kind() != frontend.KindExtra {
		item, ok := i.authorizePlaybackItem(r, viewer, key)
		if !ok {
			http.NotFound(w, r)
			return
		}
		if name == "universal" && key.IsPluginKey() && mbIsBookItem(item) {
			http.NotFound(w, r)
			return
		}
	}
	vals := route.Query(r.URL.Query(), "PlaySessionId")
	if len(vals) > 1 {
		http.Error(w, "Bad Request", 400)
		return
	}
	if len(vals) == 0 || vals[0] == "" {
		liveVals := route.Query(r.URL.Query(), "LiveStreamId")
		if len(liveVals) > 1 {
			http.Error(w, "Bad Request", 400)
			return
		}
		if len(liveVals) == 1 && liveVals[0] != "" {
			vals = liveVals
		}
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
				target, kind, tag, e := i.resolveSourceFor(r, versionScope, key, sourceIDs[0])
				if e != nil || target != key {
					if staleItem, ok := i.resolveStaleSource(r, versionScope, sourceIDs[0]); ok && staleItem == key {
						// Stale MediaSourceId for this item and viewer: map to
						// the current source for (key, viewer) instead of 404.
					} else {
						http.NotFound(w, r)
						return
					}
				} else if kind == "detail" {
					detailSource, expectedETag = sourceIDs[0], tag
				} else {
					version = sourceIDs[0]
				}
			}
		}
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
	explicitSession := len(vals) == 1 && vals[0] != ""
	sessionID := ""
	for _, session := range sessions {
		if session.Item != key || session.Channel != channel || session.Protocol != i.protocol || session.Device.ID != access.Device.ID {
			continue
		}
		if explicitSession && session.ID != vals[0] {
			continue
		}
		if sessionID != "" {
			http.Error(w, "Ambiguous session", 409)
			return
		}
		sessionID = session.ID
	}
	if sessionID != "" && !explicitSession {
		// When a client omits PlaySessionId (e.g. Infuse Static=true) and sends
		// a verified handle whose source moved on during PlaybackInfo, map to
		// the current session or open the requested version instead of 404ing.
		if detailSource != "" {
			if planner, ok := i.host.Sessions.(frontend.DetailPlanner); ok {
				if errors.Is(planner.MatchDetailSession(i.identityContext(r), versionScope, detailSource, sessionID), frontend.ErrSourceGone) {
					detailSource = ""
				}
			}
		}
		if version != "" && i.host.VersionHandles != nil && channel == "" && format == frontend.StreamDirect && queryBool(r, "Static", false) {
			if i.host.VersionHandles.MatchSession(i.identityContext(r), versionScope, version, sessionID) != nil {
				sessionID = ""
			}
		}
	}
	// Infuse opens PlaybackInfo, then ignores PlaySessionId and reads
	// /Videos/{id}/stream?Static=true. Reuse the session PlaybackInfo
	// already started so Range hits the same file. Without a session,
	// open the source through the authenticated lazy broker.
	if sessionID == "" && channel == "" && format == frontend.StreamDirect && queryBool(r, "Static", false) && !explicitSession {
		lazy, ok := i.host.LazyStreams.(frontend.AuthenticatedLazyStreams)
		if !ok || i.host.Capabilities == nil {
			http.Error(w, "Unavailable", http.StatusServiceUnavailable)
			return
		}
		dev := access.Device
		capability, e := i.host.Capabilities.Build(i.identityContext(r), frontend.CapabilityRequest{Viewer: viewer, Device: caps.DeviceInfo{Protocol: i.protocol, ClientID: dev.ID, Product: dev.Client, DeviceName: dev.DeviceName, Version: dev.Version}, UserAgent: r.UserAgent()})
		if e != nil {
			http.Error(w, "Unavailable", http.StatusServiceUnavailable)
			return
		}
		request := frontend.PlanRequest{Viewer: viewer, Device: dev, Protocol: i.protocol, Item: key, Version: version, DetailSource: detailSource, ExpectedSourceETag: expectedETag, Caps: capability.Client, Intent: frontend.PlayIntent{ExactVersion: version != "", Options: frontend.DecideOptions{DirectPlay: true, DirectStream: true}}}
		lazy.ServeAuthenticated(w, r, credential, request)
		return
	}
	if sessionID == "" && channel == "" && name == "universal" {
		i.serveUniversalAudio(w, r, viewer, access, key, version, detailSource, expectedETag)
		return
	}
	if sessionID == "" {
		http.NotFound(w, r)
		return
	}
	i.serveAliasSession(w, r, access, sessions, sessionID, versionScope, version, detailSource, format, explicitSession)
}

// Both MediaBrowser dialects resolve tune failures with a native playback code.
func (i *instance) liveTuneError(w http.ResponseWriter, err error) {
	detail := frontend.DiagnoseLiveTV("tune", err)
	code := "NoCompatibleStream"
	if errors.Is(err, frontend.ErrUnauthorized) {
		code = "NotAllowed"
	} else if errors.Is(err, frontend.ErrStreamLimit) {
		code = "RateLimitExceeded"
	}
	w.Header().Set("X-Ether-LiveTV-Error", detail.Code)
	jsonReply(w, map[string]any{"MediaSources": []any{}, "PlaySessionId": "", "ErrorCode": code, "Message": detail.Message, "LiveTVErrorCode": detail.Code})
}

// subtitleCodecAliases groups the names a declared subtitle profile and a
// probed stream use for the same format.
var subtitleCodecAliases = [][]string{
	{"subrip", "srt"},
	{"webvtt", "vtt"},
	{"ass", "ssa"},
	{"mov_text", "tx3g"},
	{"hdmv_pgs_subtitle", "pgssub", "pgs"},
	{"dvd_subtitle", "dvdsub", "vobsub"},
	{"dvb_subtitle", "dvbsub"},
	{"eia_608", "cea_608"},
}

// embedsSubtitle reports whether a declared device profile renders this
// subtitle codec from the media file itself (Method "Embed").
func embedsSubtitle(profiles []caps.SubtitleProfile, codec string) bool {
	codec = strings.ToLower(strings.TrimSpace(codec))
	if codec == "" {
		return false
	}
	names := []string{codec}
	for _, group := range subtitleCodecAliases {
		if slices.Contains(group, codec) {
			names = group
			break
		}
	}
	for _, p := range profiles {
		if !strings.EqualFold(p.Method, "embed") {
			continue
		}
		for _, format := range strings.Split(strings.ToLower(p.Format), ",") {
			if slices.Contains(names, strings.TrimSpace(format)) {
				return true
			}
		}
	}
	return false
}
