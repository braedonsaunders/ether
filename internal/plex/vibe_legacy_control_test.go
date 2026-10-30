package plex

import (
	"context"
	"errors"
	"github.com/braedonsaunders/ether/internal/core"
	"reflect"
	"testing"
)

func (s *Server) controlLegacyVibeSearch(ctx context.Context, pr *Request, query string) (*core.Collection, []core.Title, bool, error) {
	searcher := s.d.VibeSearch
	if searcher == nil {
		searcher, _ = s.d.Collections.(core.VibeSearcher)
	}
	if searcher == nil || pr == nil {
		return nil, nil, false, nil
	}
	c, items, ok, err := searcher.VibeCollection(ctx, pr.User, query)
	if err != nil || !ok {
		return c, items, ok, err
	}
	return c, s.filterAllowed(pr.User, items), true, nil
}

type vibeControlSource struct {
	collection *core.Collection
	items      []core.Title
	ok         bool
	err        error
	calls      int
}

func (s *vibeControlSource) VibeCollection(context.Context, *core.User, string) (*core.Collection, []core.Title, bool, error) {
	s.calls++
	return s.collection, s.items, s.ok, s.err
}
func (*vibeControlSource) PromoteEphemeral(context.Context, *core.User, core.RatingKey) (*core.Collection, bool, error) {
	return nil, false, nil
}

type vibeControlCollections struct {
	core.Collections
	source *vibeControlSource
}

func (c vibeControlCollections) VibeCollection(ctx context.Context, u *core.User, q string) (*core.Collection, []core.Title, bool, error) {
	return c.source.VibeCollection(ctx, u, q)
}
func (c vibeControlCollections) PromoteEphemeral(ctx context.Context, u *core.User, key core.RatingKey) (*core.Collection, bool, error) {
	return c.source.PromoteEphemeral(ctx, u, key)
}

func TestVibeLegacyControl(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	ctx := context.Background()
	pr := &Request{User: &ownerUser}
	parent := breakingBad
	parent.Adult = true
	f.catalog.titles[parent.Key] = parent
	source := &vibeControlSource{collection: &core.Collection{Key: core.CollectionKey(4), Title: "Vibe", Type: core.TypeShow}, items: []core.Title{bbE1, {Key: core.MovieKey(8), Type: core.TypeMovie, Adult: true}, fightClub}}
	f.srv.d.FilterAvailable = func(context.Context, []core.Title) []core.Title { return nil }
	failure := errors.New("source failed")
	for _, tc := range []struct {
		name     string
		ok       bool
		err      error
		fallback bool
	}{{"success", true, nil, false}, {"not matched", false, nil, false}, {"error", true, failure, false}, {"collection fallback", true, nil, true}} {
		t.Run(tc.name, func(t *testing.T) {
			source.ok, source.err, source.calls = tc.ok, tc.err, 0
			if tc.fallback {
				f.srv.d.VibeSearch = nil
				f.srv.d.Collections = vibeControlCollections{Collections: f.cols, source: source}
			} else {
				f.srv.d.VibeSearch = source
			}
			wc, wi, wo, we := f.srv.controlLegacyVibeSearch(ctx, pr, "query")
			before := source.calls
			source.calls = 0
			gc, gi, go_, ge := f.srv.vibeSearch(ctx, pr, "query")
			if !reflect.DeepEqual(wc, gc) || !reflect.DeepEqual(wi, gi) || wo != go_ || !errors.Is(we, ge) || before != 1 || source.calls != 1 {
				t.Fatalf("legacy=%+v %+v %v %v; shared=%+v %+v %v %v", wc, wi, wo, we, gc, gi, go_, ge)
			}
			if tc.ok && tc.err == nil && (len(gi) != 2 || gi[0].Key != bbE1.Key || gi[1].Key != fightClub.Key) {
				t.Fatalf("trusted selection changed: %+v", gi)
			}
		})
	}
}

func TestVibeLegacyControlMissingRequestAndSource(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	ctx := context.Background()
	for _, pr := range []*Request{nil, {User: &ownerUser}} {
		wc, wi, wo, we := f.srv.controlLegacyVibeSearch(ctx, pr, "query")
		gc, gi, go_, ge := f.srv.vibeSearch(ctx, pr, "query")
		if !reflect.DeepEqual(wc, gc) || !reflect.DeepEqual(wi, gi) || wo != go_ || !errors.Is(we, ge) {
			t.Fatalf("legacy=%+v %+v %v %v; shared=%+v %+v %v %v", wc, wi, wo, we, gc, gi, go_, ge)
		}
	}
}
