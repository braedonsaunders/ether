package viewerpolicy

import (
	"context"
	"testing"
	"time"

	_ "github.com/braedonsaunders/ether/internal/favorites"
	"github.com/braedonsaunders/ether/internal/store"
)

func TestPostgresFenceBlocksPolicyWriter(t *testing.T) {
	db := store.OpenTest(t)
	if store.DialectOf(db) != store.Postgres {
		t.Skip("PostgreSQL required")
	}
	db.SetMaxOpenConns(4)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := db.ExecContext(ctx, `INSERT INTO favorites_items(viewer_id,kind,subject_id,added_at) VALUES(71,'title','fixture',0)`); err != nil {
		t.Fatal(err)
	}
	epoch, err := Read(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	holder, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close()
	writer, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	var holderPID, writerPID int
	if err := holder.QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&holderPID); err != nil {
		t.Fatal(err)
	}
	if err := writer.QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&writerPID); err != nil {
		t.Fatal(err)
	}
	tx, err := holder.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err := Fence(ctx, db, tx, epoch); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := writer.ExecContext(ctx, `UPDATE favorites_items SET added_at=1 WHERE viewer_id=71`)
		done <- err
	}()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	blocked := false
	for !blocked {
		select {
		case err := <-done:
			t.Fatalf("writer returned before observed block: %v", err)
		case <-ctx.Done():
			t.Fatal("writer was never observed waiting on fence")
		case <-ticker.C:
			if err := db.QueryRowContext(ctx, `SELECT ? = ANY(pg_blocking_pids(?))`, holderPID, writerPID).Scan(&blocked); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("writer after release: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("writer did not finish after fence release")
	}
}
