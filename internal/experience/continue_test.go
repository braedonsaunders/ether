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

func TestContinueFiltersBeforeDedupeAndLimit(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	hidden := core.EpisodeKey(42, 1, 1)
	visible := core.EpisodeKey(42, 1, 2)
	unavailable := core.EpisodeKey(43, 1, 1)
	statuses := []core.WatchStatus{{Key: hidden, LastViewedAt: now}, {Key: unavailable, LastViewedAt: now.Add(-time.Second)}, {Key: visible, LastViewedAt: now.Add(-2 * time.Second)}}
	s := continueState{statuses: statuses, values: map[core.RatingKey]core.WatchStatus{}}
	visibility := Visibility{GetTitle: func(_ context.Context, key core.RatingKey) (*core.Title, error) {
		return &core.Title{Key: key, Type: core.TypeShow}, nil
	}, FilterAvailable: func(_ context.Context, titles []core.Title) []core.Title {
		if titles[0].Key == unavailable {
			return nil
		}
		return titles
	}}
	candidates := []core.Title{{Key: hidden, Type: core.TypeEpisode, Adult: true}, {Key: unavailable, Type: core.TypeEpisode}, {Key: visible, Type: core.TypeEpisode}}
	c := Continue{State: s, Now: func() time.Time { return now }, GetTitles: func(context.Context, []core.RatingKey) []core.Title { return candidates }, Visible: visibility.Visible, SectionAllowed: func(*core.User, core.MediaType) bool { return true }}
	got, state := c.InProgress(context.Background(), &core.User{ID: 1}, core.TypeShow, 1)
	if len(got) != 1 || got[0].Key != visible {
		t.Fatalf("visible item did not fill row: %+v", got)
	}
	if len(state) != 1 || state[visible].Key != visible {
		t.Fatalf("hidden watch status leaked: %+v", state)
	}
}

func TestContinueHiddenResumeDoesNotSuppressNext(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	prior := core.EpisodeKey(42, 1, 1)
	hidden := core.EpisodeKey(42, 1, 2)
	next := core.EpisodeKey(42, 1, 3)
	s := continueState{statuses: []core.WatchStatus{{Key: hidden, LastViewedAt: now}}, history: []core.HistoryEntry{{Key: prior, Completed: true, ViewedAt: now}}, values: map[core.RatingKey]core.WatchStatus{prior: {ViewCount: 1}}}
	c := Continue{State: s, Now: func() time.Time { return now }, GetTitles: func(context.Context, []core.RatingKey) []core.Title {
		return []core.Title{{Key: hidden, Type: core.TypeEpisode}}
	}, NextEpisode: func(context.Context, core.RatingKey) (*core.Title, error) {
		return &core.Title{Key: next, Type: core.TypeEpisode, Season: 1}, nil
	}, Visible: func(_ context.Context, _ *core.User, t *core.Title) bool { return t.Key != hidden }, SameDayUnavailable: func(context.Context, *core.Title) bool { return false }, SectionAllowed: func(*core.User, core.MediaType) bool { return true }}
	got, state := c.InProgress(context.Background(), &core.User{ID: 1}, core.TypeShow, 1)
	if len(got) != 1 || got[0].Key != next || len(state) != 1 {
		t.Fatalf("hidden resume suppressed next: %+v %+v", got, state)
	}
}

func TestContinueHonorsInheritedPolicyAndActiveSource(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	denied := core.EpisodeKey(42, 1, 1)
	active := core.EpisodeKey(43, 1, 1)
	s := continueState{statuses: []core.WatchStatus{{Key: denied, LastViewedAt: now}, {Key: active, LastViewedAt: now.Add(-time.Second)}}, values: map[core.RatingKey]core.WatchStatus{}}
	v := Visibility{GetTitle: func(_ context.Context, k core.RatingKey) (*core.Title, error) {
		return &core.Title{Key: k, Type: core.TypeShow, Adult: k == core.ShowKey(42)}, nil
	}, FilterAvailable: func(_ context.Context, titles []core.Title) []core.Title { return nil }, Sessions: func() []*core.PlaySession {
		return []*core.PlaySession{{UserID: 1, Key: denied}, {UserID: 1, Key: active}}
	}}
	titles := []core.Title{{Key: denied, Type: core.TypeEpisode, ShowKey: core.ShowKey(42)}, {Key: active, Type: core.TypeEpisode, ShowKey: core.ShowKey(43)}}
	c := Continue{State: s, Now: func() time.Time { return now }, GetTitles: func(context.Context, []core.RatingKey) []core.Title { return titles }, Visible: v.Visible, SectionAllowed: func(*core.User, core.MediaType) bool { return true }}
	got, state := c.InProgress(context.Background(), &core.User{ID: 1}, core.TypeShow, 10)
	if len(got) != 1 || got[0].Key != active || len(state) != 1 {
		t.Fatalf("inherited policy or active-source exception failed: %+v %+v", got, state)
	}
}
