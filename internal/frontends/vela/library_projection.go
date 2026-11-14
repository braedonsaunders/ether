package vela

import "github.com/braedonsaunders/ether/internal/frontend"

func NativeLibraryType(mt frontend.MediaType) string {
	if mt == frontend.TypeShow {
		return "shows"
	}
	return "movies"
}

// Vela uses the neutral movie/show model for libraries the owner already
// publishes to native video clients. Console-only and adapter-only sources
// remain unpublished; visibility is still applied by Experience.Views.
func ProjectLibrary(p frontend.Presentations, fallback frontend.MediaType) (frontend.MediaType, string, bool) {
	if p.AdapterOnly {
		return "", "", false
	}
	if p.MediaBrowser != nil {
		switch p.MediaBrowser.CollectionType {
		case "movies":
			return frontend.TypeMovie, "movies", true
		case "tvshows":
			return frontend.TypeShow, "shows", true
		}
	}
	if p.Plex != nil {
		switch p.Plex.Base {
		case "movie":
			return frontend.TypeMovie, "movies", true
		case "show":
			return frontend.TypeShow, "shows", true
		}
	}
	return "", "", false
}
