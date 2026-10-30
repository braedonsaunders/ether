package mediabrowser

import "github.com/braedonsaunders/ether/internal/frontend"

// jellyfinMediaSourceDTO adds MediaSourceInfo fields Jellyfin always serializes.
// VideoType is withheld for estimates, audio-only sources and live streams.
func (i *instance) jellyfinMediaSourceDTO(source map[string]any, media frontend.MediaInfo, live bool) {
	source["UseMostCompatibleTranscodingProfile"] = false
	if live || media.Estimated || media.Video() == nil {
		return
	}
	source["VideoType"] = "VideoFile"
}
