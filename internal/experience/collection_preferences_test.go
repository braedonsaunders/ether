package experience

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
)

type completeCollections struct {
	core.Collections
	lists map[core.MediaType][]core.Collection
	items map[core.RatingKey][]core.Title
	err   error
}

func (f completeCollections) ListComplete(_ context.Context, _ *core.User, typ core.MediaType) ([]core.Collection, error) {
	return f.lists[typ], f.err
}
func (f completeCollections) Items(_ context.Context, _ *core.User, k core.RatingKey, offset, limit int) (core.Page, error) {
	items := f.items[k]
	if offset >= len(items) {
		return core.Page{Total: len(items)}, nil
	}
	return core.Page{Items: items[offset:min(offset+limit, len(items))], Total: len(items)}, nil
}
func TestCollectionsFilterBeforePaginationAndCount(t *testing.T) {
	ctx := context.Background()
	viewer := &core.User{ID: 1, AllowedSections: []int{1}}
	a, b, c := core.CollectionKey(1), core.CollectionKey(2), core.CollectionKey(3)
	hidden := core.Title{Key: core.MovieKey(1), Type: core.TypeMovie, Adult: true}
	allowed := core.Title{Key: core.MovieKey(2), Type: core.TypeMovie}
	src := completeCollections{lists: map[core.MediaType][]core.Collection{core.TypeMovie: {{Key: a, Type: core.TypeMovie}, {Key: b, Type: core.TypeMovie}, {Key: c, Type: core.TypeMovie}}, core.TypeShow: {{Key: core.SmartKey(4), Type: core.TypeShow}}}, items: map[core.RatingKey][]core.Title{a: {hidden}, b: {hidden, allowed}, c: {allowed}}}
	s := &Service{d: ServiceDeps{Active: func(_ context.Context, id int64) (*core.User, error) {
		if id != 1 {
			return nil, core.ErrUnauthorized
		}
		return viewer, nil
	}, Libraries: []Library{{ID: 1, Type: core.TypeMovie}, {ID: 2, Type: core.TypeShow}}, Visibility: Visibility{SectionFor: func(typ core.MediaType) (int, bool) {
		if typ == core.TypeShow {
			return 2, true
		}
		return 1, true
	}}, Collections: src}}
	page, err := s.Collections(ctx, viewer, core.Device{}, frontend.CollectionQuery{Limit: 1})
	if err != nil || page.Total != 2 || len(page.Items) != 1 || page.Items[0].Key != b || page.Items[0].ChildCount != 1 {
		t.Fatalf("filtered page %+v %v", page, err)
	}
	page, err = s.Collections(ctx, viewer, core.Device{}, frontend.CollectionQuery{Offset: 1, Limit: 1})
	if err != nil || len(page.Items) != 1 || page.Items[0].Key != c {
		t.Fatalf("second page %+v %v", page, err)
	}
	src.err = errors.New("incomplete")
	s.d.Collections = src
	if _, err = s.Collections(ctx, viewer, core.Device{}, frontend.CollectionQuery{}); err == nil {
		t.Fatal("partial collection list accepted")
	}
	if _, err = s.Collections(ctx, &core.User{ID: 2}, core.Device{}, frontend.CollectionQuery{}); !errors.Is(err, core.ErrNotFound) {
		t.Fatal("inactive viewer", err)
	}
}
func TestNotInterestedPagingAndSingleEntry(t *testing.T) {
	ctx := context.Background()
	v := &core.User{ID: 1}
	a := core.Title{Key: core.MovieKey(1), Type: core.TypeMovie}
	b := core.Title{Key: core.MovieKey(2), Type: core.TypeMovie, Adult: true}
	added := time.Unix(123, 0)
	s := &Service{d: ServiceDeps{Active: func(context.Context, int64) (*core.User, error) { return v, nil }, Visibility: Visibility{SectionFor: func(core.MediaType) (int, bool) { return 1, true }}, Get: func(_ context.Context, _ *core.User, k core.RatingKey) (*core.Title, error) {
		switch k {
		case a.Key:
			return &a, nil
		case b.Key:
			return &b, nil
		}
		return nil, core.ErrNotFound
	}, NotInterestedSource: func(context.Context, *core.User) ([]frontend.NotInterestedEntry, error) {
		return []frontend.NotInterestedEntry{{Key: b.Key, AddedAt: added}, {Key: a.Key, AddedAt: added}}, nil
	}}}
	page, err := s.NotInterested(ctx, v, core.Device{}, frontend.PageQuery{Limit: 1})
	if err != nil || page.Total != 1 || len(page.Items) != 1 || page.Items[0].Key != a.Key || !page.Items[0].AddedAt.Equal(added) {
		t.Fatal(page, err)
	}
	item, err := s.NotInterestedItem(ctx, v, core.Device{}, a.Key)
	if err != nil || !item.AddedAt.Equal(added) {
		t.Fatal(item, err)
	}
	if _, err = s.NotInterestedItem(ctx, v, core.Device{}, b.Key); !errors.Is(err, core.ErrNotFound) {
		t.Fatal("hidden item", err)
	}
}
