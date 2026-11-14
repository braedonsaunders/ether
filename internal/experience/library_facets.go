package experience

import (
	"context"
	"errors"
	"sort"
	"strconv"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend/kit/paging"
)

// sqlFacets maps the facets the catalogue counts in SQL to its facet name
// (studios and countries unlimited: the walk listed every value). People are
// not counted in SQL: a credit facet would read every credit.
var sqlFacets = map[string]string{"genre": "genre", "filterStudio": "studio", "tag": "tag", "studio": "studioAll", "country": "countryAll", "year": "year", "decade": "decade", "contentRating": "contentRating"}

// indexedFacets counts a library's facet values in SQL over the titles the
// viewer may see (the same exclusions an indexed page folds in), instead of
// loading the whole library. An empty viewID is every library the viewer
// may open. ok is false when the walk must answer: no SQL facet, or
// exclusions too large for one predicate.
func (s *Service) indexedFacets(ctx context.Context, viewer *core.Viewer, viewID, name string) ([]core.FacetValue, bool, error) {
	if s.d.FacetFilter == nil || s.d.PageExclusions == nil || sqlFacets[name] == "" {
		return nil, false, nil
	}
	v, err := s.viewer(ctx, viewer)
	if err != nil {
		return nil, false, err
	}
	libs, err := s.facetLibraries(v, viewID)
	if err != nil {
		return nil, false, err
	}
	counts := map[string]int{}
	for _, lib := range libs {
		ex := s.d.PageExclusions(ctx, v, lib.Type)
		if ex.Err != nil {
			return nil, false, ex.Err
		}
		if !ex.AvailabilityInSQL {
			return nil, false, nil
		}
		base := core.Filter{Type: lib.Type}
		if s.d.PluginLibraries != nil {
			if stored, err := s.d.PluginLibraries.Library(ctx, core.LibraryID(lib.ID)); err == nil {
				base = s.libraryBrowseFilter(ctx, v, stored, base)
			}
		}
		filter, fits, none := viewerBrowseFilter(base, v, ex, lib.Type)
		if !fits {
			return nil, false, nil
		}
		if none {
			continue
		}
		vals, err := s.d.FacetFilter(ctx, filter, sqlFacets[name])
		if err != nil {
			return nil, false, err
		}
		for _, val := range vals {
			if val.Key != "" {
				counts[val.Key] += val.Count
			}
		}
	}
	keys := make([]string, 0, len(counts))
	for k := range counts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]core.FacetValue, 0, len(keys))
	seen := make(map[string]string, len(keys))
	for _, k := range keys {
		id := facetID(name, k)
		if old, ok := seen[id]; ok && old != k {
			return nil, false, errors.New("experience: facet identifier collision")
		}
		seen[id] = k
		out = append(out, core.FacetValue{ID: id, Key: k, Title: k, Count: counts[k]})
	}
	return out, true, nil
}

// resolveGlobalFacet finds id among a library's facet values counted over
// every browsable title. ok is false when no value has that id.
func (s *Service) resolveGlobalFacet(ctx context.Context, viewer *core.Viewer, viewID, kind, name, id string) (core.FacetValue, bool, error) {
	v, err := s.viewer(ctx, viewer)
	if err != nil {
		return core.FacetValue{}, false, err
	}
	global := core.WithoutCatalogViewer(ctx)
	for _, lib := range s.libraries() {
		if (viewID != "" && strconv.Itoa(lib.ID) != viewID) || !SectionAllowed(v, lib.ID) {
			continue
		}
		vals, err := s.d.FacetFilter(global, core.Filter{Type: lib.Type}, name)
		if err != nil {
			return core.FacetValue{}, false, err
		}
		for _, val := range vals {
			if val.Key != "" && facetID(kind, val.Key) == id {
				return core.FacetValue{ID: id, Key: val.Key, Title: val.Key, Count: val.Count}, true, nil
			}
		}
	}
	return core.FacetValue{}, false, nil
}

// facetLibraries are the libraries a facet list of viewID counts: that one,
// or every library the viewer may open for "".
func (s *Service) facetLibraries(v *core.User, viewID string) ([]Library, error) {
	var libs []Library
	for _, lib := range s.libraries() {
		if (viewID == "" || strconv.Itoa(lib.ID) == viewID) && SectionAllowed(v, lib.ID) {
			libs = append(libs, lib)
		}
	}
	if viewID != "" && len(libs) == 0 {
		return nil, core.ErrNotFound
	}
	return libs, nil
}

// FacetPage is Facets' page [offset, offset+limit) and how many values
// Facets lists (a negative limit is the rest). One library's studios are
// paged in SQL (Catalog.FacetPage): a page of names and a kept count of
// them, never every studio of the library.
func (s *Service) FacetPage(ctx context.Context, viewer *core.Viewer, device core.Device, viewID, name string, offset, limit int) ([]core.FacetValue, int, error) {
	ctx = core.WithCatalogViewer(ctx, viewer)
	if s.d.PluginLibraries != nil && viewID != "" {
		if id, err := strconv.Atoi(viewID); err == nil {
			if lib, err := s.d.PluginLibraries.Library(ctx, core.LibraryID(id)); err == nil && lib.Source != core.LibraryBuiltin {
				facets, err := s.Facets(ctx, viewer, device, viewID, name)
				if err != nil {
					return nil, 0, err
				}
				return paging.Slice(facets, offset, limit), len(facets), nil
			}
		}
	}
	if out, total, ok, err := s.pagedFacets(ctx, viewer, viewID, name, max(offset, 0), limit); err != nil || ok {
		return out, total, err
	}
	facets, err := s.Facets(ctx, viewer, device, viewID, name)
	if err != nil {
		return nil, 0, err
	}
	return paging.Slice(facets, offset, limit), len(facets), nil
}

// pagedFacets answers FacetPage from the catalogue when the facet list is
// one library's and its exclusions fold into the SQL page, as
// indexedFacets' do. ok is false otherwise.
func (s *Service) pagedFacets(ctx context.Context, viewer *core.Viewer, viewID, name string, offset, limit int) ([]core.FacetValue, int, bool, error) {
	if s.d.FacetPage == nil || s.d.PageExclusions == nil || sqlFacets[name] == "" || limit < 0 {
		return nil, 0, false, nil
	}
	v, err := s.viewer(ctx, viewer)
	if err != nil {
		return nil, 0, false, err
	}
	libs, err := s.facetLibraries(v, viewID)
	if err != nil || len(libs) != 1 {
		return nil, 0, false, err
	}
	lib := libs[0]
	ex := s.d.PageExclusions(ctx, v, lib.Type)
	if ex.Err != nil {
		return nil, 0, false, ex.Err
	}
	if !ex.AvailabilityInSQL {
		return nil, 0, false, nil
	}
	base := core.Filter{Type: lib.Type}
	if s.d.PluginLibraries != nil {
		if stored, err := s.d.PluginLibraries.Library(ctx, core.LibraryID(lib.ID)); err == nil {
			base = s.libraryBrowseFilter(ctx, v, stored, base)
		}
	}
	filter, fits, none := viewerBrowseFilter(base, v, ex, lib.Type)
	if !fits {
		return nil, 0, false, nil
	}
	if none {
		return []core.FacetValue{}, 0, true, nil
	}
	vals, total, ok, err := s.d.FacetPage(ctx, filter, sqlFacets[name], offset, limit)
	if err != nil || !ok {
		return nil, 0, false, err
	}
	for i := range vals {
		vals[i].ID = facetID(name, vals[i].Key)
	}
	return vals, total, true, nil
}

// resolveStudio names the studio id identifies from the indexed
// vocabulary. A library the viewer may not open resolves nothing.
func (s *Service) resolveStudio(ctx context.Context, viewer *core.Viewer, viewID, id string) (core.FacetValue, error) {
	if viewID != "" {
		v, err := s.viewer(ctx, viewer)
		if err != nil {
			return core.FacetValue{}, err
		}
		for _, lib := range s.libraries() {
			if strconv.Itoa(lib.ID) == viewID && !SectionAllowed(v, lib.ID) {
				return core.FacetValue{}, core.ErrNotFound
			}
		}
	}
	studio, ok, err := s.d.StudioByFacetID(ctx, id)
	if err != nil {
		return core.FacetValue{}, err
	}
	if !ok {
		return core.FacetValue{}, core.ErrNotFound
	}
	return core.FacetValue{ID: id, Key: studio, Title: studio}, nil
}

// FilterFacets never falls back to walking LibraryItems. Studios use the
// catalogue's top-value index rather than materializing the studio vocabulary.
func (s *Service) FilterFacets(ctx context.Context, viewer *core.Viewer, device core.Device, viewID, name string) ([]core.FacetValue, error) {
	ctx = core.WithCatalogViewer(ctx, viewer)
	queryName := name
	if name == "studio" {
		queryName = "filterStudio"
	}
	// Filter and plugin libraries keep their own visibility and membership scope.
	if s.d.PluginLibraries != nil && viewID != "" {
		if id, err := strconv.Atoi(viewID); err == nil {
			if lib, err := s.d.PluginLibraries.Library(ctx, core.LibraryID(id)); err == nil {
				if lib.Source == core.LibraryPlugin {
					values, err := s.pluginLibraryFacets(ctx, viewer, lib, name)
					if len(values) > 256 {
						values = values[:256]
					}
					return values, err
				}
				if lib.Source == core.LibraryCatalog {
					values, _, err := s.filterLibraryFacets(ctx, viewer, lib, queryName)
					if len(values) > 256 {
						values = values[:256]
					}
					for n := range values {
						values[n].ID = facetID(name, values[n].Key)
					}
					return values, err
				}
			}
		}
	}
	values, ok, err := s.indexedFacets(ctx, viewer, viewID, queryName)
	if err != nil || !ok {
		return []core.FacetValue{}, err
	}
	if len(values) > 256 {
		values = values[:256]
	}
	for n := range values {
		values[n].ID = facetID(name, values[n].Key)
	}
	return values, nil
}
