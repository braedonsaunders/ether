package capability_test

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	_ "github.com/braedonsaunders/ether/internal/auth"
	"github.com/braedonsaunders/ether/internal/core"
	. "github.com/braedonsaunders/ether/internal/handles/capability"
	_ "github.com/braedonsaunders/ether/internal/identity"
	"github.com/braedonsaunders/ether/internal/store"
)

// SQLite BEGIN IMMEDIATE blocks the replacement at transaction start; PG
// exercises the pre-lock read/reissue interleaving explicitly.
func TestTicketConsumeRechecksAfterReissue(t *testing.T) {
	if os.Getenv(store.TestPostgresEnv) == "" {
		t.Skip("requires PostgreSQL transaction interleaving")
	}
	db := store.OpenTest(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := db.ExecContext(ctx, `INSERT INTO auth_users(id,plex_id,created_at,updated_at) VALUES(1,1,0,0)`); err != nil {
		t.Fatal(err)
	}
	sourceA, sourceB := strings.Repeat("a", 64), strings.Repeat("b", 64)
	for _, source := range []string{sourceA, sourceB} {
		if _, err := db.ExecContext(ctx, `INSERT INTO identity_grants(hash,viewer_id,surface,device_id,client,product,version,created,last_used,expires) VALUES(?,1,'silo','tv','','','',0,0,9999999999999)`, source); err != nil {
			t.Fatal(err)
		}
	}
	issuer, _ := NewTickets(db)
	scope := TicketScope{ViewerID: 1, Surface: "silo", DeviceID: "tv", SourceHash: sourceA, DeviceJSON: `{"ID":"tv","Client":"old"}`}
	e := TicketEntry{Digest: strings.Repeat("c", 64), Purpose: "events-ws"}
	if _, err := issuer.IssueBatch(ctx, scope, []TicketEntry{e}); err != nil {
		t.Fatal(err)
	}
	seen := make(chan struct{})
	resume := make(chan struct{})
	consumer, _ := NewTickets(db)
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(resume) }) }
	SetBeforeConsumeLock(consumer, func() {
		close(seen)
		select {
		case <-resume:
		case <-ctx.Done():
		}
	})
	type answer struct {
		ticket Ticket
		err    error
	}
	done := make(chan answer, 1)
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		ticket, err := consumer.Consume(ctx, "silo", "events-ws", e.Digest)
		done <- answer{ticket, err}
	}()
	defer func() {
		release()
		cancel()
		select {
		case <-finished:
		case <-time.After(2 * time.Second):
			t.Error("consume worker did not exit after cancellation")
		}
	}()
	select {
	case <-seen:
	case <-ctx.Done():
		t.Fatal("initial read timed out")
	}
	if _, err := db.ExecContext(ctx, `DELETE FROM capability_headers WHERE namespace='ticket' AND digest=?`, e.Digest); err != nil {
		t.Fatal(err)
	}
	scope.SourceHash = sourceB
	scope.DeviceJSON = `{"ID":"tv","Client":"new"}`
	if _, err := issuer.IssueBatch(ctx, scope, []TicketEntry{e}); err != nil {
		t.Fatal(err)
	}
	release()
	select {
	case got := <-done:
		if got.err != nil || got.ticket.SourceHash != sourceB || got.ticket.DeviceJSON != scope.DeviceJSON {
			t.Fatalf("stale result: %+v err=%v", got.ticket, got.err)
		}
	case <-ctx.Done():
		t.Fatal("consume timed out")
	}
}

func TestMirroredRevokeBeforeDelete(t *testing.T) {
	if os.Getenv(store.TestPostgresEnv) == "" {
		t.Skip("requires PostgreSQL transaction interleaving")
	}
	db := store.OpenTest(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := db.ExecContext(ctx, `INSERT INTO auth_users(id,plex_id,created_at,updated_at) VALUES(1,1,0,0)`); err != nil {
		t.Fatal(err)
	}
	source := strings.Repeat("a", 64)
	if _, err := db.ExecContext(ctx, `INSERT INTO identity_grants(hash,viewer_id,surface,device_id,client,product,version,created,last_used,expires) VALUES(?,1,'silo','tv','','','',0,0,9999999999999)`, source); err != nil {
		t.Fatal(err)
	}
	issuer, _ := NewTickets(db)
	scope := TicketScope{ViewerID: 1, Surface: "silo", DeviceID: "tv", SourceHash: source, DeviceJSON: `{"ID":"tv"}`}
	entry := TicketEntry{Digest: strings.Repeat("c", 64), Purpose: "events-ws"}
	if _, err := issuer.IssueMirrored(ctx, scope, []TicketEntry{entry}); err != nil {
		t.Fatal(err)
	}
	seen, resume, finished := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(resume) }) }
	consumer, _ := NewTickets(db)
	SetBeforeConsumeLock(consumer, func() {
		close(seen)
		select {
		case <-resume:
		case <-ctx.Done():
		}
	})
	done := make(chan error, 1)
	go func() {
		defer close(finished)
		_, err := consumer.ConsumeMirrored(ctx, "silo", "events-ws", entry.Digest)
		done <- err
	}()
	defer func() {
		release()
		cancel()
		select {
		case <-finished:
		case <-time.After(2 * time.Second):
			t.Error("consume worker did not exit")
		}
	}()
	select {
	case <-seen:
	case <-ctx.Done():
		t.Fatal("pre-lock read timed out")
	}
	if _, err := db.ExecContext(ctx, `UPDATE identity_grants SET revoked=1 WHERE hash=?`, source); err != nil {
		t.Fatal(err)
	}
	release()
	select {
	case err := <-done:
		if !errors.Is(err, core.ErrUnauthorized) {
			t.Fatalf("revoked consume: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("consume timed out")
	}
	var count int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM frontend_tickets WHERE hash=?`, entry.Digest).Scan(&count); err != nil || count != 1 {
		t.Fatalf("revoked ticket count=%d err=%v", count, err)
	}
}

// PostgreSQL sees the old expired row while another transaction renews it;
// the outer expiry condition must be rechecked after the update commits.
func TestMirroredCleanupRechecksRenewedLegacyExpiry(t *testing.T) {
	if os.Getenv(store.TestPostgresEnv) == "" {
		t.Skip("requires PostgreSQL row version interleaving")
	}
	db := store.OpenTest(t)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	if _, err := db.ExecContext(ctx, `INSERT INTO auth_users(id,plex_id,created_at,updated_at) VALUES(1,1,0,0)`); err != nil {
		t.Fatal(err)
	}
	source := strings.Repeat("a", 64)
	if _, err := db.ExecContext(ctx, `INSERT INTO identity_grants(hash,viewer_id,surface,device_id,client,product,version,created,last_used,expires) VALUES(?,1,'silo','tv','','','',0,0,9999999999999)`, source); err != nil {
		t.Fatal(err)
	}
	oldHash := strings.Repeat("b", 64)
	if _, err := db.ExecContext(ctx, `INSERT INTO frontend_tickets(hash,surface,purpose,viewer_id,source_hash,device,expires) VALUES(?,?,?,?,?,?,?)`, oldHash, "silo", "events-ws", 1, source, `{"ID":"tv"}`, 1); err != nil {
		t.Fatal(err)
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `UPDATE frontend_tickets SET expires=? WHERE hash=?`, time.Now().Add(time.Hour).UnixMilli(), oldHash); err != nil {
		t.Fatal(err)
	}
	var holderPID int
	if err = tx.QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&holderPID); err != nil {
		t.Fatal(err)
	}
	engine, _ := NewTickets(db)
	entered := make(chan int, 1)
	SetBeforeLegacyCleanup(engine, func(cleanupTx *sql.Tx) {
		var pid int
		if e := cleanupTx.QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&pid); e != nil {
			entered <- 0
			return
		}
		entered <- pid
	})
	finished := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		defer close(finished)
		_, err := engine.IssueMirrored(ctx, TicketScope{ViewerID: 1, Surface: "silo", DeviceID: "tv", SourceHash: source, DeviceJSON: `{"ID":"tv"}`}, []TicketEntry{{Digest: strings.Repeat("c", 64), Purpose: "events-ws"}})
		done <- err
	}()
	defer func() {
		cancel()
		_ = tx.Rollback()
		select {
		case <-finished:
		case <-time.After(2 * time.Second):
			t.Error("issuer worker did not exit")
		}
	}()
	var cleanupPID int
	select {
	case cleanupPID = <-entered:
		if cleanupPID == 0 {
			t.Fatal("cleanup backend unavailable")
		}
	case <-ctx.Done():
		t.Fatal("cleanup did not start")
	}
	// Observe the actual owned backend blocked by our renewal transaction.
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	observed := false
	for !observed {
		select {
		case e := <-done:
			t.Fatalf("issuer returned before observed row wait: %v", e)
		case <-ctx.Done():
			t.Fatal("cleanup lock wait was not observed")
		case <-ticker.C:
			if e := db.QueryRowContext(ctx, `SELECT ? = ANY(pg_blocking_pids(?))`, holderPID, cleanupPID).Scan(&observed); e != nil {
				t.Fatal(e)
			}
		}
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	select {
	case err = <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("issuer timed out")
	}
	var count int
	if err = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM frontend_tickets WHERE hash=?`, oldHash).Scan(&count); err != nil || count != 1 {
		t.Fatalf("renewed ticket count=%d err=%v", count, err)
	}
}
