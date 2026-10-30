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

func TestMembershipScopedAndPreservesStoredTime(t *testing.T) {
	ctx := context.Background()
	db := store.OpenTest(t)
	now := time.Date(2026, 1, 2, 3, 4, 5, 123456789, time.UTC)
	allowed := true
	s, err := New(Options{DB: db, Marked: state.New(db, state.Options{}).Playlists().(core.TransactionalMarkedPlaylists), Now: func() time.Time { return now }, Active: func(_ context.Context, id int64) (*core.User, error) {
		if id == 9 {
			return nil, core.ErrUnauthorized
		}
		return &core.User{ID: id}, nil
	}, Allowed: func(context.Context, *core.User, Subject) bool { return allowed }, Changed: func(context.Context, int64, Subject) {}})
	if err != nil {
		t.Fatal(err)
	}
	u := &core.User{ID: 1}
	sub := Subject{Kind: KindTitle, ID: core.MovieKey(7).String()}
	present, at, err := s.Membership(ctx, u, sub)
	if err != nil || present || at != nil {
		t.Fatal(present, at, err)
	}
	if _, err = s.Set(ctx, u, sub, true); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Hour)
	if _, err = s.Set(ctx, u, sub, true); err != nil {
		t.Fatal(err)
	}
	present, at, err = s.Membership(ctx, u, sub)
	if err != nil || !present || at == nil || !at.Equal(now.Add(-time.Hour)) {
		t.Fatal(present, at, err)
	}
	if p, ts, e := s.Membership(ctx, &core.User{ID: 2}, sub); e != nil || p || ts != nil {
		t.Fatal("viewer isolation", p, ts, e)
	}
	allowed = false
	if _, _, err = s.Membership(ctx, u, sub); !errors.Is(err, ErrForbidden) {
		t.Fatal("hidden membership", err)
	}
	allowed = true
	if _, _, err = s.Membership(ctx, &core.User{ID: 9}, sub); !errors.Is(err, ErrForbidden) {
		t.Fatal("inactive membership", err)
	}
	if _, err = db.ExecContext(ctx, `UPDATE favorites_items SET added_at=0 WHERE viewer_id=? AND kind=? AND subject_id=?`, u.ID, string(sub.Kind), sub.ID); err != nil {
		t.Fatal(err)
	}
	if p, ts, e := s.Membership(ctx, u, sub); e != nil || !p || ts != nil {
		t.Fatal("unknown timestamp", p, ts, e)
	}
}
