package mediabrowser

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/kit/ids"
)

func (i *instance) trickplayImage(w http.ResponseWriter, r *http.Request) {
	if i.protocol != "jellyfin" {
		http.NotFound(w, r)
		return
	}
	tiles, ok := i.host.Sessions.(frontend.TrickplayTiles)
	if !ok {
		http.NotFound(w, r)
		return
	}
	viewer, _, err := i.viewer(r)
	if err != nil || viewer == nil {
		http.Error(w, "Unauthorized", 401)
		return
	}
	key, err := ids.ParseRatingKeyGUID(r.PathValue("id"))
	if err != nil || key == 0 {
		http.NotFound(w, r)
		return
	}
	width, err := strconv.Atoi(r.PathValue("width"))
	if err != nil || width <= 0 {
		http.NotFound(w, r)
		return
	}
	name := strings.TrimSuffix(strings.ToLower(r.PathValue("name")), ".jpg")
	index, err := strconv.Atoi(name)
	if err != nil || index < 0 {
		http.NotFound(w, r)
		return
	}
	access := frontend.SessionAccess{Viewer: viewer, Device: device(r), Protocol: i.protocol}
	offer, ok := tiles.TrickplayOffer(r.Context(), access, key)
	if !ok || offer.Width != width {
		http.NotFound(w, r)
		return
	}
	body, err := tiles.TrickplaySheet(r.Context(), access, key, index)
	if err != nil || len(body) == 0 {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	if r.Method == http.MethodHead {
		return
	}
	_, _ = w.Write(body)
}

func (i *instance) attachTrickplay(r *http.Request, viewer *frontend.Viewer, key frontend.RatingKey, dto map[string]any) {
	if i.protocol != "jellyfin" || viewer == nil {
		return
	}
	kind, _ := dto["Type"].(string)
	if kind != "Movie" && kind != "Episode" {
		return
	}
	tiles, ok := i.host.Sessions.(frontend.TrickplayTiles)
	if !ok {
		return
	}
	offer, ok := tiles.TrickplayOffer(i.identityContext(r), frontend.SessionAccess{Viewer: viewer, Device: device(r), Protocol: i.protocol}, key)
	if !ok || offer.Width <= 0 || offer.Count <= 0 || offer.Interval <= 0 {
		return
	}
	sourceID := ids.RatingKeyGUID(key)
	if sources, ok := dto["MediaSources"].([]any); ok && len(sources) > 0 {
		if source, ok := sources[0].(map[string]any); ok {
			if id, ok := source["Id"].(string); ok && id != "" {
				sourceID = id
			}
		}
	}
	dto["Trickplay"] = map[string]any{
		strconv.Itoa(offer.Width): map[string]any{
			sourceID: map[string]any{
				"Width":          offer.Width,
				"Height":         offer.Height,
				"TileWidth":      offer.Columns,
				"TileHeight":     offer.Rows,
				"ThumbnailCount": offer.Count,
				"Interval":       offer.Interval.Milliseconds(),
				"Bandwidth":      offer.Bandwidth,
			},
		},
	}
}
