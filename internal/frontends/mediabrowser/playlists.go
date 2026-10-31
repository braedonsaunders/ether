package mediabrowser

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/kit/ids"
	"github.com/braedonsaunders/ether/internal/frontend/kit/route"
)

func (i *instance) playlistRoutes() error {
	routes := []struct {
		method, path string
		h            http.HandlerFunc
	}{
		{"GET", "/Playlists/{playlistId}", i.playlist},
		{"GET", "/Playlists/{playlistId}/Items", i.playlistItems},
		{"POST", "/Playlists", i.createPlaylist},
		{"POST", "/Playlists/{playlistId}/Items", i.addPlaylistItems},
		{"DELETE", "/Playlists/{playlistId}/Items", i.removePlaylistItems},
		{"POST", "/Playlists/{playlistId}/Items/{itemId}/Move/{newIndex}", i.movePlaylistItem},
		{"DELETE", "/Playlists/{playlistId}", i.deletePlaylist},
	}
	for _, entry := range routes {
		if err := i.router.Handle(entry.method, entry.path, entry.h); err != nil {
			return err
		}
	}
	return nil
}

func (i *instance) playlistLibrary(r *http.Request) (*frontend.Viewer, frontend.PlaylistLibrary, bool) {
	viewer, _, err := i.viewer(r)
	if err != nil || viewer == nil {
		return nil, nil, false
	}
	if i.host.Experience == nil {
		return viewer, nil, true
	}
	library, _ := i.host.Experience.(frontend.PlaylistLibrary)
	return viewer, library, true
}

func (i *instance) writePlaylists(w http.ResponseWriter, r *http.Request, viewer *frontend.Viewer, q frontend.ItemQuery) {
	if viewer == nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	library, ok := i.host.Experience.(frontend.PlaylistLibrary)
	if !ok {
		jsonReply(w, i.queryResult([]any{}, 0, q.Offset))
		return
	}
	page, err := library.Playlists(r.Context(), viewer, frontend.PageQuery{Offset: q.Offset, Limit: q.Limit})
	if err != nil {
		http.Error(w, "Unavailable", http.StatusServiceUnavailable)
		return
	}
	items := make([]any, 0, len(page.Items))
	for _, playlist := range page.Items {
		items = append(items, i.playlistDTO(playlist))
	}
	jsonReply(w, i.queryResult(items, page.Total, q.Offset))
}

func (i *instance) playlistDTO(playlist core.Playlist) map[string]any {
	id := ids.RatingKeyGUID(playlist.Key)
	item := map[string]any{
		"Id": id, "Name": playlist.Title, "Type": "Playlist", "IsFolder": true,
		"ChildCount": len(playlist.Items), "MediaType": "Video", "Overview": "",
		"ServerId": i.info.ID, "SortName": playlist.Title, "CanDelete": !playlist.Smart,
		"CanDownload": false, "ProviderIds": map[string]string{},
	}
	if i.protocol == "jellyfin" {
		i.jellyfinCollectionFolder(item, id)
		item["Type"] = "Playlist"
		item["ChildCount"] = len(playlist.Items)
		item["MediaType"] = "Video"
		item["CanDelete"] = !playlist.Smart
		delete(item, "CollectionType")
	}
	if i.protocol == "emby" {
		i.embyItemDTO(item, frontend.Title{Key: playlist.Key, Title: playlist.Title}, frontend.WatchStatus{})
		item["Type"] = "Playlist"
		item["IsFolder"] = true
		item["ChildCount"] = len(playlist.Items)
		item["MediaType"] = "Video"
	}
	return item
}

func (i *instance) playlist(w http.ResponseWriter, r *http.Request) {
	viewer, library, ok := i.playlistLibrary(r)
	if !ok {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	if library == nil {
		http.Error(w, "Unavailable", http.StatusServiceUnavailable)
		return
	}
	key, err := ids.ParseRatingKeyGUID(r.PathValue("playlistId"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	detail, err := i.host.Experience.Item(r.Context(), frontend.ItemRequest{Viewer: viewer, Device: device(r), Key: key})
	if err != nil || detail.Playlist == nil {
		http.NotFound(w, r)
		return
	}
	jsonReply(w, i.playlistDTO(*detail.Playlist))
}

func (i *instance) playlistItems(w http.ResponseWriter, r *http.Request) {
	viewer, library, ok := i.playlistLibrary(r)
	if !ok {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	if library == nil || i.host.Experience == nil {
		http.Error(w, "Unavailable", http.StatusServiceUnavailable)
		return
	}
	key, err := ids.ParseRatingKeyGUID(r.PathValue("playlistId"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	detail, err := i.host.Experience.Item(r.Context(), frontend.ItemRequest{Viewer: viewer, Device: device(r), Key: key})
	if err != nil || detail.Playlist == nil {
		http.NotFound(w, r)
		return
	}
	q := itemQuery(r)
	page, err := i.host.Experience.Children(r.Context(), viewer, device(r), key, q)
	if err != nil {
		writePlaylistError(w, err)
		return
	}
	states := map[frontend.RatingKey]frontend.WatchStatus{}
	if len(page.Items) > 0 {
		keys := make([]frontend.RatingKey, len(page.Items))
		for n, title := range page.Items {
			keys[n] = title.Key
		}
		states, err = i.host.Experience.UserData(r.Context(), viewer, keys)
		if err != nil {
			http.Error(w, "Unavailable", http.StatusServiceUnavailable)
			return
		}
	}
	idsFor := playlistItemIDs(detail.Playlist.Items)
	cursor := 0
	items := make([]any, 0, len(page.Items))
	for _, title := range page.Items {
		item := i.itemDTO(title, states[title.Key])
		for cursor < len(detail.Playlist.Items) && detail.Playlist.Items[cursor] != title.Key {
			cursor++
		}
		if cursor < len(idsFor) && cursor < len(detail.Playlist.Items) && detail.Playlist.Items[cursor] == title.Key {
			item["PlaylistItemId"] = idsFor[cursor]
			cursor++
		}
		i.markUnreleased(item, title)
		items = append(items, item)
	}
	jsonReply(w, i.queryResult(items, page.Total, q.Offset))
}

func (i *instance) createPlaylist(w http.ResponseWriter, r *http.Request) {
	viewer, library, ok := i.playlistLibrary(r)
	if !ok {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	if library == nil {
		http.Error(w, "Unavailable", http.StatusServiceUnavailable)
		return
	}
	name, rawIDs, userID, err := playlistCreateFields(r)
	if err != nil {
		http.Error(w, "Bad Request", http.StatusBadRequest)
		return
	}
	if !sameViewer(viewer, userID) {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}
	keys, err := parseTitleIDs(rawIDs)
	if err != nil {
		http.Error(w, "Bad Request", http.StatusBadRequest)
		return
	}
	playlist, err := library.CreatePlaylist(r.Context(), viewer, name, keys)
	if err != nil {
		writePlaylistError(w, err)
		return
	}
	jsonReply(w, i.playlistDTO(*playlist))
}

func (i *instance) addPlaylistItems(w http.ResponseWriter, r *http.Request) {
	viewer, library, key, ok := i.playlistKey(w, r)
	if !ok {
		return
	}
	if !sameViewer(viewer, firstQuery(r, "UserId")) {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}
	raw := firstQuery(r, "Ids")
	if raw == "" {
		var body struct {
			Ids idList `json:"Ids"`
		}
		if r.Body != nil && decode(r, &body) != nil {
			http.Error(w, "Bad Request", http.StatusBadRequest)
			return
		}
		raw = strings.Join(body.Ids, ",")
	}
	keys, err := parseTitleIDs(splitCSV(raw))
	if err != nil || len(keys) == 0 {
		http.Error(w, "Bad Request", http.StatusBadRequest)
		return
	}
	if err = library.AddPlaylistItems(r.Context(), viewer, key, keys); err != nil {
		writePlaylistError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (i *instance) removePlaylistItems(w http.ResponseWriter, r *http.Request) {
	viewer, library, key, ok := i.playlistKey(w, r)
	if !ok {
		return
	}
	entries := splitCSV(firstQuery(r, "EntryIds"))
	if len(entries) == 0 {
		http.Error(w, "Bad Request", http.StatusBadRequest)
		return
	}
	visible, err := i.visiblePlaylistKeys(r, viewer, key)
	if err != nil {
		writePlaylistError(w, err)
		return
	}
	indexes := make([]int, 0, len(entries))
	for _, entry := range entries {
		index, found := visiblePlaylistIndex(visible, entry)
		if !found {
			http.NotFound(w, r)
			return
		}
		indexes = append(indexes, index)
	}
	for i := 1; i < len(indexes); i++ {
		for j := i; j > 0 && indexes[j] > indexes[j-1]; j-- {
			indexes[j], indexes[j-1] = indexes[j-1], indexes[j]
		}
	}
	for _, index := range indexes {
		if err = library.RemovePlaylistItem(r.Context(), viewer, key, index); err != nil {
			writePlaylistError(w, err)
			return
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

func (i *instance) movePlaylistItem(w http.ResponseWriter, r *http.Request) {
	viewer, library, key, ok := i.playlistKey(w, r)
	if !ok {
		return
	}
	to, err := strconv.Atoi(r.PathValue("newIndex"))
	if err != nil || to < 0 {
		http.Error(w, "Bad Request", http.StatusBadRequest)
		return
	}
	visible, err := i.visiblePlaylistKeys(r, viewer, key)
	if err != nil {
		writePlaylistError(w, err)
		return
	}
	from, found := visiblePlaylistIndex(visible, r.PathValue("itemId"))
	if !found {
		http.NotFound(w, r)
		return
	}
	if err = library.MovePlaylistItem(r.Context(), viewer, key, from, to); err != nil {
		writePlaylistError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (i *instance) deletePlaylist(w http.ResponseWriter, r *http.Request) {
	viewer, library, key, ok := i.playlistKey(w, r)
	if !ok {
		return
	}
	if err := library.DeletePlaylist(r.Context(), viewer, key); err != nil {
		writePlaylistError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (i *instance) playlistKey(w http.ResponseWriter, r *http.Request) (*frontend.Viewer, frontend.PlaylistLibrary, core.RatingKey, bool) {
	viewer, library, ok := i.playlistLibrary(r)
	if !ok {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return nil, nil, 0, false
	}
	if library == nil {
		http.Error(w, "Unavailable", http.StatusServiceUnavailable)
		return nil, nil, 0, false
	}
	key, err := ids.ParseRatingKeyGUID(r.PathValue("playlistId"))
	if err != nil {
		http.NotFound(w, r)
		return nil, nil, 0, false
	}
	return viewer, library, key, true
}

func (i *instance) visiblePlaylistKeys(r *http.Request, viewer *frontend.Viewer, key core.RatingKey) ([]core.RatingKey, error) {
	detail, err := i.host.Experience.Item(r.Context(), frontend.ItemRequest{Viewer: viewer, Device: device(r), Key: key})
	if err != nil {
		return nil, err
	}
	if detail.Playlist == nil {
		return nil, core.ErrNotFound
	}
	return detail.Playlist.Items, nil
}

func writePlaylistError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, core.ErrNotFound), errors.Is(err, frontend.ErrNotFound):
		http.Error(w, "Not Found", http.StatusNotFound)
	case errors.Is(err, core.ErrUnauthorized):
		http.Error(w, "Forbidden", http.StatusForbidden)
	case errors.Is(err, frontend.ErrBadPlaylist):
		http.Error(w, "Bad Request", http.StatusBadRequest)
	default:
		http.Error(w, "Unavailable", http.StatusServiceUnavailable)
	}
}

type idList []string

func (l *idList) UnmarshalJSON(body []byte) error {
	if len(body) == 0 || string(body) == "null" {
		return nil
	}
	if body[0] == '"' {
		var text string
		if err := json.Unmarshal(body, &text); err != nil {
			return err
		}
		*l = splitCSV(text)
		return nil
	}
	var values []string
	if err := json.Unmarshal(body, &values); err != nil {
		return err
	}
	*l = values
	return nil
}

func playlistCreateFields(r *http.Request) (string, []string, string, error) {
	name := firstQuery(r, "Name")
	ids := splitCSV(firstQuery(r, "Ids"))
	userID := firstQuery(r, "UserId")
	if r.Body != nil && r.ContentLength != 0 {
		var body struct {
			Name   string `json:"Name"`
			Ids    idList `json:"Ids"`
			UserId string `json:"UserId"`
		}
		if err := decode(r, &body); err != nil {
			return "", nil, "", err
		}
		if body.Name != "" {
			name = body.Name
		}
		if len(body.Ids) > 0 {
			ids = body.Ids
		}
		if body.UserId != "" {
			userID = body.UserId
		}
	}
	return name, ids, userID, nil
}

func parseTitleIDs(values []string) ([]core.RatingKey, error) {
	out := make([]core.RatingKey, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		key, err := ids.ParseRatingKeyGUID(value)
		if err != nil {
			return nil, err
		}
		out = append(out, key)
	}
	return out, nil
}

func playlistItemIDs(keys []core.RatingKey) []string {
	seen := map[core.RatingKey]int{}
	out := make([]string, len(keys))
	for i, key := range keys {
		id := ids.RatingKeyGUID(key)
		if n := seen[key]; n > 0 {
			id += "#" + strconv.Itoa(n)
		}
		seen[key]++
		out[i] = id
	}
	return out
}

func visiblePlaylistIndex(keys []core.RatingKey, entry string) (int, bool) {
	nth := 0
	if cut := strings.LastIndex(entry, "#"); cut > 0 {
		n, err := strconv.Atoi(entry[cut+1:])
		if err != nil || n < 0 {
			return 0, false
		}
		nth = n
		entry = entry[:cut]
	}
	key, err := ids.ParseRatingKeyGUID(entry)
	if err != nil {
		return 0, false
	}
	n := 0
	for i, candidate := range keys {
		if candidate != key {
			continue
		}
		if n == nth {
			return i, true
		}
		n++
	}
	return 0, false
}

func firstQuery(r *http.Request, name string) string {
	values := route.Query(r.URL.Query(), name)
	if len(values) != 1 {
		return ""
	}
	return values[0]
}

func sameViewer(viewer *frontend.Viewer, id string) bool {
	id = strings.TrimSpace(id)
	if id == "" || viewer == nil {
		return id == ""
	}
	if id == strconv.FormatInt(viewer.ID, 10) {
		return true
	}
	guid, err := ids.GUID(ids.User, viewer.ID)
	if err != nil {
		return false
	}
	return strings.EqualFold(strings.ReplaceAll(id, "-", ""), guid)
}
