package admin_test

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/braedonsaunders/ether/internal/admin"
	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
)

type fakePlexWatch struct {
	userID   int64
	plexID   int64
	on       bool
	resolved bool
	finish   error
}

func (f *fakePlexWatch) remember(userID, plexID int64) {
	f.userID, f.plexID = userID, plexID
}

func (f *fakePlexWatch) status() admin.PlexWatchStatus {
	return admin.PlexWatchStatus{Eligibility: "unchecked", AccountMatch: true, Enabled: f.on}
}

func (f *fakePlexWatch) Status(_ context.Context, userID, plexID int64) (admin.PlexWatchStatus, error) {
	f.remember(userID, plexID)
	return f.status(), nil
}
func (f *fakePlexWatch) BeginPIN(_ context.Context, userID, plexID int64) (admin.PlexWatchStatus, error) {
	f.remember(userID, plexID)
	st := f.status()
	st.PINCode = "ABCD"
	return st, nil
}
func (f *fakePlexWatch) FinishPIN(_ context.Context, userID, plexID int64) (admin.PlexWatchStatus, error) {
	f.remember(userID, plexID)
	if f.finish != nil {
		return admin.PlexWatchStatus{}, f.finish
	}
	return f.status(), nil
}
func (f *fakePlexWatch) SetEnabled(_ context.Context, userID, plexID int64, on bool) (admin.PlexWatchStatus, error) {
	f.remember(userID, plexID)
	f.on = on
	return f.status(), nil
}
func (f *fakePlexWatch) Disconnect(_ context.Context, userID, plexID int64) (admin.PlexWatchStatus, error) {
	f.remember(userID, plexID)
	return f.status(), nil
}
func (f *fakePlexWatch) Retry(_ context.Context, userID, plexID int64) (admin.PlexWatchStatus, error) {
	f.remember(userID, plexID)
	return f.status(), nil
}
func (f *fakePlexWatch) Resolve(_ context.Context, userID, plexID int64) (admin.PlexWatchStatus, error) {
	f.remember(userID, plexID)
	f.resolved = true
	return f.status(), nil
}
func (f *fakePlexWatch) ExportCurrent(_ context.Context, userID, plexID int64) (admin.PlexWatchStatus, error) {
	f.remember(userID, plexID)
	return f.status(), nil
}
func (f *fakePlexWatch) ImportCurrent(_ context.Context, userID, plexID int64) (admin.PlexWatchStatus, error) {
	f.remember(userID, plexID)
	return f.status(), nil
}

func TestPlexWatchUsesViewerPlexID(t *testing.T) {
	store := newStore(t, func(c *config.Config) {
		c.SetupComplete = true
		c.Plex.Token = ownerToken
	})
	watch := &fakePlexWatch{}
	h := admin.New(admin.Deps{
		Config: store, IsOwnerToken: storedOwner(store), PlexWatch: watch,
		ViewerDirectory: func(context.Context) ([]admin.ViewerRecord, error) {
			return []admin.ViewerRecord{{
				User: core.User{ID: 7, PlexID: 99, Username: "guest"},
			}}, nil
		},
	})

	r := req(http.MethodPut, "/ether/api/viewers/7/plex-watch", `{"enabled":true}`)
	r.Header.Set("X-Plex-Token", ownerToken)
	rec := serve(h, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("enable: %d %s", rec.Code, rec.Body.String())
	}
	if watch.userID != 7 || watch.plexID != 99 || !watch.on {
		t.Fatalf("bound user %d plex %d on %v", watch.userID, watch.plexID, watch.on)
	}
	if strings.Contains(rec.Body.String(), "token") {
		t.Fatalf("status leaked a token field: %s", rec.Body.String())
	}

	watch.finish = admin.ErrPlexAccountMismatch
	r = req(http.MethodPost, "/ether/api/viewers/7/plex-watch/pin/finish", "")
	r.Header.Set("X-Plex-Token", ownerToken)
	rec = serve(h, r)
	if rec.Code != http.StatusConflict {
		t.Fatalf("mismatch: %d %s", rec.Code, rec.Body.String())
	}
	if watch.plexID != 99 {
		t.Fatalf("finish used plex id %d", watch.plexID)
	}

	r = req(http.MethodPost, "/ether/api/viewers/7/plex-watch/resolve", "")
	r.Header.Set("X-Plex-Token", ownerToken)
	rec = serve(h, r)
	if rec.Code != http.StatusOK || !watch.resolved || watch.userID != 7 || watch.plexID != 99 {
		t.Fatalf("resolve: %d resolved=%v user=%d plex=%d", rec.Code, watch.resolved, watch.userID, watch.plexID)
	}
}
