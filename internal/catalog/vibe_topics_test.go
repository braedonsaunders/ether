package catalog

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
)

// The candidate budget is an invariant: sparse overview terms must not walk
// beyond the popularity prefix, while keyword matches can reach the long tail.
func TestVibeTopicBoundedCandidates(t *testing.T) {
	ctx := context.Background()
	c := newChunkTestCatalog(t)
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 520; i++ {
		summary := "A space exploration mission."
		if i == 520 {
			summary = "A hidden nebula discovery."
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO catalog_titles(key,type,title,summary,popularity,browsable,adult,released_ts) VALUES(?,'movie',?,?,?,1,0,1)`, core.MovieKey(int64(i)), fmt.Sprintf("Fixture %d", i), summary, 1000-i)
		if err != nil {
			t.Fatal(err)
		}
	}
	for _, kw := range []struct {
		n    int
		word string
	}{{519, "time travel"}, {520, "space mission"}, {518, "heist film"}} {
		if _, err = tx.ExecContext(ctx, `INSERT INTO catalog_keywords(key,keyword) VALUES(?,?)`, core.MovieKey(int64(kw.n)), kw.word); err != nil {
			t.Fatal(err)
		}
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	check := func() {
		for _, tc := range []struct {
			topic string
			want  int
		}{{"space exploration", 513}, {"time travel", 1}, {"heist", 1}, {"hidden nebula", 0}} {
			match, args := c.topicMatch(tc.topic)
			var got int
			if err = c.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM catalog_titles t WHERE `+match, args...).Scan(&got); err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Errorf("%s: %d candidates, want %d", tc.topic, got, tc.want)
			}
		}
		// Assert the outer title lookup and overview lookup stay on primary keys;
		// keyword range/equality probes and popularity prefixes use existing indexes.
		match, args, err := c.browseWhere(core.Filter{Type: core.TypeMovie, Topic: "space exploration", LocalOnly: true})
		if err != nil {
			t.Fatal(err)
		}
		rows, err := c.db.QueryContext(ctx, `EXPLAIN QUERY PLAN SELECT t.key FROM catalog_titles t`+c.topicIndexHint(core.Filter{Topic: "space exploration"})+` WHERE `+match+` ORDER BY t.popularity DESC LIMIT 50`, args...)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		for rows.Next() {
			var id, parent, unused int
			var detail string
			if err = rows.Scan(&id, &parent, &unused, &detail); err != nil {
				t.Fatal(err)
			}
			if strings.Contains(detail, "SCAN t") || strings.Contains(detail, "SCAN catalog_titles") || strings.Contains(detail, "SCAN catalog_keywords") || (strings.Contains(detail, "SEARCH t USING") && !strings.Contains(detail, "INTEGER PRIMARY KEY")) {
				t.Errorf("unbounded plan: %s", detail)
			}
		}
		if err = rows.Err(); err != nil {
			t.Fatal(err)
		}
		page, err := c.Browse(ctx, core.BrowseQuery{Filter: core.Filter{Topic: "time travel", LocalOnly: true}, Sort: core.SortPopularity, Desc: true, Limit: 50, NoTotal: true})
		if err != nil || len(page.Items) != 1 || page.Items[0].Key != core.MovieKey(519) {
			t.Fatalf("topic browse: %+v %v", page, err)
		}
	}
	check()
	if _, err := c.RebuildRank(ctx); err != nil {
		t.Fatal(err)
	}
	check()
}
