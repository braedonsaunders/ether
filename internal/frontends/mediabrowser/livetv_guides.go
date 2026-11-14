package mediabrowser

import (
	"net/http"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/kit/route"
)

func liveProgrammeMatches(r *http.Request, p frontend.LiveProgramme) bool {
	// Category selections are ORed, matching Jellyfin's Guide category picker.
	selected, matched := false, false
	for _, c := range []struct{ query, category string }{{"IsMovie", "movie"}, {"IsSeries", "series"}, {"IsSports", "sports"}, {"IsNews", "news"}, {"IsKids", "kids"}} {
		values := route.Query(r.URL.Query(), c.query)
		if len(values) > 0 && values[0] == "false" && frontend.LiveCategory(p.Genres, c.category, p.MediaType) {
			return false
		}
		if len(values) > 0 && values[0] == "true" {
			selected = true
			matched = matched || frontend.LiveCategory(p.Genres, c.category, p.MediaType)
		}
	}
	return !selected || matched
}
func (i *instance) filterLiveChannels(r *http.Request, v *frontend.Viewer, cs []frontend.LiveChannel) ([]frontend.LiveChannel, error) {
	tags := route.Query(r.URL.Query(), "Tags")
	filtered := []frontend.LiveChannel{}
	for _, c := range cs {
		ok := len(tags) == 0 || i.protocol != "emby"
		for _, raw := range tags {
			for _, tag := range strings.Split(raw, "|") {
				for _, g := range c.Guides {
					if strings.EqualFold(g.Name, tag) {
						ok = true
					}
				}
			}
		}
		if ok {
			filtered = append(filtered, c)
		}
	}
	selected := false
	for _, name := range []string{"IsMovie", "IsSeries", "IsSports", "IsNews", "IsKids"} {
		for _, v := range route.Query(r.URL.Query(), name) {
			selected = selected || v == "true" || v == "false"
		}
	}
	if !selected || len(filtered) == 0 {
		return filtered, nil
	}
	ids := make([]string, 0, len(filtered))
	for _, c := range filtered {
		ids = append(ids, c.ID)
	}
	now := time.Now().UTC()
	ps, err := i.host.LiveTV.Guide(r.Context(), v, frontend.LiveGuideQuery{Channels: ids, From: now, Until: now.Add(2 * time.Hour)})
	if err != nil {
		return nil, err
	}
	found := map[string]bool{}
	for _, p := range ps {
		if liveProgrammeMatches(r, p) {
			found[p.ChannelID] = true
		}
	}
	out := []frontend.LiveChannel{}
	for _, c := range filtered {
		if found[c.ID] {
			out = append(out, c)
		}
	}
	return out, nil
}
func (i *instance) channelTags(w http.ResponseWriter, r *http.Request) {
	viewer, _, err := i.viewer(r)
	if err != nil || viewer == nil {
		http.Error(w, "Unauthorized", 401)
		return
	}
	if i.protocol != "emby" {
		http.NotFound(w, r)
		return
	}
	tags := []any{}
	if i.host.LiveTV != nil {
		cs, err := i.host.LiveTV.Channels(r.Context(), viewer)
		if err != nil {
			http.Error(w, "Unavailable", 503)
			return
		}
		seen := map[string]bool{}
		for _, c := range cs {
			for _, g := range c.Guides {
				if !seen[g.Name] {
					tags = append(tags, map[string]any{"Name": g.Name, "Id": g.ID})
					seen[g.Name] = true
				}
			}
		}
	}
	jsonReply(w, i.queryResult(tags, len(tags), 0))
}
