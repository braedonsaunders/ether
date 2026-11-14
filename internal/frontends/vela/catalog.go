package vela

import (
	"encoding/json"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/kit/ids"
	"net/http"
	"strconv"
	"strings"
	"time"
)

func hashJSON(v any) string { b, _ := json.Marshal(v); return hash(string(b)) }
func limit(r *http.Request) int {
	n, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if n <= 0 {
		n = 50
	}
	if n > 100 {
		n = 100
	}
	return n
}
func noCursor(w http.ResponseWriter, r *http.Request) bool {
	if r.URL.Query().Get("cursor") != "" {
		problem(w, 409, "conflict", "cursor_expired")
		return false
	}
	return true
}
func key(r *http.Request) (frontend.RatingKey, error) {
	return ids.ParseRatingKeyGUID(r.PathValue("id"))
}
func titleWire(t frontend.Title) object {
	kind := "movie"
	switch t.Key.Kind() {
	case frontend.KindShow:
		kind = "show"
	case frontend.KindSeason:
		kind = "season"
	case frontend.KindEpisode:
		kind = "episode"
	}
	genres := t.Genres
	if genres == nil {
		genres = []string{}
	}
	out := object{"id": ids.RatingKeyGUID(t.Key), "kind": kind, "name": t.Title, "summary": t.Summary, "genres": genres, "artwork": []object{}}
	if t.Year > 0 {
		out["year"] = t.Year
	}
	if t.Runtime > 0 {
		out["durationSeconds"] = t.Runtime.Seconds()
	}
	if t.ContentRating != "" {
		out["rating"] = t.ContentRating
	}
	if t.ShowKey != 0 {
		out["showId"] = ids.RatingKeyGUID(t.ShowKey)
		out["seasonNumber"] = t.Season
		out["episodeNumber"] = t.Episode
	}
	return out
}
func watchWire(s frontend.WatchStatus) object {
	return object{"positionSeconds": s.ViewOffset.Seconds(), "played": s.ViewCount > 0, "favorite": s.IsFavorite, "watchlisted": s.InWatchlist}
}
func (i *instance) titles(r *http.Request, v *frontend.Viewer, token string, ts []frontend.Title) ([]object, error) {
	keys := make([]frontend.RatingKey, 0, len(ts))
	for _, t := range ts {
		keys = append(keys, t.Key)
	}
	states, err := i.host.Experience.UserData(r.Context(), v, keys)
	if err != nil {
		return nil, err
	}
	art := map[frontend.RatingKey][]object{}
	if signer, ok := i.host.ImageGrants.(frontend.ImagePageSigner); ok {
		for start := 0; start < len(ts); start += 50 {
			end := min(start+50, len(ts))
			images, e := signer.SignPage(r.Context(), frontend.ImagePageRequest{Credential: token, Device: requestDevice(r), Titles: ts[start:end], Kinds: []string{frontend.ImagePoster, frontend.ImageBackdrop, frontend.ImageStill}})
			if e != nil {
				return nil, e
			}
			for _, item := range images {
				for _, g := range item.Images {
					im := g.Image
					art[item.Item] = append(art[item.Item], object{"id": hash(im.Path), "kind": im.Kind, "tag": im.Tag, "sizes": []object{{"width": im.Width, "height": im.Height, "url": "/vela/v0/images/" + g.Secret}}, "blurHash": im.BlurHash, "thumbHash": im.ThumbHash})
				}
			}
		}
	}
	out := []object{}
	for _, t := range ts {
		item := titleWire(t)
		item["watch"] = watchWire(states[t.Key])
		if a := art[t.Key]; a != nil {
			item["artwork"] = a
		}
		out = append(out, item)
	}
	return out, nil
}
func (i *instance) home(r *http.Request, v *frontend.Viewer, token string) ([]object, bool, error) {
	placement := frontend.Placement(r.URL.Query().Get("placement"))
	if placement == "" {
		placement = frontend.PlacementHome
	}
	if placement != frontend.PlacementHome && placement != frontend.PlacementDetail && placement != frontend.PlacementLibrary {
		return nil, false, frontend.ErrNotFound
	}
	var item frontend.RatingKey
	if s := r.URL.Query().Get("titleId"); s != "" {
		var err error
		item, err = ids.ParseRatingKeyGUID(s)
		if err != nil {
			return nil, false, err
		}
	}
	rows := []frontend.Row{}
	outerMore := false
	views, hasViews := i.host.Experience.(frontend.RowViews)
	if id := r.PathValue("id"); id != "" && hasViews {
		offset := 0
		if token := r.URL.Query().Get("cursor"); token != "" {
			c, e := i.readRowToken(r, v, token)
			if e != nil || c.Row != id {
				return nil, false, frontend.ErrRowViewStale
			}
			offset = c.Offset
		}
		row, e := i.rowPage(r, v, id, offset)
		if e != nil {
			return nil, false, e
		}
		i.rowMu.Lock()
		meta, found := i.rowMeta[id]
		i.rowMu.Unlock()
		if !found {
			return nil, false, frontend.ErrRowViewStale
		}
		row.Title = meta.Title
		row.Purpose = meta.Purpose
		row.Style = meta.Layout.Style
		rows = append(rows, row)
	} else if hasViews {
		vv, e := views.ListRowViews(r.Context(), frontend.RowViewRequest{Viewer: v, Device: requestDevice(r), Protocol: "vela", Placement: placement, Item: item})
		if e != nil {
			return nil, false, e
		}
		outerMore = len(vv) > limit(r)
		vv = vv[:min(len(vv), limit(r))]
		for _, view := range vv {
			i.rowMu.Lock()
			if len(i.rowMeta) >= 2000 {
				i.rowMeta = map[string]frontend.RowView{}
			}
			i.rowMeta[view.ID] = view
			i.rowMu.Unlock()
			row, e := i.rowPage(r, v, view.ID, 0)
			if e != nil {
				return nil, false, e
			}
			row.Title = view.Title
			row.Purpose = view.Purpose
			row.Style = view.Layout.Style
			rows = append(rows, row)
		}
	} else {
		var e error
		rows, e = i.host.Experience.Rows(r.Context(), frontend.RowRequest{Viewer: v, Device: requestDevice(r), Placement: placement, Item: item, Limit: 50})
		if e != nil {
			return nil, false, e
		}
	}
	if r.PathValue("id") == "" && len(rows) > limit(r) {
		outerMore = true
		rows = rows[:limit(r)]
	}
	out := []object{}
	for _, row := range rows {
		ts := []frontend.Title{}
		for _, e := range row.Entries {
			if e.Kind == frontend.RowEntryTitle && e.Title != nil {
				ts = append(ts, *e.Title)
			}
		}
		if len(row.Entries) == 0 {
			ts = row.Items
		}
		converted, e := i.titles(r, v, token, ts)
		if e != nil {
			return nil, false, e
		}
		byID := map[string]object{}
		for _, t := range converted {
			byID[t["id"].(string)] = t
		}
		entries := []object{}
		for _, e := range row.Entries {
			base := object{"receiptToken": e.Handle}
			switch {
			case e.Title != nil:
				base["id"] = ids.RatingKeyGUID(e.Title.Key)
				base["kind"] = "title"
				base["title"] = byID[base["id"].(string)]
			case e.LiveChannel != nil:
				c := e.LiveChannel
				base["id"] = c.ID
				base["kind"] = "channel"
				base["channel"] = object{"id": c.ID, "number": c.Number, "name": c.Name, "kind": "", "favorite": false, "artwork": []object{}}
			default:
				continue
			}
			entries = append(entries, base)
		}
		if len(row.Entries) == 0 {
			for _, t := range converted {
				entries = append(entries, object{"id": t["id"], "kind": "title", "title": t, "receiptToken": ""})
			}
		}
		continuation := "exhausted"
		if row.More || row.Kind == "truncated" {
			continuation = "unknown"
		}
		wire := object{"id": row.ID, "name": row.Title, "purpose": row.Purpose, "receiptToken": row.Handle, "style": row.Style, "items": entries, "continuation": continuation}
		if row.More && hasViews {
			offset := len(row.Entries)
			if old := r.URL.Query().Get("cursor"); old != "" {
				c, _ := i.readRowToken(r, v, old)
				offset += c.Offset
			}
			wire["nextCursor"] = i.rowToken(r, v, row.ID, offset)
			wire["continuation"] = "more"
		}
		out = append(out, wire)
	}
	return out, outerMore, nil
}
func (i *instance) bootstrap(w http.ResponseWriter, r *http.Request) {
	v, t, e := i.viewer(r)
	if e != nil {
		fail(w, e)
		return
	}
	rows, _, e := i.home(r, v, t)
	if e != nil {
		fail(w, e)
		return
	}
	out := object{"viewer": viewerWire(v), "capabilities": i.capabilities(), "defaultSkinId": "default", "rows": rows}
	if hero := heroFromRows(rows); hero != nil {
		out["hero"] = hero
	}
	cache(w, r, out)
}
func (i *instance) rows(w http.ResponseWriter, r *http.Request) {
	v, t, e := i.viewer(r)
	if e != nil {
		fail(w, e)
		return
	}
	if r.PathValue("id") == "" && !noCursor(w, r) {
		return
	}
	rows, more, e := i.home(r, v, t)
	if e != nil {
		fail(w, e)
		return
	}
	if id := r.PathValue("id"); id != "" {
		for _, row := range rows {
			if row["id"] == id {
				cache(w, r, row)
				return
			}
		}
		problem(w, 404, "not-found", "not_found")
		return
	}
	continuation := "exhausted"
	if more {
		continuation = "unknown"
	}
	cache(w, r, object{"items": rows, "continuation": continuation})
}
func (i *instance) detail(w http.ResponseWriter, r *http.Request) {
	v, t, e := i.viewer(r)
	if e != nil {
		fail(w, e)
		return
	}
	k, e := key(r)
	if e != nil {
		problem(w, 404, "not-found", "not_found")
		return
	}
	d, e := i.host.Experience.Item(r.Context(), frontend.ItemRequest{Viewer: v, Device: requestDevice(r), Key: k})
	if e != nil {
		fail(w, e)
		return
	}
	extras := []object{}
	for _, x := range d.Extras {
		extras = append(extras, object{"id": ids.RatingKeyGUID(x.Key), "parentId": ids.RatingKeyGUID(x.ParentKey), "kind": x.Kind, "name": x.Title, "durationSeconds": x.Duration.Seconds(), "availability": "unavailable"})
	}
	if strings.HasSuffix(r.URL.Path, "/extras") {
		cache(w, r, object{"items": extras})
		return
	}
	ts, e := i.titles(r, v, t, []frontend.Title{d.Title})
	if e != nil {
		fail(w, e)
		return
	}
	ts[0]["watch"] = watchWire(d.UserData)
	cast := []object{}
	for _, p := range d.Title.Cast {
		cast = append(cast, object{"name": p.Name, "role": p.Role})
	}
	out := object{"title": ts[0], "extras": extras, "chapters": []object{}, "cast": cast, "crew": crew(d.Title)}
	if d.CanPlay != nil {
		out["canPlay"] = *d.CanPlay
	}
	if summary := mediaSummary(d); summary != nil {
		out["mediaSummary"] = summary
	}
	if err := i.detailChildren(r, v, t, d, out); err != nil {
		fail(w, err)
		return
	}
	cache(w, r, out)
}
func (i *instance) children(w http.ResponseWriter, r *http.Request) {
	v, t, e := i.viewer(r)
	if e != nil {
		fail(w, e)
		return
	}
	if !noCursor(w, r) {
		return
	}
	k, e := key(r)
	if e != nil {
		problem(w, 404, "not-found", "not_found")
		return
	}
	p, e := i.host.Experience.Children(r.Context(), v, requestDevice(r), k, frontend.ItemQuery{Limit: limit(r), NoTotal: true})
	if e != nil {
		fail(w, e)
		return
	}
	ts, e := i.titles(r, v, t, p.Items)
	if e != nil {
		fail(w, e)
		return
	}
	c := "exhausted"
	if len(ts) == limit(r) {
		c = "unknown"
	}
	cache(w, r, object{"items": ts, "continuation": c})
}
func (i *instance) search(w http.ResponseWriter, r *http.Request) {
	v, t, e := i.viewer(r)
	if e != nil {
		fail(w, e)
		return
	}
	var b struct {
		Query, Mode, Cursor string
		Generation, Limit   int
	}
	if body(r, &b) != nil || b.Query == "" {
		problem(w, 400, "request", "invalid_request")
		return
	}
	if b.Cursor != "" {
		problem(w, 409, "conflict", "cursor_expired")
		return
	}
	n := b.Limit
	if n <= 0 {
		n = 50
	}
	n = min(n, 100)
	var p frontend.SearchPage
	switch b.Mode {
	case "descriptive":
		source, ok := i.host.Experience.(frontend.DescriptiveSearcher)
		if !ok || !source.DescriptiveSearchAvailable() {
			i.unavailable(w, r)
			return
		}
		p, e = source.DescriptiveSearch(r.Context(), v, requestDevice(r), b.Query, frontend.ItemQuery{Limit: n, NoTotal: true})
	case "fast":
		p, e = i.host.Experience.Search(i.host.Experience.NoProviderWait(r.Context()), v, requestDevice(r), b.Query, frontend.ItemQuery{Limit: n, NoTotal: true})
	default:
		problem(w, 400, "request", "invalid_search_mode")
		return
	}
	if e != nil {
		fail(w, e)
		return
	}
	ts, e := i.titles(r, v, t, p.Page.Items)
	if e != nil {
		fail(w, e)
		return
	}
	send(w, 200, object{"query": b.Query, "generation": b.Generation, "items": ts, "continuation": p.Continuation})
}
func (i *instance) interactions(w http.ResponseWriter, r *http.Request) {
	v, _, e := i.viewer(r)
	if e != nil {
		fail(w, e)
		return
	}
	var b struct {
		Interactions []struct {
			ReceiptToken, Kind  string
			OccurredAt          time.Time
			VisibleMilliseconds int
		}
	}
	if body(r, &b) != nil || len(b.Interactions) > 100 {
		problem(w, 400, "request", "invalid_request")
		return
	}
	reporter, ok := i.host.Experience.(frontend.RowInteractionReporter)
	if !ok {
		i.unavailable(w, r)
		return
	}
	for _, x := range b.Interactions {
		kind := frontend.RowInteractionKind("")
		switch x.Kind {
		case "visible":
			kind = frontend.RowImpression
		case "selected":
			kind = frontend.RowClick
		case "played":
			kind = frontend.RowPlay
		default:
			problem(w, 400, "request", "invalid_interaction")
			return
		}
		if e = reporter.ReportRowInteraction(r.Context(), frontend.RowInteraction{Viewer: v, Device: requestDevice(r), Protocol: "vela", Handle: x.ReceiptToken, Kind: kind}); e != nil {
			fail(w, e)
			return
		}
	}
	send(w, 200, object{"accepted": true})
}
func (i *instance) watch(w http.ResponseWriter, r *http.Request) {
	v, _, e := i.viewer(r)
	if e != nil {
		fail(w, e)
		return
	}
	k, e := key(r)
	if e != nil {
		problem(w, 404, "not-found", "not_found")
		return
	}
	if _, e = i.host.Experience.Item(r.Context(), frontend.ItemRequest{Viewer: v, Device: requestDevice(r), Key: k}); e != nil {
		fail(w, e)
		return
	}
	var b struct{ Played, Favorite, Watchlisted *bool }
	if body(r, &b) != nil {
		problem(w, 400, "request", "invalid_request")
		return
	}
	if b.Played != nil {
		if *b.Played {
			e = i.host.Experience.SetPlayed(r.Context(), v, k)
		} else {
			e = i.host.Experience.SetUnplayed(r.Context(), v, k)
		}
	}
	if e == nil && b.Favorite != nil {
		e = i.host.Experience.SetFavorite(r.Context(), v, k, *b.Favorite)
	}
	if e == nil && b.Watchlisted != nil {
		e = i.host.Experience.SetWatchlist(r.Context(), v, k, *b.Watchlisted)
	}
	if e != nil {
		fail(w, e)
		return
	}
	send(w, 200, object{"accepted": true})
}
