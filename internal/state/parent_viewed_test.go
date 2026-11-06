package state

import (
	"github.com/braedonsaunders/ether/internal/core"
	"testing"
	"time"
)

func TestParentViewedFallbackKeepsOwnTimestamp(t *testing.T) {
	s, now := newTestState(t)
	ep := core.EpisodeKey(42, 1, 1)
	parent := ep.Parent()
	show := parent.Parent()
	if err := s.ApplyRemote(ctx, 1, ep, func() *bool { v := true; return &v }(), *now, nil, time.Time{}); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetMany(ctx, 1, []core.RatingKey{parent, show})
	if err != nil {
		t.Fatal(err)
	}
	if !got[parent].LastViewedAt.Equal(*now) || !got[show].LastViewedAt.Equal(*now) {
		t.Fatal("cloud leaf timestamp did not reach parent projection")
	}
	if got[parent].ViewCount != 0 || got[show].ViewCount != 0 {
		t.Fatal("fallback invented parent counts")
	}
}
