package headeronly_test

import (
	"context"
	"strings"
	"testing"

	"github.com/braedonsaunders/ether/internal/handles/capability"
	"github.com/braedonsaunders/ether/internal/store"
)

func TestHeaderOnlySchemaHasNoIdentityDependency(t *testing.T) {
	db := store.OpenTest(t)
	ctx := context.Background()
	if _, ok := store.KnownMigrations()["capability_tickets"]; ok {
		t.Fatal("header-only import registered domain ticket storage")
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	h := capability.Header{HeaderScope: capability.HeaderScope{Namespace: "session", ViewerID: 1, Surface: "silo"}, Digest: strings.Repeat("a", 64), ExpiresMillis: 100}
	if err = capability.AdmitHeadersInTx(ctx, db, tx, []capability.Header{h}, capability.Limits{PerScope: 1, PerShard: 1}); err != nil {
		t.Fatal(err)
	}
	if ok, err := capability.ConsumeHeaderInTx(ctx, db, tx, h, 1); err != nil || !ok {
		t.Fatal(ok, err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
}
