package collections

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
)

func stampsDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := store.OpenMemory(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func importStampedList(t *testing.T, db *sql.DB, at time.Time) int64 {
	t.Helper()
	ctx := context.Background()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	id, err := ImportTitlesTx(ctx, tx, 0, 1, "Imported", []core.RatingKey{core.MovieKey(603)}, at)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	return id
}

func readSpec(t *testing.T, db *sql.DB, id int64) core.CollectionSpec {
	t.Helper()
	r, err := scanSpecRow(db.QueryRow(`SELECT `+specCols+` FROM collections_specs WHERE id=?`, id))
	if err != nil {
		t.Fatal(err)
	}
	return r.view()
}

// Imported collections are stamped in seconds, like every other collection,
// so the console can encode them (GitHub #180).
func TestImportedCollectionStampIsSeconds(t *testing.T) {
	db := stampsDB(t)
	at := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	id := importStampedList(t, db, at)
	var created, updated int64
	if err := db.QueryRow(`SELECT created_at, updated_at FROM collections_specs WHERE id=?`, id).Scan(&created, &updated); err != nil {
		t.Fatal(err)
	}
	if created != at.Unix() || updated != at.Unix() {
		t.Fatalf("stamps = %d/%d, want %d", created, updated, at.Unix())
	}
	v := readSpec(t, db, id)
	if v.UpdatedAt == nil || !v.UpdatedAt.Equal(at) {
		t.Fatalf("updatedAt = %v, want %v", v.UpdatedAt, at)
	}
	if _, err := json.Marshal(v); err != nil {
		t.Fatalf("encode: %v", err)
	}
}

// Stamps already written in nanoseconds never reach the encoder, and the
// repair migration turns them back into the seconds they meant.
func TestNanosecondStampsAreRepaired(t *testing.T) {
	db := stampsDB(t)
	at := time.Date(2026, 10, 20, 8, 30, 0, 0, time.UTC)
	id := importStampedList(t, db, at)
	if _, err := db.Exec(`UPDATE collections_specs SET created_at=?, updated_at=?, last_built_at=?, changed_at=? WHERE id=?`,
		at.UnixNano(), at.UnixNano(), at.UnixNano(), at.UnixNano(), id); err != nil {
		t.Fatal(err)
	}
	v := readSpec(t, db, id)
	if v.UpdatedAt != nil || v.LastBuiltAt != nil {
		t.Fatalf("impossible stamps read as %v/%v, want none", v.UpdatedAt, v.LastBuiltAt)
	}
	if _, err := json.Marshal(v); err != nil {
		t.Fatalf("encode damaged row: %v", err)
	}

	if _, err := db.Exec(repairNanoStampsSQL); err != nil {
		t.Fatal(err)
	}
	var created, updated, built, changed int64
	if err := db.QueryRow(`SELECT created_at, updated_at, last_built_at, changed_at FROM collections_specs WHERE id=?`, id).Scan(&created, &updated, &built, &changed); err != nil {
		t.Fatal(err)
	}
	for _, got := range []int64{created, updated, built, changed} {
		if got != at.Unix() {
			t.Fatalf("repaired stamps = %d/%d/%d/%d, want %d", created, updated, built, changed, at.Unix())
		}
	}
	// Seconds and "never" are left alone.
	other := importStampedList(t, db, at)
	if _, err := db.Exec(repairNanoStampsSQL); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT updated_at, last_built_at FROM collections_specs WHERE id=?`, other).Scan(&updated, &built); err != nil {
		t.Fatal(err)
	}
	if updated != at.Unix() || built != 0 {
		t.Fatalf("untouched stamps = %d/%d", updated, built)
	}
	if v := readSpec(t, db, id); v.UpdatedAt == nil || !v.UpdatedAt.Equal(at) {
		t.Fatalf("updatedAt after repair = %v, want %v", v.UpdatedAt, at)
	}
}
