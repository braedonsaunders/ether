package catalog

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
)

func TestShareFilterBrowseSQLAndTotals(t *testing.T) {
	ctx := context.Background()
	db, err := store.OpenMemory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	c, err := New(db, nil, config.Catalog{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	released := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	titles := []core.Title{
		{Key: core.MovieKey(1), Type: core.TypeMovie, Title: "PG Marvel", ContentRating: "PG", CollectionName: "Marvel", Released: released, Year: 2020},
		{Key: core.MovieKey(2), Type: core.TypeMovie, Title: "R Marvel", ContentRating: "R", CollectionName: "Marvel", Released: released, Year: 2020},
		{Key: core.MovieKey(3), Type: core.TypeMovie, Title: "PG Other", ContentRating: "PG", CollectionName: "Other", Released: released, Year: 2020},
		{Key: core.MovieKey(4), Type: core.TypeMovie, Title: "PG none", ContentRating: "PG", Released: released, Year: 2020},
		{Key: core.MovieKey(5), Type: core.TypeMovie, Title: "R none", ContentRating: "R", Released: released, Year: 2020},
		{Key: core.MovieKey(6), Type: core.TypeMovie, Title: "NC-17", ContentRating: "NC-17", CollectionName: "Marvel", Released: released, Year: 2020},
	}
	for _, tt := range titles {
		if err := c.UpsertTitle(ctx, tt, true); err != nil {
			t.Fatal(err)
		}
	}

	f := core.Filter{
		Type:                   core.TypeMovie,
		MaxContentRating:       "PG-13",
		ExcludeContentRatings:  []string{"NC-17"},
		IncludeCollectionNames: []string{"Marvel"},
		ExcludeCollectionNames: []string{"Hidden"},
		MinContentRating:       "G",
	}
	where, args, err := c.browseWhere(f)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(where, "collection_name") || !strings.Contains(where, "content_rating_rank") || !strings.Contains(where, "content_rating NOT IN") {
		t.Fatalf("share predicates missing from SQL: %s", where)
	}

	if store.DialectOf(db) != store.Postgres {
		q := `EXPLAIN QUERY PLAN SELECT t.key FROM catalog_titles t WHERE ` + where
		rows, err := db.Query(q, args...)
		if err != nil {
			t.Fatal(err)
		}
		var plan string
		for rows.Next() {
			var id, parent, unused int
			var detail string
			if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
				t.Fatal(err)
			}
			plan += detail + "\n"
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		if plan == "" || strings.Contains(plan, "catalog_titles_show") {
			t.Fatalf("plan %s", plan)
		}
	}

	page, err := c.Browse(ctx, core.BrowseQuery{Filter: f, Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, item := range page.Items {
		got[item.Title] = true
	}
	if page.Total != 2 || !got["PG Marvel"] || !got["PG none"] || got["R Marvel"] || got["PG Other"] || got["R none"] || got["NC-17"] {
		t.Fatalf("sql totals %+v items=%v", page, got)
	}
}
