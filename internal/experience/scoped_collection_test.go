package experience

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

type scopedCollectionFake struct {
	core.Collections
	items   []core.Title
	device  string
	calls   int
	allowed bool
	err     error
}

func (*scopedCollectionFake) Get(context.Context, *core.User, core.RatingKey) (*core.Collection, error) {
	return nil, core.ErrNotFound
}
func (s *scopedCollectionFake) AllowsPlaceholders(_ context.Context, _ *core.User, _ core.RatingKey, device string) bool {
	s.device = device
	return s.allowed
}
func (*scopedCollectionFake) Placeholder(context.Context, *core.User, core.RatingKey, core.RatingKey, string) (*core.Title, error) {
	return nil, core.ErrNotFound
}
func (*scopedCollectionFake) FindPlaceholder(context.Context, *core.User, core.RatingKey, string) (*core.Title, core.RatingKey, error) {
	return nil, 0, core.ErrNotFound
}
func (s *scopedCollectionFake) Items(_ context.Context, _ *core.User, _ core.RatingKey, offset, limit int) (core.Page, error) {
	s.calls++
	if offset != 0 || limit != 5000 {
		return core.Page{}, errors.New("wrong page")
	}
	return core.Page{Items: s.items}, s.err
}

func TestScopedCollectionEnforcesViewerAndParentPolicy(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	child := core.Title{Key: core.EpisodeKey(7, 1, 1), ShowKey: core.ShowKey(7), Type: core.TypeEpisode, Released: now.Add(time.Hour)}
	visible := core.Title{Key: core.MovieKey(8), Type: core.TypeMovie, Released: now.Add(time.Hour)}
	src := &scopedCollectionFake{allowed: true, items: []core.Title{child, visible}}
	service := ScopedCollection{Collections: src, Now: func() time.Time { return now }, Visibility: Visibility{GetTitle: func(context.Context, core.RatingKey) (*core.Title, error) { return &core.Title{Adult: true}, nil }}}
	got, err := service.Items(context.Background(), &core.User{ID: 1}, core.SmartKey(5), core.DeviceTV)
	if err != nil || len(got) != 1 || got[0].Key != visible.Key || src.device != core.DeviceTV || src.calls != 1 {
		t.Fatalf("items=%+v err=%v source=%+v", got, err, src)
	}
	// Nil trusted capability must remain on the inherited-parent path even
	// when a host happens to supply no viewer.
	got, err = service.Items(context.Background(), nil, core.SmartKey(5), core.DeviceTV)
	if err != nil || len(got) != 1 || got[0].Key != visible.Key {
		t.Fatalf("nil capability/viewer bypassed parent policy: %+v %v", got, err)
	}
	src.allowed = false
	src.calls = 0
	_, err = service.Items(context.Background(), &core.User{ID: 1}, core.SmartKey(5), core.DeviceTV)
	if !errors.Is(err, core.ErrNotFound) || src.calls != 0 {
		t.Fatalf("gate err=%v calls=%d", err, src.calls)
	}
}
