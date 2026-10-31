package viewerpolicy

import (
	"context"
	"database/sql"
	"net/url"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func TestSQLiteFenceBlocksWriterOnDeferredTransaction(t *testing.T) {
	fileURL := (&url.URL{Scheme: "file", Path: filepath.Join(t.TempDir(), "policy.db")}).String() + "?_pragma=busy_timeout(0)"
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	db, err := sql.Open("sqlite", fileURL)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(2)
	for _, statement := range []string{
		`CREATE TABLE viewer_policy_epoch(id INTEGER PRIMARY KEY, generation INTEGER NOT NULL)`,
		`INSERT INTO viewer_policy_epoch(id, generation) VALUES(1, 1)`,
		`CREATE TABLE policy_writer(id INTEGER PRIMARY KEY, value INTEGER NOT NULL)`,
		`INSERT INTO policy_writer(id, value) VALUES(1, 0)`,
	} {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	writer, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err := Fence(ctx, db, tx, 1); err != nil {
		t.Fatal(err)
	}
	attemptCtx, attemptCancel := context.WithTimeout(ctx, time.Second)
	defer attemptCancel()
	result := make(chan error, 1)
	go func() {
		_, err := writer.ExecContext(attemptCtx, `UPDATE policy_writer SET value=2 WHERE id=1`)
		result <- err
	}()
	var attemptErr error
	received := false
	select {
	case attemptErr = <-result:
		received = true
		if attemptErr == nil {
			t.Fatal("writer committed while fenced")
		}
	case <-time.After(150 * time.Millisecond):
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if !received {
		select {
		case attemptErr = <-result:
		case <-ctx.Done():
			t.Fatal("writer did not finish after fence release")
		}
	}
	if attemptErr != nil {
		if _, err := writer.ExecContext(ctx, `UPDATE policy_writer SET value=2 WHERE id=1`); err != nil {
			t.Fatalf("writer after release: %v (attempt %v)", err, attemptErr)
		}
	}
}
