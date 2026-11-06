package clusterconfig

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/store"
)

func TestActiveJoinTokenMaintenance(t *testing.T) {
	ctx := context.Background()
	db := store.OpenTest(t)
	now := time.Unix(1800000000, 0)
	for i := 0; i < 260; i++ {
		if _, err := db.Exec(`INSERT INTO clusterconfig_join_tokens(id,secret_hash,created_at,expires_at) VALUES(?,?,?,?)`, fmt.Sprint(i), "hash", now.Unix()-100, now.Unix()); err != nil {
			t.Fatal(err)
		}
	}
	tok, _, err := CreateJoinToken(ctx, db, "postgres://db/ether", make([]byte, 32), time.Hour, now.Add(-time.Second))
	if err != nil {
		t.Fatal(err)
	}
	active, err := ActiveJoinTokens(ctx, db, now)
	if err != nil || len(active) != 1 || active[0].ID != tok.ID {
		t.Fatalf("active count=%d err=%v", len(active), err)
	}
	var remaining int
	if err := db.QueryRow(`SELECT COUNT(*) FROM clusterconfig_join_tokens WHERE expires_at<=?`, now.Unix()).Scan(&remaining); err != nil {
		t.Fatal(err)
	}
	if remaining != 4 {
		t.Fatalf("bounded prune left %d, want 4", remaining)
	}
	if err := ConsumeJoinToken(ctx, db, tok, "node", now); err != nil {
		t.Fatal(err)
	}
	active, err = ActiveJoinTokens(ctx, db, now)
	if err != nil || len(active) != 0 {
		t.Fatalf("used token returned, count=%d err=%v", len(active), err)
	}
	if err := ConsumeJoinToken(ctx, db, JoinToken{ID: "0", Secret: "bad"}, "node", now); err == nil {
		t.Fatal("pruned token accepted")
	}
}
