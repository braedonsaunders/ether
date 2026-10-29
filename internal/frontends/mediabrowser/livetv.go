package mediabrowser

import (
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/kit/ids"
	"github.com/braedonsaunders/ether/internal/frontend/kit/paging"
	"github.com/braedonsaunders/ether/internal/frontend/kit/route"
	"net/http"
	"strings"
	"time"
)

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
		id, e := ids.StringGUID(ids.Channel, ch.ID)
		if e != nil {
			continue
		}
		item := map[string]any{"Id": id, "Name": ch.Name, "Number": ch.Number, "Type": "TvChannel", "IsFolder": false, "UserData": map[string]any{"Key": id, "IsFavorite": ch.Favorite}}
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
		items = append(items, item)
	}
	jsonReply(w, i.queryResult(items, len(channels), offset))
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
		items = append(items, map[string]any{"Id": id, "ChannelId": channel, "Name": p.Title, "Overview": p.Summary, "Type": "Program", "StartDate": p.Start, "EndDate": p.End, "OfficialRating": p.Rating, "IndexNumber": p.Episode, "ParentIndexNumber": p.Season})
	}
	jsonReply(w, i.queryResult(items, len(items), 0))
}
