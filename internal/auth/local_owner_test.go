package auth

import (
	"context"
	"testing"

	"github.com/braedonsaunders/ether/internal/plextv"
	"github.com/braedonsaunders/ether/internal/store"
)

func TestClaimAttachesLocalOwner(t *testing.T) {
	ctx := context.Background()
	db := store.OpenTest(t)
	_, err := db.ExecContext(ctx, `INSERT INTO auth_users(id,plex_id,username,is_owner,active,origin,created_at,updated_at) VALUES(77,NULL,'local',1,1,'local',1,1)`)
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(ctx, db, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.SetOwner(ctx, plextv.Account{ID: 42, Username: "plex"}, "token"); err != nil {
		t.Fatal(err)
	}
	u, err := s.Owner(ctx)
	if err != nil || u.ID != 77 || u.PlexID != 42 {
		t.Fatalf("owner=%+v err=%v", u, err)
	}
	u, err = s.Authenticate(ctx, "token")
	if err != nil || u.ID != 77 {
		t.Fatalf("token owner=%+v err=%v", u, err)
	}
}
