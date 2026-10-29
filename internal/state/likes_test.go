package state

import (
	"context"
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
)

func TestSetLikesUsesSharedRating(t *testing.T) {
	ctx := context.Background()
	s := New(store.OpenTest(t), Options{})
	key := core.MovieKey(42)
	like, dislike := true, false
	for _, tc := range []struct {
		before float64
		input  *bool
		want   float64
	}{
		{0, &like, 10}, {8, &like, 8}, {6, &like, 10},
		{0, &dislike, 2}, {1, &dislike, 1}, {8, &dislike, 2}, {5, nil, 0},
	} {
		if err := s.Rate(ctx, 1, key, tc.before); err != nil {
			t.Fatal(err)
		}
		if err := s.SetLikes(ctx, 1, key, tc.input); err != nil {
			t.Fatal(err)
		}
		got, err := s.Get(ctx, 1, key)
		if err != nil || got.UserRating != tc.want {
			t.Fatalf("before %v: got %v want %v (%v)", tc.before, got.UserRating, tc.want, err)
		}
	}
}
