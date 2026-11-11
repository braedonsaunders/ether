package core

import "time"

// LibraryID identifies one library. 1 = Movies, 2 = TV Shows forever;
// ids are never reused.
type LibraryID int64

const (
	LibraryMovies LibraryID = 1
	LibraryShows  LibraryID = 2
)

// LibrarySource says where a library's items come from.
type LibrarySource string

const (
	LibraryBuiltin LibrarySource = "builtin" // the TMDB catalogue, split by MediaType
	LibraryPlugin  LibrarySource = "plugin"  // items owned by one plugin MediaKind instance
	LibraryFilter  LibrarySource = "filter"  // owner-saved Filter over the TMDB catalogue
)

// Library is the one library model for builtins, plugin kinds and
// owner filter-libraries alike (spec §1.1).
type Library struct {
	ID     LibraryID     `json:"id"`
	Key    string        `json:"key"`  // slug, unique, immutable: "movies", "shows", "pk-<plugin>-<kind>", owner slug
	Name   string        `json:"name"` // owner-editable display name
	Source LibrarySource `json:"source"`
	// Builtin only: TypeMovie or TypeShow.
	MediaType MediaType `json:"mediaType,omitempty"`
	// Plugin only.
	PluginID string `json:"pluginId,omitempty"`
	KindKey  string `json:"kindKey,omitempty"` // MediaKind.Key within that plugin
	// Filter only: the saved browse predicate, stored verbatim and fed to
	// catalog browseWhere. Type inside Filter pins movie|show.
	Filter *Filter `json:"filter,omitempty"`
	// Visibility rules for TMDB-sourced libraries. Built-in libraries 1 and 2
	// govern canonical movie/show visibility across every protocol; filter
	// libraries may carry additional rules on top of their canonical library.
	Rules             []LibraryExclusion `json:"rules,omitempty"`
	VisibilityDefault string             `json:"visibilityDefault,omitempty"`
	// Presentation: how protocols expose this library (§6.2). For builtins
	// this is synthesised, not stored.
	Present Presentations `json:"present,omitempty"`
	// QualityProfile is the owner-set quality profile name titles in this
	// library rank with. Empty means "Default": today's Movies/Shows
	// home/away defaults apply. Set on any library, including filter
	// libraries (an "Anime" library can rank with an anime profile).
	QualityProfile string    `json:"qualityProfile,omitempty"`
	Sort           int       `json:"sort"`   // owner order in every client
	Hidden         bool      `json:"hidden"` // owner hid it from all clients (still in console)
	CreatedAt      time.Time `json:"createdAt"`
	UpdatedAt      time.Time `json:"updatedAt"`
}

// Presentations declares how a library is published per protocol (§6.2).
// Declared by the plugin for plugin libraries, editable by the owner per
// library afterwards; a zero value publishes nowhere (console-only).
type Presentations struct {
	AdapterOnly bool `json:"adapterOnly,omitempty"` // only via FrontendAdapter (§6.5)
	Plex        *struct {
		Base string `json:"base"` // movie|show|artist|photo  (Plex section type / agent family)
		// level mapping for Base=show: root->show, mid->season, leaf->episode; movie: root|leaf->movie
	} `json:"plex,omitempty"`
	MediaBrowser *struct { // Jellyfin + Emby share internal/frontends/mediabrowser
		CollectionType string            `json:"collectionType"`      // movies|tvshows|music|audiobooks|books|photos|homevideos
		ItemTypes      map[string]string `json:"itemTypes,omitempty"` // level -> Movie|Series|Season|Episode|MusicAlbum|Audio|Book|PhotoAlbum|Photo
	} `json:"mediaBrowser,omitempty"`
	Stremio *struct {
		Type string `json:"type"` // movie|series|channel|tv — closed Stremio set; other kinds use their own add-on surface instead
	} `json:"stremio,omitempty"`
	Xtream *struct {
		Category string `json:"category"`
	} `json:"xtream,omitempty"` // movie|series|live
	Silo *struct {
		Type string `json:"type"`
	} `json:"silo,omitempty"` // mirrors MediaBrowser item family
}

// CanonicalLibraryForTitle returns the one canonical library of a TMDB
// title (§1.2): TypeMovie → 1; TypeShow/Season/Episode → 2. Any other
// type has no canonical library and returns 0.
func CanonicalLibraryForTitle(t MediaType) LibraryID {
	switch t {
	case TypeMovie:
		return LibraryMovies
	case TypeShow, TypeSeason, TypeEpisode:
		return LibraryShows
	default:
		return 0
	}
}

// AllowsLibrary reports whether u may open library id (§1.4): the owner
// and a viewer with no restriction see every library; otherwise the
// library must be listed. A nil user sees nothing.
func (u *User) AllowsLibrary(id LibraryID) bool {
	if u == nil {
		return false
	}
	if u.IsOwner || len(u.AllowedLibraries) == 0 {
		return true
	}
	for _, allowed := range u.AllowedLibraries {
		if allowed == id {
			return true
		}
	}
	return false
}
