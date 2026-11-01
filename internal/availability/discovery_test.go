package availability

import (
	"context"
	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/resolver"
	"testing"
	"time"
)

type suitabilityValidator struct {
	*fakeValidator
	r *resolver.Service
}

func (v suitabilityValidator) FeedCandidate(ctx context.Context, t *core.Title, r core.Release) (core.Release, string, error) {
	return v.r.FeedCandidate(ctx, t, r)
}

type cooldownValidator struct {
	suitabilityValidator
	missing map[core.RatingKey]time.Time
}

func (v cooldownValidator) KnownUnavailableSince(context.Context, []core.RatingKey) map[core.RatingKey]time.Time {
	return v.missing
}

func TestRecentEvidenceWindow(t *testing.T) {
	x := setup(t)
	r := resolver.New(nil, nil, nil, x.c, resolver.Options{})
	defer r.Close()
	x.s.d.Validator = suitabilityValidator{x.v, r}
	title := x.cat.titles[core.MovieKey(1)]
	title.Language = "en"
	put := func(id, name string) {
		t.Helper()
		if err := x.s.remember(t.Context(), x.f.sources[0], title, core.Release{ID: id, Name: name, Indexer: "Fake"}, x.s.policy()); err != nil {
			t.Fatal(err)
		}
	}
	visible := func(want int) {
		t.Helper()
		if got := x.s.FilterDiscovery(t.Context(), []core.Title{title}); len(got) != want {
			t.Fatalf("visible=%d want=%d", len(got), want)
		}
	}
	visible(0)
	put("cam", "Film.2020.1080p.CAM")
	put("dub", "Film.2020.1080p.WEB-DL.[Ukr.Dub]")
	put("pre", "Film.2020.1080p.HQ.Pre.Multi")
	visible(0)
	put("good", "Film.2020.1080p.WEB-DL.ENG")
	visible(1)
	// Policy/credential changes cannot reuse old evidence.
	x.f.sources[0].Generation = "two"
	visible(0)
	x.f.sources[0].Generation = "one"
	visible(1)
	x.s.Invalidate(t.Context(), "good")
	visible(0)
	title.Released = x.now.Add(-60 * 24 * time.Hour)
	visible(1)
	title.Released = x.now.Add(-60*24*time.Hour + time.Second)
	visible(0)
	// Future content remains hidden even when a named upload exists.
	title.Released = x.now.Add(time.Hour)
	put("future", "Film.2020.1080p.WEB-DL.ENG")
	visible(0)
	if x.f.calls != 0 || x.v.calls != 0 {
		t.Fatal("browsing spent provider quota")
	}
}

func TestEpisodeEvidenceExpiresAtOneDayNotSeries(t *testing.T) {
	x := setup(t)
	ep := core.Title{Key: core.EpisodeKey(7, 1, 2), Type: core.TypeEpisode, Released: x.now.Add(-23 * time.Hour)}
	show := core.Title{Key: core.ShowKey(7), Type: core.TypeShow, Released: x.now.AddDate(-20, 0, 0)}
	got := x.s.FilterDiscovery(t.Context(), []core.Title{show, ep})
	if len(got) != 1 || got[0].Key != show.Key {
		t.Fatal(got)
	}
	if err := x.s.remember(t.Context(), x.f.sources[0], ep, x.release("ep"), x.s.policy()); err != nil {
		t.Fatal(err)
	}
	if got := x.s.FilterDiscovery(t.Context(), []core.Title{ep}); len(got) != 1 {
		t.Fatal("upload must unlock immediately")
	}
	x.s.Invalidate(t.Context(), "ep")
	if got := x.s.FilterDiscovery(t.Context(), []core.Title{ep}); len(got) != 0 {
		t.Fatal("invalidated upload visible")
	}
	x.now = x.now.Add(time.Hour)
	if got := x.s.FilterDiscovery(t.Context(), []core.Title{ep}); len(got) != 1 {
		t.Fatal("24h assumption did not take effect")
	}
}

func TestNewReleaseOverridesEarlierUnavailableWithoutSearching(t *testing.T) {
	x := setup(t)
	r := resolver.New(nil, nil, nil, x.c, resolver.Options{})
	defer r.Close()
	title := x.cat.titles[core.MovieKey(1)]
	missing := map[core.RatingKey]time.Time{title.Key: x.now}
	x.s.d.Validator = cooldownValidator{suitabilityValidator{x.v, r}, missing}
	put := func(id string) {
		t.Helper()
		if err := x.s.remember(t.Context(), x.f.sources[0], title, core.Release{ID: id, Name: "Film.2020.1080p.WEB-DL", Indexer: "Fake"}, x.s.policy()); err != nil {
			t.Fatal(err)
		}
	}
	put("old")
	if got := x.s.FilterDiscovery(t.Context(), []core.Title{title}); len(got) != 0 {
		t.Fatal("old evidence defeated negative")
	}
	x.now = x.now.Add(time.Minute)
	put("new")
	if got := x.s.FilterDiscovery(t.Context(), []core.Title{title}); len(got) != 1 {
		t.Fatal("new release stuck behind old cooldown")
	}
	if x.f.calls != 0 || x.v.calls != 0 {
		t.Fatal("provider calls")
	}
}

func TestRetainedFeedUnlocksLaterCatalogEpisode(t *testing.T) {
	x := setup(t)
	r := resolver.New(nil, nil, nil, x.c, resolver.Options{})
	defer r.Close()
	x.s.d.Validator = suitabilityValidator{x.v, r}
	rel := core.Release{ID: "upload", Name: "Series.S01E02.1080p.WEB-DL", Indexer: "Fake"}
	if err := x.s.retainFeed(t.Context(), x.f.sources[0], []core.Release{rel}); err != nil {
		t.Fatal(err)
	}
	ep := core.Title{Key: core.EpisodeKey(7, 1, 2), Type: core.TypeEpisode, Title: "Second", ShowTitle: "Series", Season: 1, Episode: 2, Released: x.now.Add(-time.Hour)}
	next := ep
	next.Key = core.EpisodeKey(7, 1, 3)
	next.Episode = 3
	got := x.s.FilterDiscovery(t.Context(), []core.Title{ep, next})
	if len(got) != 1 || got[0].Key != ep.Key {
		t.Fatalf("exact episode match: %+v", got)
	}
	x.s.Invalidate(t.Context(), rel.ID)
	if got := x.s.FilterDiscovery(t.Context(), []core.Title{ep}); len(got) != 0 {
		t.Fatal("retained feed resurrected invalidated source")
	}
	if x.f.calls != 0 {
		t.Fatal("refetched feed")
	}
}

func TestTransientCheckDoesNotEraseReleaseExistence(t *testing.T) {
	x := setup(t)
	title := x.cat.titles[core.MovieKey(1)]
	if err := x.s.remember(t.Context(), x.f.sources[0], title, x.release("known"), x.s.policy()); err != nil {
		t.Fatal(err)
	}
	if _, err := x.s.db.Exec(`UPDATE availability_releases SET state='unknown',reason='timeout'`); err != nil {
		t.Fatal(err)
	}
	if got := x.s.FilterDiscovery(t.Context(), []core.Title{title}); len(got) != 1 {
		t.Fatal("timeout erased observed existence")
	}
	x.s.Invalidate(t.Context(), "known")
	if got := x.s.FilterDiscovery(t.Context(), []core.Title{title}); len(got) != 0 {
		t.Fatal("explicit invalidation ignored")
	}
}

func TestConfigurableMovieWindow(t *testing.T) {
	now := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
	movie := core.Title{Key: core.MovieKey(1), Type: core.TypeMovie, Released: now.Add(-90 * 24 * time.Hour)}
	for _, tc := range []struct {
		cfg  config.Availability
		want bool
	}{
		{config.Availability{}, false}, {config.Availability{MovieEvidenceDays: 120}, true},
		{config.Availability{MovieEvidenceDays: 90}, false}, {config.Availability{MovieEvidenceDays: 120, DisableMovieEvidenceGate: true}, false},
	} {
		if got := evidenceWindow(movie, now, tc.cfg); got != tc.want {
			t.Fatalf("%+v: got %v", tc.cfg, got)
		}
	}
	ep := core.Title{Key: core.EpisodeKey(1, 1, 1), Type: core.TypeEpisode, Released: now.Add(-23 * time.Hour)}
	if !evidenceWindow(ep, now, config.Availability{DisableMovieEvidenceGate: true, MovieEvidenceDays: 120}) {
		t.Fatal("episode policy changed")
	}
	x := setup(t)
	movie.Released = x.now.Add(-90 * 24 * time.Hour)
	x.c.Availability.MovieEvidenceDays = 120
	if len(x.s.FilterDiscovery(t.Context(), []core.Title{movie})) != 0 {
		t.Fatal("extended window not applied")
	}
	x.c.Availability.DisableMovieEvidenceGate = true
	if len(x.s.FilterDiscovery(t.Context(), []core.Title{movie})) != 1 {
		t.Fatal("disabled gate still applied")
	}
	x.c.Availability.DisableMovieEvidenceGate = false
	if len(x.s.FilterDiscovery(t.Context(), []core.Title{movie})) != 0 {
		t.Fatal("gate did not resume")
	}
}

func TestCurrentDiscoveryBypassesRemoteObservationMemo(t *testing.T) {
	x := setup(t)
	title := x.cat.titles[core.MovieKey(1)]
	if err := x.s.remember(t.Context(), x.f.sources[0], title, x.release("remote"), x.s.policy()); err != nil {
		t.Fatal(err)
	}
	if got := x.s.FilterDiscovery(t.Context(), []core.Title{title}); len(got) != 1 {
		t.Fatal("fixture not visible")
	}
	// Another node updates durable observations without clearing this node's memo.
	if _, err := x.s.db.Exec(`UPDATE availability_releases SET state='unavailable'`); err != nil {
		t.Fatal(err)
	}
	if got := x.s.FilterDiscovery(t.Context(), []core.Title{title}); len(got) != 1 {
		t.Fatal("fixture did not retain cached observation")
	}
	if got := x.s.FilterDiscoveryCurrent(t.Context(), []core.Title{title}); len(got) != 0 {
		t.Fatal("current preflight reused stale availability")
	}
}

func TestFilterDiscoveryReadsConfigOncePerBatch(t *testing.T) {
	x := setup(t)
	var calls int
	x.s.cfg = func() config.Config {
		calls++
		return x.c
	}
	titles := make([]core.Title, 8)
	for i := range titles {
		titles[i] = core.Title{Key: core.MovieKey(int64(100 + i)), Type: core.TypeMovie, Title: "Old", Released: x.now.Add(-400 * 24 * time.Hour)}
	}
	calls = 0
	if got := x.s.FilterDiscovery(t.Context(), titles); len(got) != len(titles) || calls != 1 {
		t.Fatalf("batch reads=%d visible=%d", calls, len(got))
	}
	var gen uint64 = 1
	x.s.d.ConfigGeneration = func() uint64 { return gen }
	calls = 0
	x.s.FilterDiscovery(t.Context(), titles)
	if calls != 1 {
		t.Fatalf("first generation read config %d times", calls)
	}
	calls = 0
	if got := x.s.FilterDiscovery(t.Context(), titles); len(got) != len(titles) || calls != 1 {
		t.Fatalf("fresh snapshot reads=%d visible=%d", calls, len(got))
	}
	before := x.s.policy()
	calls = 0
	gen++
	x.c.Quality.Default = "bench-changed"
	after := x.s.policy()
	if calls != 1 || before == after {
		t.Fatalf("generation bump reads=%d before=%s after=%s", calls, before, after)
	}
}
