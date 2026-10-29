package silo

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/braedonsaunders/ether/internal/frontend"
)

const profileKey = "silo.profile"

func defaults() map[string]any {
	return map[string]any{"avatar": "", "avatar_source": "none", "quality_preference": "auto", "language": "", "preferred_metadata_language": "", "auto_skip_intro": false, "auto_skip_credits": false, "auto_skip_recap": false, "auto_play_next_preview": false, "show_forced_subtitles": true, "max_playback_quality": ""}
}
func (i *instance) profileData(r *http.Request, v *frontend.Viewer) (map[string]any, error) {
	if i.host.Preferences == nil || i.host.TrackPreferences == nil || v.CreatedAt.IsZero() || v.UpdatedAt.IsZero() {
		return nil, errors.New("profile data unavailable")
	}
	p := defaults()
	raw, err := i.host.Preferences.Get(r.Context(), v, "profile", profileKey)
	if errors.Is(err, frontend.ErrPreferencesNotFound) {
		p["_updated_at"] = time.Now().UTC().Format(time.RFC3339Nano)
		raw, _ = json.Marshal(p)
		if err = i.host.Preferences.Put(r.Context(), v, "profile", profileKey, raw); err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	} else if err = json.Unmarshal(raw, &p); err != nil {
		return nil, err
	}
	tracks, err := i.host.TrackPreferences.Get(r.Context(), v)
	if err != nil {
		return nil, err
	}
	if scoped, ok := i.host.TrackPreferences.(frontend.ScopedTrackPreferenceService); ok {
		_, _, height, _, _, err := scoped.GetScoped(r.Context(), v, frontend.TrackScope{})
		if err != nil {
			return nil, err
		}
		p["max_playback_quality"] = ""
		if height > 0 {
			p["max_playback_quality"] = strconv.Itoa(height) + "p"
		}
	}
	subtitle := ""
	if len(tracks.SubtitleLanguages) > 0 {
		subtitle = tracks.SubtitleLanguages[0]
	}
	mode := map[string]string{"off": "off", "forced-only": "forced_only", "always": "always", "foreign-only": "auto"}[tracks.SubtitleMode]
	if mode == "" {
		return nil, errors.New("unknown subtitle mode")
	}
	views := []string{}
	if i.host.Experience == nil {
		return nil, errors.New("libraries unavailable")
	}
	available, err := i.host.Experience.Views(r.Context(), v)
	if err != nil {
		return nil, err
	}
	for _, view := range available {
		views = append(views, view.ID)
	}
	updated := v.UpdatedAt
	if stamp, ok := p["_updated_at"].(string); ok {
		if parsed, e := time.Parse(time.RFC3339Nano, stamp); e == nil && parsed.After(updated) {
			updated = parsed
		}
	}
	delete(p, "_updated_at")
	p["id"] = strconv.FormatInt(v.ID, 10)
	p["name"] = v.Username
	p["has_pin"] = false
	p["is_child"] = false
	p["is_primary"] = true
	rating := v.MaxContentRating
	if i.host.ViewerCapabilities != nil && i.host.ClientIP != nil {
		if ip, ok := i.host.ClientIP(r); ok {
			if info, e := i.host.ViewerCapabilities(r.Context(), v, device(r), ip); e == nil && info.MaxRating != nil {
				rating = *info.MaxRating
			}
		}
	}
	p["max_content_rating"] = rating
	p["max_advisory_age"] = nil
	p["require_advisory_age"] = false
	p["subtitle_language"] = subtitle
	p["subtitle_mode"] = mode
	p["library_restrictions_enabled"] = len(v.AllowedSections) > 0
	p["allowed_library_ids"] = views
	p["created_at"] = v.CreatedAt.UTC().Format("2006-01-02T15:04:05.000Z")
	p["updated_at"] = updated.UTC().Format("2006-01-02T15:04:05.000Z")
	return p, nil
}
func (i *instance) profiles(w http.ResponseWriter, r *http.Request) {
	v, _, err := i.viewer(r)
	if err != nil || v == nil {
		http.Error(w, "Unauthorized", 401)
		return
	}
	p, err := i.profileData(r, v)
	if err != nil {
		http.Error(w, "Unavailable", 503)
		return
	}
	reply(w, 200, map[string]any{"items": []any{p}, "avatar_upload_enabled": false, "max_advisory_age_supported": false, "require_advisory_age_supported": false})
}
func (i *instance) patchProfile(w http.ResponseWriter, r *http.Request) {
	v, credential, err := i.viewer(r)
	if err != nil || v == nil {
		http.Error(w, "Unauthorized", 401)
		return
	}
	if r.PathValue("id") != strconv.FormatInt(v.ID, 10) {
		http.NotFound(w, r)
		return
	}
	p, err := i.profileData(r, v)
	if err != nil {
		http.Error(w, "Unavailable", 503)
		return
	}
	var patch map[string]json.RawMessage
	if decode(r, &patch) != nil {
		http.Error(w, "Bad Request", 400)
		return
	}
	preferences := defaults()
	raw, err := i.host.Preferences.Get(r.Context(), v, "profile", profileKey)
	if err != nil || json.Unmarshal(raw, &preferences) != nil {
		http.Error(w, "Unavailable", 503)
		return
	}
	tracks, err := i.host.TrackPreferences.Get(r.Context(), v)
	if err != nil {
		http.Error(w, "Unavailable", 503)
		return
	}
	var maturity *string
	var maxHeight *int
	tracksChanged := false
	for k, raw := range patch {
		switch k {
		case "max_content_rating":
			var value string
			if json.Unmarshal(raw, &value) != nil {
				http.Error(w, "Bad Request", 400)
				return
			}
			maturity = &value
		case "subtitle_language":
			var value string
			if json.Unmarshal(raw, &value) != nil {
				http.Error(w, "Bad Request", 400)
				return
			}
			tracks.SubtitleLanguages = nil
			if value != "" {
				tracks.SubtitleLanguages = []string{value}
			}
			tracksChanged = true
		case "subtitle_mode":
			var value string
			if json.Unmarshal(raw, &value) != nil {
				http.Error(w, "Bad Request", 400)
				return
			}
			mode, ok := map[string]string{"off": "off", "forced_only": "forced-only", "always": "always", "auto": "foreign-only"}[value]
			if !ok {
				http.Error(w, "Unsupported", 422)
				return
			}
			tracks.SubtitleMode = mode
			tracksChanged = true
		case "max_playback_quality":
			var quality *string
			if json.Unmarshal(raw, &quality) != nil {
				http.Error(w, "Bad Request", 400)
				return
			}
			height := 0
			if quality != nil && *quality != "" {
				height = map[string]int{"1080p": 1080, "2160p": 2160}[*quality]
				if height == 0 {
					http.Error(w, "Unsupported", 422)
					return
				}
			}
			maxHeight = &height
		case "avatar", "quality_preference", "language", "preferred_metadata_language", "auto_skip_intro", "auto_skip_credits", "auto_skip_recap", "auto_play_next_preview", "show_forced_subtitles":
			var value any
			if json.Unmarshal(raw, &value) != nil {
				http.Error(w, "Bad Request", 400)
				return
			}
			switch defaults()[k].(type) {
			case string:
				if value == nil && k == "avatar" {
					value = ""
				}
				if _, ok := value.(string); !ok {
					http.Error(w, "Bad Request", 400)
					return
				}
			case bool:
				if _, ok := value.(bool); !ok {
					http.Error(w, "Bad Request", 400)
					return
				}
			}
			preferences[k] = value
		default:
			http.Error(w, "Unsupported", 422)
			return
		}
	}
	if maturity != nil {
		if i.host.ViewerPolicy == nil || i.host.ViewerPolicy.SetMaxRating(r.Context(), v, *maturity) != nil {
			http.Error(w, "Forbidden", 403)
			return
		}
		v, err = i.host.Identity.Resolve(r.Context(), credential)
		if err != nil || v == nil {
			http.Error(w, "Unauthorized", 401)
			return
		}
	}
	if tracksChanged {
		if err = i.host.TrackPreferences.SetSubtitle(r.Context(), v, tracks.SubtitleLanguages, tracks.SubtitleMode); err != nil {
			http.Error(w, "Bad Request", 400)
			return
		}
	}
	if maxHeight != nil {
		scoped, ok := i.host.TrackPreferences.(frontend.ScopedTrackPreferenceService)
		if !ok {
			http.Error(w, "Unavailable", 503)
			return
		}
		quality := "auto"
		if *maxHeight > 0 {
			quality = strconv.Itoa(*maxHeight) + "p"
		}
		if scoped.SetHeight(r.Context(), v, *maxHeight, quality) != nil {
			http.Error(w, "Unavailable", 503)
			return
		}
	}
	preferences["_updated_at"] = time.Now().UTC().Format(time.RFC3339Nano)
	encoded, err := json.Marshal(preferences)
	if err != nil || i.host.Preferences.Put(r.Context(), v, "profile", profileKey, encoded) != nil {
		http.Error(w, "Unavailable", 503)
		return
	}
	p, err = i.profileData(r, v)
	if err != nil {
		http.Error(w, "Unavailable", 503)
		return
	}
	reply(w, 200, p)
}
