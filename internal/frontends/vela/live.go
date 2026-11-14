package vela

import (
	"github.com/braedonsaunders/ether/internal/frontend"
	"net/http"
	"strings"
	"time"
)

func (i *instance) channels(w http.ResponseWriter, r *http.Request) {
	v, token, e := i.viewer(r)
	if e != nil {
		fail(w, e)
		return
	}
	if i.host.LiveTV == nil {
		i.unavailable(w, r)
		return
	}
	scope, e := i.livePageScope(r, v, token)
	if e != nil {
		fail(w, e)
		return
	}
	cursor, e := i.readLiveCursor(r, scope, hashJSON([]any{"channels", limit(r)}))
	if e != nil {
		fail(w, e)
		return
	}
	channels, e := i.host.LiveTV.Channels(r.Context(), v)
	if e != nil {
		fail(w, e)
		return
	}
	out := []object{}
	snapshot := hashJSON(channels)
	if cursor.Snapshot != "" && cursor.Snapshot != snapshot {
		fail(w, frontend.ErrLiveGuideStale)
		return
	}
	start := min(cursor.Offset, len(channels))
	end := start + min(limit(r), len(channels)-start)
	for _, c := range channels[start:end] {
		out = append(out, object{"id": c.ID, "number": c.Number, "name": c.Name, "kind": c.Kind, "artwork": []object{}, "favorite": c.Favorite, "rating": c.Rating})
	}
	page := object{"items": out, "continuation": "exhausted"}
	if end < len(channels) {
		next, err := i.nextLiveCursor(cursor, end, snapshot)
		if err != nil {
			fail(w, err)
			return
		}
		page["continuation"], page["nextCursor"] = "more", next
	}
	cache(w, r, page)
}
func (i *instance) guide(w http.ResponseWriter, r *http.Request) {
	v, token, e := i.viewer(r)
	if e != nil {
		fail(w, e)
		return
	}
	if i.host.LiveTV == nil {
		i.unavailable(w, r)
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
	scope, e := i.livePageScope(r, v, token)
	if e != nil {
		fail(w, e)
		return
	}
	cursor, e := i.readLiveCursor(r, scope, hashJSON([]any{"guide", from.UTC(), until.UTC(), channels, limit(r)}))
	if e != nil {
		fail(w, e)
		return
	}
	pager, ok := i.host.LiveTV.(frontend.LiveGuidePager)
	if !ok {
		i.unavailable(w, r)
		return
	}
	programmes, e := pager.GuidePage(i.host.Experience.NoProviderWait(r.Context()), v, frontend.LiveGuidePageQuery{LiveGuideQuery: frontend.LiveGuideQuery{Channels: channels, From: from, Until: until, Limit: limit(r), Device: requestDevice(r)}, Offset: cursor.Offset, Snapshot: cursor.Snapshot})
	if e != nil {
		fail(w, e)
		return
	}
	out := []object{}
	for _, p := range programmes.Items {
		out = append(out, object{"id": p.ID, "channelId": p.ChannelID, "name": p.Title, "summary": p.Summary, "start": p.Start.UTC().Format(time.RFC3339), "end": p.End.UTC().Format(time.RFC3339), "episodeTitle": p.EpisodeTitle, "seasonNumber": p.Season, "episodeNumber": p.Episode, "artwork": []object{}, "rating": p.Rating})
	}
	page := object{"items": out, "from": from.UTC().Format(time.RFC3339), "until": until.UTC().Format(time.RFC3339), "continuation": "exhausted"}
	if programmes.More {
		if len(out) == 0 || programmes.Snapshot == "" {
			fail(w, frontend.ErrLiveGuideStale)
			return
		}
		next, err := i.nextLiveCursor(cursor, cursor.Offset+len(out), programmes.Snapshot)
		if err != nil {
			fail(w, err)
			return
		}
		page["continuation"], page["nextCursor"] = "more", next
	}
	cache(w, r, page)
}
