package state

import (
	"context"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
)

func TestHistoryTopAggregatesAndLifetime(t *testing.T) {
	s, _ := newTestState(t)
	ctx := context.Background()
	ep := core.EpisodeKey(23, 2, 1)
	movie := core.MovieKey(1)
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	// Raw history writes (including imports) must maintain both indices.
	for i, k := range []core.RatingKey{ep, ep, movie} {
		_, err := s.db.ExecContext(ctx, `INSERT INTO state_history(user_id,rating_key,viewed_at,completed,view_type) VALUES(?,?,?,?,0)`, 1, int64(k), at.Add(time.Duration(i)*time.Second).UnixNano(), 1)
		if err != nil {
			t.Fatal(err)
		}
	}
	for _, kind := range []core.KeyKind{core.KindEpisode, core.KindSeason, core.KindShow} {
		rows, err := s.HistoryTop(ctx, core.HistoryQuery{}, kind)
		if err != nil || len(rows) != 1 || rows[0].Count != 2 || len(rows[0].Users) != 1 {
			t.Fatalf("%d: %+v %v", kind, rows, err)
		}
	}
	since := at.Add(time.Second).Unix()
	rows, err := s.HistoryTop(ctx, core.HistoryQuery{Since: &since}, core.KindShow)
	if err != nil || len(rows) != 1 || rows[0].Count != 1 {
		t.Fatalf("time range %+v %v", rows, err)
	}
	hist, _ := s.History(ctx, 1, time.Time{}, 100)
	for _, h := range hist {
		if h.Key == ep {
			if err := s.DeleteHistory(ctx, 1, h.ID); err != nil {
				t.Fatal(err)
			}
		}
	}
	empty, err := s.HistoryTop(ctx, core.HistoryQuery{}, core.KindShow)
	if err != nil || len(empty) != 0 {
		t.Fatalf("deleted history still ranked %+v %v", empty, err)
	}
	life, err := s.LifetimeUsers(ctx, ep, core.HistoryQuery{})
	if err != nil || len(life) != 1 || life[0].Count != 2 {
		t.Fatalf("lifetime lost %+v %v", life, err)
	}
	// The maintained ranking index, rather than a temporary grouped catalog scan.
	plan, err := s.db.QueryContext(ctx, `EXPLAIN QUERY PLAN SELECT rating_key,history_views,last_history FROM state_history_rank WHERE user_id=0 AND kind=2 AND history_views>0 ORDER BY history_views DESC,last_history DESC,rating_key LIMIT 10`)
	if err != nil {
		t.Fatal(err)
	}
	defer plan.Close()
	for plan.Next() {
		var id, parent, unused int
		var detail string
		if err := plan.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		if detail == "USE TEMP B-TREE FOR ORDER BY" {
			t.Fatal("ranking index not used")
		}
	}
}

func TestHistoryOrderedMigrationAdoptsExistingColumns(t *testing.T) {
	s, _ := newTestState(t)
	if _, err := s.db.Exec(`DELETE FROM schema_migrations WHERE component='state' AND version=8`); err != nil {
		t.Fatal(err)
	}
	if err := store.Migrate(context.Background(), s.db); err != nil {
		t.Fatalf("already-present prerelease columns: %v", err)
	}
}
