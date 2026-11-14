package prefetch

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/markers"
	"github.com/braedonsaunders/ether/internal/playback"
)

type fakePostPlayer struct {
	mu    sync.Mutex
	picks []core.PostPlayPick
	calls int
	last  core.RatingKey
}

func (f *fakePostPlayer) PostPlay(_ context.Context, _ *core.User, justWatched core.RatingKey, _ core.HubContext, limit int) ([]core.PostPlayPick, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.last = justWatched
	if limit > 0 && len(f.picks) > limit {
		return f.picks[:limit], nil
	}
	return f.picks, nil
}

type warmCall struct {
	key  core.RatingKey
	caps core.ClientCaps
	opts core.PrewarmOptions
}

type fakePrewarmer struct {
	mu    sync.Mutex
	calls []warmCall
}

func (f *fakePrewarmer) Prewarm(_ context.Context, _ *core.User, key core.RatingKey, caps core.ClientCaps, opts core.PrewarmOptions) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, warmCall{key, caps, opts})
	return true
}

func (f *fakePrewarmer) got() []warmCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]warmCall(nil), f.calls...)
}

type fakeMarkers struct{ ranges []markers.Marker }

func (f *fakeMarkers) Markers(context.Context, core.RatingKey, core.MediaInfo) ([]markers.Marker, error) {
	return f.ranges, nil
}

var (
	movieKey  = core.MovieKey(550)
	pickKey   = core.MovieKey(348)
	epFinale  = core.EpisodeKey(100, 1, 8)
	epNextSzn = core.EpisodeKey(100, 2, 1)
)

type ppFixture struct {
	p        *Prefetcher
	player   *fakePostPlayer
	warmer   *fakePrewarmer
	engine   *fakeEngine
	resolver *fakeResolver
}

func newPPFixture(t *testing.T, withWarmer bool, ms []markers.Marker, mod func(*ppFixture, *Deps)) *ppFixture {
	t.Helper()
	f := &ppFixture{
		player: &fakePostPlayer{picks: []core.PostPlayPick{
			{Title: core.Title{Key: pickKey, Type: core.TypeMovie, Title: "The Pick"}, Score: 0.9, Reason: "Because you watched X"},
		}},
		warmer:   &fakePrewarmer{},
		engine:   &fakeEngine{},
		resolver: &fakeResolver{},
	}
	cat := &fakeCatalog{next: map[core.RatingKey]*core.Title{
		ep1:      {Key: ep2, Type: core.TypeEpisode, Title: "Two"},
		epFinale: {Key: epNextSzn, Type: core.TypeEpisode, Title: "S2E1"},
	}}
	hints := &fakeHints{points: map[[2]int64]*playback.ResumePoint{}, live: map[core.RatingKey]bool{},
		caps: core.ClientCaps{ClientID: "tv", Product: "Plex for Apple TV", MaxHeight: 2160}}
	deps := Deps{Catalog: cat, Resolver: f.resolver, Engine: f.engine, Hints: hints,
		PostPlay: f.player, Markers: &fakeMarkers{ranges: ms}}
	if withWarmer {
		deps.Prewarmer = f.warmer
	}
	if mod != nil {
		mod(f, &deps)
	}
	p, err := New(deps, config.Defaults(t.TempDir()).Cache, Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { p.Close() })
	f.p = p
	return f
}

func (f *ppFixture) wait(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := f.p.waitIdle(ctx); err != nil {
		t.Fatal("prefetcher never went idle")
	}
}

// A movie crossing duration−3m warms the #1 pick once, with the session's
// caps and the post-play reason/byte budget.
func TestPostPlayMoviePrewarmOnce(t *testing.T) {
	f := newPPFixture(t, true, nil, nil)
	dur := 120 * time.Minute
	f.p.OnTimeline(user, movieKey, dur-11*time.Minute, dur, "playing") // probe
	f.wait(t)
	if n := len(f.warmer.got()); n != 0 {
		t.Fatalf("warmed during probe: %d calls", n)
	}
	f.p.OnTimeline(user, movieKey, dur-2*time.Minute, dur, "playing") // fire
	f.wait(t)
	f.p.OnTimeline(user, movieKey, dur-time.Minute, dur, "playing") // again: no-op
	f.wait(t)
	calls := f.warmer.got()
	if len(calls) != 1 {
		t.Fatalf("prewarm calls = %d, want 1", len(calls))
	}
	c := calls[0]
	if c.key != pickKey || c.caps.ClientID != "tv" {
		t.Errorf("prewarm key/caps = %v/%v", c.key, c.caps.ClientID)
	}
	if c.opts.Reason != core.PrewarmPostPlay || !c.opts.DuringPlayback || c.opts.PrefetchBytes != 40<<20 {
		t.Errorf("prewarm opts = %+v", c.opts)
	}
	if f.player.last != movieKey {
		t.Errorf("PostPlay called for %v, want the movie", f.player.last)
	}
}

// A (non-final) credits marker moves the trigger earlier than −3m.
func TestPostPlayCreditsTrigger(t *testing.T) {
	dur := 120 * time.Minute
	ms := []markers.Marker{
		{Type: "credits", StartMS: (dur - 8*time.Minute).Milliseconds(), EndMS: (dur - 6*time.Minute).Milliseconds()},
		{Type: "credits", StartMS: (dur - 2*time.Minute).Milliseconds(), EndMS: dur.Milliseconds(), Final: true},
	}
	f := newPPFixture(t, true, ms, nil)
	f.p.OnTimeline(user, movieKey, dur-11*time.Minute, dur, "playing")
	f.wait(t)
	f.p.OnTimeline(user, movieKey, dur-7*time.Minute, dur, "playing") // past first credits, before −3m
	f.wait(t)
	if n := len(f.warmer.got()); n != 1 {
		t.Fatalf("prewarm calls = %d, want 1 at the credits marker", n)
	}
}

func TestCreditsTriggerSelection(t *testing.T) {
	dur := 100 * time.Minute
	cases := []struct {
		name string
		ms   []markers.Marker
		want time.Duration
		ok   bool
	}{
		{"none", nil, 0, false},
		{"final only", []markers.Marker{{Type: "credits", StartMS: 95 * 60000, EndMS: 100 * 60000, Final: true}}, 95 * time.Minute, true},
		{"stinger split", []markers.Marker{
			{Type: "credits", StartMS: 98 * 60000, EndMS: 100 * 60000, Final: true},
			{Type: "credits", StartMS: 90 * 60000, EndMS: 93 * 60000},
		}, 90 * time.Minute, true},
		{"intro ignored", []markers.Marker{{Type: "intro", StartMS: 60000, EndMS: 120000}}, 0, false},
	}
	for _, c := range cases {
		if got, ok := creditsTrigger(c.ms, dur); got != c.want || ok != c.ok {
			t.Errorf("%s: got %s,%v want %s,%v", c.name, got, ok, c.want, c.ok)
		}
	}
}

// Episodes: a mid-season episode leaves warming to next-episode prefetch;
// a season finale keeps the next season opener alongside the hook queue.
func TestPostPlayEpisodeVsFinale(t *testing.T) {
	f := newPPFixture(t, true, nil, nil)
	dur := 50 * time.Minute
	f.p.OnTimeline(user, ep1, dur-11*time.Minute, dur, "playing")
	f.wait(t)
	f.p.OnTimeline(user, ep1, dur-2*time.Minute, dur, "playing")
	f.wait(t)
	if calls := f.warmer.got(); len(calls) != 1 || calls[0].key != ep2 {
		t.Fatalf("mid-season prewarm = %+v, want only the next episode", calls)
	}
	// Season finale (next opens season 2).
	f.p.OnTimeline(user, epFinale, dur-11*time.Minute, dur, "playing")
	f.wait(t)
	f.p.OnTimeline(user, epFinale, dur-2*time.Minute, dur, "playing")
	f.wait(t)
	calls := f.warmer.got()
	got := map[core.RatingKey]bool{}
	for _, call := range calls {
		got[call.key] = true
	}
	if len(calls) != 3 || !got[ep2] || !got[epNextSzn] || !got[pickKey] {
		t.Fatalf("finale must keep the next season opener: %+v", calls)
	}
}

// Without a Prewarmer the pick is resolved and its opening bytes pulled.
func TestPostPlayFallbackPrefetch(t *testing.T) {
	f := newPPFixture(t, false, nil, nil)
	dur := 120 * time.Minute
	f.p.OnTimeline(user, movieKey, dur-11*time.Minute, dur, "playing")
	f.wait(t)
	f.p.OnTimeline(user, movieKey, dur-2*time.Minute, dur, "playing")
	f.wait(t)
	calls := f.engine.got()
	if len(calls) != 1 || calls[0].Length != 40<<20 || calls[0].Offset != 0 {
		t.Fatalf("engine prefetch = %+v, want one 40MiB opening pull", calls)
	}
	if f.resolver.count() != 1 {
		t.Errorf("resolver calls = %d, want 1", f.resolver.count())
	}
}

// Owner switch off: nothing probes or warms.
func TestPostPlayDisabled(t *testing.T) {
	f := newPPFixture(t, true, nil, func(_ *ppFixture, d *Deps) {
		d.PostPlayEnabled = func() bool { return false }
	})
	dur := 120 * time.Minute
	f.p.OnTimeline(user, movieKey, dur-2*time.Minute, dur, "playing")
	f.wait(t)
	if n := len(f.warmer.got()); n != 0 || f.player.calls != 0 {
		t.Errorf("disabled but warmed %d / ranked %d", n, f.player.calls)
	}
}

// A viewer who stops near the end and plays the title again (a resume
// after a stop, or a rewatch the same evening) starts a new post-play
// session: it is probed and its pick warmed again rather than suppressed
// for hours by the previous session's finished jobs.
func TestPostPlayNewSessionAfterStopWarmsAgain(t *testing.T) {
	f := newPPFixture(t, true, nil, nil)
	dur := 120 * time.Minute
	for session := 1; session <= 2; session++ {
		f.p.OnTimeline(user, movieKey, dur-11*time.Minute, dur, "playing") // probe
		f.wait(t)
		f.p.OnTimeline(user, movieKey, dur-2*time.Minute, dur, "playing") // fire
		f.wait(t)
		if n := len(f.warmer.got()); n != session {
			t.Fatalf("session %d: prewarm calls = %d, want %d", session, n, session)
		}
		f.p.OnTimeline(user, movieKey, dur-time.Minute, dur, "stopped")
	}
}

// Post-play sessions stay bounded even when none is old enough to prune.
func TestPostPlaySessionsBounded(t *testing.T) {
	f := newPPFixture(t, true, nil, nil)
	dur := 120 * time.Minute
	for i := 0; i < postplayMaxSessions+50; i++ {
		f.p.OnTimeline(user, core.MovieKey(int64(10_000+i)), dur-11*time.Minute, dur, "playing")
	}
	f.wait(t)
	f.p.ppMu.Lock()
	n := len(f.p.ppSessions)
	f.p.ppMu.Unlock()
	if n > postplayMaxSessions {
		t.Fatalf("%d post-play sessions tracked, cap %d", n, postplayMaxSessions)
	}
}
