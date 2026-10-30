package capability_test

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
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
	"github.com/braedonsaunders/ether/internal/identity"
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
	db, tickets, scope := fixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
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
	db, tickets, scope := fixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
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
	db, tickets, scope := fixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
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
	_, tickets, scope := fixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
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
	db, tickets, scope := fixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
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

// One expired orphan can free a saturated scope without evicting live rows.
func TestTicketOrphanCapacityRecoveryAtSaturation(t *testing.T) {
	db, tickets, scope := fixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	other := strings.Repeat("b", 64)
	if _, err := db.ExecContext(ctx, `INSERT INTO identity_grants(hash,viewer_id,surface,device_id,client,product,version,created,last_used,expires) VALUES(?,1,'silo','tv','','','',0,0,9999999999999)`, other); err != nil {
		t.Fatal(err)
	}
	if _, err := tickets.IssueBatch(ctx, scope, []capability.TicketEntry{entry(1)}); err != nil {
		t.Fatal(err)
	}
	scope.SourceHash = other
	for start := 2; start <= 128; start += 40 {
		end := min(start+40, 129)
		batch := make([]capability.TicketEntry, 0, end-start)
		for i := start; i < end; i++ {
			batch = append(batch, entry(i))
		}
		if _, err := tickets.IssueBatch(ctx, scope, batch); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.ExecContext(ctx, `DELETE FROM identity_grants WHERE hash=?`, strings.Repeat("a", 64)); err != nil {
		t.Fatal(err)
	}
	var capacity *capability.CapacityError
	if _, err := tickets.IssueBatch(ctx, scope, []capability.TicketEntry{entry(129)}); !errors.As(err, &capacity) || capacity.Scope != "viewer" {
		t.Fatalf("orphan should remain charged: %v", err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE capability_headers SET expires=0 WHERE namespace='ticket' AND digest=?`, entry(1).Digest); err != nil {
		t.Fatal(err)
	}
	if _, err := tickets.IssueBatch(ctx, scope, []capability.TicketEntry{entry(129)}); err != nil {
		t.Fatalf("expired orphan capacity not recovered: %v", err)
	}
	var total, live, orphan int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM capability_headers WHERE namespace='ticket'`).Scan(&total); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM capability_headers WHERE namespace='ticket' AND digest=?`, entry(2).Digest).Scan(&live); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM capability_headers WHERE namespace='ticket' AND digest=?`, entry(1).Digest).Scan(&orphan); err != nil {
		t.Fatal(err)
	}
	if total != 128 || live != 1 || orphan != 0 {
		t.Fatalf("capacity state total=%d live=%d orphan=%d", total, live, orphan)
	}
}

// Both dialects must preserve numeric scope and expiry through compound SELECT.
func TestTicketMultiBatchPreservesTypedScope(t *testing.T) {
	db, tickets, scope := fixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	expiry, err := tickets.IssueBatch(ctx, scope, []capability.TicketEntry{entry(1), entry(2)})
	if err != nil {
		t.Fatal(err)
	}
	rows, err := db.QueryContext(ctx, `SELECT viewer_id,shard,expires FROM capability_headers WHERE namespace='ticket' ORDER BY digest`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	count := 0
	for rows.Next() {
		var viewer, shard, gotExpiry int64
		if err = rows.Scan(&viewer, &shard, &gotExpiry); err != nil {
			t.Fatal(err)
		}
		if viewer != scope.ViewerID || shard != scope.ViewerID%64 || gotExpiry != expiry {
			t.Fatalf("viewer=%d shard=%d expiry=%d want=%d", viewer, shard, gotExpiry, expiry)
		}
		count++
	}
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("header count=%d", count)
	}
}

type activeFixture struct{}

func (activeFixture) ActiveUser(_ context.Context, id int64) (*core.User, error) {
	return &core.User{ID: id}, nil
}

// The unchanged legacy service remains the consume authority in this draft.
func TestMirroredTicketOldConsumerAndRollback(t *testing.T) {
	db, tickets, scope := fixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	grants, err := identity.NewGrants(db, activeFixture{}, time.Hour, nil)
	if err != nil {
		t.Fatal(err)
	}
	old, err := identity.NewTicketService(db, "silo", grants, nil)
	if err != nil {
		t.Fatal(err)
	}
	token := base64.RawURLEncoding.EncodeToString(make([]byte, 32))
	hash := sha256.Sum256([]byte(token))
	digest := hex.EncodeToString(hash[:])
	if _, err = tickets.IssueMirrored(ctx, scope, []capability.TicketEntry{{Digest: digest, Purpose: "events-ws"}}); err != nil {
		t.Fatal(err)
	}
	got, err := old.Consume(ctx, token, "events-ws")
	if err != nil || got.Viewer == nil || got.Viewer.ID != 1 || got.Device.ID != "tv" {
		t.Fatalf("old consume: %v", err)
	}
	if _, err = old.Consume(ctx, token, "events-ws"); !errors.Is(err, core.ErrUnauthorized) {
		t.Fatalf("old replay: %v", err)
	}
	// An old consumer leaves mirror metadata reserved until bounded expiry cleanup.
	var n int
	if err = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM capability_headers WHERE namespace='ticket' AND digest=?`, digest).Scan(&n); err != nil || n != 1 {
		t.Fatalf("mirror count=%d err=%v", n, err)
	}
	// Conflict only in legacy storage must roll back a newly staged mirror.
	conflict := entry(2).Digest
	if _, err = db.ExecContext(ctx, `INSERT INTO frontend_tickets(hash,surface,purpose,viewer_id,source_hash,device,expires) VALUES(?,?,?,?,?,?,?)`, conflict, "silo", "events-ws", 1, scope.SourceHash, scope.DeviceJSON, time.Now().Add(time.Minute).UnixMilli()); err != nil {
		t.Fatal(err)
	}
	if _, err = tickets.IssueMirrored(ctx, scope, []capability.TicketEntry{{Digest: conflict, Purpose: "events-ws"}}); err == nil {
		t.Fatal("legacy conflict succeeded")
	}
	if err = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM capability_headers WHERE namespace='ticket' AND digest=?`, conflict).Scan(&n); err != nil || n != 0 {
		t.Fatalf("partial mirror count=%d err=%v", n, err)
	}
}

func TestMirroredLegacyOnlyAndConcurrentMixedConsume(t *testing.T) {
	db, tickets, scope := fixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	grants, err := identity.NewGrants(db, activeFixture{}, time.Hour, nil)
	if err != nil {
		t.Fatal(err)
	}
	old, err := identity.NewTicketService(db, "silo", grants, nil)
	if err != nil {
		t.Fatal(err)
	}
	legacyToken := base64.RawURLEncoding.EncodeToString(make([]byte, 32))
	sum := sha256.Sum256([]byte(legacyToken))
	legacyDigest := hex.EncodeToString(sum[:])
	if _, err = db.ExecContext(ctx, `INSERT INTO frontend_tickets(hash,surface,purpose,viewer_id,source_hash,device,expires) VALUES(?,?,?,?,?,?,?)`, legacyDigest, "silo", "events-ws", 1, scope.SourceHash, scope.DeviceJSON, time.Now().Add(time.Minute).UnixMilli()); err != nil {
		t.Fatal(err)
	}
	if _, err = tickets.ConsumeMirrored(ctx, "silo", "events-ws", legacyDigest); err != nil {
		t.Fatalf("legacy-only consume: %v", err)
	}
	raw := make([]byte, 32)
	raw[0] = 1
	token := base64.RawURLEncoding.EncodeToString(raw)
	sum = sha256.Sum256([]byte(token))
	digest := hex.EncodeToString(sum[:])
	if _, err = tickets.IssueMirrored(ctx, scope, []capability.TicketEntry{{Digest: digest, Purpose: "events-ws"}}); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	out := make(chan error, 2)
	wg.Add(2)
	go func() { defer wg.Done(); _, e := old.Consume(ctx, token, "events-ws"); out <- e }()
	go func() { defer wg.Done(); _, e := tickets.ConsumeMirrored(ctx, "silo", "events-ws", digest); out <- e }()
	wg.Wait()
	close(out)
	success := 0
	for e := range out {
		if e == nil {
			success++
		} else if !errors.Is(e, core.ErrUnauthorized) {
			t.Fatalf("unexpected mixed consume error: %v", e)
		}
	}
	if success != 1 {
		t.Fatalf("successful mixed consumes=%d", success)
	}
}

func TestMirroredConsumeRejectsChangedDeviceWithoutDeletion(t *testing.T) {
	db, tickets, scope := fixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if _, err := tickets.IssueMirrored(ctx, scope, []capability.TicketEntry{entry(1)}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE frontend_tickets SET device=? WHERE hash=?`, `{"ID":"foreign"}`, entry(1).Digest); err != nil {
		t.Fatal(err)
	}
	if _, err := tickets.ConsumeMirrored(ctx, "silo", "events-ws", entry(1).Digest); !errors.Is(err, core.ErrUnauthorized) {
		t.Fatalf("changed device: %v", err)
	}
	var n int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM frontend_tickets WHERE hash=?`, entry(1).Digest).Scan(&n); err != nil || n != 1 {
		t.Fatalf("denied ticket count=%d err=%v", n, err)
	}
}

func TestMirroredConsumePreservesUnrelatedHeaderCollision(t *testing.T) {
	db, tickets, scope := fixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	other := strings.Repeat("b", 64)
	if _, err := db.ExecContext(ctx, `INSERT INTO identity_grants(hash,viewer_id,surface,device_id,client,product,version,created,last_used,expires) VALUES(?,1,'silo','tv','','','',0,0,9999999999999)`, other); err != nil {
		t.Fatal(err)
	}
	digest := entry(1).Digest
	if _, err := db.ExecContext(ctx, `INSERT INTO frontend_tickets(hash,surface,purpose,viewer_id,source_hash,device,expires) VALUES(?,?,?,?,?,?,?)`, digest, "silo", "events-ws", 1, scope.SourceHash, scope.DeviceJSON, time.Now().Add(time.Minute).UnixMilli()); err != nil {
		t.Fatal(err)
	}
	scope.SourceHash = other
	if _, err := tickets.IssueBatch(ctx, scope, []capability.TicketEntry{{Digest: digest, Purpose: "other"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := tickets.ConsumeMirrored(ctx, "silo", "events-ws", digest); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM capability_headers WHERE namespace='ticket' AND digest=?`, digest).Scan(&n); err != nil || n != 1 {
		t.Fatalf("unrelated header count=%d err=%v", n, err)
	}
	got, err := tickets.Consume(ctx, "silo", "other", digest)
	if err != nil || got.SourceHash != other {
		t.Fatalf("unrelated header unusable: %v", err)
	}
}

func TestMirroredHeaderCannotBypassLegacyAuthority(t *testing.T) {
	_, tickets, scope := fixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if _, err := tickets.IssueMirrored(ctx, scope, []capability.TicketEntry{entry(1)}); err != nil {
		t.Fatal(err)
	}
	if _, err := tickets.Consume(ctx, "silo", "events-ws", entry(1).Digest); !errors.Is(err, core.ErrUnauthorized) {
		t.Fatalf("mirror bypass: %v", err)
	}
	if _, err := tickets.ConsumeMirrored(ctx, "silo", "events-ws", entry(1).Digest); err != nil {
		t.Fatal(err)
	}
}

// The shared expiry is consumed by an unchanged legacy node at the same exact
// millisecond boundary; token bytes remain the existing canonical format.
func TestMirroredLegacyTokenAndExpiryBoundary(t *testing.T) {
	db, tickets, scope := fixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	raw := make([]byte, 32)
	raw[0] = 12
	token := base64.RawURLEncoding.EncodeToString(raw)
	sum := sha256.Sum256([]byte(token))
	digest := hex.EncodeToString(sum[:])
	expiry, err := tickets.IssueMirrored(ctx, scope, []capability.TicketEntry{{Digest: digest, Purpose: "events-ws"}})
	if err != nil {
		t.Fatal(err)
	}
	var legacyExpiry, headerExpiry int64
	if err = db.QueryRowContext(ctx, `SELECT expires FROM frontend_tickets WHERE hash=?`, digest).Scan(&legacyExpiry); err != nil {
		t.Fatal(err)
	}
	if err = db.QueryRowContext(ctx, `SELECT expires FROM capability_headers WHERE namespace='ticket' AND digest=?`, digest).Scan(&headerExpiry); err != nil {
		t.Fatal(err)
	}
	if legacyExpiry != expiry || headerExpiry != expiry {
		t.Fatalf("expiry result=%d legacy=%d header=%d", expiry, legacyExpiry, headerExpiry)
	}
	now := time.UnixMilli(expiry - 1)
	clock := func() time.Time { return now }
	grants, err := identity.NewGrants(db, activeFixture{}, time.Hour, clock)
	if err != nil {
		t.Fatal(err)
	}
	old, err := identity.NewTicketService(db, "silo", grants, clock)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = old.Consume(ctx, token, "events-ws"); err != nil {
		t.Fatalf("legacy consume immediately before expiry: %v", err)
	}
	raw[0] = 13
	token = base64.RawURLEncoding.EncodeToString(raw)
	sum = sha256.Sum256([]byte(token))
	digest = hex.EncodeToString(sum[:])
	expiry, err = tickets.IssueMirrored(ctx, scope, []capability.TicketEntry{{Digest: digest, Purpose: "events-ws"}})
	if err != nil {
		t.Fatal(err)
	}
	now = time.UnixMilli(expiry)
	if _, err = old.Consume(ctx, token, "events-ws"); !errors.Is(err, core.ErrUnauthorized) {
		t.Fatalf("legacy exact expiry: %v", err)
	}
}

func TestMirroredGrantExpiryDeniesWithoutConsume(t *testing.T) {
	db, tickets, scope := fixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if _, err := tickets.IssueMirrored(ctx, scope, []capability.TicketEntry{entry(1)}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE identity_grants SET expires=0 WHERE hash=?`, scope.SourceHash); err != nil {
		t.Fatal(err)
	}
	if _, err := tickets.ConsumeMirrored(ctx, "silo", "events-ws", entry(1).Digest); !errors.Is(err, core.ErrUnauthorized) {
		t.Fatalf("expired grant: %v", err)
	}
	var n int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM frontend_tickets WHERE hash=?`, entry(1).Digest).Scan(&n); err != nil || n != 1 {
		t.Fatalf("expired grant consumed ticket count=%d err=%v", n, err)
	}
	if _, err := tickets.IssueMirrored(ctx, scope, []capability.TicketEntry{entry(2)}); !errors.Is(err, core.ErrUnauthorized) {
		t.Fatalf("expired grant issue: %v", err)
	}
}

// Exercise a genuine legacy Issue rather than only inserting an equivalent row.
func TestMirroredConsumesActualLegacyIssue(t *testing.T) {
	db, tickets, _ := fixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	grants, err := identity.NewGrants(db, activeFixture{}, time.Hour, nil)
	if err != nil {
		t.Fatal(err)
	}
	device := core.Device{ID: "legacy-device", Client: "legacy"}
	credential, err := grants.Issue(ctx, 1, nil, "silo", device)
	if err != nil {
		t.Fatal(err)
	}
	old, err := identity.NewTicketService(db, "silo", grants, nil)
	if err != nil {
		t.Fatal(err)
	}
	token, err := old.Issue(ctx, credential, device, "events-ws")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := base64.RawURLEncoding.Strict().DecodeString(token)
	if err != nil || len(raw) != 32 || base64.RawURLEncoding.EncodeToString(raw) != token {
		t.Fatal("legacy token format changed")
	}
	sum := sha256.Sum256([]byte(token))
	digest := hex.EncodeToString(sum[:])
	got, err := tickets.ConsumeMirrored(ctx, "silo", "events-ws", digest)
	if err != nil || got.ViewerID != 1 {
		t.Fatalf("new consume of actual legacy issue: %v", err)
	}
	if _, err = old.Consume(ctx, token, "events-ws"); !errors.Is(err, core.ErrUnauthorized) {
		t.Fatalf("legacy replay: %v", err)
	}
}

// The legacy DELETE RETURNING and bounded expiry cleanup may race; neither
// may remove an unexpired ticket before its authorized old consumer uses it.
func TestMirroredCleanupConcurrentOldConsumer(t *testing.T) {
	db, tickets, scope := fixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	// This helper represents old binary SQL directly because the current service
	// may be upgraded; it keeps the old DELETE as the authority under test.
	raw := make([]byte, 32)
	raw[0] = 42
	token := base64.RawURLEncoding.EncodeToString(raw)
	sum := sha256.Sum256([]byte(token))
	digest := hex.EncodeToString(sum[:])
	if _, err = db.ExecContext(ctx, `INSERT INTO frontend_tickets(hash,surface,purpose,viewer_id,source_hash,device,expires) VALUES(?,?,?,?,?,?,?)`, digest, "silo", "events-ws", 1, scope.SourceHash, scope.DeviceJSON, time.Now().Add(time.Minute).UnixMilli()); err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	out := make(chan error, 2)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		var deleted string
		err := db.QueryRowContext(ctx, `DELETE FROM frontend_tickets WHERE hash=? AND surface=? AND purpose=? AND expires>? AND EXISTS(SELECT 1 FROM identity_grants g JOIN auth_users u ON u.id=g.viewer_id WHERE g.hash=frontend_tickets.source_hash AND g.viewer_id=frontend_tickets.viewer_id AND g.surface=frontend_tickets.surface AND g.revoked=0 AND g.expires>? AND u.active=1) RETURNING hash`, digest, "silo", "events-ws", time.Now().UnixMilli(), time.Now().UnixMilli()).Scan(&deleted)
		out <- err
	}()
	go func() {
		defer wg.Done()
		<-start
		_, err := tickets.IssueMirrored(ctx, scope, []capability.TicketEntry{entry(999)})
		out <- err
	}()
	close(start)
	wg.Wait()
	close(out)
	for err := range out {
		if err != nil {
			t.Fatal(err)
		}
	}
}
