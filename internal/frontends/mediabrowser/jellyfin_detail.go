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
}
