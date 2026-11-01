package catalog_test

import (
	"context"
	"log/slog"
	"strconv"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/catalog"
	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/store"
	"github.com/braedonsaunders/ether/internal/tmdb"
)

// TestRunResumesRecordedSchedule: a new leader's Run (WithResumeSchedule)
// waits out the refresh interval since the last sync any node recorded
// instead of syncing at once; without a recorded sync it syncs at once.
func TestRunResumesRecordedSchedule(t *testing.T) {
	ctx := context.Background()
	srv := fakeTMDB(t)
	db, err := store.OpenMemory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	client := tmdb.New("testkey", slog.Default(), tmdb.WithBaseURL(srv.URL), tmdb.WithCacheTTL(0))
	t.Cleanup(client.Close)
	cfg := config.Catalog{Language: "en-US", Region: "US", MinPopularity: 5, MinVotes: 100, RefreshInterval: config.Duration(2 * time.Second)}
	open := func() *catalog.Catalog {
		c, err := catalog.New(db, client, cfg, slog.Default(), catalog.WithResumeSchedule())
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	started := func(c *catalog.Catalog) bool { return !c.SyncProgress().StartedAt.IsZero() }
	run := func(c *catalog.Catalog) func() {
		rctx, cancel := context.WithCancel(ctx)
		done := make(chan struct{})
		go func() { c.Run(rctx); close(done) }()
		return func() { cancel(); <-done }
	}

	// First leader, nothing recorded: syncs at once.
	first := open()
	stop := run(first)
	deadline := time.Now().Add(5 * time.Second)
	for !started(first) || first.SyncProgress().Running {
		if time.Now().After(deadline) {
			t.Fatal("first run never synced")
		}
		time.Sleep(5 * time.Millisecond)
	}
	stop()

	// Leadership moves: the next leader does not sync straight away...
	next := open()
	stop = run(next)
	defer stop()
	time.Sleep(300 * time.Millisecond)
	if started(next) {
		t.Fatal("new leader synced immediately despite a recent cluster-wide sync")
	}
	// ...but does once the interval since the recorded sync has passed.
	deadline = time.Now().Add(5 * time.Second)
	for !started(next) {
		if time.Now().After(deadline) {
			t.Fatal("new leader never synced after the interval")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Without WithResumeSchedule (a standalone server) Run syncs on start even
// when a sync was recorded recently, as before.
func TestRunSyncsAtStartByDefault(t *testing.T) {
	c := newTestCatalog(t, fakeTMDB(t))
	if err := c.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	before := c.SyncProgress().StartedAt
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { c.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()
	deadline := time.Now().Add(5 * time.Second)
	for !c.SyncProgress().StartedAt.After(before) {
		if time.Now().After(deadline) {
			t.Fatal("Run did not sync at start")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestRunBoundsFutureRecordedSyncAfterClockCorrection(t *testing.T) {
	db := store.OpenTest(t)
	_, err := db.Exec(`INSERT INTO catalog_meta(k,v) VALUES('last_sync',?)`, strconv.FormatInt(time.Now().Add(72*time.Hour).Unix(), 10))
	if err != nil {
		t.Fatal(err)
	}
	srv := fakeTMDB(t)
	client := tmdb.New("testkey", slog.Default(), tmdb.WithBaseURL(srv.URL))
	t.Cleanup(client.Close)
	cfg := config.Catalog{RefreshInterval: config.Duration(40 * time.Millisecond)}
	c, err := catalog.New(db, client, cfg, slog.Default(), catalog.WithResumeSchedule())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { c.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()
	deadline := time.Now().Add(2 * time.Second)
	for c.SyncProgress().StartedAt.IsZero() {
		if time.Now().After(deadline) {
			t.Fatal("future timestamp postponed sync after clock correction")
		}
		time.Sleep(5 * time.Millisecond)
	}
}
