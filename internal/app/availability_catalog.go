package app

import (
	"context"
	"github.com/braedonsaunders/ether/internal/availability"
	"github.com/braedonsaunders/ether/internal/catalog"
	"github.com/braedonsaunders/ether/internal/core"
)

// Keep optional catalogue contracts by embedding the concrete catalogue. The
// admin mirror and feed matcher still use the unfiltered catalogue.
type availabilityCatalog struct {
	*catalog.Catalog
	availability *availability.Service
}

func (c *availabilityCatalog) excludeHidden(ctx context.Context, f core.Filter) core.Filter {
	if f.IncludeKeys != nil {
		return f
	}
	f.ExcludeKeys = append(append([]core.RatingKey{}, f.ExcludeKeys...), c.availability.HiddenMovies(ctx)...)
	f.ExcludeKeys = append(f.ExcludeKeys, c.availability.HiddenShows(ctx)...)
	return f
}

func (c *availabilityCatalog) Browse(ctx context.Context, q core.BrowseQuery) (core.Page, error) {
	q.Filter = c.excludeHidden(ctx, q.Filter)
	return c.Catalog.Browse(ctx, q)
}
func (c *availabilityCatalog) Facet(ctx context.Context, t core.MediaType, facet string) ([]core.FacetValue, error) {
	return c.Catalog.FacetFilter(ctx, c.excludeHidden(ctx, core.Filter{Type: t}), facet)
}
func (c *availabilityCatalog) FacetFilter(ctx context.Context, f core.Filter, facet string) ([]core.FacetValue, error) {
	return c.Catalog.FacetFilter(ctx, c.excludeHidden(ctx, f), facet)
}
func (c *availabilityCatalog) Children(ctx context.Context, k core.RatingKey) ([]core.Title, error) {
	ts, e := c.Catalog.Children(ctx, k)
	if e != nil {
		return nil, e
	}
	return c.availability.FilterDiscovery(ctx, c.Catalog.FilterVisibility(ctx, ts)), nil
}
func (c *availabilityCatalog) Grandchildren(ctx context.Context, k core.RatingKey) ([]core.Title, error) {
	ts, e := c.Catalog.Grandchildren(ctx, k)
	if e != nil {
		return nil, e
	}
	return c.availability.FilterDiscovery(ctx, c.Catalog.FilterVisibility(ctx, ts)), nil
}
func (c *availabilityCatalog) Search(ctx context.Context, q string, types []core.MediaType, n int) ([]core.Title, error) {
	ts, e := c.Catalog.Search(ctx, q, types, n)
	if e != nil {
		return nil, e
	}
	return c.availability.FilterDiscovery(ctx, c.Catalog.FilterVisibility(ctx, ts)), nil
}
