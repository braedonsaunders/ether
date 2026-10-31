package availability

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
)

func TestLoweredAllowanceExplainsOverTheCap(t *testing.T) {
	x := setup(t)
	x.c.Availability.Effort = "custom"
	x.c.Availability.FeedRequests = 500
	for i := 0; i < 428; i++ {
		if err := x.s.spend(t.Context(), "feedRequests", 1, 500); err != nil {
			t.Fatal(err)
		}
	}
	x.c.Availability.FeedRequests = 400
	if err := x.s.spend(t.Context(), "feedRequests", 1, 400); !errors.Is(err, core.ErrBudgetExhausted) {
		t.Fatal("new requests were admitted after the allowance was lowered")
	}
	st, err := x.s.Status(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if st.Usage["feedRequests"] != 428 || st.EffectiveFeedRequests != 400 || !st.OverAllowance || st.Ceilings["feedRequests"] != 500 {
		t.Fatalf("status %+v", st)
	}
}

func TestLegacyDefaultsScaleAndCapsStayProtected(t *testing.T) {
	base := config.DefaultAvailability()
	if !base.LegacyDefaultBudgets() {
		t.Fatal("shipped defaults should scale")
	}
	four := base.Resolved([]config.IndexerCap{{}, {}, {}, {}})
	if four.Effort != "normal" || four.FeedRequests != 8000 || four.PollMinutes != 15 || four.MaxPages != 8 {
		t.Fatalf("four unlimited: %+v", four)
	}
	capped := base.Resolved([]config.IndexerCap{{DailyLimit: 100}})
	if capped.FeedRequests != 20 {
		t.Fatalf("capped usable = %d, want 20", capped.FeedRequests)
	}
	custom := base
	custom.FeedRequests = 50
	if got := custom.Resolved(fourIndexers()); got.FeedRequests != 50 || got.Effort != "custom" {
		t.Fatalf("custom was rewritten: %+v", got)
	}
	gentle := base
	gentle.Effort = "gentle"
	if got := gentle.Resolved(fourIndexers()); got.FeedRequests != 600 || got.PollMinutes != 60 {
		t.Fatalf("gentle: %+v", got)
	}
}

func fourIndexers() []config.IndexerCap { return []config.IndexerCap{{}, {}, {}, {}} }

func TestFourUnlimitedIndexersFinishCatchup(t *testing.T) {
	x := setup(t)
	x.c.Availability.Verify = false
	x.c.Availability.CatchUp = true
	x.now = time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	sources := make([]core.ReleaseFeed, 4)
	for i := range sources {
		sources[i] = core.ReleaseFeed{ID: fmt.Sprintf("ix%d", i), Name: fmt.Sprintf("Indexer %d", i), Generation: "one", Supported: true}
	}
	items := feedItems(0, 450)
	for i := range items {
		if i < 400 {
			items[i].PubDate = x.now.Add(-time.Duration(i) * time.Hour)
		} else {
			items[i].PubDate = x.now.Add(-70 * 24 * time.Hour)
		}
		items[i].Name = "Unrelated"
	}
	f := &movingFeed{sources: sources, items: items}
	x.s.d.Feeds = f
	for i := 0; i < 30; i++ {
		if err := x.s.Cycle(t.Context()); err != nil {
			t.Fatal(err)
		}
		x.now = x.now.Add(time.Minute)
	}
	if len(f.offsets) < 8 {
		t.Fatalf("catch-up never walked: %v", f.offsets)
	}
	for _, src := range sources {
		c, err := x.s.checkpoint(t.Context(), src)
		if err != nil || c.target != "" {
			t.Fatalf("%s still catching up %+v %v", src.ID, c, err)
		}
	}
}

type probeCatalog struct {
	*fakeCatalog
	homes map[core.RatingKey]time.Time
}

func (c probeCatalog) RecentMovies(context.Context, time.Time) ([]core.Title, error) {
	out := make([]core.Title, 0, len(c.titles))
	for _, t := range c.titles {
		out = append(out, t)
	}
	return out, nil
}
func (c probeCatalog) HomeReleases(context.Context, []core.RatingKey) (map[core.RatingKey]time.Time, error) {
	if c.homes == nil {
		return map[core.RatingKey]time.Time{}, nil
	}
	return c.homes, nil
}

type namedValidator struct{ reasons map[string]string }

func (v namedValidator) FeedCandidate(_ context.Context, _ *core.Title, r core.Release) (core.Release, string, error) {
	if reason := v.reasons[r.Name]; reason != "" {
		return r, reason, nil
	}
	return r, "", nil
}
func (v namedValidator) VerifyFeedCandidate(context.Context, *core.Title, core.Release) error {
	return nil
}

func TestProbeUnlocksPlayableReleaseAndKeepsGate(t *testing.T) {
	x := setup(t)
	x.c.Availability.Verify = false
	x.c.Availability.CatchUp = true
	title := x.cat.titles[core.MovieKey(1)]
	title.IDs.IMDB = "tt0000001"
	title.Popularity = 80
	x.cat.titles[title.Key] = title
	cat := probeCatalog{fakeCatalog: x.cat}
	x.s.d.Catalog = cat
	items := feedItems(0, 300)
	for i := range items {
		items[i].Name = "Unrelated"
	}
	x.s.d.Feeds = &movingFeed{sources: x.f.sources, items: items}
	x.s.d.Validator = namedValidator{reasons: map[string]string{"Film.CAM.XviD": "cam"}}
	var found string
	x.s.d.Search = func(context.Context, core.SearchRequest) ([]core.Release, error) {
		if found == "" {
			return []core.Release{{ID: "cam", Name: "Film.CAM.XviD", Indexer: "Fake"}}, nil
		}
		return []core.Release{{ID: "web", Name: "Film.2026.1080p.WEB-DL", Indexer: "Fake"}}, nil
	}
	if err := x.s.Cycle(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := x.s.FilterDiscovery(t.Context(), []core.Title{title}); len(got) != 0 {
		t.Fatal("a cam release or an unfinished feed unlocked the movie")
	}
	ex, err := x.s.Explain(t.Context(), title.Key)
	if err != nil || ex.Code != "quality" {
		t.Fatalf("explain %+v %v", ex, err)
	}
	found = "web"
	x.now = x.now.Add(7 * time.Hour)
	if err := x.s.Cycle(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := x.s.FilterDiscovery(t.Context(), []core.Title{title}); len(got) != 1 {
		t.Fatal("playable release stayed hidden")
	}
	ex, err = x.s.Explain(t.Context(), title.Key)
	if err != nil || ex.Code != "playable" {
		t.Fatalf("explain %+v %v", ex, err)
	}
}

func TestDigitalDateIsWeakEvidenceOnlyWhenEnabled(t *testing.T) {
	x := setup(t)
	x.c.Availability.CatchUp = false
	title := x.cat.titles[core.MovieKey(1)]
	cat := probeCatalog{fakeCatalog: x.cat, homes: map[core.RatingKey]time.Time{title.Key: x.now.Add(-time.Hour)}}
	x.s.d.Catalog = cat
	if got := x.s.FilterDiscovery(t.Context(), []core.Title{title}); len(got) != 0 {
		t.Fatal("digital date counted while the option is off")
	}
	x.c.Availability.DigitalDateEvidence = true
	if got := x.s.FilterDiscovery(t.Context(), []core.Title{title}); len(got) != 1 {
		t.Fatal("passed digital date did not show the movie")
	}
	cat.homes[title.Key] = time.Time{}
	x.s.clearVisibility()
	if got := x.s.FilterDiscovery(t.Context(), []core.Title{title}); len(got) != 0 {
		t.Fatal("theatrical-only movie was shown from a date")
	}
	ex, err := x.s.Explain(t.Context(), title.Key)
	if err != nil || ex.Code != "theatrical" {
		t.Fatalf("explain %+v %v", ex, err)
	}
}
