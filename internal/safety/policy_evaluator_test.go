package safety

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
)

func TestOwnedPolicyEvaluatorsMatchService(t *testing.T) {
	ctx := context.Background()
	db, err := store.OpenMemory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s, err := New(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	p := Policy{MaxContentRating: "PG", BlockedGenres: []string{"horror"}, BlockedKeywords: []string{"violence"}, RemoteDisabled: true, StreamLimit: 2, AllowedHours: []int{9}, Timezone: "UTC"}
	if err := s.Put(ctx, 1, p); err != nil {
		t.Fatal(err)
	}
	viewer := &core.User{ID: 1, MaxContentRating: "PG-13", StreamLimit: 4}
	titles := []*core.Title{nil, {Adult: true}, {ContentRating: "PG"}, {ContentRating: "R"}, {Genres: []string{" HORROR "}}, {Keywords: []string{"violence"}}}
	for i, title := range titles {
		if got, want := AllowedWithPolicy(viewer, title, p), s.Allowed(viewer, title); got != want {
			t.Fatalf("title %d got %v want %v", i, got, want)
		}
	}
	if got, want := AllowedWithPolicy(nil, &core.Title{}, Policy{}), s.Allowed(nil, &core.Title{}); got != want {
		t.Fatal("nil viewer mismatch")
	}
	for _, hour := range []int{8, 9, 10} {
		for _, remote := range []bool{false, true} {
			now := time.Date(2026, 1, 1, hour, 0, 0, 0, time.UTC)
			if got, want := CanPlayWithPolicy(viewer, p, remote, now), s.CanPlay(viewer, remote, now); got != want {
				t.Fatalf("hour=%d remote=%v got=%v want=%v", hour, remote, got, want)
			}
		}
	}
	if got, want := UserPolicyWithPolicy(viewer, p), s.UserPolicy(viewer); !reflect.DeepEqual(got, want) {
		t.Fatalf("user policy got=%+v want=%+v", got, want)
	}
}
