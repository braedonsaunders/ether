package personalize

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

func TestMatchPercentCalibration(t *testing.T) {
	if matchPercent(1) != 98 || matchPercent(0) != 60 || matchPercent(0.5) != 79 {
		t.Errorf("calibration: 1->%d 0->%d .5->%d", matchPercent(1), matchPercent(0), matchPercent(0.5))
	}
	if matchPercent(5) > 98 {
		t.Error("match must never claim 99%+")
	}
}

func TestClockIs24h(t *testing.T) {
	for style, want := range map[string]bool{
		"12h": false, "24h": true, "en-US": false, "en-GB": true, "de-DE": true,
		"en-AU": false, "fr-FR": true, "": false, "en": false,
	} {
		if got := clockIs24h(style); got != want {
			t.Errorf("clockIs24h(%q) = %v, want %v", style, got, want)
		}
	}
}

// The decision header, with a fixed clock: "NN% Match · Directed by … ·
// Ends at 9:45 PM" (12h) or "21:45" (24h); no fake % for new viewers.
func TestMicroReasonHeader(t *testing.T) {
	now := time.Date(2026, 10, 14, 20, 5, 0, 0, time.UTC)
	build := func(t *testing.T, clock string) (*harness, core.User) {
		h := newHarness(t, func(o *Options) {
			o.Now = func() time.Time { return now }
			if clock != "" {
				o.ClockFormat = func() string { return clock }
			}
		})
		u := core.User{ID: 51, Username: "fan"}
		h.users.users = []core.User{u}
		for _, j := range []int{14, 15, 16, 24, 25, 26} { // Vera Villeneuve sci-fi
			h.finish(u.ID, movieKey(0, j), now.AddDate(0, 0, -j))
		}
		h.rebuild(u)
		return h, u
	}

	h, u := build(t, "")
	target := h.cat.titles[movieKey(0, 27)] // Vera again, unwatched
	target.Runtime = 100 * time.Minute
	mr := h.e.MicroReason(context.Background(), &u, &target, now)
	if !mr.HasMatch || mr.MatchPercent < 60 || mr.MatchPercent > 98 {
		t.Fatalf("match = %+v", mr)
	}
	if !strings.Contains(mr.Header, "Directed by Vera Villeneuve") {
		t.Errorf("header %q missing the strongest reason", mr.Header)
	}
	if !strings.HasSuffix(mr.Header, "Ends at 9:45 PM") {
		t.Errorf("header %q, want 12h finish 9:45 PM", mr.Header)
	}

	h24, u24 := build(t, "en-GB")
	target24 := h24.cat.titles[movieKey(0, 27)]
	target24.Runtime = 100 * time.Minute
	mr24 := h24.e.MicroReason(context.Background(), &u24, &target24, now)
	if !strings.HasSuffix(mr24.Header, "Ends at 21:45") {
		t.Errorf("header %q, want 24h finish 21:45", mr24.Header)
	}

	// In progress: the finish time uses the remaining runtime of the
	// catalogue record (half watched, by start()).
	h.start(u.ID, target.Key, 0.5, now.Add(-time.Hour))
	catTitle := h.cat.titles[target.Key]
	mrProg := h.e.MicroReason(context.Background(), &u, &catTitle, now)
	catRT := catTitle.Runtime
	wantEnd := now.Add(catRT - catRT/2)
	if !mrProg.HasEnd || !mrProg.EndsAt.Equal(wantEnd) {
		t.Errorf("in-progress end = %v, want %v", mrProg.EndsAt, wantEnd)
	}

	// A brand-new viewer gets a finish time but no invented percentage.
	newbie := core.User{ID: 52, Username: "new"}
	h.users.users = append(h.users.users, newbie)
	h.rebuild(newbie)
	mrNew := h.e.MicroReason(context.Background(), &newbie, &target, now)
	if mrNew.HasMatch || strings.Contains(mrNew.Header, "%") {
		t.Errorf("new viewer header %q invented a match", mrNew.Header)
	}
	if !mrNew.HasEnd {
		t.Error("new viewer should still get a finish time")
	}
}

type episodeDetailCatalog struct {
	*fakeCatalog
	episodes []core.Title
}

func (c episodeDetailCatalog) Grandchildren(context.Context, core.RatingKey) ([]core.Title, error) {
	return append([]core.Title(nil), c.episodes...), nil
}

func TestShowMicroReasonUsesNextEpisodeRuntime(t *testing.T) {
	now := time.Date(2026, 10, 14, 20, 5, 0, 0, time.UTC)
	u := core.User{ID: 53, Username: "series-viewer"}
	show := core.Title{Key: showKey(0, 3), Type: core.TypeShow, Title: "A Series", Runtime: 45 * time.Minute}
	episode := core.Title{Key: core.EpisodeKey(show.Key.TMDB(), 1, 1), Type: core.TypeEpisode, Season: 1,
		Title: "Pilot", Runtime: 28 * time.Minute}
	h := newHarness(t, func(o *Options) {
		o.Now = func() time.Time { return now }
		// New starts workers that read these dependencies. Configure the
		// episode catalogue before construction, never swap a live engine's.
		o.Catalog = episodeDetailCatalog{fakeCatalog: o.Catalog.(*fakeCatalog), episodes: []core.Title{episode}}
		o.Users.(*fakeUsers).users = []core.User{u}
	})
	h.rebuild(u)

	mr := h.e.MicroReason(context.Background(), &u, &show, now)
	if !mr.HasEnd || !mr.EndsAt.Equal(now.Add(28*time.Minute)) {
		t.Fatalf("show finish = %v (hasEnd %v), want next episode at %v", mr.EndsAt, mr.HasEnd, now.Add(28*time.Minute))
	}
}
