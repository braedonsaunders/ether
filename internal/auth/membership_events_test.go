package auth

import (
	"context"
	"testing"

	"github.com/braedonsaunders/ether/internal/plextv"
	"github.com/braedonsaunders/ether/internal/store"
)

func TestMembershipDeactivationEventsAreTransactional(t *testing.T) {
	ctx := context.Background()
	db := store.OpenTest(t)
	ddl := `CREATE TABLE observed_deactivations (viewer_id INTEGER NOT NULL); CREATE TRIGGER test_subscriber AFTER INSERT ON identity_viewer_deactivations BEGIN INSERT INTO observed_deactivations(viewer_id) VALUES(NEW.viewer_id); END;`
	if store.DialectOf(db) == store.Postgres {
		ddl = `CREATE TABLE observed_deactivations(viewer_id BIGINT NOT NULL); CREATE FUNCTION test_deactivation_subscriber() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN INSERT INTO observed_deactivations(viewer_id) VALUES(NEW.viewer_id); RETURN NEW; END; $$; CREATE TRIGGER test_subscriber AFTER INSERT ON identity_viewer_deactivations FOR EACH ROW EXECUTE FUNCTION test_deactivation_subscriber();`
	}
	if _, err := db.ExecContext(ctx, ddl); err != nil {
		t.Fatal(err)
	}
	s, err := New(ctx, db, Options{})
	if err != nil {
		t.Fatal(err)
	}
	friend := plextv.Friend{PlexID: 42, Username: "friend"}
	if err := s.SyncFriends(ctx, []plextv.Friend{friend}); err != nil {
		t.Fatal(err)
	}
	var id int64
	if err := db.QueryRowContext(ctx, `SELECT id FROM auth_users WHERE plex_id=42`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE auth_users SET active=0 WHERE id=?`, id); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM observed_deactivations WHERE viewer_id=?`, id).Scan(&count); err != nil || count != 1 {
		t.Fatalf("event missing in transaction: %d %v", count, err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM observed_deactivations WHERE viewer_id=?`, id).Scan(&count); err != nil || count != 0 {
		t.Fatalf("rolled-back event persisted: %d %v", count, err)
	}
	if err := s.SyncFriends(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM observed_deactivations WHERE viewer_id=?`, id).Scan(&count); err != nil || count != 1 {
		t.Fatalf("sync deactivation event: %d %v", count, err)
	}
	if err := s.SyncFriends(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM observed_deactivations WHERE viewer_id=?`, id).Scan(&count); err != nil || count != 1 {
		t.Fatalf("duplicate event: %d %v", count, err)
	}
	if err := s.SyncFriends(ctx, []plextv.Friend{friend}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetOwnerEnabled(ctx, id, false); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM observed_deactivations WHERE viewer_id=?`, id).Scan(&count); err != nil || count != 2 {
		t.Fatalf("owner deactivation event: %d %v", count, err)
	}
}

func TestDeactivationEventRowsAreTransient(t *testing.T) {
	ctx := context.Background()
	db := store.OpenTest(t)
	if _, err := db.ExecContext(ctx, `INSERT INTO auth_users(id,plex_id,created_at,updated_at) VALUES(1,42,0,0)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE auth_users SET active=0 WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM identity_viewer_deactivations`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("event not consumed: %d %v", n, err)
	}
}
