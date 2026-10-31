package mediabrowser

import "github.com/braedonsaunders/ether/internal/frontend"

// jellyfinMediaSourceDTO adds MediaSourceInfo fields Jellyfin always serializes.
// VideoType is withheld for estimates, audio-only sources and live streams.
func (i *instance) jellyfinMediaSourceDTO(source map[string]any, media frontend.MediaInfo, live bool) {
	source["UseMostCompatibleTranscodingProfile"] = false
	if !media.Estimated {
		jellyfinDefaultStreamIndexes(source, media)
	}
	if live || media.Estimated || media.Video() == nil {
		return
	}
	source["VideoType"] = "VideoFile"
}

// jellyfinDefaultStreamIndexes follows Jellyfin when the container does not
// mark a default: the first audio stream, and -1 for subtitles so none is
// selected. A marked default wins, and the earliest mark wins when several
// streams are marked.
func jellyfinDefaultStreamIndexes(source map[string]any, media frontend.MediaInfo) {
	audioIndex, haveAudio, audioDefault := 0, false, false
	subtitleIndex, subtitleDefault, subtitleCount := 0, false, 0
	for _, stream := range media.Streams {
		selected := stream.Default
		if stream.DefaultDisposition != nil {
			selected = *stream.DefaultDisposition
		}
		switch stream.Type {
		case "audio":
			if !haveAudio {
				audioIndex = stream.Index
				haveAudio = true
			}
			if selected && !audioDefault {
				audioIndex = stream.Index
				audioDefault = true
			}
		case "subtitle":
			subtitleCount++
			if selected && !subtitleDefault {
				subtitleIndex = stream.Index
				subtitleDefault = true
			}
		}
	}
	if haveAudio {
		source["DefaultAudioStreamIndex"] = audioIndex
	}
	if subtitleDefault {
		source["DefaultSubtitleStreamIndex"] = subtitleIndex
	} else if subtitleCount > 0 {
		source["DefaultSubtitleStreamIndex"] = -1
	}
}

// jellyfinResumeSubtitle records the lab viewer's in-progress choice. Jellyfin
// stores -1 on a partially watched item when no subtitle stream is selected,
// and omits the field once the item is unplayed or finished.
func jellyfinResumeSubtitle(item map[string]any) {
	data, _ := item["UserData"].(map[string]any)
	if data == nil {
		return
	}
	if played, _ := data["Played"].(bool); played {
		return
	}
	ticks, ok := data["PlaybackPositionTicks"].(int64)
	if !ok {
		if wide, wideOK := data["PlaybackPositionTicks"].(float64); wideOK {
			ticks, ok = int64(wide), wide != 0
		}
	}
	if !ok || ticks <= 0 {
		return
	}
	sources, _ := item["MediaSources"].([]any)
	for _, source := range sources {
		dto, ok := source.(map[string]any)
		if !ok {
			continue
		}
		if _, exists := dto["DefaultSubtitleStreamIndex"]; !exists {
			dto["DefaultSubtitleStreamIndex"] = -1
		}
	}
}
