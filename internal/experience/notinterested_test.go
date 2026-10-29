package experience

import (
	"context"
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
)

func TestNotInterestedUserDataAndMutationArePolicyChecked(t *testing.T) {
	ctx := context.Background()
	v := &core.User{ID: 1}
	title := core.Title{Key: core.MovieKey(1), Type: core.TypeMovie}
	on := false
	s := &Service{d: ServiceDeps{Active: func(_ context.Context, id int64) (*core.User, error) {
		if id == 1 {
			return v, nil
		}
		return nil, core.ErrUnauthorized
	}, Get: func(_ context.Context, _ *core.User, k core.RatingKey) (*core.Title, error) {
		if k == title.Key {
			return &title, nil
		}
		return nil, core.ErrNotFound
	}, Visibility: Visibility{SectionFor: func(core.MediaType) (int, bool) { return 1, true }}, State: serviceState{}, IsNotInterested: func(context.Context, *core.User, core.RatingKey) (bool, error) { return on, nil }, SetNotInterestedSource: func(_ context.Context, _ *core.User, _ core.RatingKey, value bool) error { on = value; return nil }}}
	if err := s.SetNotInterested(ctx, v, title.Key, true); err != nil {
		t.Fatal(err)
	}
	detail, err := s.Item(ctx, frontend.ItemRequest{Viewer: v, Key: title.Key})
	if err != nil || !detail.UserData.IsNotInterested {
		t.Fatalf("detail %+v %v", detail.UserData, err)
	}
	data, err := s.UserData(ctx, v, []core.RatingKey{title.Key})
	if err != nil || !data[title.Key].IsNotInterested {
		t.Fatalf("userdata %+v %v", data, err)
	}
	title.Adult = true
	if err := s.SetNotInterested(ctx, v, title.Key, false); err == nil {
		t.Fatal("hidden mutation accepted")
	}
	data, err = s.UserData(ctx, v, []core.RatingKey{title.Key})
	if err != nil || len(data) != 0 {
		t.Fatalf("hidden userdata %+v %v", data, err)
	}
}
