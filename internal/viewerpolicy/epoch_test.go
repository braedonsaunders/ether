package viewerpolicy

import (
	"context"
	"testing"

	_ "github.com/braedonsaunders/ether/internal/favorites"
	"github.com/braedonsaunders/ether/internal/store"
)

func TestEpochWritersAndRollback(t *testing.T) {
	db := store.OpenTest(t)
	ctx := context.Background()
	before, err := Read(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{
		`INSERT INTO favorites_items(viewer_id,kind,subject_id,added_at) VALUES(7,'title','1',0)`,
		`UPDATE favorites_items SET added_at=1 WHERE viewer_id=7`,
		`DELETE FROM favorites_items WHERE viewer_id=7`,
	} {
		if _, err := db.ExecContext(ctx, query); err != nil {
			t.Fatal(err)
		}
		after, err := Read(ctx, db)
		if err != nil || after <= before {
			t.Fatalf("generation %d -> %d: %v", before, after, err)
		}
		before = after
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO favorites_items(viewer_id,kind,subject_id,added_at) VALUES(7,'title','1',0)`); err != nil {
		t.Fatal(err)
	}
	if err := Fence(ctx, db, tx, before); err != ErrChanged {
		t.Fatalf("fence=%v", err)
	}
	tx.Rollback()
	after, err := Read(ctx, db)
	if err != nil || after != before {
		t.Fatalf("rollback %d -> %d: %v", before, after, err)
	}
}

func TestExistingStateMigration(t *testing.T) {
	db := store.OpenTest(t)
	ctx := context.Background()
	if _, err := db.Exec(`INSERT INTO favorites_items(viewer_id,kind,subject_id,added_at) VALUES(7,'title','1',0)`); err != nil {
		t.Fatal(err)
	}
	// Reconstruct the schema before the epoch migration, retaining policy data.
	// Every registered component may have received a trigger; drop those too.
	for _, table := range tables {
		if store.DialectOf(db) == store.Postgres {
			if _, err := db.Exec("DROP TRIGGER IF EXISTS viewer_policy_" + table + " ON " + table); err != nil {
				t.Fatal(err)
			}
		} else {
			for _, event := range []string{"insert", "update", "delete"} {
				if _, err := db.Exec("DROP TRIGGER IF EXISTS viewer_policy_" + table + "_" + event); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	if store.DialectOf(db) == store.Postgres {
		if _, err := db.Exec(`DROP FUNCTION viewer_policy_bump()`); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`DROP TABLE viewer_policy_epoch; DELETE FROM schema_migrations WHERE component='viewer_policy'`); err != nil {
		t.Fatal(err)
	}
	if err := store.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM favorites_items`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("existing=%d %v", count, err)
	}
	before, _ := Read(ctx, db)
	if _, err := db.Exec(`DELETE FROM favorites_items`); err != nil {
		t.Fatal(err)
	}
	after, err := Read(ctx, db)
	if err != nil || after <= before {
		t.Fatalf("migrated fence %d -> %d %v", before, after, err)
	}
}
