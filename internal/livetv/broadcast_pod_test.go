package livetv

import (
	"context"
	"io"
	"testing"
	"testing/synctest"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/transcode"
)

type podSequenceDecoder struct {
	*broadcastFakeDecoder
	sequence int
}

type alternatePodPlayback struct{ fakePlayback }

func (p *alternatePodPlayback) ChannelReadyRelease(_ context.Context, _ *core.User, k core.RatingKey, _ core.ClientCaps) (string, bool) {
	return "ready-second", k == core.MovieKey(2)
}

func (p *alternatePodPlayback) PrepareChannel(ctx context.Context, u *core.User, key core.RatingKey, caps core.ClientCaps, group string) (*core.PlaySession, error) {
	if key == core.MovieKey(1) {
		return nil, core.ErrNotAvailable
	}
	return p.fakePlayback.PrepareChannel(ctx, u, key, caps, group)
}

// The replacement source is parked at its own first frame throughout a long
// pod. Its source clock begins at the airing, not at the earlier resolution.
func TestBroadcastPodPrewarmsAlternateProgrammeClock(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		origin := time.Now()
		format := smallBroadcastFormat()
		playback := &alternatePodPlayback{}
		s := testService(t, Options{Playback: playback})
		// The readiness worker has settled; admit the alternate locally.
		synctest.Wait()
		s.mu.Lock()
		s.readyTitles = map[core.RatingKey]bool{core.MovieKey(2): true}
		s.mu.Unlock()
		channel := testChannel()
		after := Programme{ID: "planned", ChannelID: channel.ID, Start: origin.Add(2 * time.Minute), End: origin.Add(3 * time.Minute), Item: channel.Items[0]}
		runner := &broadcastFakeRunner{decoder: func(context.Context, transcode.DecoderRequest) (transcode.Decoder, error) {
			return &podSequenceDecoder{broadcastFakeDecoder: &broadcastFakeDecoder{format: format, closed: make(chan struct{})}}, nil
		}}
		planner := &scheduleBroadcastPlanner{s: s, channel: channel, user: core.User{ID: 1, IsOwner: true}, format: format, runner: runner}
		pod := broadcastPlan{id: "pod", interstitial: true, start: origin, end: after.Start, open: func(context.Context) (transcode.Decoder, func(), error) {
			return &broadcastFakeDecoder{format: format, closed: make(chan struct{}), value: 240}, nil, nil
		}}
		b, err := newBroadcast(t.Context(), runner, elementBroadcastPlanner{pod, planner.programme(after)}, format)
		if err != nil {
			t.Fatal(err)
		}
		defer b.close()
		if _, err = b.addRendition(t.Context(), "main", core.Decision{}); err != nil {
			t.Fatal(err)
		}
		b.start()
		time.Sleep(121 * time.Second)
		synctest.Wait()
		b.close()
		frames := runner.encoders[0].snapshot()
		if len(frames) < 3630 {
			t.Fatalf("output stopped: %d", len(frames))
		}
		for tick := 3600; tick < 3630; tick++ {
			if got, want := frames[tick].Video[0], byte(40+tick-3600); got != want {
				t.Fatalf("alternate programme lost parked position at tick%d: got%d want%d", tick, got, want)
			}
		}
		if len(playback.prepared) != 1 || playback.prepared[0].Key != core.MovieKey(2) {
			t.Fatalf("alternate source not used: %+v", playback.prepared)
		}
	})
}

func (d *podSequenceDecoder) Next(ctx context.Context) (transcode.RawFrame, error) {
	f, e := d.broadcastFakeDecoder.Next(ctx)
	for i := range f.Video {
		f.Video[i] = byte(40 + d.sequence%180)
	}
	d.sequence++
	return f, e
}

func TestBroadcastPodExitHasExactProgrammeFrame(t *testing.T) {
	for _, resume := range []bool{false, true} {
		t.Run(map[bool]string{false: "next programme", true: "parked programme"}[resume], func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				origin := time.Now()
				f := smallBroadcastFormat()
				runner := &broadcastFakeRunner{}
				opens := 0
				open := func(value byte) func(context.Context) (transcode.Decoder, func(), error) {
					return func(context.Context) (transcode.Decoder, func(), error) {
						return &broadcastFakeDecoder{format: f, closed: make(chan struct{}), value: value}, nil, nil
					}
				}
				source := func(context.Context) (transcode.Decoder, func(), error) {
					opens++
					return &overlayDecoder{Decoder: &podSequenceDecoder{broadcastFakeDecoder: &broadcastFakeDecoder{format: f, closed: make(chan struct{})}}, format: f, wallStart: origin}, nil, nil
				}
				first := broadcastPlan{id: "first", programme: true, mediaKey: 1, start: origin, end: origin.Add(4 * time.Second), open: source}
				pod := broadcastPlan{id: "pod", interstitial: true, element: "reel", start: first.end, end: origin.Add(12 * time.Second), open: open(240)}
				after := broadcastPlan{id: "after", programme: true, mediaKey: 2, start: pod.end, end: origin.Add(20 * time.Second), open: open(90)}
				if resume {
					after.mediaKey = 1
					after.sourceOffset = 4 * time.Second
					after.open = source
				}
				b, e := newBroadcast(t.Context(), runner, elementBroadcastPlanner{first, pod, after}, f)
				if e != nil {
					t.Fatal(e)
				}
				defer b.close()
				if _, e = b.addRendition(t.Context(), "main", core.Decision{}); e != nil {
					t.Fatal(e)
				}
				b.start()
				time.Sleep(15 * time.Second)
				synctest.Wait()
				b.close()
				frames := runner.encoders[0].snapshot()
				if len(frames) < 449 {
					t.Fatalf("output stopped: %d", len(frames))
				}
				want := byte(90)
				if resume {
					want = 160
				}
				if frames[359].Video[0] != 240 || frames[360].Video[0] != want {
					t.Fatalf("pod exit: before=%d after=%d want=%d", frames[359].Video[0], frames[360].Video[0], want)
				}
				if opens != 1 {
					t.Fatalf("parked decoder reopened: %d", opens)
				}
				for tick := 360; tick < 449; tick++ {
					if frames[tick].Video[0] == 0 || frames[tick].Video[0] == 240 {
						t.Fatalf("fallback at tick %d", tick)
					}
				}
			})
		})
	}
}

func TestPodStartPreparesProgrammeBeyondShortElements(t *testing.T) {
	now := time.Now()
	p := scheduleBroadcastPlanner{window: []Programme{{ID: "pod", Start: now, End: now.Add(40 * time.Second), Item: Item{ReelClipID: "ad"}}, {ID: "after", Start: now.Add(40 * time.Second), End: now.Add(time.Minute), Item: Item{Key: 1}}}}
	plans := p.prewarmPlans(now)
	if len(plans) != 1 || !plans[0].programme || plans[0].start.Sub(now) != 40*time.Second {
		t.Fatalf("programme not prepared at pod start: %+v", plans)
	}
	// Replacing an element at an unchanged start must not reuse retry identity.
	a := p.window[0]
	b := a
	b.Item = Item{Key: 2}
	if broadcastProgrammeID(a) == broadcastProgrammeID(b) {
		t.Fatal("runtime identity reused across items")
	}
	b = a
	b.End = b.End.Add(time.Second)
	if broadcastProgrammeID(a) == broadcastProgrammeID(b) {
		t.Fatal("runtime identity reused across intervals")
	}
}

// A source that is still resolving after the boundary extends the preceding
// pod. The normal signal is reserved for an outage that exceeds the grace.
func TestBroadcastPodContinuesWhileProgrammeRetries(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		origin := time.Now()
		f := smallBroadcastFormat()
		runner := &broadcastFakeRunner{}
		open := func(value byte, delay time.Duration) func(context.Context) (transcode.Decoder, func(), error) {
			return func(context.Context) (transcode.Decoder, func(), error) {
				return &delayedIntroProgramme{Decoder: &broadcastFakeDecoder{format: f, closed: make(chan struct{}), value: value}, delay: delay}, nil, nil
			}
		}
		first := broadcastPlan{id: "first", programme: true, start: origin, end: origin.Add(4 * time.Second), open: open(80, 0)}
		pod := broadcastPlan{id: "pod", interstitial: true, start: first.end, end: origin.Add(8 * time.Second), open: open(240, 0)}
		after := broadcastPlan{id: "after", programme: true, start: pod.end, end: origin.Add(20 * time.Second), open: open(90, 10*time.Second)}
		b, e := newBroadcast(t.Context(), runner, elementBroadcastPlanner{first, pod, after}, f)
		if e != nil {
			t.Fatal(e)
		}
		defer b.close()
		if _, e = b.addRendition(t.Context(), "main", core.Decision{}); e != nil {
			t.Fatal(e)
		}
		b.start()
		time.Sleep(12 * time.Second)
		synctest.Wait()
		b.close()
		frames := runner.encoders[0].snapshot()
		for tick := 240; tick < 300; tick++ {
			if frames[tick].Video[0] != 240 {
				t.Fatalf("pod replaced by fallback at tick %d: %d", tick, frames[tick].Video[0])
			}
		}
		if frames[303].Video[0] != 90 {
			t.Fatal("ready programme did not replace extended pod")
		}
	})
}

type finitePodDecoder struct {
	*broadcastFakeDecoder
	remaining int
}

func (d *finitePodDecoder) Next(ctx context.Context) (transcode.RawFrame, error) {
	if d.remaining == 0 {
		return transcode.RawFrame{}, io.EOF
	}
	d.remaining--
	return d.broadcastFakeDecoder.Next(ctx)
}

type failedPodPlanner struct {
	elementBroadcastPlanner
	signal broadcastPlan
}

func (p failedPodPlanner) screen(time.Time) broadcastPlan { return p.signal }

// Reproduce a final ad EOF, failed back bumper and uncached look chain. Even
// legacy guide IDs that coincide cannot transfer the bumper retry deadline to
// the already prepared next programme.
func TestBroadcastPodFailuresKeepWarmProgrammeIdentity(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		origin := time.Now()
		f := smallBroadcastFormat()
		runner := &broadcastFakeRunner{}
		open := func(value byte) func(context.Context) (transcode.Decoder, func(), error) {
			return func(context.Context) (transcode.Decoder, func(), error) {
				return &broadcastFakeDecoder{format: f, closed: make(chan struct{}), value: value}, nil, nil
			}
		}
		plans := elementBroadcastPlanner{}
		opened := time.Time{}
		for i, element := range []string{"programme", "reel", "back", "ident", "upnext", "tonight", "programme"} {
			row := Programme{ID: "legacy-start-id", Start: origin.Add(time.Duration(i*4) * time.Second), End: origin.Add(time.Duration(i*4+4) * time.Second), Item: Item{Slate: element}}
			if element == "programme" {
				row.Item = Item{Key: core.RatingKey(i + 1)}
			}
			plan := broadcastPlan{id: broadcastProgrammeID(row), element: element, start: row.Start, end: row.End, programme: element == "programme", interstitial: element != "programme", open: open(80)}
			if element == "reel" {
				plan.open = func(context.Context) (transcode.Decoder, func(), error) {
					return &finitePodDecoder{broadcastFakeDecoder: &broadcastFakeDecoder{format: f, closed: make(chan struct{}), value: 240}, remaining: 117}, nil, nil
				}
			}
			if i > 1 && i < 6 {
				plan.open = func(context.Context) (transcode.Decoder, func(), error) { return nil, nil, core.ErrNotAvailable }
				plan.failed = func(error) time.Time { return row.End }
			}
			if i == 6 {
				plan.end = origin.Add(40 * time.Second)
				plan.open = func(ctx context.Context) (transcode.Decoder, func(), error) {
					opened = time.Now()
					return open(90)(ctx)
				}
			}
			plans = append(plans, plan)
		}
		b, e := newBroadcast(t.Context(), runner, failedPodPlanner{plans, broadcastPlan{id: "screen", open: open(30)}}, f)
		if e != nil {
			t.Fatal(e)
		}
		defer b.close()
		if _, e = b.addRendition(t.Context(), "main", core.Decision{}); e != nil {
			t.Fatal(e)
		}
		b.start()
		time.Sleep(26 * time.Second)
		synctest.Wait()
		b.close()
		if opened.IsZero() || opened.After(origin.Add(5*time.Second)) {
			t.Fatalf("next programme lost its prewarm: %v", opened.Sub(origin))
		}
		frames := runner.encoders[0].snapshot()
		for tick := 720; tick < 779; tick++ {
			if frames[tick].Video[0] != 90 {
				t.Fatalf("pod failure contaminated programme boundary at tick %d: %d", tick, frames[tick].Video[0])
			}
		}
	})
}

// The playback manager supersedes different titles on one client. Adjacent
// programme decoders must retain separate identities inside one channel group.
type podIdentityPlayback struct {
	fakePlayback
	clients []string
	groups  []string
}

func (p *podIdentityPlayback) PrepareChannel(ctx context.Context, u *core.User, key core.RatingKey, caps core.ClientCaps, group string) (*core.PlaySession, error) {
	p.clients = append(p.clients, caps.ClientID)
	p.groups = append(p.groups, group)
	return p.fakePlayback.PrepareChannel(ctx, u, key, caps, group)
}
func TestBroadcastPodProgrammeInputsRetainSeparateIdentities(t *testing.T) {
	playback := &podIdentityPlayback{}
	s := testService(t, Options{Playback: playback})
	format := smallBroadcastFormat()
	runner := &broadcastFakeRunner{decoder: func(context.Context, transcode.DecoderRequest) (transcode.Decoder, error) {
		return &broadcastFakeDecoder{format: format, closed: make(chan struct{})}, nil
	}}
	channel := testChannel()
	planner := &scheduleBroadcastPlanner{s: s, channel: channel, user: core.User{ID: 1, IsOwner: true}, format: format, runner: runner}
	now := time.Now()
	first := Programme{ID: "first", ChannelID: channel.ID, Start: now, End: now.Add(time.Minute), Item: channel.Items[0]}
	next := first
	next.ID = "next"
	next.Start = first.End.Add(time.Minute)
	next.End = next.Start.Add(time.Minute)
	next.Item = channel.Items[1]
	var releases []func()
	for _, airing := range []Programme{first, next} {
		decoder, release, err := planner.programme(airing).open(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		releases = append(releases, func() {
			decoder.Close()
			if release != nil {
				release()
			}
		})
	}
	defer func() {
		for _, release := range releases {
			release()
		}
	}()
	if len(playback.clients) != 2 || playback.clients[0] == "" || playback.clients[0] == playback.clients[1] {
		t.Fatalf("overlapping sources would supersede each other: %v", playback.clients)
	}
	if playback.groups[0] != playback.groups[1] || playback.groups[0] != "broadcast-"+channel.ID {
		t.Fatalf("sources left the channel budget: %v", playback.groups)
	}
}
