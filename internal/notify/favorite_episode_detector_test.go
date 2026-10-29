package notify

import (
	"context"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
)

func TestFavoriteEpisodeScanNoBacklogAndDeduplicated(t *testing.T) {
	ctx := context.Background()
	db := store.OpenTest(t)
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	active := true
	prefs := &ViewerAlerts{DB: db, Now: func() time.Time { return now }, Active: func(_ context.Context, id int64) (*core.User, error) {
		if !active {
			return nil, core.ErrUnauthorized
		}
		return &core.User{ID: id}, nil
	}}
	viewer := &core.User{ID: 4}
	if err := prefs.SetEnabled(ctx, viewer, true); err != nil {
		t.Fatal(err)
	}
	key := core.EpisodeKey(1, 1, 1)
	aired := now.Add(time.Minute)
	d := FavoriteEpisodeDetector{Preferences: prefs, Now: func() time.Time { return now }, Episodes: func(context.Context, *core.User) ([]core.Title, error) {
		return []core.Title{{Key: key, Type: core.TypeEpisode, Released: aired, Title: "Episode", ShowTitle: "Show"}}, nil
	}}
	if err := d.Scan(ctx); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM viewer_episode_alerts`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("backlog: %d %v", count, err)
	}
	now = now.Add(2 * time.Minute)
	if err := d.Scan(ctx); err != nil {
		t.Fatal(err)
	}
	if err := d.Scan(ctx); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM viewer_episode_alerts`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("dedupe: %d %v", count, err)
	}
	active = false
	if err := d.Scan(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestFavoriteEpisodeScanRebaselinesAfterOutage(t *testing.T) {
	ctx := context.Background()
	db := store.OpenTest(t)
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	prefs := &ViewerAlerts{DB: db, Now: func() time.Time { return now }, Active: func(_ context.Context, id int64) (*core.User, error) { return &core.User{ID: id}, nil }}
	viewer := &core.User{ID: 3}
	if err := prefs.SetEnabled(ctx, viewer, true); err != nil {
		t.Fatal(err)
	}
	aired := now.Add(time.Minute)
	d := FavoriteEpisodeDetector{Preferences: prefs, Now: func() time.Time { return now }, Episodes: func(context.Context, *core.User) ([]core.Title, error) {
		return []core.Title{{Key: core.EpisodeKey(1, 1, 2), Type: core.TypeEpisode, Released: aired}}, nil
	}}
	if err := d.Scan(ctx); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Hour)
	if err := d.Scan(ctx); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM viewer_episode_alerts`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("outage backlog %d: %v", count, err)
	}
	if err := prefs.SetEnabled(ctx, viewer, false); err != nil {
		t.Fatal(err)
	}
	if err := prefs.SetEnabled(ctx, viewer, true); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Minute)
	if err := d.Scan(ctx); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM viewer_episode_alerts`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("opt-in backlog %d: %v", count, err)
	}
}

func TestFavoriteEpisodeScanDetectsDelayedDatedMetadata(t *testing.T) {
	ctx := context.Background()
	db := store.OpenTest(t)
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	prefs := &ViewerAlerts{DB: db, Now: func() time.Time { return now }, Active: func(_ context.Context, id int64) (*core.User, error) { return &core.User{ID: id}, nil }}
	if err := prefs.SetEnabled(ctx, &core.User{ID: 7}, true); err != nil {
		t.Fatal(err)
	}
	visible := false
	d := FavoriteEpisodeDetector{Preferences: prefs, Now: func() time.Time { return now }, Episodes: func(context.Context, *core.User) ([]core.Title, error) {
		if !visible {
			return nil, nil
		}
		return []core.Title{{Key: core.EpisodeKey(2, 1, 1), Type: core.TypeEpisode, Released: time.Date(2026, 10, 29, 0, 0, 0, 0, time.UTC)}}, nil
	}}
	if err := d.Scan(ctx); err != nil {
		t.Fatal(err)
	}
	now = now.Add(15 * time.Minute)
	visible = true
	if err := d.Scan(ctx); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM viewer_episode_alerts`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("delayed metadata: %d %v", count, err)
	}
}
