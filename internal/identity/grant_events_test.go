package identity_test

import (
	"context"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/identity"
	"github.com/braedonsaunders/ether/internal/store"
)

func TestGrantRevocationEventsAreTransactional(t *testing.T) {
	ctx := context.Background()
	db := store.OpenTest(t)
	ddl := `CREATE TABLE observed_revocations (viewer_id INTEGER NOT NULL, grant_hash TEXT NOT NULL); CREATE TRIGGER test_subscriber AFTER INSERT ON identity_grant_revocations BEGIN INSERT INTO observed_revocations(viewer_id,grant_hash) VALUES(NEW.viewer_id,NEW.grant_hash); END;`
	if store.DialectOf(db) == store.Postgres {
		ddl = `CREATE TABLE observed_revocations(viewer_id BIGINT NOT NULL,grant_hash TEXT NOT NULL); CREATE FUNCTION test_revocation_subscriber() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN INSERT INTO observed_revocations(viewer_id,grant_hash) VALUES(NEW.viewer_id,NEW.grant_hash); RETURN NEW; END; $$; CREATE TRIGGER test_subscriber AFTER INSERT ON identity_grant_revocations FOR EACH ROW EXECUTE FUNCTION test_revocation_subscriber();`
	}
	if _, err := db.ExecContext(ctx, ddl); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO auth_users(id,plex_id,created_at,updated_at) VALUES(1,123,0,0)`); err != nil {
		t.Fatal(err)
	}
	g, err := identity.NewGrants(db, members{db}, time.Hour, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := g.Issue(ctx, 1, nil, "viewer-portal", core.Device{ID: "one"}); err != nil {
		t.Fatal(err)
	}
	if _, err := g.Issue(ctx, 1, nil, "viewer-portal", core.Device{ID: "two"}); err != nil {
		t.Fatal(err)
	}
	var first, second string
	if err := db.QueryRowContext(ctx, `SELECT hash FROM identity_grants WHERE device_id='one'`).Scan(&first); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, `SELECT hash FROM identity_grants WHERE device_id='two'`).Scan(&second); err != nil {
		t.Fatal(err)
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = tx.ExecContext(ctx, `UPDATE identity_grants SET revoked=1 WHERE hash=?`, first); err != nil {
		t.Fatal(err)
	}
	var count int
	if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM observed_revocations WHERE viewer_id=1 AND grant_hash=?`, first).Scan(&count); err != nil || count != 1 {
		t.Fatalf("event missing: %d %v", count, err)
	}
	if err = tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if err = db.QueryRowContext(ctx, `SELECT count(*) FROM observed_revocations`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("rollback leaked: %d %v", count, err)
	}
	if err = g.RevokeDevice(ctx, 1, "one"); err != nil {
		t.Fatal(err)
	}
	if err = db.QueryRowContext(ctx, `SELECT count(*) FROM observed_revocations WHERE viewer_id=1 AND grant_hash=?`, first).Scan(&count); err != nil || count != 1 {
		t.Fatalf("wrong event: %d %v", count, err)
	}
	if err = db.QueryRowContext(ctx, `SELECT count(*) FROM observed_revocations WHERE grant_hash=?`, second).Scan(&count); err != nil || count != 0 {
		t.Fatalf("other device affected: %d %v", count, err)
	}
	if err = g.RevokeDevice(ctx, 1, "one"); err != nil {
		t.Fatal(err)
	}
	if err = db.QueryRowContext(ctx, `SELECT count(*) FROM observed_revocations`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("repeat emitted: %d %v", count, err)
	}
}

func TestGrantEventRowsAreTransient(t *testing.T) {
	ctx := context.Background()
	db := store.OpenTest(t)
	if _, err := db.ExecContext(ctx, `INSERT INTO auth_users(id,plex_id,created_at,updated_at) VALUES(1,42,0,0)`); err != nil {
		t.Fatal(err)
	}
	g, err := identity.NewGrants(db, members{db}, time.Hour, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = g.Issue(ctx, 1, nil, "viewer-portal", core.Device{ID: "one"}); err != nil {
		t.Fatal(err)
	}
	if err = g.RevokeDevice(ctx, 1, "one"); err != nil {
		t.Fatal(err)
	}
	var n int
	if err = db.QueryRowContext(ctx, `SELECT count(*) FROM identity_grant_revocations`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("event not consumed: %d %v", n, err)
	}
}
