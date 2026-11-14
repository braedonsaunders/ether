package frontend

import "strings"

// LiveCategory keeps owner channel groups separate from programme categories.
// Unknown categories remain genres; only explicit provider classifications set
// the standard live programme flags.
func LiveCategory(genres []string, category, mediaType string) bool {
	if category == "movie" && mediaType == "movie" {
		return true
	}
	if category == "series" && mediaType == "episode" {
		return true
	}
	for _, g := range genres {
		g = strings.ToLower(strings.TrimSpace(g))
		switch category {
		case "movie":
			if g == "movie" || g == "movies" || g == "film" || g == "films" {
				return true
			}
		case "series":
			if g == "series" || g == "tv series" || g == "show" {
				return true
			}
		case "sports":
			if g == "sports" || g == "sport" {
				return true
			}
		case "news":
			if g == "news" {
				return true
			}
		case "kids":
			if g == "kids" || g == "children" || g == "children's" || g == "children’s" {
				return true
			}
		}
	}
	return false
}
