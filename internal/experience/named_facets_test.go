package experience

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
)

func TestNamedFacetIDsAuthorizationAndBounds(t *testing.T) {
	v := &core.User{ID: 1}
	title := core.Title{Key: core.MovieKey(1), Type: core.TypeMovie, Genres: []string{"Drama", "Drama"}, Studios: []string{"Studio"}}
	s := &Service{d: ServiceDeps{Active: func(_ context.Context, id int64) (*core.User, error) {
		if id != 1 {
			return nil, core.ErrUnauthorized
		}
		return v, nil
	}, Get: func(_ context.Context, _ *core.User, k core.RatingKey) (*core.Title, error) {
		if k != title.Key {
			return nil, core.ErrNotFound
		}
		return &title, nil
	}, Visibility: Visibility{SectionFor: func(core.MediaType) (int, bool) { return 1, true }}}}
	ctx := context.Background()
	for _, tc := range []struct{ kind, name string }{{"genre", "Drama"}, {"studio", "Studio"}} {
		got, err := s.NamedFacetIDs(ctx, v, core.Device{}, title.Key, tc.kind, []string{tc.name, "missing", tc.name})
		if err != nil || len(got) != 1 || got[tc.name] != facetID(tc.kind, tc.name) {
			t.Fatalf("%s: %v %v", tc.kind, got, err)
		}
	}
	title.Adult = true
	if _, err := s.NamedFacetIDs(ctx, v, core.Device{}, title.Key, "genre", []string{"Drama"}); !errors.Is(err, core.ErrNotFound) {
		t.Fatal("hidden parent", err)
	}
	if _, err := s.NamedFacetIDs(ctx, v, core.Device{}, title.Key, "genre", make([]string, 257)); err == nil {
		t.Fatal("batch limit")
	}
	if _, err := s.NamedFacetIDs(ctx, v, core.Device{}, title.Key, "genre", []string{strings.Repeat("x", 1025)}); err == nil {
		t.Fatal("length limit")
	}
	if _, err := s.NamedFacetIDs(ctx, v, core.Device{}, title.Key, "person", []string{"Drama"}); err == nil {
		t.Fatal("invalid kind")
	}
}
