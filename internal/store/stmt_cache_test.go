package store

import (
	"context"
	"database/sql"
	"testing"
)

func TestCachedSQLiteStatementsReuse(t *testing.T) {
	db, err := sql.Open("sqlite-cached", "file::memory:?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	db.SetMaxOpenConns(1)
	ctx := context.Background()
	if _, err := db.ExecContext(ctx, `CREATE TABLE sample(id INTEGER PRIMARY KEY, name TEXT)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO sample(name) VALUES(?)`, "one"); err != nil {
		t.Fatal(err)
	}
	read := func() string {
		t.Helper()
		var name string
		if err := db.QueryRowContext(ctx, `SELECT name FROM sample WHERE id=?`, 1).Scan(&name); err != nil {
			t.Fatal(err)
		}
		return name
	}
	if read() != "one" || read() != "one" {
		t.Fatal(read())
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE sample SET name=? WHERE id=?`, "two", 1); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if read() != "two" {
		t.Fatal(read())
	}
}
