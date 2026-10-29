package main

import (
	"context"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/auth"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/livetv"
	"github.com/braedonsaunders/ether/internal/state"
	"github.com/braedonsaunders/ether/internal/store"
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
		st := state.New(db, state.Options{})
		w, err := st.Get(ctx, u.ID, core.MovieKey(10020))
		if err != nil || w.ViewOffset <= 0 {
			t.Fatalf("%s progress: %+v %v", token, w, err)
		}
		st.Close(ctx)
	}
}
