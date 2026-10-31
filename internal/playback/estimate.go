package playback

import (
	"context"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

// Default runtimes when TMDB has none.
const (
	defaultMovieRuntime   = 110 * time.Minute
	defaultEpisodeRuntime = 45 * time.Minute
)

// Estimate is the virtual media info for a title before it is resolved,
// derived from the quality profile chosen for the client and the title's
// runtime. Estimate has no user, so profile rules keyed on a user do not
// apply here.
func (m *Manager) Estimate(_ context.Context, t *core.Title, caps core.ClientCaps) core.MediaInfo {
	return EstimateInfo(t, caps, m.deps.SelectProfile(caps, nil))
}

// EstimateInfo builds the estimated MediaInfo for a title, client and
// profile: Matroska, H.264 (HEVC at 4K or when the client lacks H.264) at
// the profile height, a typical bitrate for that height kept inside the
// profile's size bounds, and E-AC-3 5.1 or AAC stereo audio.
func EstimateInfo(t *core.Title, caps core.ClientCaps, p core.QualityProfile) core.MediaInfo {
	height := p.MaxHeight
	if height <= 0 {
		height = 1080
	}
	if caps.MaxHeight > 0 && caps.MaxHeight < height {
		height = caps.MaxHeight
	}
	height = standardHeight(height)

	codec := videoCodecFor(height, caps, p)
	videoKbps := typicalVideoKbps(height, codec)

	audio := core.StreamInfo{Index: 1, Type: "audio", Default: true, SampleRate: 48000}
	if len(caps.AudioCodecs) == 0 || caps.SupportsAudio("eac3") {
		audio.Codec, audio.Channels, audio.ChannelLayout, audio.BitrateKbps = "eac3", 6, "5.1(side)", 640
	} else {
		audio.Codec, audio.Channels, audio.ChannelLayout, audio.BitrateKbps = "aac", 2, "stereo", 192
	}

	// Keep the total inside the profile's size sanity range (MB per minute).
	total := videoKbps + audio.BitrateKbps
	minMBPerMin, maxMBPerMin := p.SizeBoundsFor(t)
	if minMBPerMin > 0 {
		if lo := mbPerMinToKbps(minMBPerMin); total < lo {
			total = lo
		}
	}
	if maxMBPerMin > 0 {
		if hi := mbPerMinToKbps(maxMBPerMin); total > hi {
			total = hi
		}
	}
	if total-audio.BitrateKbps > 0 {
		videoKbps = total - audio.BitrateKbps
	}

	video := core.StreamInfo{
		Index:       0,
		Type:        "video",
		Codec:       codec,
		Width:       widthFor(height),
		Height:      height,
		FrameRate:   23.976,
		BitDepth:    8,
		Default:     true,
		BitrateKbps: videoKbps,
	}
	if codec == "h264" {
		video.Profile = "high"
	} else {
		video.Profile = "main"
	}
	if height >= 2160 && p.AllowHDR && caps.HDR10 {
		video.BitDepth = 10
		video.Profile = "main 10"
		video.ColorTransfer = "smpte2084"
	}

	var dur time.Duration
	if t != nil {
		dur = t.Runtime
		audio.Language = t.Language
	}
	if dur <= 0 {
		dur = defaultMovieRuntime
		if t != nil && t.Type == core.TypeEpisode {
			dur = defaultEpisodeRuntime
		}
	}
	size := int64(float64(total) * 1000 / 8 * dur.Seconds())

	return core.MediaInfo{
		Container:   "mkv",
		Duration:    dur,
		Size:        size,
		BitrateKbps: total,
		Streams:     []core.StreamInfo{video, audio},
		Estimated:   true,
	}
}

func standardHeight(h int) int {
	switch {
	case h >= 2160:
		return 2160
	case h >= 1080:
		return 1080
	case h >= 720:
		return 720
	case h >= 576:
		return 576
	default:
		return 480
	}
}

func widthFor(h int) int {
	switch {
	case h >= 2160:
		return 3840
	case h == 1080:
		return 1920
	case h == 720:
		return 1280
	case h == 576:
		return 1024
	default:
		return 854
	}
}

// videoCodecFor picks HEVC for 4K when the client plays it (or declares
// nothing), else H.264, else the first profile codec the client supports.
func videoCodecFor(height int, caps core.ClientCaps, p core.QualityProfile) string {
	ok := func(c string) bool { return len(caps.VideoCodecs) == 0 || caps.SupportsVideo(c) }
	if height >= 2160 && ok("hevc") {
		return "hevc"
	}
	if ok("h264") {
		return "h264"
	}
	for _, c := range p.PreferredCodecs {
		if caps.SupportsVideo(c) {
			return c
		}
	}
	if len(caps.VideoCodecs) > 0 {
		return caps.VideoCodecs[0]
	}
	return "h264"
}

// typicalVideoKbps is a representative bitrate of web and Blu-ray encodes.
func typicalVideoKbps(height int, codec string) int {
	var kbps int
	switch height {
	case 2160:
		kbps = 20000
	case 1080:
		kbps = 8000
	case 720:
		kbps = 4000
	case 576:
		kbps = 2500
	default:
		kbps = 1500
	}
	if codec == "hevc" && height < 2160 || codec == "av1" {
		kbps = kbps * 6 / 10
	}
	return kbps
}

func mbPerMinToKbps(mb float64) int { return int(mb * 1024 * 1024 * 8 / 60 / 1000) }
