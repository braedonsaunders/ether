package vela

import (
	"github.com/braedonsaunders/ether/internal/frontend"
	"net/http"
	"strings"
	"time"
)

func (i *instance) channels(w http.ResponseWriter, r *http.Request) {
	v, _, e := i.viewer(r)
	if e != nil {
		fail(w, e)
		return
	}
	if i.host.LiveTV == nil {
		i.unavailable(w, r)
		return
	}
	if !noCursor(w, r) {
		return
	}
	channels, e := i.host.LiveTV.Channels(r.Context(), v)
	if e != nil {
		fail(w, e)
		return
	}
	out := []object{}
	for _, c := range channels[:min(len(channels), limit(r))] {
		out = append(out, object{"id": c.ID, "number": c.Number, "name": c.Name, "kind": c.Kind, "artwork": []object{}, "favorite": c.Favorite, "rating": c.Rating})
	}
	continuation := "exhausted"
	if len(channels) > len(out) {
		continuation = "unknown"
	}
	cache(w, r, object{"items": out, "continuation": continuation})
}
func (i *instance) guide(w http.ResponseWriter, r *http.Request) {
	v, _, e := i.viewer(r)
	if e != nil {
		fail(w, e)
		return
	}
	if i.host.LiveTV == nil {
		i.unavailable(w, r)
		return
	}
	if !noCursor(w, r) {
		return
	}
	q := r.URL.Query()
	from, e1 := time.Parse(time.RFC3339, q.Get("from"))
	until, e2 := time.Parse(time.RFC3339, q.Get("until"))
	if e1 != nil || e2 != nil || !until.After(from) || until.Sub(from) > 24*time.Hour {
		problem(w, 400, "request", "invalid_guide_window")
		return
	}
	channels := []string{}
	if s := q.Get("channelIds"); s != "" {
		channels = strings.Split(s, ",")
	}
	if len(channels) > 100 {
		problem(w, 400, "request", "too_many_channels")
		return
	}
	programmes, e := i.host.LiveTV.Guide(r.Context(), v, frontend.LiveGuideQuery{Channels: channels, From: from, Until: until, Limit: limit(r) + 1, Device: requestDevice(r)})
	if e != nil {
		fail(w, e)
		return
	}
	out := []object{}
	for _, p := range programmes[:min(len(programmes), limit(r))] {
		out = append(out, object{"id": p.ID, "channelId": p.ChannelID, "name": p.Title, "summary": p.Summary, "start": p.Start.UTC().Format(time.RFC3339), "end": p.End.UTC().Format(time.RFC3339), "episodeTitle": p.EpisodeTitle, "seasonNumber": p.Season, "episodeNumber": p.Episode, "artwork": []object{}, "rating": p.Rating})
	}
	continuation := "exhausted"
	if len(programmes) > len(out) {
		continuation = "unknown"
	}
	cache(w, r, object{"items": out, "from": from.UTC().Format(time.RFC3339), "until": until.UTC().Format(time.RFC3339), "continuation": continuation})
}
