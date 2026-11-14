package watchlist

import (
	"context"
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
)

// Membership snapshots must not regenerate consumed intent. Every ingestion
// path uses the same transactional helper, including Plex and friends feeds.
func TestWarmIntentMembershipEdges(t *testing.T) {
	s, _, _, _, _, _ := setup(t)
	ctx := context.Background()
	title := core.Title{Key: core.ShowKey(10), Type: core.TypeShow, Title: "Show"}
	count := func() int {
		var n int
		if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM watchlist_warm_intents`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if err := s.upsert(ctx, 1, "manual", title); err != nil {
		t.Fatal(err)
	}
	if count() != 1 {
		t.Fatal("add didn't queue intent")
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM watchlist_warm_intents`); err != nil {
		t.Fatal(err)
	}
	if err := s.upsert(ctx, 1, "manual", title); err != nil {
		t.Fatal(err)
	}
	if err := s.replace(ctx, &core.User{ID: 1}, "plex", []core.Title{title}); err != nil {
		t.Fatal(err)
	}
	if count() != 0 {
		t.Fatal("snapshot or duplicate source recreated consumed intent")
	}
	if err := s.Remove(ctx, 1, title.Key); err != nil {
		t.Fatal(err)
	}
	if err := s.replace(ctx, &core.User{ID: 1}, "plex", []core.Title{title}); err != nil {
		t.Fatal(err)
	}
	if count() != 1 {
		t.Fatal("re-add didn't queue intent")
	}
}
