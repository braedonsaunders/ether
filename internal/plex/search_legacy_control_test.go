package plex

import (
	"context"
	"errors"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/llm"
	"github.com/braedonsaunders/ether/internal/store"
	"github.com/braedonsaunders/ether/internal/watchlist"
	"reflect"
	"strings"
	"testing"
)

func (s *Server) controlLegacyRunSearch(ctx context.Context, pr *Request, query string, sectionID, limit int) ([]core.Title, error) {
	types := s.searchTypes(pr.User, sectionID)
	if strings.TrimSpace(query) == "" || len(types) == 0 {
		return nil, nil
	}
	if items, ok := s.discoverySearch(ctx, pr, query, limit); ok {
		items = s.visibleTitles(pr.User, items)
		allowed := map[core.MediaType]bool{}
		for _, typ := range types {
			allowed[typ] = true
		}
		filtered := items[:0:0]
		for _, item := range items {
			if allowed[item.Type] {
				filtered = append(filtered, item)
			}
		}
		return filtered, nil
	}
	res, err := s.d.Catalog.Search(ctx, query, types, limit)
	if err != nil {
		return nil, err
	}
	return s.filterAllowed(pr.User, res), nil
}

func TestSearchLegacyControlDiscovery(t *testing.T) {
	ctx := context.Background()
	f := newFixture()
	defer f.srv.Close()
	db, err := store.OpenMemory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	cat := &legacySearchCatalog{Catalog: f.catalog, results: []core.Title{fightClub, bbE1, breakingBad}}
	f.srv.d.Catalog = cat
	watch := watchlist.New(db, watchlist.Options{})
	if err := watch.SaveSettings(ctx, watchlist.Settings{NaturalSearch: true, CheckHours: 6}); err != nil {
		t.Fatal(err)
	}
	// A disabled model makes NaturalSearch use its catalogue fallback while
	// discoverySearch still returns its successful natural-source branch.
	f.srv.d.Discovery = &DiscoveryFeatures{Watchlist: watch, LLM: llm.New(llm.ServiceOptions{Catalog: cat})}
	pr := &Request{User: &ownerUser}
	want, we := f.srv.controlLegacyRunSearch(ctx, pr, "query", 1, 2)
	before := append([]int(nil), cat.limits...)
	cat.limits = nil
	got, ge := f.srv.runSearch(ctx, pr, "query", 1, 2)
	if we != nil || ge != nil || !reflect.DeepEqual(got, want) || !reflect.DeepEqual(before, cat.limits) || len(got) != 1 || got[0].Key != fightClub.Key {
		t.Fatalf("legacy=%+v %v calls=%v; shared=%+v %v calls=%v", want, we, before, got, ge, cat.limits)
	}
}

type legacySearchCatalog struct {
	core.Catalog
	results []core.Title
	limits  []int
	err     error
}

type legacySearchPolicy struct{ denied map[core.RatingKey]bool }

func (p legacySearchPolicy) Allowed(_ *core.User, title *core.Title) bool {
	return !p.denied[title.Key]
}

func (c *legacySearchCatalog) Search(_ context.Context, _ string, _ []core.MediaType, limit int) ([]core.Title, error) {
	c.limits = append(c.limits, limit)
	return append([]core.Title(nil), c.results...), c.err
}

func TestSearchLegacyControl(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	cat := &legacySearchCatalog{Catalog: f.catalog, results: []core.Title{fightClub, bbE1, {Key: core.MovieKey(9182), Type: core.TypeMovie, Adult: true}}}
	f.srv.d.Catalog = cat
	// Availability and inherited-parent checks must not change the trusted legacy path.
	f.srv.d.FilterAvailable = func(context.Context, []core.Title) []core.Title { return nil }
	pr := &Request{User: &ownerUser}
	for _, tc := range []struct {
		name, query    string
		section, limit int
	}{{"default", "fight", 0, 1}, {"movie section", "fight", 1, 0}, {"blank", " ", 0, 5}, {"missing section", "fight", 999, 5}} {
		t.Run(tc.name, func(t *testing.T) {
			cat.limits = nil
			want, we := f.srv.controlLegacyRunSearch(context.Background(), pr, tc.query, tc.section, tc.limit)
			before := append([]int(nil), cat.limits...)
			cat.limits = nil
			got, ge := f.srv.runSearch(context.Background(), pr, tc.query, tc.section, tc.limit)
			if !reflect.DeepEqual(got, want) || (we == nil) != (ge == nil) || !reflect.DeepEqual(before, cat.limits) {
				t.Fatalf("legacy=%+v %v calls=%v; shared=%+v %v calls=%v", want, we, before, got, ge, cat.limits)
			}
		})
	}
	// The historical path applies the title guard but does not look up an
	// episode's parent. Preserve that distinction for trusted viewers.
	f.srv.d.AccessPolicy = legacySearchPolicy{denied: map[core.RatingKey]bool{fightClub.Key: true, breakingBad.Key: true}}
	want, we := f.srv.controlLegacyRunSearch(context.Background(), pr, "fight", 0, 10)
	got, ge := f.srv.runSearch(context.Background(), pr, "fight", 0, 10)
	if we != nil || ge != nil || !reflect.DeepEqual(want, got) || len(got) != 1 || got[0].Key != bbE1.Key {
		t.Fatalf("title/parent policy: legacy=%+v %v shared=%+v %v", want, we, got, ge)
	}
	cat.err = errors.New("catalogue unavailable")
	_, beforeErr := f.srv.controlLegacyRunSearch(context.Background(), pr, "fight", 0, 3)
	_, afterErr := f.srv.runSearch(context.Background(), pr, "fight", 0, 3)
	if !errors.Is(beforeErr, cat.err) || !errors.Is(afterErr, cat.err) {
		t.Fatalf("catalogue errors: legacy=%v shared=%v", beforeErr, afterErr)
	}
}
