package watchlist

import (
	"context"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

func TestMembershipSelectsLatestCurrentSourceAndUnknownTime(t *testing.T) {
	s, _, _, _, _, now := setup(t)
	ctx := context.Background()
	key := core.MovieKey(1)
	if present, at, err := s.Membership(ctx, 1, key); err != nil || present || at != nil {
		t.Fatal(present, at, err)
	}
	title := core.Title{Key: key, Title: "Test", Type: core.TypeMovie}
	if err := s.upsert(ctx, 1, "first", title); err != nil {
		t.Fatal(err)
	}
	first := *now
	*now = now.Add(time.Hour)
	if err := s.upsert(ctx, 1, "second", title); err != nil {
		t.Fatal(err)
	}
	if err := s.upsert(ctx, 1, "first", title); err != nil {
		t.Fatal(err)
	}
	p, at, err := s.Membership(ctx, 1, key)
	if err != nil || !p || at == nil || !at.Equal(*now) {
		t.Fatal(p, at, err)
	}
	if p, ts, e := s.Membership(ctx, 2, key); e != nil || p || ts != nil {
		t.Fatal("viewer isolation", p, ts, e)
	}
	if _, err = s.db.ExecContext(ctx, `DELETE FROM watchlist_items WHERE user_id=? AND item_key=? AND source=?`, 1, key, "second"); err != nil {
		t.Fatal(err)
	}
	p, at, err = s.Membership(ctx, 1, key)
	if err != nil || !p || at == nil || !at.Equal(first) {
		t.Fatal("source removal", p, at, err)
	}
	if _, err = s.db.ExecContext(ctx, `UPDATE watchlist_items SET added=0 WHERE user_id=? AND item_key=?`, 1, key); err != nil {
		t.Fatal(err)
	}
	if p, ts, e := s.Membership(ctx, 1, key); e != nil || !p || ts != nil {
		t.Fatal("unknown timestamp", p, ts, e)
	}
}
