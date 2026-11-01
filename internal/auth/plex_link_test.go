package auth

import (
	"context"
	"database/sql"
	"github.com/braedonsaunders/ether/internal/plextv"
	"github.com/braedonsaunders/ether/internal/store"
	"testing"
)

func TestPlexLinksFollowMembership(t *testing.T) {
	ctx := context.Background()
	db := store.OpenTest(t)
	s, err := New(ctx, db, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.SetOwner(ctx, plextv.Account{ID: 101, Username: "owner"}, "token"); err != nil {
		t.Fatal(err)
	}
	if err = s.SyncFriends(ctx, []plextv.Friend{{PlexID: 202, Username: "friend"}}); err != nil {
		t.Fatal(err)
	}
	for _, subject := range []string{"101", "202"} {
		var count int
		if err = db.QueryRowContext(ctx, `SELECT count(*) FROM identity_links l JOIN auth_users u ON u.id=l.viewer_id WHERE l.provider='plex' AND l.subject=? AND CAST(u.plex_id AS TEXT)=l.subject`, subject).Scan(&count); err != nil || count != 1 {
			t.Fatalf("%s: %d %v", subject, count, err)
		}
	}
	if err = s.SyncFriends(ctx, nil); err != nil {
		t.Fatal(err)
	}
	var active int
	if err = db.QueryRowContext(ctx, `SELECT active FROM auth_users WHERE plex_id=202`).Scan(&active); err != nil || active != 0 {
		t.Fatalf("unshare: %d %v", active, err)
	}
}

func TestBackfillPlexLinksOnStartup(t *testing.T) {
	ctx := context.Background()
	db := store.OpenTest(t)
	if _, err := db.ExecContext(ctx, `INSERT INTO auth_users(id,plex_id,title,created_at,updated_at) VALUES (42,999,'Existing',1,2)`); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if _, err := New(ctx, db, Options{}); err != nil {
			t.Fatal(err)
		}
	}
	var viewer int64
	if err := db.QueryRowContext(ctx, `SELECT viewer_id FROM identity_links WHERE provider='plex' AND subject='999'`).Scan(&viewer); err != nil || viewer != 42 {
		t.Fatalf("viewer=%d err=%v", viewer, err)
	}
}

func TestLinkPlexVerifiesSharingAndNeverMerges(t *testing.T) {
	ctx := context.Background()
	db := store.OpenTest(t)
	_, err := db.ExecContext(ctx, `INSERT INTO auth_users(id,plex_id,username,created_at,updated_at) VALUES (10,NULL,'local',1,1),(11,222,'existing',1,1),(12,NULL,'other',1,1)`)
	if err != nil {
		t.Fatal(err)
	}
	v := &fakeValidator{accounts: map[string]plextv.Account{"new": {ID: 111, Username: "new"}, "existing": {ID: 222}, "noaccess": {ID: 333}}, access: map[string]bool{"new": true, "existing": true}}
	s, err := New(ctx, db, Options{Validator: v})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.LinkPlex(ctx, 10, "noaccess"); err == nil {
		t.Fatal("accepted unshared account")
	}
	if err = s.LinkPlex(ctx, 10, "existing"); err == nil {
		t.Fatal("merged existing viewer")
	}
	if err = s.LinkPlex(ctx, 10, "new"); err != nil {
		t.Fatal(err)
	}
	if err = s.LinkPlex(ctx, 12, "new"); err == nil {
		t.Fatal("duplicate link accepted")
	}
	var viewer int64
	if err = db.QueryRowContext(ctx, `SELECT viewer_id FROM identity_links WHERE provider='plex' AND subject='111'`).Scan(&viewer); err != nil || viewer != 10 {
		t.Fatalf("viewer %d err %v", viewer, err)
	}
	if err = s.SyncFriends(ctx, nil); err != nil {
		t.Fatal(err)
	}
	var active int
	if err = db.QueryRowContext(ctx, `SELECT active FROM auth_users WHERE id=10`).Scan(&active); err != nil || active != 0 {
		t.Fatalf("unshare active %d err %v", active, err)
	}
}

type linkRaceValidator struct{ sync func() error }

func (v linkRaceValidator) Account(context.Context, string) (*plextv.Account, error) {
	return &plextv.Account{ID: 444}, nil
}
func (v linkRaceValidator) HasServer(context.Context, string) (bool, error) { return true, v.sync() }
func TestLinkPlexRejectsConcurrentUnshare(t *testing.T) {
	ctx := context.Background()
	db := store.OpenTest(t)
	if _, err := db.ExecContext(ctx, `INSERT INTO auth_users(id,plex_id,created_at,updated_at) VALUES (20,NULL,1,1)`); err != nil {
		t.Fatal(err)
	}
	other, err := New(ctx, db, Options{})
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(ctx, db, Options{Validator: linkRaceValidator{sync: func() error { return other.SyncFriends(ctx, nil) }}})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.LinkPlex(ctx, 20, "token"); err == nil {
		t.Fatal("accepted stale membership check")
	}
	var id sql.NullInt64
	if err = db.QueryRowContext(ctx, `SELECT plex_id FROM auth_users WHERE id=20`).Scan(&id); err != nil || id.Valid {
		t.Fatalf("linked stale account: %v %v", id, err)
	}
}
