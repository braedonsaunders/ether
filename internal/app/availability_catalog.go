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

func (c *availabilityCatalog) Browse(ctx context.Context, q core.BrowseQuery) (core.Page, error) {
	var err error
	if q.PlaylistSource != nil {
		source := *q.PlaylistSource
		source.Filter, err = c.availability.PagingFilter(ctx, source.Filter)
		if err != nil {
			return core.Page{}, err
		}
		q.PlaylistSource = &source
	}
	q.Filter, err = c.availability.PagingFilter(ctx, q.Filter)
	if err != nil {
		return core.Page{}, err
	}
	return c.Catalog.Browse(ctx, q)
}

// BrowseAvailabilityInSQL marks pages whose availability decision was applied
// before counting and pagination.
func (c *availabilityCatalog) BrowseAvailabilityInSQL() bool { return true }
func (c *availabilityCatalog) Facet(ctx context.Context, t core.MediaType, facet string) ([]core.FacetValue, error) {
	return c.FacetFilter(ctx, core.Filter{Type: t}, facet)
}
func (c *availabilityCatalog) FacetFilter(ctx context.Context, f core.Filter, facet string) ([]core.FacetValue, error) {
	f, err := c.availability.PagingFilter(ctx, f)
	if err != nil {
		return nil, err
	}
	return c.Catalog.FacetFilter(ctx, f, facet)
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
