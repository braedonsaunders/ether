package app

import (
	"context"
	"errors"
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/identity"
)

func TestTrackPreferencesAreSharedAndViewerScoped(t *testing.T) {
	ctx := context.Background()
	a := setupTestApp(t, true)
	local, err := identity.NewLocal(a.db)
	if err != nil {
		t.Fatal(err)
	}
	id, err := local.CreateViewer(ctx, "alice", "password")
	if err != nil {
		t.Fatal(err)
	}
	if err = a.users.RefreshViewers(ctx); err != nil {
		t.Fatal(err)
	}
	svc := surfaceTrackPreferences{app: a}
	v := &frontend.Viewer{ID: id}
	p, err := svc.Get(ctx, v)
	if err != nil {
		t.Fatal(err)
	}
	p.SubtitleMode = "always"
	p.SubtitleLanguages = []string{"fr"}
	if err = svc.SetSubtitle(ctx, v, p.SubtitleLanguages, p.SubtitleMode); err != nil {
		t.Fatal(err)
	}
	actual, err := a.trackprefs.GetRules(ctx, id)
	if err != nil || actual.SubtitleMode != "always" || len(actual.SubtitleLanguages) != 1 || actual.SubtitleLanguages[0] != "fr" {
		t.Fatalf("shared %+v %v", actual, err)
	}
	if err = a.users.SetOwnerEnabled(ctx, id, false); err != nil {
		t.Fatal(err)
	}
	if _, err = svc.Get(ctx, v); !errors.Is(err, core.ErrUnauthorized) {
		t.Fatalf("inactive %v", err)
	}
}
