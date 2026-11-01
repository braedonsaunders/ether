package experience

import (
	"context"
	"strconv"
	"strings"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
)

// ResolveEventSubject rechecks visibility on delivery rather than trusting the
// event's subject. Unknown and inaccessible entities return the same error.
func (s *Service) ResolveEventSubject(ctx context.Context, viewer *core.Viewer, _ core.Device, ref frontend.FavoriteRef) (frontend.ResolvedEventSubject, error) {
	v, err := s.viewer(ctx, viewer)
	if err != nil {
		return frontend.ResolvedEventSubject{}, core.ErrNotFound
	}
	result := frontend.ResolvedEventSubject{Ref: ref}
	switch ref.Kind {
	case frontend.FavoriteTitle:
		key, e := core.ParseRatingKey(ref.ID)
		if e != nil || key.String() != ref.ID {
			return frontend.ResolvedEventSubject{}, core.ErrNotFound
		}
		data, e := s.UserData(ctx, v, []core.RatingKey{key})
		if e != nil {
			return frontend.ResolvedEventSubject{}, e
		}
		status, ok := data[key]
		if !ok {
			return frontend.ResolvedEventSubject{}, core.ErrNotFound
		}
		result.Favorite = status.IsFavorite
		result.UserData = &status
		return result, nil
	case frontend.FavoritePerson:
		raw, ok := strings.CutPrefix(ref.ID, "tmdb:")
		if !ok {
			return frontend.ResolvedEventSubject{}, core.ErrNotFound
		}
		id, e := strconv.ParseInt(raw, 10, 64)
		if e != nil || id <= 0 || strconv.FormatInt(id, 10) != raw {
			return frontend.ResolvedEventSubject{}, core.ErrNotFound
		}
		result.PersonFacetID = facetID("person", raw)
	case frontend.FavoriteCollection:
		key, e := core.ParseRatingKey(ref.ID)
		if e != nil || key.String() != ref.ID || (key.Kind() != core.KindCollection && key.Kind() != core.KindSmart) {
			return frontend.ResolvedEventSubject{}, core.ErrNotFound
		}
	case frontend.FavoriteChannel:
		if ref.ID == "" || len(ref.ID) > 512 {
			return frontend.ResolvedEventSubject{}, core.ErrNotFound
		}
	default:
		return frontend.ResolvedEventSubject{}, core.ErrNotFound
	}
	if !s.entityAllowed(ctx, v, ref) {
		return frontend.ResolvedEventSubject{}, core.ErrNotFound
	}
	if s.d.EntityFavoritesSource == nil {
		return frontend.ResolvedEventSubject{}, core.ErrNotAvailable
	}
	values, err := s.d.EntityFavoritesSource(ctx, v, []frontend.FavoriteRef{ref})
	if err != nil {
		return frontend.ResolvedEventSubject{}, err
	}
	result.Favorite = values[ref]
	return result, nil
}
