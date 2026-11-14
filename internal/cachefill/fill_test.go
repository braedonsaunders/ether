package cachefill

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/segcache"
)

type catalogFake struct{ items []core.Title }

func (c catalogFake) Browse(_ context.Context, q core.BrowseQuery) (core.Page, error) {
	if q.Offset >= len(c.items) {
		return core.Page{Total: len(c.items)}, nil
	}
	return core.Page{Items: c.items[q.Offset:min(q.Offset+q.Limit, len(c.items))], Total: len(c.items)}, nil
}

func (c catalogFake) CachedTitle(_ context.Context, key core.RatingKey) (*core.Title, error) {
	for _, t := range c.items {
		if t.Key == key {
			return &t, nil
		}
	}
	return nil, core.ErrNotFound
}
func (c catalogFake) Children(context.Context, core.RatingKey) ([]core.Title, error) { return nil, nil }

type resolverFake struct {
	core.Resolver
	calls atomic.Int32
}

func (r *resolverFake) Resolve(_ context.Context, req core.ResolveRequest) (*core.Resolution, error) {
	r.calls.Add(1)
	return &core.Resolution{Release: core.Release{ID: fmt.Sprint(req.Title.Key), Size: 32}, NZB: []byte("payload")}, nil
}

type engineFake struct {
	cache  *segcache.Cache
	fail   bool
	calls  int
	during func(context.Context)
}

func (e *engineFake) Open(_ context.Context, id string, _ []byte) (core.MediaSource, error) {
	return &sourceFake{Reader: bytes.NewReader(make([]byte, 32)), id: id}, nil
}
func (e *engineFake) Prefetch(context.Context, string, []byte, int64, int64) error {
	panic("managed fill must use durable prefetch")
}
func (e *engineFake) DurablePrefetch(ctx context.Context, id string, _ []byte, off, n int64) error {
	e.calls++
	if e.during != nil {
		e.during(ctx)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if e.fail {
		return errors.New("disk unavailable")
	}
	return e.cache.Put(id, int(off), make([]byte, n))
}

type sourceFake struct {
	*bytes.Reader
	id string
}

func (s *sourceFake) Name() string      { return "movie.mkv" }
func (s *sourceFake) ReleaseID() string { return s.id }
func (s *sourceFake) Close() error      { return nil }

func harness(t *testing.T) (*Manager, *config.Cache, *engineFake, *resolverFake) {
	t.Helper()
	dir := t.TempDir()
	c, err := segcache.Open(dir, 1<<30, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	cfg := &config.Cache{Mode: "adaptive", BudgetBytes: 1 << 30, PreloadEnabled: true, PreloadBudgetBytes: 256 << 20, PreloadMaxTitles: 2}
	cat := catalogFake{items: []core.Title{{Key: core.MovieKey(1), Title: "One", Type: core.TypeMovie, Released: time.Now().Add(-time.Hour)}, {Key: core.MovieKey(2), Title: "Two", Type: core.TypeMovie, Released: time.Now().Add(-time.Hour)}}}
	eng := &engineFake{cache: c}
	res := &resolverFake{}
	m, err := New(dir, Deps{Catalog: cat, Resolver: res, Engine: eng, Cache: c, State: &watchFake{history: map[int64][]core.HistoryEntry{1: {{Key: core.MovieKey(999), ViewedAt: time.Now()}}}}, Household: func(context.Context, string) ([]core.User, error) { return []core.User{{ID: 1}}, nil }, Config: func() config.Cache { return *cfg }, Owner: func(context.Context) (*core.User, error) { return &core.User{ID: 1}, nil }, FreeBytes: func() (int64, error) { return 100 << 30, nil }})
	if err != nil {
		t.Fatal(err)
	}
	return m, cfg, eng, res
}
func TestFillsPersistsAndReusesCompletedShelf(t *testing.T) {
	m, _, eng, res := harness(t)
	if err := m.pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	if s := m.Status(); s.Ready != 2 || s.Allocated > 256<<20 {
		t.Fatalf("unexpected shelf %+v", s)
	}
	if eng.calls != 2 {
		t.Fatal(eng.calls)
	}
	for _, it := range m.Status().Items {
		if len(eng.cache.Pins()[it.ReleaseID]) != 1 {
			t.Fatal("completed source unprotected")
		}
		if it.State != "cached" {
			t.Fatalf("item state = %q", it.State)
		}
	}
	for _, sel := range m.Status().Selection {
		if sel.State != "cached" || len(sel.ReasonCodes) == 0 {
			t.Fatalf("selection = %+v", sel)
		}
	}
	for _, rel := range eng.cache.Releases() {
		if rel.Reason == nil || len(rel.Reason.Causes) == 0 {
			t.Fatalf("missing shelf reason: %+v", rel)
		}
		cause := rel.Reason.Causes[0]
		if cause.Code != core.CacheCauseShelf || cause.Shelf == nil || cause.Shelf.State != "fetched" || rel.Reason.File == "" {
			t.Fatalf("shelf reason = %+v", cause)
		}
	}
	reopened, err := New(eng.cache.Directory(), m.deps)
	if err != nil {
		t.Fatal(err)
	}
	if err = reopened.pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	if res.calls.Load() != 2 || reopened.Status().Ready != 2 {
		t.Fatal("completed sources fetched again")
	}
}
func TestDisabledPreloadingNeverResolvesAndReleasesItsPins(t *testing.T) {
	m, cfg, eng, res := harness(t)
	cfg.PreloadEnabled = false
	if err := m.pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	if res.calls.Load() != 0 {
		t.Fatal("disabled job resolved")
	}
	cfg.PreloadEnabled = true
	if err := m.pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	cfg.PreloadEnabled = false
	if err := m.pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(eng.cache.Pins()) != 0 || m.Status().Ready != 0 {
		t.Fatal("disabled shelf kept reservations")
	}
	if used, _ := eng.cache.Usage(); used == 0 {
		t.Fatal("disabling must return bytes to retention, not delete them")
	}
}
func TestIdleAndDiskGuardsAvoidProviderWork(t *testing.T) {
	for _, mode := range []string{"live", "disk", "paused"} {
		t.Run(mode, func(t *testing.T) {
			m, _, eng, res := harness(t)
			switch mode {
			case "live":
				m.deps.Active = func() bool { return true }
			case "disk":
				m.deps.FreeBytes = func() (int64, error) { return 1 << 30, nil }
			case "paused":
				m.Pause(true)
			}
			if err := m.pass(context.Background()); err == nil {
				t.Fatal("guard ignored")
			}
			if eng.calls != 0 || res.calls.Load() != 0 {
				t.Fatal("guard allowed provider work")
			}
		})
	}
}
func TestFailedDurableWritesNeverBecomeReady(t *testing.T) {
	m, _, eng, _ := harness(t)
	eng.fail = true
	if err := m.pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	s := m.Status()
	if s.Ready != 0 || s.Failed != 2 {
		t.Fatalf("false completion: %+v", s)
	}
	if len(s.Items) != 2 {
		t.Fatalf("failed titles missing from manifest: %+v", s.Items)
	}
	for _, it := range s.Items {
		if it.State != "failed" {
			t.Fatalf("item state = %q", it.State)
		}
	}
	for _, sel := range s.Selection {
		if sel.State != "failed" {
			t.Fatalf("selection state = %q", sel.State)
		}
	}
}
func TestAllocationAndTitleLimits(t *testing.T) {
	m, cfg, eng, _ := harness(t)
	cfg.PreloadBudgetBytes = 16 << 20
	if err := m.pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	if eng.calls != 0 || m.Status().Allocated != 0 {
		t.Fatal("oversized source admitted")
	}
	cfg.PreloadBudgetBytes = 128 << 20
	cfg.PreloadMaxTitles = 1
	if err := m.pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	if m.Status().Ready != 1 {
		t.Fatal(m.Status())
	}
	cfg.PreloadBudgetBytes = 16 << 20
	if err := m.pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	if m.Status().Allocated != 0 {
		t.Fatal("lower budget left shelf over allocated")
	}
}
func TestFutureTitlesNeverResolve(t *testing.T) {
	m, _, _, res := harness(t)
	m.deps.Catalog = catalogFake{items: []core.Title{{Key: core.MovieKey(3), Type: core.TypeMovie, Released: time.Now().Add(time.Hour)}}}
	if err := m.pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	if res.calls.Load() != 0 {
		t.Fatal("future title was preloaded")
	}
}

func TestShelfRotatesToNewHigherRankedTitles(t *testing.T) {
	m, cfg, eng, _ := harness(t)
	cfg.PreloadMaxTitles = 1
	if err := m.pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	old := m.Status().Items[0]
	m.deps.Catalog = catalogFake{items: []core.Title{{Key: core.MovieKey(3), Title: "New favourite", Type: core.TypeMovie, Released: time.Now().Add(-time.Hour)}}}
	if err := m.pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	s := m.Status()
	if s.Ready != 1 || s.Items[0].Key != core.MovieKey(3) {
		t.Fatal(s)
	}
	if len(eng.cache.Pins()[old.ReleaseID]) > 0 {
		t.Fatal("old preload pin remained")
	}
	if eng.cache.ReleaseBytes(old.ReleaseID) == 0 {
		t.Fatal("rotation should not delete cached bytes")
	}
}
func TestPauseSurvivesRestart(t *testing.T) {
	m, _, eng, _ := harness(t)
	if err := m.Pause(true); err != nil {
		t.Fatal(err)
	}
	reopened, err := New(eng.cache.Directory(), m.deps)
	if err != nil {
		t.Fatal(err)
	}
	if !reopened.Status().Paused {
		t.Fatal("pause lost at restart")
	}
}

func TestCancelledRunStopsProviderWork(t *testing.T) {
	m, _, eng, _ := harness(t)
	ctx, cancel := context.WithCancel(context.Background())
	entered := make(chan struct{})
	eng.during = func(providerCtx context.Context) {
		close(entered)
		<-providerCtx.Done()
	}
	done := make(chan struct{})
	go func() { m.Run(ctx); close(done) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("preload worker still running during cleanup")
		}
	})
	// Reservation and manifest writes happen before provider work starts.
	// Measure cancellation only after the provider has entered, not from
	// worker launch (which also measures filesystem startup under race CI).
	select {
	case <-entered:
	case <-done:
		t.Fatal("preload worker stopped before provider work")
	case <-time.After(10 * time.Second):
		t.Fatal("preload provider work did not start")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("preload worker did not stop after provider cancellation")
	}
	if eng.calls != 1 {
		t.Fatal("cancelled fill kept fetching", eng.calls)
	}
}
