package livetv

import (
	"context"
	"testing"
	"testing/synctest"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/transcode"
)

type coldHoldPlanner struct {
	elementBroadcastPlanner
	screenPlan broadcastPlan
	card       broadcastPlan
}

func (p coldHoldPlanner) screen(time.Time) broadcastPlan         { return p.screenPlan }
func (p coldHoldPlanner) momentScreenAt(time.Time) broadcastPlan { return p.card }

func coldHoldOpen(f transcode.RawFormat, value byte, delay time.Duration) func(context.Context) (transcode.Decoder, func(), error) {
	return func(context.Context) (transcode.Decoder, func(), error) {
		return &delayedIntroProgramme{Decoder: &broadcastFakeDecoder{format: f, closed: make(chan struct{}), value: value}, delay: delay}, nil, nil
	}
}

func newColdHoldBroadcast(t *testing.T, planner coldHoldPlanner, f transcode.RawFormat) (*broadcast, *broadcastFakeRunner) {
	t.Helper()
	runner := &broadcastFakeRunner{}
	b, err := newBroadcast(t.Context(), runner, planner, f)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.addRendition(t.Context(), "main", core.Decision{}); err != nil {
		b.close()
		t.Fatal(err)
	}
	b.start()
	return b, runner
}

// A cold tune-in lands in an ident pod whose master is not rendered yet, so
// the live ident plate airs. The programme behind it takes longer than the
// pod to open (a cold remote source): the ident look must hold the whole
// wait and the screen — the styled off-air card — must never appear first.
func TestColdTuneInHoldsIdentLookUntilFirstProgramme(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		origin := time.Now()
		f := smallBroadcastFormat()
		pod := broadcastPlan{id: "pod", element: "ident", interstitial: true, start: origin, end: origin.Add(4 * time.Second), open: func(context.Context) (transcode.Decoder, func(), error) {
			return &delayedMomentDecoder{broadcastFakeDecoder: &broadcastFakeDecoder{format: f, closed: make(chan struct{}), value: 200}, ready: origin.Add(30 * time.Second)}, nil, nil
		}}
		programme := broadcastPlan{id: "programme", element: "programme", programme: true, start: pod.end, end: origin.Add(10 * time.Minute), open: coldHoldOpen(f, 90, 8*time.Second)}
		planner := coldHoldPlanner{
			elementBroadcastPlanner: elementBroadcastPlanner{pod, programme},
			screenPlan:              broadcastPlan{id: "screen", open: coldHoldOpen(f, 10, 0)},
			card:                    broadcastPlan{id: "moment-card", element: "card", open: coldHoldOpen(f, 30, 0)},
		}
		b, runner := newColdHoldBroadcast(t, planner, f)
		time.Sleep(20 * time.Second)
		synctest.Wait()
		b.close()
		frames := runner.encoders[0].snapshot()
		if len(frames) < 590 {
			t.Fatalf("output stopped: %d frames", len(frames))
		}
		programmeAt := -1
		for tick, frame := range frames {
			v := frame.Video[0]
			if v == 10 {
				t.Fatalf("screen aired before the first programme at tick %d", tick)
			}
			if v == 90 && programmeAt < 0 {
				programmeAt = tick
			}
			if programmeAt < 0 && v != 30 {
				t.Fatalf("tick %d: want held ident look 30, got %d", tick, v)
			}
		}
		if programmeAt < 0 {
			t.Fatal("programme never replaced the held ident look")
		}
	})
}

// A cold tune-in directly into a programme whose source opens slowly holds
// the ident plate, never the screen. Once the cold bound expires without a
// picture, the existing screen behaviour takes over.
func TestColdTuneInDirectProgrammeHoldIsBounded(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		origin := time.Now()
		f := smallBroadcastFormat()
		programme := broadcastPlan{id: "programme", element: "programme", programme: true, start: origin, end: origin.Add(10 * time.Minute), open: coldHoldOpen(f, 90, 10*time.Minute)}
		planner := coldHoldPlanner{
			elementBroadcastPlanner: elementBroadcastPlanner{programme},
			screenPlan:              broadcastPlan{id: "screen", open: coldHoldOpen(f, 10, 0)},
			card:                    broadcastPlan{id: "moment-card", element: "card", open: coldHoldOpen(f, 30, 0)},
		}
		b, runner := newColdHoldBroadcast(t, planner, f)
		time.Sleep(broadcastColdProgrammeWait + 10*time.Second)
		synctest.Wait()
		b.close()
		frames := runner.encoders[0].snapshot()
		bound := int(broadcastColdProgrammeWait * 30 / time.Second)
		if len(frames) <= bound {
			t.Fatalf("output stopped: %d frames", len(frames))
		}
		for tick := 0; tick < bound; tick++ {
			if v := frames[tick].Video[0]; v != 30 {
				t.Fatalf("tick %d: want held ident look 30 before the cold bound, got %d", tick, v)
			}
		}
		screenAired := false
		for _, frame := range frames[bound:] {
			if frame.Video[0] == 10 {
				screenAired = true
				break
			}
		}
		if !screenAired {
			t.Fatal("screen never took over after the cold bound expired")
		}
	})
}

// A real scheduled off-air element airs as itself on a cold tune-in; the
// cold hold only covers a programme that is still preparing.
func TestColdTuneInScheduledOffAirStillAirs(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		origin := time.Now()
		f := smallBroadcastFormat()
		offair := broadcastPlan{id: "offair", element: "offair", start: origin, end: origin.Add(10 * time.Minute), open: coldHoldOpen(f, 55, 0)}
		planner := coldHoldPlanner{
			elementBroadcastPlanner: elementBroadcastPlanner{offair},
			screenPlan:              broadcastPlan{id: "screen", open: coldHoldOpen(f, 10, 0)},
			card:                    broadcastPlan{id: "moment-card", element: "card", open: coldHoldOpen(f, 30, 0)},
		}
		b, runner := newColdHoldBroadcast(t, planner, f)
		time.Sleep(6 * time.Second)
		synctest.Wait()
		b.close()
		frames := runner.encoders[0].snapshot()
		if len(frames) < 179 {
			t.Fatalf("output stopped: %d frames", len(frames))
		}
		for tick, frame := range frames {
			if v := frame.Video[0]; v != 55 {
				t.Fatalf("tick %d: want scheduled off-air 55, got %d", tick, v)
			}
		}
	})
}
