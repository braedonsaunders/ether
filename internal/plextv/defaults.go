package plextv

import (
	"bytes"
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

// plex.tv defaultSubtitleForced is an enum, not a bool:
// 0 prefer non-forced, 1 prefer forced, 2 only forced, 3 only non-forced.
const plexForcedOnly = 2

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
// Forced-only is only defaultSubtitleForced=2 when the subtitle mode is off.
// Value 1 (prefer forced) must not turn subtitles on.
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
	audio := strings.TrimSpace(n.attr("defaultAudioLanguage"))
	if audio == "" {
		audio = firstCSV(n.attr("defaultAudioLanguages"))
	}
	sub := strings.TrimSpace(n.attr("defaultSubtitleLanguage"))
	if sub == "" {
		sub = firstCSV(n.attr("defaultSubtitleLanguages"))
	}
	return TrackDefaults{
		AudioLanguage:    audio,
		AutoSelectAudio:  n.boolAttr("autoSelectAudio"),
		SubtitleLanguage: sub,
		SubtitleMode:     parsed,
		ForcedOnly:       flexIntFromString(n.attr("defaultSubtitleForced")) == plexForcedOnly,
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

type jsonTrack struct {
	DefaultAudioLanguage     string          `json:"defaultAudioLanguage"`
	DefaultAudioLanguages    json.RawMessage `json:"defaultAudioLanguages"`
	AutoSelectAudio          bool            `json:"autoSelectAudio"`
	DefaultSubtitleLanguage  string          `json:"defaultSubtitleLanguage"`
	DefaultSubtitleLanguages json.RawMessage `json:"defaultSubtitleLanguages"`
	AutoSelectSubtitle       flexInt         `json:"autoSelectSubtitle"`
	SubtitleMode             flexInt         `json:"subtitleMode"`
	DefaultSubtitleForced    flexInt         `json:"defaultSubtitleForced"`
}

func trackDefaultsFromJSON(raw []byte) TrackDefaults {
	var v struct {
		jsonTrack
		Profile *jsonTrack `json:"profile"`
	}
	if json.Unmarshal(raw, &v) != nil {
		return TrackDefaults{}
	}
	src := v.jsonTrack
	if v.Profile != nil {
		src = *v.Profile
	}
	audio := strings.TrimSpace(src.DefaultAudioLanguage)
	if audio == "" {
		audio = firstLanguage(src.DefaultAudioLanguages)
	}
	sub := strings.TrimSpace(src.DefaultSubtitleLanguage)
	if sub == "" {
		sub = firstLanguage(src.DefaultSubtitleLanguages)
	}
	mode := int(src.AutoSelectSubtitle)
	if mode == 0 {
		mode = int(src.SubtitleMode)
	}
	forced := int(src.DefaultSubtitleForced)
	if audio == "" && sub == "" && !src.AutoSelectAudio && mode == 0 && forced == 0 {
		return TrackDefaults{}
	}
	return TrackDefaults{
		AudioLanguage:    audio,
		AutoSelectAudio:  src.AutoSelectAudio,
		SubtitleLanguage: sub,
		SubtitleMode:     mode,
		ForcedOnly:       forced == plexForcedOnly,
		HasProfile:       true,
	}
}

// flexInt accepts the JSON number plex.tv sends, a quoted digit from XML-shaped
// fixtures, and a bool from older fakes.
type flexInt int

func (f *flexInt) UnmarshalJSON(raw []byte) error {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		*f = 0
		return nil
	}
	switch raw[0] {
	case '"':
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return err
		}
		*f = flexInt(flexIntFromString(s))
		return nil
	case 't', 'T':
		*f = 1
		return nil
	case 'f', 'F':
		*f = 0
		return nil
	}
	var n int
	if err := json.Unmarshal(raw, &n); err != nil {
		return err
	}
	*f = flexInt(n)
	return nil
}

func flexIntFromString(s string) int {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		if strings.EqualFold(s, "true") {
			return 1
		}
		return 0
	}
	return n
}

func firstLanguage(raw json.RawMessage) string {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	if raw[0] == '"' {
		var s string
		if json.Unmarshal(raw, &s) != nil {
			return ""
		}
		return firstCSV(s)
	}
	var parts []string
	if json.Unmarshal(raw, &parts) != nil {
		return ""
	}
	for _, p := range parts {
		if t := strings.TrimSpace(p); t != "" {
			return t
		}
	}
	return ""
}

func firstCSV(s string) string {
	for _, p := range strings.Split(s, ",") {
		if t := strings.TrimSpace(p); t != "" {
			return t
		}
	}
	return ""
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
