package silo

import (
	"context"
	"net/http"
	"strings"

	"github.com/braedonsaunders/ether/internal/frontend"
)

const (
	nowAvailableLibraryID = "now-available"
	nowAvailableRowID     = "watchlist.available"
	nowAvailableTitle     = "Now available"
)

// rowSnapshotter reads the same authorized rows as Experience.Rows without
// recording an impression. Home remains the impression for a Silo visit.
type rowSnapshotter interface {
	SnapshotRows(context.Context, frontend.RowRequest) ([]frontend.Row, error)
}

func nowAvailableExperienceRow(row frontend.Row) bool {
	return row.ID == nowAvailableRowID || row.Title == nowAvailableTitle
}

func onlyNowAvailable(rows []frontend.Row) []frontend.Row {
	out := make([]frontend.Row, 0, 1)
	for _, row := range rows {
		if nowAvailableExperienceRow(row) {
			out = append(out, row)
		}
	}
	return out
}

func (i *instance) snapshotHome(r *http.Request, v *frontend.Viewer, limit int) ([]frontend.Row, error) {
	req := frontend.RowRequest{Viewer: v, Device: device(r), Placement: frontend.PlacementHome, Limit: limit}
	if snap, ok := i.host.Experience.(rowSnapshotter); ok {
		return snap.SnapshotRows(r.Context(), req)
	}
	return i.host.Experience.Rows(r.Context(), req)
}

func (i *instance) nowAvailableTitles(r *http.Request, v *frontend.Viewer) ([]frontend.Title, bool) {
	if i.host.Experience == nil {
		return nil, false
	}
	rows, err := i.snapshotHome(r, v, 50)
	if err != nil {
		return nil, false
	}
	for _, row := range onlyNowAvailable(rows) {
		titles := rowTitles(row)
		if len(titles) > 0 {
			return titles, true
		}
	}
	return nil, false
}

func rowTitles(row frontend.Row) []frontend.Title {
	if len(row.Entries) == 0 {
		return append([]frontend.Title(nil), row.Items...)
	}
	out := make([]frontend.Title, 0, len(row.Entries))
	for _, entry := range row.Entries {
		if entry.Kind == frontend.RowEntryTitle && entry.Title != nil {
			out = append(out, *entry.Title)
		}
	}
	return out
}

func siloSectionType(row frontend.Row, library bool) string {
	if library && siloKeywordRow(row.Title) {
		return "custom_filter"
	}
	if nowAvailableExperienceRow(row) {
		return "new_to_library"
	}
	switch row.Kind {
	case "because-you-watched":
		return "because_you_watched"
	case "trending", "trending-in-your-taste", "trending-with-fans", "popular", "popular-on-server":
		return "trending_on_server"
	case "hidden-gems":
		return "hidden_gems"
	case "fans-also-loved":
		return "similar_users_liked"
	case "favorites":
		return "favorites"
	case "new-releases", "new-this-week", "recently-added":
		return "recently_added"
	}
	kind := map[frontend.RowPurpose]string{
		frontend.RowContinue:         "continue_watching",
		frontend.RowNextUp:           "returning_shows",
		frontend.RowRecentlyReleased: "recently_added",
		frontend.RowRecentlyAdded:    "recently_added",
		frontend.RowRecommendation:   "recommended_for_you",
		frontend.RowWatchlist:        "watchlist",
		frontend.RowCollection:       "collection",
		frontend.RowLive:             "live",
	}[row.Purpose]
	if kind == "" {
		return "custom"
	}
	return kind
}

func discoverType(row frontend.Row) string {
	if nowAvailableExperienceRow(row) {
		return "recently_added"
	}
	switch row.Kind {
	case "fans-also-loved":
		return "similar_users_liked"
	case "trending", "trending-in-your-taste", "trending-with-fans", "popular", "popular-on-server":
		return "popular"
	case "new-releases", "new-this-week", "recently-added":
		return "recently_added"
	case "top-rated", "top-10", "top-10-today", "top-rated-in-genre":
		return "top_rated"
	case "genre-showcase":
		return "genre_sampler"
	default:
		return "cluster"
	}
}

func discoverRow(row frontend.Row) bool {
	switch row.Purpose {
	case frontend.RowContinue, frontend.RowNextUp, frontend.RowLive:
		return false
	}
	if row.ID == "watchlist.coming" || strings.EqualFold(row.Title, "Waiting for a stream") {
		return false
	}
	return len(rowTitles(row)) > 0
}

type sectionPrepKey struct{}

type sectionPrep struct {
	states   map[frontend.RatingKey]frontend.WatchStatus
	artwork  map[frontend.RatingKey][]frontend.GrantedImage
	overlays map[frontend.RatingKey]map[string]any
	batched  bool
}

func (i *instance) sectionCards(r *http.Request, v *frontend.Viewer, credential string, row frontend.Row) ([]any, error) {
	titles := rowTitles(row)
	keys := make([]frontend.RatingKey, 0, len(titles))
	for _, t := range titles {
		keys = append(keys, t.Key)
	}
	var states map[frontend.RatingKey]frontend.WatchStatus
	var artwork map[frontend.RatingKey][]frontend.GrantedImage
	var overlays map[frontend.RatingKey]map[string]any
	var batched bool
	var err error
	if prep, ok := r.Context().Value(sectionPrepKey{}).(sectionPrep); ok {
		states, artwork, overlays, batched = prep.states, prep.artwork, prep.overlays, prep.batched
	} else {
		states, err = i.host.Experience.UserData(r.Context(), v, keys)
		if err != nil {
			return nil, err
		}
		overlays, batched = i.editionOverlays(r, v, keys)
		artwork = i.sectionArtwork(r, v, credential, titles)
	}
	home := !strings.Contains(r.URL.Path, "/library/")
	items := make([]any, 0, len(titles))
	for _, t := range titles {
		item := titleDTO(t, states[t.Key])
		if home {
			item["keywords"] = []string{}
		}
		if row.Purpose == frontend.RowNextUp {
			item["item_source"] = "next_up"
		} else if row.Purpose == frontend.RowContinue && states[t.Key].ViewOffset > 0 {
			item["item_source"] = "in_progress"
		}
		i.paintCardArtwork(r, artwork[t.Key], item)
		if summary := overlays[t.Key]; len(summary) > 0 {
			item["overlay_summary"] = summary
		} else if !batched {
			i.applyEditionOverlay(r, v, t.Key, item)
		}
		items = append(items, item)
	}
	return items, nil
}

func (i *instance) editionOverlays(r *http.Request, v *frontend.Viewer, keys []frontend.RatingKey) (map[frontend.RatingKey]map[string]any, bool) {
	out := map[frontend.RatingKey]map[string]any{}
	if i == nil || i.host.Editions == nil || len(keys) == 0 {
		return out, false
	}
	type many interface {
		EditionsMany(context.Context, *frontend.Viewer, frontend.Device, []frontend.RatingKey) (map[frontend.RatingKey][]frontend.Version, error)
	}
	batch, ok := i.host.Editions.(many)
	if !ok {
		return out, false
	}
	versions, err := batch.EditionsMany(r.Context(), v, device(r), keys)
	if err != nil {
		return out, false
	}
	for key, list := range versions {
		if summary := bestSiloOverlay(list); len(summary) > 0 {
			out[key] = summary
		}
	}
	return out, true
}

func (i *instance) discover(w http.ResponseWriter, r *http.Request) {
	v, credential, err := i.viewer(r)
	if err != nil || v == nil {
		http.Error(w, "Unauthorized", 401)
		return
	}
	if i.host.Experience == nil {
		http.Error(w, "Unavailable", 503)
		return
	}
	rows, err := i.snapshotHome(r, v, 20)
	if err != nil {
		http.Error(w, "Unavailable", 503)
		return
	}
	items := make([]any, 0, len(rows))
	for _, row := range rows {
		if !discoverRow(row) {
			continue
		}
		cards, e := i.sectionCards(r, v, credential, row)
		if e != nil {
			http.Error(w, "Unavailable", 503)
			return
		}
		if len(cards) == 0 {
			continue
		}
		items = append(items, map[string]any{"type": discoverType(row), "title": row.Title, "items": cards})
	}
	reply(w, 200, map[string]any{"items": items})
}
