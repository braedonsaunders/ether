package state

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
)

func newTestState(t *testing.T) (*State, *time.Time) {
	t.Helper()
	db, err := store.OpenMemory(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	s := New(db, Options{Now: func() time.Time { return now }})
	return s, &now
}

var ctx = context.Background()

func TestProgressThresholds(t *testing.T) {
	s, now := newTestState(t)
	key := core.MovieKey(1)
	dur := 100 * time.Minute

	// 30 seconds — not started, no resume point.
	if err := s.Progress(ctx, 1, key, 30*time.Second, dur, "stopped"); err != nil {
		t.Fatal(err)
	}
	ws, _ := s.Get(ctx, 1, key)
	if ws.ViewOffset != 0 || ws.ViewCount != 0 {
		t.Fatalf("30s: %+v", ws)
	}

	// 50% — in progress.
	*now = now.Add(time.Hour)
	if err := s.Progress(ctx, 1, key, 50*time.Minute, dur, "paused"); err != nil {
		t.Fatal(err)
	}
	ws, _ = s.Get(ctx, 1, key)
	if ws.ViewOffset != 50*time.Minute || ws.ViewCount != 0 {
		t.Fatalf("50%%: %+v", ws)
	}
	ip, _ := s.InProgress(ctx, 1, 10)
	if len(ip) != 1 || ip[0].Key != key {
		t.Fatalf("InProgress: %+v", ip)
	}

	// 91% — watched exactly once; repeating end progress must not recount.
	if err := s.Progress(ctx, 1, key, 91*time.Minute, dur, "stopped"); err != nil {
		t.Fatal(err)
	}
	if err := s.Progress(ctx, 1, key, 99*time.Minute, dur, "stopped"); err != nil {
		t.Fatal(err)
	}
	// Scrub below both start thresholds and to the end again: still one.
	if err := s.Progress(ctx, 1, key, 30*time.Second, dur, "playing"); err != nil {
		t.Fatal(err)
	}
	if err := s.Progress(ctx, 1, key, 95*time.Minute, dur, "stopped"); err != nil {
		t.Fatal(err)
	}
	ws, _ = s.Get(ctx, 1, key)
	if ws.ViewCount != 1 || ws.ViewOffset != 0 {
		t.Fatalf("watched: %+v", ws)
	}
	if ip, _ := s.InProgress(ctx, 1, 10); len(ip) != 0 {
		t.Fatalf("should leave InProgress: %+v", ip)
	}

	// A fresh rewatch past the started threshold, then to the end: counts again.
	if err := s.Progress(ctx, 1, key, 40*time.Minute, dur, "playing"); err != nil {
		t.Fatal(err)
	}
	if err := s.Progress(ctx, 1, key, 92*time.Minute, dur, "stopped"); err != nil {
		t.Fatal(err)
	}
	ws, _ = s.Get(ctx, 1, key)
	if ws.ViewCount != 2 {
		t.Fatalf("rewatch: %+v", ws)
	}

	hist, _ := s.History(ctx, 1, time.Time{}, 50)
	completed := 0
	for _, h := range hist {
		if h.Completed {
			completed++
		}
	}
	if completed != 2 {
		t.Fatalf("completed history entries = %d, want 2 (%+v)", completed, hist)
	}
}

func TestResumeStartsAtFivePercent(t *testing.T) {
	for _, tc := range []struct {
		name     string
		duration time.Duration
		offset   time.Duration
		resume   bool
		episode  bool
	}{
		{"movie-30s", 97 * time.Minute, 30 * time.Second, false, false},
		{"movie-61s", 97 * time.Minute, 61 * time.Second, false, false},
		{"movie-exact-minute", 97 * time.Minute, time.Minute, false, false},
		{"infuse-actual-stop", 97 * time.Minute, 201274 * time.Millisecond, false, false},
		{"episode-just-under-five-percent", 20 * time.Minute, 59 * time.Second, false, true},
		{"short-item-existing-five-percent", 10 * time.Minute, 30 * time.Second, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := newTestState(t)
			key := core.MovieKey(4951)
			if tc.episode {
				key = core.EpisodeKey(1, 1, 1)
			}
			if err := s.Progress(ctx, 1, key, tc.offset, tc.duration, "stopped"); err != nil {
				t.Fatal(err)
			}
			got, err := s.Get(ctx, 1, key)
			if err != nil {
				t.Fatal(err)
			}
			want := time.Duration(0)
			if tc.resume {
				want = tc.offset
			}
			if got.ViewOffset != want || got.ViewCount != 0 {
				t.Fatalf("offset=%v count=%d; want offset=%v count=0", got.ViewOffset, got.ViewCount, want)
			}
			progress, err := s.InProgress(ctx, 1, 10)
			if err != nil || (len(progress) > 0) != tc.resume {
				t.Fatalf("in-progress=%+v err=%v; want resumable=%v", progress, err, tc.resume)
			}
		})
	}
}

func TestScrobbleUnscrobbleRate(t *testing.T) {
	s, _ := newTestState(t)
	key := core.MovieKey(2)
	if err := s.Rate(ctx, 1, key, 8.5); err != nil {
		t.Fatal(err)
	}
	if err := s.Rate(ctx, 1, key, 11); err == nil {
		t.Fatal("rating >10 should error")
	}
	if err := s.Scrobble(ctx, 1, key); err != nil {
		t.Fatal(err)
	}
	ws, _ := s.Get(ctx, 1, key)
	if ws.ViewCount != 1 || ws.UserRating != 8.5 {
		t.Fatalf("%+v", ws)
	}
	if err := s.Unscrobble(ctx, 1, key); err != nil {
		t.Fatal(err)
	}
	ws, _ = s.Get(ctx, 1, key)
	if ws.ViewCount != 0 || ws.ViewOffset != 0 || ws.UserRating != 8.5 {
		t.Fatalf("unscrobble keeps rating: %+v", ws)
	}
	// Per-user isolation.
	other, _ := s.Get(ctx, 2, key)
	if other.ViewCount != 0 || other.UserRating != 0 {
		t.Fatalf("user isolation: %+v", other)
	}
}

func TestInProgressOrderingAndAny(t *testing.T) {
	s, now := newTestState(t)
	a, b := core.MovieKey(10), core.MovieKey(11)
	dur := time.Hour
	_ = s.Progress(ctx, 1, a, 30*time.Minute, dur, "paused")
	*now = now.Add(time.Hour)
	_ = s.Progress(ctx, 1, b, 30*time.Minute, dur, "paused")
	_ = s.Progress(ctx, 2, a, 30*time.Minute, dur, "paused")

	ip, _ := s.InProgress(ctx, 1, 10)
	if len(ip) != 2 || ip[0].Key != b || ip[1].Key != a {
		t.Fatalf("recency order: %+v", ip)
	}
	anyKeys, _ := s.InProgressAny(ctx)
	if len(anyKeys) != 2 {
		t.Fatalf("InProgressAny distinct: %v", anyKeys)
	}
	m, _ := s.GetMany(ctx, 1, []core.RatingKey{a, b, core.MovieKey(99)})
	if len(m) != 3 || m[core.MovieKey(99)].ViewCount != 0 {
		t.Fatalf("GetMany: %v", m)
	}
}

func TestEvents(t *testing.T) {
	s, _ := newTestState(t)
	var kinds []EventKind
	unsub := s.Subscribe(func(ev Event) { kinds = append(kinds, ev.Kind) })
	s.Subscribe(func(Event) { panic("boom") }) // must be recovered
	key := core.MovieKey(3)
	_ = s.Progress(ctx, 1, key, 95*time.Minute, 100*time.Minute, "stopped")
	_ = s.Rate(ctx, 1, key, 7)
	unsub()
	_ = s.Scrobble(ctx, 1, key)
	want := []EventKind{EventProgress, EventCompleted, EventRated}
	if !slices.Equal(kinds, want) {
		t.Fatalf("kinds=%v want %v", kinds, want)
	}
}

func TestRollup(t *testing.T) {
	s, _ := newTestState(t)
	eps := []core.RatingKey{core.EpisodeKey(5, 1, 1), core.EpisodeKey(5, 1, 2), core.EpisodeKey(5, 1, 3)}
	_ = s.Scrobble(ctx, 1, eps[0])
	_ = s.Progress(ctx, 1, eps[1], 20*time.Minute, 40*time.Minute, "paused")
	r, _, err := s.ChildrenStatus(ctx, 1, eps)
	if err != nil {
		t.Fatal(err)
	}
	if r.Total != 3 || r.Watched != 1 || r.InProgress != 1 || r.AllWatched || !r.AnyWatched {
		t.Fatalf("%+v", r)
	}
	_ = s.Scrobble(ctx, 1, eps[1])
	_ = s.Scrobble(ctx, 1, eps[2])
	r, _, _ = s.ChildrenStatus(ctx, 1, eps)
	if !r.AllWatched {
		t.Fatalf("%+v", r)
	}
}

func TestPlaylists(t *testing.T) {
	s, _ := newTestState(t)
	pl := s.Playlists()
	a, b, c, d := core.MovieKey(1), core.MovieKey(2), core.MovieKey(3), core.MovieKey(4)
	p, err := pl.Create(ctx, 1, "Favs", []core.RatingKey{a, b, c})
	if err != nil {
		t.Fatal(err)
	}
	if p.Key.Kind() != core.KindPlaylist {
		t.Fatalf("key kind %v", p.Key.Kind())
	}
	if err := pl.Add(ctx, 1, p.Key, []core.RatingKey{d}); err != nil {
		t.Fatal(err)
	}
	if err := pl.Move(ctx, 1, p.Key, 3, 0); err != nil {
		t.Fatal(err)
	}
	got, _ := pl.Get(ctx, 1, p.Key)
	if !slices.Equal(got.Items, []core.RatingKey{d, a, b, c}) {
		t.Fatalf("after move: %v", got.Items)
	}
	if err := pl.Move(ctx, 1, p.Key, 0, 2); err != nil {
		t.Fatal(err)
	}
	got, _ = pl.Get(ctx, 1, p.Key)
	if !slices.Equal(got.Items, []core.RatingKey{a, b, d, c}) {
		t.Fatalf("move forward: %v", got.Items)
	}
	if err := pl.Remove(ctx, 1, p.Key, 1); err != nil {
		t.Fatal(err)
	}
	got, _ = pl.Get(ctx, 1, p.Key)
	if !slices.Equal(got.Items, []core.RatingKey{a, d, c}) {
		t.Fatalf("after remove: %v", got.Items)
	}
	if err := pl.Remove(ctx, 1, p.Key, 9); err == nil {
		t.Fatal("out-of-range remove should error")
	}
	if _, err := pl.Get(ctx, 2, p.Key); err == nil {
		t.Fatal("other user must not see playlist")
	}
	list, _ := pl.List(ctx, 1)
	if len(list) != 1 {
		t.Fatalf("list: %v", list)
	}
	if err := pl.Delete(ctx, 1, p.Key); err != nil {
		t.Fatal(err)
	}
	if _, err := pl.Get(ctx, 1, p.Key); err == nil {
		t.Fatal("deleted playlist should be gone")
	}
}

func TestConcurrentProgressNoLostUpdates(t *testing.T) {
	db, err := store.OpenPath(ctx, t.TempDir()+"/state.db")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s := New(db, Options{})
	var completions int
	var mu sync.Mutex
	s.Subscribe(func(ev Event) {
		if ev.Kind == EventCompleted {
			mu.Lock()
			completions++
			mu.Unlock()
		}
	})
	key := core.MovieKey(77)
	dur := 100 * time.Minute
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_ = s.Progress(ctx, 1, key, time.Duration(10+i)*time.Minute, dur, "playing")
			_ = s.Progress(ctx, 1, key, 95*time.Minute, dur, "stopped")
		}(i)
	}
	wg.Wait()
	ws, _ := s.Get(ctx, 1, key)
	mu.Lock()
	defer mu.Unlock()
	if ws.ViewCount != completions || ws.ViewCount < 1 {
		t.Fatalf("view count %d vs completion events %d", ws.ViewCount, completions)
	}
}

func TestProgressEdgeCases(t *testing.T) {
	s, now := newTestState(t)
	key := core.EpisodeKey(3, 1, 1)

	// Unknown duration: resume point kept, never auto-watched.
	if err := s.Progress(ctx, 1, key, 30*time.Minute, 0, "playing"); err != nil {
		t.Fatal(err)
	}
	ws, _ := s.Get(ctx, 1, key)
	if ws.ViewOffset != 30*time.Minute || ws.ViewCount != 0 {
		t.Fatalf("unknown duration: %+v", ws)
	}
	// Duration learnt later; offset beyond duration is clamped and watched.
	*now = now.Add(time.Minute)
	if err := s.Progress(ctx, 1, key, 50*time.Minute, 40*time.Minute, "stopped"); err != nil {
		t.Fatal(err)
	}
	ws, _ = s.Get(ctx, 1, key)
	if ws.ViewCount != 1 || ws.ViewOffset != 0 || ws.Duration != 40*time.Minute {
		t.Fatalf("clamped completion: %+v", ws)
	}
	// Negative offsets are treated as 0.
	if err := s.Progress(ctx, 1, core.MovieKey(4), -time.Second, time.Hour, "playing"); err != nil {
		t.Fatal(err)
	}

	// A "stopped" first report in the middle does not log a "started" entry.
	other := core.MovieKey(5)
	_ = s.Progress(ctx, 2, other, 30*time.Minute, time.Hour, "stopped")
	hist, _ := s.History(ctx, 2, time.Time{}, 10)
	if len(hist) != 0 {
		t.Fatalf("stopped-only progress logged history: %+v", hist)
	}
	_ = s.Progress(ctx, 2, core.MovieKey(6), 30*time.Minute, time.Hour, "playing")
	hist, _ = s.History(ctx, 2, time.Time{}, 10)
	if len(hist) != 1 || hist[0].Completed {
		t.Fatalf("start entry: %+v", hist)
	}

	// History since filter.
	cut := now.Add(time.Hour)
	*now = cut.Add(time.Minute)
	_ = s.Scrobble(ctx, 2, core.MovieKey(7))
	hist, _ = s.History(ctx, 2, cut, 10)
	if len(hist) != 1 || hist[0].Key != core.MovieKey(7) || !hist[0].Completed {
		t.Fatalf("since filter: %+v", hist)
	}

	// Rating validation.
	if err := s.Rate(ctx, 1, key, -1); err == nil {
		t.Fatal("negative rating should error")
	}
	if err := s.Rate(ctx, 1, key, 0); err != nil {
		t.Fatal(err)
	}
}

func TestScrobbleManyRollup(t *testing.T) {
	s, _ := newTestState(t)
	eps := []core.RatingKey{core.EpisodeKey(8, 1, 1), core.EpisodeKey(8, 1, 2), core.EpisodeKey(8, 2, 1)}
	var evs []Event
	s.Subscribe(func(ev Event) { evs = append(evs, ev) })
	_ = s.Progress(ctx, 1, eps[1], 10*time.Minute, 40*time.Minute, "paused")
	evs = nil
	if err := s.ScrobbleMany(ctx, 1, eps); err != nil {
		t.Fatal(err)
	}
	if len(evs) != 3 || evs[0].Kind != EventScrobble || evs[2].Key != eps[2] {
		t.Fatalf("events: %+v", evs)
	}
	r, m, _ := s.ChildrenStatus(ctx, 1, eps)
	if !r.AllWatched || r.InProgress != 0 || m[eps[1]].ViewOffset != 0 {
		t.Fatalf("after ScrobbleMany: %+v %+v", r, m)
	}
	if err := s.UnscrobbleMany(ctx, 1, eps[:2]); err != nil {
		t.Fatal(err)
	}
	r, _, _ = s.ChildrenStatus(ctx, 1, eps)
	if r.Watched != 1 || r.AllWatched || !r.AnyWatched {
		t.Fatalf("after UnscrobbleMany: %+v", r)
	}
	// History is a log: unscrobble keeps completed entries.
	hist, _ := s.History(ctx, 1, time.Time{}, 50)
	completed := 0
	for _, h := range hist {
		if h.Completed {
			completed++
		}
	}
	if completed != 3 {
		t.Fatalf("history completions %d (%+v)", completed, hist)
	}
	if empty := Summarize(nil, nil); empty.AllWatched || empty.Total != 0 {
		t.Fatalf("empty rollup: %+v", empty)
	}
	_ = s.Scrobble(ctx, 1, core.MovieKey(99))
	watchedEps, err := s.WatchedEpisodes(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(watchedEps) != 1 || watchedEps[0] != eps[2] {
		t.Fatalf("WatchedEpisodes = %v, want [%v]", watchedEps, eps[2])
	}
}

func TestPlaylistEdgeCases(t *testing.T) {
	s, _ := newTestState(t)
	pl := s.Playlists()
	if _, err := pl.Create(ctx, 1, "", nil); err == nil {
		t.Fatal("empty title should error")
	}
	p, err := pl.Create(ctx, 1, "Empty", nil)
	if err != nil || len(p.Items) != 0 {
		t.Fatalf("empty create: %+v %v", p, err)
	}
	if err := pl.Move(ctx, 1, p.Key, 0, 0); err == nil {
		t.Fatal("move on empty playlist should error")
	}
	if err := pl.Add(ctx, 2, p.Key, []core.RatingKey{core.MovieKey(1)}); err == nil {
		t.Fatal("other user must not add")
	}
	if err := pl.Delete(ctx, 2, p.Key); err == nil {
		t.Fatal("other user must not delete")
	}
	if _, err := pl.Get(ctx, 1, core.MovieKey(1)); err == nil {
		t.Fatal("non-playlist key should error")
	}
	// Duplicates are allowed (Plex playlists may repeat items) and keep order.
	k := core.MovieKey(9)
	if err := pl.Add(ctx, 1, p.Key, []core.RatingKey{k, k}); err != nil {
		t.Fatal(err)
	}
	got, _ := pl.Get(ctx, 1, p.Key)
	if !slices.Equal(got.Items, []core.RatingKey{k, k}) {
		t.Fatalf("dupes: %v", got.Items)
	}
	p2, _ := pl.Create(ctx, 1, "Second", []core.RatingKey{k})
	list, _ := pl.List(ctx, 1)
	if len(list) != 2 {
		t.Fatalf("list: %+v", list)
	}
	_ = p2
}

func TestWatchedEpisodesOfReadsOnlyTheListedShows(t *testing.T) {
	s, _ := newTestState(t)
	keys := []core.RatingKey{core.EpisodeKey(1, 1, 1), core.EpisodeKey(1, 2, 3), core.EpisodeKey(2, 1, 1), core.EpisodeKey(3, 999, 9999), core.MovieKey(1)}
	if err := s.ScrobbleMany(ctx, 7, keys); err != nil {
		t.Fatal(err)
	}
	if err := s.Scrobble(ctx, 8, core.EpisodeKey(1, 1, 2)); err != nil {
		t.Fatal(err)
	}
	got, err := s.WatchedEpisodesOf(ctx, 7, []int64{1, 3, 4})
	if err != nil {
		t.Fatal(err)
	}
	want := []core.RatingKey{core.EpisodeKey(1, 1, 1), core.EpisodeKey(1, 2, 3), core.EpisodeKey(3, 999, 9999)}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("watched = %v, want %v", got, want)
	}
	all, err := s.WatchedEpisodes(ctx, 7)
	if err != nil || len(all) != 4 {
		t.Fatalf("all watched = %v, %v", all, err)
	}
}
