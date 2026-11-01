package app

import (
	"context"
	"errors"
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/identity"
	"github.com/braedonsaunders/ether/internal/trackprefs"
)

func TestTrackPreferencesAreSharedAndViewerScoped(t *testing.T) {
	ctx := context.Background()
	a := setupTestApp(t, true)
	local, err := identity.NewLocal(a.db)
	if err != nil {
		t.Fatal(err)
	}
	id, err := local.CreateViewer(ctx, "alice", "password-1234")
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

func TestScopedTrackPreferencesReachSharedSelector(t *testing.T) {
	ctx := context.Background()
	a := setupTestApp(t, true)
	local, err := identity.NewLocal(a.db)
	if err != nil {
		t.Fatal(err)
	}
	id, err := local.CreateViewer(ctx, "scoped-viewer", "password-1234")
	if err != nil {
		t.Fatal(err)
	}
	if err = a.users.RefreshViewers(ctx); err != nil {
		t.Fatal(err)
	}
	viewer := &frontend.Viewer{ID: id}
	svc := surfaceTrackPreferences{app: a}
	lang := []string{"fr"}
	cap := 2500
	if err = svc.SetScoped(ctx, viewer, "device", "tv", frontend.TrackPreferencePatch{AudioLanguages: &lang, MaxBitrateKbps: &cap}); err != nil {
		t.Fatal(err)
	}
	tracks, bitrate, _, _, sources, err := svc.GetScoped(ctx, viewer, frontend.TrackScope{Device: "tv"})
	if err != nil || bitrate != 2500 || len(tracks.AudioLanguages) != 1 || tracks.AudioLanguages[0] != "fr" || sources["audio"] != "device" {
		t.Fatalf("shared scope: %+v %d %v %v", tracks, bitrate, sources, err)
	}
	info := core.MediaInfo{Streams: []core.StreamInfo{{Type: "audio", Language: "en"}, {Type: "audio", Language: "fr"}}}
	selected, applied, err := a.trackprefs.SelectScoped(ctx, id, core.MovieKey(1), info, "", trackprefs.Scope{Device: "tv"})
	if err != nil || !applied || selected.Audio != 1 {
		t.Fatalf("selection: %+v %v %v", selected, applied, err)
	}
}
