package silo

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/braedonsaunders/ether/internal/frontend"
)

func (i *instance) applyEditionOverlay(r *http.Request, viewer *frontend.Viewer, key frontend.RatingKey, item map[string]any) {
	if i.host.Editions == nil {
		return
	}
	versions, err := i.host.Editions.Editions(r.Context(), viewer, device(r), key)
	if err != nil || len(versions) == 0 {
		return
	}
	if summary := bestSiloOverlay(versions); len(summary) > 0 {
		item["overlay_summary"] = summary
	}
}

// bestSiloOverlay is the technical badge of the tallest probed file.
// Estimated media has no badges: a release name is not a probe.
func bestSiloOverlay(versions []frontend.Version) map[string]any {
	var best map[string]any
	bestHeight := -1
	for _, version := range versions {
		summary, height := siloOverlay(version)
		if height > bestHeight && len(summary) > 0 {
			best, bestHeight = summary, height
		}
	}
	return best
}

func siloOverlay(version frontend.Version) (map[string]any, int) {
	if version.Media.Estimated {
		return nil, -1
	}
	summary := map[string]any{}
	if version.Media.Container != "" {
		summary["container"] = strings.ToUpper(version.Media.Container)
	}
	if version.Edition != "" {
		summary["edition"] = version.Edition
	}
	height := -1
	var video frontend.StreamInfo
	var sawVideo bool
	languages := map[string]struct{}{}
	audioStreams := 0
	subtitles := 0
	for _, stream := range version.Media.Streams {
		switch stream.Type {
		case "video":
			if !sawVideo || stream.Height > video.Height {
				video = stream
				sawVideo = true
			}
		case "audio":
			audioStreams++
			if stream.Language != "" {
				languages[stream.Language] = struct{}{}
			}
			if _, ok := summary["audio"]; !ok && stream.Codec != "" {
				summary["audio"] = codecBadge(stream.Codec)
			}
			if _, ok := summary["audio_channels"]; !ok {
				if badge := channelBadge(stream.Channels, stream.ChannelLayout); badge != "" {
					summary["audio_channels"] = badge
				}
			}
		case "subtitle":
			subtitles++
		}
	}
	if sawVideo {
		if video.Height > 0 {
			height = video.Height
			if label := resolutionLabel(video.Height); label != "" {
				summary["resolution"] = label
			}
		}
		if video.Codec != "" {
			summary["video_codec"] = codecBadge(video.Codec)
		}
		if video.DisplayAspectRatio != "" {
			summary["aspect_ratio"] = video.DisplayAspectRatio
		}
		switch siloObservedRange(version.Media, video) {
		case "dolby_vision":
			summary["hdr"] = "Dolby Vision"
		case "hdr10_plus":
			summary["hdr"] = "HDR10+"
		case "hdr10":
			summary["hdr"] = "HDR10"
		case "hlg":
			summary["hdr"] = "HLG"
		}
	}
	if audioStreams > 1 && len(languages) == audioStreams && len(languages) >= 2 {
		summary["multi_audio"] = true
	}
	if subtitles > 0 {
		summary["multi_sub"] = true
	}
	if len(summary) == 0 {
		return nil, -1
	}
	return summary, height
}

func resolutionLabel(height int) string {
	if height <= 0 {
		return ""
	}
	return strconv.Itoa(height) + "p"
}

func codecBadge(codec string) string {
	switch strings.ToLower(codec) {
	case "h264":
		return "H.264"
	case "hevc", "h265":
		return "HEVC"
	case "aac":
		return "AAC"
	case "ac3":
		return "AC3"
	case "eac3":
		return "EAC3"
	case "mp3":
		return "MP3"
	default:
		return codec
	}
}

func channelBadge(channels int, layout string) string {
	switch strings.ToLower(layout) {
	case "mono":
		return "Mono"
	case "stereo":
		return "Stereo"
	case "5.1", "5.1(side)":
		return "5.1"
	case "7.1":
		return "7.1"
	}
	switch channels {
	case 1:
		return "1.0"
	case 2:
		return "Stereo"
	case 6:
		return "5.1"
	case 8:
		return "7.1"
	default:
		return ""
	}
}
