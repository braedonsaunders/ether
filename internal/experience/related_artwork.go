package experience

import (
	"context"
	"errors"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
)

// RelatedArtworkDeps uses the same cache-only authorization and descriptor
// source as item artwork; it never resolves media or fetches catalogue data.
type RelatedArtworkDeps struct {
	Authorize func(context.Context, *core.Viewer, core.Device, core.RatingKey) (*core.Viewer, error)
	List      func(context.Context, *core.Viewer, core.Device, core.RatingKey) ([]frontend.Image, bool, error)
}

type RelatedArtworkService struct{ deps RelatedArtworkDeps }

func NewRelatedArtworkService(deps RelatedArtworkDeps) (*RelatedArtworkService, error) {
	if deps.Authorize == nil || deps.List == nil {
		return nil, errors.New("experience: related artwork dependencies required")
	}
	return &RelatedArtworkService{deps: deps}, nil
}

func (s *RelatedArtworkService) RelatedArtwork(ctx context.Context, viewer *core.Viewer, device core.Device, key core.RatingKey) (frontend.RelatedArtwork, error) {
	var result frontend.RelatedArtwork
	active, err := s.deps.Authorize(ctx, viewer, device, key)
	if err != nil {
		return result, err
	}
	if viewer == nil || active == nil || active.ID != viewer.ID {
		return result, core.ErrUnauthorized
	}
	images, complete, err := s.deps.List(ctx, active, device, key)
	if err != nil {
		return result, err
	}
	result.PeopleAuthoritative = complete
	if complete {
		for _, image := range images {
			if image.Kind == frontend.ImagePerson && image.Subject != "" {
				result.People = append(result.People, image)
			}
		}
	}
	parent := func(key core.RatingKey) (*frontend.ArtworkOwner, error) {
		if key == 0 {
			return nil, nil
		}
		if _, err := s.deps.Authorize(ctx, active, device, key); err != nil {
			if errors.Is(err, core.ErrNotFound) {
				return nil, nil
			}
			return nil, err
		}
		images, complete, err := s.deps.List(ctx, active, device, key)
		if errors.Is(err, core.ErrNotFound) || errors.Is(err, core.ErrNotAvailable) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		if _, err = s.deps.Authorize(ctx, active, device, key); err != nil {
			return nil, err
		}
		return &frontend.ArtworkOwner{Item: key, Images: images, Authoritative: complete}, nil
	}
	switch key.Kind() {
	case core.KindEpisode:
		result.Season, err = parent(key.Parent())
		if err == nil {
			result.Series, err = parent(key.Parent().Parent())
		}
	case core.KindSeason:
		result.Series, err = parent(key.Parent())
	}
	if err != nil {
		return frontend.RelatedArtwork{}, err
	}
	if _, err = s.deps.Authorize(ctx, active, device, key); err != nil {
		return frontend.RelatedArtwork{}, err
	}
	for _, owner := range []*frontend.ArtworkOwner{result.Season, result.Series} {
		if owner != nil {
			if _, err = s.deps.Authorize(ctx, active, device, owner.Item); err != nil {
				return frontend.RelatedArtwork{}, err
			}
		}
	}
	return result, nil
}

var _ frontend.RelatedArtworkProvider = (*RelatedArtworkService)(nil)
