package capability_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	_ "github.com/braedonsaunders/ether/internal/auth"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/handles/capability"
	_ "github.com/braedonsaunders/ether/internal/identity"
	"github.com/braedonsaunders/ether/internal/store"
)

func fixture(t *testing.T) (*sql.DB, *capability.Tickets, capability.TicketScope) {
	t.Helper()
	ctx := context.Background()
	var db *sql.DB
	if os.Getenv(store.TestPostgresEnv) != "" {
		db = store.OpenTest(t)
	} else {
		var err error
		db, err = store.OpenPath(ctx, filepath.Join(t.TempDir(), "tickets.db"))
		if err != nil {
			t.Fatal(err)
		}
		db.SetMaxOpenConns(8)
		t.Cleanup(func() { _ = db.Close() })
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO auth_users(id,plex_id,created_at,updated_at) VALUES(1,1,0,0)`); err != nil {
		t.Fatal(err)
	}
	source := strings.Repeat("a", 64)
	if _, err := db.ExecContext(ctx, `INSERT INTO identity_grants(hash,viewer_id,surface,device_id,client,product,version,created,last_used,expires) VALUES(?,1,'silo','tv','','','',0,0,9999999999999)`, source); err != nil {
		t.Fatal(err)
	}
	tickets, err := capability.NewTickets(db)
	if err != nil {
		t.Fatal(err)
	}
	return db, tickets, capability.TicketScope{ViewerID: 1, Surface: "silo", DeviceID: "tv", SourceHash: source, DeviceJSON: `{"ID":"tv"}`}
}
func entry(i int) capability.TicketEntry {
	return capability.TicketEntry{Digest: fmt.Sprintf("%064x", i), Purpose: "events-ws"}
}
func TestTicketBatchScopeRevocationAndReplay(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	db, tickets, scope := fixture(t)
	expiry, err := tickets.IssueBatch(ctx, scope, []capability.TicketEntry{entry(1), entry(2)})
	if err != nil || expiry <= 0 {
		t.Fatalf("issue expiry=%d err=%v", expiry, err)
	}
	if _, err = tickets.Consume(ctx, "emby", "events-ws", entry(1).Digest); !errors.Is(err, core.ErrUnauthorized) {
		t.Fatalf("foreign scope: %v", err)
	}
	if _, err = tickets.Consume(ctx, "silo", "other", entry(1).Digest); !errors.Is(err, core.ErrUnauthorized) {
		t.Fatalf("foreign purpose: %v", err)
	}
	got, err := tickets.Consume(ctx, "silo", "events-ws", entry(1).Digest)
	if err != nil || got.ViewerID != 1 || got.SourceHash != scope.SourceHash {
		t.Fatalf("consume: %+v %v", got, err)
	}
	if _, err = tickets.Consume(ctx, "silo", "events-ws", entry(1).Digest); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("replay: %v", err)
	}
	if _, err = db.ExecContext(ctx, `UPDATE identity_grants SET revoked=1 WHERE hash=?`, scope.SourceHash); err != nil {
		t.Fatal(err)
	}
	if _, err = tickets.Consume(ctx, "silo", "events-ws", entry(2).Digest); !errors.Is(err, core.ErrUnauthorized) {
		t.Fatalf("revoked: %v", err)
	}
	if _, err = tickets.IssueBatch(ctx, scope, []capability.TicketEntry{entry(3)}); !errors.Is(err, core.ErrUnauthorized) {
		t.Fatalf("revoked issue: %v", err)
	}
}
func TestTicketBatchRollbackAndConcurrentConsume(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	db, tickets, scope := fixture(t)
	if _, err := tickets.IssueBatch(ctx, scope, []capability.TicketEntry{entry(1), entry(1)}); !errors.Is(err, core.ErrUnauthorized) {
		t.Fatalf("duplicate: %v", err)
	}
	if _, err := tickets.IssueBatch(ctx, scope, []capability.TicketEntry{entry(2), entry(3)}); err != nil {
		t.Fatal(err)
	}
	if _, err := tickets.IssueBatch(ctx, scope, []capability.TicketEntry{entry(4), entry(2)}); err == nil {
		t.Fatal("conflict succeeded")
	}
	var n int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM capability_headers WHERE digest=?`, entry(4).Digest).Scan(&n); err != nil || n != 0 {
		t.Fatalf("partial batch count=%d err=%v", n, err)
	}
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := tickets.Consume(ctx, "silo", "events-ws", entry(2).Digest)
			results <- err
		}()
	}
	wg.Wait()
	close(results)
	success := 0
	for err := range results {
		if err == nil {
			success++
		} else if !errors.Is(err, sql.ErrNoRows) && !errors.Is(err, core.ErrUnauthorized) {
			t.Fatal(err)
		}
	}
	if success != 1 {
		t.Fatalf("successful consumes=%d", success)
	}
}

func TestTicketBatchBoundsAndExpiry(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	db, tickets, scope := fixture(t)
	tooMany := make([]capability.TicketEntry, 41)
	for i := range tooMany {
		tooMany[i] = entry(i + 1)
	}
	if _, err := tickets.IssueBatch(ctx, scope, tooMany); !errors.Is(err, core.ErrUnauthorized) {
		t.Fatalf("batch bound: %v", err)
	}
	if _, err := tickets.IssueBatch(ctx, scope, []capability.TicketEntry{entry(1)}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE capability_headers SET expires=0 WHERE namespace='ticket' AND digest=?`, entry(1).Digest); err != nil {
		t.Fatal(err)
	}
	if _, err := tickets.Consume(ctx, "silo", "events-ws", entry(1).Digest); !errors.Is(err, core.ErrUnauthorized) {
		t.Fatalf("expired consume: %v", err)
	}
	if _, err := tickets.IssueBatch(ctx, scope, []capability.TicketEntry{entry(2)}); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM capability_headers WHERE digest=?`, entry(1).Digest).Scan(&n); err != nil || n != 0 {
		t.Fatalf("expired cleanup n=%d err=%v", n, err)
	}
}

func TestTicketRejectsForeignDeviceJSON(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, tickets, scope := fixture(t)
	for _, bad := range []string{`{`, `{"ID":"other"}`, `null`} {
		scope.DeviceJSON = bad
		if _, err := tickets.IssueBatch(ctx, scope, []capability.TicketEntry{entry(1)}); !errors.Is(err, core.ErrUnauthorized) {
			t.Fatalf("device JSON %q: %v", bad, err)
		}
	}
}

// Deleting the source cascades only the typed child. The header stays charged
// until bounded expiry cleanup; a live header must not be evicted.
func TestTicketDeletedGrantOrphanQuotaRecovery(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	db, tickets, scope := fixture(t)
	other := strings.Repeat("b", 64)
	if _, err := db.ExecContext(ctx, `INSERT INTO identity_grants(hash,viewer_id,surface,device_id,client,product,version,created,last_used,expires) VALUES(?,1,'silo','tv','','','',0,0,9999999999999)`, other); err != nil {
		t.Fatal(err)
	}
	if _, err := tickets.IssueBatch(ctx, scope, []capability.TicketEntry{entry(1)}); err != nil {
		t.Fatal(err)
	}
	scope.SourceHash = other
	if _, err := tickets.IssueBatch(ctx, scope, []capability.TicketEntry{entry(2)}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `DELETE FROM identity_grants WHERE hash=?`, strings.Repeat("a", 64)); err != nil {
		t.Fatal(err)
	}
	if _, err := tickets.Consume(ctx, "silo", "events-ws", entry(1).Digest); !errors.Is(err, core.ErrUnauthorized) {
		t.Fatalf("orphan consume: %v", err)
	}
	var headers, children int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM capability_headers WHERE namespace='ticket'`).Scan(&headers); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM capability_tickets WHERE namespace='ticket'`).Scan(&children); err != nil {
		t.Fatal(err)
	}
	if headers != 2 || children != 1 {
		t.Fatalf("source cascade headers=%d children=%d", headers, children)
	}
	if _, err := db.ExecContext(ctx, `UPDATE capability_headers SET expires=0 WHERE namespace='ticket' AND digest=?`, entry(1).Digest); err != nil {
		t.Fatal(err)
	}
	if _, err := tickets.IssueBatch(ctx, scope, []capability.TicketEntry{entry(3)}); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM capability_headers WHERE namespace='ticket' AND digest=?`, entry(1).Digest).Scan(&headers); err != nil || headers != 0 {
		t.Fatalf("expired orphan retained=%d err=%v", headers, err)
	}
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM capability_headers WHERE namespace='ticket' AND digest=?`, entry(2).Digest).Scan(&headers); err != nil || headers != 1 {
		t.Fatalf("live header count=%d err=%v", headers, err)
	}
}
