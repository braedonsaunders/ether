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
		jsonReply(w, map[string]any{"Id": key, "Client": client[0], "CustomPrefs": map[string]string{}, "ScrollDirection": "Horizontal", "SortOrder": "Ascending"})
		return
	}
	if err != nil {
		http.Error(w, "Unavailable", 503)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_, _ = w.Write(body)
}
