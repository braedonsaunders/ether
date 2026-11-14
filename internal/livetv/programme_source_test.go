package livetv

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/transcode"
)

type programmeSourcePlayback struct {
	fakePlayback
	caps     []core.ClientCaps
	sourceMu sync.Mutex
	ready    map[core.RatingKey]string
	delays   map[core.RatingKey]time.Duration
	starts   map[core.RatingKey]time.Time
}

func (p *programmeSourcePlayback) PrepareChannel(ctx context.Context, u *core.User, k core.RatingKey, c core.ClientCaps, g string) (*core.PlaySession, error) {
	p.sourceMu.Lock()
	p.caps = append(p.caps, c)
	if p.starts == nil {
		p.starts = map[core.RatingKey]time.Time{}
	}
	p.starts[k] = time.Now()
	delay := p.delays[k]
	p.sourceMu.Unlock()
	if delay > 0 {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(delay):
		}
	}
	ps, err := p.fakePlayback.PrepareChannel(ctx, u, k, c, g)
	if k != core.MovieKey(1) {
		ps.Info.Duration = 2 * time.Hour
	}
	id := ps.ID
	if c.BroadcastReadyRelease != "" {
		id = c.BroadcastReadyRelease
	}
	ps.Resolution = &core.Resolution{Release: core.Release{ID: id}}
	return ps, err
}

func (p *programmeSourcePlayback) InputURL(id string) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, ps := range p.prepared {
		if ps.ID == id {
			return "http://source/movie/" + strconv.FormatUint(uint64(ps.Key), 10)
		}
	}
	return ""
}

func (p *programmeSourcePlayback) ChannelReadyRelease(_ context.Context, _ *core.User, k core.RatingKey, _ core.ClientCaps) (string, bool) {
	if p.ready == nil {
		return "ready-2", k == core.MovieKey(2)
	}
	id, ok := p.ready[k]
	return id, ok
}

type programmeSourceDecoder struct {
	*broadcastFakeDecoder
	life    context.Context
	blocked bool
}

func (d *programmeSourceDecoder) Next(ctx context.Context) (transcode.RawFrame, error) {
	if d.blocked {
		<-ctx.Done()
		return transcode.RawFrame{}, ctx.Err()
	}
	if d.life.Err() != nil {
		return transcode.RawFrame{}, d.life.Err()
	}
	return d.broadcastFakeDecoder.Next(ctx)
}
func TestProgrammeSourceRecovery(t *testing.T) {
	t.Run("cold acquisition races ready sources while title airs", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			origin := time.Now()
			f := smallBroadcastFormat()
			pb := &programmeSourcePlayback{ready: map[core.RatingKey]string{core.MovieKey(2): "verified-ghost", core.MovieKey(3): "verified-diehard"}, delays: map[core.RatingKey]time.Duration{core.MovieKey(1): 13 * time.Second, core.MovieKey(2): time.Second, core.MovieKey(3): 6 * time.Second}}
			s := testService(t, Options{Playback: pb})
			c := testChannel()
			c.Items[1].DurationMS = 2 * 60 * 60 * 1000
			c.Fallback = []Item{c.Items[1], {Key: core.MovieKey(3), Title: "Third", DurationMS: 2 * 60 * 60 * 1000}, {Key: core.MovieKey(4), Title: "Unverified", DurationMS: 2 * 60 * 60 * 1000}}
			runner := &broadcastFakeRunner{decoder: func(ctx context.Context, r transcode.DecoderRequest) (transcode.Decoder, error) {
				blocked := strings.HasSuffix(r.InputURL, "/"+strconv.FormatUint(uint64(core.MovieKey(2)), 10))
				return &programmeSourceDecoder{broadcastFakeDecoder: &broadcastFakeDecoder{format: f, closed: make(chan struct{}), value: 90}, life: ctx, blocked: blocked}, nil
			}}
			p := &scheduleBroadcastPlanner{s: s, channel: c, user: core.User{ID: 1, IsOwner: true}, format: f, runner: runner, joinAt: origin.Add(4 * time.Second)}
			prog := Programme{ID: "scheduled", ChannelID: c.ID, Start: origin.Add(-18 * time.Minute), End: origin.Add(time.Hour), Item: c.Items[0]}
			plan := p.programme(prog)
			look := titleStartupPlanner{coldHoldPlanner{elementBroadcastPlanner: elementBroadcastPlanner{plan}, screenPlan: broadcastPlan{id: "offair", open: coldHoldOpen(f, 10, 0)}, card: broadcastPlan{id: "title", element: "tonight", open: coldHoldOpen(f, 30, 0)}}}
			b, err := newBroadcast(t.Context(), runner, look, f)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = b.addRendition(t.Context(), "main", core.Decision{}); err != nil {
				t.Fatal(err)
			}
			b.start()
			time.Sleep(12 * time.Second)
			synctest.Wait()
			b.close()
			for _, key := range []core.RatingKey{core.MovieKey(1), core.MovieKey(2), core.MovieKey(3)} {
				if at, ok := pb.starts[key]; !ok || at.Sub(origin) > 200*time.Millisecond {
					t.Fatalf("source %d was not acquired at tune time: %s", key, at)
				}
			}
			if _, ok := pb.starts[core.MovieKey(4)]; ok {
				t.Fatal("unverified substitute started discovery")
			}
			first := time.Duration(-1)
			for _, frame := range runner.encoders[0].snapshot() {
				if frame.Video[0] == 10 {
					t.Fatal("off air during cold race")
				}
				if frame.Video[0] == 90 && first < 0 {
					first = frame.OutputTime.Sub(origin)
				}
			}
			if first < 4*time.Second || first > 12*time.Second {
				t.Fatalf("first programme picture %s: want title end + <=8s", first)
			}
			pb.mu.Lock()
			defer pb.mu.Unlock()
			if len(pb.stopped) != 2 {
				t.Fatalf("losing source or winner leaked: stops=%v", pb.stopped)
			}
		})
	})
	t.Run("duration offset substitutes without seeking EOF", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			now := time.Now()
			f := smallBroadcastFormat()
			pb := &programmeSourcePlayback{}
			s := testService(t, Options{Playback: pb})
			c := testChannel()
			c.Items[1].DurationMS = 2 * 60 * 60 * 1000
			c.Fallback = []Item{c.Items[1]}
			runner := &broadcastFakeRunner{decoder: func(ctx context.Context, r transcode.DecoderRequest) (transcode.Decoder, error) {
				if strings.HasSuffix(r.InputURL, "/"+strconv.FormatUint(uint64(core.MovieKey(1)), 10)) {
					return nil, fmt.Errorf("scheduled source sought at EOF")
				}
				return &programmeSourceDecoder{broadcastFakeDecoder: &broadcastFakeDecoder{format: f, closed: make(chan struct{}), value: 90}, life: ctx}, nil
			}}
			p := &scheduleBroadcastPlanner{s: s, channel: c, user: core.User{ID: 1, IsOwner: true}, format: f, runner: runner}
			prog := Programme{ID: "expired-runtime", ChannelID: c.ID, Start: now.Add(-time.Hour), End: now.Add(time.Hour), Item: c.Items[0]}
			d, release, err := p.programme(prog).open(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			defer d.Close()
			defer release()
			if d.(*overlayDecoder).policy.programme.Item.Key != c.Items[1].Key {
				t.Fatal("duration-length offset did not substitute")
			}
		})
	})
	t.Run("runtime retry retains scheduled offset", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			now := time.Now()
			f := smallBroadcastFormat()
			pb := &programmeSourcePlayback{ready: map[core.RatingKey]string{}}
			s := testService(t, Options{Playback: pb})
			c := testChannel()
			var offsets []time.Duration
			runner := &broadcastFakeRunner{decoder: func(ctx context.Context, r transcode.DecoderRequest) (transcode.Decoder, error) {
				offsets = append(offsets, r.Offset)
				return &programmeSourceDecoder{broadcastFakeDecoder: &broadcastFakeDecoder{format: f, closed: make(chan struct{}), value: 90}, life: ctx}, nil
			}}
			p := &scheduleBroadcastPlanner{s: s, channel: c, user: core.User{ID: 1, IsOwner: true}, format: f, runner: runner}
			prog := Programme{ID: "scheduled", ChannelID: c.ID, Start: now.Add(-18 * time.Minute), End: now.Add(time.Hour), Item: c.Items[0]}
			plan := p.programme(prog)
			d, release, err := plan.open(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			d.Close()
			release()
			if retry := plan.failed(errors.New("decoder stalled")); time.Until(retry) > time.Second {
				t.Fatal("runtime failover delayed")
			}
			time.Sleep(time.Second)
			d, release, err = plan.open(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			defer d.Close()
			defer release()
			if len(pb.caps[1].BroadcastExclude) != 1 {
				t.Fatal("retry reused failed release")
			}
			if offsets[1] < 18*time.Minute || offsets[1] > 19*time.Minute {
				t.Fatalf("retry sought outside programme position: %v", offsets)
			}
		})
	})
	t.Run("stalled programme never airs off air", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			origin := time.Now()
			f := smallBroadcastFormat()
			programme := broadcastPlan{id: "programme", programme: true, mediaKey: core.MovieKey(1), start: origin, end: origin.Add(time.Hour), open: func(context.Context) (transcode.Decoder, func(), error) {
				return &broadcastFakeDecoder{origin: origin, format: f, closed: make(chan struct{}), value: 90, stall: true}, nil, nil
			}}
			planner := coldHoldPlanner{elementBroadcastPlanner: elementBroadcastPlanner{programme}, screenPlan: broadcastPlan{id: "offair", open: coldHoldOpen(f, 10, 0)}}
			b, runner := newColdHoldBroadcast(t, planner, f)
			time.Sleep(25 * time.Second)
			synctest.Wait()
			b.close()
			frames := runner.encoders[0].snapshot()
			if len(frames) < 700 {
				t.Fatal("channel clock stopped")
			}
			for n, frame := range frames {
				if frame.Video[0] == 10 {
					t.Fatalf("off air at frame %d during recovery", n)
				}
			}
		})
	})
}
