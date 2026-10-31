package silo

import (
	"encoding/base64"
	"encoding/json"
	"errors"
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
	cards := i.catalogCards(r, v, token, page.Items)
	editions, batched := i.pageEditions(r, v, keys)
	items := make([]any, 0, len(page.Items))
	for _, t := range page.Items {
		item := titleDTO(t, state[t.Key])
		if granted, ok := cards[t.Key]; ok {
			paintCatalogArtwork(r, granted, item)
		} else {
			i.artwork(r, v, token, t.Key, item)
		}
		if batched {
			if summary := bestSiloOverlay(editions[t.Key]); len(summary) > 0 {
				item["overlay_summary"] = summary
			}
		} else {
			i.applyEditionOverlay(r, v, t.Key, item)
		}
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
	episode := r.PathValue("season") != "" || r.PathValue("all") != ""
	limit, offset := 0, 0
	if episode {
		limit = 50
		if x := r.URL.Query().Get("limit"); x != "" {
			n, e := strconv.Atoi(x)
			if e != nil || n < 1 || n > 100 {
				http.Error(w, "Bad Request", 400)
				return
			}
			limit = n
		}
		n, e := decodeEpisodeCursor(r.URL.Query().Get("cursor"))
		if e != nil {
			http.Error(w, "Bad Request", 400)
			return
		}
		offset = n
	}
	query := frontend.ItemQuery{Limit: limit, Offset: offset}
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
		if r.PathValue("season") != "" {
			reply(w, 200, map[string]any{"items": []any{}, "page": map[string]any{"has_more": false}})
			return
		}
		http.Error(w, "Unavailable", 503)
		return
	}
	if raw := r.PathValue("season"); raw != "" {
		n, _ := strconv.Atoi(raw)
		kept := make([]frontend.Title, 0, len(page.Items))
		for _, t := range page.Items {
			if t.Type == frontend.MediaType("episode") && t.Season == n {
				kept = append(kept, t)
			}
		}
		if len(kept) != len(page.Items) {
			page.Items = kept
			if len(kept) == 0 {
				page.Total = 0
			}
		}
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
	overlays, batched := i.editionOverlays(r, v, keys)
	artwork := i.sectionArtwork(r, v, token, page.Items)
	items := make([]any, 0, len(page.Items))
	for _, t := range page.Items {
		item := titleDTO(t, states[t.Key])
		item["season_number"] = t.Season
		item["episode_number"] = t.Episode
		item["is_specials"] = t.Season == 0
		if t.Type == "season" {
			item["episode_count"] = t.ChildCount
		} else {
			item["episode_count"] = t.LeafCount
		}
		i.paintCardArtwork(r, artwork[t.Key], item)
		if t.Type == "episode" && (t.StillPath != "" || t.PosterPath != "" || t.BackdropPath != "") {
			item["poster_is_episode_still"] = t.StillPath != "" && t.PosterPath == t.StillPath
			item["backdrop_is_episode_still"] = t.StillPath != "" && t.BackdropPath == t.StillPath
		}
		if t.Type == "episode" {
			if still, ok := item["still_url"].(string); ok && still != "" {
				if _, hasPoster := item["poster_url"]; !hasPoster {
					item["poster_url"] = still
					item["poster_is_episode_still"] = true
				}
			}
		}
		if summary := overlays[t.Key]; len(summary) > 0 {
			item["overlay_summary"] = summary
		} else if !batched {
			i.applyEditionOverlay(r, v, t.Key, item)
		}
		items = append(items, item)
	}
	more := limit > 0 && page.Total > offset+limit
	info := map[string]any{"has_more": more}
	if more {
		info["next_cursor"] = encodeEpisodeCursor(offset + limit)
	}
	reply(w, 200, map[string]any{"items": items, "page": info})
}

func decodeEpisodeCursor(s string) (int, error) {
	if s == "" {
		return 0, nil
	}
	if n, err := strconv.Atoi(s); err == nil {
		if n < 0 {
			return 0, errors.New("cursor")
		}
		return n, nil
	}
	raw, err := base64.RawStdEncoding.DecodeString(s)
	if err != nil {
		raw, err = base64.StdEncoding.DecodeString(s)
	}
	if err != nil {
		return 0, err
	}
	var c struct {
		Offset int `json:"offset"`
	}
	if json.Unmarshal(raw, &c) != nil || c.Offset < 0 {
		return 0, errors.New("cursor")
	}
	return c.Offset, nil
}

func encodeEpisodeCursor(offset int) string {
	raw, err := json.Marshal(struct {
		Offset int `json:"offset"`
	}{Offset: offset})
	if err != nil {
		return ""
	}
	return base64.RawStdEncoding.EncodeToString(raw)
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

// catalogCards signs a catalogue page's movie and series artwork in one
// stateless pass, as the home rows do, instead of loading each title's
// detail and writing a grant per image. An episode, or a title the signer
// could not list, keeps the per-title path.
func (i *instance) catalogCards(r *http.Request, v *frontend.Viewer, token string, titles []frontend.Title) map[frontend.RatingKey][]frontend.GrantedImage {
	if _, ok := i.host.ImageGrants.(frontend.ImagePageSigner); !ok {
		return nil
	}
	cards := make([]frontend.Title, 0, len(titles))
	for _, t := range titles {
		if t.Type == "movie" || t.Type == "show" {
			cards = append(cards, t)
		}
	}
	return i.sectionArtwork(r, v, token, cards)
}

// paintCatalogArtwork writes the fields artwork writes for a list card: the
// first signed image of each kind and its thumbhash.
func paintCatalogArtwork(r *http.Request, granted []frontend.GrantedImage, out map[string]any) {
	prefix := ""
	if r.Context().Value(sharedPrefix{}) != nil {
		prefix = "/silo"
	}
	fields := map[string]string{frontend.ImagePoster: "poster_url", frontend.ImageBackdrop: "backdrop_url", frontend.ImageLogo: "logo_url"}
	hashes := map[string]string{frontend.ImagePoster: "poster_thumbhash", frontend.ImageBackdrop: "backdrop_thumbhash"}
	for _, grant := range granted {
		field := fields[grant.Image.Kind]
		if field == "" || grant.Secret == "" || grant.Image.Path == "" {
			continue
		}
		if _, exists := out[field]; exists {
			continue
		}
		out[field] = prefix + "/api/v2/images/" + grant.Secret
		if hash := hashes[grant.Image.Kind]; hash != "" && grant.Image.ThumbHash != "" {
			out[hash] = grant.Image.ThumbHash
		}
	}
}

// pageEditions reads a page's editions in one batch when the host can.
func (i *instance) pageEditions(r *http.Request, v *frontend.Viewer, keys []frontend.RatingKey) (map[frontend.RatingKey][]frontend.Version, bool) {
	batch, ok := i.host.Editions.(frontend.EditionBatch)
	if !ok || len(keys) == 0 {
		return nil, false
	}
	versions, err := batch.EditionsMany(r.Context(), v, device(r), keys)
	if err != nil {
		return nil, false
	}
	return versions, true
}
