package silo

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
)

type skipSettings struct {
	introMode              string
	autoIntro, autoCredits bool
	autoRecap              bool
}

func (s skipSettings) hideIntro() bool { return s.introMode == "never" }

func (s skipSettings) seekIntro() bool {
	return s.introMode == "always" || (s.autoIntro && s.introMode != "never")
}

func (i *instance) skipSettings(r *http.Request, viewer *frontend.Viewer) skipSettings {
	settings := skipSettings{introMode: "ask"}
	var mode string
	if json.Unmarshal(i.effectiveSetting(r, viewer, "playback.intro_skip_mode"), &mode) == nil {
		switch mode {
		case "never", "ask", "always":
			settings.introMode = mode
		}
	}
	settings.autoIntro = settingBool(i.effectiveSetting(r, viewer, "playback.auto_skip_intro"))
	settings.autoCredits = settingBool(i.effectiveSetting(r, viewer, "playback.auto_skip_credits"))
	settings.autoRecap = settingBool(i.effectiveSetting(r, viewer, "playback.auto_skip_recap"))
	return settings
}

func settingBool(raw json.RawMessage) bool {
	var value bool
	if json.Unmarshal(raw, &value) != nil {
		return false
	}
	return value
}

func (i *instance) subtitleSyncMS(r *http.Request, viewer *frontend.Viewer) int {
	var value int
	if json.Unmarshal(i.effectiveSetting(r, viewer, "player.subtitle_sync_ms"), &value) != nil || value < -10000 || value > 10000 {
		return 0
	}
	return value
}

func (i *instance) effectiveSetting(r *http.Request, viewer *frontend.Viewer, key string) json.RawMessage {
	definition, ok := settingDefinitions[key]
	if !ok {
		return nil
	}
	fallback := json.RawMessage(definition.defaultJSON)
	if viewer == nil || i.host.Preferences == nil {
		return fallback
	}
	for _, scope := range []string{"profile_device", "profile_series", "profile_library", "profile_client", "profile", "account"} {
		if !definition.acceptsScope(scope) {
			continue
		}
		id, ok := i.settingScope(r, viewer, scope)
		if !ok {
			continue
		}
		stored, found, err := i.getSetting(r, viewer, key, scope, id)
		if err != nil || !found {
			continue
		}
		return stored.Value
	}
	if definition.acceptsScope("profile") {
		for field, settingKey := range profileSettings {
			if settingKey != key {
				continue
			}
			raw, err := i.host.Preferences.Get(r.Context(), viewer, "profile", profileKey)
			if err != nil {
				break
			}
			var saved map[string]any
			if json.Unmarshal(raw, &saved) != nil {
				break
			}
			if value, ok := saved[field]; !ok {
				break
			} else if encoded, err := json.Marshal(value); err == nil {
				return encoded
			}
		}
	}
	return fallback
}

func (i *instance) withoutSkippedIntros(r *http.Request, viewer *frontend.Viewer, segments []any) []any {
	if !i.skipSettings(r, viewer).hideIntro() {
		if segments == nil {
			return []any{}
		}
		return segments
	}
	out := make([]any, 0, len(segments))
	for _, raw := range segments {
		segment, _ := raw.(map[string]any)
		if segment["kind"] == "intro" {
			continue
		}
		out = append(out, raw)
	}
	return out
}

func attachWatchMarkers(item map[string]any, segments []any) {
	for _, raw := range segments {
		segment, _ := raw.(map[string]any)
		kind, _ := segment["kind"].(string)
		switch kind {
		case "intro", "credits", "recap", "preview":
		default:
			continue
		}
		if _, exists := item[kind]; exists {
			continue
		}
		item[kind] = map[string]any{"start_seconds": segment["start_seconds"], "end_seconds": segment["end_seconds"]}
	}
}

func (i *instance) skipIntroAtStart(r *http.Request, viewer *frontend.Viewer, scope frontend.VersionScope, key core.RatingKey, versionHandle string, position time.Duration) time.Duration {
	settings := i.skipSettings(r, viewer)
	if !settings.seekIntro() || i.host.Experience == nil {
		return position
	}
	detail, err := i.host.Experience.Item(r.Context(), frontend.ItemRequest{Viewer: viewer, Device: device(r), Key: key})
	if err != nil {
		return position
	}
	markers := detail.Markers
	if versionHandle != "" && i.host.VersionHandles != nil {
		if target, err := i.host.VersionHandles.Resolve(r.Context(), scope, versionHandle); err == nil && target.Release != "" {
			for _, version := range detail.Versions {
				if version.ID == target.Release && len(version.Markers) > 0 {
					markers = version.Markers
					break
				}
			}
		}
	} else if len(detail.Versions) == 1 && len(detail.Versions[0].Markers) > 0 {
		markers = detail.Versions[0].Markers
	}
	return seekPastIntro(position, markers)
}

func seekPastIntro(position time.Duration, markers []frontend.Marker) time.Duration {
	for _, marker := range markers {
		if marker.Type == "intro" && position >= marker.Start && position < marker.End {
			return marker.End
		}
	}
	return position
}
