package experience

import (
	"context"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

type continueState struct {
	core.State
	statuses []core.WatchStatus
	history  []core.HistoryEntry
	values   map[core.RatingKey]core.WatchStatus
}

func (s continueState) InProgress(context.Context, int64, int) ([]core.WatchStatus, error) {
	return s.statuses, nil
}
func (s continueState) History(context.Context, int64, time.Time, int) ([]core.HistoryEntry, error) {
	return s.history, nil
}
func (s continueState) GetMany(_ context.Context, _ int64, keys []core.RatingKey) (map[core.RatingKey]core.WatchStatus, error) {
	return s.values, nil
}
func (s continueState) Get(_ context.Context, _ int64, k core.RatingKey) (core.WatchStatus, error) {
	return s.values[k], nil
}

func TestContinueAdvancesAndPreservesRecency(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	a, b := core.EpisodeKey(42, 1, 1), core.EpisodeKey(42, 1, 2)
	state := continueState{history: []core.HistoryEntry{{Key: a, Completed: true, ViewedAt: now}}, values: map[core.RatingKey]core.WatchStatus{a: {ViewCount: 1}}}
	c := Continue{State: state, Now: func() time.Time { return now }, GetTitles: func(context.Context, []core.RatingKey) []core.Title { return nil }, NextEpisode: func(context.Context, core.RatingKey) (*core.Title, error) {
		return &core.Title{Key: b, Type: core.TypeEpisode, Season: 1}, nil
	}, Visible: func(context.Context, *core.User, *core.Title) bool { return true }, SameDayUnavailable: func(context.Context, *core.Title) bool { return false }, SectionAllowed: func(*core.User, core.MediaType) bool { return true }}
	titles, sts := c.InProgress(context.Background(), &core.User{ID: 1}, core.TypeShow, 10)
	if len(titles) != 1 || titles[0].Key != b || !sts[b].LastViewedAt.Equal(now) {
		t.Fatalf("unexpected result: %+v %+v", titles, sts)
	}
	state.values[a] = core.WatchStatus{} // A later unwatch must suppress advancement.
	titles, _ = c.InProgress(context.Background(), &core.User{ID: 1}, core.TypeShow, 10)
	if len(titles) != 0 {
		t.Fatalf("unwatched episode advanced: %+v", titles)
	}
}
