package notify

import (
	"context"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
)

func TestViewerPushRejectsNonPublicEndpoints(t *testing.T) {
	ctx := context.Background()
	db := store.OpenTest(t)
	testPushGrants(t, db)
	now := time.Unix(1000, 0)
	prefs := &ViewerAlerts{
		DB:  db,
		Now: func() time.Time { return now },
		Active: func(_ context.Context, id int64) (*core.User, error) {
			return &core.User{ID: id}, nil
		},
	}
	push := &ViewerPush{
		Notifications: New(db, Options{}),
		Preferences:   prefs,
		Now:           func() time.Time { return now },
		Allowed:       func(context.Context, *core.User, core.RatingKey) (bool, error) { return true, nil },
	}
	viewer := &core.User{ID: 1}
	for _, endpoint := range []string{
		"https://127.0.0.1:8443/hook",
		"https://10.0.0.5/hook",
		"https://169.254.169.254/latest/meta-data",
		"https://[::1]/hook",
		"https://localhost/hook",
		"https://printer.local/hook",
	} {
		sub := testViewerPushSubscription(t, endpoint)
		if err := push.Register(ctx, viewer, sub, testGrant(viewer.ID)); err == nil {
			t.Errorf("Register(%q) succeeded, want error", endpoint)
		}
	}
}
