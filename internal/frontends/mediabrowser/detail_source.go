package mediabrowser

import "github.com/braedonsaunders/ether/internal/frontend"

// detailMediaSourceDTO does not describe guessed streams as probed source
// facts. Estimates may contain default codecs, dimensions and even a guessed
// file size. PlaybackInfo can describe the actual source after resolution.
func (i *instance) detailMediaSourceDTO(id string, media frontend.MediaInfo) map[string]any {
	if !media.Estimated {
		return i.mediaSourceDTO(id, media)
	}
	source := i.mediaSourceDTO(id, frontend.MediaInfo{Estimated: true})
	for _, field := range []string{"Container", "RunTimeTicks", "Size", "Bitrate", "Chapters"} {
		delete(source, field)
	}
	return source
}
