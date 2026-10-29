package silo

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/kit/ids"
	"github.com/braedonsaunders/ether/internal/langtag"
)

// settingDefinition records the revision-8 wire vocabulary, not an Ether
// policy; authoritative playback limits remain enforced by the broker.
type settingDefinition struct {
	kind, defaultJSON      string
	scopes, choices        []string
	nullable               bool
	minimum, maximum       float64
	hasMinimum, hasMaximum bool
}

func (i *instance) settingsRoutes() error {
	for _, route := range []struct {
		method, path string
		handler      http.HandlerFunc
	}{
		{"GET", "/api/v2/settings/contract/capabilities", i.settingsCapabilities},
		{"GET", "/api/v2/settings/contract", i.settingsContract},
		{"GET", "/api/v2/settings/values/effective", i.settingValues},
		{"PUT", "/api/v2/settings/values/{key}", i.settingWrite},
		{"DELETE", "/api/v2/settings/values/{key}", i.settingWrite},
	} {
		if err := i.router.Handle(route.method, route.path, route.handler); err != nil {
			return err
		}
	}
	return nil
}

type storedSetting struct {
	Value     json.RawMessage `json:"value"`
	Revision  int64           `json:"revision"`
	UpdatedAt string          `json:"updated_at"`
}

func settingsClient(scope, id string) string {
	sum := sha256.Sum256([]byte(scope + "\x00" + id))
	return "settings-" + hex.EncodeToString(sum[:])
}

func (d settingDefinition) acceptsScope(scope string) bool {
	for _, s := range d.scopes {
		if s == scope {
			return true
		}
	}
	return false
}

func (d settingDefinition) valid(raw json.RawMessage) bool {
	if string(raw) == "null" {
		return d.nullable
	}
	var value any
	if json.Unmarshal(raw, &value) != nil {
		return false
	}
	switch d.kind {
	case "boolean":
		_, ok := value.(bool)
		return ok
	case "integer", "number":
		v, ok := value.(float64)
		return ok && !math.IsInf(v, 0) && (d.kind != "integer" || math.Trunc(v) == v) && (!d.hasMinimum || v >= d.minimum) && (!d.hasMaximum || v <= d.maximum)
	case "string":
		_, ok := value.(string)
		return ok
	case "language_tag":
		v, ok := value.(string)
		if !ok {
			return false
		}
		_, ok = langtag.ParseBCP47(v)
		return ok
	case "enum":
		v, ok := value.(string)
		if !ok {
			return false
		}
		for _, choice := range d.choices {
			if choice == v {
				return true
			}
		}
		return false
	case "object":
		_, ok := value.(map[string]any)
		return ok
	default:
		return false
	}
}

func (i *instance) settingsCapabilities(w http.ResponseWriter, r *http.Request) {
	v, _, err := i.viewer(r)
	if err != nil || v == nil {
		http.Error(w, "Unauthorized", 401)
		return
	}
	if _, ok := i.host.Preferences.(frontend.VersionedPreferences); !ok {
		http.Error(w, "Unavailable", 503)
		return
	}
	if _, ok := i.host.TrackPreferences.(frontend.ScopedTrackPreferenceService); !ok {
		http.Error(w, "Unavailable", 503)
		return
	}
	_, etag := settingsContractDocument()
	reply(w, 200, map[string]any{"revision": "ether-settings-8-" + strings.Trim(etag, `"`), "state": "available", "allowed": true, "api_version": 1, "manifest_revision": 8, "contract_etag": etag, "definition_count": len(settingDefinitions), "scopes": []string{"profile", "profile_client", "profile_device", "profile_library", "profile_series"}, "client_families": []string{"tv", "mobile", "tablet", "desktop", "web"}, "supports_batched_effective": true, "supports_idempotent_writes": false, "supports_atomic_shortcuts": false, "supports_device_rename": false})
}

func settingsContractDocument() (map[string]any, string) {
	defs := make([]any, 0, len(settingDefinitions))
	keys := make([]string, 0, len(settingDefinitions))
	for key := range settingDefinitions {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		d := settingDefinitions[key]
		var value any
		_ = json.Unmarshal([]byte(d.defaultJSON), &value)
		schema := map[string]any{"type": d.kind, "nullable": d.nullable}
		if d.hasMinimum {
			schema["minimum"] = d.minimum
		}
		if d.hasMaximum {
			schema["maximum"] = d.maximum
		}
		if len(d.choices) > 0 {
			values := make([]any, 0, len(d.choices))
			for _, choice := range d.choices {
				values = append(values, map[string]any{"value": choice})
			}
			schema["values"] = values
		}
		order := []string{}
		for _, scope := range []string{"profile_device", "profile_series", "profile_library", "profile_client", "profile", "account"} {
			if d.acceptsScope(scope) {
				order = append(order, scope)
			}
		}
		order = append(order, "default")
		persistence := "remote"
		if d.acceptsScope("client_local") {
			persistence = "client_local"
			order = []string{"client_local", "default"}
		}
		defs = append(defs, map[string]any{"key": key, "persistence": persistence, "allowed_scopes": d.scopes, "resolution_order": order, "default_value": value, "value_schema": schema})
	}
	doc := map[string]any{"api_version": 1, "revision": 8, "definitions": defs, "option_sets": map[string]any{}}
	raw, _ := json.Marshal(doc)
	sum := sha256.Sum256(raw)
	return doc, `"` + hex.EncodeToString(sum[:]) + `"`
}

func (i *instance) settingsContract(w http.ResponseWriter, r *http.Request) {
	v, _, err := i.viewer(r)
	if err != nil || v == nil {
		http.Error(w, "Unauthorized", 401)
		return
	}
	doc, etag := settingsContractDocument()
	w.Header().Set("ETag", etag)
	reply(w, 200, doc)
}

func (i *instance) settingScope(r *http.Request, viewer *frontend.Viewer, scope string) (string, bool) {
	q := r.URL.Query()
	if p := q.Get("profile_id"); p != "" && p != strconv.FormatInt(viewer.ID, 10) {
		return "", false
	}
	switch scope {
	case "account", "profile":
		return "", true
	case "profile_device":
		id := device(r).ID
		return id, id != "" && len(id) <= 128
	case "profile_client":
		id := r.Header.Get("X-Silo-Client-Family")
		switch id {
		case "tv", "mobile", "tablet", "desktop", "web":
			return id, true
		}
		return "", false
	case "profile_library":
		id := q.Get("library_id")
		if id == "" {
			id = q.Get("library_ids")
		}
		n, err := strconv.Atoi(id)
		if err != nil || n <= 0 || i.host.Experience == nil {
			return "", false
		}
		views, err := i.host.Experience.Views(r.Context(), viewer)
		if err != nil {
			return "", false
		}
		for _, view := range views {
			if view.ID == id {
				return id, true
			}
		}
		return "", false
	case "profile_series":
		id := q.Get("series_id")
		if id == "" {
			id = q.Get("series_ids")
		}
		key, err := ids.ParseRatingKeyGUID(id)
		if err != nil || key.Kind() != core.KindShow || i.host.Experience == nil {
			return "", false
		}
		_, err = i.host.Experience.Item(r.Context(), frontend.ItemRequest{Viewer: viewer, Device: device(r), Key: key})
		return id, err == nil
	default:
		return "", false
	}
}

func settingTrackScope(r *http.Request, i *instance, v *frontend.Viewer) frontend.TrackScope {
	scope := frontend.TrackScope{Device: device(r).ID}
	if id, ok := i.settingScope(r, v, "profile_library"); ok {
		scope.Library = id
	}
	if id, ok := i.settingScope(r, v, "profile_series"); ok {
		if key, err := ids.ParseRatingKeyGUID(id); err == nil {
			scope.Series = strconv.FormatInt(int64(key), 10)
		}
	}
	return scope
}

func trackField(key string) bool {
	switch key {
	case "playback.audio_language", "playback.subtitle_language", "playback.subtitle_mode", "playback.preferred_quality", "playback.max_bitrate_kbps":
		return true
	}
	return false
}

func (i *instance) setTrackValue(r *http.Request, v *frontend.Viewer, key, scope, id string, raw json.RawMessage) error {
	svc, ok := i.host.TrackPreferences.(frontend.ScopedTrackPreferenceService)
	if !ok {
		return errors.New("shared track preferences unavailable")
	}
	var patch frontend.TrackPreferencePatch
	field := ""
	switch key {
	case "playback.audio_language", "playback.subtitle_language":
		var value *string
		if err := json.Unmarshal(raw, &value); err != nil {
			return err
		}
		langs := []string{}
		if value != nil && *value != "" {
			langs = []string{*value}
		}
		if key == "playback.audio_language" {
			patch.AudioLanguages = &langs
			field = "audio"
		} else {
			patch.SubtitleLanguages = &langs
			field = "subtitle"
		}
	case "playback.subtitle_mode":
		var value string
		if err := json.Unmarshal(raw, &value); err != nil {
			return err
		}
		mode, ok := map[string]string{"off": "off", "always": "always", "auto": "foreign-only"}[value]
		if !ok {
			return errors.New("unsupported subtitle mode")
		}
		patch.SubtitleMode = &mode
		field = "subtitle_mode"
	case "playback.max_bitrate_kbps":
		var value *int
		if err := json.Unmarshal(raw, &value); err != nil {
			return err
		}
		n := 0
		if value != nil {
			n = *value
		}
		patch.MaxBitrateKbps = &n
		field = "bitrate"
	case "playback.preferred_quality":
		var value string
		if err := json.Unmarshal(raw, &value); err != nil {
			return err
		}
		n := 0
		if value != "auto" && value != "original" {
			var err error
			n, err = strconv.Atoi(strings.TrimSuffix(value, "p"))
			if err != nil {
				return err
			}
		}
		patch.MaxHeight = &n
		patch.PreferredQuality = &value
		field = "height"
	}
	if scope == "profile" {
		switch field {
		case "audio":
			return svc.SetAudio(r.Context(), v, *patch.AudioLanguages)
		case "bitrate":
			return svc.SetBitrate(r.Context(), v, *patch.MaxBitrateKbps)
		case "height":
			return svc.SetHeight(r.Context(), v, *patch.MaxHeight, *patch.PreferredQuality)
		case "subtitle":
			return svc.SetSubtitleLanguage(r.Context(), v, *patch.SubtitleLanguages)
		case "subtitle_mode":
			return svc.SetSubtitleMode(r.Context(), v, *patch.SubtitleMode)
		}
	}
	target := map[string]string{"profile_device": "device", "profile_library": "library", "profile_series": "series"}[scope]
	if target == "" {
		return errors.New("unsupported scope")
	}
	if target == "series" {
		key, err := ids.ParseRatingKeyGUID(id)
		if err != nil {
			return err
		}
		id = strconv.FormatInt(int64(key), 10)
	}
	return svc.SetScoped(r.Context(), v, target, id, patch)
}

func (i *instance) getSetting(r *http.Request, viewer *frontend.Viewer, key, scope, id string) (storedSetting, bool, error) {
	var value storedSetting
	service, ok := i.host.Preferences.(frontend.VersionedPreferences)
	if !ok {
		return value, false, errors.New("versioned preferences unavailable")
	}
	raw, revision, updated, err := service.GetVersioned(r.Context(), viewer, settingsClient(scope, id), key)
	if errors.Is(err, frontend.ErrPreferencesNotFound) {
		return value, false, nil
	}
	if err != nil {
		return value, false, err
	}
	if !json.Valid(raw) {
		return storedSetting{}, false, errors.New("invalid stored setting")
	}
	value = storedSetting{Value: raw, Revision: revision, UpdatedAt: updated.Format(time.RFC3339Nano)}
	return value, true, nil
}

func (i *instance) settingValues(w http.ResponseWriter, r *http.Request) {
	v, _, err := i.viewer(r)
	if err != nil || v == nil {
		http.Error(w, "Unauthorized", 401)
		return
	}
	if i.host.Preferences == nil {
		http.Error(w, "Unavailable", 503)
		return
	}
	keys := r.URL.Query()["keys"]
	if len(keys) == 0 {
		for key, d := range settingDefinitions {
			if !d.acceptsScope("client_local") {
				keys = append(keys, key)
			}
		}
		sort.Strings(keys)
	}
	if len(keys) > 200 {
		http.Error(w, "Bad Request", 400)
		return
	}
	items := make([]any, 0, len(keys))
	seen := map[string]bool{}
	for _, key := range keys {
		d, ok := settingDefinitions[key]
		if !ok || seen[key] {
			http.Error(w, "Unknown setting", 422)
			return
		}
		seen[key] = true
		row := map[string]any{"key": key, "value": json.RawMessage(d.defaultJSON), "source": "default", "definition_revision": 8}
		for _, scope := range []string{"profile_device", "profile_series", "profile_library", "profile_client", "profile", "account"} {
			if !d.acceptsScope(scope) {
				continue
			}
			id, ok := i.settingScope(r, v, scope)
			if !ok {
				continue
			}
			stored, found, err := i.getSetting(r, v, key, scope, id)
			if err != nil {
				http.Error(w, "Unavailable", 503)
				return
			}
			if !found {
				continue
			}
			row["value"] = stored.Value
			row["source"] = scope
			row["scope"] = scope
			if scope != "account" {
				row["profile_id"] = strconv.FormatInt(v.ID, 10)
			}
			switch scope {
			case "profile_device":
				row["device_id"] = id
			case "profile_client":
				row["client_family"] = id
			case "profile_library":
				row["library_id"] = id
			case "profile_series":
				row["series_id"] = id
			}
			break
		}
		if trackField(key) {
			delete(row, "scope")
			delete(row, "profile_id")
			delete(row, "device_id")
			delete(row, "client_family")
			delete(row, "library_id")
			delete(row, "series_id")
			row["source"] = "default"
			svc, ok := i.host.TrackPreferences.(frontend.ScopedTrackPreferenceService)
			if !ok {
				http.Error(w, "Unavailable", 503)
				return
			}
			tracks, bitrate, height, quality, sources, e := svc.GetScoped(r.Context(), v, settingTrackScope(r, i, v))
			if e != nil {
				http.Error(w, "Unavailable", 503)
				return
			}
			switch key {
			case "playback.audio_language":
				if len(tracks.AudioLanguages) > 0 {
					row["value"] = tracks.AudioLanguages[0]
				} else {
					row["value"] = nil
				}
			case "playback.subtitle_language":
				if len(tracks.SubtitleLanguages) > 0 {
					row["value"] = tracks.SubtitleLanguages[0]
				} else {
					row["value"] = nil
				}
			case "playback.subtitle_mode":
				row["value"] = map[string]string{"off": "off", "always": "always", "foreign-only": "auto", "forced-only": "auto"}[tracks.SubtitleMode]
			case "playback.max_bitrate_kbps":
				if bitrate > 0 {
					row["value"] = bitrate
				} else {
					row["value"] = nil
				}
			case "playback.preferred_quality":
				if quality != "" {
					row["value"] = quality
				} else if height > 0 {
					row["value"] = strconv.Itoa(height) + "p"
				} else {
					row["value"] = "auto"
				}
			}
			field := map[string]string{"playback.audio_language": "audio", "playback.subtitle_language": "subtitle", "playback.subtitle_mode": "subtitle_mode", "playback.max_bitrate_kbps": "bitrate", "playback.preferred_quality": "height"}[key]
			scope := map[string]string{"device": "profile_device", "series": "profile_series", "library": "profile_library", "profile": "profile"}[sources[field]]
			if scope != "" {
				row["source"], row["scope"], row["profile_id"] = scope, scope, strconv.FormatInt(v.ID, 10)
			}
			if id, ok := i.settingScope(r, v, scope); ok {
				switch scope {
				case "profile_device":
					row["device_id"] = id
				case "profile_library":
					row["library_id"] = id
				case "profile_series":
					row["series_id"] = id
				}
			}
		}
		items = append(items, row)
	}
	reply(w, 200, map[string]any{"items": items, "revision": 8})
}

func (i *instance) settingWrite(w http.ResponseWriter, r *http.Request) {
	v, _, err := i.viewer(r)
	if err != nil || v == nil {
		http.Error(w, "Unauthorized", 401)
		return
	}
	if i.host.Preferences == nil {
		http.Error(w, "Unavailable", 503)
		return
	}
	key := r.PathValue("key")
	d, ok := settingDefinitions[key]
	scope := r.URL.Query().Get("scope")
	id, valid := i.settingScope(r, v, scope)
	if !ok || !valid || !d.acceptsScope(scope) {
		http.Error(w, "Unsupported", 422)
		return
	}
	if r.Method == "DELETE" {
		if trackField(key) {
			originalID := id
			svc, ok := i.host.TrackPreferences.(frontend.ScopedTrackPreferenceService)
			if !ok {
				http.Error(w, "Unavailable", 503)
				return
			}
			if scope == "profile" {
				if err := i.setTrackValue(r, v, key, scope, id, json.RawMessage(d.defaultJSON)); err != nil {
					http.Error(w, "Unavailable", 503)
					return
				}
			} else {
				target := map[string]string{"profile_device": "device", "profile_library": "library", "profile_series": "series"}[scope]
				field := map[string]string{"playback.audio_language": "audio", "playback.subtitle_language": "subtitle", "playback.subtitle_mode": "subtitle_mode", "playback.max_bitrate_kbps": "bitrate", "playback.preferred_quality": "height"}[key]
				if target == "series" {
					k, e := ids.ParseRatingKeyGUID(id)
					if e != nil {
						http.Error(w, "Bad Request", 400)
						return
					}
					id = strconv.FormatInt(int64(k), 10)
				}
				if target == "" || svc.DeleteScoped(r.Context(), v, target, id, field) != nil {
					http.Error(w, "Unavailable", 503)
					return
				}
			}
			if err := i.host.Preferences.Delete(r.Context(), v, settingsClient(scope, originalID), key); err != nil {
				http.Error(w, "Unavailable", 503)
				return
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if err := i.host.Preferences.Delete(r.Context(), v, settingsClient(scope, id), key); err != nil {
			http.Error(w, "Unavailable", 503)
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	var body struct {
		Value json.RawMessage `json:"value"`
	}
	if decode(r, &body) != nil || !d.valid(body.Value) {
		http.Error(w, "Bad Request", 400)
		return
	}
	stored := storedSetting{Value: body.Value}
	service, ok := i.host.Preferences.(frontend.VersionedPreferences)
	if !ok {
		http.Error(w, "Unavailable", 503)
		return
	}
	revision, updated, err := service.PutVersioned(r.Context(), v, settingsClient(scope, id), key, body.Value)
	if err != nil {
		http.Error(w, "Unavailable", 503)
		return
	}
	if trackField(key) {
		if err := i.setTrackValue(r, v, key, scope, id, body.Value); err != nil {
			http.Error(w, "Unavailable", 503)
			return
		}
	}
	stored.Revision, stored.UpdatedAt = revision, updated.Format(time.RFC3339Nano)
	row := map[string]any{"key": key, "scope": scope, "value": stored.Value, "revision": stored.Revision, "updated_at": stored.UpdatedAt}
	if scope != "account" {
		row["profile_id"] = strconv.FormatInt(v.ID, 10)
	}
	switch scope {
	case "profile_device":
		row["device_id"] = id
	case "profile_client":
		row["client_family"] = id
	case "profile_library":
		row["library_id"] = id
	case "profile_series":
		row["series_id"] = id
	}
	reply(w, 200, row)
}
