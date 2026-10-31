package silo

import (
	"net/http"
	"strconv"

	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/kit/ids"
)

func (i *instance) catalog(w http.ResponseWriter, r *http.Request) {
	v, token, err := i.viewer(r)
	if err != nil || v == nil {
		http.Error(w, "Unauthorized", 401)
		return
	}
	if i.host.Experience == nil {
		http.Error(w, "Unavailable", 503)
		return
	}
	q := r.URL.Query()
	limit := 50
	if x := q.Get("limit"); x != "" {
		limit, err = strconv.Atoi(x)
		if err != nil || limit < 1 || limit > 100 {
			http.Error(w, "Bad Request", 400)
			return
		}
	}
	offset := 0
	if x := q.Get("cursor"); x != "" {
		offset, err = strconv.Atoi(x)
		if err != nil || offset < 0 {
			http.Error(w, "Bad Request", 400)
			return
		}
	}
	query := frontend.ItemQuery{Offset: offset, Limit: limit}
	var page frontend.Page
	exact := true
	more := false
	if term := q.Get("q"); term != "" {
		result, e := i.host.Experience.Search(r.Context(), v, device(r), term, query)
		if e != nil {
			http.Error(w, "Unavailable", 503)
			return
		}
		page = result.Page
		exact = result.TotalExact
		more = result.Continuation == frontend.ContinuationMore
	} else if q.Get("library_id") == nowAvailableLibraryID {
		titles, ok := i.nowAvailableTitles(r, v)
		if !ok {
			titles = nil
		}
		total := len(titles)
		start := min(offset, total)
		end := total
		if limit > 0 {
			end = min(start+limit, total)
		}
		page = frontend.Page{Items: titles[start:end], Total: total}
		more = end < total
	} else {
		page, err = i.host.Experience.LibraryItems(r.Context(), v, device(r), q.Get("library_id"), query)
		if err != nil {
			http.Error(w, "Unavailable", 503)
			return
		}
		more = offset+len(page.Items) < page.Total
	}
	keys := make([]frontend.RatingKey, 0, len(page.Items))
	for _, t := range page.Items {
		keys = append(keys, t.Key)
	}
	state, err := i.host.Experience.UserData(r.Context(), v, keys)
	if err != nil {
		http.Error(w, "Unavailable", 503)
		return
	}
	items := make([]any, 0, len(page.Items))
	for _, t := range page.Items {
		item := titleDTO(t, state[t.Key])
		i.artwork(r, v, token, t.Key, item)
		i.applyEditionOverlay(r, v, t.Key, item)
		items = append(items, item)
	}
	var next any
	if more && len(page.Items) > 0 {
		next = strconv.Itoa(offset + len(page.Items))
	}
	reply(w, 200, map[string]any{"items": items, "total": page.Total, "total_exact": exact, "window_cursor": "", "page": map[string]any{"has_more": next != nil, "next_cursor": next}})
}
func (i *instance) childItems(w http.ResponseWriter, r *http.Request) {
	v, token, err := i.viewer(r)
	if err != nil || v == nil {
		http.Error(w, "Unauthorized", 401)
		return
	}
	if i.host.Experience == nil {
		http.Error(w, "Unavailable", 503)
		return
	}
	key, err := ids.ParseRatingKeyGUID(r.PathValue("id"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	query := frontend.ItemQuery{Limit: 1000}
	episode := r.PathValue("season") != "" || r.PathValue("all") != ""
	if r.PathValue("season") != "" {
		n, e := strconv.Atoi(r.PathValue("season"))
		if e != nil || n < 0 {
			http.Error(w, "Bad Request", 400)
			return
		}
		query.Season = &n
		query.Recursive = true
	}
	if episode {
		query.Types = []frontend.MediaType{"episode"}
	} else {
		query.Types = []frontend.MediaType{"season"}
	}
	page, err := i.host.Experience.Children(r.Context(), v, device(r), key, query)
	if err != nil {
		http.Error(w, "Unavailable", 503)
		return
	}
	if page.Total > len(page.Items) {
		http.Error(w, "Result too large", 503)
		return
	}
	keys := make([]frontend.RatingKey, 0, len(page.Items))
	for _, t := range page.Items {
		keys = append(keys, t.Key)
	}
	states, err := i.host.Experience.UserData(r.Context(), v, keys)
	if err != nil {
		http.Error(w, "Unavailable", 503)
		return
	}
	items := make([]any, 0, len(page.Items))
	for _, t := range page.Items {
		item := titleDTO(t, states[t.Key])
		item["season_number"] = t.Season
		item["episode_number"] = t.Episode
		item["is_specials"] = t.Season == 0
		item["episode_count"] = t.LeafCount
		i.artwork(r, v, token, t.Key, item)
		i.applyEditionOverlay(r, v, t.Key, item)
		items = append(items, item)
	}
	reply(w, 200, map[string]any{"items": items, "page": map[string]any{"has_more": false, "next_cursor": nil}})
}
func (i *instance) catalogRoutes() error {
	for _, e := range []struct {
		path string
		h    http.HandlerFunc
	}{{"/api/v2/catalog", i.catalog}, {"/api/v2/catalog/series/{id}/seasons", i.childItems}, {"/api/v2/catalog/series/{id}/seasons/{season}/episodes", i.childItems}} {
		if err := i.router.Handle("GET", e.path, e.h); err != nil {
			return err
		}
	}
	return nil
}
