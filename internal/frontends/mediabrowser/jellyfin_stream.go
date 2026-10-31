package mediabrowser

import (
	"strconv"
	"strings"
	"sync"

	"golang.org/x/text/language"
	"golang.org/x/text/language/display"

	"github.com/braedonsaunders/ether/internal/frontend"
)

var (
	englishLanguages = sync.OnceValue(func() display.Namer { return display.English.Languages() })
	languageNames    sync.Map
)

// jellyfinStreamDTO exposes only probed values, preserving unknowns as absent.
func (i *instance) jellyfinStreamDTO(dto map[string]any, s frontend.StreamInfo) {
	streamMetadataDTO(dto, s)
	dto["AudioSpatialFormat"] = jellyfinAudioSpatialFormat(s)
	if s.Type == "audio" {
		dto["LocalizedDefault"] = "Default"
		dto["LocalizedExternal"] = "External"
		dto["LocalizedOriginal"] = "Original"
		dto["LocalizedLanguage"] = jellyfinLanguageName(s.Language)
	}
	if s.Type == "subtitle" {
		// Jellyfin sends these words on every subtitle stream. They name the
		// flag, including when the flag itself is false.
		dto["LocalizedDefault"] = "Default"
		dto["LocalizedExternal"] = "External"
		dto["LocalizedForced"] = "Forced"
		dto["LocalizedHearingImpaired"] = "Hearing Impaired"
		dto["LocalizedUndefined"] = "Undefined"
		dto["LocalizedLanguage"] = jellyfinLanguageName(s.Language)
	}
	if s.Type == "subtitle" && jellyfinTextSubtitle(s.Codec) {
		dto["IsTextSubtitleStream"] = true
	}
	if videoRange, videoRangeType, ok := jellyfinVideoRange(s); ok {
		dto["VideoRange"] = videoRange
		dto["VideoRangeType"] = videoRangeType
	}
	if s.Type == "video" && s.Codec != "" {
		codec := strings.ToLower(s.Codec)
		dto["IsAVC"] = codec == "h264" || codec == "avc" || codec == "avc1"
	}
	if anamorphic, ok := jellyfinAnamorphic(s); ok {
		dto["IsAnamorphic"] = anamorphic
	}
	if s.AverageFrameRate != nil {
		dto["AverageFrameRate"] = *s.AverageFrameRate
	}
	if s.ReferenceFrameRate != nil {
		dto["ReferenceFrameRate"] = *s.ReferenceFrameRate
		if _, ok := dto["RealFrameRate"]; !ok {
			dto["RealFrameRate"] = *s.ReferenceFrameRate
		}
	}
	for key, value := range map[string]string{"AspectRatio": s.DisplayAspectRatio, "PixelFormat": s.PixelFormat, "CodecTag": s.CodecTag, "TimeBase": s.TimeBase, "ColorRange": s.ColorRange, "ColorSpace": s.ColorSpace, "ColorTransfer": s.ColorTransfer, "ColorPrimaries": s.ColorPrimaries} {
		if value == "" {
			continue
		}
		// Jellyfin omits the default limited range. A full-range probe still
		// reports "pc".
		if key == "ColorRange" && (value == "tv" || value == "mpeg") {
			continue
		}
		dto[key] = value
	}
	if s.NALLengthSize != nil {
		dto["NalLengthSize"] = strconv.Itoa(*s.NALLengthSize)
	}
	if s.ReferenceFrames != nil {
		dto["RefFrames"] = *s.ReferenceFrames
	}
	if s.BitsPerRawSample != nil {
		dto["BitDepth"] = *s.BitsPerRawSample
	}
	if s.ProbedLevel != nil {
		dto["Level"] = *s.ProbedLevel
	}
	if s.ProbedInterlaced != nil {
		dto["IsInterlaced"] = *s.ProbedInterlaced
	}
}

func jellyfinTextSubtitle(codec string) bool {
	switch strings.ToLower(codec) {
	case "subrip", "srt", "ass", "ssa", "webvtt", "vtt", "mov_text", "text", "ttml":
		return true
	default:
		return false
	}
}

func jellyfinAudioSpatialFormat(s frontend.StreamInfo) string {
	if strings.Contains(strings.ToLower(s.ChannelLayout+" "+s.Title+" "+s.Codec), "atmos") {
		return "DolbyAtmos"
	}
	return "None"
}

// jellyfinVideoRange reports Jellyfin's picture-range enums only when the
// stream type or a completed probe establishes them. A video without an
// explicit negative Dolby Vision and HDR10+ result stays absent rather than
// being labelled SDR.
func jellyfinVideoRange(s frontend.StreamInfo) (string, string, bool) {
	if s.Type != "video" {
		return "Unknown", "Unknown", true
	}
	if s.DolbyVision || (s.DolbyVisionConfigurationPresent != nil && *s.DolbyVisionConfigurationPresent) {
		return "HDR", "DOVI", true
	}
	if s.HDR10Plus != nil && *s.HDR10Plus {
		return "HDR", "HDR10Plus", true
	}
	switch s.ColorTransfer {
	case "smpte2084":
		return "HDR", "HDR10", true
	case "arib-std-b67":
		return "HDR", "HLG", true
	}
	if jellyfinSDR(s) {
		return "SDR", "SDR", true
	}
	return "", "", false
}

// jellyfinSDR is true only with probe evidence. Explicit negative Dolby Vision
// and HDR10+ results are enough. An untagged 8-bit 4:2:0 frame is SDR when the
// container probe explicitly found no Dolby Vision configuration: those files
// omit color transfer and HDR10+ side data, and 8-bit 4:2:0 cannot carry
// HDR10, HLG, or Dolby Vision. A guessed estimate without that probe stays absent.
func jellyfinSDR(s frontend.StreamInfo) bool {
	if s.DolbyVision || s.DolbyVisionProfile > 0 || (s.ProbedDolbyVisionProfile != nil && *s.ProbedDolbyVisionProfile > 0) {
		return false
	}
	positive := func(v *bool) bool { return v != nil && *v }
	if positive(s.DolbyVisionConfigurationPresent) || positive(s.DolbyVisionBLCompatibilityIDPresent) || positive(s.DolbyVisionRPUPresent) || positive(s.DolbyVisionELPresent) || positive(s.DolbyVisionBLPresent) || positive(s.HDR10Plus) {
		return false
	}
	if s.DolbyVisionConfigurationPresent != nil && !*s.DolbyVisionConfigurationPresent && s.HDR10Plus != nil && !*s.HDR10Plus {
		return true
	}
	if s.DolbyVisionConfigurationPresent == nil || *s.DolbyVisionConfigurationPresent {
		return false
	}
	depth := s.BitDepth
	if s.BitsPerRawSample != nil && *s.BitsPerRawSample > 0 {
		depth = *s.BitsPerRawSample
	}
	if depth != 8 {
		return false
	}
	switch s.PixelFormat {
	case "yuv420p", "yuvj420p":
		return true
	default:
		return false
	}
}

func jellyfinAnamorphic(s frontend.StreamInfo) (bool, bool) {
	if s.Type != "video" {
		return false, false
	}
	parts := strings.Split(s.SampleAspectRatio, ":")
	if len(parts) != 2 {
		return false, false
	}
	n, errN := strconv.Atoi(parts[0])
	d, errD := strconv.Atoi(parts[1])
	if errN != nil || errD != nil || n <= 0 || d <= 0 || n > 100000 || d > 100000 {
		return false, false
	}
	return n != d, true
}

func jellyfinLanguageName(code string) string {
	code = strings.TrimSpace(code)
	if code == "" || strings.EqualFold(code, "und") || strings.EqualFold(code, "undetermined") {
		return "Undetermined"
	}
	if cached, ok := languageNames.Load(code); ok {
		return cached.(string)
	}
	tag, err := language.Parse(code)
	name := code
	if err == nil {
		if parsed := englishLanguages().Name(tag); parsed != "" {
			name = parsed
		}
	}
	languageNames.Store(code, name)
	return name
}
