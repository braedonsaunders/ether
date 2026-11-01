package plextv

import (
	"encoding/json"
	"strconv"
	"strings"
)

// TrackDefaults are a plex.tv account's language and subtitle settings.
// Ether uses them only as a default layer; owner rules and a viewer's own
// picks always win.
type TrackDefaults struct {
	AudioLanguage    string `json:"defaultAudioLanguage,omitempty"`
	AutoSelectAudio  bool   `json:"autoSelectAudio,omitempty"`
	SubtitleLanguage string `json:"defaultSubtitleLanguage,omitempty"`
	SubtitleMode     int    `json:"autoSelectSubtitle,omitempty"`
	ForcedOnly       bool   `json:"-"`
	HasProfile       bool   `json:"-"`
}

// ShareDefaults are a plex.tv shared-server grant. Empty filters impose no
// extra restriction. AllowSync is nil when the grant omitted the flag.
type ShareDefaults struct {
	FilterMovies     string
	FilterTelevision string
	AllowSync        *bool
	HasSettings      bool
}

// EtherSubtitleMode maps a plex.tv subtitle mode onto Ether's stored names.
func EtherSubtitleMode(mode int) string {
	switch mode {
	case 1:
		return "foreign-only"
	case 2:
		return "always"
	default:
		return "off"
	}
}

// EtherMode is the Ether subtitle mode for this plex.tv profile.
func (d TrackDefaults) EtherMode() string {
	if d.ForcedOnly && d.SubtitleMode == 0 {
		return "forced-only"
	}
	return EtherSubtitleMode(d.SubtitleMode)
}

func profileDefaults(n *xmlNode) TrackDefaults {
	if n == nil {
		return TrackDefaults{}
	}
	mode := n.attr("autoSelectSubtitle", "subtitleMode")
	parsed, _ := strconv.Atoi(strings.TrimSpace(mode))
	return TrackDefaults{
		AudioLanguage:    strings.TrimSpace(n.attr("defaultAudioLanguage")),
		AutoSelectAudio:  n.boolAttr("autoSelectAudio"),
		SubtitleLanguage: strings.TrimSpace(n.attr("defaultSubtitleLanguage")),
		SubtitleMode:     parsed,
		ForcedOnly:       n.boolAttr("defaultSubtitleForced"),
		HasProfile:       true,
	}
}

func shareDefaults(n *xmlNode) ShareDefaults {
	if n == nil {
		return ShareDefaults{}
	}
	out := ShareDefaults{
		FilterMovies:     n.attr("filterMovies"),
		FilterTelevision: n.attr("filterTelevision"),
		HasSettings:      true,
	}
	if raw := n.attr("allowSync"); raw != "" {
		on := n.boolAttr("allowSync")
		out.AllowSync = &on
	}
	return out
}

func trackDefaultsFromJSON(raw []byte) TrackDefaults {
	var v struct {
		DefaultAudioLanguage    string `json:"defaultAudioLanguage"`
		AutoSelectAudio         bool   `json:"autoSelectAudio"`
		DefaultSubtitleLanguage string `json:"defaultSubtitleLanguage"`
		AutoSelectSubtitle      int    `json:"autoSelectSubtitle"`
		SubtitleMode            int    `json:"subtitleMode"`
		DefaultSubtitleForced   bool   `json:"defaultSubtitleForced"`
		Profile                 *struct {
			DefaultAudioLanguage    string `json:"defaultAudioLanguage"`
			AutoSelectAudio         bool   `json:"autoSelectAudio"`
			DefaultSubtitleLanguage string `json:"defaultSubtitleLanguage"`
			AutoSelectSubtitle      int    `json:"autoSelectSubtitle"`
			SubtitleMode            int    `json:"subtitleMode"`
			DefaultSubtitleForced   bool   `json:"defaultSubtitleForced"`
		} `json:"profile"`
	}
	if json.Unmarshal(raw, &v) != nil {
		return TrackDefaults{}
	}
	src := v
	if v.Profile != nil {
		src.DefaultAudioLanguage = v.Profile.DefaultAudioLanguage
		src.AutoSelectAudio = v.Profile.AutoSelectAudio
		src.DefaultSubtitleLanguage = v.Profile.DefaultSubtitleLanguage
		src.AutoSelectSubtitle = v.Profile.AutoSelectSubtitle
		src.SubtitleMode = v.Profile.SubtitleMode
		src.DefaultSubtitleForced = v.Profile.DefaultSubtitleForced
	}
	mode := src.AutoSelectSubtitle
	if mode == 0 {
		mode = src.SubtitleMode
	}
	if src.DefaultAudioLanguage == "" && src.DefaultSubtitleLanguage == "" && !src.AutoSelectAudio && mode == 0 && !src.DefaultSubtitleForced {
		return TrackDefaults{}
	}
	return TrackDefaults{
		AudioLanguage:    strings.TrimSpace(src.DefaultAudioLanguage),
		AutoSelectAudio:  src.AutoSelectAudio,
		SubtitleLanguage: strings.TrimSpace(src.DefaultSubtitleLanguage),
		SubtitleMode:     mode,
		ForcedOnly:       src.DefaultSubtitleForced,
		HasProfile:       true,
	}
}

func optionalBoolAttr(n *xmlNode, names ...string) *bool {
	if n == nil {
		return nil
	}
	if raw := n.attr(names...); raw != "" {
		on := n.boolAttr(names...)
		return &on
	}
	return nil
}
