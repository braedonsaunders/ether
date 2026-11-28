package clusterplayback

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/placement"
	"github.com/braedonsaunders/ether/internal/store"
)

func TestPutRouteSkipsNoOpWritesWhenUnchanged(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	db, err := store.Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	ledger, err := placement.New(db, placement.Options{MaxSessions: 16, MaxLease: time.Minute, LockTimeout: time.Second}, outputRetirementVerifier{})
	if err != nil {
		t.Fatal(err)
	}
	target := placement.Target{NodeID: "encoder", Boot: "boot"}
	if err := ledger.SetPool(ctx, "cpu", config.RoleTranscode, 2); err != nil {
		t.Fatal(err)
	}
	if err := ledger.SetTarget(ctx, target, config.NodeRoles{config.RoleTranscode}, map[config.NodeRole][]string{config.RoleTranscode: {"cpu"}}); err != nil {
		t.Fatal(err)
	}
	rec, err := ledger.Reserve(ctx, placement.Request{
		Session:    "steady-route",
		Operation:  "op-1",
		Controller: placement.Controller{ID: "controller", Boot: "boot"},
		Target:     target,
		Role:       config.RoleTranscode,
		Claims:     []placement.Claim{{Scope: "cpu", Units: 1}},
		Lease:      time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}

	r := &Runtime{o: Options{DB: db, Placement: ledger, ClockBoundKnown: true}}
	route := Route{Fence: rec.Fence, Viewer: 7, PlayID: "play-1"}
	if err := r.putRoute(ctx, route); err != nil {
		t.Fatal(err)
	}

	// Hold SQLite's single-writer lock on an independent connection. A steady-state
	// putRoute for an unchanged route and initialized coordinator must succeed via
	// read-only WAL lookups without issuing no-op INSERT ... ON CONFLICT writes.
	writerDB, err := sql.Open("sqlite", filepath.Join(dir, "ether.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer writerDB.Close()
	conn, err := writerDB.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		t.Fatal(err)
	}
	defer conn.ExecContext(ctx, `ROLLBACK`)

	shortCtx, cancel := context.WithTimeout(ctx, 150*time.Millisecond)
	defer cancel()
	if err := r.putRoute(shortCtx, route); err != nil {
		t.Fatalf("unchanged putRoute attempted a write under contended writer lock: %v", err)
	}

	stale := route
	stale.Viewer = 99
	if err := r.putRoute(shortCtx, stale); err != ErrDenied {
		t.Fatalf("conflicting same-generation putRoute got %v, want ErrDenied", err)
	}
}
