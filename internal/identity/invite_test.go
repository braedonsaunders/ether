package identity_test

import (
	"context"
	"errors"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/identity"
	"github.com/braedonsaunders/ether/internal/store"
	"testing"
	"time"
)

func TestInviteSingleUse(t *testing.T) {
	ctx := context.Background()
	db := store.OpenTest(t)
	_, err := db.Exec(`INSERT INTO auth_users(id,plex_id,created_at,updated_at) VALUES(1,123,0,0)`)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1000, 0)
	i, err := identity.NewInvite(db, time.Hour, time.Hour, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	secret, err := i.Create(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	token, err := i.Redeem(ctx, secret)
	if err != nil {
		t.Fatal(err)
	}
	g, _ := identity.NewGrants(db, members{db}, time.Hour, func() time.Time { return now })
	if _, err = g.Resolve(ctx, "viewer-portal", token); err != nil {
		t.Fatal(err)
	}
	if _, err = i.Redeem(ctx, secret); !errors.Is(err, core.ErrUnauthorized) {
		t.Fatalf("reused invite: %v", err)
	}
	secret, err = i.Create(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Hour)
	if _, err = i.Redeem(ctx, secret); !errors.Is(err, core.ErrUnauthorized) {
		t.Fatalf("expired invite: %v", err)
	}
	_, err = db.Exec(`INSERT INTO auth_users(id,plex_id,is_owner,created_at,updated_at) VALUES(2,124,1,0,0)`)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = i.Create(ctx, 2); !errors.Is(err, core.ErrUnauthorized) {
		t.Fatalf("owner invite: %v", err)
	}
	secret, err = i.Create(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`UPDATE auth_users SET is_owner=0 WHERE id=2`)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`UPDATE auth_users SET is_owner=1 WHERE id=1`)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = i.Redeem(ctx, secret); !errors.Is(err, core.ErrUnauthorized) {
		t.Fatalf("promoted owner invite: %v", err)
	}

	id, secret, err := i.CreateViewer(ctx, "Invited Viewer")
	if err != nil {
		t.Fatal(err)
	}
	var origin string
	var pid *int64
	if err = db.QueryRow(`SELECT origin,plex_id FROM auth_users WHERE id=?`, id).Scan(&origin, &pid); err != nil || origin != "local" || pid != nil {
		t.Fatalf("viewer origin=%s plex=%v err=%v", origin, pid, err)
	}
	local, _ := identity.NewLocal(db)
	if _, _, err = local.Authenticate(ctx, "Invited Viewer", ""); !errors.Is(err, core.ErrUnauthorized) {
		t.Fatalf("empty password accepted: %v", err)
	}
	if _, err = i.Redeem(ctx, secret); err != nil {
		t.Fatal(err)
	}

}
