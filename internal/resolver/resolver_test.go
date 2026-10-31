package resolver

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
)

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (f *fakeClock) now() time.Time      { f.mu.Lock(); defer f.mu.Unlock(); return f.t }
func (f *fakeClock) add(d time.Duration) { f.mu.Lock(); f.t = f.t.Add(d); f.mu.Unlock() }

type fakeIndexer struct {
	mu       sync.Mutex
	releases []core.Release
	searches atomic.Int64
	delay    time.Duration
}

func (f *fakeIndexer) Name() string { return "fake" }
func (f *fakeIndexer) Search(ctx context.Context, req core.SearchRequest) ([]core.Release, error) {
	f.searches.Add(1)
	if f.delay > 0 {
		select {
		case <-time.After(f.delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]core.Release(nil), f.releases...), nil
}
func (f *fakeIndexer) FetchNZB(ctx context.Context, r core.Release) ([]byte, error) {
	return []byte("nzb:" + r.ID), nil
}

type fakeHealth struct {
	unhealthy map[string]bool
	checks    *atomic.Int64
}

func (f fakeHealth) Check(ctx context.Context, nzb []byte) (bool, error) {
	if f.checks != nil {
		f.checks.Add(1)
	}
	for id := range f.unhealthy {
		if strings.Contains(string(nzb), id) {
			return false, nil
		}
	}
	return true, nil
}

type parallelHealth struct {
	entered chan struct{}
	release chan struct{}
}

func (f *parallelHealth) Check(ctx context.Context, _ []byte) (bool, error) {
	select {
	case f.entered <- struct{}{}:
	case <-ctx.Done():
		return false, ctx.Err()
	}
	select {
	case <-f.release:
		return true, nil
	case <-ctx.Done():
		return false, ctx.Err()
	}
}

func rel(id, name string, sizeMB int64) core.Release {
	return core.Release{ID: id, GUID: id, Indexer: "fake", Name: name, NZBURL: "x", Size: sizeMB << 20, PubDate: time.Now().Add(-30 * 24 * time.Hour), Grabs: 10}
}

func testCfg() config.Config {
	cfg := config.Defaults("/tmp")
	cfg.Resolver.SearchCacheTTL = config.Duration(30 * time.Minute)
	cfg.Resolver.UnavailableCooloff = config.Duration(time.Hour)
	cfg.Resolver.MaxCandidates = 2
	return cfg
}

func matrixTitle() *core.Title {
	return &core.Title{
		Key: core.MovieKey(603), Type: core.TypeMovie,
		IDs:   core.IDs{TMDB: 603, IMDB: "tt0133093"},
		Title: "The Signal", Year: 1999, Runtime: 136 * time.Minute,
		Released: time.Date(1999, 3, 31, 0, 0, 0, 0, time.UTC),
	}
}

func appleTVCaps() core.ClientCaps {
	return core.ClientCaps{Product: "Plex for Apple TV", MaxHeight: 2160, HDR10: true, DolbyVision: true,
		VideoCodecs: []string{"h264", "hevc", "av1"}, AudioCodecs: []string{"aac", "ac3", "eac3", "truehd"}}
}
func webCaps() core.ClientCaps {
	return core.ClientCaps{Product: "Plex Web", MaxHeight: 1080, VideoCodecs: []string{"h264"}, AudioCodecs: []string{"aac", "ac3"}}
}

func TestSelectProfile(t *testing.T) {
	q := config.DefaultQuality()
	p := SelectProfile(q, appleTVCaps(), nil)
	if p.Name != "4K" || !p.AllowHDR || !p.AllowDolbyVision || p.MaxHeight != 2160 {
		t.Fatalf("apple tv profile %+v", p)
	}
	p = SelectProfile(q, webCaps(), nil)
	if p.Name != "1080p" || p.MaxHeight != 1080 || p.AllowHDR || len(p.PreferredCodecs) != 1 || p.PreferredCodecs[0] != "h264" {
		t.Fatalf("web profile %+v", p)
	}
	remote := appleTVCaps()
	remote.Remote = true
	if p = SelectProfile(q, remote, nil); p.Name != "1080p" {
		t.Fatalf("remote profile %q want 1080p", p.Name)
	}
	// 4K profile clamped to a 1080p Apple TV (rule still matches).
	small := appleTVCaps()
	small.MaxHeight = 1080
	small.HDR10, small.DolbyVision = false, false
	if p = SelectProfile(q, small, nil); p.MaxHeight != 1080 || p.AllowDolbyVision || p.MinHeight > 1080 {
		t.Fatalf("clamped profile %+v", p)
	}
	// User rule.
	q.Rules = append([]config.QualityRule{{UserID: 7, Profile: "720p"}}, q.Rules...)
	if p = SelectProfile(q, webCaps(), &core.User{ID: 7}); p.Name != "720p" {
		t.Fatalf("user rule profile %q", p.Name)
	}
}

func movieReleases() []core.Release {
	return []core.Release{
		rel("uhd", "The.Signal.1999.2160p.UHD.BluRay.REMUX.DV.HEVC.TrueHD.7.1-GRP", 50000),
		rel("hd", "The.Signal.1999.1080p.WEB-DL.DDP5.1.H.264-GRP", 8000),
		rel("sd720", "The.Signal.1999.720p.BluRay.x264-GRP", 4000),
	}
}

func TestResolveRankingPerClient(t *testing.T) {
	idx := &fakeIndexer{releases: movieReleases()}
	svc := New(idx, fakeHealth{}, nil, testCfg(), Options{})
	ctx := context.Background()
	res, err := svc.Resolve(ctx, core.ResolveRequest{Title: matrixTitle(), Client: appleTVCaps()})
	if err != nil || res.Release.ID != "uhd" {
		t.Fatalf("atv res=%+v err=%v", res, err)
	}
	res, err = svc.Resolve(ctx, core.ResolveRequest{Title: matrixTitle(), Client: webCaps()})
	if err != nil || res.Release.ID != "hd" {
		t.Fatalf("web res=%+v err=%v", res, err)
	}
	if len(res.Alternates) == 0 || res.Alternates[0].ID != "sd720" {
		t.Fatalf("alternates %+v", res.Alternates)
	}
}

func TestResolveHealthFailoverAndTraces(t *testing.T) {
	idx := &fakeIndexer{releases: movieReleases()}
	svc := New(idx, fakeHealth{unhealthy: map[string]bool{"uhd": true}}, nil, testCfg(), Options{})
	res, err := svc.Resolve(context.Background(), core.ResolveRequest{Title: matrixTitle(), Client: appleTVCaps()})
	if err != nil || res.Release.ID != "hd" {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	traces := svc.Traces()
	if len(traces) != 1 || traces[0].Winner != "hd" {
		t.Fatalf("traces %+v", traces)
	}
	sawUnhealthy := false
	for _, c := range traces[0].Candidates {
		if c.ReleaseID == "uhd" && c.HealthChecked && !c.Healthy && c.Rejected {
			sawUnhealthy = true
		}
	}
	if !sawUnhealthy {
		t.Errorf("trace candidates %+v", traces[0].Candidates)
	}
}

type failingFetchIndexer struct{ *fakeIndexer }

func (f failingFetchIndexer) FetchNZB(context.Context, core.Release) ([]byte, error) {
	return nil, errors.New("NZB unavailable")
}

func TestResolveFetchFailureDoesNotReportHealthCheck(t *testing.T) {
	idx := failingFetchIndexer{&fakeIndexer{releases: movieReleases()[:1]}}
	var checks atomic.Int64
	svc := New(idx, fakeHealth{checks: &checks}, nil, testCfg(), Options{})
	t.Cleanup(func() { _ = svc.Close() })
	_, err := svc.Resolve(context.Background(), core.ResolveRequest{Title: matrixTitle(), Client: appleTVCaps()})
	if err == nil || errors.Is(err, core.ErrNotAvailable) {
		t.Fatalf("error = %v; want transient fetch failure", err)
	}
	if checks.Load() != 0 {
		t.Fatal("health checker ran after an NZB fetch failure")
	}
	traces := svc.Traces()
	if len(traces) != 1 || len(traces[0].Candidates) != 1 {
		t.Fatalf("traces %+v", traces)
	}
	c := traces[0].Candidates[0]
	if c.HealthChecked || c.Healthy || !c.Rejected || !strings.Contains(c.Reason, "NZB unavailable") {
		t.Fatalf("fetch failure trace %+v", c)
	}
}

func TestResolveValidatesLeadingCandidatesConcurrentlyInRankOrder(t *testing.T) {
	idx := &fakeIndexer{releases: movieReleases()}
	h := &parallelHealth{entered: make(chan struct{}, 2), release: make(chan struct{})}
	svc := New(idx, h, nil, testCfg(), Options{})
	ctx, cancel := context.WithCancel(context.Background())
	type result struct {
		release *core.Resolution
		err     error
	}
	done := make(chan result, 1)
	go func() {
		got, err := svc.Resolve(ctx, core.ResolveRequest{Title: matrixTitle(), Client: appleTVCaps()})
		done <- result{got, err}
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("Resolve did not stop after cancellation")
		}
		_ = svc.Close()
	})
	for i := 0; i < 2; i++ {
		select {
		case <-h.entered:
		case <-time.After(5 * time.Second):
			t.Fatal("two health checks did not enter concurrently")
		}
	}
	close(h.release)
	select {
	case got := <-done:
		// Restore the result for cleanup to observe completion.
		done <- got
		if got.err != nil {
			t.Fatal(got.err)
		}
		if got.release.Release.ID != "uhd" {
			t.Fatalf("winner %q; want highest-ranked uhd", got.release.Release.ID)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Resolve did not finish")
	}

}

func TestResolveCamGating(t *testing.T) {
	ctx := context.Background()
	cam := []core.Release{rel("cam1", "The.Signal.1999.CAM.x264-GRP", 1500)}
	title := matrixTitle()
	svc := New(&fakeIndexer{releases: cam}, fakeHealth{}, nil, testCfg(), Options{})
	_, err := svc.Resolve(ctx, core.ResolveRequest{Title: title, Client: webCaps()})
	if !errors.Is(err, core.ErrNotAvailable) {
		t.Fatalf("cam default err=%v", err)
	}
	// No profile allows cinema recordings, so the title is unavailable for everyone.
	if known, avail := svc.Available(ctx, title.Key); !known || avail {
		t.Fatalf("cam-only title known=%v avail=%v", known, avail)
	}

	// A profile that allows CAM plays it. There is no global switch.
	cfg := testCfg()
	for i := range cfg.Quality.Profiles {
		cfg.Quality.Profiles[i].AllowCam = true
		cfg.Quality.Profiles[i].MinSizeMBPerMin = 1
	}
	svc = New(&fakeIndexer{releases: cam}, fakeHealth{}, nil, cfg, Options{})
	res, err := svc.Resolve(ctx, core.ResolveRequest{Title: title, Client: webCaps()})
	if err != nil || res.Release.ID != "cam1" {
		t.Fatalf("cam allowed res=%+v err=%v", res, err)
	}

	// One other profile still allows CAM, so a refusing profile does not
	// hide the title from everyone.
	cfg = testCfg()
	cfg.Quality.Profiles[0].AllowCam = true
	for i := range cfg.Quality.Profiles {
		cfg.Quality.Profiles[i].MinSizeMBPerMin = 1
	}
	svc = New(&fakeIndexer{releases: cam}, fakeHealth{}, nil, cfg, Options{})
	_, err = svc.Resolve(ctx, core.ResolveRequest{Title: title, Client: webCaps()})
	if !errors.Is(err, core.ErrNotAvailable) {
		t.Fatalf("refusing profile err=%v", err)
	}
	if known, avail := svc.Available(ctx, title.Key); known && !avail {
		t.Fatalf("cam hidden for everyone while another profile allows it: known=%v avail=%v", known, avail)
	}
}

func TestUnavailableCooloff(t *testing.T) {
	ctx := context.Background()
	clock := &fakeClock{t: time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)}
	idx := &fakeIndexer{releases: []core.Release{rel("x", "Other.Movie.2020.1080p.BluRay.x264-GRP", 8000)}}
	svc := New(idx, fakeHealth{}, nil, testCfg(), Options{Now: clock.now})
	title := matrixTitle()
	_, err := svc.Resolve(ctx, core.ResolveRequest{Title: title, Client: webCaps()})
	if !errors.Is(err, core.ErrNotAvailable) {
		t.Fatal(err)
	}
	if known, avail := svc.Available(ctx, title.Key); !known || avail {
		t.Fatalf("available known=%v avail=%v", known, avail)
	}
	// Within cooloff: no new search.
	_, _ = svc.Resolve(ctx, core.ResolveRequest{Title: title, Client: webCaps()})
	if idx.searches.Load() != 1 {
		t.Fatalf("searches=%d want 1 within cooloff", idx.searches.Load())
	}
	clock.add(2 * time.Hour)
	if known, _ := svc.Available(ctx, title.Key); known {
		t.Fatal("availability should expire after cooloff")
	}
	_, _ = svc.Resolve(ctx, core.ResolveRequest{Title: title, Client: webCaps()})
	if idx.searches.Load() != 2 {
		t.Fatalf("searches=%d want 2 after cooloff", idx.searches.Load())
	}
}

func TestUnavailableCooloffRechecksWhenCachedOnlyOff(t *testing.T) {
	ctx := context.Background()
	// Empty results are search-cached only for freshReleaseCooloff (3m), so
	// a fresh Resolve after the 5m recheck window reaches the indexer again.
	run := func(cachedOnly bool) (*fakeIndexer, *fakeClock, *Service, *core.Title) {
		clock := &fakeClock{t: time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)}
		idx := &fakeIndexer{}
		cfg := testCfg()
		cfg.Debrid.CachedOnly = cachedOnly
		svc := New(idx, fakeHealth{}, nil, cfg, Options{Now: clock.now})
		return idx, clock, svc, matrixTitle()
	}

	t.Run("cached only off rechecks after window", func(t *testing.T) {
		idx, clock, svc, title := run(false)
		if _, err := svc.Resolve(ctx, core.ResolveRequest{Title: title, Client: webCaps()}); !errors.Is(err, core.ErrNotAvailable) {
			t.Fatalf("first resolve err=%v want ErrNotAvailable", err)
		}
		if idx.searches.Load() != 1 {
			t.Fatalf("searches=%d want 1 after first resolve", idx.searches.Load())
		}
		// Within the recheck window: the remembered answer still serves.
		if _, err := svc.Resolve(ctx, core.ResolveRequest{Title: title, Client: webCaps()}); !errors.Is(err, core.ErrNotAvailable) {
			t.Fatalf("second resolve err=%v want ErrNotAvailable", err)
		}
		if idx.searches.Load() != 1 {
			t.Fatalf("searches=%d want 1 within recheck window", idx.searches.Load())
		}
		// Past the window: one fresh search instead of the 24h-style cooloff.
		clock.add(6 * time.Minute)
		if _, err := svc.Resolve(ctx, core.ResolveRequest{Title: title, Client: webCaps()}); !errors.Is(err, core.ErrNotAvailable) {
			t.Fatalf("third resolve err=%v want ErrNotAvailable", err)
		}
		if idx.searches.Load() != 2 {
			t.Fatalf("searches=%d want 2 after recheck window with cached-only off", idx.searches.Load())
		}
	})

	t.Run("cached only on keeps long cooloff", func(t *testing.T) {
		idx, clock, svc, title := run(true)
		if _, err := svc.Resolve(ctx, core.ResolveRequest{Title: title, Client: webCaps()}); !errors.Is(err, core.ErrNotAvailable) {
			t.Fatalf("first resolve err=%v want ErrNotAvailable", err)
		}
		clock.add(6 * time.Minute)
		if _, err := svc.Resolve(ctx, core.ResolveRequest{Title: title, Client: webCaps()}); !errors.Is(err, core.ErrNotAvailable) {
			t.Fatalf("second resolve err=%v want ErrNotAvailable", err)
		}
		if idx.searches.Load() != 1 {
			t.Fatalf("searches=%d want 1 after recheck window with cached-only on", idx.searches.Load())
		}
	})
}

func TestUnavailableRecheckRefreshesNonemptyRejectedSearchCache(t *testing.T) {
	ctx := context.Background()
	clock := &fakeClock{t: time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)}
	// A non-empty result of only unrelated releases is ranked away, so the
	// title is remembered unavailable and the result is search-cached for
	// the full SearchCacheTTL (30m in testCfg) — unlike an empty result,
	// which expires after freshReleaseCooloff.
	idx := &fakeIndexer{releases: []core.Release{rel("x", "Other.Movie.2020.1080p.BluRay.x264-GRP", 8000)}}
	cfg := testCfg()
	cfg.Debrid.CachedOnly = false
	svc := New(idx, fakeHealth{}, nil, cfg, Options{Now: clock.now})
	title := matrixTitle()
	if _, err := svc.Resolve(ctx, core.ResolveRequest{Title: title, Client: webCaps()}); !errors.Is(err, core.ErrNotAvailable) {
		t.Fatalf("first resolve err=%v want ErrNotAvailable", err)
	}
	if idx.searches.Load() != 1 {
		t.Fatalf("searches=%d want 1 after first resolve", idx.searches.Load())
	}
	// Within the recheck window the remembered answer still serves, with
	// no new search even though the search cache is non-empty.
	clock.add(4 * time.Minute)
	if _, err := svc.Resolve(ctx, core.ResolveRequest{Title: title, Client: webCaps()}); !errors.Is(err, core.ErrNotAvailable) {
		t.Fatalf("within-window resolve err=%v want ErrNotAvailable", err)
	}
	if idx.searches.Load() != 1 {
		t.Fatalf("searches=%d want 1 within recheck window", idx.searches.Load())
	}
	// A matching release appears, then the clock moves past the 5m
	// recheck window (6m total) but well inside the 30m search-cache TTL
	// and the 1h unavailable cooloff.
	idx.mu.Lock()
	idx.releases = []core.Release{rel("good", "The.Signal.1999.1080p.WEB-DL.DDP5.1.H.264-GRP", 8000)}
	idx.mu.Unlock()
	clock.add(2 * time.Minute)
	res, err := svc.Resolve(ctx, core.ResolveRequest{Title: title, Client: webCaps()})
	if err != nil || res == nil || res.Release.ID != "good" {
		t.Fatalf("recheck resolve res=%+v err=%v want release good", res, err)
	}
	if idx.searches.Load() != 2 {
		t.Fatalf("searches=%d want 2 (fresh search on recheck)", idx.searches.Load())
	}
}

func TestUnavailableRecheckAfterHealthFailureWhenCachedOnlyOff(t *testing.T) {
	ctx := context.Background()
	clock := &fakeClock{t: time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)}
	// The only release fails its health check, so Resolve remembers the
	// title unavailable via the "no healthy candidates" path. Its result
	// stays in the 30m search cache, like the rejected-search-cache case.
	idx := &fakeIndexer{releases: []core.Release{rel("bad", "The.Signal.1999.1080p.WEB-DL.DDP5.1.H.264-GRP", 8000)}}
	cfg := testCfg()
	cfg.Debrid.CachedOnly = false
	// The health-check counter is the path discriminant: a ranked-away
	// title never reaches health checks, this one must (exactly the one
	// failing check on "bad").
	healthChecks := &atomic.Int64{}
	svc := New(idx, fakeHealth{unhealthy: map[string]bool{"bad": true}, checks: healthChecks}, nil, cfg, Options{Now: clock.now})
	title := matrixTitle()
	if _, err := svc.Resolve(ctx, core.ResolveRequest{Title: title, Client: webCaps()}); !errors.Is(err, core.ErrNotAvailable) {
		t.Fatalf("first resolve err=%v want ErrNotAvailable", err)
	}
	if idx.searches.Load() != 1 {
		t.Fatalf("searches=%d want 1 after first resolve", idx.searches.Load())
	}
	if healthChecks.Load() != 1 {
		t.Fatalf("health checks=%d want 1 (health-failed path, not ranked-away)", healthChecks.Load())
	}
	// Within the recheck window: remembered answer, no new search and no
	// new health check, even though the 2m failed-health TTL has expired.
	clock.add(4 * time.Minute)
	if _, err := svc.Resolve(ctx, core.ResolveRequest{Title: title, Client: webCaps()}); !errors.Is(err, core.ErrNotAvailable) {
		t.Fatalf("within-window resolve err=%v want ErrNotAvailable", err)
	}
	if idx.searches.Load() != 1 {
		t.Fatalf("searches=%d want 1 within recheck window", idx.searches.Load())
	}
	if healthChecks.Load() != 1 {
		t.Fatalf("health checks=%d want 1 within recheck window", healthChecks.Load())
	}
	// A healthy release appears; past the 5m window (6m total, beyond the
	// 2m failed-health availability TTL) Resolve must fresh-search rather
	// than serve the non-empty 30m search cache of the failed release.
	idx.mu.Lock()
	idx.releases = []core.Release{rel("good", "The.Signal.1999.1080p.WEB-DL.DDP5.1.H.264-GRP", 8000)}
	idx.mu.Unlock()
	clock.add(2 * time.Minute)
	// Maintainer's probe: a Prune just before the final Resolve must not
	// drop the unavailable marker while the 30m search cache of the
	// failed release can still suppress the fresh recheck.
	if _, err := svc.Prune(ctx); err != nil {
		t.Fatal(err)
	}
	res, err := svc.Resolve(ctx, core.ResolveRequest{Title: title, Client: webCaps()})
	if err != nil || res == nil || res.Release.ID != "good" {
		t.Fatalf("recheck resolve res=%+v err=%v want release good", res, err)
	}
	if idx.searches.Load() != 2 {
		t.Fatalf("searches=%d want 2 (fresh search on recheck)", idx.searches.Load())
	}
}

// Same probe as TestUnavailableRecheckAfterHealthFailureWhenCachedOnlyOff
// but over a persisted service: Prune's DB delete must also retain the
// marker (checked via a fresh service's peek over the same DB, the
// prune_test idiom) until the stale search cache can no longer suppress
// recovery.
func TestUnavailableRecheckAfterHealthFailurePrunePersisted(t *testing.T) {
	ctx := context.Background()
	db, err := store.OpenMemory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	clock := &fakeClock{t: time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)}
	idx := &fakeIndexer{releases: []core.Release{rel("bad", "The.Signal.1999.1080p.WEB-DL.DDP5.1.H.264-GRP", 8000)}}
	cfg := testCfg()
	cfg.Debrid.CachedOnly = false
	healthChecks := &atomic.Int64{}
	svc := New(idx, fakeHealth{unhealthy: map[string]bool{"bad": true}, checks: healthChecks}, db, cfg, Options{Now: clock.now})
	title := matrixTitle()
	if _, err := svc.Resolve(ctx, core.ResolveRequest{Title: title, Client: webCaps()}); !errors.Is(err, core.ErrNotAvailable) {
		t.Fatalf("first resolve err=%v want ErrNotAvailable", err)
	}
	if idx.searches.Load() != 1 || healthChecks.Load() != 1 {
		t.Fatalf("searches=%d health checks=%d want 1 and 1 (health-failed path)", idx.searches.Load(), healthChecks.Load())
	}
	idx.mu.Lock()
	idx.releases = []core.Release{rel("good", "The.Signal.1999.1080p.WEB-DL.DDP5.1.H.264-GRP", 8000)}
	idx.mu.Unlock()
	clock.add(6 * time.Minute)
	if _, err := svc.Prune(ctx); err != nil {
		t.Fatal(err)
	}
	freshSvc := New(idx, fakeHealth{}, db, cfg, Options{Now: clock.now})
	if _, ok := freshSvc.lastUnavailableChecked(ctx, int64(title.Key)); !ok {
		t.Fatal("peek after prune (DB) lost the unavailable marker at 6m")
	}
	res, err := svc.Resolve(ctx, core.ResolveRequest{Title: title, Client: webCaps()})
	if err != nil || res == nil || res.Release.ID != "good" {
		t.Fatalf("recheck resolve after prune res=%+v err=%v want release good", res, err)
	}
	if idx.searches.Load() != 2 {
		t.Fatalf("searches=%d want 2 (fresh search on recheck after prune)", idx.searches.Load())
	}
}

func TestUnavailableRecheckFreshRankedAwayWhenCachedOnlyOff(t *testing.T) {
	ctx := context.Background()
	clock := &fakeClock{t: time.Date(2026, 10, 21, 20, 30, 0, 0, time.UTC)}
	// Fresh title (released an hour ago, inside freshReleaseWindow): its
	// ranked-away write uses cooloffFor = freshReleaseCooloff (3m), so by
	// +6m the entry has expired but the last unavailable check is past
	// unavailableRecheck and must force a fresh search.
	idx := &fakeIndexer{releases: []core.Release{rel("x", "Other.Movie.2020.1080p.BluRay.x264-GRP", 8000)}}
	cfg := testCfg()
	cfg.Debrid.CachedOnly = false
	svc := New(idx, fakeHealth{}, nil, cfg, Options{Now: clock.now})
	title := &core.Title{
		Key: core.MovieKey(999), Type: core.TypeMovie,
		IDs:   core.IDs{TMDB: 999},
		Title: "The Signal", Year: 2026, Runtime: 136 * time.Minute,
		Released: clock.now().Add(-time.Hour),
	}
	if _, err := svc.Resolve(ctx, core.ResolveRequest{Title: title, Client: webCaps()}); !errors.Is(err, core.ErrNotAvailable) {
		t.Fatalf("first resolve err=%v want ErrNotAvailable", err)
	}
	if idx.searches.Load() != 1 {
		t.Fatalf("searches=%d want 1 after first resolve", idx.searches.Load())
	}
	idx.mu.Lock()
	idx.releases = []core.Release{rel("good", "The.Signal.2026.1080p.WEB-DL.DDP5.1.H.264-GRP", 8000)}
	idx.mu.Unlock()
	// Within the recheck window the remembered answer still serves, even
	// though the 3m fresh entry has already expired.
	clock.add(4 * time.Minute)
	if _, err := svc.Resolve(ctx, core.ResolveRequest{Title: title, Client: webCaps()}); !errors.Is(err, core.ErrNotAvailable) {
		t.Fatalf("within-window resolve err=%v want ErrNotAvailable", err)
	}
	if idx.searches.Load() != 1 {
		t.Fatalf("searches=%d want 1 within recheck window", idx.searches.Load())
	}
	clock.add(2 * time.Minute)
	res, err := svc.Resolve(ctx, core.ResolveRequest{Title: title, Client: webCaps()})
	if err != nil || res == nil || res.Release.ID != "good" {
		t.Fatalf("recheck resolve res=%+v err=%v want release good", res, err)
	}
	if idx.searches.Load() != 2 {
		t.Fatalf("searches=%d want 2 (fresh search on recheck)", idx.searches.Load())
	}
}

func TestHealthFailedAvailabilityExpiresAtShortTTL(t *testing.T) {
	ctx := context.Background()
	// Regression for the reverted TTL: Available must stop knowing the
	// health-failed title once failedHealthTTL (2m) passes, whatever the
	// cached-only mode — it must not stay hidden for the full cooloff.
	for _, cachedOnly := range []bool{false, true} {
		clock := &fakeClock{t: time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)}
		idx := &fakeIndexer{releases: []core.Release{rel("bad", "The.Signal.1999.1080p.WEB-DL.DDP5.1.H.264-GRP", 8000)}}
		cfg := testCfg()
		cfg.Debrid.CachedOnly = cachedOnly
		svc := New(idx, fakeHealth{unhealthy: map[string]bool{"bad": true}}, nil, cfg, Options{Now: clock.now})
		title := matrixTitle()
		if _, err := svc.Resolve(ctx, core.ResolveRequest{Title: title, Client: webCaps()}); !errors.Is(err, core.ErrNotAvailable) {
			t.Fatalf("cachedOnly=%v first resolve err=%v want ErrNotAvailable", cachedOnly, err)
		}
		if known, avail := svc.Available(ctx, title.Key); !known || avail {
			t.Fatalf("cachedOnly=%v Available known=%v avail=%v want known unavailable", cachedOnly, known, avail)
		}
		clock.add(3 * time.Minute)
		if known, _ := svc.Available(ctx, title.Key); known {
			t.Fatalf("cachedOnly=%v availability still known after 3m, want expired at failedHealthTTL", cachedOnly)
		}
	}
}

func TestLastUnavailableCheckedPeeksExpiredEntry(t *testing.T) {
	ctx := context.Background()
	clock := &fakeClock{t: time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)}
	t0 := clock.now()
	svc := New(&fakeIndexer{}, fakeHealth{}, nil, testCfg(), Options{Now: clock.now})
	if _, ok := svc.lastUnavailableChecked(ctx, 99); ok {
		t.Fatal("peek without entry returned ok")
	}
	svc.setAvailability(ctx, 42, false, time.Minute)
	svc.setAvailability(ctx, 43, true, time.Minute)
	clock.add(2 * time.Minute)
	if _, ok := svc.getAvailability(ctx, 42); ok {
		t.Fatal("entry should be expired for getAvailability")
	}
	checked, ok := svc.lastUnavailableChecked(ctx, 42)
	if !ok || !checked.Equal(t0) {
		t.Fatalf("peek expired entry checked=%v ok=%v want %v,true", checked, ok, t0)
	}
	if _, ok := svc.lastUnavailableChecked(ctx, 43); ok {
		t.Fatal("peek of available=true entry returned ok")
	}

	// DB path: a fresh service over the same DB has an empty memory map,
	// so the peek must come from resolver_availability without the
	// expires_at filter.
	db, err := store.OpenMemory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	dbSvc := New(&fakeIndexer{}, fakeHealth{}, db, testCfg(), Options{Now: clock.now})
	dbChecked := clock.now()
	dbSvc.setAvailability(ctx, 44, false, time.Minute)
	clock.add(2 * time.Minute)
	freshSvc := New(&fakeIndexer{}, fakeHealth{}, db, testCfg(), Options{Now: clock.now})
	checked, ok = freshSvc.lastUnavailableChecked(ctx, 44)
	if !ok || !checked.Equal(dbChecked) {
		t.Fatalf("DB peek expired entry checked=%v ok=%v want %v,true", checked, ok, dbChecked)
	}
	// Clearing the entry (Forget/forgetAvailability) clears the peek.
	if err := freshSvc.forgetAvailability(ctx, 44); err != nil {
		t.Fatal(err)
	}
	if _, ok := freshSvc.lastUnavailableChecked(ctx, 44); ok {
		t.Fatal("peek still ok after forgetAvailability")
	}
}

func TestFreshEpisodeShortCooloffAndReResolveWhenUploaded(t *testing.T) {
	ctx := context.Background()
	airDay := time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC)
	clock := &fakeClock{t: time.Date(2026, 10, 21, 20, 30, 0, 0, time.UTC)} // 8:30 PM on air day
	idx := &fakeIndexer{}
	svc := New(idx, fakeHealth{}, nil, testCfg(), Options{Now: clock.now})
	ep := &core.Title{
		Key: core.EpisodeKey(500, 28, 25), Type: core.TypeEpisode,
		ShowTitle: "Big Brother", Title: "Episode 25", Season: 28, Episode: 25,
		Released: airDay, Runtime: 45 * time.Minute,
	}
	// Checked at 8:30 PM before the release drops: not available yet.
	if _, err := svc.Resolve(ctx, core.ResolveRequest{Title: ep, Client: webCaps()}); !errors.Is(err, core.ErrNotAvailable) {
		t.Fatalf("expected ErrNotAvailable at 8:30 PM, got %v", err)
	}
	if known, avail := svc.Available(ctx, ep.Key); !known || avail {
		t.Fatalf("expected known unavailable immediately after 8:30 PM check, got %v/%v", known, avail)
	}
	// Release lands at 10:00 PM (90 minutes later — well inside the 6h/24h old cooloff,
	// past the 3-minute fresh-release cooloff).
	clock.add(90 * time.Minute)
	idx.releases = []core.Release{rel("bb25", "Big.Brother.S28E25.1080p.WEB-DL.H264-GRP", 1800)}
	res, err := svc.Resolve(ctx, core.ResolveRequest{Title: ep, Client: webCaps()})
	if err != nil || res == nil || res.Release.ID != "bb25" {
		t.Fatalf("expected fresh episode to resolve at 10:00 PM without waiting 24h, got res=%+v err=%v", res, err)
	}
}

func TestMarkBadPersists(t *testing.T) {
	ctx := context.Background()
	db, err := store.OpenMemory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	idx := &fakeIndexer{releases: movieReleases()}
	svc := New(idx, fakeHealth{}, db, testCfg(), Options{})
	if err := svc.MarkBad(ctx, "uhd", "broken"); err != nil {
		t.Fatal(err)
	}
	res, err := svc.Resolve(ctx, core.ResolveRequest{Title: matrixTitle(), Client: appleTVCaps()})
	if err != nil || res.Release.ID != "hd" {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	// New service over the same DB still knows.
	svc2 := New(idx, fakeHealth{}, db, testCfg(), Options{})
	res, err = svc2.Resolve(ctx, core.ResolveRequest{Title: matrixTitle(), Client: appleTVCaps()})
	if err != nil || res.Release.ID != "hd" {
		t.Fatalf("persisted res=%+v err=%v", res, err)
	}
}

func TestEpisodeFiltering(t *testing.T) {
	idx := &fakeIndexer{releases: []core.Release{
		rel("pack", "Boundary.Bay.S01.COMPLETE.720p.BluRay.x264-GRP", 10000),
		rel("wrongep", "Boundary.Bay.S01E02.720p.HDTV.x264-GRP", 1500),
		rel("sample", "Boundary.Bay.S01E01.Sample.720p.HDTV.x264-GRP", 1500),
		rel("pwd", "Boundary.Bay.S01E01.1080p.WEB-DL.x264-GRP", 1500),
		rel("good", "Boundary.Bay.S01E01.720p.HDTV.x264-CTU", 1500),
	}}
	idx.releases[3].Password = true
	idx.releases[3].Protocol = core.ProtocolTorrent
	svc := New(idx, fakeHealth{}, nil, testCfg(), Options{})
	title := &core.Title{
		Key: core.EpisodeKey(1396, 1, 1), Type: core.TypeEpisode,
		ShowTitle: "Boundary Bay", Season: 1, Episode: 1, Runtime: 49 * time.Minute,
		ShowIDs: core.IDs{TVDB: 81189},
	}
	res, err := svc.Resolve(context.Background(), core.ResolveRequest{Title: title, Client: webCaps()})
	if err != nil || res.Release.ID != "good" {
		t.Fatalf("res=%+v err=%v traces=%+v", res, err, svc.Traces())
	}
	reasons := map[string]string{}
	for _, c := range svc.Traces()[0].Candidates {
		reasons[c.ReleaseID] = c.Reason
	}
	if !strings.Contains(reasons["pack"], "season pack") || !strings.Contains(reasons["sample"], "sample") ||
		!strings.Contains(reasons["pwd"], "passworded") || reasons["wrongep"] == "" {
		t.Errorf("reasons %v", reasons)
	}
}

func TestSizeSanityAndBlockedGroup(t *testing.T) {
	idx := &fakeIndexer{releases: []core.Release{
		rel("tiny", "The.Signal.1999.1080p.BluRay.x264-GRP", 100), // < 5 MB/min
		rel("blocked", "The.Signal.1999.1080p.BluRay.x264-BADGRP", 8000),
		rel("good", "The.Signal.1999.1080p.BluRay.x264-GOOD", 8000),
	}}
	cfg := testCfg()
	cfg.Resolver.BlockedGroups = []string{"BADGRP"}
	svc := New(idx, fakeHealth{}, nil, cfg, Options{})
	res, err := svc.Resolve(context.Background(), core.ResolveRequest{Title: matrixTitle(), Client: webCaps()})
	if err != nil || res.Release.ID != "good" {
		t.Fatalf("res=%+v err=%v traces=%+v", res, err, svc.Traces())
	}
}

func TestExcludeAndUnreleased(t *testing.T) {
	ctx := context.Background()
	idx := &fakeIndexer{releases: movieReleases()}
	svc := New(idx, fakeHealth{}, nil, testCfg(), Options{})
	res, err := svc.Resolve(ctx, core.ResolveRequest{Title: matrixTitle(), Client: webCaps(), Exclude: []string{"uhd", "hd"}})
	if err != nil || res.Release.ID != "sd720" {
		t.Fatalf("exclude res=%+v err=%v", res, err)
	}
	future := matrixTitle()
	future.Key = core.MovieKey(999)
	future.Title = "Future Movie"
	future.Released = time.Now().AddDate(1, 0, 0)
	svc2 := New(&fakeIndexer{}, fakeHealth{}, nil, testCfg(), Options{})
	if _, err := svc2.Resolve(ctx, core.ResolveRequest{Title: future, Client: webCaps()}); !errors.Is(err, core.ErrNotAvailable) {
		t.Fatalf("unreleased err=%v", err)
	}
}

func TestConcurrentResolveSingleflight(t *testing.T) {
	idx := &fakeIndexer{releases: movieReleases(), delay: 50 * time.Millisecond}
	svc := New(idx, fakeHealth{}, nil, testCfg(), Options{})
	var wg sync.WaitGroup
	errs := make([]error, 8)
	winners := make([]string, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			res, err := svc.Resolve(context.Background(), core.ResolveRequest{Title: matrixTitle(), Client: webCaps()})
			errs[i] = err
			if res != nil {
				winners[i] = res.Release.ID
			}
		}(i)
	}
	wg.Wait()
	for i := range errs {
		if errs[i] != nil || winners[i] != "hd" {
			t.Fatalf("goroutine %d winner=%q err=%v", i, winners[i], errs[i])
		}
	}
	if idx.searches.Load() != 1 {
		t.Errorf("searches=%d want 1 (singleflight)", idx.searches.Load())
	}
}

type countingHealth struct{ n atomic.Int64 }

func (c *countingHealth) Check(context.Context, []byte) (bool, error) {
	c.n.Add(1)
	return true, nil
}

func TestHealthCheckCachedUntilTTLOrMarkBad(t *testing.T) {
	ctx := context.Background()
	db, err := store.OpenMemory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	clock := &fakeClock{t: time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)}
	h := &countingHealth{}
	idx := &fakeIndexer{releases: movieReleases()}
	svc := New(idx, h, db, testCfg(), Options{Now: clock.now, HealthTTL: time.Hour})
	req := core.ResolveRequest{Title: matrixTitle(), Client: appleTVCaps()}
	first, err := svc.Resolve(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	checked := h.n.Load()

	// Same release again within the TTL, even from a fresh service sharing
	// the database: no new health checks for it.
	svc2 := New(idx, h, db, testCfg(), Options{Now: clock.now, HealthTTL: time.Hour})
	if _, err := svc2.checkHealth(ctx, first.Release.ID, nil); err != nil {
		t.Fatal(err)
	}
	if h.n.Load() != checked {
		t.Fatalf("cached release was re-checked")
	}
	clock.add(2 * time.Hour)
	if _, err := svc2.checkHealth(ctx, first.Release.ID, nil); err != nil || h.n.Load() != checked+1 {
		t.Fatalf("expired entry not re-checked (checks %d)", h.n.Load())
	}
	if err := svc2.MarkBad(ctx, first.Release.ID, "dead"); err != nil {
		t.Fatal(err)
	}
	if svc2.recentlyVerified(ctx, first.Release.ID) {
		t.Fatal("MarkBad must drop the verified entry")
	}
}
