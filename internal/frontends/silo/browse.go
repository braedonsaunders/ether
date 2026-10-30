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
	out := map[string]any{"content_id": id, "title": t.Title, "type": kind, "overview": t.Summary, "year": t.Year, "runtime": int(t.Runtime.Minutes()), "genres": nonNil(t.Genres), "keywords": nonNil(t.Keywords), "studios": nonNil(t.Studios), "rating_tmdb": t.Rating, "status": "matched", "user_state": map[string]any{"played": state.ViewCount > 0, "is_favorite": state.IsFavorite, "in_watchlist": state.InWatchlist, "not_interested": state.IsNotInterested}}
	if t.ContentRating != "" {
		out["content_rating"] = t.ContentRating
	}
	if t.Language != "" {
		out["original_language"] = t.Language
	}
	if !t.Released.IsZero() {
		out["release_date"] = t.Released.Format("2006-01-02")
	}
	if t.Type == "show" && t.Status != "" {
		out["show_status"] = strings.ReplaceAll(strings.ToLower(t.Status), " ", "_")
	}
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
	// Small or newly configured libraries may not yet have enough history for
	// ranked rows. Keep them discoverable through a policy-scoped browse row.
	if len(rows) == 0 {
		views, e := i.host.Experience.Views(r.Context(), v)
		if e != nil {
			http.Error(w, "Unavailable", 503)
			return
		}
		pageLimit := limit
		if pageLimit == 0 {
			pageLimit = 20
		}
		for _, library := range views {
			if view != "" && library.ID != view {
				continue
			}
			page, e := i.host.Experience.LibraryItems(r.Context(), v, device(r), library.ID, frontend.ItemQuery{Limit: pageLimit})
			if e != nil {
				http.Error(w, "Unavailable", 503)
				return
			}
			keys := make([]frontend.RatingKey, 0, len(page.Items))
			for _, title := range page.Items {
				keys = append(keys, title.Key)
			}
			states, e := i.host.Experience.UserData(r.Context(), v, keys)
			if e != nil {
				http.Error(w, "Unavailable", 503)
				return
			}
			items := make([]frontend.Title, 0, len(page.Items))
			for _, title := range page.Items {
				if !states[title.Key].IsNotInterested {
					items = append(items, title)
				}
			}
			if len(items) > 0 {
				rows = append(rows, frontend.Row{ID: "browse:" + library.ID, Title: library.Name, Purpose: frontend.RowPurpose("browse"), Items: items})
			}
		}
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
			if row.Purpose == frontend.RowNextUp {
				item["item_source"] = "next_up"
			} else if row.Purpose == frontend.RowContinue && states[t.Key].ViewOffset > 0 {
				item["item_source"] = "in_progress"
			}
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
	if d.Title.OriginalTitle != "" {
		out["original_title"] = d.Title.OriginalTitle
	}
	if len(d.Title.Countries) > 0 {
		out["countries"] = d.Title.Countries
	}
	if d.Title.IDs.TMDB > 0 {
		out["tmdb_id"] = strconv.FormatInt(d.Title.IDs.TMDB, 10)
	}
	if d.Title.IDs.IMDB != "" {
		out["imdb_id"] = d.Title.IDs.IMDB
	}
	if d.Title.IDs.TVDB > 0 {
		out["tvdb_id"] = strconv.FormatInt(d.Title.IDs.TVDB, 10)
	}
	portraits := i.creditPortraits(r, v, credential, d)
	cast := make([]any, 0, len(d.Title.Cast))
	for n, p := range d.Title.Cast {
		credit := map[string]any{"name": p.Name, "character": p.Role, "order": n}
		if p.TMDB > 0 {
			credit["tmdb_id"] = strconv.FormatInt(p.TMDB, 10)
			if portrait := portraits[p.TMDB]; portrait.url != "" {
				credit["photo_url"] = portrait.url
				if portrait.thumbHash != "" {
					credit["photo_thumbhash"] = portrait.thumbHash
				}
			}
		}
		cast = append(cast, credit)
	}
	crew := make([]any, 0, len(d.Title.Crew)+len(d.Title.Directors)+len(d.Title.Writers))
	for _, people := range [][]frontend.Person{d.Title.Crew, d.Title.Directors, d.Title.Writers} {
		for _, p := range people {
			credit := map[string]any{"name": p.Name, "job": p.Role}
			if p.TMDB > 0 {
				credit["tmdb_id"] = strconv.FormatInt(p.TMDB, 10)
				if portrait := portraits[p.TMDB]; portrait.url != "" {
					credit["photo_url"] = portrait.url
					if portrait.thumbHash != "" {
						credit["photo_thumbhash"] = portrait.thumbHash
					}
				}
			}
			crew = append(crew, credit)
		}
	}
	out["cast"] = cast
	out["crew"] = crew
	if len(d.Extras) > 0 {
		videos := make([]any, 0, len(d.Extras))
		for _, extra := range d.Extras {
			if extra.Source == "" || extra.SourceID == "" {
				continue
			}
			videos = append(videos, map[string]any{"name": extra.Title, "kind": extra.Kind, "site": strings.ToLower(extra.Source), "site_key": extra.SourceID, "language": extra.Language, "is_official": extra.Official})
		}
		if len(videos) > 0 {
			out["videos"] = videos
		}
	}
	out["versions"] = []any{}
	out["ratings"] = []any{}
	out["subtitles"] = []any{}
	if d.Title.Rating > 0 && d.Title.Rating <= 10 {
		score := d.Title.Rating * 10
		out["ratings"] = []any{map[string]any{"source": "tmdb", "name": "TMDB", "score": score, "display": strconv.FormatFloat(d.Title.Rating, 'f', 1, 64)}}
		source := map[string]any{"source": "tmdb", "score": score}
		if d.Title.VoteCount > 0 {
			source["votes"] = d.Title.VoteCount
		}
		out["rating_sources"] = []any{source}
	}
	if d.Title.Type == "movie" || d.Title.Type == "episode" {
		played := d.UserData.ViewCount > 0
		watched, unplayed, inProgress := 0, 1, 0
		if played {
			watched, unplayed = 1, 0
		}
		if d.UserData.ViewOffset > 0 {
			inProgress = 1
		}
		rollup := map[string]any{"played": played, "watched_count": watched, "unplayed_count": unplayed, "in_progress_count": inProgress, "is_in_progress": inProgress > 0}
		if d.UserData.ViewOffset > 0 {
			rollup["position_seconds"] = d.UserData.ViewOffset.Seconds()
		}
		if d.UserData.Duration > 0 {
			rollup["duration_seconds"] = d.UserData.Duration.Seconds()
		}
		if i.host.LastPlayedSource != nil {
			if source, err := i.host.LastPlayedSource.LastPlayedSource(r.Context(), v, device(r), key); err == nil && source != nil {
				addLastPlayedFacts(rollup, source)
			}
		}
		out["user_data"] = rollup
	}
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
				var defaultDuration int64
				for _, version := range versions {
					if !siloSourceHasKnownHDR10Plus(version.Media) {
						continue
					}
					ref, _ := json.Marshal(fileReference{Key: int64(key), Version: version.ID})
					file, e := i.host.IDs.Compact(r.Context(), "silo", "file", string(ref))
					if e != nil {
						http.Error(w, "Unavailable", 503)
						return
					}
					id := strconv.FormatInt(int64(file), 10)
					if defaultID == "" {
						defaultID = id
						if !version.Media.Estimated && (version.ID != "" || !plan.Estimated) {
							defaultDuration = int64(version.Media.Duration.Seconds())
						}
					}
					if version.AddedAt.IsZero() {
						http.Error(w, "Version timestamp unavailable", 503)
						return
					}
					resolution, video, audio := "", "", ""
					hdr := false
					audioTracks := []any{}
					videoTracks := []any{}
					for _, stream := range version.Media.Streams {
						if stream.Type == "video" {
							video = stream.Codec
							if stream.Height > 0 {
								resolution = strconv.Itoa(stream.Height) + "p"
							}
							rangeName := siloObservedRange(version.Media, stream)
							hdr = rangeName != "" && rangeName != "sdr"
							track := map[string]any{"codec": stream.Codec, "color_range": "unknown", "dv_enhancement_layer": "unknown"}
							if stream.ColorRange != "" {
								track["color_range"] = stream.ColorRange
							}
							if stream.HDR10Plus != nil {
								track["hdr10_plus"] = *stream.HDR10Plus
							} else if stream.ColorTransfer != "smpte2084" {
								track["hdr10_plus"] = false
							}
							if stream.DolbyVisionELPresent != nil && !*stream.DolbyVisionELPresent {
								track["dv_enhancement_layer"] = "none"
							}
							if stream.Width > 0 {
								track["width"] = stream.Width
							}
							if stream.Height > 0 {
								track["height"] = stream.Height
							}
							if stream.BitDepth > 0 {
								track["bit_depth"] = stream.BitDepth
							}
							if stream.BitrateKbps > 0 {
								track["bitrate"] = stream.BitrateKbps
							}
							if stream.Profile != "" {
								track["profile"] = stream.Profile
							}
							if stream.Level > 0 {
								track["level"] = stream.Level
							}
							if stream.Title != "" {
								track["title"] = stream.Title
							}
							if stream.FrameRate > 0 {
								track["frame_rate"] = strconv.FormatFloat(stream.FrameRate, 'f', 3, 64)
							}
							if stream.DisplayAspectRatio != "" {
								track["aspect_ratio"] = stream.DisplayAspectRatio
							}
							if stream.PixelFormat != "" {
								track["pixel_format"] = stream.PixelFormat
							}
							if stream.ColorPrimaries != "" {
								track["color_primaries"] = stream.ColorPrimaries
							}
							if stream.ColorSpace != "" {
								track["color_space"] = stream.ColorSpace
							}
							if stream.ColorTransfer != "" {
								track["color_transfer"] = stream.ColorTransfer
							}
							if stream.ReferenceFrames != nil {
								track["reference_frames"] = *stream.ReferenceFrames
							}
							if stream.ProbedLevel != nil {
								track["level"] = *stream.ProbedLevel
							}
							if stream.ProbedDolbyVisionProfile != nil {
								track["dv_profile"] = *stream.ProbedDolbyVisionProfile
							}
							if stream.ProbedDolbyVisionLevel != nil {
								track["dv_level"] = *stream.ProbedDolbyVisionLevel
							}
							if stream.DolbyVisionBLPresent != nil {
								track["dv_bl_present"] = *stream.DolbyVisionBLPresent
							}
							if stream.DolbyVisionELPresent != nil {
								track["dv_el_present"] = *stream.DolbyVisionELPresent
							}
							if stream.DolbyVisionRPUPresent != nil {
								track["dv_rpu_present"] = *stream.DolbyVisionRPUPresent
							}
							if stream.DolbyVisionBLCompatibilityIDPresent != nil && *stream.DolbyVisionBLCompatibilityIDPresent {
								track["dv_bl_compat_id"] = stream.DolbyVisionCompatibilityID
							}
							switch rangeName {
							case "sdr":
								track["video_range_type"] = "SDR"
							case "hdr10":
								track["video_range_type"] = "HDR10"
							case "hlg":
								track["video_range_type"] = "HLG"
							}
							// These booleans are mandatory in Silo's track DTO. Only
							// publish a track when their presence is established by
							// the probe; missing metadata is not proof of absence.
							config, compat, interlaced := stream.DolbyVisionConfigurationPresent, stream.DolbyVisionBLCompatibilityIDPresent, stream.ProbedInterlaced
							positive := true
							if config == nil && stream.DolbyVisionVersion != "" {
								config = &positive
							}
							if compat == nil && stream.DolbyVisionCompatibilityID > 0 {
								compat = &positive
							}
							if interlaced == nil && stream.Interlaced {
								interlaced = &positive
							}
							if !version.Media.Estimated && config != nil && compat != nil && interlaced != nil {
								track["dv_config_present"] = *config
								track["dv_bl_compat_id_present"] = *compat
								track["interlaced"] = *interlaced
								videoTracks = append(videoTracks, track)
							}
						}
						if stream.Type == "audio" && audio == "" {
							audio = stream.Codec
						}
						if stream.Type == "audio" {
							track := map[string]any{"default": stream.Default}
							if stream.Codec != "" {
								track["codec"] = stream.Codec
							}
							if stream.Language != "" {
								track["language"] = stream.Language
							}
							if stream.Title != "" {
								track["title"] = stream.Title
							}
							if stream.Channels > 0 {
								track["channels"] = stream.Channels
							}
							if stream.ChannelLayout != "" {
								track["layout"] = stream.ChannelLayout
							}
							if stream.SampleRate > 0 {
								track["sample_rate"] = stream.SampleRate
							}
							if stream.BitrateKbps > 0 {
								track["bitrate"] = stream.BitrateKbps
							}
							if stream.BitDepth > 0 {
								track["bit_depth"] = stream.BitDepth
							}
							if stream.Profile != "" {
								track["profile"] = stream.Profile
							}
							audioTracks = append(audioTracks, track)
						}
					}
					item := map[string]any{"file_id": id, "file_name": version.Name, "resolution": resolution, "codec_video": video, "codec_audio": audio, "hdr": hdr, "container": version.Media.Container, "duration": int64(version.Media.Duration.Seconds()), "file_size": version.Media.Size, "bitrate": version.Media.BitrateKbps, "added_at": version.AddedAt.UTC().Format("2006-01-02T15:04:05.000Z")}
					if len(audioTracks) > 0 {
						item["audio_tracks"] = audioTracks
					}
					if len(videoTracks) > 0 {
						item["video_tracks"] = videoTracks
					}
					items = append(items, item)
				}
				out["versions"] = items
				if len(items) > 0 {
					part := map[string]any{"part_index": 1, "default_file_id": defaultID, "versions": items}
					group := map[string]any{"variant_id": "default", "default_file_id": defaultID, "part_count": 1, "parts": []any{part}}
					if defaultDuration > 0 {
						part["total_duration"] = defaultDuration
						group["total_duration"] = defaultDuration
					}
					out["playback_variants"] = []any{group}
				}

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

// addLastPlayedFacts only exposes facts measured on the opened source. Missing
// probe values stay absent instead of being inferred from a selected version.
func addLastPlayedFacts(out map[string]any, source *frontend.PlaybackSourceHistory) {
	if source.VideoCodec != "" {
		out["last_codec_video"] = source.VideoCodec
	}
	if source.Height > 0 {
		out["last_resolution"] = strconv.Itoa(source.Height) + "p"
	}
	if source.HDR != nil {
		out["last_hdr"] = *source.HDR
	}
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
