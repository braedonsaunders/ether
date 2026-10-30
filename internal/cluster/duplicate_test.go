package cluster

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/store"
)

// This exercises the portable compare-and-swap SQL offline; PostgreSQL
// integration tests below also cover simultaneous processes and recovery.
func TestHeartbeatClaimCompareAndSwapSQLite(t *testing.T) {
	t.Setenv(store.TestPostgresEnv, "")
	db := store.OpenTest(t)
	id := newID()
	newNode := func(name string) *Postgres {
		p, err := NewPostgres(Options{DB: db, Local: Local{Identity: Identity{NodeID: id, Name: name}, Version: "test"}, Logger: discardLogger()})
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	a, b := newNode("one"), newNode("two")
	ctx := context.Background()
	claimA, err := a.checkDuplicate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	claimB, err := b.checkDuplicate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.beatClaim(ctx, &claimA); err != nil {
		t.Fatal(err)
	}
	if err := b.beatClaim(ctx, &claimB); !errors.Is(err, ErrDuplicateNode) {
		t.Fatalf("second absent-row claim: %v", err)
	}
	var seen int64
	if err := db.QueryRowContext(ctx, `SELECT last_seen FROM cluster_nodes WHERE id=?`, id).Scan(&seen); err != nil {
		t.Fatal(err)
	}
	stale := nodeClaim{incarnation: a.inc, lastSeen: seen, exists: true}
	if _, err := db.ExecContext(ctx, `UPDATE cluster_nodes SET last_seen=last_seen+1 WHERE id=?`, id); err != nil {
		t.Fatal(err)
	}
	if err := b.beatClaim(ctx, &stale); !errors.Is(err, ErrDuplicateNode) {
		t.Fatalf("changed-row claim: %v", err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE cluster_nodes SET incarnation=? WHERE id=?`, b.inc, id); err != nil {
		t.Fatal(err)
	}
	if err := a.beat(ctx); !errors.Is(err, ErrDuplicateNode) {
		t.Fatalf("old owner wrote: %v", err)
	}
}

// A second process started with a copied node.json refuses to join while
// the original is alive.
func TestPostgresRefusesDuplicateNodeAtStart(t *testing.T) {
	start := pgClusterStart(t)
	a, err := start("a", nil)
	if err != nil {
		t.Fatal(err)
	}
	clone, err := start("clone", func(o *Options) {
		o.Local.Identity = a.local.Identity
		o.IdentityPath = "/data/node.json"
	})
	if !errors.Is(err, ErrDuplicateNode) || !strings.Contains(err.Error(), "delete /data/node.json") {
		t.Fatalf("clone start: %v", err)
	}
	if clone.IsLeader() {
		t.Fatal("clone leads")
	}
	// The original keeps heartbeating under its own incarnation.
	time.Sleep(300 * time.Millisecond)
	var inc string
	if err := a.db.QueryRowContext(context.Background(), `SELECT incarnation FROM cluster_nodes WHERE id=?`, a.local.Identity.NodeID).Scan(&inc); err != nil || inc != a.inc {
		t.Fatalf("row incarnation %q (want %q) err=%v", inc, a.inc, err)
	}
	if a.Err() != nil {
		t.Fatalf("original failed: %v", a.Err())
	}
}

// A row left by this node's crashed previous process does not block a
// restart: it stops advancing, so the new process takes it over.
func TestPostgresTakesOverStaleOwnRow(t *testing.T) {
	start := pgClusterStart(t)
	a, err := start("a", nil)
	if err != nil {
		t.Fatal(err)
	}
	id := newID()
	now := time.Now().UnixMilli()
	if _, err := a.db.ExecContext(context.Background(),
		`INSERT INTO cluster_nodes(id,name,started_at,last_seen,incarnation) VALUES(?,?,?,?,?)`, id, "b", now, now, "crashed"); err != nil {
		t.Fatal(err)
	}
	b, err := start("b", func(o *Options) { o.Local.Identity = Identity{NodeID: id, Name: "b"} })
	if err != nil {
		t.Fatalf("restart after crash: %v", err)
	}
	waitFor(t, "b's row taken over", 3*time.Second, func() bool {
		var inc string
		_ = a.db.QueryRowContext(context.Background(), `SELECT incarnation FROM cluster_nodes WHERE id=?`, id).Scan(&inc)
		return inc == b.inc
	})
}

// Once joined, a foreign write to this node's row (a clone that got past
// the start check) ends this node's membership.
func TestPostgresDetectsHeartbeatConflict(t *testing.T) {
	start := pgClusterStart(t)
	var fatal atomic.Value
	a, err := start("a", func(o *Options) {
		o.OnFatal = func(err error) { fatal.Store(err) }
	})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "a leads", 5*time.Second, a.IsLeader)
	if _, err := a.db.ExecContext(context.Background(), `UPDATE cluster_nodes SET incarnation='other' WHERE id=?`, a.local.Identity.NodeID); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "conflict reported", 5*time.Second, func() bool { return fatal.Load() != nil })
	if err := fatal.Load().(error); !errors.Is(err, ErrDuplicateNode) || !errors.Is(a.Err(), ErrDuplicateNode) {
		t.Fatalf("fatal %v / %v", err, a.Err())
	}
	waitFor(t, "a steps down", 3*time.Second, func() bool { return !a.IsLeader() })
	// It no longer heartbeats: the other process keeps the row.
	time.Sleep(300 * time.Millisecond)
	var inc string
	if err := a.db.QueryRowContext(context.Background(), `SELECT incarnation FROM cluster_nodes WHERE id=?`, a.local.Identity.NodeID).Scan(&inc); err != nil || inc != "other" {
		t.Fatalf("row incarnation %q err=%v", inc, err)
	}
}

func TestPostgresConcurrentFirstJoinsOwnOnlyOneHeartbeat(t *testing.T) {
	build := pgClusterNew(t)
	id := newID()
	a := build("one", func(o *Options) { o.Local.Identity = Identity{NodeID: id, Name: "one"} })
	b := build("two", func(o *Options) { o.Local.Identity = Identity{NodeID: id, Name: "two"} })
	start := make(chan struct{})
	results := make(chan error, 2)
	for _, p := range []*Postgres{a, b} {
		go func(p *Postgres) { <-start; results <- p.Start(context.Background()) }(p)
	}
	close(start)
	successes, duplicates := 0, 0
	for n := 0; n < 2; n++ {
		err := <-results
		if err == nil {
			successes++
		} else if errors.Is(err, ErrDuplicateNode) {
			duplicates++
		} else {
			t.Fatalf("start: %v", err)
		}
	}
	if successes != 1 || duplicates != 1 {
		t.Fatalf("success=%d duplicate=%d", successes, duplicates)
	}
	var incarnation, version string
	if err := a.db.QueryRowContext(context.Background(), `SELECT incarnation,version FROM cluster_nodes WHERE id=?`, id).Scan(&incarnation, &version); err != nil {
		t.Fatal(err)
	}
	if (incarnation != a.inc && incarnation != b.inc) || version != "test" {
		t.Fatalf("owner=%q version=%q", incarnation, version)
	}
}

func TestPostgresWaitsForOwnedHeartbeatBeforeStarting(t *testing.T) {
	build := pgClusterNew(t)
	p := build("delayed", nil)
	lock, err := p.db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Rollback()
	if _, err = lock.ExecContext(context.Background(), `LOCK TABLE cluster_nodes IN ACCESS EXCLUSIVE MODE`); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- p.Start(context.Background()) }()
	select {
	case err := <-done:
		t.Fatalf("started without heartbeat: %v", err)
	case <-time.After(350 * time.Millisecond):
	}
	if p.IsLeader() {
		t.Fatal("led before owned heartbeat")
	}
	if err = lock.Commit(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("did not start after database recovery")
	}
	var incarnation, version string
	if err = p.db.QueryRowContext(context.Background(), `SELECT incarnation,version FROM cluster_nodes WHERE id=?`, p.local.Identity.NodeID).Scan(&incarnation, &version); err != nil || incarnation != p.inc || version != "test" {
		t.Fatalf("owner=%q version=%q err=%v", incarnation, version, err)
	}
}

func TestPostgresClaimRejectsChangedRowAndOldIncarnation(t *testing.T) {
	build := pgClusterNew(t)
	id := newID()
	a := build("a", func(o *Options) { o.Local.Identity = Identity{NodeID: id, Name: "a"} })
	b := build("b", func(o *Options) { o.Local.Identity = Identity{NodeID: id, Name: "b"} })
	ctx := context.Background()
	ac, err := a.checkDuplicate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	bc, err := b.checkDuplicate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err = a.beatClaim(ctx, &ac); err != nil {
		t.Fatal(err)
	}
	if err = b.beatClaim(ctx, &bc); !errors.Is(err, ErrDuplicateNode) {
		t.Fatalf("raced first claim: %v", err)
	}
	if _, err = a.db.ExecContext(ctx, `UPDATE cluster_nodes SET incarnation=? WHERE id=?`, b.inc, id); err != nil {
		t.Fatal(err)
	}
	if err = a.beat(ctx); !errors.Is(err, ErrDuplicateNode) {
		t.Fatalf("old heartbeat succeeded: %v", err)
	}
	var owner string
	if err = a.db.QueryRowContext(ctx, `SELECT incarnation FROM cluster_nodes WHERE id=?`, id).Scan(&owner); err != nil || owner != b.inc {
		t.Fatalf("owner=%q err=%v", owner, err)
	}
}

func TestPostgresRecoveryRefusesLiveDuplicate(t *testing.T) {
	build := pgClusterNew(t)
	a := build("original", nil)
	if err := a.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	clone := build("clone", func(o *Options) {
		o.Local.Identity = a.local.Identity
		o.HeartbeatInterval = 300 * time.Millisecond
		o.LiveAfter = 5 * time.Second
	})
	lock, err := clone.db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Rollback()
	if _, err = lock.ExecContext(context.Background(), `LOCK TABLE cluster_nodes IN ACCESS EXCLUSIVE MODE`); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- clone.Start(context.Background()) }()
	select {
	case err := <-done:
		t.Fatalf("joined while unavailable: %v", err)
	case <-time.After(350 * time.Millisecond):
	}
	if err = lock.Commit(); err != nil {
		t.Fatal(err)
	}
	select {
	case err = <-done:
		if !errors.Is(err, ErrDuplicateNode) {
			t.Fatalf("recovered duplicate: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("duplicate detection timed out")
	}
	var owner string
	if err = a.db.QueryRowContext(context.Background(), `SELECT incarnation FROM cluster_nodes WHERE id=?`, a.local.Identity.NodeID).Scan(&owner); err != nil || owner != a.inc {
		t.Fatalf("original lost ownership: %q %v", owner, err)
	}
}

func TestPostgresStartTimeoutCanRetryCleanly(t *testing.T) {
	build := pgClusterNew(t)
	p := build("retry", nil)
	lock, err := p.db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Rollback()
	if _, err = lock.ExecContext(context.Background(), `LOCK TABLE cluster_nodes IN ACCESS EXCLUSIVE MODE`); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 350*time.Millisecond)
	err = p.Start(ctx)
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("start timeout: %v", err)
	}
	if p.started || p.cancel != nil || p.IsLeader() {
		t.Fatal("failed start left membership or goroutines active")
	}
	if err = lock.Commit(); err != nil {
		t.Fatal(err)
	}
	var count int
	if err = p.db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM cluster_nodes WHERE id=?`, p.local.Identity.NodeID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("partial heartbeat=%d err=%v", count, err)
	}
	if err = p.Start(context.Background()); err != nil {
		t.Fatalf("retry after recovery: %v", err)
	}
	var owner string
	if err = p.db.QueryRowContext(context.Background(), `SELECT incarnation FROM cluster_nodes WHERE id=?`, p.local.Identity.NodeID).Scan(&owner); err != nil || owner != p.inc {
		t.Fatalf("recovered owner=%q err=%v", owner, err)
	}
}
