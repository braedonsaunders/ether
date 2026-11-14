package frontend

import "strings"

// OriginalPlaybackCapabilities describes installed demux/decoder families, not
// native canPlayType results or a guarantee about a particular encoded profile.
// The client probes exact decoder configurations before consuming the source.
type OriginalPlaybackCapabilities struct {
	Version         int      `json:"version"`
	Range           bool     `json:"range"`
	DemuxContainers []string `json:"demuxContainers"`
	VideoDecoders   []string `json:"videoDecoders"`
	AudioDecoders   []string `json:"audioDecoders"`
	WebGPU          bool     `json:"webgpu"`
}

func (c *OriginalPlaybackCapabilities) Valid() bool {
	return c != nil && c.Version == 1 && c.Range && len(c.DemuxContainers) > 0 && len(c.VideoDecoders) > 0
}

func originalFamily(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "matroska", "x-matroska":
		return "mkv"
	case "mov", "m4v":
		return "mp4"
	case "h265", "h.265", "hev1", "hvc1":
		return "hevc"
	case "avc", "h.264", "avc1":
		return "h264"
	case "dca":
		return "dts"
	case "ac-3":
		return "ac3"
	case "ec-3":
		return "eac3"
	}
	return strings.ToLower(strings.TrimSpace(s))
}
func originalHas(families []string, value string) bool {
	for _, family := range families {
		if originalFamily(family) == originalFamily(value) {
			return true
		}
	}
	return false
}

// Supports requires observed source codecs and the selected source audio index.
// A declaration never enables live originals or guesses an unprobed codec.
func (c *OriginalPlaybackCapabilities) Supports(info MediaInfo, audioIndex int) bool {
	return c.supports(info, audioIndex, false)
}
func (c *OriginalPlaybackCapabilities) SupportsAudio(codec string) bool {
	return c != nil && originalHas(c.AudioDecoders, codec)
}
func (c *OriginalPlaybackCapabilities) SupportsVideo(info MediaInfo) bool {
	return c.supports(info, -1, true)
}
func (c *OriginalPlaybackCapabilities) supports(info MediaInfo, audioIndex int, audioFallback bool) bool {
	if !c.Valid() || info.Estimated || !originalHas(c.DemuxContainers, info.Container) {
		return false
	}
	video, audio, selected := false, false, false
	for _, s := range info.Streams {
		switch s.Type {
		case "video":
			if s.Codec == "" || !originalHas(c.VideoDecoders, s.Codec) {
				return false
			}
			video = true
		case "audio":
			audio = true
			if s.Index == audioIndex || audioIndex < 0 {
				if !audioFallback && (s.Codec == "" || !originalHas(c.AudioDecoders, s.Codec)) {
					return false
				}
				selected = true
			}
		}
	}
	return video && (!audio || selected)
}
