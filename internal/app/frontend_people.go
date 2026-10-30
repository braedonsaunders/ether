package app

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
)

func (s *surfaceExperience) personArtwork(ctx context.Context, viewer *frontend.Viewer, device frontend.Device, person frontend.PersonDetail) (frontend.PersonDetail, error) {
	current, err := s.Service.PersonCredit(ctx, viewer, device, person.ArtworkItem, person.TMDB)
	if err != nil {
		return frontend.PersonDetail{}, err
	}
	if current.ID != person.ID {
		return frontend.PersonDetail{}, core.ErrNotFound
	}
	images, complete, artErr := s.ItemArtwork(ctx, viewer, device, person.ArtworkItem)
	if errors.Is(artErr, core.ErrUnauthorized) || errors.Is(artErr, core.ErrNotFound) {
		return frontend.PersonDetail{}, core.ErrNotFound
	}
	person.Images = nil
	if artErr == nil && complete {
		subject := "tmdb:" + strconv.FormatInt(person.TMDB, 10)
		for _, image := range images {
			if image.Kind == frontend.ImagePerson && image.Subject == subject && image.Path != "" {
				person.Images = append(person.Images, image)
				break
			}
		}
	}
	// Descriptor selection may cross I/O; the same credited membership must
	// still be authorized before any descriptor reaches an image-grant issuer.
	current, err = s.Service.PersonCredit(ctx, viewer, device, person.ArtworkItem, person.TMDB)
	if err != nil {
		return frontend.PersonDetail{}, err
	}
	if current.ID != person.ID {
		return frontend.PersonDetail{}, core.ErrNotFound
	}
	current.Images = person.Images
	return current, nil
}

func (s *surfaceExperience) People(ctx context.Context, viewer *frontend.Viewer, device frontend.Device, q frontend.PeopleQuery) ([]frontend.PersonDetail, error) {
	ctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	people, err := s.Service.People(ctx, viewer, device, q)
	if err != nil {
		return nil, err
	}
	for n, person := range people {
		people[n], err = s.personArtwork(ctx, viewer, device, person)
		if err != nil {
			return nil, err
		}
	}
	return people, nil
}
func (s *surfaceExperience) Person(ctx context.Context, viewer *frontend.Viewer, device frontend.Device, id string, options frontend.PersonReadOptions) (frontend.PersonDetail, error) {
	ctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	person, err := s.Service.Person(ctx, viewer, device, id, options)
	if err != nil {
		return frontend.PersonDetail{}, err
	}
	if options.Prefetch {
		// Artwork List can enqueue metadata/variant work. A prefetch is strictly
		// a current cached-credit read and must not schedule any refresh.
		return person, nil
	}
	return s.personArtwork(ctx, viewer, device, person)
}

var _ frontend.PeopleReader = (*surfaceExperience)(nil)
