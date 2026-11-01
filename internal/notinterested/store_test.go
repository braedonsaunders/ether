package notinterested

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
)

func TestPreferencesIsolationUndoAndPolicy(t *testing.T) {
	ctx := context.Background()
	db := store.OpenTest(t)
	now := time.Unix(100, 0)
	allowed := map[core.RatingKey]bool{}
	active := map[int64]bool{1: true, 2: true}
	var events []core.RatingKey
	opts := Options{DB: db, Now: func() time.Time { return now }, Active: func(_ context.Context, id int64) (*core.User, error) {
		if !active[id] {
			return nil, core.ErrUnauthorized
		}
		return &core.User{ID: id}, nil
	}, Allowed: func(_ context.Context, _ *core.User, k core.RatingKey) bool { return allowed[k] }, Changed: func(_ context.Context, _ int64, k core.RatingKey) { events = append(events, k) }}
	s, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	a, b := core.MovieKey(1), core.ShowKey(2)
	allowed[a] = true
	allowed[b] = true
	u := &core.User{ID: 1}
	if ok, err := s.Has(ctx, u, a); err != nil || ok {
		t.Fatalf("default %v %v", ok, err)
	}
	if ok, err := s.Set(ctx, u, a, true); err != nil || !ok {
		t.Fatal(ok, err)
	}
	if ok, err := s.Set(ctx, u, a, true); err != nil || ok || len(events) != 1 {
		t.Fatal(ok, err, events)
	}
	now = now.Add(time.Second)
	if ok, err := s.Set(ctx, u, b, true); err != nil || !ok {
		t.Fatal(ok, err)
	}
	if ok, err := s.Has(ctx, &core.User{ID: 2}, a); err != nil || ok {
		t.Fatalf("cross viewer %v %v", ok, err)
	}
	allowed[b] = false
	list, err := s.List(ctx, u, 1)
	if err != nil || len(list) != 1 || list[0].Key != a {
		t.Fatalf("filter before limit %+v %v", list, err)
	}
	if _, err = s.Set(ctx, u, b, false); !errors.Is(err, ErrForbidden) {
		t.Fatal("hidden title changed", err)
	}
	if ok, err := s.Set(ctx, u, a, false); err != nil || !ok || len(events) != 3 {
		t.Fatalf("undo %v %v %v", ok, err, events)
	}
	if ok, err := s.Set(ctx, u, a, false); err != nil || ok || len(events) != 3 {
		t.Fatalf("repeat undo %v %v", ok, err)
	}
	active[1] = false
	if _, err = s.List(ctx, u, 0); !errors.Is(err, ErrForbidden) {
		t.Fatal("inactive viewer", err)
	}
	if _, err = s.Set(ctx, u, a, true); !errors.Is(err, ErrForbidden) {
		t.Fatal("inactive viewer", err)
	}
}

func TestPreferencesSharedAcrossInstancesAndEventsAfterCommit(t *testing.T) {
	ctx := context.Background()
	db := store.OpenTest(t)
	key := core.EpisodeKey(7, 1, 1)
	v := &core.User{ID: 2}
	events := 0
	opts := Options{DB: db, Active: func(_ context.Context, id int64) (*core.User, error) { return &core.User{ID: id}, nil }, Allowed: func(context.Context, *core.User, core.RatingKey) bool { return true }}
	opts.Changed = func(ctx context.Context, id int64, key core.RatingKey) {
		events++
		var n int
		if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM notinterested_titles WHERE viewer_id=? AND title_key=?`, id, key.String()).Scan(&n); err != nil || n != 1 {
			t.Errorf("event before commit: %d %v", n, err)
		}
	}
	a, _ := New(opts)
	b, _ := New(opts)
	if _, err := a.Set(ctx, v, key, true); err != nil {
		t.Fatal(err)
	}
	if got, err := b.Has(ctx, v, key); err != nil || !got || events != 1 {
		t.Fatal(got, err, events)
	}
	if _, err := a.Set(ctx, v, core.PlaylistKey(1), true); !errors.Is(err, ErrForbidden) {
		t.Fatal("non-title accepted", err)
	}
}

func TestHiddenTitleUndoUsesViewerScopedOpaqueHandle(t *testing.T) {
	ctx := context.Background()
	db := store.OpenTest(t)
	visible := true
	active := true
	var changes []core.RatingKey
	s, err := New(Options{DB: db, Active: func(_ context.Context, id int64) (*core.User, error) {
		if !active {
			return nil, core.ErrUnauthorized
		}
		return &core.User{ID: id}, nil
	}, Allowed: func(context.Context, *core.User, core.RatingKey) bool { return visible }, Changed: func(_ context.Context, _ int64, k core.RatingKey) { changes = append(changes, k) }})
	if err != nil {
		t.Fatal(err)
	}
	u := &core.User{ID: 1}
	key := core.ShowKey(100)
	if _, err = s.Set(ctx, u, key, true); err != nil {
		t.Fatal(err)
	}
	entries, err := s.ListUndo(ctx, u)
	if err != nil || len(entries) != 1 || entries[0].Key != key || len(entries[0].Handle) != 64 {
		t.Fatalf("visible %+v %v", entries, err)
	}
	handle := entries[0].Handle
	visible = false
	entries, err = s.ListUndo(ctx, u)
	if err != nil || len(entries) != 1 || entries[0].Key != 0 || entries[0].Handle != handle {
		t.Fatalf("hidden %+v %v", entries, err)
	}
	if changed, err := s.Undo(ctx, &core.User{ID: 2}, handle); err != nil || changed {
		t.Fatal("cross-viewer removal", changed, err)
	}
	active = false
	if _, err = s.Undo(ctx, u, handle); !errors.Is(err, ErrForbidden) {
		t.Fatal("inactive", err)
	}
	active = true
	if changed, err := s.Undo(ctx, u, handle); err != nil || !changed {
		t.Fatal(changed, err)
	}
	if len(changes) != 2 || changes[1] != 0 {
		t.Fatal("hidden identity in event", changes)
	}
	if changed, err := s.Undo(ctx, u, handle); err != nil || changed {
		t.Fatal("repeat undo", changed, err)
	}
	if _, err = s.Set(ctx, u, key, true); !errors.Is(err, ErrForbidden) {
		t.Fatal("hidden re-add", err)
	}
}
