package plex

import (
	"context"
	"errors"
	"github.com/braedonsaunders/ether/internal/core"
	"reflect"
	"testing"
	"time"
)

func (s *Server) controlLegacyScopedCollectionItems(ctx context.Context, u *core.User, k core.RatingKey) ([]core.Title, error) {
	pr := FromContext(ctx)
	p, ok := s.d.Collections.(core.CollectionPlaceholder)
	if !ok || !p.AllowsPlaceholders(ctx, u, k, core.ClassifyDevice(pr.Product, pr.Platform, pr.Device)) {
		return nil, core.ErrNotFound
	}
	page, err := s.d.Collections.Items(ctx, u, k, 0, 5000)
	if err != nil {
		return nil, err
	}
	allowed := make([]core.Title, 0, len(page.Items))
	for _, t := range page.Items {
		if !s.titleVisible(u, &t) || t.Released.IsZero() {
			continue
		}
		if t.Released.After(s.now()) {
			if _, ok := s.d.Collections.(core.CollectionPlaceholder); !ok {
				continue
			}
		} else if len(s.availableForViewer(ctx, u, []core.Title{t})) == 0 {
			continue
		}
		allowed = append(allowed, t)
	}
	return allowed, nil
}

type collectionControlSource struct {
	*scopedFakeCollections
	device string
	calls  int
	err    error
}

func (s *collectionControlSource) AllowsPlaceholders(ctx context.Context, u *core.User, key core.RatingKey, device string) bool {
	s.device = device
	return s.scopedFakeCollections.AllowsPlaceholders(ctx, u, key, device)
}
func (s *collectionControlSource) Items(ctx context.Context, u *core.User, key core.RatingKey, offset, limit int) (core.Page, error) {
	s.calls++
	if offset != 0 || limit != 5000 {
		return core.Page{}, errors.New("unexpected page")
	}
	if s.err != nil {
		return core.Page{}, s.err
	}
	return s.scopedFakeCollections.Items(ctx, u, key, offset, limit)
}

func TestScopedCollectionLegacyControl(t *testing.T) {
	f := newFixture(func(d *Deps) { d.AccessPolicy = denyTitlePolicy{} })
	defer f.srv.Close()
	key := core.SmartKey(920)
	future := core.Title{Key: core.MovieKey(201), Type: core.TypeMovie, Title: "Future", Released: fixedNow.Add(time.Hour)}
	blocked := future
	blocked.Key = core.MovieKey(202)
	blocked.Title = "Blocked"
	noDate := future
	noDate.Key = core.MovieKey(203)
	noDate.Released = time.Time{}
	past := future
	past.Key = core.MovieKey(204)
	past.Title = "Unavailable"
	past.Released = fixedNow.Add(-time.Hour)
	child := core.Title{Key: core.EpisodeKey(999, 1, 1), ShowKey: core.ShowKey(999), Type: core.TypeEpisode, Title: "Missing parent", Released: fixedNow.Add(time.Hour)}
	source := &collectionControlSource{scopedFakeCollections: &scopedFakeCollections{&fakeCollections{cols: map[core.RatingKey]core.Collection{key: {Key: key, Upcoming: true}}, items: map[core.RatingKey][]core.Title{key: {future, blocked, noDate, past, child}}}}}
	f.srv.d.Collections = source
	f.srv.d.FilterAvailable = func(_ context.Context, items []core.Title) []core.Title {
		if len(items) > 0 && items[0].Key == past.Key {
			return nil
		}
		return items
	}
	ctx := withRequest(context.Background(), &Request{User: &ownerUser, Product: "Plex for Apple TV"})
	want, we := f.srv.controlLegacyScopedCollectionItems(ctx, &ownerUser, key)
	before, device := source.calls, source.device
	source.calls = 0
	got, ge := f.srv.scopedCollectionItems(ctx, &ownerUser, key)
	if !reflect.DeepEqual(want, got) || we != nil || ge != nil || before != 1 || source.calls != 1 || device != core.DeviceTV || source.device != device {
		t.Fatalf("legacy=%+v %v shared=%+v %v calls=%d device=%s", want, we, got, ge, source.calls, source.device)
	}
	if len(got) != 2 || got[0].Key != future.Key || got[1].Key != child.Key {
		t.Fatalf("legacy selection changed: %+v", got)
	}
	source.err = errors.New("items failed")
	_, we = f.srv.controlLegacyScopedCollectionItems(ctx, &ownerUser, key)
	_, ge = f.srv.scopedCollectionItems(ctx, &ownerUser, key)
	if !errors.Is(we, source.err) || !errors.Is(ge, source.err) {
		t.Fatalf("errors legacy=%v shared=%v", we, ge)
	}
	source.err = nil
	source.calls = 0
	_, ge = f.srv.scopedCollectionItems(ctx, &friendUser, key)
	if !errors.Is(ge, core.ErrNotFound) || source.calls != 0 {
		t.Fatalf("placeholder gate error=%v calls=%d", ge, source.calls)
	}
	source.calls = 0
	_, ge = f.srv.scopedCollectionItems(ctx, nil, key)
	if !errors.Is(ge, core.ErrNotFound) || source.calls != 0 {
		t.Fatalf("nil viewer gate error=%v calls=%d", ge, source.calls)
	}
	f.srv.d.Collections = f.cols
	_, ge = f.srv.scopedCollectionItems(ctx, &ownerUser, key)
	if !errors.Is(ge, core.ErrNotFound) {
		t.Fatalf("missing capability: %v", ge)
	}
}
