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
