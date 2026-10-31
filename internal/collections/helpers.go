package collections

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"golang.org/x/text/language"
	"golang.org/x/text/language/display"

	"github.com/braedonsaunders/ether/internal/core"
)

// Catalogue helpers for the Collection Studio's pickers.

var standardContentRatings = []string{
	"G", "TV-Y", "TV-G", "TV-Y7", "PG", "TV-PG", "PG-13", "TV-14", "R", "TV-MA", "NC-17", "NR",
}

// BrowseTitles lists the released local catalogue with filters and pagination.
func (s *Service) BrowseTitles(ctx context.Context, q core.BrowseQuery) (core.Page, error) {
	return s.cat.Browse(ctx, q)
}

// SearchTitles searches the catalogue (reaching all of TMDB).
func (s *Service) SearchTitles(ctx context.Context, q string, t core.MediaType, limit int) ([]core.Title, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	var types []core.MediaType
	if t == core.TypeMovie || t == core.TypeShow {
		types = []core.MediaType{t}
	}
	return s.cat.Search(ctx, q, types, limit)
}

// Facets lists filter values over the browsable catalogue: genre, decade,
// contentRating, studio, country, language (and year).
func (s *Service) Facets(ctx context.Context, t core.MediaType, facet string) ([]core.FacetValue, error) {
	switch facet {
	case "language":
		return s.languageFacet(ctx, t)
	case "genre", "decade", "contentRating", "studio", "country", "year":
	default:
		return nil, &ValidationError{Fields: map[string]string{"facet": "facet is genre, decade, contentRating, studio, country or language"}}
	}
	vals, err := s.cat.Facet(ctx, t, facet)
	if err != nil {
		return nil, err
	}
	if facet == "contentRating" {
		seen := make(map[string]bool, len(vals))
		for _, v := range vals {
			seen[strings.ToUpper(v.Key)] = true
		}
		for _, r := range standardContentRatings {
			if !seen[r] {
				vals = append(vals, core.FacetValue{Key: r, Title: r})
			}
		}
		return vals, nil
	}
	if facet == "country" {
		// Movies store country names, shows ISO codes; merge them.
		merged := map[string]*core.FacetValue{}
		var order []string
		for _, v := range vals {
			key := v.Key
			title := v.Title
			if len(key) == 2 {
				if n, ok := countryNames[strings.ToUpper(key)]; ok {
					title = n
				}
			} else if code := countryCode(key); code != "" {
				key = code
			}
			if m := merged[strings.ToUpper(key)]; m != nil {
				m.Count += v.Count
				continue
			}
			fv := core.FacetValue{Key: strings.ToUpper(key), Title: title, Count: v.Count}
			if len(fv.Key) != 2 {
				fv.Key = v.Key
			}
			merged[strings.ToUpper(key)] = &fv
			order = append(order, strings.ToUpper(key))
		}
		out := make([]core.FacetValue, 0, len(order))
		for _, k := range order {
			out = append(out, *merged[k])
		}
		sort.SliceStable(out, func(i, j int) bool { return out[i].Count > out[j].Count })
		return out, nil
	}
	return vals, nil
}

func (s *Service) languageFacet(ctx context.Context, t core.MediaType) ([]core.FacetValue, error) {
	vals, err := s.cat.Facet(ctx, t, "language")
	if err != nil {
		return nil, err
	}
	for i := range vals {
		vals[i].Title = languageName(vals[i].Key)
	}
	return vals, nil
}

func languageName(code string) string {
	tag, err := language.Parse(code)
	if err != nil {
		return code
	}
	if n := display.English.Languages().Name(tag); n != "" {
		return n
	}
	return code
}

// Lookup finds TMDB people, keywords, companies, networks, collections or
// streaming services by name for the rule and source pickers.
func (s *Service) Lookup(ctx context.Context, kind, q, region string) ([]core.LookupResult, error) {
	q = strings.TrimSpace(q)
	switch kind {
	case "network":
		if n, err := strconv.ParseInt(q, 10, 64); err == nil && n > 0 {
			r, err := s.names.resolve(ctx, "network", ref{ID: n}, "")
			if err != nil {
				return []core.LookupResult{}, nil
			}
			return []core.LookupResult{{ID: r.ID, Name: r.Name}}, nil
		}
		return searchNetworks(q), nil
	case "watchProvider":
		ps, err := s.api.watchProviders(ctx, s.region(region))
		if err != nil {
			return nil, err
		}
		want := normName(q)
		out := []core.LookupResult{}
		for _, p := range ps {
			if want == "" || strings.Contains(normName(p.Name), want) {
				out = append(out, core.LookupResult{ID: p.ID, Name: p.Name, Subtitle: s.region(region), ImagePath: p.Logo})
			}
			if len(out) >= 60 {
				break
			}
		}
		return out, nil
	case "person", "keyword", "company", "collection":
		if q == "" {
			return []core.LookupResult{}, nil
		}
		if s.api.c == nil {
			return nil, errNoTMDB
		}
		if n, err := strconv.ParseInt(q, 10, 64); err == nil && n > 0 {
			name, err := s.api.entityName(ctx, kind, n)
			if err != nil {
				return []core.LookupResult{}, nil
			}
			return []core.LookupResult{{ID: n, Name: name}}, nil
		}
		res, err := s.api.search(ctx, kind, q)
		if err != nil {
			return nil, err
		}
		if len(res) > 20 {
			res = res[:20]
		}
		return res, nil
	}
	return nil, &ValidationError{Fields: map[string]string{"kind": fmt.Sprintf("unknown lookup kind %q", kind)}}
}

// TitleDetail returns a title with credits and keywords (fetching details
// from TMDB when the mirror only has a summary). The owner console also
// inspects titles viewers cannot open yet, such as an upcoming movie on the
// release calendar, so an unreleased title Get refuses is read from the
// mirror instead. Adult titles stay hidden.
func (s *Service) TitleDetail(ctx context.Context, key core.RatingKey) (*core.Title, error) {
	t, err := s.cat.Get(ctx, key)
	if !errors.Is(err, core.ErrNotFound) {
		return t, err
	}
	stored, ok := s.cat.(core.StoredTitleLookup)
	if !ok || (key.Kind() != core.KindMovie && key.Kind() != core.KindShow) {
		return nil, err
	}
	t, serr := stored.StoredTitle(ctx, key)
	if serr != nil || t == nil || t.Adult || t.Key != key {
		return nil, err
	}
	return t, nil
}
