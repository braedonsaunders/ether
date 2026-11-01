package catalog_test

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/catalog"
	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
	"github.com/braedonsaunders/ether/internal/tmdb"
)

// finishedSeasons is show 800 with three seasons of two episodes, all aired
// long ago except season 2's second episode, which has no air date yet.
// grow adds a season's further episodes, and last raises the season count.
// It counts SeasonDetails requests; other TMDB calls panic.
type finishedSeasons struct {
	catalog.TMDB
	mu    sync.Mutex
	calls map[int]int
	grow  map[int]int
	last  int
}

func (f *finishedSeasons) episodes(season int) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return 2 + f.grow[season]
}

func (f *finishedSeasons) lastSeason() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return max(3, f.last)
}

func (f *finishedSeasons) TVDetails(_ context.Context, id int64) (*tmdb.TVDetails, error) {
	d := &tmdb.TVDetails{TVShow: tmdb.TVShow{ID: id, Name: "Long Run", FirstAirDate: "2001-01-01", Popularity: 50, VoteCount: 500}}
	for n := 1; n <= f.lastSeason(); n++ {
		d.Seasons = append(d.Seasons, tmdb.SeasonSummary{ID: int64(n), Name: fmt.Sprintf("Season %d", n), SeasonNumber: n, EpisodeCount: f.episodes(n), AirDate: fmt.Sprintf("%d-01-01", 2000+n)})
	}
	return d, nil
}

func (f *finishedSeasons) SeasonDetails(_ context.Context, id int64, season int) (*tmdb.SeasonDetails, error) {
	f.mu.Lock()
	f.calls[season]++
	f.mu.Unlock()
	sd := &tmdb.SeasonDetails{ID: int64(season), Name: fmt.Sprintf("Season %d", season), SeasonNumber: season, AirDate: fmt.Sprintf("%d-01-01", 2000+season)}
	for e := 1; e <= f.episodes(season); e++ {
		air := fmt.Sprintf("%d-01-%02d", 2000+season, e)
		if season == 2 && e == 2 {
			air = ""
		}
		sd.Episodes = append(sd.Episodes, tmdb.Episode{ID: int64(season*10 + e), Name: fmt.Sprintf("E%d", e), SeasonNumber: season, EpisodeNumber: e, AirDate: air})
	}
	return sd, nil
}

func (f *finishedSeasons) fetched() map[int]int {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := f.calls
	f.calls = map[int]int{}
	return out
}

type seasonLoadFixture struct {
	c     *catalog.Catalog
	db    *sql.DB
	fake  *finishedSeasons
	clock *testClock
}

func newSeasonLoadFixture(t *testing.T) *seasonLoadFixture {
	t.Helper()
	f := &seasonLoadFixture{db: store.OpenTest(t), fake: &finishedSeasons{calls: map[int]int{}, grow: map[int]int{}},
		clock: &testClock{t: time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)}}
	cfg := config.Catalog{Language: "en-US", Region: "US", RefreshInterval: config.Duration(24 * time.Hour)}
	c, err := catalog.New(f.db, f.fake, cfg, slog.New(slog.DiscardHandler), catalog.WithNow(f.clock.now))
	if err != nil {
		t.Fatal(err)
	}
	f.c = c
	return f
}

// read checks a whole-show read's episodes and the seasons it fetched.
func (f *seasonLoadFixture) read(t *testing.T, step string, want []core.RatingKey, fetches map[int]int) {
	t.Helper()
	eps, err := f.c.Grandchildren(context.Background(), core.ShowKey(800))
	if err != nil {
		t.Fatal(err)
	}
	// Stale seasons are served as stored and refreshed behind the read.
	f.c.WaitRefreshes()
	if got := episodeKeys(eps); !sameKeys(got, want) {
		t.Fatalf("%s: episodes %v, want %v", step, got, want)
	}
	if got := f.fake.fetched(); fmt.Sprint(got) != fmt.Sprint(fetches) {
		t.Fatalf("%s: season fetches %v, want %v", step, got, fetches)
	}
}

func (f *seasonLoadFixture) stale() { f.clock.set(f.clock.now().Add(25 * time.Hour)) }

var showEpisodes800 = []core.RatingKey{core.EpisodeKey(800, 1, 1), core.EpisodeKey(800, 1, 2), core.EpisodeKey(800, 2, 1), core.EpisodeKey(800, 3, 1), core.EpisodeKey(800, 3, 2)}

// A whole-show read fetches missing seasons, and once its seasons go stale
// refetches only those that can still change: the latest, and one with an
// episode yet to air. A finished season is refreshed by its own reads.
func TestGrandchildrenRefreshesOnlySeasonsThatCanChange(t *testing.T) {
	f := newSeasonLoadFixture(t)
	f.read(t, "cold", showEpisodes800, map[int]int{1: 1, 2: 1, 3: 1})
	f.read(t, "fresh", showEpisodes800, map[int]int{})
	f.stale()
	f.read(t, "stale", showEpisodes800, map[int]int{2: 1, 3: 1})
	f.read(t, "refreshed", showEpisodes800, map[int]int{})
	// Opening the finished season itself still refreshes it.
	if _, err := f.c.Children(context.Background(), core.SeasonKey(800, 1)); err != nil {
		t.Fatal(err)
	}
	f.c.WaitRefreshes()
	if got := f.fake.fetched(); fmt.Sprint(got) != fmt.Sprint(map[int]int{1: 1}) {
		t.Fatalf("season page fetches %v, want season 1", got)
	}
}

// A season whose episodes are only partly mirrored, without a season detail
// record (an episode stored on its own, or data from an older version), is
// loaded in full even though it is finished and not the latest.
func TestGrandchildrenLoadsPartlyMirroredSeason(t *testing.T) {
	f := newSeasonLoadFixture(t)
	f.read(t, "cold", showEpisodes800, map[int]int{1: 1, 2: 1, 3: 1})
	season1 := int64(core.SeasonKey(800, 1))
	if _, err := f.db.Exec(`DELETE FROM catalog_titles WHERE key=?`, int64(core.EpisodeKey(800, 1, 2))); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.Exec(`UPDATE catalog_titles SET detail=0, detail_at=0 WHERE key=?`, season1); err != nil {
		t.Fatal(err)
	}
	f.stale()
	f.read(t, "stale", showEpisodes800, map[int]int{1: 1, 2: 1, 3: 1})
}

// When a new season starts, the former latest season is no longer refreshed
// as the latest; if the show's refreshed summary counts more episodes than
// the mirror holds for it, it is still loaded so its final episodes appear.
func TestGrandchildrenLoadsSeasonWhoseEpisodeCountGrew(t *testing.T) {
	f := newSeasonLoadFixture(t)
	f.read(t, "cold", showEpisodes800, map[int]int{1: 1, 2: 1, 3: 1})
	f.fake.mu.Lock()
	f.fake.grow[3], f.fake.last = 1, 4
	f.fake.mu.Unlock()
	f.stale()
	want := append(append([]core.RatingKey(nil), showEpisodes800...), core.EpisodeKey(800, 3, 3), core.EpisodeKey(800, 4, 1), core.EpisodeKey(800, 4, 2))
	// The stale read serves the mirror and refreshes the show and the seasons
	// that can change; the next read lists the new season and loads it.
	// Whether this read already sees season 3's new episode depends on
	// whether the show refresh lands first; the fetches do not.
	if _, err := f.c.Grandchildren(context.Background(), core.ShowKey(800)); err != nil {
		t.Fatal(err)
	}
	f.c.WaitRefreshes()
	// If the show refresh lands before the season refreshes are planned,
	// the new season 4 is loaded in the same pass; either way every season
	// is fetched exactly once across the two reads.
	stale := f.fake.fetched()
	if stale[2] != 1 || stale[3] != 1 || stale[4] > 1 || len(stale) > 3 {
		t.Fatalf("stale: season fetches %v, want seasons 2 and 3 (and at most season 4)", stale)
	}
	next := map[int]int{4: 1}
	if stale[4] == 1 {
		next = map[int]int{}
	}
	f.read(t, "new season", want, next)
}
