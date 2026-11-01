package mediabrowser

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// These Emby operations are not implemented by the frontend. Playback and
// authorization decisions remain with the Host, not these descriptive fields.
func (i *instance) embyUserDTO(user map[string]any) {
	if name, ok := user["Name"].(string); ok {
		if prefix := embyPrefix(name); prefix != "" {
			user["Prefix"] = prefix
		}
	}
	if cfg, ok := user["Configuration"].(map[string]any); ok {
		// ShowButton is Emby's skip-intro control. None tells the client not to
		// skip even when MediaSegments carries intro and credits ranges.
		cfg["IntroSkipMode"] = "ShowButton"
		cfg["ResumeRewindSeconds"] = 0
		if latest, ok := cfg["HidePlayedInLatest"]; ok {
			cfg["HidePlayedInMoreLikeThis"] = latest
			cfg["HidePlayedInSuggestions"] = latest
		}
	}
	policy, ok := user["Policy"].(map[string]any)
	if !ok {
		return
	}
	policy["AllowCameraUpload"] = false
	policy["AllowSharingPersonalItems"] = false
	policy["AllowTagOrRating"] = false
	policy["EnableSubtitleDownloading"] = i.host.Subtitles != nil && i.host.Subtitles.Enabled()
	policy["EnableTranscodingQuality"] = false
	policy["IsHiddenFromUnusedDevices"] = false
	policy["IsTagBlockingModeInclusive"] = false
	policy["AutoRemoteQuality"] = 0
	policy["LockedOutDate"] = 0
	if hidden, ok := policy["IsHidden"].(bool); ok {
		policy["IsHiddenRemotely"] = hidden
	}
	for _, key := range []string{"AccessSchedules", "BlockUnratedItems", "ExcludedSubFolders", "IncludeTags", "RestrictedFeatures"} {
		if _, present := policy[key]; !present {
			policy[key] = []any{}
		}
	}
	if _, present := policy["EnabledFolders"]; !present {
		policy["EnabledFolders"] = []string{}
	}
}

// embyPrefix is the public grouping letter Emby shows on the user picker.
func embyPrefix(name string) string {
	name = strings.TrimSpace(name)
	r, size := utf8.DecodeRuneInString(name)
	if size == 0 || r == utf8.RuneError {
		return ""
	}
	return string(unicode.ToUpper(r))
}
