package viewerpolicy

import (
	"context"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/store"
)

func TestAvailabilitySyncChurnDoesNotMoveEpoch(t *testing.T) {
	db := store.OpenTest(t)
	ctx := context.Background()
	// Minimal observation schema also verifies migrations in partial binaries.
	_, err := db.Exec(`CREATE TABLE availability_releases(rating_key INTEGER,source_id TEXT,generation TEXT,payload TEXT,state TEXT,first_seen INTEGER,revision INTEGER,last_seen INTEGER,attempts INTEGER); CREATE TABLE resolver_availability(rating_key INTEGER PRIMARY KEY,available INTEGER,checked_at INTEGER,expires_at INTEGER); DELETE FROM schema_migrations WHERE component='viewer_policy' AND version=2`)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	if _, err := db.Exec(`INSERT INTO availability_releases VALUES(1,'feed','one','{}','observed',?,0,?,0)`, now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO resolver_availability VALUES(1,1,?,?)`, now, now+60); err != nil {
		t.Fatal(err)
	}
	before, err := Read(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20; i++ {
		if _, err := db.Exec(`UPDATE availability_releases SET last_seen=?,attempts=attempts+1`, now+int64(i)); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`UPDATE resolver_availability SET checked_at=?,expires_at=? WHERE rating_key=1`, now+int64(i), now+int64(i)+60); err != nil {
			t.Fatal(err)
		}
	}
	after, err := Read(ctx, db)
	if err != nil || before != after {
		t.Fatalf("sync moved epoch %d -> %d %v", before, after, err)
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := Fence(ctx, db, tx, before); err != nil {
		t.Fatalf("unchanged issuance fenced: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE availability_releases SET state='unavailable'`); err != nil {
		t.Fatal(err)
	}
	after, err = Read(ctx, db)
	if err != nil || after <= before {
		t.Fatalf("denial failed to move epoch %d -> %d %v", before, after, err)
	}
}
