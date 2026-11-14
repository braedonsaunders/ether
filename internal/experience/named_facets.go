package experience

import (
	"context"
	"errors"

	"github.com/braedonsaunders/ether/internal/core"
)

// NamedFacetIDs maps exact credited genre or studio names on one visible title
// to the same identifier namespace as library facets.
func (s *Service) NamedFacetIDs(ctx context.Context, viewer *core.Viewer, _ core.Device, key core.RatingKey, kind string, names []string) (map[string]string, error) {
	ctx = core.WithCatalogViewer(ctx, viewer)
	if kind != "genre" && kind != "studio" {
		return nil, errors.New("experience: unsupported facet")
	}
	if len(names) > 256 {
		return nil, errors.New("experience: too many facet names")
	}
	wanted := make(map[string]bool, len(names))
	for _, name := range names {
		if name == "" || len(name) > 1024 {
			return nil, errors.New("experience: invalid facet name")
		}
		wanted[name] = true
	}
	active, err := s.viewer(ctx, viewer)
	if err != nil {
		return nil, err
	}
	// Facet identifiers require current membership and policy facts, not a
	// title's credits, trailers or alternate names.
	facts := core.FactGenres
	if kind == "studio" {
		facts = core.FactStudios
	}
	var title *core.Title
	rows, err := s.visibilityRows(ctx, active, []core.RatingKey{key}, facts)
	if err != nil {
		return nil, err
	}
	if rows != nil {
		if row, ok := rows[key]; ok {
			title = &row
		}
		if title == nil || title.Key != key || !s.visible(ctx, active, title) {
			err = core.ErrNotFound
		}
	} else if s.d.CachedGet != nil {
		title, err = s.d.CachedGet(ctx, key)
		if err == nil && (title == nil || title.Key != key || !s.visible(ctx, active, title)) {
			err = core.ErrNotFound
		}
	} else {
		title, err = s.item(ctx, active, key)
	}
	if err != nil {
		return nil, err
	}
	values := title.Genres
	if kind == "studio" {
		values = title.Studios
	}
	out := make(map[string]string)
	seen := map[string]string{}
	for _, name := range values {
		if name == "" {
			continue
		}
		id := facetID(kind, name)
		if previous, ok := seen[id]; ok && previous != name {
			return nil, errors.New("experience: facet identifier collision")
		}
		seen[id] = name
		if wanted[name] {
			out[name] = id
		}
	}
	return out, nil
}
