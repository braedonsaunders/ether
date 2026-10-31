package availability

import (
	"context"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

type countingRecent struct {
	*fakeCatalog
	calls int
}

func (c *countingRecent) RecentShows(ctx context.Context, since time.Time) ([]core.Title, error) {
	c.calls++
	return c.fakeCatalog.RecentShows(ctx, since)
}

// Every library page excludes the hidden evidence-window titles, so the
// answer is memoized: a second page reads no catalogue, and a new
// observation recomputes it at once.
func TestHiddenTitlesAreMemoizedUntilAnObservation(t *testing.T) {
	x := setup(t)
	recent := &countingRecent{fakeCatalog: x.cat}
	x.s.d.Catalog = recent
	show := core.Title{Key: core.ShowKey(7), Type: core.TypeShow, Title: "Series", Language: "en", Released: x.now.Add(-time.Hour)}
	x.cat.titles[show.Key] = show
	for range 3 {
		if hidden := x.s.HiddenShows(t.Context()); len(hidden) != 1 || hidden[0] != show.Key {
			t.Fatalf("hidden: %v", hidden)
		}
	}
	if recent.calls != 1 {
		t.Fatalf("read the recent catalogue %d times for three pages", recent.calls)
	}
	if err := x.s.retainFeed(t.Context(), x.f.sources[0], []core.Release{{ID: "x", Name: "Other.S01E01.1080p.WEB-DL", Indexer: "Fake"}}); err != nil {
		t.Fatal(err)
	}
	x.s.HiddenShows(t.Context())
	if recent.calls != 2 {
		t.Fatalf("an observation did not recompute the hidden titles (%d reads)", recent.calls)
	}
}
