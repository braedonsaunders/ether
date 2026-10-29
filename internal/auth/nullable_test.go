package auth

import (
	"context"
	"testing"

	"github.com/braedonsaunders/ether/internal/plextv"
	"github.com/braedonsaunders/ether/internal/store"
)

func TestNullablePlexIDMigrationPreservesCredentials(t *testing.T) {
	ctx := context.Background()
	db := store.OpenTest(t)
	if _, err := db.Exec(`INSERT INTO auth_users(id,plex_id,username,created_at,updated_at) VALUES (41,123,'viewer',1,1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO auth_tokens VALUES ('hash',41,'friend',1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO auth_delegations VALUES ('cast',41,99999)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`DELETE FROM schema_migrations WHERE component='auth' AND version=4`); err != nil {
		t.Fatal(err)
	}
	if err := store.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"auth_tokens", "auth_delegations"} {
		var id int
		if err := db.QueryRow(`SELECT user_id FROM ` + table).Scan(&id); err != nil || id != 41 {
			t.Fatalf("%s: id=%d err=%v", table, id, err)
		}
	}
	if _, err := db.Exec(`INSERT INTO auth_users(plex_id,username,created_at,updated_at) VALUES (NULL,'local',1,1),(NULL,'other',1,1)`); err != nil {
		t.Fatal(err)
	}
	s, err := New(ctx, db, Options{})
	if err != nil {
		t.Fatal(err)
	}
	users, err := s.Users(ctx)
	if err != nil || len(users) != 3 {
		t.Fatalf("users=%v err=%v", users, err)
	}
}

func TestFriendSyncKeepsLocalViewersActive(t *testing.T) {
	ctx := context.Background()
	db := store.OpenTest(t)
	if _, err := db.Exec(`INSERT INTO auth_users(plex_id,username,origin,created_at,updated_at) VALUES (NULL,'local','local',1,1)`); err != nil {
		t.Fatal(err)
	}
	s, err := New(ctx, db, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SyncFriends(ctx, []plextv.Friend{}); err != nil {
		t.Fatal(err)
	}
	var active int
	if err := db.QueryRow(`SELECT active FROM auth_users WHERE username='local'`).Scan(&active); err != nil || active != 1 {
		t.Fatalf("active=%d err=%v", active, err)
	}
}
