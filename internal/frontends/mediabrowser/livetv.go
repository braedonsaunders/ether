package mediabrowser

import (
	"github.com/braedonsaunders/ether/internal/contentrating"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/kit/ids"
	"github.com/braedonsaunders/ether/internal/frontend/kit/paging"
	"github.com/braedonsaunders/ether/internal/frontend/kit/route"
	"net/http"
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
	item := map[string]any{"Id": id, "Name": ch.Name, "Number": ch.Number, "Type": "TvChannel", "ChannelType": "TV", "MediaType": "Video", "IsFolder": false, "UserData": map[string]any{"Key": id, "IsFavorite": ch.Favorite}}
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
		id, e := ids.StringGUID(ids.Programme, p.ID)
		if e != nil {
			continue
		}
		channel, e := ids.StringGUID(ids.Channel, p.ChannelID)
		if e != nil {
			continue
		}
		item := map[string]any{"Id": id, "ChannelId": channel, "Name": p.Title, "Overview": p.Summary, "Type": "Program", "StartDate": wireTime(p.Start), "EndDate": wireTime(p.End), "OfficialRating": contentrating.Prefixed(p.Rating), "IndexNumber": p.Episode, "ParentIndexNumber": p.Season, "IsMovie": false, "IsSeries": p.Episode > 0 || p.Season > 0}
		if ch, ok := names[p.ChannelID]; ok {
			item["ChannelName"] = ch.Name
			item["ChannelNumber"] = ch.Number
		}
		items = append(items, item)
	}
	jsonReply(w, i.queryResult(items, total, offset))
}
