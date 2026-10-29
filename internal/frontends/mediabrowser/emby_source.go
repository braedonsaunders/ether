package mediabrowser

import (
	"strings"

	"github.com/braedonsaunders/ether/internal/frontend"
)

func (i *instance) embyMediaSourceDTO(source map[string]any, itemID string, media frontend.MediaInfo) {
	if itemID != "" {
		source["ItemId"] = itemID
	}
	// Containers can hold audio alone; a video stream is required before
	// describing the source as video.
	if media.Video() == nil {
		return
	}
	switch strings.ToLower(media.Container) {
	case "mp4", "m4v":
		source["MimeType"] = "video/mp4"
	case "mkv":
		source["MimeType"] = "video/x-matroska"
	case "webm":
		source["MimeType"] = "video/webm"
	}
}
