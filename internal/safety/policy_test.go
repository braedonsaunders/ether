package safety

import (
	"context"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
	"testing"
	"time"
)

func TestPolicyPersistsAndFailsClosed(t *testing.T) {
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
	u := &core.User{ID: 1}
	p := KidsPreset("preschool")
	p.RemoteDisabled = true
	p.AllowedHours = []int{9}
	if err := s.Put(ctx, 1, p); err != nil {
		t.Fatal(err)
	}
	s, err = New(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		title core.Title
		want  bool
	}{{core.Title{ContentRating: "TV-Y"}, true}, {core.Title{}, false}, {core.Title{ContentRating: "R"}, false}, {core.Title{ContentRating: "G", Adult: true}, false}, {core.Title{ContentRating: "G", Keywords: []string{"pornography"}}, false}} {
		if got := s.Allowed(u, &tt.title); got != tt.want {
			t.Errorf("%+v got %v", tt.title, got)
		}
	}
	now := time.Date(2026, 1, 1, 9, 0, 0, 0, time.UTC)
	if s.CanPlay(u, true, now) || !s.CanPlay(u, false, now) || s.CanPlay(u, false, now.Add(time.Hour)) {
		t.Fatal("play policy failed")
	}
}

// The restricted-viewer cap compares age levels: a TV-PG cap admits PG and
// a German 6, a PG-13 cap admits TV-14, and unrated titles stay blocked.
func TestRatingCapComparesLevels(t *testing.T) {
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
	if err := s.Put(ctx, 1, Policy{MaxContentRating: "TV-PG"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Put(ctx, 2, Policy{MaxContentRating: "gb/12A"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Put(ctx, 3, Policy{MaxContentRating: "bogus"}); err == nil {
		t.Fatal("a limit naming no level was stored")
	}
	for _, tc := range []struct {
		viewer int64
		rating string
		want   bool
	}{
		{1, "PG", true}, {1, "G", true}, {1, "TV-Y7", true}, {1, "de/6", true}, {1, "TV-PG", true},
		{1, "PG-13", false}, {1, "", false}, {1, "NR", false},
		{2, "TV-14", true}, {2, "PG-13", true}, {2, "R", false}, {2, "gb/15", false},
	} {
		if got := s.Allowed(&core.User{ID: tc.viewer}, &core.Title{ContentRating: tc.rating}); got != tc.want {
			t.Errorf("viewer %d, %q: got %v", tc.viewer, tc.rating, got)
		}
	}
	// The stricter of the account limit and the policy wins, by level.
	if got := s.UserPolicy(&core.User{ID: 1, MaxContentRating: "TV-14"}).MaxContentRating; got != "TV-PG" {
		t.Fatalf("effective limit %q", got)
	}
	if got := s.UserPolicy(&core.User{ID: 2, MaxContentRating: "PG"}).MaxContentRating; got != "PG" {
		t.Fatalf("effective limit %q", got)
	}
}
