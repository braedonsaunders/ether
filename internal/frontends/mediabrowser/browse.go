package mediabrowser

import (
	"net/http"
	"strconv"

	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/kit/ids"
	"github.com/braedonsaunders/ether/internal/frontend/kit/paging"
	"github.com/braedonsaunders/ether/internal/frontend/kit/route"
)

func queryInt(r *http.Request, name string, def int) int {
	v := route.Query(r.URL.Query(), name)
	if len(v) != 1 {
		return def
	}
	n, e := strconv.Atoi(v[0])
	if e != nil || n < 0 {
		return def
	}
	return n
}
func itemDTO(t frontend.Title, state frontend.WatchStatus) map[string]any {
	kind := map[frontend.MediaType]string{"movie": "Movie", "show": "Series", "season": "Season", "episode": "Episode"}[t.Type]
	m := map[string]any{"Id": ids.RatingKeyGUID(t.Key), "Name": t.Title, "Type": kind, "LocationType": "FileSystem", "IsFolder": t.Type == "show" || t.Type == "season", "Overview": t.Summary, "ProductionYear": t.Year, "RunTimeTicks": t.Runtime.Nanoseconds() / 100, "UserData": map[string]any{"Key": ids.RatingKeyGUID(t.Key), "PlaybackPositionTicks": state.ViewOffset.Nanoseconds() / 100, "PlayCount": state.ViewCount, "Played": state.ViewCount > 0, "Rating": state.UserRating}}
	if t.ShowKey != 0 {
		m["SeriesId"] = ids.RatingKeyGUID(t.ShowKey)
		m["SeriesName"] = t.ShowTitle
	}
	if t.Type == "episode" {
		m["ParentIndexNumber"] = t.Season
		m["IndexNumber"] = t.Episode
	}
	if t.Type == "season" {
		m["IndexNumber"] = t.Season
	}
	return m
}
func (i *instance) browseRoutes() error {
	add := func(method, path string, h http.HandlerFunc) error { return i.router.Handle(method, path, h) }
	views := func(w http.ResponseWriter, r *http.Request) {
		v, _, err := i.viewer(r)
		if err != nil || v == nil {
			http.Error(w, "Unauthorized", 401)
			return
		}
		if i.host.Experience == nil {
			http.Error(w, "Unavailable", 503)
			return
		}
		vs, err := i.host.Experience.Views(r.Context(), v)
		if err != nil {
			http.Error(w, "Unavailable", 503)
			return
		}
		items := make([]any, 0, len(vs))
		for _, view := range vs {
			viewNum, e := ids.ParseDecimal(view.ID)
			if e != nil {
				continue
			}
			viewID, e := ids.GUID(ids.View, viewNum)
			if e != nil {
				continue
			}
			kind := "movies"
			if view.Type == "show" {
				kind = "tvshows"
			}
			items = append(items, map[string]any{"Id": viewID, "Name": view.Name, "Type": "CollectionFolder", "CollectionType": kind, "IsFolder": true})
		}
		jsonReply(w, map[string]any{"Items": items, "TotalRecordCount": len(items), "StartIndex": 0})
	}
	rowHandler := func(purpose frontend.RowPurpose, asArray bool) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			v, _, err := i.viewer(r)
			if err != nil || v == nil {
				http.Error(w, "Unauthorized", 401)
				return
			}
			if i.host.Experience == nil {
				http.Error(w, "Unavailable", 503)
				return
			}
			rows, err := i.host.Experience.Rows(r.Context(), frontend.RowRequest{Viewer: v, Device: device(r), Placement: frontend.PlacementHome})
			if err != nil {
				http.Error(w, "Unavailable", 503)
				return
			}
			titles := []frontend.Title{}
			for _, row := range rows {
				if row.Purpose != purpose {
					continue
				}
				if len(row.Entries) > 0 {
					for _, entry := range row.Entries {
						if entry.Kind == frontend.RowEntryTitle && entry.Title != nil {
							titles = append(titles, *entry.Title)
						}
					}
				} else {
					titles = append(titles, row.Items...)
				}
			}
			offset := queryInt(r, "StartIndex", 0)
			limit := queryInt(r, "Limit", 50)
			page := paging.Slice(titles, offset, limit)
			keys := make([]frontend.RatingKey, 0, len(page))
			for _, title := range page {
				keys = append(keys, title.Key)
			}
			state, err := i.host.Experience.UserData(r.Context(), v, keys)
			if err != nil {
				http.Error(w, "Unavailable", 503)
				return
			}
			items := make([]any, 0, len(page))
			for _, title := range page {
				items = append(items, itemDTO(title, state[title.Key]))
			}
			if asArray {
				jsonReply(w, items)
			} else {
				jsonReply(w, map[string]any{"Items": items, "TotalRecordCount": len(titles), "StartIndex": offset})
			}
		}
	}
	detail := func(w http.ResponseWriter, r *http.Request) {
		v, _, err := i.viewer(r)
		if err != nil || v == nil {
			http.Error(w, "Unauthorized", 401)
			return
		}
		key, err := ids.ParseRatingKeyGUID(r.PathValue("id"))
		if err != nil {
			http.NotFound(w, r)
			return
		}
		if i.host.Experience == nil {
			http.Error(w, "Unavailable", 503)
			return
		}
		d, err := i.host.Experience.Item(r.Context(), frontend.ItemRequest{Viewer: v, Device: device(r), Key: key})
		if err != nil {
			http.NotFound(w, r)
			return
		}
		jsonReply(w, itemDTO(d.Title, d.UserData))
	}
	endpoints := []struct {
		path    string
		handler http.HandlerFunc
	}{{"/UserViews", views}, {"/Users/{userId}/Views", views}, {"/Items/Latest", rowHandler(frontend.RowRecentlyReleased, true)}, {"/Users/{userId}/Items/Latest", rowHandler(frontend.RowRecentlyReleased, true)}, {"/UserItems/Resume", rowHandler(frontend.RowContinue, false)}, {"/Users/{userId}/Items/Resume", rowHandler(frontend.RowContinue, false)}, {"/Shows/NextUp", rowHandler(frontend.RowNextUp, false)}, {"/Items/{id}", detail}, {"/Users/{userId}/Items/{id}", detail}}
	for _, e := range endpoints {
		if err := add("GET", e.path, e.handler); err != nil {
			return err
		}
	}
	return nil
}
