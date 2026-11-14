package livetv

import (
	"context"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
)

type unavailableBroadcastPlayback struct {
	core.Playback
	attempts atomic.Int64
}

func (p *unavailableBroadcastPlayback) Prepare(context.Context, *core.User, core.RatingKey, core.ClientCaps) (*core.PlaySession, error) {
	p.attempts.Add(1)
	return nil, core.ErrNotAvailable
}

// A failed virtual rundown must keep publishing the live Signal generator while
// its bounded candidate attempts retry. It must never settle onto black fill.
func TestBroadcastAllCandidatesFailUsesLiveSignal(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		playback := &unavailableBroadcastPlayback{}
		s := testService(t, Options{Playback: playback, StyleScreens: func(string) *config.LiveTVScreens { return &config.LiveTVScreens{NoSignal: "static", ApplyTo: "all"} }})
		c := testChannel()
		c.Look = &ChannelLook{LookID: "signal-test"}
		c.Items[0].DurationMS = 3600000
		c.Items[1].DurationMS = 3600000
		runner := &broadcastFakeRunner{}
		format := smallBroadcastFormat()
		planner := &scheduleBroadcastPlanner{s: s, channel: c, user: core.User{ID: 1, IsOwner: true}, format: format, runner: runner}
		b, err := newBroadcast(t.Context(), runner, planner, format)
		if err != nil {
			t.Fatal(err)
		}
		defer b.close()
		if _, err = b.addRendition(t.Context(), "main", core.Decision{}); err != nil {
			t.Fatal(err)
		}
		b.start()
		// Admission now retries the on-air candidate at the bounded 15s cadence.
		// Observe two retries without requiring unready alternates to be probed.
		time.Sleep(32 * time.Second)
		synctest.Wait()
		b.close()
		frames := runner.encoders[0].snapshot()
		if len(frames) < 500 {
			t.Fatalf("continuous ticks=%d", len(frames))
		}
		for i, frame := range frames[45:] {
			sum := 0
			for _, v := range frame.Video[:4] {
				sum += int(v)
			}
			if sum <= 64 {
				t.Fatalf("black fill after deadline at tick %d", i+45)
			}
		}
		if playback.attempts.Load() < 2 {
			t.Fatalf("candidate retries stopped: %d", playback.attempts.Load())
		}
	})
}
