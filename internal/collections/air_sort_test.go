package collections

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	_ "modernc.org/sqlite"
)

func TestAirDateSorts(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	_, err = db.Exec(`CREATE TABLE catalog_titles (show_key INTEGER, type TEXT, season INTEGER, episode INTEGER, released_ts INTEGER, release_known INTEGER GENERATED ALWAYS AS (CASE WHEN released_ts<>0 THEN 1 ELSE 0 END) VIRTUAL)`)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	day := func(n int) int64 { return now.AddDate(0, 0, n).Unix() }
	for _, row := range []struct {
		show, season, ep int
		date             int64
	}{{1, 1, 1, day(-30)}, {1, 1, 2, day(-1)}, {1, 2, 1, day(1)}, {1, 0, 1, day(0)}, {2, 1, 1, day(-10)}, {2, 2, 1, day(-5)}, {2, 2, 2, 0}, {3, 1, 1, day(-40)}, {3, 2, 1, 0}, {3, 2, 2, day(-2)}} {
		if _, err := db.Exec(`INSERT INTO catalog_titles(show_key,type,season,episode,released_ts) VALUES (?, 'episode', ?, ?, ?)`, row.show, row.season, row.ep, row.date); err != nil {
			t.Fatal(err)
		}
	}
	s := &Service{db: db}
	for _, tc := range []struct {
		mode string
		desc bool
		want []core.RatingKey
	}{
		{"latestEpisode", true, []core.RatingKey{1, 3, 2}}, {"latestEpisode", false, []core.RatingKey{2, 3, 1}},
		{"latestSeasonPremiere", true, []core.RatingKey{2, 1, 3}}, {"latestSeasonPremiere", false, []core.RatingKey{1, 2, 3}},
	} {
		cs := []*cand{{e: &entry{key: 3, typ: core.TypeShow}}, {e: &entry{key: 2, typ: core.TypeShow}}, {e: &entry{key: 1, typ: core.TypeShow}}}
		if err := s.loadAirDates(context.Background(), cs, tc.mode, now); err != nil {
			t.Fatal(err)
		}
		sortCands(cs, tc.mode, tc.desc, 0)
		for i, want := range tc.want {
			if cs[i].e.key != want {
				t.Fatalf("%s desc=%v: position %d got %d want %d", tc.mode, tc.desc, i, cs[i].e.key, want)
			}
		}
	}
}
