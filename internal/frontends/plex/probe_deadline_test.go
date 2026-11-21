package plex

import (
	"context"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

// deadlineProbePlayback records the deadline ProbeMetadata is called with.
type deadlineProbePlayback struct {
	core.Playback
	deadline    time.Time
	hasDeadline bool
	calls       int
}

func (p *deadlineProbePlayback) ProbeMetadata(ctx context.Context, _ *core.User, _ core.RatingKey, _ core.ClientCaps) error {
	p.calls++
	p.deadline, p.hasDeadline = ctx.Deadline()
	return nil
}
func (p *deadlineProbePlayback) WarmProbeInfo(*core.User, core.ClientCaps, core.RatingKey) (core.MediaInfo, bool) {
	return core.MediaInfo{}, false
}
func (p *deadlineProbePlayback) Estimate(context.Context, *core.Title, core.ClientCaps) core.MediaInfo {
	return core.MediaInfo{Estimated: true}
}

func TestProbeDetailTracksKeepsVersionReserve(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	pb := &deadlineProbePlayback{Playback: f.playback}
	f.srv.setPlayback(pb)
	user := ownerUser
	title := &fightClub

	t.Run("allowance", func(t *testing.T) {
		pr := &Request{User: &user, ClientID: "android-reserve"}
		start := time.Now()
		pr.detailWaitUntil = start.Add(4 * time.Second)
		if err := f.srv.probeDetailTracks(context.Background(), pr, title); err != nil {
			t.Fatal(err)
		}
		if !pb.hasDeadline {
			t.Fatal("probe ran without the detail allowance deadline")
		}
		// The probe may use the allowance except the reserve.
		if want := pr.detailWaitUntil.Add(-detailVariantReserve); !pb.deadline.Equal(want) {
			t.Fatalf("probe deadline %s, want allowance minus reserve %s", pb.deadline, want)
		}
		// Version discovery runs after the probe, inside the same 4 s
		// bound (detailVariantContext ends no later than detailWaitUntil):
		// the probe deadline leaves it at least the reserve.
		const minReserve = 500 * time.Millisecond
		if left := pr.detailWaitUntil.Sub(pb.deadline); left < minReserve || left < detailVariantReserve {
			t.Fatalf("version discovery keeps %s after the probe deadline, want >= %s", left, minReserve)
		}
		vctx, cancel := detailVariantContext(context.Background(), pr)
		defer cancel()
		if vdl, ok := vctx.Deadline(); !ok || vdl.After(pr.detailWaitUntil) {
			t.Fatalf("version discovery %s exceeds the 4 s allowance %s", vdl, pr.detailWaitUntil)
		}
		if total := pb.deadline.Sub(start); total < 3*time.Second {
			t.Fatalf("probe gets only %s of the 4 s allowance", total)
		}
	})

	t.Run("no-allowance-passes-caller-ctx", func(t *testing.T) {
		pr := &Request{User: &user, ClientID: "desktop-reserve"}
		if err := f.srv.probeDetailTracks(context.Background(), pr, title); err != nil {
			t.Fatal(err)
		}
		if pb.hasDeadline {
			t.Fatalf("probe got a deadline %s without a detail allowance", pb.deadline)
		}
	})
}

type warmProbeWaitPlayback struct {
	core.Playback
	waitCalled bool
	waitBudget time.Duration
}

func (p *warmProbeWaitPlayback) WaitWarmProbe(_ context.Context, _ *core.User, _ core.ClientCaps, _ core.RatingKey, budget time.Duration) (core.MediaInfo, bool) {
	p.waitCalled = true
	p.waitBudget = budget
	return core.MediaInfo{}, false
}

func (p *warmProbeWaitPlayback) WarmProbeInfo(*core.User, core.ClientCaps, core.RatingKey) (core.MediaInfo, bool) {
	return core.MediaInfo{}, false
}

func (p *warmProbeWaitPlayback) Estimate(context.Context, *core.Title, core.ClientCaps) core.MediaInfo {
	return core.MediaInfo{Estimated: true}
}

func TestStartDetailPageWaitAndWarmDetail(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	user := ownerUser
	title := &fightClub

	for _, tc := range []struct {
		name     string
		pr       *Request
		wantWait bool
	}{
		{
			name:     "plex-web",
			pr:       &Request{User: &user, ClientID: "web-1", Product: "Plex Web", Platform: "Chrome"},
			wantWait: true,
		},
		{
			name:     "plex-xbox",
			pr:       &Request{User: &user, ClientID: "xbox-1", Product: "Plex for Xbox", Platform: "Xbox"},
			wantWait: true,
		},
		{
			name:     "plex-samsung-excluded",
			pr:       &Request{User: &user, ClientID: "samsung-1", Product: "Plex for Samsung", Platform: "Tizen"},
			wantWait: false,
		},
		{
			name:     "library-tool-excluded",
			pr:       &Request{User: &user, ClientID: "tautulli-1", Product: "Tautulli"},
			wantWait: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pb := &warmProbeWaitPlayback{Playback: f.playback}
			f.srv.setPlayback(pb)

			startDetailPageWait(tc.pr)
			if tc.wantWait && tc.pr.detailWaitUntil.IsZero() {
				t.Fatalf("expected non-zero detailWaitUntil for %s", tc.name)
			}
			if !tc.wantWait && !tc.pr.detailWaitUntil.IsZero() {
				t.Fatalf("expected zero detailWaitUntil for %s, got %s", tc.name, tc.pr.detailWaitUntil)
			}

			f.srv.warmDetail(context.Background(), tc.pr, title)
			if pb.waitCalled != tc.wantWait {
				t.Fatalf("WaitWarmProbe called=%v, want %v", pb.waitCalled, tc.wantWait)
			}
			if tc.wantWait && pb.waitBudget <= 0 {
				t.Fatalf("WaitWarmProbe budget=%s, want > 0", pb.waitBudget)
			}
		})
	}
}
