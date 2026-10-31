package silo

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/kit/ids"
)

// similar is GET /api/v2/recommendations/similar/{id}. Cards come from
// Experience's indexed related titles. The answer is bounded (default 20,
// maximum 50) and uses the catalog item shape.
func (i *instance) similar(w http.ResponseWriter, r *http.Request) {
	v, token, err := i.viewer(r)
	if err != nil || v == nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	if i.host.Experience == nil {
		http.Error(w, "Unavailable", http.StatusServiceUnavailable)
		return
	}
	key, err := ids.ParseRatingKeyGUID(r.PathValue("id"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	q := r.URL.Query()
	limit := 20
	if raw := q.Get("limit"); raw != "" {
		limit, err = strconv.Atoi(raw)
		if err != nil || limit < 1 || limit > 50 {
			http.Error(w, "Bad Request", http.StatusBadRequest)
			return
		}
	}
	offset := 0
	if raw := q.Get("cursor"); raw != "" {
		offset, err = strconv.Atoi(raw)
		if err != nil || offset < 0 {
			http.Error(w, "Bad Request", http.StatusBadRequest)
			return
		}
	}
	page, err := i.host.Experience.Similar(r.Context(), v, device(r), key, frontend.ItemQuery{Offset: offset, Limit: limit})
	if err != nil {
		if errors.Is(err, frontend.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		http.Error(w, "Unavailable", http.StatusServiceUnavailable)
		return
	}
	keys := make([]frontend.RatingKey, 0, len(page.Page.Items))
	for _, title := range page.Page.Items {
		keys = append(keys, title.Key)
	}
	state, err := i.host.Experience.UserData(r.Context(), v, keys)
	if err != nil {
		http.Error(w, "Unavailable", http.StatusServiceUnavailable)
		return
	}
	items := make([]any, 0, len(page.Page.Items))
	for _, title := range page.Page.Items {
		item := titleDTO(title, state[title.Key])
		i.artwork(r, v, token, title.Key, item)
		i.applyEditionOverlay(r, v, title.Key, item)
		items = append(items, item)
	}
	body := map[string]any{"items": items}
	if page.Continuation == frontend.ContinuationMore && len(items) > 0 {
		body["page"] = map[string]any{"has_more": true, "next_cursor": strconv.Itoa(offset + len(items))}
	}
	reply(w, http.StatusOK, body)
}
