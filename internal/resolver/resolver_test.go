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
	active atomic.Int64
	peak   atomic.Int64
	delay  time.Duration
}

func (f *parallelHealth) Check(ctx context.Context, _ []byte) (bool, error) {
	n := f.active.Add(1)
	for p := f.peak.Load(); n > p && !f.peak.CompareAndSwap(p, n); p = f.peak.Load() {
	}
	defer f.active.Add(-1)
	select {
	case <-time.After(f.delay):
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
	h := &parallelHealth{delay: 40 * time.Millisecond}
	svc := New(idx, h, nil, testCfg(), Options{})
	started := time.Now()
	got, err := svc.Resolve(context.Background(), core.ResolveRequest{Title: matrixTitle(), Client: appleTVCaps()})
	if err != nil {
		t.Fatal(err)
	}
	if got.Release.ID != "uhd" {
		t.Fatalf("winner %q; want highest-ranked uhd", got.Release.ID)
	}
	if h.peak.Load() < 2 {
		t.Fatalf("health checks were not concurrent (peak=%d)", h.peak.Load())
	}
	if time.Since(started) > 100*time.Millisecond {
		t.Fatalf("validation took %s; candidates likely serialized", time.Since(started))
	}
}

func TestResolveCamGating(t *testing.T) {
	ctx := context.Background()
	cam := []core.Release{rel("cam1", "The.Signal.1999.CAM.x264-GRP", 1500)}
	svc := New(&fakeIndexer{releases: cam}, fakeHealth{}, nil, testCfg(), Options{})
	_, err := svc.Resolve(ctx, core.ResolveRequest{Title: matrixTitle(), Client: webCaps()})
	if !errors.Is(err, core.ErrNotAvailable) {
		t.Fatalf("cam default err=%v", err)
	}
	// Allow cams globally and in the profile.
	cfg := testCfg()
	cfg.Resolver.AllowCam = true
	for i := range cfg.Quality.Profiles {
		cfg.Quality.Profiles[i].AllowCam = true
		cfg.Quality.Profiles[i].MinSizeMBPerMin = 1
	}
	svc = New(&fakeIndexer{releases: cam}, fakeHealth{}, nil, cfg, Options{})
	res, err := svc.Resolve(ctx, core.ResolveRequest{Title: matrixTitle(), Client: webCaps()})
	if err != nil || res.Release.ID != "cam1" {
		t.Fatalf("cam allowed res=%+v err=%v", res, err)
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
