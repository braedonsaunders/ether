package playback

import (
	"context"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

func TestInvalidateTitleDropsWarmResolution(t *testing.T) {
	h := newHarness(t, []*fakeRelease{mkv("r1", 2<<30, 90*time.Minute)}, []string{"r1"})
	caps := defaultCaps("client")
	if !h.m.PreResolve(context.Background(), owner, movieKey, caps) {
		t.Fatal("warm skipped")
	}
	h.m.mu.Lock()
	e := h.m.warm[identity{userID: owner.ID, clientID: caps.ClientID, key: movieKey}]
	h.m.mu.Unlock()
	select {
	case <-e.done:
	case <-time.After(time.Second):
		t.Fatal("warm did not complete")
	}
	if n := h.m.InvalidateTitle(movie2); n != 0 {
		t.Fatalf("other title dropped %d entries", n)
	}
	if n := h.m.InvalidateTitle(movieKey); n != 1 {
		t.Fatalf("dropped %d entries, want 1", n)
	}
	if _, err := h.m.Prepare(context.Background(), owner, movieKey, caps); err != nil {
		t.Fatal(err)
	}
	h.resolver.mu.Lock()
	calls := h.resolver.calls
	h.resolver.mu.Unlock()
	if calls != 2 {
		t.Fatalf("play reused an invalidated warm resolution: %d resolver calls", calls)
	}
}

func TestInvalidateTitleShowAndSeasonKeys(t *testing.T) {
	h := newHarness(t, nil, nil)
	ep := func(show int64, season, episode int) identity {
		return identity{userID: 1, clientID: "c", key: core.EpisodeKey(show, season, episode)}
	}
	reset := func() {
		h.m.mu.Lock()
		h.m.warm = map[identity]*warmEntry{
			ep(10, 1, 1): {}, ep(10, 2, 1): {}, ep(11, 1, 1): {},
			{userID: 1, clientID: "c", key: core.MovieKey(10)}: {},
		}
		h.m.mu.Unlock()
	}
	reset()
	if n := h.m.InvalidateTitle(core.SeasonKey(10, 1)); n != 1 {
		t.Fatalf("season dropped %d, want 1", n)
	}
	reset()
	if n := h.m.InvalidateTitle(core.ShowKey(10)); n != 2 {
		t.Fatalf("show dropped %d, want 2", n)
	}
	reset()
	if n := h.m.InvalidateTitle(core.EpisodeKey(11, 1, 1)); n != 1 {
		t.Fatalf("episode dropped %d, want 1", n)
	}
}

func TestInvalidateTitleDropsRememberedSourcesForEveryViewer(t *testing.T) {
	h := newHarness(t, []*fakeRelease{mkv("fresh", 10000, time.Minute)}, []string{"fresh"})
	ctx := t.Context()
	caps := defaultCaps("client")
	old := &core.Resolution{Release: core.Release{ID: "old"}, NZB: []byte("nzb-old")}
	for _, user := range []*core.User{owner, friend} {
		if err := h.m.Remember(ctx, user, movieKey, caps, old); err != nil {
			t.Fatal(err)
		}
	}
	if err := h.m.Remember(ctx, owner, movie2, caps, old); err != nil {
		t.Fatal(err)
	}
	h.m.InvalidateTitle(movieKey)
	for _, user := range []*core.User{owner, friend} {
		if p, err := h.m.ResumePoint(ctx, user.ID, movieKey); err != nil || p != nil {
			t.Fatalf("viewer %d retained stale source: %+v %v", user.ID, p, err)
		}
	}
	if p, err := h.m.ResumePoint(ctx, owner.ID, movie2); err != nil || p == nil {
		t.Fatalf("unrelated source lost: %+v %v", p, err)
	}
	ps := h.prepare(owner, movieKey, "client")
	if ps.Resolution.Release.ID != "fresh" || h.resolver.callCount() != 1 {
		t.Fatal("owner release change was bypassed by a remembered source")
	}
}
