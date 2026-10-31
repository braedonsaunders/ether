package viewerpolicy

import (
	"context"
	"errors"
	"testing"

	_ "github.com/braedonsaunders/ether/internal/clusterconfig"
	"github.com/braedonsaunders/ether/internal/store"
)

func TestFencedSharedConfigVersion(t *testing.T) {
	db := store.OpenTest(t)
	ctx := context.Background()
	if _, err := db.ExecContext(ctx, `INSERT INTO clusterconfig_shared(id,version,doc,key_fingerprint,updated_at) VALUES(1,7,'{}','fixture',0)`); err != nil {
		t.Fatal(err)
	}
	epoch, err := Read(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err := Fence(ctx, db, tx, epoch); err != nil {
		t.Fatal(err)
	}
	if err := CheckSharedConfigVersion(ctx, tx, 7); err != nil {
		t.Fatal(err)
	}
	if err := CheckSharedConfigVersion(ctx, tx, 6); !errors.Is(err, ErrChanged) {
		t.Fatalf("stale version: %v", err)
	}
}
