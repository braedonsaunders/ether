package mediabrowser

import (
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"

	"github.com/braedonsaunders/ether/internal/frontend"
)

// embyItemDTO adds facts that apply to both Emby list and detail items.
func (i *instance) embyItemDTO(m map[string]any, t frontend.Title, _ frontend.WatchStatus) {
	// Ether does not support editing or locking catalog metadata in Emby.
	m["LockData"] = false
	m["LockedFields"] = []string{}
	if i.host.Preferences != nil && t.Key != 0 {
		// Item IDs are stable, viewer-scoped preference keys on this surface.
		m["DisplayPreferencesId"] = m["Id"]
	}
	if t.SortTitle != "" {
		m["ForcedSortName"] = t.SortTitle
	}
	urls := []any{}
	if id := t.IDs.IMDB; len(id) > 2 && strings.HasPrefix(id, "tt") && strings.Trim(id[2:], "0123456789") == "" {
		urls = append(urls, map[string]string{"Name": "IMDb", "Url": "https://www.imdb.com/title/" + id + "/"})
	}
	if t.IDs.TMDB > 0 {
		kind := ""
		switch t.Type {
		case "movie":
			kind = "movie"
		case "show":
			kind = "tv"
		}
		if kind != "" {
			urls = append(urls, map[string]string{"Name": "The Movie Database", "Url": "https://www.themoviedb.org/" + kind + "/" + strconv.FormatInt(t.IDs.TMDB, 10)})
		}
	}
	if len(urls) > 0 {
		m["ExternalUrls"] = urls
	}
	// List and detail responses both carry these collections. An empty value
	// means this item has no such asset.
	if _, ok := m["BackdropImageTags"]; !ok {
		m["BackdropImageTags"] = []any{}
	}
	if _, ok := m["ImageTags"]; !ok {
		m["ImageTags"] = map[string]any{}
	}
}

func embyETag(id string) string {
	sum := sha256.Sum256([]byte("emby:" + id))
	return hex.EncodeToString(sum[:16])
}

// embyItemDetailDTO only describes the selected source when it is known.
func (i *instance) embyItemDetailDTO(m map[string]any, d frontend.ItemDetail) {
	m["SupportsSync"] = false
	trailers := 0
	for _, extra := range d.Extras {
		if strings.EqualFold(extra.Kind, "trailer") {
			trailers++
		}
	}
	m["LocalTrailerCount"] = trailers
	if remote := jellyfinRemoteTrailers(d); len(remote) > 0 {
		m["RemoteTrailers"] = remote
	}
	for _, key := range []string{"RemoteTrailers", "People", "Studios", "TagItems", "Taglines", "GenreItems"} {
		if _, ok := m[key]; !ok {
			m[key] = []any{}
		}
	}
	if id, ok := m["Id"].(string); ok && id != "" {
		m["PresentationUniqueKey"] = id
		m["Etag"] = embyETag(id)
	}
	if container, ok := m["Container"].(string); ok {
		if mime, ok := embyVideoMime(container); ok {
			m["MimeType"] = mime
		}
	}
	v, ok := embyDetailVersion(d.Versions)
	if !ok {
		return
	}
	if v.DateModified != nil && !v.DateModified.IsZero() && len(d.Versions) == 1 {
		m["DateModified"] = wireTime(*v.DateModified)
	}
	if v.FileName != "" && v.FileName != "." && v.FileName != ".." && !strings.ContainsAny(v.FileName, "/\\\x00\r\n") {
		m["FileName"] = v.FileName
	}
	if v.PartCount != nil && *v.PartCount >= 0 {
		m["PartCount"] = *v.PartCount
	}
	if v.Media.Estimated {
		return
	}
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
	if v.Media.Container != "" {
		m["Container"] = v.Media.Container
		if mime, ok := embyVideoMime(v.Media.Container); ok && v.Media.Video() != nil {
			m["MimeType"] = mime
		}
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
	if len(v.Media.Streams) > 0 {
		m["MediaStreams"] = i.mediaStreams(v.Media.Streams)
	}
}

func embyVideoMime(container string) (string, bool) {
	switch strings.ToLower(container) {
	case "mp4", "m4v":
		return "video/mp4", true
	case "mkv", "matroska":
		return "video/x-matroska", true
	case "webm":
		return "video/webm", true
	default:
		return "", false
	}
}

// embyDetailVersion is the one version when the item has a single candidate.
// Several probed pictures still have one Emby item body: the highest-bitrate
// video, then the taller picture, then the larger file. That is the file Emby
// 4.10.2 puts on the movie the library lists first. A size without a video
// stream is not a selection.
func embyDetailVersion(versions []frontend.Version) (frontend.Version, bool) {
	if len(versions) == 1 {
		return versions[0], true
	}
	found := false
	var best frontend.Version
	for _, version := range versions {
		if version.Media.Estimated || version.Media.Video() == nil {
			continue
		}
		if !found || embyRicherVersion(version, best) {
			best = version
			found = true
		}
	}
	return best, found
}

func embyRicherVersion(a, b frontend.Version) bool {
	if a.Media.BitrateKbps != b.Media.BitrateKbps {
		return a.Media.BitrateKbps > b.Media.BitrateKbps
	}
	ah, bh := 0, 0
	if video := a.Media.Video(); video != nil {
		ah = video.Height
	}
	if video := b.Media.Video(); video != nil {
		bh = video.Height
	}
	if ah != bh {
		return ah > bh
	}
	return a.Media.Size > b.Media.Size
}
