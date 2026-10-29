package store

import (
	"bytes"
	"context"
	"testing"
)

func TestPostgresSeededMigrationAdvancesIdentity(t *testing.T) {
	u := pgURL(t)
	ctx := context.Background()
	regMu.Lock()
	saved := migrations
	migrations = append(append([]migration(nil), saved...), migration{component: "zz_identity_seed_test", version: 1, sql: `CREATE TABLE identity_seed_test(id INTEGER PRIMARY KEY AUTOINCREMENT, label TEXT NOT NULL); INSERT INTO IDENTITY_SEED_TEST(id,label) VALUES(10,'seed')`})
	regMu.Unlock()
	defer func() { regMu.Lock(); migrations = saved; regMu.Unlock() }()
	db, err := OpenTestPostgres(ctx, u)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var id int64
	if err := db.QueryRowContext(ctx, `INSERT INTO identity_seed_test(label) VALUES('new') RETURNING id`).Scan(&id); err != nil || id != 11 {
		t.Fatalf("seeded identity: %d %v", id, err)
	}
	if _, err := db.ExecContext(ctx, `SELECT setval(pg_get_serial_sequence('identity_seed_test','id'),100,true)`); err != nil {
		t.Fatal(err)
	}
	regMu.Lock()
	migrations = append(migrations, migration{component: "zz_identity_seed_test", version: 2, sql: `INSERT INTO identity_seed_test(id,label) VALUES(5,'later seed')`})
	regMu.Unlock()
	if err := Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, `INSERT INTO identity_seed_test(label) VALUES('after') RETURNING id`).Scan(&id); err != nil || id != 101 {
		t.Fatalf("sequence rewound: %d %v", id, err)
	}
}

func TestPostgresLogicalRestoreDoesNotRewindIdentity(t *testing.T) {
	u := pgURL(t)
	ctx := context.Background()
	src, err := OpenTestPostgres(ctx, u)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	dst, err := OpenTestPostgres(ctx, u)
	if err != nil {
		t.Fatal(err)
	}
	defer dst.Close()
	if _, err := src.ExecContext(ctx, `INSERT INTO storetest_links(id,label) VALUES(9,'restored')`); err != nil {
		t.Fatal(err)
	}
	var dump bytes.Buffer
	if err := ExportLogical(ctx, src, &dump); err != nil {
		t.Fatal(err)
	}
	if _, err := dst.ExecContext(ctx, `SELECT setval(pg_get_serial_sequence('storetest_links','id'),100,true)`); err != nil {
		t.Fatal(err)
	}
	if err := ImportLogical(ctx, dst, bytes.NewReader(dump.Bytes())); err != nil {
		t.Fatal(err)
	}
	var id int64
	if err := dst.QueryRowContext(ctx, `INSERT INTO storetest_links(label) VALUES('new') RETURNING id`).Scan(&id); err != nil || id != 101 {
		t.Fatalf("restored identity rewound: %d %v", id, err)
	}
}
