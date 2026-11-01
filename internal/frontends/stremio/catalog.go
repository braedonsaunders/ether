package stremio

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/braedonsaunders/ether/internal/frontend"
	kitsort "github.com/braedonsaunders/ether/internal/frontend/kit/sort"
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
	i.observeClient(credential)
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
	fresh, cacheKey := i.catalogCacheKey(viewer, typ, catalogID, extra, q)
	if body, ok := i.cachedCatalog(cacheKey, fresh); ok {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
		return
	}
	var titles []frontend.Title
	if purpose == frontend.RowRecentlyReleased {
		q.Sort, _ = kitsort.Order("PremiereDate")
		q.Desc = true
		titles, err = i.libraryPage(r, viewer, device, typ, q)
	} else if purpose != "" {
		var rows []frontend.Row
		rows, err = i.homeRows(r.Context(), frontend.RowRequest{Viewer: viewer, Device: device, Placement: frontend.PlacementHome, Purpose: purpose, Limit: 100})
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
					if typ == "series" {
						promoted, ok := promoteSeries(title)
						if !ok {
							detail, e := i.host.Experience.Item(r.Context(), frontend.ItemRequest{Viewer: viewer, Device: device, Key: title.ShowKey})
							if e != nil {
								continue
							}
							title = detail.Title
						} else {
							title = promoted
						}
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
		titles, err = i.libraryPage(r, viewer, device, typ, q)
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
	var body bytes.Buffer
	if err := json.NewEncoder(&body).Encode(map[string]any{"metas": metas}); err != nil {
		http.Error(w, "Unavailable", http.StatusServiceUnavailable)
		return
	}
	i.storeCatalog(cacheKey, fresh, body.Bytes())
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(body.Bytes())
}

func (i *instance) libraryPage(r *http.Request, viewer *frontend.Viewer, device frontend.Device, typ string, q frontend.ItemQuery) ([]frontend.Title, error) {
	views, err := i.host.Experience.Views(r.Context(), viewer)
	if err != nil {
		return nil, err
	}
	var titles []frontend.Title
	offset := q.Offset
	want := map[string]string{"movie": "movie", "series": "show"}[typ]
	for _, view := range views {
		if string(view.Type) != want {
			continue
		}
		query := q
		query.Offset = offset
		query.Limit = q.Limit - len(titles)
		page, err := i.host.Experience.LibraryItems(r.Context(), viewer, device, view.ID, query)
		if err != nil {
			return nil, err
		}
		titles = append(titles, page.Items...)
		if len(titles) >= q.Limit {
			break
		}
		offset = max(0, offset-page.Total)
	}
	return titles, nil
}

type rowSnapshot interface {
	SnapshotRows(context.Context, frontend.RowRequest) ([]frontend.Row, error)
}

func (i *instance) homeRows(ctx context.Context, req frontend.RowRequest) ([]frontend.Row, error) {
	if snap, ok := i.host.Experience.(rowSnapshot); ok {
		return snap.SnapshotRows(ctx, req)
	}
	return i.host.Experience.Rows(ctx, req)
}

// promoteSeries turns an episode row into its show when the row already
// carries the show identity. Missing show facts fall back to an item read.
func promoteSeries(title frontend.Title) (frontend.Title, bool) {
	if string(title.Type) != "episode" || title.ShowKey == 0 {
		return title, true
	}
	if title.ShowIDs.TMDB <= 0 {
		return title, false
	}
	show := title
	show.Key = title.ShowKey
	show.Type = "show"
	show.IDs = title.ShowIDs
	if title.ShowTitle != "" {
		show.Title = title.ShowTitle
	}
	if title.ShowYear > 0 {
		show.Year = title.ShowYear
	}
	return show, true
}

func (i *instance) catalogCacheKey(viewer *frontend.Viewer, typ, catalogID string, extra url.Values, q frontend.ItemQuery) (frontend.PageFreshness, string) {
	var fresh frontend.PageFreshness
	if i.host.PageFreshness != nil {
		fresh = i.host.PageFreshness()
	}
	genre := ""
	if len(q.Filter.Genres) > 0 {
		genre = q.Filter.Genres[0]
	}
	key := strings.Join([]string{
		strconv.FormatInt(viewer.ID, 10), typ, catalogID, strconv.Itoa(q.Offset), genre, extra.Get("search"),
	}, "|")
	return fresh, key
}

func (i *instance) cachedCatalog(key string, fresh frontend.PageFreshness) ([]byte, bool) {
	if i.host.PageFreshness == nil {
		return nil, false
	}
	i.pages.mu.Lock()
	defer i.pages.mu.Unlock()
	ent, ok := i.pages.entries[key]
	if !ok || ent.fresh != fresh {
		return nil, false
	}
	return append([]byte(nil), ent.body...), true
}

func (i *instance) storeCatalog(key string, fresh frontend.PageFreshness, body []byte) {
	if i.host.PageFreshness == nil {
		return
	}
	i.pages.mu.Lock()
	defer i.pages.mu.Unlock()
	if i.pages.entries == nil {
		i.pages.entries = map[string]catalogPage{}
	}
	if len(i.pages.entries) >= 128 {
		i.pages.entries = map[string]catalogPage{}
	}
	i.pages.entries[key] = catalogPage{fresh: fresh, body: append([]byte(nil), body...)}
}
