package sessions

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"github.com/braedonsaunders/ether/internal/handles/capability"
	"sync"
	"testing"
	"time"

	_ "github.com/braedonsaunders/ether/internal/auth"
	_ "github.com/braedonsaunders/ether/internal/identity"
	"github.com/braedonsaunders/ether/internal/store"
)

// Deliberately prebind without common quota checks to independently test the DB fence.
func insertLazyRow(db *sql.DB, hash string, viewer int64) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	until := time.Now().Add(time.Hour).UnixMilli()
	if _, err = tx.Exec(`INSERT INTO capability_headers(namespace,digest,shard,viewer_id,surface,device_id,expires) VALUES('lazy-stream',?,?,?,'stremio','',?)`, hash, viewer%64, viewer, until); err != nil {
		return err
	}
	if _, err = tx.Exec(`INSERT INTO sessions_lazy_streams(hash,protocol,viewer_id,grant_reference,request,expires) VALUES(?,'stremio',?,'grant','{}',?)`, hash, viewer, until); err != nil {
		return err
	}
	return tx.Commit()
}
func retireLazyRows(t *testing.T, db *sql.DB, predicate string) {
	t.Helper()
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err = tx.Exec(`DELETE FROM capability_headers WHERE namespace='lazy-stream' AND digest IN (SELECT hash FROM sessions_lazy_streams WHERE ` + predicate + `)`); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(`DELETE FROM sessions_lazy_streams WHERE ` + predicate); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func checkLazyCounts(t *testing.T, db *sql.DB) {
	t.Helper()
	var actual, charged int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sessions_lazy_streams`).Scan(&actual); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT used FROM sessions_lazy_capacity WHERE id=1`).Scan(&charged); err != nil || charged != actual {
		t.Fatalf("total actual=%d charged=%d err=%v", actual, charged, err)
	}
	var wrong int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sessions_lazy_viewer_capacity c WHERE c.used<>(SELECT COUNT(*) FROM sessions_lazy_streams s WHERE s.viewer_id=c.viewer_id)`).Scan(&wrong); err != nil || wrong != 0 {
		t.Fatalf("incorrect viewer charges=%d err=%v", wrong, err)
	}
}

func TestLazyBoundsMixedWriterPressure(t *testing.T) {
	db := store.OpenTest(t)
	// Old writers without shared admission cannot create, renew or prune rows.
	if _, err := db.Exec(`INSERT INTO sessions_lazy_streams VALUES('old','m3u',1,'grant','{}',1)`); err == nil {
		t.Fatal("old insert accepted")
	}
	// Matching admitted headers still cannot bypass domain-wide quotas.
	for i := 0; i < maxLazyViewerCapabilities-4; i++ {
		if err := insertLazyRow(db, fmt.Sprint(i), 1); err != nil {
			t.Fatal(err)
		}
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	success := 0
	for i := 0; i < 24; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if insertLazyRow(db, fmt.Sprintf("concurrent-%d", i), 1) == nil {
				mu.Lock()
				success++
				mu.Unlock()
			}
		}(i)
	}
	wg.Wait()
	if success != 4 {
		t.Fatalf("admitted %d writers, want 4", success)
	}
	if _, err := db.Exec(`UPDATE sessions_lazy_streams SET viewer_id=2 WHERE hash='0'`); err == nil {
		t.Fatal("old scope-moving writer accepted")
	}
	if _, err := db.Exec(`UPDATE sessions_lazy_streams SET request=? WHERE hash='0'`, string(make([]byte, 65537))); err == nil {
		t.Fatal("oversized payload accepted")
	}
	if _, err := db.Exec(`INSERT INTO sessions_lazy_streams SELECT * FROM sessions_lazy_streams WHERE hash='0' ON CONFLICT(hash) DO NOTHING`); err != nil {
		t.Fatal(err)
	}
	checkLazyCounts(t, db)
	if _, err := db.Exec(`DELETE FROM sessions_lazy_streams WHERE hash='0'`); err == nil {
		t.Fatal("old delete accepted")
	}
	if _, err := db.Exec(`UPDATE sessions_lazy_streams SET expires=expires+1 WHERE hash='0'`); err == nil {
		t.Fatal("old renewal accepted")
	}
	retireLazyRows(t, db, "hash='0'")
	if err := insertLazyRow(db, "replacement", 1); err != nil {
		t.Fatal(err)
	}
	checkLazyCounts(t, db)
}

func TestLazyBoundsTotalAndRollback(t *testing.T) {
	db := store.OpenTest(t)
	tx, err := db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < maxLazyCapabilities; i++ {
		if _, err = tx.Exec(`INSERT INTO capability_headers(namespace,digest,shard,viewer_id,surface,device_id,expires) VALUES('lazy-stream',?,?,?,'stremio','',1)`, fmt.Sprint(i), (1+i/maxLazyViewerCapabilities)%64, 1+i/maxLazyViewerCapabilities); err != nil {
			t.Fatal(err)
		}
		if _, err = tx.Exec(`INSERT INTO sessions_lazy_streams(hash,protocol,viewer_id,grant_reference,request,expires) VALUES(?,'stremio',?,'grant','{}',1)`, fmt.Sprint(i), 1+i/maxLazyViewerCapabilities); err != nil {
			t.Fatal(err)
		}
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err = insertLazyRow(db, "overflow", 100); err == nil {
		t.Fatal("total cap bypassed")
	}
	checkLazyCounts(t, db)
	// Expired rows count until explicitly deleted, including writes by old nodes.
	retireLazyRows(t, db, "expires=1")
	checkLazyCounts(t, db)
	var n int
	if err = db.QueryRow(`SELECT COUNT(*) FROM sessions_lazy_viewer_capacity`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("viewer bookkeeping leaked: %d %v", n, err)
	}
}

func TestLazyBoundsActiveBindingsAndFlights(t *testing.T) {
	b := &Broker{sessions: make(map[string]*brokerSession)}
	for i := 0; i < maxLazyViewerBindings; i++ {
		key := fmt.Sprint(i)
		if !b.reserveLazyBinding(key, 1) {
			t.Fatalf("early refusal %d", i)
		}
		b.mu.Lock()
		b.sessions[key] = &brokerSession{}
		b.lazySessions[key] = lazySession{id: key, viewer: 1, expires: 1}
		b.mu.Unlock()
	}
	if b.reserveLazyBinding("overflow", 1) {
		t.Fatal("expired active binding evicted")
	}
	if !b.reserveLazyBinding("other", 2) {
		t.Fatal("other viewer refused")
	}
	b.releaseLazyBindingFlight("other")
	b.mu.Lock()
	delete(b.sessions, "0")
	b.mu.Unlock()
	if !b.reserveLazyBinding("replacement", 1) {
		t.Fatal("retired binding retained")
	}
	b.releaseLazyBindingFlight("replacement")
	if len(b.lazySessions) != maxLazyViewerBindings-1 {
		t.Fatal("failed open placeholder leaked")
	}
	// Unfinished opens are charged even though they have no session yet.
	if !b.reserveLazyBinding("flight", 1) || b.reserveLazyBinding("overflow", 1) {
		t.Fatal("flight not charged")
	}
	b.releaseLazyBindingFlight("flight")
}

func TestLazyBoundsMigrationPreservesExistingCapabilities(t *testing.T) {
	db := store.OpenTest(t)
	// Reconstruct the exact sessions/7 shape while retaining current other components.
	for _, name := range []string{"insert", "delete", "update"} {
		q := `DROP TRIGGER sessions_lazy_` + name
		if store.DialectOf(db) == store.Postgres {
			q += ` ON sessions_lazy_streams`
		}
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	if store.DialectOf(db) == store.Postgres {
		if _, err := db.Exec(`DROP FUNCTION sessions_lazy_account()`); err != nil {
			t.Fatal(err)
		}
	}
	for _, q := range []string{`DROP INDEX sessions_lazy_viewer`, `DELETE FROM capability_headers WHERE namespace='lazy-stream'`, `DROP TABLE sessions_lazy_capacity`, `DROP TABLE sessions_lazy_viewer_capacity`, `DELETE FROM schema_migrations WHERE component='sessions' AND version=8`} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	// An oversized old store may have live transfers. Migration must preserve them.
	for i := 0; i < maxLazyViewerCapabilities+1; i++ {
		if _, err := db.Exec(`INSERT INTO sessions_lazy_streams VALUES(?,'stremio',1,'grant','{}',?)`, fmt.Sprintf("%064x", i), time.Now().Add(time.Hour).UnixMilli()); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Migrate(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	checkLazyCounts(t, db)
	// Current consume/readmit semantics reject renewal of an in-use capability
	// when an oversized pre-upgrade store is charged. Preserve this policy proof.
	activeHash := fmt.Sprintf("%064x", 0)
	now := time.Now()
	b := &Broker{deps: BrokerDeps{Now: func() time.Time { return now }}, lazySessions: map[string]lazySession{activeHash: {id: "active", viewer: 1}}}
	streams := &lazyStreams{db: db, broker: b, protocol: "stremio", grants: &fakeLazyGrants{}, ttl: time.Hour}
	var capacity *capability.CapacityError
	if err := streams.renewActive(context.Background(), activeHash, "grant", 1, "active"); !errors.As(err, &capacity) {
		t.Fatalf("oversized active renewal: %v", err)
	}
	if err := insertLazyRow(db, "new", 1); err == nil {
		t.Fatal("grandfathered store grew")
	}
	retireLazyRows(t, db, fmt.Sprintf("hash IN ('%064x','%064x')", 0, 1))
	if err := insertLazyRow(db, "new", 1); err != nil {
		t.Fatal(err)
	}
	checkLazyCounts(t, db)
}
