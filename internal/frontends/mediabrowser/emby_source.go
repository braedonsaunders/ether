package mediabrowser

import (
	"net/url"
	"strings"

	"github.com/braedonsaunders/ether/internal/frontend"
)

func (i *instance) embyMediaSourceDTO(source map[string]any, itemID string, media frontend.MediaInfo) {
	if path, ok := source["Path"].(string); ok && embyScopedDirectURL(path) {
		// Issued capability URLs authorize their own read and must not expose
		// the account credential in a query parameter.
		source["AddApiKeyToDirectStreamUrl"] = false
	}
	if itemID != "" {
		source["ItemId"] = itemID
	}
	// The common PlaybackInfo path may have populated this from a legacy
	// default bit; replace it only with an unambiguous authoritative source.
	delete(source, "DefaultAudioStreamIndex")
	if !media.Estimated {
		defaultIndex, defaults := 0, 0
		for _, stream := range media.Streams {
			if stream.Type != "audio" {
				continue
			}
			isDefault := stream.Default
			if stream.DefaultDisposition != nil {
				isDefault = *stream.DefaultDisposition
			}
			if isDefault {
				defaultIndex, defaults = stream.Index, defaults+1
			}
		}
		if defaults == 1 {
			source["DefaultAudioStreamIndex"] = defaultIndex
		}
	}
	// Containers can hold audio alone; a video stream is required before
	// describing the source as video.
	if media.Estimated || media.Video() == nil {
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

func embyScopedDirectURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.RawPath != "" {
		return false
	}
	if u.IsAbs() && ((u.Scheme != "http" && u.Scheme != "https") || u.Host == "") {
		return false
	}
	parts := strings.Split(strings.TrimPrefix(u.Path, "/"), "/")
	if len(parts) > 0 && strings.EqualFold(parts[0], "emby") {
		parts = parts[1:]
	}
	if len(parts) == 4 && parts[0] == "sessions" && parts[1] == "lazy" && parts[3] == "file" {
		return embySafeGrant(parts[2])
	}
	return len(parts) == 5 && parts[0] == "sessions" && parts[1] == "media" && parts[4] == "file" && embySafeGrant(parts[2]) && embySafeGrant(parts[3])
}

func embySafeGrant(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}
