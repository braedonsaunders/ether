package silo

import (
	"net/http"
	"strconv"
	"time"

	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/kit/ids"
)

func (i *instance) notInterestedCapability(w http.ResponseWriter, r *http.Request) {
	v, _, err := i.viewer(r)
	if err != nil || v == nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	allowed := i.host.Experience != nil
	state := "available"
	if !allowed {
		state = "not_configured"
	}
	reply(w, http.StatusOK, map[string]any{"revision": "ether-not-interested-v1", "state": state, "allowed": allowed})
}

func (i *instance) notInterestedItem(w http.ResponseWriter, r *http.Request) {
	v, _, err := i.viewer(r)
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
	entry, err := i.host.Experience.NotInterestedItem(r.Context(), v, device(r), key)
	if err != nil || entry.Key != key {
		http.NotFound(w, r)
		return
	}
	if entry.AddedAt.IsZero() {
		http.Error(w, "Timestamp unavailable", http.StatusServiceUnavailable)
		return
	}
	reply(w, http.StatusOK, map[string]any{"item_id": ids.RatingKeyGUID(entry.Key), "marked_at": entry.AddedAt.UTC().Format(time.RFC3339Nano)})
}

func (i *instance) notInterestedList(w http.ResponseWriter, r *http.Request) {
	v, credential, err := i.viewer(r)
	if err != nil || v == nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	if i.host.Experience == nil {
		http.Error(w, "Unavailable", http.StatusServiceUnavailable)
		return
	}
	limit, offset := 50, 0
	if raw := r.URL.Query().Get("limit"); raw != "" {
		limit, err = strconv.Atoi(raw)
		if err != nil || limit < 1 || limit > 200 {
			http.Error(w, "Bad Request", http.StatusBadRequest)
			return
		}
	}
	if raw := r.URL.Query().Get("cursor"); raw != "" {
		offset, err = strconv.Atoi(raw)
		if err != nil || offset < 0 {
			http.Error(w, "Bad Request", http.StatusBadRequest)
			return
		}
	}
	page, err := i.host.Experience.NotInterested(r.Context(), v, device(r), frontend.PageQuery{Offset: offset, Limit: limit})
	if err != nil {
		http.Error(w, "Unavailable", http.StatusServiceUnavailable)
		return
	}
	items := make([]any, 0, len(page.Items))
	for _, entry := range page.Items {
		detail, e := i.host.Experience.Item(r.Context(), frontend.ItemRequest{Viewer: v, Device: device(r), Key: entry.Key})
		if e != nil || detail.Title.Key != entry.Key {
			continue
		}
		state := detail.UserData
		state.IsNotInterested = true
		item := titleDTO(detail.Title, state)
		i.artwork(r, v, credential, entry.Key, item)
		items = append(items, item)
	}
	pageInfo := map[string]any{"has_more": false}
	if offset < page.Total && len(page.Items) > 0 && len(page.Items) < page.Total-offset {
		pageInfo["has_more"] = true
		pageInfo["next_cursor"] = strconv.Itoa(offset + len(page.Items))
	}
	reply(w, http.StatusOK, map[string]any{"items": items, "page": pageInfo})
}

func (i *instance) notInterestedRoutes() error {
	for _, route := range []struct {
		path    string
		handler http.HandlerFunc
	}{
		{"/api/v2/capabilities/not-interested", i.notInterestedCapability},
		{"/api/v2/not-interested", i.notInterestedList},
		{"/api/v2/not-interested/{id}", i.notInterestedItem},
	} {
		if err := i.router.Handle(http.MethodGet, route.path, route.handler); err != nil {
			return err
		}
	}
	return nil
}
