package identity

import (
	"context"
	"testing"

	_ "github.com/braedonsaunders/ether/internal/auth"
	"github.com/braedonsaunders/ether/internal/store"
)

func TestCreateLocalOwnerAtomic(t *testing.T) {
	ctx := context.Background()
	db := store.OpenTest(t)
	l, err := NewLocal(db)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = l.CreateOwner(ctx, "owner", ""); err == nil {
		t.Fatal("empty password accepted")
	}
	id, err := l.CreateOwner(ctx, "owner", "secret")
	if err != nil {
		t.Fatal(err)
	}
	got, _, err := l.Authenticate(ctx, "owner", "secret")
	if err != nil || got != id {
		t.Fatalf("authenticate %d %v", got, err)
	}
	if _, err = l.CreateOwner(ctx, "another", "secret"); err == nil {
		t.Fatal("second owner accepted")
	}
	var count int
	if err = db.QueryRowContext(ctx, `SELECT count(*) FROM auth_users`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("extra owner left behind: %d %v", count, err)
	}
}
