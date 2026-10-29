package mediabrowser

import "github.com/braedonsaunders/ether/internal/frontend"

// embyItemDTO adds facts that apply to both Emby list and detail items.
func (i *instance) embyItemDTO(m map[string]any, t frontend.Title, _ frontend.WatchStatus) {
	// Ether does not support editing or locking catalog metadata in Emby.
	m["LockData"] = false
	m["LockedFields"] = []string{}
	if t.SortTitle != "" {
		m["ForcedSortName"] = t.SortTitle
	}
}

// embyItemDetailDTO only describes the selected source when it is known.
func (i *instance) embyItemDetailDTO(m map[string]any, d frontend.ItemDetail) {
	m["SupportsSync"] = false
	// The Host does not identify a selected version. Multiple alternatives do
	// not establish which one supplies the top-level media properties.
	if len(d.Versions) != 1 {
		return
	}
	v := d.Versions[0]
	if len(v.Media.Chapters) > 0 {
		chapters := make([]any, 0, len(v.Media.Chapters))
		for index, chapter := range v.Media.Chapters {
			entry := map[string]any{"ChapterIndex": index, "MarkerType": "Chapter", "StartPositionTicks": chapter.Start.Nanoseconds() / 100}
			if chapter.Title != "" {
				entry["Name"] = chapter.Title
			}
			chapters = append(chapters, entry)
		}
		m["Chapters"] = chapters
	}
	if !v.AddedAt.IsZero() {
		m["DateCreated"] = v.AddedAt.UTC()
	}
	if v.Media.Container != "" {
		m["Container"] = v.Media.Container
	}
	if v.Media.Size > 0 {
		m["Size"] = v.Media.Size
	}
	if v.Media.BitrateKbps > 0 {
		m["Bitrate"] = v.Media.BitrateKbps * 1000
	}
	if video := v.Media.Video(); video != nil {
		if video.Width > 0 {
			m["Width"] = video.Width
		}
		if video.Height > 0 {
			m["Height"] = video.Height
		}
	}
}
