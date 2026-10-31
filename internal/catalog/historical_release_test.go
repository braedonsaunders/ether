package catalog

import (
	"context"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
)

func TestPre1970ReleasesStayKnown(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	c := newChunkTestCatalog(t)
	c.SetNow(func() time.Time { return now })
	showID := int64(91759)
	show := core.ShowKey(showID)
	casablanca := time.Date(1942, 11, 26, 0, 0, 0, 0, time.UTC)
	aired := time.Date(1960, 12, 9, 0, 0, 0, 0, time.UTC)
	later := time.Date(1961, 6, 1, 0, 0, 0, 0, time.UTC)
	otherAired := time.Date(1971, 1, 4, 0, 0, 0, 0, time.UTC)
	future := now.Add(48 * time.Hour)
	recent := now.Add(-10 * 24 * time.Hour)
	classic := core.MovieKey(289)
	undated := core.MovieKey(290)
	upcoming := core.MovieKey(291)
	fresh := core.MovieKey(292)
	modern := core.MovieKey(293)
	ep1 := core.EpisodeKey(showID, 1, 1)
	ep2 := core.EpisodeKey(showID, 1, 2)
	epUnknown := core.EpisodeKey(showID, 1, 3)
	epFuture := core.EpisodeKey(showID, 1, 4)
	epOther := core.EpisodeKey(showID, 11, 1)
	earlyID := int64(1952)
	early := core.ShowKey(earlyID)
	earlyAired := time.Date(1952, 6, 1, 0, 0, 0, 0, time.UTC)
	items := []core.Title{
		{Key: classic, Type: core.TypeMovie, Title: "Casablanca", Year: 1942, Released: casablanca},
		{Key: modern, Type: core.TypeMovie, Title: "Later", Year: 1972, Released: time.Date(1972, 3, 1, 0, 0, 0, 0, time.UTC)},
		{Key: undated, Type: core.TypeMovie, Title: "Undated"},
		{Key: upcoming, Type: core.TypeMovie, Title: "Upcoming", Released: future},
		{Key: fresh, Type: core.TypeMovie, Title: "Fresh", Released: recent},
		{Key: show, Type: core.TypeShow, Title: "Coronation Street", Released: aired},
		{Key: core.SeasonKey(showID, 1), Type: core.TypeSeason, ShowKey: show, Season: 1, Title: "Season 1", Released: aired},
		{Key: core.SeasonKey(showID, 11), Type: core.TypeSeason, ShowKey: show, Season: 11, Title: "Season 11", Released: time.Date(1971, 1, 1, 0, 0, 0, 0, time.UTC)},
		{Key: ep1, Type: core.TypeEpisode, ShowKey: show, Season: 1, Episode: 1, Title: "Episode 1", Released: aired, StillPath: "/still.jpg"},
		{Key: ep2, Type: core.TypeEpisode, ShowKey: show, Season: 1, Episode: 2, Title: "Episode 2", Released: later},
		{Key: epUnknown, Type: core.TypeEpisode, ShowKey: show, Season: 1, Episode: 3, Title: "Undated episode"},
		{Key: epFuture, Type: core.TypeEpisode, ShowKey: show, Season: 1, Episode: 4, Title: "Tomorrow", Released: future},
		{Key: epOther, Type: core.TypeEpisode, ShowKey: show, Season: 11, Episode: 1, Title: "Other season", Released: otherAired},
		{Key: early, Type: core.TypeShow, Title: "Early", Released: earlyAired},
		{Key: core.EpisodeKey(earlyID, 1, 1), Type: core.TypeEpisode, ShowKey: early, Season: 1, Episode: 1, Title: "Early episode", Released: earlyAired},
		{Key: core.ShowKey(1953), Type: core.TypeShow, Title: "Not yet", Released: future},
	}
	if err := c.upsertMany(ctx, items, upsertOpts{browsable: true, detail: true}); err != nil {
		t.Fatal(err)
	}
	var known, ts int64
	if err := c.db.QueryRowContext(ctx, `SELECT release_known, released_ts FROM catalog_titles WHERE key=?`, int64(ep1)).Scan(&known, &ts); err != nil || known != 1 || ts >= 0 {
		t.Fatalf("1960 flag=%d ts=%d err=%v", known, ts, err)
	}
	if err := c.db.QueryRowContext(ctx, `SELECT release_known FROM catalog_titles WHERE key=?`, int64(undated)).Scan(&known); err != nil || known != 0 {
		t.Fatalf("unknown flag=%d err=%v", known, err)
	}

	page, err := c.SeasonEpisodePage(ctx, show, 1, 0, 50)
	if err != nil || page.Total != 2 || len(page.Items) != 2 || page.Items[0].Key != ep1 || page.Items[1].Key != ep2 {
		t.Fatalf("season 1 %+v %v", page, err)
	}
	other, err := c.SeasonEpisodePage(ctx, show, 11, 0, 50)
	if err != nil || other.Total != 1 || len(other.Items) != 1 || other.Items[0].Key != epOther {
		t.Fatalf("season 11 %+v %v", other, err)
	}
	seasons, err := c.queryTitles(ctx, `SELECT `+titleCols+` FROM catalog_titles WHERE type='season' AND show_key=? ORDER BY season ASC`, int64(show))
	if err != nil || len(seasons) != 2 || seasons[0].ChildCount != 2 || seasons[1].ChildCount != 1 {
		t.Fatalf("season counts %+v %v", seasons, err)
	}

	added, err := c.CachedAddedTimes(ctx, []core.RatingKey{classic, undated, upcoming, show, core.SeasonKey(showID, 1), ep1, epFuture})
	if err != nil || !added[classic].Equal(casablanca) || !added[show].Equal(otherAired) || !added[core.SeasonKey(showID, 1)].Equal(later) || !added[ep1].Equal(aired) || !added[undated].IsZero() || !added[upcoming].IsZero() || !added[epFuture].IsZero() {
		t.Fatalf("date added %+v %v", added, err)
	}
	unplayed, err := c.CountUnplayedAired(ctx, 7, []core.RatingKey{show, core.SeasonKey(showID, 1)}, "")
	if err != nil || unplayed[show] != 3 || unplayed[core.SeasonKey(showID, 1)] != 2 {
		t.Fatalf("unplayed %+v %v", unplayed, err)
	}
	if _, err := c.db.ExecContext(ctx, `INSERT INTO state_watch(user_id, rating_key, view_count) VALUES(7, ?, 1)`, int64(ep1)); err != nil {
		t.Fatal(err)
	}
	unplayed, err = c.CountUnplayedAired(ctx, 7, []core.RatingKey{core.SeasonKey(showID, 1)}, "")
	if err != nil || unplayed[core.SeasonKey(showID, 1)] != 1 {
		t.Fatalf("unplayed after watch %+v %v", unplayed, err)
	}

	browsed, err := c.Browse(ctx, core.BrowseQuery{Filter: core.Filter{Type: core.TypeMovie, KnownAddedDate: true}, Sort: core.SortAdded, Desc: false, Limit: 10})
	if err != nil || len(browsed.Items) != 3 || browsed.Items[0].Key != classic || browsed.Items[1].Key != modern || browsed.Items[2].Key != fresh {
		t.Fatalf("added order %+v %v", keysOf(browsed.Items), err)
	}
	showsAdded, err := c.Browse(ctx, core.BrowseQuery{Filter: core.Filter{Type: core.TypeShow, KnownAddedDate: true}, Sort: core.SortAdded, Desc: true, Limit: 10})
	if err != nil || len(showsAdded.Items) != 2 || showsAdded.Items[0].Key != show || !showsAdded.Items[0].AddedAt.Equal(otherAired) || showsAdded.Items[1].Key != early || !showsAdded.Items[1].AddedAt.Equal(earlyAired) {
		t.Fatalf("show added %+v %v", showsAdded, err)
	}
	desc, err := c.Browse(ctx, core.BrowseQuery{Filter: core.Filter{Type: core.TypeMovie}, Sort: core.SortReleased, Desc: true, Limit: 10})
	if err != nil || len(desc.Items) != 3 || desc.Items[0].Key != fresh || desc.Items[len(desc.Items)-1].Key != classic {
		t.Fatalf("release order %+v %v", keysOf(desc.Items), err)
	}
	airedEpisodes, err := c.CachedAiredEpisodes(ctx, show, 0, 10)
	if err != nil || airedEpisodes.Total != 3 || len(airedEpisodes.Items) != 3 || airedEpisodes.Items[0].Key != epOther {
		t.Fatalf("aired %+v %v", airedEpisodes, err)
	}
	movies, _, err := c.RecentAir(ctx, core.TypeMovie, 0, 10)
	if err != nil || len(movies) != 3 || movies[0].Key != fresh || movies[len(movies)-1].Key != classic || !movies[len(movies)-1].AddedAt.Equal(casablanca) {
		t.Fatalf("recent movies %+v %v", movies, err)
	}
	shows, _, err := c.RecentAir(ctx, core.TypeShow, 0, 10)
	if err != nil || len(shows) != 2 || shows[0].Key != show || !shows[0].AddedAt.Equal(otherAired) || shows[1].Key != early || !shows[1].AddedAt.Equal(earlyAired) {
		t.Fatalf("recent shows %+v %v", shows, err)
	}
	dates, err := NewMirror(c.db, func() time.Time { return now }).AirDates(ctx, now, false)
	if err != nil || dates[show] != otherAired.Unix() {
		t.Fatalf("air dates %+v %v", dates, err)
	}
}

func TestPre1970MoviePassesHomeReleaseGate(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	db := store.OpenTest(t)
	c, err := New(db, nil, config.Catalog{GateMoviesByHomeRelease: true}, nil, WithNow(func() time.Time { return now }))
	if err != nil {
		t.Fatal(err)
	}
	classic := core.MovieKey(289)
	fresh := core.MovieKey(292)
	upcoming := core.MovieKey(291)
	undated := core.MovieKey(290)
	items := []core.Title{
		{Key: classic, Type: core.TypeMovie, Title: "Casablanca", Released: time.Date(1942, 11, 26, 0, 0, 0, 0, time.UTC)},
		{Key: fresh, Type: core.TypeMovie, Title: "Fresh", Released: now.Add(-10 * 24 * time.Hour)},
		{Key: upcoming, Type: core.TypeMovie, Title: "Upcoming", Released: now.Add(48 * time.Hour)},
		{Key: undated, Type: core.TypeMovie, Title: "Undated"},
	}
	if err := c.upsertMany(ctx, items, upsertOpts{browsable: true, detail: true}); err != nil {
		t.Fatal(err)
	}
	for _, key := range []core.RatingKey{classic, fresh, upcoming, undated} {
		if _, err := db.ExecContext(ctx, `INSERT INTO catalog_home_release(key, checked, earliest_ts, fallback_admitted, attempted_at) VALUES(?,1,0,0,?)`, int64(key), now.Unix()); err != nil {
			t.Fatal(err)
		}
	}
	page, err := c.Browse(ctx, core.BrowseQuery{Filter: core.Filter{Type: core.TypeMovie}, Sort: core.SortReleased, Desc: false, Limit: 10})
	if err != nil || len(page.Items) != 1 || page.Items[0].Key != classic {
		t.Fatalf("gated %+v %v", keysOf(page.Items), err)
	}
}

func TestFutureEpisodeStaysScheduled(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	db := store.OpenTest(t)
	c, err := New(db, nil, config.Catalog{ShowUpcomingEpisodes: true}, nil, WithNow(func() time.Time { return now }))
	if err != nil {
		t.Fatal(err)
	}
	showID := int64(42)
	show := core.ShowKey(showID)
	aired := time.Date(1960, 12, 9, 0, 0, 0, 0, time.UTC)
	future := now.Add(48 * time.Hour)
	items := []core.Title{
		{Key: show, Type: core.TypeShow, Title: "Street", Released: aired, Status: "Ended"},
		{Key: core.SeasonKey(showID, 1), Type: core.TypeSeason, ShowKey: show, Season: 1, Title: "Season 1", Released: aired},
		{Key: core.EpisodeKey(showID, 1, 1), Type: core.TypeEpisode, ShowKey: show, Season: 1, Episode: 1, Released: aired},
		{Key: core.EpisodeKey(showID, 1, 2), Type: core.TypeEpisode, ShowKey: show, Season: 1, Episode: 2, Released: future},
	}
	if err := c.upsertMany(ctx, items, upsertOpts{browsable: true, detail: true}); err != nil {
		t.Fatal(err)
	}
	page, err := c.SeasonEpisodePage(ctx, show, 1, 0, 50)
	if err != nil || page.Total != 1 || page.Items[0].Episode != 1 {
		t.Fatalf("aired page %+v %v", page, err)
	}
	scheduled, err := c.ScheduledEpisodes(ctx, core.SeasonKey(showID, 1))
	if err != nil || len(scheduled) != 1 || scheduled[0].Episode != 2 || !scheduled[0].Released.Equal(future) {
		t.Fatalf("scheduled %+v %v", scheduled, err)
	}
}

func keysOf(items []core.Title) []core.RatingKey {
	out := make([]core.RatingKey, len(items))
	for i := range items {
		out[i] = items[i].Key
	}
	return out
}
