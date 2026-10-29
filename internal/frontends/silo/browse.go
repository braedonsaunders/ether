package silo

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"github.com/braedonsaunders/ether/internal/frontend/kit/caps"

	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/kit/ids"
)

func titleDTO(t frontend.Title, state frontend.WatchStatus) map[string]any {
	id := ids.RatingKeyGUID(t.Key)
	kind := map[frontend.MediaType]string{"movie": "movie", "show": "series", "season": "season", "episode": "episode"}[t.Type]
	out := map[string]any{"content_id": id, "title": t.Title, "type": kind, "overview": t.Summary, "year": t.Year, "runtime": int(t.Runtime.Minutes()), "genres": nonNil(t.Genres), "keywords": nonNil(t.Keywords), "studios": nonNil(t.Studios), "rating_tmdb": t.Rating, "status": "matched", "user_state": map[string]any{"played": state.ViewCount > 0, "is_favorite": state.IsFavorite, "in_watchlist": state.InWatchlist, "not_interested": false}}
	if t.Type == "movie" || t.Type == "episode" {
		out["play_content_id"] = id
	}
	if state.ViewOffset > 0 {
		out["position_seconds"] = state.ViewOffset.Seconds()
	}
	if state.Duration > 0 {
		out["duration_seconds"] = state.Duration.Seconds()
	}
	if t.ShowKey != 0 {
		out["series_id"] = ids.RatingKeyGUID(t.ShowKey)
		out["series_title"] = t.ShowTitle
	}
	if t.Type == "season" || t.Type == "episode" {
		out["season_number"] = t.Season
	}
	if t.Type == "episode" {
		out["episode_number"] = t.Episode
	}
	return out
}
func nonNil(v []string) []string {
	if v == nil {
		return []string{}
	}
	return v
}
func (i *instance) views(w http.ResponseWriter, r *http.Request) {
	v, _, err := i.viewer(r)
	if err != nil || v == nil {
		http.Error(w, "Unauthorized", 401)
		return
	}
	if i.host.Experience == nil {
		http.Error(w, "Unavailable", 503)
		return
	}
	views, err := i.host.Experience.Views(r.Context(), v)
	if err != nil {
		http.Error(w, "Unavailable", 503)
		return
	}
	items := make([]any, 0, len(views))
	for n, view := range views {
		kind := "movies"
		if view.Type == "show" {
			kind = "series"
		}
		items = append(items, map[string]any{"id": view.ID, "name": view.Name, "type": kind, "sort_order": n + 1})
	}
	reply(w, 200, map[string]any{"items": items})
}
func (i *instance) sections(w http.ResponseWriter, r *http.Request) {
	v, credential, err := i.viewer(r)
	if err != nil || v == nil {
		http.Error(w, "Unauthorized", 401)
		return
	}
	if i.host.Experience == nil {
		http.Error(w, "Unavailable", 503)
		return
	}
	placement := frontend.PlacementHome
	view := r.PathValue("library")
	if view != "" {
		placement = frontend.PlacementLibrary
	}
	limit := 0
	if text := r.URL.Query().Get("item_limit"); text != "" {
		limit, err = strconv.Atoi(text)
		if err != nil || limit < 1 || limit > 100 {
			http.Error(w, "Bad Request", 400)
			return
		}
	}
	rows, err := i.host.Experience.Rows(r.Context(), frontend.RowRequest{Viewer: v, Device: device(r), Placement: placement, ViewID: view, Limit: limit})
	if err != nil {
		http.Error(w, "Unavailable", 503)
		return
	}
	sections := make([]any, 0, len(rows))
	seen := map[string]int{}
	used := map[string]bool{}
	for _, row := range rows {
		titles := make([]frontend.Title, 0)
		if len(row.Entries) > 0 {
			for _, entry := range row.Entries {
				if entry.Kind == frontend.RowEntryTitle && entry.Title != nil {
					titles = append(titles, *entry.Title)
				}
			}
		} else {
			titles = append(titles, row.Items...)
		}
		keys := make([]frontend.RatingKey, 0, len(titles))
		for _, t := range titles {
			keys = append(keys, t.Key)
		}
		states, e := i.host.Experience.UserData(r.Context(), v, keys)
		if e != nil {
			http.Error(w, "Unavailable", 503)
			return
		}
		items := make([]any, 0, len(titles))
		for _, t := range titles {
			item := titleDTO(t, states[t.Key])
			i.artwork(r, v, credential, t.Key, item)
			items = append(items, item)
		}
		kind := map[frontend.RowPurpose]string{frontend.RowContinue: "continue_watching", frontend.RowNextUp: "returning_shows", frontend.RowRecentlyReleased: "recently_added", frontend.RowRecommendation: "recommended_for_you", frontend.RowWatchlist: "watchlist", frontend.RowCollection: "collection", frontend.RowLive: "live"}[row.Purpose]
		if kind == "" {
			kind = "custom"
		}
		id := row.ID
		if id == "" {
			id = "row"
		}
		base := id
		for used[id] {
			seen[base]++
			id = base + ":" + strconv.Itoa(seen[base])
		}
		used[id] = true
		section := map[string]any{"id": id, "title": row.Title, "section_type": kind, "customized": false, "featured": row.Promoted, "is_custom": kind == "custom", "item_limit": len(items), "items": items}
		if !row.More {
			section["total_count"] = len(items)
		}
		sections = append(sections, section)
	}
	reply(w, 200, map[string]any{"sections": sections})
}
func (i *instance) detail(w http.ResponseWriter, r *http.Request) {
	v, credential, err := i.viewer(r)
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
	out := titleDTO(d.Title, d.UserData)
	cast := make([]any, 0, len(d.Title.Cast))
	for n, p := range d.Title.Cast {
		cast = append(cast, map[string]any{"name": p.Name, "character": p.Role, "order": n})
	}
	crew := make([]any, 0, len(d.Title.Crew)+len(d.Title.Directors)+len(d.Title.Writers))
	for _, people := range [][]frontend.Person{d.Title.Crew, d.Title.Directors, d.Title.Writers} {
		for _, p := range people {
			crew = append(crew, map[string]any{"name": p.Name, "job": p.Role})
		}
	}
	out["cast"] = cast
	out["crew"] = crew
	out["versions"] = []any{}
	out["ratings"] = []any{}
	out["subtitles"] = []any{}
	i.artwork(r, v, credential, key, out)
	if i.host.Sessions != nil && i.host.Capabilities != nil {
		dev := device(r)
		capability, e := i.host.Capabilities.Build(i.identityContext(r), frontend.CapabilityRequest{Viewer: v, Device: caps.DeviceInfo{Protocol: "silo", ClientID: dev.ID, Product: dev.Client, Version: dev.Version}, UserAgent: r.UserAgent()})
		if e == nil {
			plan, e := i.host.Sessions.Plan(r.Context(), frontend.PlanRequest{Viewer: v, Device: dev, Protocol: "silo", Item: key, Caps: capability.Client, Intent: frontend.PlayIntent{Estimate: true, Options: frontend.DecideOptions{DirectPlay: true, DirectStream: true, AudioStream: -1, SubtitleStream: -1}}})
			if e == nil {
				versions := d.Versions
				if len(versions) == 0 {
					added := d.Title.UpdatedAt
					if added.IsZero() {
						added = d.Title.Released
					}
					versions = []frontend.Version{{Media: plan.Media, AddedAt: added}}
				}
				items := make([]any, 0, len(versions))
				defaultID := ""
				for _, version := range versions {
					ref, _ := json.Marshal(fileReference{Key: int64(key), Version: version.ID})
					file, e := i.host.IDs.Compact(r.Context(), "silo", "file", string(ref))
					if e != nil {
						http.Error(w, "Unavailable", 503)
						return
					}
					id := strconv.FormatInt(int64(file), 10)
					if defaultID == "" {
						defaultID = id
					}
					if version.AddedAt.IsZero() {
						http.Error(w, "Version timestamp unavailable", 503)
						return
					}
					resolution, video, audio := "", "", ""
					hdr := false
					for _, stream := range version.Media.Streams {
						if stream.Type == "video" {
							video = stream.Codec
							if stream.Height > 0 {
								resolution = strconv.Itoa(stream.Height) + "p"
							}
							hdr = stream.ColorTransfer == "smpte2084" || stream.ColorTransfer == "arib-std-b67" || stream.DolbyVision
						}
						if stream.Type == "audio" && audio == "" {
							audio = stream.Codec
						}
					}
					items = append(items, map[string]any{"file_id": id, "file_name": version.Name, "resolution": resolution, "codec_video": video, "codec_audio": audio, "hdr": hdr, "container": version.Media.Container, "duration": int64(version.Media.Duration.Seconds()), "file_size": version.Media.Size, "bitrate": version.Media.BitrateKbps, "added_at": version.AddedAt.UTC().Format("2006-01-02T15:04:05.000Z")})
				}
				out["versions"] = items
				out["playback_variants"] = []any{map[string]any{"variant_id": "default", "default_file_id": defaultID, "part_count": 1, "parts": []any{map[string]any{"part_index": 1, "default_file_id": defaultID, "versions": items}}}}

			}
		}
	}
	if strings.HasPrefix(r.URL.Path, "/api/v2/watch/") {
		watch := map[string]any{"content_id": out["content_id"], "type": out["type"], "title": out["title"], "versions": []any{}, "subtitles": []any{}}
		for _, name := range []string{"overview", "year", "poster_url", "backdrop_url", "series_id", "series_title", "season_number", "episode_number"} {
			if value, ok := out[name]; ok {
				watch[name] = value
			}
		}
		versions := []any{}
		for _, entry := range out["versions"].([]any) {
			source := entry.(map[string]any)
			item := map[string]any{}
			for k, v := range source {
				if k != "duration" {
					item[k] = v
				}
			}
			item["duration_seconds"] = source["duration"]
			item["marker_segments"] = []any{}
			item["trickplay_available"] = false
			versions = append(versions, item)
		}
		watch["versions"] = versions
		if profile, e := i.profileData(r, v); e == nil {
			for _, pair := range [][2]string{{"subtitle_language", "effective_subtitle_language"}, {"subtitle_mode", "effective_subtitle_mode"}, {"show_forced_subtitles", "effective_show_forced_subtitles"}} {
				if value, ok := profile[pair[0]]; ok {
					watch[pair[1]] = value
				}
			}
		}
		if len(versions) > 0 {
			defaultID := versions[0].(map[string]any)["file_id"]
			watch["playback_variants"] = []any{map[string]any{"variant_id": "default", "part_count": 1, "default_file_id": defaultID, "parts": []any{map[string]any{"part_index": 1, "default_file_id": defaultID, "versions": versions}}}}
		}
		for _, marker := range d.Markers {
			name := map[string]string{"intro": "intro", "credits": "credits", "recap": "recap"}[marker.Type]
			if name != "" {
				watch[name] = map[string]any{"start_seconds": marker.Start.Seconds(), "end_seconds": marker.End.Seconds()}
			}
		}
		st := d.UserData
		if st.ViewOffset > 0 || st.ViewCount > 0 {
			inProgress := 0
			if st.ViewOffset > 0 {
				inProgress = 1
			}
			watch["user_data"] = map[string]any{"played": st.ViewCount > 0, "watched_count": st.ViewCount, "unplayed_count": 0, "in_progress_count": inProgress, "is_in_progress": st.ViewOffset > 0, "position_seconds": st.ViewOffset.Seconds(), "duration_seconds": st.Duration.Seconds()}
		}
		reply(w, 200, watch)
		return
	}
	if strings.HasSuffix(r.URL.Path, "/versions") {
		reply(w, 200, map[string]any{"items": out["versions"], "page": map[string]any{"has_more": false, "next_cursor": nil}})
		return
	}
	reply(w, 200, out)
}
func (i *instance) homeCapabilities(w http.ResponseWriter, r *http.Request) {
	v, _, err := i.viewer(r)
	if err != nil || v == nil {
		http.Error(w, "Unauthorized", 401)
		return
	}
	reply(w, 200, map[string]any{"state": "available", "allowed": true, "section_item_limit_max": 100})
}
func (i *instance) browseRoutes() error {
	for _, entry := range []struct {
		path string
		h    http.HandlerFunc
	}{{"/api/v2/home/capabilities", i.homeCapabilities}, {"/api/v2/user/libraries", i.views}, {"/api/v2/home/sections", i.sections}, {"/api/v2/library/{library}/sections", i.sections}, {"/api/v2/watch/{id}", i.detail}, {"/api/v2/catalog/items/{id}/versions", i.detail}, {"/api/v2/catalog/items/{id}", i.detail}} {
		if err := i.router.Handle("GET", entry.path, entry.h); err != nil {
			return err
		}
	}
	return nil
}
