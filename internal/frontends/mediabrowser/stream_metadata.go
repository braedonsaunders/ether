package mediabrowser

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/braedonsaunders/ether/internal/frontend"
)

// streamMetadataDTO formats source facts only. Callers suppress estimated
// streams before conversion; this helper neither probes nor changes a plan.
func streamMetadataDTO(dto map[string]any, s frontend.StreamInfo) {
	if ratio := streamAspectRatio(s); ratio != "" {
		dto["AspectRatio"] = ratio
	}
	title := mediaStreamDisplayTitle(s)
	if title != "" {
		dto["DisplayTitle"] = title
	}
}

// mediaStreamDisplayTitle follows Jellyfin's MediaStream.DisplayTitle, checked
// against the lab server for the same stream shape. Audio and subtitles list
// the localized language, then flags, then the codec. When the file stored a
// track title, that title comes first and a later part is kept only if the
// title does not already contain it. Video stays a literal codec and size: a
// marketed resolution class needs range evidence this helper does not invent.
func mediaStreamDisplayTitle(s frontend.StreamInfo) string {
	switch s.Type {
	case "audio":
		return joinDisplayTitle(s.Title, audioDisplayAttributes(s))
	case "subtitle":
		return joinDisplayTitle(s.Title, subtitleDisplayAttributes(s))
	case "video":
		var parts []string
		if s.Title != "" {
			parts = append(parts, s.Title)
		}
		if s.Codec != "" {
			parts = append(parts, strings.ToUpper(s.Codec))
		}
		if s.Width > 0 && s.Height > 0 {
			parts = append(parts, fmt.Sprintf("%dx%d", s.Width, s.Height))
		}
		return strings.Join(parts, " - ")
	default:
		return ""
	}
}

func audioDisplayAttributes(s frontend.StreamInfo) []string {
	var attributes []string
	if label, ok := displayLanguage(s.Language); ok && !specialLanguage(s.Language) {
		attributes = append(attributes, label)
	}
	if s.Profile != "" && !strings.EqualFold(s.Profile, "lc") {
		attributes = append(attributes, s.Profile)
	} else if s.Codec != "" {
		attributes = append(attributes, friendlyAudioCodec(s.Codec))
	}
	if s.ChannelLayout != "" {
		attributes = append(attributes, firstUpper(s.ChannelLayout))
	} else if s.Channels > 0 {
		attributes = append(attributes, fmt.Sprintf("%d ch", s.Channels))
	}
	if streamFlag(s.Default, s.DefaultDisposition) {
		attributes = append(attributes, "Default")
	}
	if s.External {
		attributes = append(attributes, "External")
	}
	if s.Original != nil && *s.Original {
		attributes = append(attributes, "Original")
	}
	return attributes
}

func subtitleDisplayAttributes(s frontend.StreamInfo) []string {
	var attributes []string
	if label, ok := displayLanguage(s.Language); ok {
		attributes = append(attributes, label)
	} else {
		attributes = append(attributes, "Undefined")
	}
	if s.HearingImpaired != nil && *s.HearingImpaired {
		attributes = append(attributes, "Hearing Impaired")
	}
	if streamFlag(s.Default, s.DefaultDisposition) {
		attributes = append(attributes, "Default")
	}
	if streamFlag(s.Forced, s.ForcedDisposition) {
		attributes = append(attributes, "Forced")
	}
	if s.Codec != "" {
		attributes = append(attributes, strings.ToUpper(s.Codec))
	}
	if s.External {
		attributes = append(attributes, "External")
	}
	return attributes
}

func joinDisplayTitle(title string, attributes []string) string {
	if title == "" {
		return strings.Join(attributes, " - ")
	}
	parts := []string{title}
	for _, tag := range attributes {
		if tag == "" || strings.Contains(strings.ToLower(title), strings.ToLower(tag)) {
			continue
		}
		parts = append(parts, tag)
	}
	return strings.Join(parts, " - ")
}

func displayLanguage(code string) (string, bool) {
	code = strings.TrimSpace(code)
	if code == "" {
		return "", false
	}
	return firstUpper(jellyfinLanguageName(code)), true
}

func specialLanguage(code string) bool {
	switch strings.ToLower(strings.TrimSpace(code)) {
	case "mis", "mul", "und", "zxx":
		return true
	default:
		return false
	}
}

func streamFlag(set bool, override *bool) bool {
	if override != nil {
		return *override
	}
	return set
}

func firstUpper(s string) string {
	if s == "" {
		return ""
	}
	r := []rune(s)
	return strings.ToUpper(string(r[0])) + string(r[1:])
}

func friendlyAudioCodec(codec string) string {
	switch strings.ToLower(codec) {
	case "ac3":
		return "Dolby Digital"
	case "eac3":
		return "Dolby Digital+"
	case "dca", "dts":
		return "DTS"
	case "truehd":
		return "Dolby TrueHD"
	case "aac":
		return "AAC"
	case "mp3":
		return "MP3"
	case "flac":
		return "FLAC"
	case "alac":
		return "ALAC"
	case "opus":
		return "Opus"
	case "vorbis":
		return "Vorbis"
	case "wma":
		return "WMA"
	case "pcm", "pcm_s16le", "pcm_s24le", "pcm_s32le":
		return "PCM"
	default:
		return strings.ToUpper(codec)
	}
}

func streamAspectRatio(s frontend.StreamInfo) string {
	if s.Type != "video" {
		return ""
	}
	if s.DisplayAspectRatio != "" {
		return s.DisplayAspectRatio
	}
	// Never assume square pixels when SAR is absent. Bound multiplication and
	// use int64 so dimensions and sample aspect cannot overflow on 32-bit hosts.
	if s.Width <= 0 || s.Height <= 0 || s.Width > 100000 || s.Height > 100000 {
		return ""
	}
	parts := strings.Split(s.SampleAspectRatio, ":")
	if len(parts) != 2 {
		return ""
	}
	n, en := strconv.Atoi(parts[0])
	d, ed := strconv.Atoi(parts[1])
	if en != nil || ed != nil || n <= 0 || d <= 0 || n > 100000 || d > 100000 {
		return ""
	}
	w, h := int64(s.Width)*int64(n), int64(s.Height)*int64(d)
	a, b := w, h
	for b != 0 {
		a, b = b, a%b
	}
	return fmt.Sprintf("%d:%d", w/a, h/a)
}
