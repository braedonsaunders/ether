package playback

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

// Down-failover on a direct-play (MKV) session whose first healthy
// size-mismatched candidate is HIGHER bitrate than the starving release,
// followed by a lower-bitrate mismatch: the lower one is the useful remux
// fallback and must be the release the session switches to.
func TestDownFailoverDirectFallbackPicksLowerBitrateMismatch(t *testing.T) {
	const dur = 100 * time.Second
	uhd := mkv("UHD", 630_000_000, dur) // 6.3 MB/s
	uhd.Height = 2160
	h := newHarness(t, []*fakeRelease{
		mkv("A", 390_000_000, dur), // 3.9 MB/s, starving
		uhd,
		mkv("LOW", 200_000_000, dur), // 2.0 MB/s
	}, []string{"A", "UHD", "LOW"})
	ps := h.prepare(owner, movieKey, "c1")
	h.m.SetDecision(ps.ID, core.Decision{Kind: core.DecisionDirectPlay})
	s := h.m.lookup(ps.ID)

	g, err := h.m.failoverWith(context.Background(), s, s.current(), 10*time.Second,
		failoverOpts{reason: "source slower than playback", down: true})
	if !errors.Is(err, errRequireRemux) {
		t.Fatalf("down-failover err = %v, want remux switch", err)
	}
	if g == nil || g.releaseID() != "LOW" {
		t.Fatalf("down-failover release = %v, want LOW", g)
	}
	st, _ := h.m.Session(ps.ID)
	if st.Resolution.Release.ID != "LOW" || !st.RequiresRemux || st.DownFailovers != 1 || st.Failovers != 1 {
		t.Fatalf("down-failover state: %+v", st)
	}
}

func TestDirectFailoverFallbackRegressions(t *testing.T) {
	const dur = 100 * time.Second
	rels := func() []*fakeRelease {
		uhd := mkv("UHD", 630_000_000, dur) // 6.3 MB/s
		uhd.Height = 2160
		return []*fakeRelease{
			mkv("A", 390_000_000, dur),   // 3.9 MB/s
			uhd,                          //
			mkv("LOW", 200_000_000, dur), // 2.0 MB/s
			mkv("HIGH", 500_000_000, dur),
		}
	}

	t.Run("down: first mismatch already lower still switches to it", func(t *testing.T) {
		h := newHarness(t, rels(), []string{"A", "LOW", "UHD"})
		ps := h.prepare(owner, movieKey, "c1")
		h.m.SetDecision(ps.ID, core.Decision{Kind: core.DecisionDirectPlay})
		s := h.m.lookup(ps.ID)

		g, err := h.m.failoverWith(context.Background(), s, s.current(), 10*time.Second,
			failoverOpts{reason: "source slower than playback", down: true})
		if !errors.Is(err, errRequireRemux) || g == nil || g.releaseID() != "LOW" {
			t.Fatalf("down-failover = %v, %v; want remux switch to LOW", g, err)
		}
		if st, _ := h.m.Session(ps.ID); st.Resolution.Release.ID != "LOW" || !st.RequiresRemux || st.DownFailovers != 1 {
			t.Fatalf("down-failover state: %+v", st)
		}
	})

	t.Run("down: no lower-bitrate mismatch still finds no working release", func(t *testing.T) {
		h := newHarness(t, rels(), []string{"A", "UHD", "HIGH"})
		ps := h.prepare(owner, movieKey, "c1")
		h.m.SetDecision(ps.ID, core.Decision{Kind: core.DecisionDirectPlay})
		s := h.m.lookup(ps.ID)

		g, err := h.m.failoverWith(context.Background(), s, s.current(), 10*time.Second,
			failoverOpts{reason: "source slower than playback", down: true})
		if err == nil || errors.Is(err, errRequireRemux) || !errors.Is(err, core.ErrNotAvailable) {
			t.Fatalf("down-failover err = %v, want no working release", err)
		}
		if g != nil {
			t.Fatalf("down-failover release = %v, want none", g.releaseID())
		}
		if st, _ := h.m.Session(ps.ID); st.Resolution.Release.ID != "A" || st.RequiresRemux || st.DownFailovers != 0 || st.Failovers != 0 {
			t.Fatalf("failed down-failover changed session: %+v", st)
		}
	})

	t.Run("non-down: first mismatch still wins over a later lower one", func(t *testing.T) {
		h := newHarness(t, rels(), []string{"A", "UHD", "LOW"})
		ps := h.prepare(owner, movieKey, "c1")
		h.m.SetDecision(ps.ID, core.Decision{Kind: core.DecisionDirectPlay})
		s := h.m.lookup(ps.ID)

		g, err := h.m.failover(context.Background(), s, s.current(), 10*time.Second, "source read failed")
		if !errors.Is(err, errRequireRemux) || g == nil || g.releaseID() != "UHD" {
			t.Fatalf("failover = %v, %v; want remux switch to first mismatch UHD", g, err)
		}
		if st, _ := h.m.Session(ps.ID); st.Resolution.Release.ID != "UHD" || !st.RequiresRemux || st.DownFailovers != 0 || st.Failovers != 1 {
			t.Fatalf("failover state: %+v", st)
		}
	})
}
