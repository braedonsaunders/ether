package watchlist

import (
	"bytes"
	"context"
	"errors"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeCatalog struct {
	core.Catalog
	titles map[core.RatingKey]core.Title
}

func (c fakeCatalog) Get(_ context.Context, k core.RatingKey) (*core.Title, error) {
	t, ok := c.titles[k]
	if !ok {
		return nil, core.ErrNotFound
	}
	return &t, nil
}

type fakeResolver struct {
	core.Resolver
	calls int
	fail  bool
}

func (f *fakeResolver) Resolve(_ context.Context, r core.ResolveRequest) (*core.Resolution, error) {
	f.calls++
	if f.fail {
		return nil, core.ErrNotAvailable
	}
	return &core.Resolution{Title: r.Title.Key, Release: core.Release{ID: "release-1"}, NZB: []byte("nzb")}, nil
}

type fakeEngine struct {
	core.Engine
	calls int
}

func (f *fakeEngine) Prefetch(context.Context, string, []byte, int64, int64) error {
	f.calls++
	return nil
}

type fakeNotifier struct{ calls int }

func (f *fakeNotifier) TitleAvailable(context.Context, string) error { f.calls++; return nil }

type fakeSource struct {
	titles []core.Title
	err    error
}

func (f *fakeSource) Watchlist(context.Context, int64, int) ([]core.Title, error) {
	return f.titles, f.err
}
func setup(t *testing.T) (*Service, *fakeResolver, *fakeEngine, *fakeNotifier, *fakeSource, *time.Time) {
	t.Helper()
	ctx := context.Background()
	db, e := store.OpenMemory(ctx)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { db.Close() })
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	released := core.Title{Key: core.MovieKey(1), Type: core.TypeMovie, Title: "Released", ContentRating: "PG", Released: now.Add(-24 * time.Hour)}
	future := core.Title{Key: core.MovieKey(2), Type: core.TypeMovie, Title: "Soon", ContentRating: "PG", Released: now.Add(24 * time.Hour)}
	adult := core.Title{Key: core.MovieKey(3), Type: core.TypeMovie, Title: "Adult", Adult: true, ContentRating: "R", Released: now.Add(-24 * time.Hour)}
	cat := fakeCatalog{titles: map[core.RatingKey]core.Title{released.Key: released, future.Key: future, adult.Key: adult}}
	res, eng, note, src := &fakeResolver{}, &fakeEngine{}, &fakeNotifier{}, &fakeSource{titles: []core.Title{released, future, adult}}
	s := New(db, Options{Catalog: cat, Resolver: res, Engine: eng, Notifier: note, Trakt: src, Now: func() time.Time { return now }})
	if e = s.SaveSettings(ctx, Settings{Enabled: true, Trakt: true, Notify: true, Prewarm: true, CheckHours: 6}); e != nil {
		t.Fatal(e)
	}
	return s, res, eng, note, src, &now
}
func TestAvailabilityIsHealthyReleasedViewerScopedAndOnce(t *testing.T) {
	s, res, eng, note, _, now := setup(t)
	ctx := context.Background()
	u := &core.User{ID: 1, MaxContentRating: "PG"}
	if e := s.Sync(ctx, u); e != nil {
		t.Fatal(e)
	}
	if e := s.Check(ctx, u); e != nil {
		t.Fatal(e)
	}
	if res.calls != 1 || eng.calls != 1 || note.calls != 1 {
		t.Fatalf("resolve/warm/notify %d/%d/%d", res.calls, eng.calls, note.calls)
	}
	h, e := s.Hubs(ctx, u, 12)
	if e != nil || len(h) != 1 || h[0].Title != "Now available" || len(h[0].Items) != 1 {
		t.Fatalf("hubs %#v %v", h, e)
	}
	other, e := s.Items(ctx, &core.User{ID: 2})
	if e != nil || len(other) != 0 {
		t.Fatalf("viewer leak %v %v", other, e)
	}
	*now = now.Add(7 * time.Hour)
	if e = s.Check(ctx, u); e != nil {
		t.Fatal(e)
	}
	if note.calls != 1 || eng.calls != 1 {
		t.Fatal("duplicate side effects")
	}
}
func TestUnavailableDoesNotNotifyAndUpstreamFailurePreservesSnapshot(t *testing.T) {
	s, res, eng, note, src, _ := setup(t)
	ctx := context.Background()
	u := &core.User{ID: 1}
	if e := s.Sync(ctx, u); e != nil {
		t.Fatal(e)
	}
	res.fail = true
	if e := s.Check(ctx, u); e != nil {
		t.Fatal(e)
	}
	if eng.calls != 0 || note.calls != 0 {
		t.Fatal("availability inferred without healthy release")
	}
	before, _ := s.Items(ctx, u)
	src.err = errors.New("upstream unavailable")
	if e := s.Sync(ctx, u); e == nil {
		t.Fatal("missing upstream failure")
	}
	after, _ := s.Items(ctx, u)
	if len(before) != len(after) {
		t.Fatal("failed sync removed items")
	}
	src.err = nil
	src.titles = nil
	if e := s.Sync(ctx, u); e != nil {
		t.Fatal(e)
	}
	after, _ = s.Items(ctx, u)
	if len(after) != 0 {
		t.Fatal("successful empty sync did not remove items")
	}
}
func TestPlexUsesOwnAccountTokenAndResolvesGUID(t *testing.T) {
	s, _, _, _, _, _ := setup(t)
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Header.Get("X-Plex-Token") != "account-token" || r.URL.Query().Get("X-Plex-Token") != "" {
			t.Error("credential handling")
		}
		if r.URL.Path != "/library/sections/watchlist/all" {
			t.Error("wrong endpoint")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"MediaContainer":{"size":1,"totalSize":1,"Metadata":[{"type":"movie","Guid":[{"id":"tmdb://1"}]}]}}`))
	}))
	defer srv.Close()
	s.o.PlexBaseURL = srv.URL
	s.o.Tokens = func(_ context.Context, uid int64) (string, error) {
		if uid != 7 {
			t.Error("wrong viewer")
		}
		return "account-token", nil
	}
	ctx := context.Background()
	cfg, _ := s.Settings(ctx)
	cfg.Plex = true
	cfg.Trakt = false
	if e := s.SaveSettings(ctx, cfg); e != nil {
		t.Fatal(e)
	}
	if e := s.Sync(ctx, &core.User{ID: 7}); e != nil {
		t.Fatal(e)
	}
	items, e := s.Items(ctx, &core.User{ID: 7})
	if e != nil || len(items) != 1 || calls != 1 {
		t.Fatalf("sync %v %v calls %d", items, e, calls)
	}
}
func TestOwnerRouterUsesRealSettingsAndViewer(t *testing.T) {
	s, _, _, _, _, _ := setup(t)
	s.o.Users = func(context.Context) ([]core.User, error) { return []core.User{{ID: 1}}, nil }
	r := httptest.NewRequest("PUT", "/settings", strings.NewReader(`{"enabled":true,"checkHours":0}`))
	w := httptest.NewRecorder()
	s.Router().ServeHTTP(w, r)
	if w.Code != 400 {
		t.Fatalf("bad interval accepted %d", w.Code)
	}
	r = httptest.NewRequest("PUT", "/users/1/items/"+core.MovieKey(1).String(), nil)
	w = httptest.NewRecorder()
	s.Router().ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("reminder failed %d %s", w.Code, w.Body.String())
	}
	r = httptest.NewRequest("GET", "/users/1/", nil)
	w = httptest.NewRecorder()
	s.Router().ServeHTTP(w, r)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "Released") {
		t.Fatalf("no persisted reminder %d %s", w.Code, w.Body.String())
	}
}

func TestRouterItemsFailureIs500(t *testing.T) {
	s, _, _, _, _, _ := setup(t)
	s.o.Users = func(context.Context) ([]core.User, error) { return []core.User{{ID: 1}}, nil }
	// Make Sync and Check succeed trivially so only the Items read can fail:
	// no upstream sources for Sync, and Check returns at once with no resolver.
	s.o.Trakt, s.o.Resolver = nil, nil
	if _, e := s.db.ExecContext(context.Background(), `DROP TABLE watchlist_items`); e != nil {
		t.Fatal(e)
	}
	for _, tc := range []struct{ method, path string }{
		{"GET", "/users/1/"},
		{"POST", "/users/1/sync"},
		{"POST", "/users/1/check"},
	} {
		w := httptest.NewRecorder()
		s.Router().ServeHTTP(w, httptest.NewRequest(tc.method, tc.path, nil))
		if w.Code != 500 {
			t.Fatalf("%s %s: got %d (%s), want 500", tc.method, tc.path, w.Code, w.Body.String())
		}
	}
}

type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestRunLogsSyncCheckAndRecordPassFailures(t *testing.T) {
	s, _, _, _, src, _ := setup(t)
	src.err = errors.New("upstream unavailable")
	s.o.Users = func(context.Context) ([]core.User, error) { return []core.User{{ID: 1}}, nil }
	// Items (used by Check) and recordPass now fail; Sync fails upstream.
	for _, q := range []string{`DROP TABLE watchlist_items`, `DROP TABLE watchlist_meta`} {
		if _, e := s.db.ExecContext(context.Background(), q); e != nil {
			t.Fatal(e)
		}
	}
	out := &lockedBuffer{}
	s.o.Logger = slog.New(slog.NewTextHandler(out, nil))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); s.Run(ctx) }()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		got := out.String()
		if strings.Contains(got, "watchlist sync failed") && strings.Contains(got, "watchlist check failed") && strings.Contains(got, "watchlist: record pass failed") {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not stop")
	}
	got := out.String()
	for _, want := range []string{"watchlist sync failed", "watchlist check failed", "watchlist: record pass failed"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in log output:\n%s", want, got)
		}
	}
}

func TestAvailabilityRetractsAfterConfirmedMissingRelease(t *testing.T) {
	s, res, _, note, _, now := setup(t)
	ctx := context.Background()
	u := &core.User{ID: 1}
	if e := s.Sync(ctx, u); e != nil {
		t.Fatal(e)
	}
	if e := s.Check(ctx, u); e != nil {
		t.Fatal(e)
	}
	res.fail = true
	*now = now.Add(7 * time.Hour)
	if e := s.Check(ctx, u); e != nil {
		t.Fatal(e)
	}
	items, e := s.Items(ctx, u)
	if e != nil {
		t.Fatal(e)
	}
	for _, it := range items {
		if it.Title.Key == core.MovieKey(1) && !it.AvailableAt.IsZero() {
			t.Fatal("missing release still advertised available")
		}
	}
	res.fail = false
	*now = now.Add(7 * time.Hour)
	if e := s.Check(ctx, u); e != nil {
		t.Fatal(e)
	}
	if note.calls != 2 {
		t.Fatal("new availability transition not notified")
	}
}

func TestContainsViewerAndMutation(t *testing.T) {
	s, _, _, _, _, _ := setup(t)
	ctx := context.Background()
	u := &core.User{ID: 1}
	key := core.MovieKey(1)
	if err := s.Remind(ctx, u, key); err != nil {
		t.Fatal(err)
	}
	yes, err := s.Contains(ctx, 1, key)
	if err != nil || !yes {
		t.Fatalf("membership %v %v", yes, err)
	}
	yes, err = s.Contains(ctx, 2, key)
	if err != nil || yes {
		t.Fatalf("viewer isolation %v %v", yes, err)
	}
	if err = s.Remove(ctx, 1, key); err != nil {
		t.Fatal(err)
	}
	yes, err = s.Contains(ctx, 1, key)
	if err != nil || yes {
		t.Fatalf("removal %v %v", yes, err)
	}
}
