package experience

import (
	"context"
	"errors"
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
)

func TestArtworkAuthorizationPropagatesCacheFailure(t *testing.T) {
	failure := errors.New("catalogue unavailable")
	s := &Service{d: ServiceDeps{
		Active:    func(_ context.Context, id int64) (*core.User, error) { return &core.User{ID: id}, nil },
		CachedGet: func(context.Context, core.RatingKey) (*core.Title, error) { return nil, failure },
		Get: func(context.Context, *core.User, core.RatingKey) (*core.Title, error) {
			t.Fatal("network-capable lookup used")
			return nil, nil
		},
	}}
	if _, err := s.AuthorizeArtwork(context.Background(), &core.User{ID: 1}, core.Device{}, core.MovieKey(1)); !errors.Is(err, failure) {
		t.Fatalf("cache error hidden: %v", err)
	}
}
