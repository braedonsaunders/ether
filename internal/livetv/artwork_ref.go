package livetv

import (
	"strings"

	"github.com/braedonsaunders/ether/internal/artwork"
	"github.com/braedonsaunders/ether/internal/core"
)

// artworkRefPrefix marks an On Air artwork reference that names a title's own
// poster or backdrop by key, for artwork that is not a bare TMDB image path
// (Plex-sourced art is stored as a sentinel and resolved per title).
const artworkRefPrefix = "title:"

// ArtworkRef returns the On Air reference for a programme image: a TMDB path
// unchanged, otherwise the title's artwork by key. element is "poster" or
// "art". An image with neither a path nor a title yields "".
func ArtworkRef(key core.RatingKey, path, element string) string {
	if path == "" {
		return ""
	}
	if artwork.ValidPath(path) {
		return path
	}
	if key == 0 {
		return ""
	}
	return artworkRefPrefix + key.String() + "/" + element
}

// ParseArtworkRef reverses ArtworkRef's title form.
func ParseArtworkRef(ref string) (core.RatingKey, string, bool) {
	rest, ok := strings.CutPrefix(ref, artworkRefPrefix)
	if !ok {
		return 0, "", false
	}
	raw, element, ok := strings.Cut(rest, "/")
	if !ok || (element != "poster" && element != "art") {
		return 0, "", false
	}
	key, err := core.ParseRatingKey(raw)
	if err != nil || key == 0 {
		return 0, "", false
	}
	return key, element, true
}
