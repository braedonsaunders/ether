package mediabrowser

import (
	"encoding/json"
	"errors"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/kit/route"
	"io"
	"net/http"
)

func (i *instance) displayPreferences(w http.ResponseWriter, r *http.Request) {
	v, _, err := i.viewer(r)
	if err != nil || v == nil {
		http.Error(w, "Unauthorized", 401)
		return
	}
	if i.host.Preferences == nil {
		http.Error(w, "Unavailable", 503)
		return
	}
	client := route.Query(r.URL.Query(), "client")
	key := r.PathValue("id")
	if len(client) != 1 || client[0] == "" || len(client[0]) > 128 || len(key) > 128 {
		http.Error(w, "Bad Request", 400)
		return
	}
	if r.Method == "POST" {
		body, e := io.ReadAll(http.MaxBytesReader(w, r.Body, 65536))
		if e != nil || !json.Valid(body) {
			http.Error(w, "Bad Request", 400)
			return
		}
		var object map[string]any
		if json.Unmarshal(body, &object) != nil || object == nil {
			http.Error(w, "Bad Request", 400)
			return
		}
		if e = i.host.Preferences.Put(r.Context(), v, client[0], key, body); e != nil {
			http.Error(w, "Unavailable", 503)
			return
		}
		w.WriteHeader(204)
		return
	}
	body, err := i.host.Preferences.Get(r.Context(), v, client[0], key)
	if errors.Is(err, frontend.ErrPreferencesNotFound) {
		i.writeDisplayPreferences(w, key, client[0], nil)
		return
	}
	if err != nil {
		http.Error(w, "Unavailable", 503)
		return
	}
	i.writeDisplayPreferences(w, key, client[0], body)
}

// Jellyfin's DisplayPreferences constructor fills these fields even when the
// viewer has never saved a document. CustomPrefs stay empty: those keys are
// client-written settings, not server defaults.
func jellyfinDisplayDefaults(id, client string) map[string]any {
	return map[string]any{
		"Id": id, "Client": client, "CustomPrefs": map[string]any{},
		"ScrollDirection": "Horizontal", "SortOrder": "Ascending", "SortBy": "SortName",
		"RememberIndexing": false, "RememberSorting": false, "ShowBackdrop": true, "ShowSidebar": false,
		"PrimaryImageHeight": 250, "PrimaryImageWidth": 250,
	}
}

func (i *instance) writeDisplayPreferences(w http.ResponseWriter, id, client string, body []byte) {
	if i.protocol != "jellyfin" {
		if body == nil {
			jsonReply(w, map[string]any{"Id": id, "Client": client, "CustomPrefs": map[string]string{}, "ScrollDirection": "Horizontal", "SortOrder": "Ascending"})
			return
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_, _ = w.Write(body)
		return
	}
	object := map[string]any{}
	if body != nil && (json.Unmarshal(body, &object) != nil || object == nil) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_, _ = w.Write(body)
		return
	}
	for key, value := range jellyfinDisplayDefaults(id, client) {
		if _, ok := object[key]; !ok {
			object[key] = value
		}
	}
	if prefs, ok := object["CustomPrefs"].(map[string]any); !ok || prefs == nil {
		object["CustomPrefs"] = map[string]any{}
	}
	jsonReply(w, object)
}
