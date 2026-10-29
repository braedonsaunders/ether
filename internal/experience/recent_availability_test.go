package experience

import (
	"context"
	"github.com/braedonsaunders/ether/internal/core"
	"testing"
	"time"
)

type evidence struct{ known, available bool }

func (e evidence) Available(context.Context, core.RatingKey) (bool, bool) {
	return e.known, e.available
}
func TestSameDayUnavailable(t *testing.T) {
	now := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	ep := &core.Title{Type: core.TypeEpisode, Released: now.Add(-time.Hour)}
	if !SameDayUnavailable(context.Background(), ep, now, nil, evidence{true, false}) {
		t.Fatal("known missing episode should be unavailable")
	}
	if SameDayUnavailable(context.Background(), ep, now, nil, evidence{false, false}) {
		t.Fatal("unknown evidence should not hide episode")
	}
	ep.Released = now.Add(-25 * time.Hour)
	if SameDayUnavailable(context.Background(), ep, now, nil, evidence{true, false}) {
		t.Fatal("old episode should not use same-day evidence")
	}
	if !SameDayUnavailable(context.Background(), ep, now, func(context.Context, []core.Title) []core.Title { return nil }, nil) {
		t.Fatal("configured discovery filter should apply")
	}
}
