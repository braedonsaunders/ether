package reservations_test

import (
	"context"
	"testing"

	_ "github.com/braedonsaunders/ether/internal/auth"
	_ "github.com/braedonsaunders/ether/internal/identity"
	"github.com/braedonsaunders/ether/internal/store"
)

func TestRetiredReservationsMigrateIntoCommonHeaders(t *testing.T) {
	db := store.OpenTest(t)
	ctx := context.Background()
	// Reconstruct exactly the former component's v1 table and version marker.
	if _, err := db.ExecContext(ctx, `CREATE TABLE handle_reservations(namespace TEXT NOT NULL,digest TEXT NOT NULL,shard INTEGER NOT NULL,viewer_id BIGINT NOT NULL,surface TEXT NOT NULL,expires BIGINT NOT NULL,PRIMARY KEY(namespace,digest))`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO handle_reservations VALUES('retirement','aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa',1,65,'silo',12345)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `DELETE FROM schema_migrations WHERE component='handle_reservations' AND version=2`); err != nil {
		t.Fatal(err)
	}
	if err := store.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	var viewer, shard, expiry int64
	if err := db.QueryRowContext(ctx, `SELECT viewer_id,shard,expires FROM capability_headers WHERE namespace='retirement'`).Scan(&viewer, &shard, &expiry); err != nil || viewer != 65 || shard != 1 || expiry != 12345 {
		t.Fatalf("migrated %d %d %d: %v", viewer, shard, expiry, err)
	}
	if _, err := db.ExecContext(ctx, `SELECT 1 FROM handle_reservations`); err == nil {
		t.Fatal("retired store remains")
	}
}
