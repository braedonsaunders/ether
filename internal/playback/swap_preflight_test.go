package playback

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

// fixedFormatHLS is a session whose delivered format cannot change (the
// transcoder's segmented-Matroska contract): it refuses HDR replacements.
type fixedFormatHLS struct {
	*fakeHLS
}

var errFixedFormat = errors.New("replacement cannot match the delivered video")

func (h *fixedFormatHLS) CheckSwap(info core.MediaInfo) error {
	if v := info.Video(); v != nil && v.ColorTransfer == "smpte2084" {
		return errFixedFormat
	}
	return nil
}

func (e *fakeEngine) sourcesOf(id string) []*fakeSource {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []*fakeSource
	for _, s := range e.sources {
		if s.rel.ID == id {
			out = append(out, s)
		}
	}
	return out
}

func assertPreflightSkipped(t *testing.T, h *harness, hls *fixedFormatHLS, ps2 *core.PlaySession, err error) {
	t.Helper()
	if err != nil || ps2.Resolution.Release.ID != "C" {
		t.Fatalf("failover: %v %+v", err, ps2)
	}
	select {
	case c := <-hls.swapC:
		if v := c.Info.Video(); v == nil || v.ColorTransfer != "" {
			t.Fatalf("swapped to an incompatible release: %+v", c.Info)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no input swap")
	}
	if len(hls.swaps) != 1 {
		t.Fatalf("swaps = %+v", hls.swaps)
	}
	if h.resolver.isBad("B") {
		t.Fatal("a session-specific format mismatch marked the release bad")
	}
	srcs := h.engine.sourcesOf("B")
	if len(srcs) == 0 {
		t.Fatal("B was never opened")
	}
	deadline := time.Now().Add(5 * time.Second)
	for _, src := range srcs {
		for !src.closed.Load() {
			if time.Now().After(deadline) {
				t.Fatal("rejected generation was not released")
			}
			time.Sleep(time.Millisecond)
		}
	}
}

// The serial failover loop checks a healthy candidate against the
// running stream before publishing it and moves on to the next.
func TestFailoverSkipsReplacementTheHLSStreamCannotCarry(t *testing.T) {
	b := mkv("B", 100_000, time.Minute)
	b.HDR = true
	h := newHarness(t, []*fakeRelease{mkv("A", 100_000, time.Minute), b, mkv("C", 100_000, time.Minute)}, []string{"A", "B", "C"})
	ps := h.prepare(owner, movieKey, "preflight-serial")
	hls := &fixedFormatHLS{fakeHLS: h.tr.addHLS(ps.ID)}
	h.m.deps.HLSLookup = func(string) (core.HLSSession, bool) { return hls, true }
	h.m.SetDecision(ps.ID, core.Decision{Kind: core.DecisionRemux, VideoCopy: true, AudioCopy: true})
	ps2, err := h.m.Failover(context.Background(), ps.ID, 10*time.Second)
	assertPreflightSkipped(t, h, hls, ps2, err)
}

// Raced alternates get the same check: an incompatible winner of the
// open race counts as failed and the race continues.
func TestFailoverRaceSkipsReplacementTheHLSStreamCannotCarry(t *testing.T) {
	b := mkv("B", 100_000, time.Minute)
	b.HDR = true
	c := mkv("C", 100_000, time.Minute)
	c.ReadDelay = 50 * time.Millisecond // B opens first
	h := newHarness(t, []*fakeRelease{mkv("A", 100_000, time.Minute), b, c}, []string{"A", "B", "C"})
	h.m.deps.FetchNZB = func(ctx context.Context, r core.Release) ([]byte, error) {
		return []byte("nzb-" + r.ID), nil
	}
	ps := h.prepare(owner, movieKey, "preflight-race")
	hls := &fixedFormatHLS{fakeHLS: h.tr.addHLS(ps.ID)}
	h.m.deps.HLSLookup = func(string) (core.HLSSession, bool) { return hls, true }
	h.m.SetDecision(ps.ID, core.Decision{Kind: core.DecisionRemux, VideoCopy: true, AudioCopy: true})
	ps2, err := h.m.Failover(context.Background(), ps.ID, 10*time.Second)
	assertPreflightSkipped(t, h, hls, ps2, err)
}

// Without any acceptable candidate the failover fails before touching
// the running stream, rather than splicing in a mismatched release.
func TestFailoverWithOnlyIncompatibleReplacementsKeepsStream(t *testing.T) {
	b := mkv("B", 100_000, time.Minute)
	b.HDR = true
	h := newHarness(t, []*fakeRelease{mkv("A", 100_000, time.Minute), b}, []string{"A", "B"})
	ps := h.prepare(owner, movieKey, "preflight-none")
	hls := &fixedFormatHLS{fakeHLS: h.tr.addHLS(ps.ID)}
	h.m.deps.HLSLookup = func(string) (core.HLSSession, bool) { return hls, true }
	h.m.SetDecision(ps.ID, core.Decision{Kind: core.DecisionRemux, VideoCopy: true, AudioCopy: true})
	if _, err := h.m.Failover(context.Background(), ps.ID, 10*time.Second); err == nil {
		t.Fatal("failover succeeded with only an incompatible replacement")
	}
	if len(hls.swaps) != 0 {
		t.Fatalf("swapped: %+v", hls.swaps)
	}
	if s := h.m.lookup(ps.ID); s == nil || s.current().releaseID() != "A" {
		t.Fatal("the incompatible replacement was published")
	}
	if h.resolver.isBad("B") {
		t.Fatal("format mismatch marked the release bad")
	}
}

type picturePreparedHLS struct {
	*fixedFormatHLS
	rejected chan struct{}
	once     sync.Once
}

func (h *picturePreparedHLS) PrepareSwap(ctx context.Context, input string, info core.MediaInfo, at time.Duration) error {
	if info.Video().Width == 1280 {
		if h.rejected != nil {
			h.once.Do(func() { close(h.rejected) })
		}
		return errors.New("picture sample timed out")
	}
	return nil
}
func TestFailoverRejectsUnreadablePictureBeforePublication(t *testing.T) {
	for _, race := range []bool{false, true} {
		t.Run(fmt.Sprint(race), func(t *testing.T) {
			b := mkv("B", 100_000, time.Minute)
			h := newHarness(t, []*fakeRelease{mkv("A", 100_000, time.Minute), b, mkv("C", 100_000, time.Minute)}, []string{"A", "B", "C"})

			ps := h.prepare(owner, movieKey, "picture-preflight")
			hls := &picturePreparedHLS{fixedFormatHLS: &fixedFormatHLS{fakeHLS: h.tr.addHLS(ps.ID)}, rejected: make(chan struct{})}
			if race {
				h.m.deps.FetchNZB = func(ctx context.Context, r core.Release) ([]byte, error) {
					if r.ID == "C" {
						select {
						case <-hls.rejected:
						case <-ctx.Done():
							return nil, ctx.Err()
						}
					}
					return []byte("nzb-" + r.ID), nil
				}
			}
			h.m.deps.HLSLookup = func(string) (core.HLSSession, bool) { return hls, true }
			// Only B's picture probe is unavailable; it is otherwise healthy.
			h.m.deps.AugmentMedia = func(ctx context.Context, title *core.Title, user *core.User, res *core.Resolution, src core.MediaSource, info *core.MediaInfo) {
				if res.Release.ID == "B" {
					info.Video().Width = 1280
				}
			}
			h.m.SetDecision(ps.ID, core.Decision{Kind: core.DecisionRemux, VideoCopy: true, AudioCopy: true})
			next, err := h.m.Failover(t.Context(), ps.ID, 10*time.Second)
			assertPreflightSkipped(t, h, hls.fixedFormatHLS, next, err)
		})
	}
}
