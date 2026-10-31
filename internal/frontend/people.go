package frontend

import (
	"context"
	"errors"
)

type PeopleScope string

const (
	PeopleAll               PeopleScope = ""
	PeopleVideo             PeopleScope = "video"
	PeopleVideoWithEpisodes PeopleScope = "video_with_episodes"
	PeopleMovies            PeopleScope = "movie"
	PeopleSeries            PeopleScope = "series"
	PeopleEpisodes          PeopleScope = "episode"
)

var ErrPeopleQuery = errors.New("frontend: invalid people query")

type PeopleQuery struct {
	Query string
	Limit int
	Scope PeopleScope
}

type PersonReadOptions struct{ Prefetch bool }

// PersonDetail contains only facts from currently visible credits. Artwork is
// optional and remains bound to the authorized title that owns the image.
type PersonDetail struct {
	ID, Name, IMDB string
	TMDB           int64
	ArtworkItem    RatingKey
	Images         []Image
}

// PeopleReader resolves existing person facet IDs through current visible
// credit membership, including episode-only credits. Reads never resolve media
// sources. Prefetch must not schedule a provider refresh.
type PeopleReader interface {
	People(context.Context, *Viewer, Device, PeopleQuery) ([]PersonDetail, error)
	Person(context.Context, *Viewer, Device, string, PersonReadOptions) (PersonDetail, error)
	// PersonCredit checks one current cached title-credit membership.
	PersonCredit(context.Context, *Viewer, Device, RatingKey, int64) (PersonDetail, error)
}

// PeopleImageReader checks exact portrait ownership and current credit policy
// in one host-owned snapshot. Generic ItemArtwork is not a substitute.
// The optional trusted admission callback must not publish; it runs after the
// exact descriptor match, before final policy validation and lease release.
type PeopleImageReader interface {
	AuthorizePersonImage(context.Context, *Viewer, Device, RatingKey, Image, func(context.Context) error) error
}

// PersonArtwork constructs known portrait descriptors only from the detached
// current title supplied by a host's authorized credit snapshot. It performs no
// metadata lookup, refresh, scheduling, or independent title-cache read.
type PersonArtwork interface {
	PersonImages(context.Context, ArtworkRequest, Title) ([]Image, error)
}
