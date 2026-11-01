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

func TestOwnerLocalCredentialForRecovery(t *testing.T) {
	ctx := context.Background()
	db := store.OpenTest(t)
	l, err := NewLocal(db)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := OwnerLocalCredential(ctx, db); ok || err != nil {
		t.Fatalf("credential before any owner: %v %v", ok, err)
	}
	if _, err := l.CreateViewer(ctx, "viewer", "viewer-secret"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := OwnerLocalCredential(ctx, db); ok {
		t.Fatal("a viewer's credential was returned as the owner's")
	}
	id, err := l.CreateOwner(ctx, "owner", "secret")
	if err != nil {
		t.Fatal(err)
	}
	cred, ok, err := OwnerLocalCredential(ctx, db)
	if err != nil || !ok || cred.ViewerID != id || cred.Username != "owner" || cred.IdentityID == 0 {
		t.Fatalf("credential = %+v %v %v", cred, ok, err)
	}
	if !VerifyOwnerCredential(cred, "owner", "secret") {
		t.Fatal("owner password rejected")
	}
	for _, c := range [][2]string{{"owner", "wrong"}, {"Owner", "secret"}, {"owner", ""}} {
		if VerifyOwnerCredential(cred, c[0], c[1]) {
			t.Fatalf("accepted %q/%q", c[0], c[1])
		}
	}
}
