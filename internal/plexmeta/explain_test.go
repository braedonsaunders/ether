package plexmeta

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
)

func TestLookupUsesPrimaryKey(t *testing.T) {
	db := store.OpenTest(t)
	now := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	s := New(db, Options{Now: func() time.Time { return now }})
	t.Cleanup(s.Close)
	if err := s.store(context.Background(), core.MovieKey(550), movieGUID, Overlay{Summary: "x"}, now); err != nil {
		t.Fatal(err)
	}
	switch store.DialectOf(db) {
	case store.Postgres:
		rows, err := db.Query(`EXPLAIN SELECT guid,payload,fetched_at FROM plexmeta_titles WHERE rating_key=$1`, int64(core.MovieKey(550)))
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var plan strings.Builder
		for rows.Next() {
			var line string
			if err := rows.Scan(&line); err != nil {
				t.Fatal(err)
			}
			plan.WriteString(line)
			plan.WriteByte('\n')
		}
		got := plan.String()
		if !strings.Contains(got, "Index") && !strings.Contains(got, "index") {
			t.Fatalf("postgres plan did not use index:\n%s", got)
		}
	default:
		rows, err := db.Query(`EXPLAIN QUERY PLAN SELECT guid,payload,fetched_at FROM plexmeta_titles WHERE rating_key=?`, int64(core.MovieKey(550)))
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var plan strings.Builder
		for rows.Next() {
			var sel, order, from int
			var detail string
			if err := rows.Scan(&sel, &order, &from, &detail); err != nil {
				t.Fatal(err)
			}
			plan.WriteString(detail)
			plan.WriteByte('\n')
		}
		got := strings.ToLower(plan.String())
		if !strings.Contains(got, "primary key") && !strings.Contains(got, "search") {
			t.Fatalf("sqlite plan was not a key lookup:\n%s", plan.String())
		}
	}
}
