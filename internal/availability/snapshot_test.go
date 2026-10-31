package availability

import (
	"context"
	"encoding/json"
	"os"
	"reflect"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
)

// ETHE-16 A: preserve recorded older Home titles, including ordering and
// duplicates, while bounding full settings copies independently of item count.
func recordedHome(tb testing.TB) []core.Title {
	tb.Helper()
	raw, err := os.ReadFile("../../testdata/conform/plex/web/web_json_hubs.json")
	if err != nil {
		tb.Fatal(err)
	}
	var recording struct{ Body []byte }
	if err := json.Unmarshal(raw, &recording); err != nil {
		tb.Fatal(err)
	}
	var body struct {
		MediaContainer struct {
			Hub []struct {
				Metadata []struct {
					RatingKey             string
					Type                  core.MediaType
					OriginallyAvailableAt string
				}
			}
		}
	}
	if err := json.Unmarshal(recording.Body, &body); err != nil {
		tb.Fatal(err)
	}
	var items []core.Title
	for _, h := range body.MediaContainer.Hub {
		for _, m := range h.Metadata {
			date, err := time.Parse("2006-01-02", m.OriginallyAvailableAt)
			if err != nil || !date.Before(time.Date(2025, 9, 30, 0, 0, 0, 0, time.UTC)) || (m.Type != core.TypeMovie && m.Type != core.TypeShow) {
				continue
			}
			id, err := strconv.ParseInt(m.RatingKey, 10, 64)
			if err != nil {
				tb.Fatal(err)
			}
			key := core.MovieKey(id)
			if m.Type == core.TypeShow {
				key = core.ShowKey(id)
			}
			items = append(items, core.Title{Key: key, Type: m.Type, Released: date})
		}
	}
	if len(items) == 0 {
		tb.Fatal("empty recorded Home")
	}
	return items
}

func TestDiscoverySnapshotRecordedHome(t *testing.T) {
	x := setup(t)
	settings, err := config.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	copies := 0
	x.s.cfg = func() config.Config { copies++; return settings.Get() }
	items := recordedHome(t)
	for _, filter := range []struct {
		name string
		run  func(context.Context, []core.Title) []core.Title
	}{
		{"filter", x.s.FilterDiscovery}, {"prioritize", x.s.Prioritize},
	} {
		copies = 0
		if got := filter.run(t.Context(), items); !reflect.DeepEqual(got, items) {
			t.Fatalf("%s changed Home content/order", filter.name)
		}
		if copies != 1 {
			t.Fatalf("%s copies=%d want one", filter.name, copies)
		}
		t.Logf("%s: %d recorded items, unchanged content/order, %d full settings copy", filter.name, len(items), copies)
	}
}

type blockingCandidate struct {
	*fakeValidator
	entered, resume chan struct{}
	once            sync.Once
}

func (v *blockingCandidate) FeedCandidate(ctx context.Context, title *core.Title, r core.Release) (core.Release, string, error) {
	v.once.Do(func() { close(v.entered); <-v.resume })
	return v.fakeValidator.FeedCandidate(ctx, title, r)
}

// ETHE-16 A: an update during filtering must not mix availability inputs;
// next-call settings/source changes and explicit invalidation must still win.
func TestDiscoverySnapshotConcurrentUpdateAndInvalidation(t *testing.T) {
	x := setup(t)
	settings, err := config.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := settings.Update(func(c *config.Config) error { *c = x.c; return nil }); err != nil {
		t.Fatal(err)
	}
	var copies atomic.Int64
	x.s.cfg = func() config.Config { copies.Add(1); return settings.Get() }
	title := x.cat.titles[core.MovieKey(1)]
	if err := x.s.remember(t.Context(), x.f.sources[0], title, x.release("good"), x.s.policy()); err != nil {
		t.Fatal(err)
	}
	v := &blockingCandidate{fakeValidator: x.v, entered: make(chan struct{}), resume: make(chan struct{})}
	x.s.d.Validator = v
	copies.Store(0)
	done := make(chan []core.Title, 1)
	go func() { done <- x.s.FilterDiscovery(t.Context(), []core.Title{title}) }()
	<-v.entered
	// Both mutation of a returned copy and a durable update are isolated from
	// the in-flight snapshot. No provider or production account is involved.
	detached := settings.Get()
	detached.Usenet.Providers[0].Enabled = false
	if !settings.Get().Usenet.Providers[0].Enabled {
		t.Fatal("Store.Get leaked mutation")
	}
	if err := settings.Update(func(c *config.Config) error { c.Usenet.Providers[0].Enabled = false; return nil }); err != nil {
		t.Fatal(err)
	}
	x.s.clearVisibility() // concurrent invalidation must prevent stale cache writes
	close(v.resume)
	if got := <-done; len(got) != 1 {
		t.Fatal("update changed in-flight inputs", got)
	}
	if copies.Load() != 1 {
		t.Fatalf("in-flight copies=%d", copies.Load())
	}
	x.s.visibilityMu.Lock()
	cached := len(x.s.visibilityCache)
	x.s.visibilityMu.Unlock()
	if cached != 0 {
		t.Fatal("invalidation lost to in-flight cache write")
	}
	if got := x.s.FilterDiscovery(t.Context(), []core.Title{title}); len(got) != 0 {
		t.Fatal("next call ignored disabled source")
	}
	if err := settings.Update(func(c *config.Config) error { c.Usenet.Providers[0].Enabled = true; return nil }); err != nil {
		t.Fatal(err)
	}
	if got := x.s.FilterDiscovery(t.Context(), []core.Title{title}); len(got) != 1 {
		t.Fatal("next call ignored enabled source")
	}
	x.f.sources[0].Generation = "changed"
	if got := x.s.FilterDiscovery(t.Context(), []core.Title{title}); len(got) != 0 {
		t.Fatal("source generation reused visibility")
	}
	t.Log("in-flight inputs isolated; next-call source updates and concurrent generation invalidation honored")
}

// ETHE-16 A: the snapshot owns all referenced discovery inputs, not only the
// Config struct header. Mutating the dependency's later values cannot change it.
func TestDiscoverySnapshotOwnsInputs(t *testing.T) {
	x := setup(t)
	x.c.Availability.IndexerIDs = []string{"f"}
	x.f.sources[0].Categories = []int{2000}
	input := x.s.discoverySnapshot()
	policy := input.policy
	x.c.Availability.IndexerIDs[0] = "other"
	x.c.Usenet.Providers[0].Enabled = false
	x.f.sources[0].Generation = "changed"
	x.f.sources[0].Categories[0] = 5000
	if input.availability.IndexerIDs[0] != "f" || input.feeds[0].Generation != "one" || input.feeds[0].Categories[0] != 2000 || !input.usable("") || input.policy != policy {
		t.Fatal("snapshot aliases mutable dependency")
	}
	if next := x.s.discoverySnapshot(); next.policy == policy || next.usable("") {
		t.Fatal("snapshot remained stale on next call")
	}
}

func BenchmarkDiscoveryRecordedHome(b *testing.B) {
	settings, err := config.Open(b.TempDir())
	if err != nil {
		b.Fatal(err)
	}
	items := recordedHome(b)
	copies := 0
	// Old recorded titles need no database, catalogue or validator work.
	s := &Service{cfg: func() config.Config { copies++; return settings.Get() }, d: Deps{Feeds: &fakeFeeds{}}, now: func() time.Time { return time.Date(2026, 10, 31, 10, 0, 0, 0, time.UTC) }}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if got := s.FilterDiscovery(context.Background(), items); len(got) != len(items) {
			b.Fatal("changed visibility")
		}
	}
	b.ReportMetric(float64(copies)/float64(b.N), "copies/op")
}

// ETHE-16 A also covers nested retained-release and newly-available reads:
// use the same operation inputs for evidence, ranking and verified-key lookup.
func TestDiscoverySnapshotRecentOperations(t *testing.T) {
	x := setup(t)
	settings, err := config.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := settings.Update(func(c *config.Config) error { *c = x.c; return nil }); err != nil {
		t.Fatal(err)
	}
	copies := 0
	x.s.cfg = func() config.Config { copies++; return settings.Get() }
	title := x.cat.titles[core.MovieKey(1)]
	if err := x.s.retainFeed(t.Context(), x.f.sources[0], []core.Release{x.release("retained")}); err != nil {
		t.Fatal(err)
	}
	copies = 0
	if got := x.s.FilterDiscovery(t.Context(), []core.Title{title}); len(got) != 1 {
		t.Fatal("retained evidence did not unlock recent title")
	}
	if copies != 1 {
		t.Fatalf("retained path copies=%d", copies)
	}
	x.s.Observe(t.Context(), &title, x.release("retained"))
	copies = 0
	if got := x.s.Prioritize(t.Context(), []core.Title{title}); len(got) != 1 {
		t.Fatal("recent verified title missing")
	}
	if copies != 1 {
		t.Fatalf("prioritize copies=%d", copies)
	}
	copies = 0
	if got := x.s.Hubs(t.Context(), 10); len(got) != 1 || len(got[0].Items) != 1 || got[0].Items[0].Key != title.Key {
		t.Fatal("newly available shelf changed", got)
	}
	if copies != 1 {
		t.Fatalf("hubs copies=%d", copies)
	}
	if err := settings.Update(func(c *config.Config) error { c.Availability.NewlyAvailable = false; return nil }); err != nil {
		t.Fatal(err)
	}
	if got := x.s.Hubs(t.Context(), 10); len(got) != 0 {
		t.Fatal("next call ignored shelf settings")
	}
	t.Log("retained evidence, verified priority and newly available shelves each use one settings copy; next-call preference updates apply")
}
