package notinterested

import (
	"context"
	"errors"
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
)

func TestSnapshotRevisionTracksSetUndoAndNoops(t *testing.T) {
	ctx := context.Background()
	db := store.OpenTest(t)
	visible := true
	s, err := New(Options{DB: db, Active: func(_ context.Context, id int64) (*core.User, error) {
		if id == 3 {
			return nil, core.ErrUnauthorized
		}
		return &core.User{ID: id}, nil
	}, Allowed: func(context.Context, *core.User, core.RatingKey) bool { return visible }, Changed: func(context.Context, int64, core.RatingKey) {}})
	if err != nil {
		t.Fatal(err)
	}
	viewer := &core.User{ID: 1}
	key := core.ShowKey(1)
	check := func(want uint64, n int) {
		t.Helper()
		entries, rev, err := s.Snapshot(ctx, viewer)
		if err != nil || rev != want || len(entries) != n {
			t.Fatalf("snapshot revision=%d entries=%v error=%v; want %d/%d", rev, entries, err, want, n)
		}
	}
	check(0, 0)
	if _, err = s.Set(ctx, viewer, key, true); err != nil {
		t.Fatal(err)
	}
	check(1, 1)
	if _, err = s.Set(ctx, viewer, key, true); err != nil {
		t.Fatal(err)
	}
	check(1, 1)
	handles, err := s.ListUndo(ctx, viewer)
	if err != nil || len(handles) != 1 {
		t.Fatal(handles, err)
	}
	visible = false
	check(1, 0)
	if changed, err := s.Undo(ctx, viewer, handles[0].Handle); err != nil || !changed {
		t.Fatal(changed, err)
	}
	check(2, 0)
	if _, err = s.Undo(ctx, viewer, handles[0].Handle); err != nil {
		t.Fatal(err)
	}
	check(2, 0)
	visible = true
	if _, err = s.Set(ctx, viewer, key, true); err != nil {
		t.Fatal(err)
	}
	check(3, 1)
	if _, err = s.Set(ctx, viewer, key, false); err != nil {
		t.Fatal(err)
	}
	check(4, 0)
	other, revision, err := s.Snapshot(ctx, &core.User{ID: 2})
	if err != nil || revision != 0 || len(other) != 0 {
		t.Fatal(other, revision, err)
	}
	if _, _, err = s.Snapshot(ctx, &core.User{ID: 3}); !errors.Is(err, ErrForbidden) {
		t.Fatal(err)
	}
}

func TestRevisionRollsBackWithMutation(t *testing.T) {
	ctx := context.Background()
	db := store.OpenTest(t)
	s, _ := New(Options{DB: db, Active: func(_ context.Context, id int64) (*core.User, error) { return &core.User{ID: id}, nil }, Allowed: func(context.Context, *core.User, core.RatingKey) bool { return true }, Changed: func(context.Context, int64, core.RatingKey) {}})
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO notinterested_titles(viewer_id,title_key,added_at,handle) VALUES(1,?,1,'test')`, core.MovieKey(1).String()); err != nil {
		t.Fatal(err)
	}
	var rev int64
	if err = tx.QueryRowContext(ctx, `SELECT revision FROM notinterested_revisions WHERE viewer_id=1`).Scan(&rev); err != nil || rev != 1 {
		t.Fatal(rev, err)
	}
	if err = tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	entries, version, err := s.Snapshot(ctx, &core.User{ID: 1})
	if err != nil || version != 0 || len(entries) != 0 {
		t.Fatal(entries, version, err)
	}
}

func TestSnapshotRevisionIsFromSameReadAsEntries(t *testing.T) {
	ctx := context.Background()
	db := store.OpenTest(t)
	key := core.MovieKey(12)
	u := &core.User{ID: 1}
	mutate := false
	opts := Options{DB: db, Active: func(_ context.Context, id int64) (*core.User, error) { return &core.User{ID: id}, nil }, Changed: func(context.Context, int64, core.RatingKey) {}}
	opts.Allowed = func(context.Context, *core.User, core.RatingKey) bool {
		if mutate {
			mutate = false
			if _, err := db.ExecContext(ctx, `DELETE FROM notinterested_titles WHERE viewer_id=? AND title_key=?`, u.ID, key.String()); err != nil {
				t.Fatal(err)
			}
		}
		return true
	}
	s, _ := New(opts)
	if _, err := s.Set(ctx, u, key, true); err != nil {
		t.Fatal(err)
	}
	mutate = true
	entries, rev, err := s.Snapshot(ctx, u)
	if err != nil || rev != 1 || len(entries) != 1 || entries[0].Key != key {
		t.Fatalf("inconsistent snapshot %v %d %v", entries, rev, err)
	}
	entries, rev, err = s.Snapshot(ctx, u)
	if err != nil || rev != 2 || len(entries) != 0 {
		t.Fatalf("changed snapshot %v %d %v", entries, rev, err)
	}
}
