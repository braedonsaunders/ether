package playback

// Intent: ETHE-18 confines search-only refill to an admitted detail warm-up
// reusing a valid matching remembered resolution; all existing admission gates
// remain authoritative. This test owns the trigger boundary, not scheduling.

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

type refillCountingResolver struct {
	*fakeResolver
	refills atomic.Int32
}

func (r *refillCountingResolver) RefillSearchCache(*core.Title) <-chan struct{} {
	r.refills.Add(1)
	return nil
}

func TestRememberedDetailRefillTriggerAdmission(t *testing.T) {
	for _, mode := range []string{"detail", "trailer", "post-play", "no-resume", "mismatch", "blocked", "disabled", "cache-off", "detail-off", "active", "rate-limit", "per-user", "capacity", "already-warm"} {
		t.Run(mode, func(t *testing.T) {
			h := newHarness(t, []*fakeRelease{mkv("r1", 2<<20, 90*time.Minute), mkv("r2", 2<<20, 90*time.Minute)}, []string{"r1", "r2"})
			r := &refillCountingResolver{fakeResolver: h.resolver}
			h.m.deps.Resolver = r
			key := epKey
			caps := defaultCaps("detail")
			if mode != "no-resume" {
				name := "Boundary.Bay.S01E01.1080p.WEB-DL.H.264-GRP"
				if mode == "mismatch" {
					name = "Boundary.Bay.S01E02.1080p.WEB-DL.H.264-GRP"
				}
				if err := h.m.Remember(t.Context(), owner, key, caps, &core.Resolution{Release: core.Release{ID: "r1", Name: name}, NZB: []byte("nzb-r1")}); err != nil {
					t.Fatal(err)
				}
			}
			opts := core.PrewarmOptions{Reason: core.PrewarmDetail}
			wantAdmitted, wantRefills := true, int32(0)
			switch mode {
			case "detail", "already-warm":
				wantRefills = 1
			case "trailer":
				opts.Reason = core.PrewarmTrailer
			case "post-play":
				opts.Reason = core.PrewarmPostPlay
			case "blocked":
				h.resolver.MarkBad(t.Context(), "r1", "owner block")
			case "disabled", "cache-off", "detail-off":
				cfg := h.m.config()
				if mode == "disabled" {
					cfg.Cache.DisableSpeculation = true
				}
				if mode == "cache-off" {
					cfg.Cache.Mode = "off"
				}
				if mode == "detail-off" {
					cfg.Cache.DetailWarmup = "off"
				}
				h.m.SetConfig(cfg)
				wantAdmitted = false
			case "active":
				h.prepare(friend, movieKey, "playing")
				wantAdmitted = false
			case "rate-limit":
				h.m.mu.Lock()
				for range 30 {
					h.m.warmRequests[warmBudgetKey{userID: owner.ID, reason: core.PrewarmDetail}] = append(h.m.warmRequests[warmBudgetKey{userID: owner.ID, reason: core.PrewarmDetail}], h.clock.Now())
				}
				h.m.mu.Unlock()
				wantAdmitted = false
			case "per-user", "capacity":
				// Existing admission state: admitted warm-ups still in flight.
				h.m.mu.Lock()
				users := []int64{owner.ID}
				if mode == "capacity" {
					users = []int64{friend.ID, friend.ID + 1}
				}
				for _, userID := range users {
					h.m.warm[identity{userID: userID, clientID: "other", key: movieKey}] = &warmEntry{done: make(chan struct{}), started: h.clock.Now(), expires: h.clock.Now().Add(preResolveTTL)}
				}
				h.m.mu.Unlock()
				wantAdmitted = false
			}
			if admitted := h.m.Prewarm(context.Background(), owner, key, caps, opts); admitted != wantAdmitted {
				t.Fatalf("admission=%v want %v", admitted, wantAdmitted)
			}
			if wantAdmitted {
				waitWarm(t, h, owner.ID, caps.ClientID, key)
				if mode == "already-warm" && h.m.PreResolve(t.Context(), owner, key, caps) {
					t.Fatal("existing warm re-admitted")
				}
			}
			h.m.Close() // join every admitted worker before checking non-events
			if got := r.refills.Load(); got != wantRefills {
				t.Fatalf("refills=%d want %d", got, wantRefills)
			}
			if mode == "detail" && h.resolver.callCount() != 0 {
				t.Fatal("remembered source was resolved again")
			}
		})
	}
}
