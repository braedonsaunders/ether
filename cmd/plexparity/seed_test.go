package main

import (
	"context"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/auth"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/livetv"
	"github.com/braedonsaunders/ether/internal/state"
	"github.com/braedonsaunders/ether/internal/store"
	"github.com/braedonsaunders/ether/internal/watchlist"
)

func TestSeed(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	if err := seed(ctx, dir); err != nil {
		t.Fatal(err)
	}
	if err := seed(ctx, dir); err == nil {
		t.Fatal("seeding existing database must fail")
	}
	db, err := store.Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	settings, err := watchlist.New(db, watchlist.Options{}).Settings(ctx)
	if err != nil || !settings.Enabled || settings.Plex || settings.Trakt {
		t.Fatalf("offline watchlist settings: %+v %v", settings, err)
	}
	a, err := auth.New(ctx, db, auth.Options{})
	if err != nil {
		t.Fatal(err)
	}
	tv, err := livetv.New(livetv.Options{DB: db, Now: func() time.Time { return fixtureNow }})
	if err != nil {
		t.Fatal(err)
	}
	defer tv.Close()
	programs, err := tv.Guide(ctx, &core.User{ID: 1, IsOwner: true}, fixtureNow, fixtureNow.Add(time.Hour))
	if err != nil || len(programs) == 0 {
		t.Fatalf("live TV fixture guide empty: %v %v", programs, err)
	}
	for _, table := range []string{"catalog_titles", "state_watch", "state_history", "state_playlists", "watchlist_items", "trackprefs_rules", "personalize_home_layout", "personalize_row_impressions"} {
		var n int
		if err := db.QueryRowContext(ctx, "SELECT count(*) FROM "+table).Scan(&n); err != nil || n == 0 {
			t.Fatalf("%s empty: count=%d err=%v", table, n, err)
		}
	}
	for _, token := range []string{"parity-owner", "parity-adult", "parity-teen", "parity-child", "parity-friend"} {
		u, err := a.Authenticate(ctx, token)
		if err != nil || u == nil {
			t.Fatalf("%s: user=%v err=%v", token, u, err)
		}
		if u.Username == "teen" || u.Username == "child" {
			var count int
			if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM watchlist_items WHERE user_id=?`, u.ID).Scan(&count); err != nil || count < 30 {
				t.Fatalf("%s watchlist pool count=%d err=%v", token, count, err)
			}
		}
		var attributed int
		if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM personalize_plays WHERE user_id=? AND hub_id<>'' AND kind<>'' AND max_fraction>=0.3`, u.ID).Scan(&attributed); err != nil || attributed == 0 {
			t.Fatalf("%s attributed personalization plays=%d err=%v", token, attributed, err)
		}
		st := state.New(db, state.Options{})
		w, err := st.Get(ctx, u.ID, core.MovieKey(10020))
		if err != nil || w.ViewOffset <= 0 {
			t.Fatalf("%s progress: %+v %v", token, w, err)
		}
		st.Close(ctx)
	}
}

func TestSeedAttributionIsDeterministic(t *testing.T) {
	ctx := context.Background()
	read := func(dir string) []string {
		t.Helper()
		if err := seed(ctx, dir); err != nil {
			t.Fatal(err)
		}
		db, err := store.Open(ctx, dir)
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		rows, err := db.QueryContext(ctx, `SELECT user_id,item_key,hub_id,kind,max_fraction FROM personalize_plays ORDER BY user_id,item_key,hub_id`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var result []string
		for rows.Next() {
			var user, key int64
			var hub, kind string
			var fraction float64
			if err := rows.Scan(&user, &key, &hub, &kind, &fraction); err != nil {
				t.Fatal(err)
			}
			result = append(result, fmt.Sprintf("%d/%d/%s/%s/%.3f", user, key, hub, kind, fraction))
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		return result
	}
	a, b := read(t.TempDir()), read(t.TempDir())
	if len(a) != 5 || !reflect.DeepEqual(a, b) {
		t.Fatalf("seeded play attribution differs: %v vs %v", a, b)
	}
}
