package catalog

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
)

// Art, current viewer policy and exclusions must precede LIMIT; neither a cold
// chart nor a long prefix of artless titles can trigger remote or linear work.
func TestHeroCandidateSelection(t *testing.T) {
	ctx := context.Background()
	db, err := store.OpenMemory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	now := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	c, err := New(db, nil, config.Catalog{}, nil, WithNow(func() time.Time { return now }))
	if err != nil {
		t.Fatal(err)
	}
	var titles []core.Title
	for n := 1; n <= 30; n++ {
		item := core.Title{Key: core.MovieKey(int64(n)), Type: core.TypeMovie, Title: fmt.Sprint(n), Released: now.Add(-time.Duration(n) * 24 * time.Hour), ContentRating: "PG", Genres: []string{"Drama"}}
		if n > 20 {
			item.BackdropPath = "/backdrop.jpg"
		}
		if n == 21 {
			item.ContentRating = "R"
		}
		titles = append(titles, item)
	}
	if err := c.upsertSummaries(ctx, titles, true); err != nil {
		t.Fatal(err)
	}
	filter := core.Filter{Type: core.TypeMovie, MaxContentRating: "PG", KnownAddedDate: true, ExcludeKeys: []core.RatingKey{core.MovieKey(22)}}
	items, err := c.HeroCandidates(ctx, filter, 6, true)
	if err != nil || len(items) != 0 {
		t.Fatal("cold chart must stay local and empty", items, err)
	}
	items, err = c.HeroCandidates(ctx, filter, 6, false)
	if err != nil || len(items) != 6 {
		t.Fatal("eligible titles lost before limit", len(items), err)
	}
	for n, item := range items {
		if item.Key != core.MovieKey(int64(n+23)) || item.BackdropPath == "" || len(item.Genres) != 1 {
			t.Fatal("art/policy/order/facts lost", item)
		}
	}
	chart := []core.Title{titles[28], titles[20], titles[21], titles[23]}
	raw, _ := json.Marshal(chart)
	if _, err := db.ExecContext(ctx, `INSERT INTO catalog_trends(name,media_type,items,fetched_at) VALUES('trending_week','movie',?,?)`, string(raw), now.Unix()); err != nil {
		t.Fatal(err)
	}
	items, err = c.HeroCandidates(ctx, filter, 2, true)
	if err != nil || len(items) != 2 || items[0].Key != core.MovieKey(29) || items[1].Key != core.MovieKey(24) {
		t.Fatal("chart provenance/order lost", items, err)
	}
	plan, err := db.QueryContext(ctx, `EXPLAIN QUERY PLAN SELECT key FROM catalog_titles INDEXED BY catalog_hero_recent WHERE type='movie' AND backdrop<>'' AND browsable=1 AND adult=0 AND release_known=1 ORDER BY released_ts DESC,key ASC LIMIT 6`)
	if err != nil {
		t.Fatal(err)
	}
	defer plan.Close()
	var detail string
	for plan.Next() {
		var a, b, d int
		var text string
		if err := plan.Scan(&a, &b, &d, &text); err != nil {
			t.Fatal(err)
		}
		detail += text
	}
	if !strings.Contains(detail, "SEARCH") || !strings.Contains(detail, "catalog_hero_recent") || strings.Contains(detail, "TEMP B-TREE") {
		t.Fatal("recent fill lost indexed order", detail)
	}
}
