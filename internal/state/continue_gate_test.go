package state

import (
	"context"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
	"testing"
	"time"
)

func TestContinuePlaybackGateSurvivesUnwatchAndUsesWholeShowHistory(t *testing.T) {
	ctx := context.Background()
	db, err := store.OpenMemory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s := New(db, Options{})
	show := core.ShowKey(44)
	ep := core.EpisodeKey(44, 1, 1)
	if err := s.Scrobble(ctx, 1, ep); err != nil {
		t.Fatal(err)
	}
	got, err := s.PlaybackCompletedShows(ctx, 1, []core.RatingKey{show})
	if err != nil || got[show] {
		t.Fatal("manual mark opened playback gate", got, err)
	}
	_, err = db.ExecContext(ctx, `INSERT INTO state_history(user_id,rating_key,viewed_at,completed,view_type) VALUES(?,?,?,?,?)`, 1, int64(ep), time.Now().AddDate(-1, 0, 0).UnixNano(), 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Unscrobble(ctx, 1, ep); err != nil {
		t.Fatal(err)
	}
	got, err = s.PlaybackCompletedShows(ctx, 1, []core.RatingKey{show, core.ShowKey(45)})
	if err != nil || !got[show] || got[core.ShowKey(45)] {
		t.Fatal(got, err)
	}
	got, err = s.PlaybackCompletedShows(ctx, 2, []core.RatingKey{show})
	if err != nil || got[show] {
		t.Fatal("viewer leak", got, err)
	}
}

func TestRecentViewedUsesSettingsWriteTime(t *testing.T) {
	ctx := context.Background()
	db, err := store.OpenMemory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	clock := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	s := New(db, Options{Now: func() time.Time { return clock }})
	first, second := core.MovieKey(1), core.MovieKey(2)
	if err := s.Scrobble(ctx, 1, first); err != nil {
		t.Fatal(err)
	}
	clock = clock.Add(time.Minute)
	if err := s.Scrobble(ctx, 1, second); err != nil {
		t.Fatal(err)
	}
	before, _ := s.Get(ctx, 1, first)
	clock = clock.Add(time.Minute)
	if err := s.Rate(ctx, 1, first, 8); err != nil {
		t.Fatal(err)
	}
	after, _ := s.Get(ctx, 1, first)
	if !before.LastViewedAt.Equal(after.LastViewedAt) {
		t.Fatal("rating altered view time")
	}
	rows, err := s.RecentlyViewedSettings(ctx, 1, time.Time{}, 100)
	if err != nil || len(rows) != 2 || rows[0].Key != first {
		t.Fatal(rows, err)
	}
}

func TestContinueHideStampsSettingsWithoutMovingResume(t *testing.T) {
	s, now := newTestState(t)
	k := core.MovieKey(99)
	if err := s.LegacyProgress(context.Background(), 1, k, 2*time.Minute); err != nil {
		t.Fatal(err)
	}
	before, _ := s.Get(context.Background(), 1, k)
	*now = now.Add(time.Hour)
	if err := s.HideContinue(context.Background(), 1, k); err != nil {
		t.Fatal(err)
	}
	after, _ := s.Get(context.Background(), 1, k)
	if !after.UpdatedAt.After(before.UpdatedAt) || after.ViewOffset != before.ViewOffset || !after.LastViewedAt.Equal(before.LastViewedAt) {
		t.Fatalf("hide timestamps/resume %+v -> %+v", before, after)
	}
}
