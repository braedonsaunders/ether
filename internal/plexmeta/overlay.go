// Package plexmeta caches Plex metadata-provider details for Plex apps.
// TMDB stays the catalogue and the fallback. Viewer policy uses the safer
// (more mature) of the TMDB and Plex content ratings when this overlay is
// on; Plex apps still display Plex's own rating. Date-added stays the
// catalogue release/air date — Plex dates never change addedAt.
package plexmeta

import (
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/contentrating"
	"github.com/braedonsaunders/ether/internal/core"
)

// Overlay is one title's cached Plex metadata. Empty fields mean "keep TMDB".
type Overlay struct {
	ContentRating  string    `json:"contentRating,omitempty"`
	Year           int       `json:"year,omitempty"`
	AvailableAt    time.Time `json:"availableAt,omitempty"`
	Summary        string    `json:"summary,omitempty"`
	Tagline        string    `json:"tagline,omitempty"`
	Studio         string    `json:"studio,omitempty"`
	ThumbURL       string    `json:"thumbUrl,omitempty"`
	ArtURL         string    `json:"artUrl,omitempty"`
	Rating         float64   `json:"rating,omitempty"`
	AudienceRating float64   `json:"audienceRating,omitempty"`
	Genres         []string  `json:"genres,omitempty"`
	Cast           []Person  `json:"cast,omitempty"`
	Directors      []Person  `json:"directors,omitempty"`
	Writers        []Person  `json:"writers,omitempty"`
}

// Person is a Plex Role/Director/Writer tag.
type Person struct {
	Name  string `json:"name,omitempty"`
	Role  string `json:"role,omitempty"`
	Thumb string `json:"thumb,omitempty"`
}

// Cached is a stored overlay plus when it was fetched.
type Cached struct {
	Overlay
	FetchedAt time.Time
	GUID      string
}

// Empty reports a cache hit that carries no display fields (a recorded miss).
func (o Overlay) Empty() bool {
	return o.ContentRating == "" && o.Year == 0 && o.AvailableAt.IsZero() &&
		o.Summary == "" && o.Tagline == "" && o.Studio == "" &&
		o.ThumbURL == "" && o.ArtURL == "" && o.Rating == 0 && o.AudienceRating == 0 &&
		len(o.Genres) == 0 && len(o.Cast) == 0 && len(o.Directors) == 0 && len(o.Writers) == 0
}

// Stale reports that the row should be refreshed in the background.
func (c Cached) Stale(now time.Time) bool {
	return c.FetchedAt.IsZero() || now.Sub(c.FetchedAt) >= cacheTTL
}

// ApplyDisplay copies present Plex fields onto t for the Plex convert path.
// Released and AddedAt stay on the caller — Plex dates must not change
// date-added.
func ApplyDisplay(t *core.Title, o Overlay) {
	if t == nil || o.Empty() {
		return
	}
	if o.ContentRating != "" {
		t.ContentRating = o.ContentRating
	}
	if o.Year > 0 {
		t.Year = o.Year
	}
	if o.Summary != "" {
		t.Summary = o.Summary
	}
	if o.Tagline != "" {
		t.Tagline = o.Tagline
	}
	if o.Studio != "" {
		t.Studios = []string{o.Studio}
	}
	if o.AudienceRating > 0 {
		t.Rating = o.AudienceRating
	} else if o.Rating > 0 {
		t.Rating = o.Rating
	}
	if len(o.Genres) > 0 {
		t.Genres = append([]string(nil), o.Genres...)
	}
	if o.ThumbURL != "" {
		if t.Type == core.TypeEpisode {
			if t.StillPath == "" {
				t.StillPath = remoteArtSentinel
			}
		} else if t.PosterPath == "" {
			t.PosterPath = remoteArtSentinel
		}
	}
	if o.ArtURL != "" && t.BackdropPath == "" {
		t.BackdropPath = remoteArtSentinel
	}
	if people := overlayPeople(o.Cast); len(people) > 0 {
		t.Cast = people
	}
	if people := overlayPeople(o.Directors); len(people) > 0 {
		t.Directors = people
	}
	if people := overlayPeople(o.Writers); len(people) > 0 {
		t.Writers = people
	}
}

const remoteArtSentinel = "plex"

// SaferRating is the more mature of two stored ratings. An unrated side is
// ignored so a missing Plex label does not hide a rated TMDB title; if both
// are unrated the TMDB value is kept.
func SaferRating(tmdb, plex string) string {
	tmdb, plex = strings.TrimSpace(tmdb), strings.TrimSpace(plex)
	if plex == "" {
		return tmdb
	}
	if tmdb == "" {
		return plex
	}
	if contentrating.LevelOf(plex) > contentrating.LevelOf(tmdb) {
		return plex
	}
	return tmdb
}

func overlayPeople(in []Person) []core.Person {
	out := make([]core.Person, 0, len(in))
	for _, p := range in {
		if strings.TrimSpace(p.Name) == "" {
			continue
		}
		out = append(out, core.Person{Name: p.Name, Role: p.Role, ThumbPath: p.Thumb})
	}
	return out
}
