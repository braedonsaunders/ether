package livetv

import (
	"context"
	"testing"
	"testing/synctest"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/transcode"
)

type momentDeadlinePlanner struct {
	programme, moment, after, fallback, card broadcastPlan
}

func (p momentDeadlinePlanner) plans(now time.Time) (broadcastPlan, *broadcastPlan) {
	if now.Before(p.moment.start) {
		return p.programme, &p.moment
	}
	if now.Before(p.moment.end) {
		return p.moment, &p.after
	}
	return p.after, nil
}
func (p momentDeadlinePlanner) screen(time.Time) broadcastPlan { return p.fallback }
func (p momentDeadlinePlanner) momentScreen() broadcastPlan    { return p.card }

type delayedMomentDecoder struct {
	*broadcastFakeDecoder
	ready time.Time
}

func (d *delayedMomentDecoder) Next(ctx context.Context) (transcode.RawFrame, error) {
	if delay := time.Until(d.ready); delay > 0 {
		select {
		case <-ctx.Done():
			return transcode.RawFrame{}, ctx.Err()
		case <-time.After(delay):
		}
	}
	return d.broadcastFakeDecoder.Next(ctx)
}

// A late rendered master cannot move a boundary or replace its live fallback
// mid-moment. The following programme cuts on its original frame as well.
func TestBroadcastMomentDeadlineKeepsWholeFallbackInterval(t *testing.T) {
	for _, moment := range []string{"ident", "plugin:weather/forecast"} {
		for _, lead := range []time.Duration{0, 8 * time.Second} {
			t.Run(moment+"/"+lead.String(), func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					origin := time.Now()
					format := smallBroadcastFormat()
					open := func(value byte) func(context.Context) (transcode.Decoder, func(), error) {
						return func(context.Context) (transcode.Decoder, func(), error) {
							return &broadcastFakeDecoder{format: format, value: value, closed: make(chan struct{})}, nil, nil
						}
					}
					p := momentDeadlinePlanner{
						programme: broadcastPlan{id: "before", end: origin.Add(lead), open: open(80)},
						moment:    broadcastPlan{id: moment, element: moment, interstitial: true, liveMoment: true, start: origin.Add(lead), end: origin.Add(lead + 4*time.Second)},
						after:     broadcastPlan{id: "after", start: origin.Add(lead + 4*time.Second), open: open(90)},
						fallback:  broadcastPlan{id: "reconnecting", open: open(10)},
						card:      broadcastPlan{id: "style-card", open: open(30)},
					}
					p.moment.open = func(context.Context) (transcode.Decoder, func(), error) {
						return &delayedMomentDecoder{broadcastFakeDecoder: &broadcastFakeDecoder{format: format, value: 200, closed: make(chan struct{})}, ready: origin.Add(lead + 60*time.Second)}, nil, nil
					}
					if lead == 0 {
						p.card.open = func(context.Context) (transcode.Decoder, func(), error) {
							return &delayedMomentDecoder{broadcastFakeDecoder: &broadcastFakeDecoder{format: format, value: 30, closed: make(chan struct{})}, ready: origin.Add(500 * time.Millisecond)}, nil, nil
						}
					}
					runner := &broadcastFakeRunner{}
					b, err := newBroadcast(t.Context(), runner, p, format)
					if err != nil {
						t.Fatal(err)
					}
					defer b.close()
					if _, err := b.addRendition(t.Context(), "main", core.Decision{MaxHeight: 720}); err != nil {
						t.Fatal(err)
					}
					b.start()
					time.Sleep(14 * time.Second)
					synctest.Wait()
					b.close()
					frames := runner.encoders[0].snapshot()
					if len(frames) < 420 {
						t.Fatalf("output stopped: %d ticks", len(frames))
					}
					first := 30
					if lead == 0 {
						first = 0
					}
					for tick := first; tick < 420; tick++ {
						want := byte(80)
						if tick >= int((lead+4*time.Second)*30/time.Second) {
							want = 90
						} else if tick >= int(lead*30/time.Second) {
							want = 30
						}
						if frames[tick].Video[0] != want {
							t.Fatalf("tick %d: picture %d, want %d", tick, frames[tick].Video[0], want)
						}
					}
				})
			})
		}
	}
}
