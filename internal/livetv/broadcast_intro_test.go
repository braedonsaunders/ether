package livetv

import (
	"context"
	"encoding/binary"
	"testing"
	"testing/synctest"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/transcode"
)

type introFillPlanner struct {
	broadcastFakePlanner
	intro broadcastPlan
}

func (p introFillPlanner) introPlan(time.Time) (broadcastPlan, time.Duration) {
	return p.intro, 4 * time.Second
}

type delayedIntroProgramme struct {
	transcode.Decoder
	delay  time.Duration
	waited bool
}

func (d *delayedIntroProgramme) Next(ctx context.Context) (transcode.RawFrame, error) {
	if !d.waited {
		d.waited = true
		select {
		case <-ctx.Done():
			return transcode.RawFrame{}, ctx.Err()
		case <-time.After(d.delay):
		}
	}
	frame, err := d.Decoder.Next(ctx)
	amplitude := int16(-10000)
	for n := 0; n+2 <= len(frame.PCM); n += 2 {
		binary.LittleEndian.PutUint16(frame.PCM[n:], uint16(amplitude))
	}
	return frame, err
}

func TestColdIdentFillMinimumCapAndCrossfade(t *testing.T) {
	for _, ready := range []time.Duration{300 * time.Millisecond, 6 * time.Second} {
		t.Run(ready.String(), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				f := smallBroadcastFormat()
				runner := &broadcastFakeRunner{}
				open := func(value byte) func(context.Context) (transcode.Decoder, func(), error) {
					return func(context.Context) (transcode.Decoder, func(), error) {
						return &broadcastFakeDecoder{format: f, closed: make(chan struct{}), value: value}, nil, nil
					}
				}
				programme := broadcastPlan{id: "programme", open: func(context.Context) (transcode.Decoder, func(), error) {
					return &delayedIntroProgramme{Decoder: &broadcastFakeDecoder{format: f, closed: make(chan struct{}), value: 80}, delay: ready}, nil, nil
				}}
				planner := introFillPlanner{broadcastFakePlanner: broadcastFakePlanner{plan: programme, screenPlan: broadcastPlan{id: "screen", open: open(40)}}, intro: broadcastPlan{id: "ident", fill: true, open: open(200)}}
				b, e := newBroadcast(t.Context(), runner, planner, f)
				if e != nil {
					t.Fatal(e)
				}
				defer b.close()
				if _, e = b.addRendition(t.Context(), "main", core.Decision{}); e != nil {
					t.Fatal(e)
				}
				b.start()
				time.Sleep(8 * time.Second)
				synctest.Wait()
				b.close()
				frames := runner.encoders[0].snapshot()
				if len(frames) < 230 {
					t.Fatal("output stopped")
				}
				for i := 3; i < 30; i++ {
					if frames[i].Video[0] != 200 {
						t.Fatalf("ident flashed instead of1s minimum at tick%d value%d", i, frames[i].Video[0])
					}
				}
				target := 80
				if ready > 4*time.Second {
					target = 40
				}
				mixed := 0
				for _, frame := range frames[30:140] {
					v := int(frame.Video[0])
					if v > target && v < 200 {
						mixed++
					}
				}
				if mixed < 7 || mixed > 10 {
					t.Fatalf("300ms video crossfade ticks%d", mixed)
				}
				if ready > 4*time.Second {
					for _, frame := range frames[132:175] {
						if frame.Video[0] != 40 {
							t.Fatal("ident exceeded cap or stale programme aired")
						}
					}
				} else {
					if frames[45].Video[0] != 80 {
						t.Fatal("ready programme delayed after crossfade")
					}
					mid := int16(binary.LittleEndian.Uint16(frames[35].PCM))
					end := int16(binary.LittleEndian.Uint16(frames[45].PCM))
					if mid <= -9000 || mid >= 9000 || end > -9900 {
						t.Fatalf("300ms audio crossfade missing: mid%d end%d", mid, end)
					}
				}
			})
		})
	}
}

// A welcome can share the scheduled programme identity. A slow welcome must
// neither steal that input nor prevent the prewarmed signal from airing.
func TestColdWelcomeKeepsLiveSignalReady(t *testing.T) {
	for _, fill := range []bool{false, true} {
		t.Run(map[bool]string{false: "scheduled", true: "fill"}[fill], func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				f := smallBroadcastFormat()
				runner := &broadcastFakeRunner{}
				open := func(value byte, delay time.Duration) func(context.Context) (transcode.Decoder, func(), error) {
					return func(context.Context) (transcode.Decoder, func(), error) {
						return &delayedIntroProgramme{Decoder: &broadcastFakeDecoder{format: f, closed: make(chan struct{}), value: value}, delay: delay}, nil, nil
					}
				}
				planner := introFillPlanner{broadcastFakePlanner: broadcastFakePlanner{plan: broadcastPlan{id: "programme", open: open(80, 3*time.Second)}, screenPlan: broadcastPlan{id: "screen", open: open(40, 0)}}, intro: broadcastPlan{id: "programme", fill: fill, open: open(200, 2*time.Second)}}
				b, err := newBroadcast(t.Context(), runner, planner, f)
				if err != nil {
					t.Fatal(err)
				}
				defer b.close()
				if _, err = b.addRendition(t.Context(), "main", core.Decision{}); err != nil {
					t.Fatal(err)
				}
				b.start()
				time.Sleep(6 * time.Second)
				synctest.Wait()
				b.close()
				frames := runner.encoders[0].snapshot()
				if len(frames) < 175 {
					t.Fatalf("output stopped: %d frames", len(frames))
				}
				for tick, frame := range frames {
					if frame.Video[0] != 40 && frame.Video[0] != 80 {
						t.Fatalf("tick %d: unexpected black or late welcome picture %d", tick, frame.Video[0])
					}
				}
				if frames[0].Video[0] != 40 || frames[135].Video[0] != 80 {
					t.Fatal("signal or programme was not ready on time")
				}
			})
		})
	}
}
