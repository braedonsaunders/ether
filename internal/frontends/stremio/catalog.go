package stremio

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/braedonsaunders/ether/internal/frontend"
)

func deviceFor(credential string) frontend.Device {
	h := sha256.Sum256([]byte(credential))
	return frontend.Device{ID: "stremio-" + hex.EncodeToString(h[:16]), Client: "Stremio", Product: "Stremio"}
}

func (i *instance) catalog(w http.ResponseWriter, r *http.Request) {
	credential := r.PathValue("credential")
	viewer, err := i.host.Identity.Resolve(r.Context(), credential)
	if err != nil || viewer == nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	typ, id := r.PathValue("type"), r.PathValue("id")
	catalogID := strings.TrimSuffix(id, ".json")
	purpose := map[string]frontend.RowPurpose{"ether-continue": frontend.RowContinue, "ether-recent": frontend.RowRecentlyReleased, "ether-recommendations": frontend.RowRecommendation}[catalogID]
	if (typ != "movie" && typ != "series") || (purpose == "" && !((typ == "movie" && catalogID == "ether-movies") || (typ == "series" && catalogID == "ether-series"))) {
		http.NotFound(w, r)
		return
	}
	extra := r.URL.Query()
	if raw := r.PathValue("extra"); raw != "" {
		if !strings.HasSuffix(raw, ".json") {
			http.NotFound(w, r)
			return
		}
		extra, err = url.ParseQuery(strings.TrimSuffix(raw, ".json"))
		if err != nil {
			http.Error(w, "Bad Request", http.StatusBadRequest)
			return
		}
	} else if !strings.HasSuffix(id, ".json") {
		http.NotFound(w, r)
		return
	}
	q := frontend.ItemQuery{Limit: 100}
	if skip := extra.Get("skip"); skip != "" {
		q.Offset, err = strconv.Atoi(skip)
		if err != nil || q.Offset < 0 || q.Offset > 10000 {
			http.Error(w, "Bad Request", http.StatusBadRequest)
			return
		}
	}
	if genre := extra.Get("genre"); genre != "" {
		q.Filter.Genres = []string{genre}
	}
	device := deviceFor(credential)
	var titles []frontend.Title
	if purpose != "" {
		var rows []frontend.Row
		rows, err = i.host.Experience.Rows(r.Context(), frontend.RowRequest{Viewer: viewer, Device: device, Placement: frontend.PlacementHome, Limit: 100})
		if err == nil {
			seen := map[frontend.RatingKey]bool{}
			for _, row := range rows {
				if row.Purpose != purpose {
					continue
				}
				items := row.Items
				if len(row.Entries) > 0 {
					items = nil
					for _, entry := range row.Entries {
						if entry.Kind == frontend.RowEntryTitle && entry.Title != nil {
							items = append(items, *entry.Title)
						}
					}
				}
				for _, title := range items {
					if typ == "series" && string(title.Type) == "episode" && title.ShowKey != 0 {
						detail, e := i.host.Experience.Item(r.Context(), frontend.ItemRequest{Viewer: viewer, Device: device, Key: title.ShowKey})
						if e != nil {
							continue
						}
						title = detail.Title
					}
					if string(title.Type) != map[string]string{"movie": "movie", "series": "show"}[typ] || seen[title.Key] {
						continue
					}
					seen[title.Key] = true
					titles = append(titles, title)
				}
			}
			if q.Offset >= len(titles) {
				titles = nil
			} else {
				titles = titles[q.Offset:]
				if len(titles) > q.Limit {
					titles = titles[:q.Limit]
				}
			}
		}
	} else if query := extra.Get("search"); query != "" {
		q.Types = []frontend.MediaType{frontend.MediaType(map[string]string{"movie": "movie", "series": "show"}[typ])}
		page, e := i.host.Experience.Search(r.Context(), viewer, device, query, q)
		err, titles = e, page.Page.Items
	} else {
		var views []frontend.View
		views, err = i.host.Experience.Views(r.Context(), viewer)
		if err == nil {
			offset := q.Offset
			for _, view := range views {
				if string(view.Type) != map[string]string{"movie": "movie", "series": "show"}[typ] {
					continue
				}
				query := q
				query.Offset = offset
				query.Limit = q.Limit - len(titles)
				var page frontend.Page
				page, err = i.host.Experience.LibraryItems(r.Context(), viewer, device, view.ID, query)
				if err != nil {
					break
				}
				titles = append(titles, page.Items...)
				if len(titles) >= q.Limit {
					break
				}
				offset = max(0, offset-page.Total)
			}
		}
	}
	if err != nil {
		http.Error(w, "Unavailable", http.StatusServiceUnavailable)
		return
	}
	metas := make([]map[string]any, 0, len(titles))
	posters := i.artwork(r, credential, device, titles)
	for _, title := range titles {
		if title.IDs.TMDB <= 0 {
			continue
		}
		id := "tmdb:" + strconv.FormatInt(title.IDs.TMDB, 10)
		meta := map[string]any{"id": id, "type": typ, "name": title.Title, "description": title.Summary}
		if title.Year > 0 {
			meta["releaseInfo"] = strconv.Itoa(title.Year)
		}
		if poster := posters[title.Key]; poster != "" {
			meta["poster"] = poster
		}
		metas = append(metas, meta)
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"metas": metas})
}
