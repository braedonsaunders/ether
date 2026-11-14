package playback

import (
	"context"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

type slotTestResolver struct {
	core.Resolver
	selector string
	release  string
}

func (r slotTestResolver) Resolve(ctx context.Context, req core.ResolveRequest) (*core.Resolution, error) {
	choice := req.Prefer
	if choice == r.selector {
		req.Prefer = r.release
	}
	res, err := r.Resolver.Resolve(ctx, req)
	if res != nil && choice == r.selector && res.Release.ID == r.release {
		cp := *res
		cp.VersionSelector = choice
		res = &cp
	}
	return res, err
}
func TestConfiguredVersionSlotResume(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, []*fakeRelease{mkv("A", 10000, time.Minute), mkv("B", 20000, time.Minute)}, []string{"A", "B"})
	selector := core.VersionSelectorPrefix + "test-choice"
	h.m.deps.Resolver = slotTestResolver{Resolver: h.resolver, selector: selector, release: "B"}
	caps := defaultCaps("slot-client")
	// A remembered Auto source must not replace a successful logical pick
	// simply because its concrete release id differs from the selector.
	if _, err := h.m.Prepare(ctx, owner, movieKey, caps); err != nil {
		t.Fatal(err)
	}
	picked, err := h.m.PrepareVariant(ctx, owner, movieKey, caps, selector)
	if err != nil || !picked.Resolution.MatchesVersion(selector) || picked.Resolution.Release.ID != "B" {
		t.Fatalf("slot pick: %+v %v", picked, err)
	}
	h.m.mu.Lock()
	stale := h.m.preferenceStaleLocked(movieKey, selector, h.m.now())
	h.m.mu.Unlock()
	if stale {
		t.Fatal("logical success marked unavailable")
	}
	h.m.Stop(picked.ID)
	resumed, err := h.m.PrepareVariant(ctx, owner, movieKey, caps, selector)
	if err != nil || !resumed.Resolution.MatchesVersion(selector) || resumed.Resolution.Release.ID != "B" {
		t.Fatalf("slot resume: %+v %v", resumed, err)
	}
	h.m.Stop(resumed.ID)
	missing := selector + "-unavailable"
	h.m.deps.Resolver = slotTestResolver{Resolver: h.resolver, selector: missing, release: "missing"}
	fallback, err := h.m.PrepareVariant(ctx, owner, movieKey, caps, missing)
	if err != nil || fallback == nil || fallback.Resolution.MatchesVersion(missing) {
		t.Fatalf("unavailable slot fallback: %+v %v", fallback, err)
	}
	h.m.mu.Lock()
	stale = h.m.preferenceStaleLocked(movieKey, missing, h.m.now())
	h.m.mu.Unlock()
	if !stale {
		t.Fatal("unavailable slot did not retain the normal fallback cooldown")
	}

}
