package silo

import (
	"errors"
	"net/http"

	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/kit/ids"
)

func (i *instance) mutation(w http.ResponseWriter, r *http.Request) {
	v, _, err := i.viewer(r)
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
	if _, err = i.host.Experience.Item(r.Context(), frontend.ItemRequest{Viewer: v, Device: device(r), Key: key}); err != nil {
		http.NotFound(w, r)
		return
	}
	switch r.PathValue("kind") {
	case "not_interested":
		err = i.host.Experience.SetNotInterested(r.Context(), v, key, r.Method == "PUT")
	case "watchlist":
		err = i.host.Experience.SetWatchlist(r.Context(), v, key, r.Method == "PUT")
	default:
		if r.Method == "PUT" {
			err = i.host.Experience.SetFavorite(r.Context(), v, key, true)
		} else if r.Method == "DELETE" {
			err = i.host.Experience.SetFavorite(r.Context(), v, key, false)
		}
	case "watched":
		if r.Method == "POST" {
			err = i.host.Experience.SetPlayed(r.Context(), v, key)
		} else {
			err = i.host.Experience.SetUnplayed(r.Context(), v, key)
		}
	}
	if err != nil {
		http.Error(w, "Unavailable", 503)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
func (i *instance) mutationRoutes() error {
	for _, e := range []struct{ method, path, kind string }{{"PUT", "/api/v2/not-interested/{id}", "not_interested"}, {"DELETE", "/api/v2/not-interested/{id}", "not_interested"}, {"PUT", "/api/v2/watchlist/{id}", "watchlist"}, {"DELETE", "/api/v2/watchlist/{id}", "watchlist"}, {"PUT", "/api/v2/favorites/{id}", "favorite"}, {"DELETE", "/api/v2/favorites/{id}", "favorite"}, {"POST", "/api/v2/watched/{id}", "watched"}, {"DELETE", "/api/v2/watched/{id}", "watched"}} {
		kind := e.kind
		handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { r.SetPathValue("kind", kind); i.mutation(w, r) })
		if err := i.router.Handle(e.method, e.path, handler); err != nil {
			return err
		}
	}
	for _, e := range []struct{ path, kind string }{{"/api/v2/favorites/{id}", "favorite"}, {"/api/v2/watchlist/{id}", "watchlist"}} {
		kind := e.kind
		handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			r.SetPathValue("kind", kind)
			i.membershipItem(w, r)
		})
		if err := i.router.Handle("GET", e.path, handler); err != nil {
			return err
		}
	}
	return nil
}

// membershipItem serves one favorite or watchlist entry. added_at is the stored
// membership time, not the title's date added. Absence and inaccessibility are
// both 404; a present row without a real timestamp is unavailable.
func (i *instance) membershipItem(w http.ResponseWriter, r *http.Request) {
	v, _, err := i.viewer(r)
	if err != nil || v == nil {
		http.Error(w, "Unauthorized", 401)
		return
	}
	reader, ok := i.host.Experience.(frontend.MembershipReader)
	if !ok {
		http.Error(w, "Unavailable", 503)
		return
	}
	key, err := ids.ParseRatingKeyGUID(r.PathValue("id"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	var got frontend.Membership
	if r.PathValue("kind") == "watchlist" {
		got, err = reader.WatchlistItem(r.Context(), v, device(r), key)
	} else {
		got, err = reader.FavoriteItem(r.Context(), v, device(r), key)
	}
	if errors.Is(err, frontend.ErrNotFound) || (err == nil && !got.Present) {
		http.NotFound(w, r)
		return
	}
	if err != nil || got.AddedAt == nil || got.AddedAt.IsZero() {
		http.Error(w, "Membership timestamp unavailable", 503)
		return
	}
	reply(w, 200, map[string]any{"content_id": ids.RatingKeyGUID(key), "added_at": got.AddedAt.UTC().Format("2006-01-02T15:04:05.000Z")})
}
