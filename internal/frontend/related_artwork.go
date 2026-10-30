package frontend

import "context"

// ArtworkOwner identifies the independently authorized item whose descriptors
// must be passed to ImageGrants. Incomplete lists must not be granted.
type ArtworkOwner struct {
	Item          RatingKey
	Images        []Image
	Authoritative bool
}

// RelatedArtwork contains only artwork associated with an authorized item.
// Season is the episode's canonical season; Series is its show. People belong
// to the requested item, and Image.Subject identifies each credited person.
type RelatedArtwork struct {
	People              []Image
	PeopleAuthoritative bool
	Season, Series      *ArtworkOwner
}

// RelatedArtworkProvider supplies cache-only parent and credit artwork. Each
// descriptor must be granted using its owning item, and is revalidated by the
// image-grant service on issue and read.
type RelatedArtworkProvider interface {
	RelatedArtwork(context.Context, *Viewer, Device, RatingKey) (RelatedArtwork, error)
}
