package mediabrowser

import (
	"github.com/braedonsaunders/ether/internal/contentrating"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/kit/authhdr"
	"github.com/braedonsaunders/ether/internal/frontend/kit/ids"
	"github.com/braedonsaunders/ether/internal/frontend/kit/paging"
	"github.com/braedonsaunders/ether/internal/frontend/kit/route"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

// liveTVViewNumber is the stable library id for the Live TV user view.
// Library sections are 1 (movies) and 2 (shows). This id is not a catalogue
// section; clients open it as CollectionType livetv.
const liveTVViewNumber int64 = 3

func (i *instance) liveTVEnabled() bool { return i != nil && i.host.LiveTV != nil }

func (i *instance) liveTVInfo(w http.ResponseWriter, r *http.Request) {
	viewer, _, err := i.viewer(r)
	if err != nil || viewer == nil {
		http.Error(w, "Unauthorized", 401)
		return
	}
	enabled := i.liveTVEnabled()
	tuners := []any{}
	if enabled {
		tuners = []any{"Ether"}
	}
	// Jellyfin always returns a service list. Emby's disabled server omits it;
	// an enabled tuner still uses the same service object so both clients can
	// see that live TV is on.
	body := map[string]any{"IsEnabled": enabled, "EnabledUsers": []any{}}
	if i.protocol == "jellyfin" || enabled {
		body["Services"] = []any{map[string]any{"Name": "Ether", "Status": "Ok", "HasUpdateAvailable": false, "IsVisible": enabled, "Tuners": tuners}}
	}
	jsonReply(w, body)
}

func (i *instance) channelDTO(r *http.Request, token string, ch frontend.LiveChannel) (map[string]any, bool) {
	id, err := ids.StringGUID(ids.Channel, ch.ID)
	if err != nil {
		return nil, false
	}
	item := map[string]any{
		"Id": id, "Name": ch.Name, "Number": ch.Number, "Type": "TvChannel",
		"ChannelType": "TV", "MediaType": "Video", "IsFolder": false,
		"MediaSources": []any{i.liveChannelMediaSourceDTO(id, ch.Name)},
		"UserData":     map[string]any{"Key": id, "IsFavorite": ch.Favorite},
	}
	if i.host.ImageGrants != nil {
		for _, img := range ch.Images {
			if img.Kind != frontend.ImageChannel {
				continue
			}
			grant, err := i.host.ImageGrants.Issue(r.Context(), frontend.ImageGrantRequest{Credential: token, Device: device(r), Channel: ch.ID, Image: img})
			if err == nil && grant != "" {
				item["ImageTags"] = map[string]string{"Primary": grant}
				break
			}
		}
	}
	return item, true
}

func (i *instance) liveChannelMediaSourceDTO(id, name string) map[string]any {
	media := frontend.MediaInfo{Container: "ts", Estimated: true}
	source := i.detailMediaSourceDTO(id, media)
	i.shapeLiveMediaSource(source, id, name, "", "", media, false)
	i.mediaSourceDialect(source)
	if i.protocol == "emby" {
		i.embyMediaSourceDTO(source, id, media)
	}
	if i.protocol == "jellyfin" {
		i.jellyfinMediaSourceDTO(source, media, true)
	}
	return source
}

func (i *instance) shapeLiveMediaSource(source map[string]any, id, name, liveStreamID, transcodingURL string, media frontend.MediaInfo, opened bool) {
	for _, field := range []string{"RunTimeTicks", "Size", "Chapters"} {
		delete(source, field)
	}
	source["Id"] = id
	if name != "" {
		source["Name"] = name
	}
	source["Container"] = "ts"
	source["Protocol"] = "Http"
	source["Type"] = "Default"
	source["IsRemote"] = false
	source["IsInfiniteStream"] = true
	source["ReadAtNativeFramerate"] = true
	source["IgnoreDts"] = true
	source["IgnoreIndex"] = false
	source["GenPtsInput"] = false
	source["SupportsDirectPlay"] = false
	source["SupportsDirectStream"] = false
	source["SupportsTranscoding"] = true
	source["SupportsProbing"] = true
	source["RequiresOpening"] = true
	source["RequiresClosing"] = true
	source["RequiresLooping"] = false
	source["OpenToken"] = id
	source["TranscodingSubProtocol"] = "hls"
	source["TranscodingContainer"] = "ts"
	if opened {
		source["LiveStreamId"] = liveStreamID
		source["TranscodingUrl"] = transcodingURL
		source["MediaStreams"] = i.mediaStreams(media.Streams)
	}
}

func defaultLiveMediaInfo(in frontend.MediaInfo) frontend.MediaInfo {
	out := in
	out.Container = "ts"
	out.Duration = 0
	out.Size = 0
	out.Chapters = nil
	out.Estimated = false
	if len(out.Streams) == 0 {
		no := false
		eight := 8
		out.Streams = []frontend.StreamInfo{
			{Index: 0, Type: "video", Codec: "h264", Width: 1920, Height: 1080, BitDepth: 8, BitsPerRawSample: &eight, PixelFormat: "yuv420p", ColorTransfer: "bt709", ColorSpace: "bt709", ColorPrimaries: "bt709", SampleAspectRatio: "1:1", DisplayAspectRatio: "16:9", Default: true, DolbyVisionConfigurationPresent: &no, HDR10Plus: &no},
			{Index: 1, Type: "audio", Codec: "aac", Channels: 2, SampleRate: 48000, ChannelLayout: "stereo", Default: true},
		}
	}
	return out
}

func (i *instance) channels(w http.ResponseWriter, r *http.Request) {
	viewer, token, err := i.viewer(r)
	if err != nil || viewer == nil {
		http.Error(w, "Unauthorized", 401)
		return
	}
	if i.host.LiveTV == nil {
		http.Error(w, "Unavailable", 503)
		return
	}
	channels, err := i.host.LiveTV.Channels(r.Context(), viewer)
	if err != nil {
		http.Error(w, "Unavailable", 503)
		return
	}
	offset := queryInt(r, "StartIndex", 0)
	limit := queryInt(r, "Limit", 50)
	items := make([]any, 0)
	for _, ch := range paging.Slice(channels, offset, limit) {
		item, ok := i.channelDTO(r, token, ch)
		if !ok {
			continue
		}
		items = append(items, item)
	}
	jsonReply(w, i.queryResult(items, len(channels), offset))
}

// programmeLookupQuery is the guide window a single program id is resolved
// against. On Now and the surrounding guide fit; the facade rejects a longer span.
func programmeLookupQuery(now time.Time) frontend.LiveGuideQuery {
	return frontend.LiveGuideQuery{From: now.Add(-12 * time.Hour), Until: now.Add(12 * time.Hour)}
}

// liveChannelTypesOnly is Swiftfin's Channels library: IncludeItemTypes is
// only a live channel. That request must not become a catalogue library walk.
func liveChannelTypesOnly(r *http.Request) bool {
	values := route.Query(r.URL.Query(), "IncludeItemTypes")
	if len(values) == 0 {
		return false
	}
	saw := false
	for _, value := range values {
		for _, typ := range strings.Split(value, ",") {
			switch strings.ToLower(strings.TrimSpace(typ)) {
			case "":
				continue
			case "livetvchannel", "tvchannel", "channel":
				saw = true
			default:
				return false
			}
		}
	}
	return saw
}

func (i *instance) writeLiveItemsByID(w http.ResponseWriter, r *http.Request, viewer *frontend.Viewer) bool {
	values := route.Query(r.URL.Query(), "Ids")
	if len(values) == 0 {
		return false
	}
	var idsList []string
	for _, value := range values {
		for _, id := range strings.Split(value, ",") {
			id = strings.TrimSpace(id)
			if id == "" {
				continue
			}
			_, chErr := ids.ParseGUID(ids.Channel, id)
			_, prErr := ids.ParseGUID(ids.Programme, id)
			if chErr != nil && prErr != nil {
				return false
			}
			idsList = append(idsList, id)
		}
	}
	if len(idsList) == 0 {
		return false
	}
	if i.host.LiveTV == nil {
		i.replyItems(w, r, i.queryResult([]any{}, 0, queryInt(r, "StartIndex", 0)))
		return true
	}
	token := authhdr.MediaBrowserToken(r)
	var allChannels []frontend.LiveChannel
	items := make([]any, 0, len(idsList))
	for _, wireID := range idsList {
		if _, err := ids.ParseGUID(ids.Channel, wireID); err == nil {
			ch, err := i.host.LiveTV.ResolveChannelGUID(r.Context(), viewer, wireID)
			if err != nil || ch.ID == "" {
				continue
			}
			if item, ok := i.channelDTO(r, token, ch); ok {
				items = append(items, item)
			}
			continue
		}
		p, err := i.host.LiveTV.ResolveProgrammeGUID(r.Context(), viewer, wireID, programmeLookupQuery(time.Now().UTC()))
		if err != nil || p.ID == "" || p.ChannelID == "" {
			continue
		}
		if allChannels == nil {
			allChannels, _ = i.host.LiveTV.Channels(r.Context(), viewer)
		}
		var channel frontend.LiveChannel
		for _, ch := range allChannels {
			if ch.ID == p.ChannelID {
				channel = ch
				break
			}
		}
		if item, ok := i.programmeDTO(p, channel); ok {
			items = append(items, item)
		}
	}
	offset := queryInt(r, "StartIndex", 0)
	limit := queryInt(r, "Limit", 50)
	i.replyItems(w, r, i.queryResult(paging.Slice(items, offset, limit), len(items), offset))
	return true
}

func (i *instance) writeLiveChannelItems(w http.ResponseWriter, r *http.Request, viewer *frontend.Viewer) {
	if i.host.LiveTV == nil {
		i.replyItems(w, r, i.queryResult([]any{}, 0, queryInt(r, "StartIndex", 0)))
		return
	}
	channels, err := i.host.LiveTV.Channels(r.Context(), viewer)
	if err != nil {
		http.Error(w, "Unavailable", 503)
		return
	}
	q := itemQuery(r)
	sort.SliceStable(channels, func(a, b int) bool {
		if q.Desc {
			return channels[a].Name > channels[b].Name
		}
		return channels[a].Name < channels[b].Name
	})
	token := authhdr.MediaBrowserToken(r)
	offset := q.Offset
	limit := q.Limit
	if limit <= 0 {
		limit = 50
	}
	items := make([]any, 0)
	for _, ch := range paging.Slice(channels, offset, limit) {
		item, ok := i.channelDTO(r, token, ch)
		if !ok {
			continue
		}
		items = append(items, item)
	}
	i.replyItems(w, r, i.queryResult(items, len(channels), offset))
}

func (i *instance) recordings(w http.ResponseWriter, r *http.Request) {
	viewer, _, err := i.viewer(r)
	if err != nil || viewer == nil {
		http.Error(w, "Unauthorized", 401)
		return
	}
	// Ether has no DVR. An empty page is the Jellyfin recordings list, so
	// Swiftfin's recordings row is empty instead of an unmatched 404.
	i.replyItems(w, r, i.queryResult([]any{}, 0, queryInt(r, "StartIndex", 0)))
}

func (i *instance) programmeDTO(p frontend.LiveProgramme, channel frontend.LiveChannel) (map[string]any, bool) {
	id, err := ids.StringGUID(ids.Programme, p.ID)
	if err != nil {
		return nil, false
	}
	channelID, err := ids.StringGUID(ids.Channel, p.ChannelID)
	if err != nil {
		return nil, false
	}
	item := map[string]any{
		"Id": id, "ChannelId": channelID, "Name": p.Title, "Overview": p.Summary, "Type": "Program",
		"StartDate": wireTime(p.Start), "EndDate": wireTime(p.End), "OfficialRating": contentrating.Prefixed(p.Rating),
		"IndexNumber": p.Episode, "ParentIndexNumber": p.Season, "IsMovie": false, "IsSeries": p.Episode > 0 || p.Season > 0,
		"MediaType": "Video", "IsFolder": false, "UserData": map[string]any{"Key": id, "PlaybackPositionTicks": 0, "PlayCount": 0, "IsFavorite": false, "Played": false},
	}
	if channel.ID != "" {
		item["ChannelName"] = channel.Name
		item["ChannelNumber"] = channel.Number
	}
	return item, true
}

// writeLiveGuideItem serves a channel or program GUID as one item. A normal
// catalogue id returns false so detail can keep resolving versions.
func (i *instance) writeLiveGuideItem(w http.ResponseWriter, r *http.Request, viewer *frontend.Viewer, token, wireID string) bool {
	if _, err := ids.ParseGUID(ids.Channel, wireID); err == nil {
		if i.host.LiveTV == nil {
			http.NotFound(w, r)
			return true
		}
		ch, err := i.host.LiveTV.ResolveChannelGUID(r.Context(), viewer, wireID)
		if err != nil || ch.ID == "" {
			http.NotFound(w, r)
			return true
		}
		item, ok := i.channelDTO(r, token, ch)
		if !ok {
			http.NotFound(w, r)
			return true
		}
		i.replyItems(w, r, item)
		return true
	}
	if _, err := ids.ParseGUID(ids.Programme, wireID); err != nil {
		return false
	}
	if i.host.LiveTV == nil {
		http.NotFound(w, r)
		return true
	}
	p, err := i.host.LiveTV.ResolveProgrammeGUID(r.Context(), viewer, wireID, programmeLookupQuery(time.Now().UTC()))
	if err != nil || p.ID == "" || p.ChannelID == "" {
		http.NotFound(w, r)
		return true
	}
	var channel frontend.LiveChannel
	if channels, e := i.host.LiveTV.Channels(r.Context(), viewer); e == nil {
		for _, ch := range channels {
			if ch.ID == p.ChannelID {
				channel = ch
				break
			}
		}
	}
	item, ok := i.programmeDTO(p, channel)
	if !ok {
		http.NotFound(w, r)
		return true
	}
	i.replyItems(w, r, item)
	return true
}

func (i *instance) programme(w http.ResponseWriter, r *http.Request) {
	viewer, token, err := i.viewer(r)
	if err != nil || viewer == nil {
		http.Error(w, "Unauthorized", 401)
		return
	}
	if !i.writeLiveGuideItem(w, r, viewer, token, r.PathValue("id")) {
		http.NotFound(w, r)
	}
}

func (i *instance) channel(w http.ResponseWriter, r *http.Request) {
	viewer, token, err := i.viewer(r)
	if err != nil || viewer == nil {
		http.Error(w, "Unauthorized", 401)
		return
	}
	if i.host.LiveTV == nil {
		http.Error(w, "Unavailable", 503)
		return
	}
	ch, err := i.host.LiveTV.ResolveChannelGUID(r.Context(), viewer, r.PathValue("id"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	item, ok := i.channelDTO(r, token, ch)
	if !ok {
		http.NotFound(w, r)
		return
	}
	jsonReply(w, item)
}
func (i *instance) programmes(w http.ResponseWriter, r *http.Request) {
	viewer, _, err := i.viewer(r)
	if err != nil || viewer == nil {
		http.Error(w, "Unauthorized", 401)
		return
	}
	if i.host.LiveTV == nil {
		http.Error(w, "Unavailable", 503)
		return
	}
	now := time.Now().UTC()
	q := frontend.LiveGuideQuery{From: now, Until: now.Add(24 * time.Hour), Limit: queryInt(r, "Limit", 100), Device: device(r)}
	for _, p := range []struct {
		name string
		dest *time.Time
	}{{"MinStartDate", &q.From}, {"MaxStartDate", &q.Until}} {
		values := route.Query(r.URL.Query(), p.name)
		if len(values) > 1 {
			http.Error(w, "Bad Request", 400)
			return
		}
		if len(values) == 1 {
			t, e := time.Parse(time.RFC3339, values[0])
			if e != nil {
				http.Error(w, "Bad Request", 400)
				return
			}
			*p.dest = t
		}
	}
	for _, value := range route.Query(r.URL.Query(), "ChannelIds") {
		for _, id := range strings.Split(value, ",") {
			ch, e := i.host.LiveTV.ResolveChannelGUID(r.Context(), viewer, id)
			if e != nil {
				http.NotFound(w, r)
				return
			}
			q.Channels = append(q.Channels, ch.ID)
		}
	}
	rows, err := i.host.LiveTV.Guide(r.Context(), viewer, q)
	if err != nil {
		http.Error(w, "Unavailable", 503)
		return
	}
	i.writeProgrammes(w, r, viewer, rows, queryInt(r, "StartIndex", 0), 0)
}

func (i *instance) recommendedProgrammes(w http.ResponseWriter, r *http.Request) {
	viewer, _, err := i.viewer(r)
	if err != nil || viewer == nil {
		http.Error(w, "Unauthorized", 401)
		return
	}
	if i.host.LiveTV == nil {
		http.Error(w, "Unavailable", 503)
		return
	}
	now := time.Now().UTC()
	q := frontend.LiveGuideQuery{From: now.Add(-12 * time.Hour), Until: now.Add(12 * time.Hour), Limit: 0, Device: device(r)}
	airing := true
	if values := route.Query(r.URL.Query(), "IsAiring"); len(values) == 1 {
		airing, err = strconv.ParseBool(values[0])
		if err != nil {
			http.Error(w, "Bad Request", 400)
			return
		}
	}
	if !airing {
		q.From, q.Until = now, now.Add(24*time.Hour)
	}
	rows, err := i.host.LiveTV.Guide(r.Context(), viewer, q)
	if err != nil {
		http.Error(w, "Unavailable", 503)
		return
	}
	if airing {
		current := rows[:0]
		for _, p := range rows {
			if !p.Start.After(now) && p.End.After(now) {
				current = append(current, p)
			}
		}
		rows = current
	}
	offset := queryInt(r, "StartIndex", 0)
	limit := queryInt(r, "Limit", 100)
	i.writeProgrammes(w, r, viewer, rows, offset, limit)
}

func (i *instance) writeProgrammes(w http.ResponseWriter, r *http.Request, viewer *frontend.Viewer, rows []frontend.LiveProgramme, offset, limit int) {
	names := map[string]frontend.LiveChannel{}
	if i.host.LiveTV != nil && viewer != nil {
		if channels, err := i.host.LiveTV.Channels(r.Context(), viewer); err == nil {
			for _, ch := range channels {
				names[ch.ID] = ch
			}
		}
	}
	total := len(rows)
	if limit > 0 {
		rows = paging.Slice(rows, offset, limit)
	}
	items := make([]any, 0, len(rows))
	for _, p := range rows {
		item, ok := i.programmeDTO(p, names[p.ChannelID])
		if !ok {
			continue
		}
		items = append(items, item)
	}
	jsonReply(w, i.queryResult(items, total, offset))
}
