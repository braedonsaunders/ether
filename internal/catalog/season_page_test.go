package catalog

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
	"log/slog"
)

func TestSeasonEpisodePageUsesTheShowIndex(t *testing.T) {
	db := store.OpenTest(t)
	now := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	show := int64(7)
	showKey := core.ShowKey(show)
	past, future := now.Add(-24*time.Hour).Unix(), now.Add(24*time.Hour).Unix()
	for ep := 1; ep <= 120; ep++ {
		_, err := db.Exec(`INSERT INTO catalog_titles(key,type,show_key,show_tmdb,season,episode,title,released_ts,adult,detail,detail_at) VALUES(?,'episode',?,?,1,?,?,?,0,1,?)`, core.EpisodeKey(show, 1, ep), showKey, show, ep, strconv.Itoa(ep), past, now.Unix())
		if err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`INSERT INTO catalog_titles(key,type,show_key,show_tmdb,season,episode,title,released_ts,adult,detail,detail_at) VALUES(?,'episode',?,?,1,121,'tomorrow',?,0,1,?)`, core.EpisodeKey(show, 1, 121), showKey, show, future, now.Unix()); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO catalog_titles(key,type,show_key,show_tmdb,season,episode,title,released_ts,adult,detail,detail_at) VALUES(?,'episode',?,?,1,999,'adult',?,1,1,?)`, core.EpisodeKey(show, 1, 999), showKey, show, past, now.Unix()); err != nil {
		t.Fatal(err)
	}
	if store.DialectOf(db) != store.Postgres {
		rows, err := db.Query(`EXPLAIN QUERY PLAN SELECT key FROM catalog_titles INDEXED BY catalog_titles_show WHERE type='episode' AND show_key=? AND season=? AND adult=0 AND release_known=1 AND released_ts<=? ORDER BY episode ASC LIMIT ? OFFSET ?`, int64(showKey), 1, now.Unix(), 50, 0)
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
		if !strings.Contains(plan, "catalog_titles_show") || strings.Contains(plan, "TEMP B-TREE") || strings.Contains(plan, "SCAN catalog_titles") {
			t.Fatalf("plan %s", plan)
		}
	}
	c, err := New(db, nil, config.Catalog{}, slog.New(slog.DiscardHandler), WithNow(func() time.Time { return now }))
	if err != nil {
		t.Fatal(err)
	}
	first, err := c.SeasonEpisodePage(context.Background(), showKey, 1, 0, 50)
	if err != nil || first.Total != 120 || len(first.Items) != 50 || first.Items[0].Episode != 1 || first.Items[49].Episode != 50 {
		t.Fatalf("first %+v %v", first, err)
	}
	last, err := c.SeasonEpisodePage(context.Background(), showKey, 1, 100, 50)
	if err != nil || last.Total != 120 || len(last.Items) != 20 || last.Items[0].Episode != 101 || last.Items[19].Episode != 120 {
		t.Fatalf("last %+v %v", last, err)
	}
}
