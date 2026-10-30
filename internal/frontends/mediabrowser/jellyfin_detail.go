package mediabrowser

import "github.com/braedonsaunders/ether/internal/frontend"

// jellyfinItemDetailDTO reports only the host's policy-backed estimate.
func (i *instance) jellyfinItemDetailDTO(detail frontend.ItemDetail, dto map[string]any) {
	if detail.CanPlay != nil {
		if *detail.CanPlay {
			dto["PlayAccess"] = "Full"
		} else {
			dto["PlayAccess"] = "None"
		}
	}
	if detail.Title.Type != "movie" && detail.Title.Type != "episode" {
		return
	}
	// Recorded Jellyfin detail responses include these keys for video items
	// even when the library has no chapter list or trickplay tiles.
	dto["Trickplay"] = map[string]any{}
	if _, ok := dto["Chapters"]; !ok {
		if sources, ok := dto["MediaSources"].([]any); ok {
			for _, entry := range sources {
				source, ok := entry.(map[string]any)
				if !ok {
					continue
				}
				if chapters, ok := source["Chapters"]; ok {
					dto["Chapters"] = chapters
					break
				}
			}
		}
	}
	if _, ok := dto["Chapters"]; !ok {
		dto["Chapters"] = []any{}
	}
	if width, height, ok := jellyfinVideoSize(dto, detail); ok {
		dto["Width"] = width
		dto["Height"] = height
		// Jellyfin's item flag: either edge at or above this size is HD.
		dto["IsHD"] = width >= 1200 || height >= 700
	}
}

func jellyfinVideoSize(dto map[string]any, detail frontend.ItemDetail) (int, int, bool) {
	if streams, ok := dto["MediaStreams"].([]any); ok {
		for _, entry := range streams {
			stream, ok := entry.(map[string]any)
			if !ok || stream["Type"] != "Video" {
				continue
			}
			width, widthOK := jellyfinInt(stream["Width"])
			height, heightOK := jellyfinInt(stream["Height"])
			if widthOK && heightOK && width > 0 && height > 0 {
				return width, height, true
			}
		}
	}
	for _, stream := range detail.Tracks {
		if stream.Type == "video" && stream.Width > 0 && stream.Height > 0 {
			return stream.Width, stream.Height, true
		}
	}
	return 0, 0, false
}

func jellyfinInt(value any) (int, bool) {
	switch n := value.(type) {
	case int:
		return n, true
	case int64:
		return int(n), true
	default:
		return 0, false
	}
}
