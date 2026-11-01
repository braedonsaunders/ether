package availability

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
)

type fakeFeeds struct {
	sources []core.ReleaseFeed
	pages   [][]core.Release
	calls   int
	fail    error
}

func (f *fakeFeeds) FeedSources() []core.ReleaseFeed { return f.sources }
func (f *fakeFeeds) Recent(ctx context.Context, id string, offset, limit int) ([]core.Release, error) {
	if err := core.SpendBackground(ctx, "hit", id, 1); err != nil {
		return nil, err
	}
	f.calls++
	if f.fail != nil {
		return nil, f.fail
	}
	if offset/100 >= len(f.pages) {
		return nil, nil
	}
	return f.pages[offset/100], nil
}

type fakeCatalog struct{ titles map[core.RatingKey]core.Title }

func (c *fakeCatalog) ReleaseMatches(_ context.Context, rs []core.Release) (map[string][]core.Title, error) {
	out := map[string][]core.Title{}
	for _, r := range rs {
		for _, t := range c.titles {
			if t.Title == r.Name {
				out[r.ID] = append(out[r.ID], t)
			}
		}
	}
	return out, nil
}
func (c *fakeCatalog) CachedTitle(_ context.Context, k core.RatingKey) (*core.Title, error) {
	t, ok := c.titles[k]
	if !ok {
		return nil, core.ErrNotFound
	}
	return &t, nil
}
func (c *fakeCatalog) CachedTitles(ctx context.Context, ks []core.RatingKey) ([]core.Title, error) {
	var out []core.Title
	for _, k := range ks {
		if t, e := c.CachedTitle(ctx, k); e == nil {
			out = append(out, *t)
		}
	}
	return out, nil
}

type fakeValidator struct {
	calls   int
	failure error
	reject  bool
	after   func()
}

func (v *fakeValidator) FeedCandidate(_ context.Context, _ *core.Title, r core.Release) (core.Release, string, error) {
	if v.reject {
		return r, "quality", nil
	}
	return r, "", nil
}
func (v *fakeValidator) VerifyFeedCandidate(ctx context.Context, _ *core.Title, _ core.Release) error {
	if e := core.SpendBackground(ctx, "grab", "f", 1); e != nil {
		return e
	}
	v.calls++
	if v.after != nil {
		v.after()
	}
	return v.failure
}

type fixture struct {
	s      *Service
	c      config.Config
	f      *fakeFeeds
	cat    *fakeCatalog
	v      *fakeValidator
	now    time.Time
	active bool
}

func setup(t *testing.T) *fixture {
	t.Helper()
	x := &fixture{c: config.Defaults(t.TempDir()), now: time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)}
	x.c.Availability.Monitor = true
	x.c.Availability.Verify = true
	x.c.Availability.CatchUp = true
	x.c.Usenet.Providers = []config.UsenetProvider{{Enabled: true, Host: "fake"}}
	x.f = &fakeFeeds{sources: []core.ReleaseFeed{{ID: "f", Name: "Fake", Generation: "one", Supported: true}}}
	title := core.Title{Key: core.MovieKey(1), Type: core.TypeMovie, Title: "Film", Year: 2020, Released: x.now.Add(-time.Hour)}
	x.cat = &fakeCatalog{titles: map[core.RatingKey]core.Title{title.Key: title}}
	x.v = &fakeValidator{}
	var err error
	x.s, err = New(Deps{DB: store.OpenTest(t), Config: func() config.Config { return x.c }, Feeds: x.f, Catalog: x.cat, Validator: x.v, Now: func() time.Time { return x.now }, Active: func() bool { return x.active }})
	if err != nil {
		t.Fatal(err)
	}
	return x
}
func (x *fixture) release(id string) core.Release {
	return core.Release{ID: id, Name: "Film", Indexer: "Fake", NZBURL: "https://secret.invalid/?apikey=secret"}
}
func TestFeedLifecycleAndNoEvidenceGate(t *testing.T) {
	x := setup(t)
	ctx := t.Context()
	x.f.pages = [][]core.Release{{x.release("r")}}
	if e := x.s.Cycle(ctx); e != nil {
		t.Fatal(e)
	}
	if x.f.calls != 1 || x.v.calls != 1 {
		t.Fatalf("calls feeds=%d checks=%d", x.f.calls, x.v.calls)
	}
	st, e := x.s.Status(ctx)
	if e != nil {
		t.Fatal(e)
	}
	if st.Counts["verified"] != 1 || st.Usage["feedRequests"] != 1 {
		t.Fatalf("%+v", st)
	}
	if len(x.s.Hubs(ctx, 40)) != 1 {
		t.Fatal("missing checked shelf")
	}
	items := []core.Title{{Key: core.MovieKey(99)}, x.cat.titles[core.MovieKey(1)]}
	got := x.s.Prioritize(ctx, items)
	if len(got) != 2 || got[0].Key != core.MovieKey(1) || items[0].Key != core.MovieKey(99) {
		t.Fatal("ranking removed/mutated titles")
	}
	if e = x.s.Cycle(ctx); e != nil {
		t.Fatal(e)
	}
	if x.f.calls != 1 || x.v.calls != 1 {
		t.Fatal("duplicate work before deadline")
	}
	x.now = x.now.Add(7 * time.Hour)
	if len(x.s.Hubs(ctx, 40)) != 0 {
		t.Fatal("stale evidence advertised")
	}
	got = x.s.Prioritize(ctx, items)
	if got[0].Key != items[0].Key {
		t.Fatal("stale evidence promoted")
	}
}
func TestConcurrentPageSpendStaysInsideTheAllowance(t *testing.T) {
	x := setup(t)
	var wg sync.WaitGroup
	var success atomic.Int32
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := x.s.spend(t.Context(), "feedRequests", 3, 10)
			if err == nil {
				success.Add(1)
			} else if !errors.Is(err, core.ErrBudgetExhausted) {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	st, err := x.s.Status(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	used := st.Usage["feedRequests"]
	if used != int64(success.Load())*3 || used > 10 || used%3 != 0 {
		t.Fatalf("overspent: admitted %d stored %d", success.Load(), used)
	}
}

func TestAtomicBudgetAndUTCReset(t *testing.T) {
	x := setup(t)
	var wg sync.WaitGroup
	var success atomic.Int32
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			e := x.s.spend(t.Context(), "test", 1, 7)
			if e == nil {
				success.Add(1)
			} else if !errors.Is(e, core.ErrBudgetExhausted) {
				t.Error(e)
			}
		}()
	}
	wg.Wait()
	if success.Load() != 7 {
		t.Fatalf("overspent: %d", success.Load())
	}
	if !errors.Is(x.s.spend(t.Context(), "zero", 1, 0), core.ErrBudgetExhausted) {
		t.Fatal("zero did not disable")
	}
	x.now = x.now.Add(24 * time.Hour)
	if e := x.s.spend(t.Context(), "test", 7, 7); e != nil {
		t.Fatal(e)
	}
}
func TestGapFailureAndRestart(t *testing.T) {
	x := setup(t)
	ctx := t.Context()
	x.c.Availability.Verify = false
	x.f.pages = [][]core.Release{{x.release("old")}}
	if e := x.s.Cycle(ctx); e != nil {
		t.Fatal(e)
	}
	x.now = x.now.Add(time.Hour)
	x.f.fail = errors.New("transport secret")
	_ = x.s.Cycle(ctx)
	anchor, _, _, _ := x.s.feedState(ctx, x.f.sources[0])
	if anchor != "old" {
		t.Fatal("failed request advanced cursor")
	}
	x.f.fail = nil
	x.c.Availability.MaxPages = 1
	x.f.pages = [][]core.Release{make([]core.Release, 100)}
	for i := range x.f.pages[0] {
		x.f.pages[0][i] = x.release(fmt.Sprintf("new%d", i))
	}
	x.now = x.now.Add(time.Hour)
	if e := x.s.Cycle(ctx); e != nil {
		t.Fatal(e)
	}
	anchor, _, gap, _ := x.s.feedState(ctx, x.f.sources[0])
	if !gap || anchor != "new0" {
		t.Fatal("gap was not recorded")
	}
	restarted, e := New(x.s.d)
	if e != nil {
		t.Fatal(e)
	}
	before := x.f.calls
	if e = restarted.Cycle(ctx); e != nil {
		t.Fatal(e)
	}
	if before != x.f.calls {
		t.Fatal("restart lost durable cursor")
	}
}
func TestNoProvidersOffAndActiveAreCheap(t *testing.T) {
	for _, mode := range []string{"off", "no-provider", "active", "zero"} {
		t.Run(mode, func(t *testing.T) {
			x := setup(t)
			x.f.pages = [][]core.Release{{x.release("r")}}
			switch mode {
			case "off":
				x.c.Availability.Monitor = false
				x.c.Availability.Verify = false
			case "no-provider":
				x.c.Usenet.Providers = nil
			case "active":
				x.active = true
			case "zero":
				x.c.Availability.Checks = 0
			}
			if e := x.s.Cycle(t.Context()); e != nil {
				t.Fatal(e)
			}
			if x.v.calls != 0 {
				t.Fatal("performed forbidden background check")
			}
			if mode == "off" && x.f.calls != 0 {
				t.Fatal("off spent requests")
			}
		})
	}
}
func TestPolicyAndSourceChangesInvalidateEvidence(t *testing.T) {
	x := setup(t)
	ctx := t.Context()
	r := x.release("r")
	x.f.pages = [][]core.Release{{r}}
	if e := x.s.Cycle(ctx); e != nil {
		t.Fatal(e)
	}
	x.v.reject = true
	x.c.Quality.Default = "changed"
	if len(x.s.Hubs(ctx, 40)) != 0 {
		t.Fatal("old policy still promoted")
	}
	if e := x.s.Cycle(ctx); e != nil {
		t.Fatal(e)
	}
	if x.v.calls != 1 {
		t.Fatal("policy rejection spent a check")
	}
	x.f.sources[0].Generation = "two"
	if len(x.s.Candidates(ctx, &core.Title{Key: core.MovieKey(1)})) != 0 {
		t.Fatal("old source credential reused")
	}
	if e := x.s.remember(ctx, x.f.sources[0], x.cat.titles[core.MovieKey(1)], r, x.s.policy()); e != nil {
		t.Fatal(e)
	}
	var at int64
	if e := x.s.db.QueryRow(`SELECT verified_at FROM availability_releases`).Scan(&at); e != nil || at != 0 {
		t.Fatal("reconfiguration retained evidence", at, e)
	}
}
func TestFailureIsSourceScopedAndSyncIsBounded(t *testing.T) {
	x := setup(t)
	x.v.failure = core.ErrArticleMissing
	x.f.pages = [][]core.Release{{x.release("bad")}}
	if e := x.s.Cycle(t.Context()); e != nil {
		t.Fatal(e)
	}
	st, e := x.s.Status(t.Context())
	if e != nil {
		t.Fatal(e)
	}
	if st.Counts["unavailable"] != 1 {
		t.Fatal(st)
	}
	if got := x.s.Prioritize(t.Context(), []core.Title{x.cat.titles[core.MovieKey(1)]}); len(got) != 0 {
		t.Fatal("failed recent release still published")
	}
	req := httptest.NewRequest(http.MethodPost, "/sync", nil)
	w := httptest.NewRecorder()
	x.s.Handler().ServeHTTP(w, req)
	if w.Code != 202 {
		t.Fatal(w.Code)
	}
	if x.f.calls != 1 {
		t.Fatal("HTTP handler made network requests")
	}
	x.c.Availability.Monitor = false
	w = httptest.NewRecorder()
	x.s.Handler().ServeHTTP(w, req)
	if w.Code != 409 {
		t.Fatal(w.Code)
	}
}
func TestDisabledCandidatesCannotStarveQueue(t *testing.T) {
	x := setup(t)
	ctx := t.Context()
	x.c.Availability.Monitor = false
	for i := 0; i < 30; i++ {
		f := x.f.sources[0]
		f.ID = "removed"
		if e := x.s.remember(ctx, f, x.cat.titles[core.MovieKey(1)], x.release(fmt.Sprint(i)), "old"); e != nil {
			t.Fatal(e)
		}
	}
	if e := x.s.remember(ctx, x.f.sources[0], x.cat.titles[core.MovieKey(1)], x.release("valid"), x.s.policy()); e != nil {
		t.Fatal(e)
	}
	if e := x.s.Cycle(ctx); e != nil {
		t.Fatal(e)
	}
	if x.v.calls != 1 {
		t.Fatal("valid candidate starved")
	}
}

func TestCheckBudgetCoversDistinctTitlesBeforeAlternateReleases(t *testing.T) {
	x := setup(t)
	ctx := t.Context()
	x.c.Availability.Monitor = false
	x.c.Availability.Checks = 2
	for i := 0; i < 20; i++ {
		if e := x.s.remember(ctx, x.f.sources[0], x.cat.titles[core.MovieKey(1)], x.release(fmt.Sprint(i)), x.s.policy()); e != nil {
			t.Fatal(e)
		}
	}
	second := x.cat.titles[core.MovieKey(1)]
	second.Key = core.MovieKey(2)
	second.Title = "Second"
	x.cat.titles[second.Key] = second
	if e := x.s.remember(ctx, x.f.sources[0], second, x.release("second"), x.s.policy()); e != nil {
		t.Fatal(e)
	}
	if e := x.s.Cycle(ctx); e != nil {
		t.Fatal(e)
	}
	if len(x.s.verifiedKeys(ctx)) != 2 {
		t.Fatal("alternate releases monopolized the allowance")
	}
}

func TestDebridBatchSkipsDefinitiveMissWithoutSpendingChecks(t *testing.T) {
	x := setup(t)
	ctx := t.Context()
	x.c.Availability.Monitor = false
	x.c.Usenet.Providers = nil
	x.c.Debrid.Providers = []config.DebridProvider{{Enabled: true, APIKey: "fake"}}
	x.f.sources[0].Protocol = core.ProtocolTorrent
	r := x.release("torrent")
	r.Protocol = core.ProtocolTorrent
	r.InfoHash = "hash"
	if e := x.s.remember(ctx, x.f.sources[0], x.cat.titles[core.MovieKey(1)], r, x.s.policy()); e != nil {
		t.Fatal(e)
	}
	calls := 0
	x.s.d.CacheCheck = func(context.Context, []string) (map[string]bool, bool) { calls++; return map[string]bool{}, true }
	if e := x.s.Cycle(ctx); e != nil {
		t.Fatal(e)
	}
	if calls != 1 || x.v.calls != 0 {
		t.Fatalf("calls batch=%d health=%d", calls, x.v.calls)
	}
	st, e := x.s.Status(ctx)
	if e != nil {
		t.Fatal(e)
	}
	if st.Usage["checks"] != 0 || st.Counts["unavailable"] != 1 {
		t.Fatal(st)
	}
}
func TestInvalidationRetractsOnlyFailedSource(t *testing.T) {
	x := setup(t)
	ctx := t.Context()
	title := x.cat.titles[core.MovieKey(1)]
	x.s.Observe(ctx, &title, x.release("one"))
	x.s.Observe(ctx, &title, x.release("two"))
	x.s.Invalidate(ctx, "one")
	if len(x.s.verifiedKeys(ctx)) != 1 {
		t.Fatal("lost alternative source")
	}
	x.s.Invalidate(ctx, "two")
	if len(x.s.verifiedKeys(ctx)) != 0 {
		t.Fatal("failed source still promoted")
	}
}

func TestInFlightCheckCannotRestoreInvalidatedEvidence(t *testing.T) {
	x := setup(t)
	ctx := t.Context()
	x.f.pages = [][]core.Release{{x.release("r")}}
	x.v.after = func() { x.s.Invalidate(ctx, "r") }
	if err := x.s.Cycle(ctx); err != nil {
		t.Fatal(err)
	}
	if len(x.s.verifiedKeys(ctx)) != 0 {
		t.Fatal("old check overwrote newer invalidation")
	}
	st, err := x.s.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.Counts["unknown"] != 1 {
		t.Fatal(st)
	}
}

func TestPruneSkipsWritesWhenIdle(t *testing.T) {
	x := setup(t)
	statsCtx, qs := store.WithQueryStats(t.Context())
	if err := x.s.prune(statsCtx); err != nil {
		t.Fatal(err)
	}
	if qs.LockWait() > 0 || qs.Writes() > 0 {
		t.Fatalf("idle prune acquired write lock or wrote: lockWait=%s writes=%d", qs.LockWait(), qs.Writes())
	}
}
