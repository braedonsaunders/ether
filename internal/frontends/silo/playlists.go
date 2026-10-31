package silo

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/kit/ids"
)

func (i *instance) playlistRoutes() error {
	routes := []struct {
		method, path string
		h            http.HandlerFunc
	}{
		{http.MethodGet, "/api/v2/playlists", i.playlists},
		{http.MethodPost, "/api/v2/playlists", i.createPlaylist},
		{http.MethodGet, "/api/v2/playlists/{id}", i.playlist},
		{http.MethodDelete, "/api/v2/playlists/{id}", i.deletePlaylist},
		{http.MethodGet, "/api/v2/playlists/{id}/items", i.playlistItems},
		{http.MethodPost, "/api/v2/playlists/{id}/items", i.addPlaylistItems},
		{http.MethodDelete, "/api/v2/playlists/{id}/items/{content_id}", i.removePlaylistItem},
		{http.MethodPost, "/api/v2/playlists/{id}/items/{content_id}/move", i.movePlaylistItem},
	}
	for _, route := range routes {
		if err := i.router.Handle(route.method, route.path, route.h); err != nil {
			return err
		}
	}
	return nil
}

func (i *instance) playlistLibrary(r *http.Request) (*frontend.Viewer, string, frontend.PlaylistLibrary, bool) {
	viewer, token, err := i.viewer(r)
	if err != nil || viewer == nil {
		return nil, "", nil, false
	}
	library, _ := i.host.Experience.(frontend.PlaylistLibrary)
	return viewer, token, library, true
}

func (i *instance) playlists(w http.ResponseWriter, r *http.Request) {
	viewer, _, library, ok := i.playlistLibrary(r)
	if !ok {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	if library == nil {
		http.Error(w, "Unavailable", http.StatusServiceUnavailable)
		return
	}
	page, err := library.Playlists(r.Context(), viewer, frontend.PageQuery{Limit: 500})
	if err != nil {
		http.Error(w, "Unavailable", http.StatusServiceUnavailable)
		return
	}
	items := make([]any, 0, len(page.Items))
	for _, playlist := range page.Items {
		items = append(items, map[string]any{"id": ids.RatingKeyGUID(playlist.Key), "name": playlist.Title, "count": len(playlist.Items), "smart": playlist.Smart})
	}
	reply(w, http.StatusOK, map[string]any{"playlists": items})
}

func (i *instance) playlist(w http.ResponseWriter, r *http.Request) {
	viewer, _, library, key, ok := i.siloPlaylistKey(w, r)
	if !ok {
		return
	}
	page, err := library.Playlists(r.Context(), viewer, frontend.PageQuery{Limit: 500})
	if err != nil {
		http.Error(w, "Unavailable", http.StatusServiceUnavailable)
		return
	}
	for _, playlist := range page.Items {
		if playlist.Key == key {
			reply(w, http.StatusOK, map[string]any{"id": ids.RatingKeyGUID(playlist.Key), "name": playlist.Title, "count": len(playlist.Items), "smart": playlist.Smart})
			return
		}
	}
	http.NotFound(w, r)
}

func (i *instance) createPlaylist(w http.ResponseWriter, r *http.Request) {
	viewer, _, library, ok := i.playlistLibrary(r)
	if !ok {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	if library == nil {
		http.Error(w, "Unavailable", http.StatusServiceUnavailable)
		return
	}
	var body struct {
		Name       string   `json:"name"`
		ContentIDs []string `json:"content_ids"`
	}
	if decode(r, &body) != nil {
		http.Error(w, "Bad Request", http.StatusBadRequest)
		return
	}
	keys, err := contentKeys(body.ContentIDs)
	if err != nil {
		http.Error(w, "Bad Request", http.StatusBadRequest)
		return
	}
	playlist, err := library.CreatePlaylist(r.Context(), viewer, body.Name, keys)
	if err != nil {
		writeSiloPlaylistError(w, err)
		return
	}
	reply(w, http.StatusOK, map[string]any{"id": ids.RatingKeyGUID(playlist.Key), "name": playlist.Title, "count": len(playlist.Items), "smart": playlist.Smart})
}

func (i *instance) deletePlaylist(w http.ResponseWriter, r *http.Request) {
	viewer, _, library, key, ok := i.siloPlaylistKey(w, r)
	if !ok {
		return
	}
	if err := library.DeletePlaylist(r.Context(), viewer, key); err != nil {
		writeSiloPlaylistError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (i *instance) playlistItems(w http.ResponseWriter, r *http.Request) {
	viewer, token, _, key, ok := i.siloPlaylistKey(w, r)
	if !ok {
		return
	}
	page, err := i.host.Experience.Children(r.Context(), viewer, device(r), key, frontend.ItemQuery{Limit: 500})
	if err != nil {
		writeSiloPlaylistError(w, err)
		return
	}
	keys := make([]frontend.RatingKey, len(page.Items))
	for n, title := range page.Items {
		keys[n] = title.Key
	}
	states, err := i.host.Experience.UserData(r.Context(), viewer, keys)
	if err != nil {
		http.Error(w, "Unavailable", http.StatusServiceUnavailable)
		return
	}
	items := make([]any, 0, len(page.Items))
	for _, title := range page.Items {
		item := titleDTO(title, states[title.Key])
		i.artwork(r, viewer, token, title.Key, item)
		items = append(items, item)
	}
	reply(w, http.StatusOK, map[string]any{"items": items, "page": map[string]any{"has_more": page.Total > len(page.Items), "next_cursor": nil}})
}

func (i *instance) addPlaylistItems(w http.ResponseWriter, r *http.Request) {
	viewer, _, library, key, ok := i.siloPlaylistKey(w, r)
	if !ok {
		return
	}
	var body struct {
		ContentIDs []string `json:"content_ids"`
	}
	if decode(r, &body) != nil {
		http.Error(w, "Bad Request", http.StatusBadRequest)
		return
	}
	keys, err := contentKeys(body.ContentIDs)
	if err != nil || len(keys) == 0 {
		http.Error(w, "Bad Request", http.StatusBadRequest)
		return
	}
	if err = library.AddPlaylistItems(r.Context(), viewer, key, keys); err != nil {
		writeSiloPlaylistError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (i *instance) removePlaylistItem(w http.ResponseWriter, r *http.Request) {
	viewer, _, library, key, ok := i.siloPlaylistKey(w, r)
	if !ok {
		return
	}
	index, err := i.playlistContentIndex(r, viewer, key, r.PathValue("content_id"))
	if err != nil {
		writeSiloPlaylistError(w, err)
		return
	}
	if err = library.RemovePlaylistItem(r.Context(), viewer, key, index); err != nil {
		writeSiloPlaylistError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (i *instance) movePlaylistItem(w http.ResponseWriter, r *http.Request) {
	viewer, _, library, key, ok := i.siloPlaylistKey(w, r)
	if !ok {
		return
	}
	var body struct {
		Index int `json:"index"`
	}
	if decode(r, &body) != nil || body.Index < 0 {
		http.Error(w, "Bad Request", http.StatusBadRequest)
		return
	}
	from, err := i.playlistContentIndex(r, viewer, key, r.PathValue("content_id"))
	if err != nil {
		writeSiloPlaylistError(w, err)
		return
	}
	if err = library.MovePlaylistItem(r.Context(), viewer, key, from, body.Index); err != nil {
		writeSiloPlaylistError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (i *instance) siloPlaylistKey(w http.ResponseWriter, r *http.Request) (*frontend.Viewer, string, frontend.PlaylistLibrary, frontend.RatingKey, bool) {
	viewer, token, library, ok := i.playlistLibrary(r)
	if !ok {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return nil, "", nil, 0, false
	}
	if library == nil || i.host.Experience == nil {
		http.Error(w, "Unavailable", http.StatusServiceUnavailable)
		return nil, "", nil, 0, false
	}
	key, err := ids.ParseRatingKeyGUID(r.PathValue("id"))
	if err != nil {
		http.NotFound(w, r)
		return nil, "", nil, 0, false
	}
	return viewer, token, library, key, true
}

func (i *instance) playlistContentIndex(r *http.Request, viewer *frontend.Viewer, playlist frontend.RatingKey, contentID string) (int, error) {
	detail, err := i.host.Experience.Item(r.Context(), frontend.ItemRequest{Viewer: viewer, Device: device(r), Key: playlist})
	if err != nil {
		return 0, err
	}
	if detail.Playlist == nil {
		return 0, frontend.ErrNotFound
	}
	nth := 0
	if cut := strings.LastIndex(contentID, "#"); cut > 0 {
		n, err := strconv.Atoi(contentID[cut+1:])
		if err != nil || n < 0 {
			return 0, frontend.ErrBadPlaylist
		}
		nth = n
		contentID = contentID[:cut]
	}
	key, err := ids.ParseRatingKeyGUID(contentID)
	if err != nil {
		return 0, frontend.ErrBadPlaylist
	}
	seen := 0
	for index, candidate := range detail.Playlist.Items {
		if candidate != key {
			continue
		}
		if seen == nth {
			return index, nil
		}
		seen++
	}
	return 0, frontend.ErrNotFound
}

func contentKeys(values []string) ([]frontend.RatingKey, error) {
	out := make([]frontend.RatingKey, 0, len(values))
	for _, value := range values {
		key, err := ids.ParseRatingKeyGUID(strings.TrimSpace(value))
		if err != nil {
			return nil, err
		}
		out = append(out, key)
	}
	return out, nil
}

func writeSiloPlaylistError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, frontend.ErrNotFound):
		http.Error(w, "Not Found", http.StatusNotFound)
	case errors.Is(err, frontend.ErrUnauthorized):
		http.Error(w, "Forbidden", http.StatusForbidden)
	case errors.Is(err, frontend.ErrBadPlaylist):
		http.Error(w, "Bad Request", http.StatusBadRequest)
	default:
		http.Error(w, "Unavailable", http.StatusServiceUnavailable)
	}
}
