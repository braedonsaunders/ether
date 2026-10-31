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

// jellyfinDefaultStreamIndexes reports a default only when the probe names
// one, or when the file has a single audio stream. Several unmarked audio
// or subtitle streams stay absent: Jellyfin's -1 subtitle index is sent only
// when subtitle streams exist and none of them is the default.
func jellyfinDefaultStreamIndexes(source map[string]any, media frontend.MediaInfo) {
	audioIndex, audioDefaults, audioCount, soleAudio := 0, 0, 0, 0
	subtitleIndex, subtitleDefaults, subtitleCount := 0, 0, 0
	for _, stream := range media.Streams {
		selected := stream.Default
		if stream.DefaultDisposition != nil {
			selected = *stream.DefaultDisposition
		}
		switch stream.Type {
		case "audio":
			audioCount++
			soleAudio = stream.Index
			if selected {
				audioDefaults++
				audioIndex = stream.Index
			}
		case "subtitle":
			subtitleCount++
			if selected {
				subtitleDefaults++
				subtitleIndex = stream.Index
			}
		}
	}
	switch {
	case audioDefaults == 1:
		source["DefaultAudioStreamIndex"] = audioIndex
	case audioDefaults == 0 && audioCount == 1:
		source["DefaultAudioStreamIndex"] = soleAudio
	}
	switch {
	case subtitleDefaults == 1:
		source["DefaultSubtitleStreamIndex"] = subtitleIndex
	case subtitleDefaults == 0 && subtitleCount > 0:
		source["DefaultSubtitleStreamIndex"] = -1
	}
}
