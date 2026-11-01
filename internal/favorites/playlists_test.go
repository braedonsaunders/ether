package favorites

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/state"
	"github.com/braedonsaunders/ether/internal/store"
)

func TestPlaylistProjectsSameFavorites(t *testing.T) {
	ctx := context.Background()
	db := store.OpenTest(t)
	base := state.New(db, state.Options{}).Playlists()
	s, err := New(Options{DB: db, Marked: base.(core.TransactionalMarkedPlaylists), Active: func(_ context.Context, id int64) (*core.User, error) { return &core.User{ID: id}, nil }, Allowed: func(context.Context, *core.User, Subject) bool { return true }, Changed: func(context.Context, int64, Subject) {}})
	if err != nil {
		t.Fatal(err)
	}
	lists, err := NewPlaylists(base, s)
	if err != nil {
		t.Fatal(err)
	}
	before, err := lists.List(ctx, 1)
	if err != nil || len(before) != 0 {
		t.Fatalf("unexpected playlist: %+v %v", before, err)
	}
	a, b := core.MovieKey(21), core.MovieKey(22)
	if _, err = s.Set(ctx, &core.User{ID: 1}, Subject{Kind: KindTitle, ID: a.String()}, true); err != nil {
		t.Fatal(err)
	}
	got, err := lists.List(ctx, 1)
	if err != nil || len(got) != 1 || len(got[0].Items) != 1 || got[0].Items[0] != a {
		t.Fatalf("missing projected favorite: %+v %v", got, err)
	}
	if err = lists.Add(ctx, 1, got[0].Key, []core.RatingKey{b}); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.Has(ctx, &core.User{ID: 1}, Subject{Kind: KindTitle, ID: b.String()}); err != nil || !ok {
		t.Fatalf("playlist add not shared: %v %v", ok, err)
	}
	latest, err := lists.Get(ctx, 1, got[0].Key)
	if err != nil || len(latest.Items) != 2 {
		t.Fatal(latest, err)
	}
	if err = lists.Remove(ctx, 1, got[0].Key, 0); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.Has(ctx, &core.User{ID: 1}, Subject{Kind: KindTitle, ID: latest.Items[0].String()}); err != nil || ok {
		t.Fatalf("playlist remove not shared: %v %v", ok, err)
	}
}

func TestFavoriteBatchDenialCreatesNothing(t *testing.T) {
	ctx := context.Background()
	db := store.OpenTest(t)
	base := state.New(db, state.Options{}).Playlists()
	s, err := New(Options{DB: db, Marked: base.(core.TransactionalMarkedPlaylists), Active: func(_ context.Context, id int64) (*core.User, error) { return &core.User{ID: id}, nil }, Allowed: func(_ context.Context, _ *core.User, sub Subject) bool { return sub.ID != core.MovieKey(2).String() }, Changed: func(context.Context, int64, Subject) { t.Error("denied batch emitted event") }})
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.SetMany(ctx, &core.User{ID: 1}, []Subject{{Kind: KindTitle, ID: core.MovieKey(1).String()}, {Kind: KindTitle, ID: core.MovieKey(2).String()}}, true)
	if err == nil {
		t.Fatal("denied batch accepted")
	}
	lists, err := base.List(ctx, 1)
	if err != nil || len(lists) != 0 {
		t.Fatalf("denied batch created playlist: %+v %v", lists, err)
	}
}

func TestFavoritesRemainWhenMarkedPlaylistRenamed(t *testing.T) {
	ctx := context.Background()
	db := store.OpenTest(t)
	base := state.New(db, state.Options{}).Playlists()
	s, err := New(Options{DB: db, Marked: base.(core.TransactionalMarkedPlaylists), Active: func(_ context.Context, id int64) (*core.User, error) { return &core.User{ID: id}, nil }, Allowed: func(context.Context, *core.User, Subject) bool { return true }, Changed: func(context.Context, int64, Subject) {}})
	if err != nil {
		t.Fatal(err)
	}
	key := core.MovieKey(41)
	if _, err = s.Set(ctx, &core.User{ID: 1}, Subject{Kind: KindTitle, ID: key.String()}, true); err != nil {
		t.Fatal(err)
	}
	marked := base.(core.MarkedPlaylists)
	p, err := marked.MarkedPlaylist(ctx, 1, "favorites")
	if err != nil {
		t.Fatal(err)
	}
	id := int64(p.Key) % 1_000_000_000_000_000
	if _, err = db.ExecContext(ctx, `UPDATE state_playlists SET title=? WHERE id=?`, "My Movies", id); err != nil {
		t.Fatal(err)
	}
	wrapper, err := NewPlaylists(base, s)
	if err != nil {
		t.Fatal(err)
	}
	got, err := wrapper.Get(ctx, 1, p.Key)
	if err != nil || got.Title != "My Movies" || len(got.Items) != 1 || got.Items[0] != key {
		t.Fatalf("lost marked playlist: %+v %v", got, err)
	}
}

func TestFavoriteRemoveByIndexFailsIfOrderChanges(t *testing.T) {
	ctx := context.Background()
	db := store.OpenTest(t)
	base := state.New(db, state.Options{}).Playlists()
	uid := &core.User{ID: 1}
	var s *Service
	inject := false
	inserted := false
	allowed := func(c context.Context, u *core.User, sub Subject) bool {
		if inject && !inserted {
			inserted = true
			_, err := s.Set(c, u, Subject{Kind: KindTitle, ID: core.MovieKey(3).String()}, true)
			if err != nil {
				t.Error(err)
			}
		}
		return true
	}
	var err error
	s, err = New(Options{DB: db, Marked: base.(core.TransactionalMarkedPlaylists), Active: func(_ context.Context, id int64) (*core.User, error) { return &core.User{ID: id}, nil }, Allowed: allowed, Changed: func(context.Context, int64, Subject) {}})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []int64{1, 2} {
		if _, err = s.Set(ctx, uid, Subject{Kind: KindTitle, ID: core.MovieKey(id).String()}, true); err != nil {
			t.Fatal(err)
		}
	}
	inject = true
	if err = s.removeTitleAt(ctx, uid, func() core.RatingKey {
		p, err := base.(core.MarkedPlaylists).MarkedPlaylist(ctx, 1, "favorites")
		if err != nil {
			t.Fatal(err)
		}
		return p.Key
	}(), 0); err == nil {
		t.Fatal("expected conflict")
	}
	for _, id := range []int64{1, 2, 3} {
		ok, err := s.Has(ctx, uid, Subject{Kind: KindTitle, ID: core.MovieKey(id).String()})
		if err != nil || !ok {
			t.Fatalf("wrong favorite removed: %d %v %v", id, ok, err)
		}
	}
}

func TestMoveDeleteAndReenableMirror(t *testing.T) {
	ctx := context.Background()
	db := store.OpenTest(t)
	base := state.New(db, state.Options{}).Playlists()
	viewer := &core.User{ID: 1}
	events := 0
	s, err := New(Options{DB: db, Marked: base.(core.TransactionalMarkedPlaylists), Active: func(_ context.Context, id int64) (*core.User, error) { return &core.User{ID: id}, nil }, Allowed: func(context.Context, *core.User, Subject) bool { return true }, Changed: func(context.Context, int64, Subject) { events++ }})
	if err != nil {
		t.Fatal(err)
	}
	wrapper, err := NewPlaylists(base, s)
	if err != nil {
		t.Fatal(err)
	}
	a, b, c := core.MovieKey(1), core.MovieKey(2), core.MovieKey(3)
	for _, k := range []core.RatingKey{a, b} {
		if _, err = s.Set(ctx, viewer, Subject{Kind: KindTitle, ID: k.String()}, true); err != nil {
			t.Fatal(err)
		}
	}
	lists, err := wrapper.List(ctx, 1)
	if err != nil || len(lists) != 1 {
		t.Fatal(lists, err)
	}
	key := lists[0].Key
	if err = wrapper.Move(ctx, 1, key, 0, 1); err != nil {
		t.Fatal(err)
	}
	pl, err := wrapper.Get(ctx, 1, key)
	if err != nil || len(pl.Items) != 2 || pl.Items[0] != a || pl.Items[1] != b {
		t.Fatal(pl, err)
	}
	if _, err = s.Set(ctx, viewer, Subject{Kind: KindTitle, ID: c.String()}, true); err != nil {
		t.Fatal(err)
	}
	pl, err = wrapper.Get(ctx, 1, key)
	if err != nil || pl.Items[0] != c || pl.Items[1] != a || pl.Items[2] != b {
		t.Fatal(pl, err)
	}
	if err = wrapper.Delete(ctx, 1, key); err != nil {
		t.Fatal(err)
	}
	if events != 5 {
		t.Fatal("delete emitted favorite events", events)
	}
	if _, err = s.Set(ctx, viewer, Subject{Kind: KindTitle, ID: core.MovieKey(4).String()}, true); err != nil {
		t.Fatal(err)
	}
	lists, err = wrapper.List(ctx, 1)
	if err != nil || len(lists) != 0 {
		t.Fatalf("disabled mirror recreated: %+v %v", lists, err)
	}
	if err = s.SetMirror(ctx, viewer, true); err != nil {
		t.Fatal(err)
	}
	lists, err = wrapper.List(ctx, 1)
	if err != nil || len(lists) != 1 || len(lists[0].Items) != 4 || lists[0].Key == key {
		t.Fatalf("failed restore: %+v %v", lists, err)
	}
}

func TestStalePlaylistCannotMutateAfterMirrorDeleted(t *testing.T) {
	ctx := context.Background()
	db := store.OpenTest(t)
	base := state.New(db, state.Options{}).Playlists()
	viewer := &core.User{ID: 1}
	s, err := New(Options{DB: db, Marked: base.(core.TransactionalMarkedPlaylists), Active: func(_ context.Context, id int64) (*core.User, error) { return &core.User{ID: id}, nil }, Allowed: func(context.Context, *core.User, Subject) bool { return true }, Changed: func(context.Context, int64, Subject) {}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Set(ctx, viewer, Subject{Kind: KindTitle, ID: core.MovieKey(1).String()}, true); err != nil {
		t.Fatal(err)
	}
	pl, err := base.(core.MarkedPlaylists).MarkedPlaylist(ctx, 1, "favorites")
	if err != nil {
		t.Fatal(err)
	}
	if err = s.SetMirror(ctx, viewer, false); err != nil {
		t.Fatal(err)
	}
	_, err = s.setMany(ctx, viewer, []Subject{{Kind: KindTitle, ID: core.MovieKey(2).String()}}, true, pl.Key)
	if !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("stale key allowed: %v", err)
	}
	ok, err := s.Has(ctx, viewer, Subject{Kind: KindTitle, ID: core.MovieKey(2).String()})
	if err != nil || ok {
		t.Fatalf("stale add changed favorites: %v %v", ok, err)
	}
}

func TestMoveAndRemoveAdvancePlaylistRevision(t *testing.T) {
	ctx := context.Background()
	db := store.OpenTest(t)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	base := state.New(db, state.Options{Now: func() time.Time { return now }}).Playlists()
	var changed []Subject
	s, err := New(Options{DB: db, Now: func() time.Time { return now }, Marked: base.(core.TransactionalMarkedPlaylists), Active: func(_ context.Context, id int64) (*core.User, error) { return &core.User{ID: id}, nil }, Allowed: func(context.Context, *core.User, Subject) bool { return true }, Changed: func(_ context.Context, _ int64, sub Subject) { changed = append(changed, sub) }})
	if err != nil {
		t.Fatal(err)
	}
	viewer := &core.User{ID: 1}
	for _, id := range []int64{1, 2, 3} {
		if _, err = s.Set(ctx, viewer, Subject{Kind: KindTitle, ID: core.MovieKey(id).String()}, true); err != nil {
			t.Fatal(err)
		}
	}
	wrapper, _ := NewPlaylists(base, s)
	pls, err := wrapper.List(ctx, 1)
	if err != nil || len(pls) != 1 {
		t.Fatal(pls, err)
	}
	key := pls[0].Key
	before := pls[0].UpdatedAt
	changed = nil
	if err = wrapper.Move(ctx, 1, key, 0, 2); err != nil {
		t.Fatal(err)
	}
	moved, err := wrapper.Get(ctx, 1, key)
	if err != nil || !moved.UpdatedAt.After(before) || len(changed) != 3 {
		t.Fatalf("move refresh: %+v %v events=%v", moved, err, changed)
	}
	changed = nil
	if err = wrapper.Remove(ctx, 1, key, 0); err != nil {
		t.Fatal(err)
	}
	removed, err := wrapper.Get(ctx, 1, key)
	if err != nil || !removed.UpdatedAt.After(moved.UpdatedAt) || len(changed) != 1 {
		t.Fatalf("remove refresh: %+v %v events=%v", removed, err, changed)
	}
}

func TestMovePreservesHiddenFavorites(t *testing.T) {
	ctx := context.Background()
	db := store.OpenTest(t)
	base := state.New(db, state.Options{}).Playlists()
	viewer := &core.User{ID: 1}
	hidden := false
	s, err := New(Options{DB: db, Marked: base.(core.TransactionalMarkedPlaylists), Active: func(_ context.Context, id int64) (*core.User, error) { return &core.User{ID: id}, nil }, Allowed: func(_ context.Context, _ *core.User, sub Subject) bool {
		return !hidden || sub.ID != core.MovieKey(2).String()
	}, Changed: func(context.Context, int64, Subject) {}})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []int64{1, 2, 3} {
		if _, err = s.Set(ctx, viewer, Subject{Kind: KindTitle, ID: core.MovieKey(id).String()}, true); err != nil {
			t.Fatal(err)
		}
	}
	wrapper, _ := NewPlaylists(base, s)
	p, err := base.(core.MarkedPlaylists).MarkedPlaylist(ctx, 1, "favorites")
	if err != nil {
		t.Fatal(err)
	}
	hidden = true
	if err = wrapper.Move(ctx, 1, p.Key, 0, 1); err != nil {
		t.Fatal(err)
	}
	hidden = false
	got, err := wrapper.Get(ctx, 1, p.Key)
	if err != nil || len(got.Items) != 3 || got.Items[0] != core.MovieKey(1) || got.Items[1] != core.MovieKey(2) || got.Items[2] != core.MovieKey(3) {
		t.Fatalf("hidden position changed: %+v %v", got, err)
	}
}

func TestOrdinaryPlaylistUnaffectedWithoutFavorites(t *testing.T) {
	ctx := context.Background()
	db := store.OpenTest(t)
	base := state.New(db, state.Options{}).Playlists()
	s, err := New(Options{DB: db, Marked: base.(core.TransactionalMarkedPlaylists), Active: func(_ context.Context, id int64) (*core.User, error) { return &core.User{ID: id}, nil }, Allowed: func(context.Context, *core.User, Subject) bool { return true }, Changed: func(context.Context, int64, Subject) { t.Error("ordinary playlist emitted favorite event") }})
	if err != nil {
		t.Fatal(err)
	}
	wrapper, _ := NewPlaylists(base, s)
	p, err := wrapper.Create(ctx, 1, "Regular", []core.RatingKey{core.MovieKey(1)})
	if err != nil {
		t.Fatal(err)
	}
	if err = wrapper.Add(ctx, 1, p.Key, []core.RatingKey{core.MovieKey(2)}); err != nil {
		t.Fatal(err)
	}
	if err = wrapper.Move(ctx, 1, p.Key, 1, 0); err != nil {
		t.Fatal(err)
	}
	got, err := wrapper.Get(ctx, 1, p.Key)
	if err != nil || len(got.Items) != 2 || got.Items[0] != core.MovieKey(2) || got.Items[1] != core.MovieKey(1) {
		t.Fatalf("regular playlist differs: %+v %v", got, err)
	}
	if err = wrapper.Remove(ctx, 1, p.Key, 0); err != nil {
		t.Fatal(err)
	}
	if err = wrapper.Delete(ctx, 1, p.Key); err != nil {
		t.Fatal(err)
	}
	if _, err = base.(core.MarkedPlaylists).MarkedPlaylist(ctx, 1, "favorites"); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("ordinary playlist created favorites marker: %v", err)
	}
}
