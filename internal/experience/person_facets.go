package experience

import (
	"context"
	"errors"
	"strconv"

	"github.com/braedonsaunders/ether/internal/core"
)

const maxPersonFacetIDs = 256

// PersonFacetIDs maps requested credits on one currently visible title to the
// same identifiers used by library person facets. An episode-only credit may
// have no corresponding library-wide facet and therefore may not resolve via
// ResolveFacet for that library.
func (s *Service) PersonFacetIDs(ctx context.Context, viewer *core.Viewer, _ core.Device, key core.RatingKey, ids []int64) (map[int64]string, error) {
	if len(ids) > maxPersonFacetIDs {
		return nil, errors.New("experience: too many person IDs")
	}
	wanted := make(map[int64]bool, len(ids))
	for _, id := range ids {
		if id <= 0 {
			return nil, errors.New("experience: invalid person ID")
		}
		wanted[id] = true
	}
	v, err := s.viewer(ctx, viewer)
	if err != nil {
		return nil, err
	}
	title, err := s.item(ctx, v, key)
	if err != nil {
		return nil, err
	}
	result := make(map[int64]string)
	seen := make(map[string]int64)
	for _, group := range [][]core.Person{title.Cast, title.Directors, title.Writers, title.Crew} {
		for _, p := range group {
			if p.TMDB <= 0 || p.Name == "" {
				continue
			}
			id := facetID("person", strconv.FormatInt(p.TMDB, 10))
			if old, ok := seen[id]; ok && old != p.TMDB {
				return nil, errors.New("experience: facet identifier collision")
			}
			seen[id] = p.TMDB
			if wanted[p.TMDB] {
				result[p.TMDB] = id
			}
		}
	}
	return result, nil
}
