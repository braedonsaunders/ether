package playback

import (
	"context"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

func TestSourcePlaybackProgressIsExact(t *testing.T) {
	h := newHarness(t, []*fakeRelease{mkv("A", 100_000, time.Minute)}, []string{"A"})
	ps := h.prepare(owner, movieKey, "viewer")
	other, _, err := h.m.PrepareSurfaceSource(context.Background(), owner, movieKey, core.ClientCaps{ClientID: "viewer", Protocol: "jellyfin"}, "", false)
	if err != nil {
		t.Fatal(err)
	}
	revision := core.SourceRevision(ps)
	apply := func(id, rev string, viewer int64, client string, key core.RatingKey) error {
		return h.m.SourcePlaybackProgress(context.Background(), id, rev, viewer, client, key, time.Second, "paused")
	}
	for _, tc := range []struct {
		id, rev string
		viewer  int64
		client  string
		key     core.RatingKey
	}{
		{ps.ID, revision, owner.ID + 1, ps.ClientID, ps.Key},
		{ps.ID, revision, owner.ID, "foreign", ps.Key},
		{ps.ID, revision, owner.ID, ps.ClientID, core.MovieKey(999)},
		{ps.ID, "stale", owner.ID, ps.ClientID, ps.Key},
	} {
		if apply(tc.id, tc.rev, tc.viewer, tc.client, tc.key) == nil {
			t.Fatal("accepted foreign activity")
		}
	}
	if err := apply(ps.ID, revision, ps.UserID, ps.ClientID, ps.Key); err != nil {
		t.Fatal(err)
	}
	snap, _ := h.m.Session(ps.ID)
	untouched, _ := h.m.Session(other.ID)
	if snap.ViewerState != "paused" || snap.ViewOffset == nil || *snap.ViewOffset != 1000 {
		t.Fatal("exact activity missing")
	}
	if untouched.ViewOffset != nil || untouched.ViewerState == "paused" {
		t.Fatal("activity fanned out to another surface")
	}
	h.m.Stop(ps.ID)
	if apply(ps.ID, revision, ps.UserID, ps.ClientID, ps.Key) == nil {
		t.Fatal("stopped exact source revived")
	}
}
