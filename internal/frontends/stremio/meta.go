package stremio

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/kit/ids"
)

func (i *instance) meta(w http.ResponseWriter, r *http.Request) {
	credential := r.PathValue("credential")
	viewer, err := i.host.Identity.Resolve(r.Context(), credential)
	if err != nil || viewer == nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	typ, raw := r.PathValue("type"), r.PathValue("id")
	if !strings.HasSuffix(raw, ".json") {
		http.NotFound(w, r)
		return
	}
	id, err := ids.ParseStremio(strings.TrimSuffix(raw, ".json"))
	if err != nil || id.HasEpisode {
		http.NotFound(w, r)
		return
	}
	key, err := ids.StremioTMDBKey(typ, id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	device := deviceFor(credential)
	detail, err := i.host.Experience.Item(r.Context(), frontend.ItemRequest{Viewer: viewer, Device: device, Key: key})
	if err != nil {
		http.NotFound(w, r)
		return
	}
	title := detail.Title
	meta := map[string]any{"id": strings.TrimSuffix(raw, ".json"), "type": typ, "name": title.Title, "description": title.Summary, "releaseInfo": strconv.Itoa(title.Year), "genres": title.Genres}
	if poster := i.artwork(r, credential, device, []frontend.Title{title})[title.Key]; poster != "" {
		meta["poster"] = poster
	}
	if typ == "series" {
		page, e := i.host.Experience.Children(r.Context(), viewer, device, key, frontend.ItemQuery{Limit: 1000, Recursive: true})
		if e != nil {
			http.Error(w, "Unavailable", http.StatusServiceUnavailable)
			return
		}
		videos := make([]map[string]any, 0, len(page.Items))
		for _, episode := range page.Items {
			if string(episode.Type) != "episode" {
				continue
			}
			episodeID, e := ids.FormatStremio(ids.StremioID{Source: "tmdb", ID: strconv.FormatInt(key.TMDB(), 10), HasEpisode: true, Season: episode.Season, Episode: episode.Episode})
			if e != nil {
				continue
			}
			videos = append(videos, map[string]any{"id": episodeID, "title": episode.Title, "season": episode.Season, "episode": episode.Episode, "overview": episode.Summary})
		}
		meta["videos"] = videos
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"meta": meta})
}
