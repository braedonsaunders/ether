package state

import (
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
)

func TestFollowedShowsGroupsEpisodesByViewerAndShow(t *testing.T) {
	// OpenTest runs on PostgreSQL too when the PostgreSQL test helper sets it up.
	at := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	now := &at
	s := New(store.OpenTest(t), Options{Now: func() time.Time { return *now }})
	start := *now
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(s.Scrobble(ctx, 1, core.EpisodeKey(100, 1, 1)))
	*now = start.Add(time.Hour)
	must(s.Scrobble(ctx, 1, core.EpisodeKey(100, 2, 3)))
	must(s.Progress(ctx, 2, core.EpisodeKey(200, 1, 1), 10*time.Minute, time.Hour, "playing"))
	*now = start.Add(2 * time.Hour)
	must(s.Scrobble(ctx, 1, core.MovieKey(300))) // movies are not shows
	must(s.Scrobble(ctx, 1, core.EpisodeKey(400, 1, 1)))
	must(s.Unscrobble(ctx, 1, core.EpisodeKey(400, 1, 1))) // nothing watched any more

	got, err := s.FollowedShows(ctx, start.Add(-time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	want := []ShowActivity{
		{UserID: 1, Show: core.ShowKey(100), LastViewed: start.Add(time.Hour)},
		{UserID: 2, Show: core.ShowKey(200), LastViewed: start.Add(time.Hour)},
	}
	if len(got) != len(want) {
		t.Fatalf("followed: %+v", got)
	}
	for _, w := range want {
		found := false
		for _, g := range got {
			found = found || (g.UserID == w.UserID && g.Show == w.Show && g.LastViewed.Equal(w.LastViewed))
		}
		if !found {
			t.Fatalf("missing %+v in %+v", w, got)
		}
	}
	later, err := s.FollowedShows(ctx, start.Add(30*time.Minute))
	if err != nil || len(later) != 2 {
		t.Fatalf("since filter: %+v %v", later, err)
	}
	none, err := s.FollowedShows(ctx, start.Add(3*time.Hour))
	if err != nil || len(none) != 0 {
		t.Fatalf("future since: %+v %v", none, err)
	}
}
