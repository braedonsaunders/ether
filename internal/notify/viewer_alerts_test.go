package notify

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
)

func TestViewerAlertPreference(t *testing.T) {
	ctx := context.Background()
	db := store.OpenTest(t)
	now := time.Unix(1000, 0)
	active := true
	s := &ViewerAlerts{DB: db, Now: func() time.Time { return now }, Active: func(_ context.Context, id int64) (*core.User, error) {
		if !active {
			return nil, core.ErrUnauthorized
		}
		return &core.User{ID: id}, nil
	}}
	viewer := &core.User{ID: 2}
	if enabled, err := s.Enabled(ctx, viewer); err != nil || enabled {
		t.Fatalf("default: %v %v", enabled, err)
	}
	if err := s.SetEnabled(ctx, viewer, true); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Minute)
	if err := s.SetEnabled(ctx, viewer, true); err != nil {
		t.Fatal(err)
	}
	var since int64
	if err := db.QueryRowContext(ctx, `SELECT enabled_at FROM viewer_alert_preferences WHERE viewer_id=?`, viewer.ID).Scan(&since); err != nil || since != time.Unix(1000, 0).UnixNano() {
		t.Fatalf("repeated opt-in: %d %v", since, err)
	}
	if err := s.SetEnabled(ctx, viewer, false); err != nil {
		t.Fatal(err)
	}
	if err := s.SetEnabled(ctx, viewer, true); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, `SELECT enabled_at FROM viewer_alert_preferences WHERE viewer_id=?`, viewer.ID).Scan(&since); err != nil || since != time.Unix(1060, 0).UnixNano() {
		t.Fatalf("new opt-in: %d %v", since, err)
	}
	active = false
	if _, err := s.Enabled(ctx, viewer); !errors.Is(err, core.ErrUnauthorized) {
		t.Fatal(err)
	}
	if err := s.SetEnabled(ctx, viewer, false); !errors.Is(err, core.ErrUnauthorized) {
		t.Fatal(err)
	}
}
