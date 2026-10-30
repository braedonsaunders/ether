package app

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/store"
)

func TestPrismUpgradePostgresTransitionGate(t *testing.T) {
	dsn := os.Getenv(store.TestPostgresEnv)
	if dsn == "" {
		t.Skip("ETHER_TEST_POSTGRES is not configured")
	}
	ctx := context.Background()
	now := time.Now()
	for _, oldFirst := range []bool{true, false} {
		name := "old starts after migration"
		if oldFirst {
			name = "old is live before migration"
		}
		t.Run(name, func(t *testing.T) {
			db := newPostgresSchema(t, dsn)
			applyDelivered9503(t, db, store.Postgres)
			seedDeliveredRows(t, db)
			var schema string
			if err := db.QueryRow(`SELECT current_schema()`).Scan(&schema); err != nil {
				t.Fatal(err)
			}
			loc, err := postgresSearchPath(dsn, schema)
			if err != nil {
				t.Fatal(err)
			}
			gate := startupMigrationsGate("new-node", false, 20*time.Second, func() time.Time { return now })
			if oldFirst {
				if _, err := db.Exec(`UPDATE cluster_nodes SET last_seen=? WHERE id='old-node'`, now.UnixMilli()); err != nil {
					t.Fatal(err)
				}
				opened, _, err := store.OpenWith(ctx, store.Options{URL: loc, MigrationGate: gate})
				if err == nil {
					opened.Close()
					t.Fatal("first Prism transition migrated past live pre-Prism writer")
				}
				if !errors.Is(err, store.ErrMigrationDeferred) {
					t.Fatal(err)
				}
				if got := maxMigration(t, db, "auth"); got != 3 {
					t.Fatalf("deferral advanced auth to %d", got)
				}
				if _, err := db.Exec(`UPDATE cluster_nodes SET last_seen=? WHERE id='old-node'`, now.Add(-time.Hour).UnixMilli()); err != nil {
					t.Fatal(err)
				}
			}
			opened, _, err := store.OpenWith(ctx, store.Options{URL: loc, MigrationGate: gate})
			if err != nil {
				t.Fatal(err)
			}
			opened.Close()
			assertUpgradeInventory(t, db)
			// Delivered 9503's SQL remains executable even after auth/5. A newer
			// binary cannot revoke the old process's DB credentials. This explicitly
			// characterizes why the external writer fence is required in BOTH orders.
			var before, after int64
			if err := db.QueryRow(`SELECT epoch FROM auth_membership_epoch WHERE id=1`).Scan(&before); err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec(`UPDATE auth_users SET active=0,updated_at=? WHERE id=?`, now.Unix(), upgradeFriendID); err != nil {
				t.Fatal(err)
			}
			if err := db.QueryRow(`SELECT epoch FROM auth_membership_epoch WHERE id=1`).Scan(&after); err != nil || after != before {
				t.Fatalf("old-writer epoch changed %d -> %d: %v", before, after, err)
			}
			// Old cluster heartbeats still fit the unchanged cluster/2 schema.
			if _, err := db.Exec(`UPDATE cluster_nodes SET last_seen=?,migrations='{"auth":3,"cluster":2}' WHERE id='old-node'`, now.UnixMilli()); err != nil {
				t.Fatal(err)
			}
			t.Log("old writer SQL and cluster heartbeat remain executable; deployment must revoke old DB access before migrating")
		})
	}
}
