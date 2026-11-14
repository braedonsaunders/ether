package app

import (
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontends/mediabrowser"
	"github.com/braedonsaunders/ether/internal/frontends/plex"
	"github.com/braedonsaunders/ether/internal/frontends/silo"
	"github.com/braedonsaunders/ether/internal/frontends/stremio"
	"github.com/braedonsaunders/ether/internal/frontends/vela"
	"github.com/braedonsaunders/ether/internal/frontends/xtream"
)

type libraryPresentation struct {
	native  func(core.MediaType) string
	project func(core.Presentations, core.MediaType) (core.MediaType, string, bool)
}

// Adapter function wiring; presentation behavior stays with the adapter.
var libraryPresentations = map[core.Protocol]libraryPresentation{
	"plex":     {plex.NativeLibraryType, plex.ProjectLibrary},                 // Main adapter wiring.
	"jellyfin": {mediabrowser.NativeLibraryType, mediabrowser.ProjectLibrary}, // Native adapter wiring.
	"emby":     {mediabrowser.NativeLibraryType, mediabrowser.ProjectLibrary}, // Native adapter wiring.
	"silo":     {silo.NativeLibraryType, silo.ProjectLibrary},
	"vela":     {vela.NativeLibraryType, vela.ProjectLibrary},
	"stremio":  {stremio.NativeLibraryType, stremio.ProjectLibrary},
	"xtream":   {xtream.NativeLibraryType, xtream.ProjectLibrary},
	"dlna":     {plex.NativeLibraryType, fileLibraryPresentation},
	"webdav":   {plex.NativeLibraryType, fileLibraryPresentation},
	"m3u":      {plex.NativeLibraryType, fileLibraryPresentation},
}

func builtinNative(mt core.MediaType, protocol core.Protocol) string {
	if adapter, ok := libraryPresentations[protocol]; ok {
		return adapter.native(mt)
	}
	return plex.NativeLibraryType(mt)
}
func presentLibraryForProtocol(p core.Presentations, protocol core.Protocol, fallback core.MediaType) (core.MediaType, string, bool) {
	if p.AdapterOnly {
		return "", "", false
	}
	if fallback == "" {
		fallback = core.TypeMovie
	}
	if adapter, ok := libraryPresentations[protocol]; ok {
		return adapter.project(p, fallback)
	}
	return "", "", false
}
func fileLibraryPresentation(p core.Presentations, fallback core.MediaType) (core.MediaType, string, bool) {

	if p.MediaBrowser != nil && p.MediaBrowser.CollectionType != "" {
		switch p.MediaBrowser.CollectionType {
		case "movies":
			return core.TypeMovie, "movies", true
		case "tvshows":
			return core.TypeShow, "tvshows", true
		}
	}
	if p.Plex != nil && p.Plex.Base != "" {
		switch p.Plex.Base {
		case "movie":
			return core.TypeMovie, "movie", true
		case "show":
			return core.TypeShow, "show", true
		}
	}
	return "", "", false

}
