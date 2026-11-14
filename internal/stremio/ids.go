package stremio

import (
	"context"
	"fmt"

	"github.com/braedonsaunders/ether/internal/core"
)

// Lookup is how to ask an add-on's stream route about one search: its
// content type ("movie" or "series") and id, whether only whole-season
// packs are wanted, and, for an anime database id, the episode's number
// inside that entry.
type Lookup struct {
	Type           string
	ID             string
	Packs          bool
	SpecialEpisode int // mapped IMDb special number, zero for other lookups
	EntryEpisode   int
}

// Route is the stream route path for the lookup.
func (l Lookup) Route() string {
	return "/" + ResourceStream + "/" + l.Type + "/" + escapeID(l.ID) + ".json"
}

// LookupStreams chooses how to ask the add-on for req's streams: by IMDb id
// when it takes them, else TMDB, else TVDB, else (for anime) an anime
// database id the community mapping knows unambiguously. A season request
// (Episode 0) asks for the season's first episode and keeps only packs.
// ok is false when the add-on cannot be asked, which skips it for this
// title. anime may be nil (no anime lookups).
func LookupStreams(ctx context.Context, m Manifest, req core.SearchRequest, anime AnimeIDs, specials SpecialEpisodes) (Lookup, bool) {
	movie := req.Type == core.TypeMovie
	switch {
	case movie:
	case req.Type == core.TypeEpisode || req.Type == core.TypeSeason:
		if req.Season < 0 || (req.Season == 0 && (req.Type != core.TypeEpisode || req.Episode <= 0)) {
			return Lookup{}, false
		}
	default:
		return Lookup{}, false
	}
	typ := TypeMovie
	if !movie {
		typ = TypeSeries
	}
	if !m.Serves(ResourceStream, typ) {
		return Lookup{}, false
	}
	ep, packs := req.Episode, false
	if !movie && ep <= 0 {
		ep, packs = 1, true
	}
	ask := func(base string) Lookup {
		if movie {
			return Lookup{Type: typ, ID: base}
		}
		return Lookup{Type: typ, ID: fmt.Sprintf("%s:%d:%d", base, req.Season, ep), Packs: packs}
	}
	if imdb := IMDbID(req.IDs.IMDB); imdb != "" && m.takesPrefix(ResourceStream, "tt") {
		if req.Season == 0 && !movie {
			if specials == nil {
				return Lookup{}, false
			}
			mapped, ok := specials.LookupSpecial(ctx, req)
			if !ok || mapped <= 0 {
				return Lookup{}, false
			}
			look := Lookup{Type: typ, ID: fmt.Sprintf("%s:0:%d", imdb, mapped), SpecialEpisode: mapped}
			return look, true
		}
		return ask(imdb), true
	}
	if req.IDs.TMDB > 0 && m.takesPrefix(ResourceStream, "tmdb") {
		return ask(fmt.Sprintf("tmdb:%d", req.IDs.TMDB)), true
	}
	if !movie && req.Season == 0 {
		return Lookup{}, false
	}
	if req.IDs.TVDB > 0 && m.takesPrefix(ResourceStream, "tvdb") {
		return ask(fmt.Sprintf("tvdb:%d", req.IDs.TVDB)), true
	}
	// Anime databases number each season or cour separately; whole-season
	// pack requests and non-anime titles never load the mapping.
	if !req.Anime || packs || anime == nil {
		return Lookup{}, false
	}
	for _, db := range animeDatabases {
		if !m.takesPrefix(ResourceStream, db) {
			continue
		}
		id, episode, ok, err := anime.Lookup(ctx, req, db)
		if err != nil || !ok {
			continue
		}
		if movie {
			return Lookup{Type: typ, ID: fmt.Sprintf("%s:%d", db, id)}, true
		}
		return Lookup{Type: typ, ID: fmt.Sprintf("%s:%d:%d", db, id, episode), EntryEpisode: episode}, true
	}
	return Lookup{}, false
}

// IMDbID normalises an IMDb id to "tt" + digits, or "".
func IMDbID(id string) string { return imdbID(id) }

// TitleID is the content id the protocol uses for a catalogue title:
// IMDb when known, else tmdb:N. Episodes add ":season:episode" to their
// show's id. ok is false when the title has neither id.
func TitleID(t *core.Title) (typ, id string, ok bool) {
	switch t.Type {
	case core.TypeMovie:
		typ = TypeMovie
		if imdb := IMDbID(t.IDs.IMDB); imdb != "" {
			return typ, imdb, true
		}
		if t.IDs.TMDB > 0 {
			return typ, fmt.Sprintf("tmdb:%d", t.IDs.TMDB), true
		}
	case core.TypeEpisode:
		typ = TypeSeries
		base := IMDbID(t.ShowIDs.IMDB)
		if base == "" && t.ShowIDs.TMDB > 0 {
			base = fmt.Sprintf("tmdb:%d", t.ShowIDs.TMDB)
		}
		if base != "" {
			return typ, fmt.Sprintf("%s:%d:%d", base, t.Season, t.Episode), true
		}
	case core.TypeShow:
		typ = TypeSeries
		if imdb := IMDbID(t.IDs.IMDB); imdb != "" {
			return typ, imdb, true
		}
		if t.IDs.TMDB > 0 {
			return typ, fmt.Sprintf("tmdb:%d", t.IDs.TMDB), true
		}
	}
	return "", "", false
}
