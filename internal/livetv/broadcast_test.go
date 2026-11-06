package livetv

import (
	"context"
	"encoding/binary"
	"io"
	"net/http"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/transcode"
)

type broadcastFakeEncoder struct {
	mu     sync.Mutex
	frames []transcode.RawFrame
	closed bool
	output fakeHLS
	index  transcode.SegmentIndex
}

func (e *broadcastFakeEncoder) Write(ctx context.Context, f transcode.RawFrame) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	copyFrame := transcode.RawFrame{Video: append([]byte(nil), f.Video...), PCM: append([]byte(nil), f.PCM...), OutputTime: f.OutputTime, SourcePTS: f.SourcePTS, SourcePTSValid: f.SourcePTSValid}
	if len(f.Video) > 6 {
		return nil
	}
	e.mu.Lock()
	e.frames = append(e.frames, copyFrame)
	e.mu.Unlock()
	return nil
}
func (e *broadcastFakeEncoder) Segments() transcode.SegmentIndex {
	if e.index != nil {
		return e.index
	}
	return fakeBroadcastIndex{}
}
func (e *broadcastFakeEncoder) Close() error { e.mu.Lock(); e.closed = true; e.mu.Unlock(); return nil }
func (e *broadcastFakeEncoder) snapshot() []transcode.RawFrame {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]transcode.RawFrame(nil), e.frames...)
}

type broadcastFakeRunner struct {
	mu       sync.Mutex
	encoders []*broadcastFakeEncoder
	requests []transcode.EncoderRequest
	decoder  func(context.Context, transcode.DecoderRequest) (transcode.Decoder, error)
}

func (r *broadcastFakeRunner) StartEncoder(_ context.Context, req transcode.EncoderRequest) (transcode.ContinuousEncoder, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e := &broadcastFakeEncoder{}
	r.encoders = append(r.encoders, e)
	r.requests = append(r.requests, req)
	return e, nil
}
func (r *broadcastFakeRunner) StartDecoder(ctx context.Context, req transcode.DecoderRequest) (transcode.Decoder, error) {
	return r.decoder(ctx, req)
}

type broadcastFakePlanner struct {
	plan       broadcastPlan
	next       *broadcastPlan
	screenPlan broadcastPlan
}

func (p broadcastFakePlanner) plans(time.Time) (broadcastPlan, *broadcastPlan) { return p.plan, p.next }
func (p broadcastFakePlanner) screen(time.Time) broadcastPlan                  { return p.screenPlan }

type broadcastFakeDecoder struct {
	origin time.Time
	format transcode.RawFormat
	closed chan struct{}
	once   sync.Once
	tick   int64
	stall  bool
	value  byte
}

func (d *broadcastFakeDecoder) Next(ctx context.Context) (transcode.RawFrame, error) {
	if d.stall {
		elapsed := time.Since(d.origin)
		if elapsed >= time.Second && elapsed < 21*time.Second {
			select {
			case <-ctx.Done():
				return transcode.RawFrame{}, ctx.Err()
			case <-d.closed:
				return transcode.RawFrame{}, io.EOF
			case <-time.After(21*time.Second - elapsed):
			}
		}
	}
	select {
	case <-ctx.Done():
		return transcode.RawFrame{}, ctx.Err()
	case <-d.closed:
		return transcode.RawFrame{}, io.EOF
	default:
	}
	frame := blackFrame(d.format, tickSamples(d.format, d.tick))
	frame.SourcePTS = time.Duration(d.tick) * time.Second / 30
	frame.SourcePTSValid = true
	d.tick++
	for i := range frame.Video {
		frame.Video[i] = 80
		if d.value != 0 {
			frame.Video[i] = d.value
		}
	}
	for i := 0; i < len(frame.PCM); i += 2 {
		binary.LittleEndian.PutUint16(frame.PCM[i:], 10000)
	}
	return frame, nil
}
func (d *broadcastFakeDecoder) Close() error { d.once.Do(func() { close(d.closed) }); return nil }
func smallBroadcastFormat() transcode.RawFormat {
	return transcode.RawFormat{Width: 2, Height: 2, FPSNum: 30, FPSDen: 1, SampleRate: 48000, Channels: 2, PixelFormat: "yuv420p"}
}

func TestBroadcastStallAndRecoveryKeepsOneOutput(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		origin := time.Now()
		format := smallBroadcastFormat()
		runner := &broadcastFakeRunner{}
		opened := 0
		plan := broadcastPlan{id: "programme", open: func(context.Context) (transcode.Decoder, func(), error) {
			opened++
			return &broadcastFakeDecoder{origin: origin, format: format, closed: make(chan struct{}), stall: true}, nil, nil
		}}
		b, err := newBroadcast(context.Background(), runner, broadcastFakePlanner{plan: plan, screenPlan: broadcastPlan{id: "screen", open: func(context.Context) (transcode.Decoder, func(), error) {
			return &broadcastFakeDecoder{format: format, closed: make(chan struct{}), value: 16}, nil, nil
		}}}, format)
		if err != nil {
			t.Fatal(err)
		}
		defer b.close()
		if _, err := b.addRendition(context.Background(), "main", core.Decision{MaxHeight: 720}); err != nil {
			t.Fatal(err)
		}
		b.start()
		time.Sleep(40 * time.Second)
		synctest.Wait()
		b.close()
		if opened != 2 {
			t.Fatalf("decoder attempts=%d, want initial and one bounded retry", opened)
		}
		if len(runner.encoders) != 1 {
			t.Fatal("encoder restarted")
		}
		frames := runner.encoders[0].snapshot()
		if len(frames) < 1199 || len(frames) > 1201 {
			t.Fatalf("tick gap: %d frames", len(frames))
		}
		samples := 0
		seenReal, seenScreen, recovered := false, false, false
		for _, frame := range frames {
			samples += len(frame.PCM) / 4
			switch frame.Video[0] {
			case 80:
				if seenScreen {
					recovered = true
				}
				seenReal = true
			case 16:
				if seenReal {
					seenScreen = true
				}
			}
		}
		if !seenReal || !seenScreen || !recovered {
			t.Fatal("missing real→screen→real transition")
		}
		if samples != len(frames)*1600 {
			t.Fatal("PCM drift")
		}
		if runner.requests[0].DVRWindow != 30*time.Minute {
			t.Fatal("DVR contract lost")
		}
	})
}

func TestBroadcastRegistrySharesViewersAndStopsAfterGrace(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		runner := &broadcastFakeRunner{}
		registry := &broadcastRegistry{}
		defer registry.close()
		format := smallBroadcastFormat()
		created := 0
		create := func() (*broadcast, error) {
			created++
			return newBroadcast(context.Background(), runner, broadcastFakePlanner{plan: broadcastPlan{id: "screen"}}, format)
		}
		a, err := registry.acquire(context.Background(), broadcastKey{channelID: "one"}, "main", core.Decision{MaxHeight: 720}, create)
		if err != nil {
			t.Fatal(err)
		}
		b, err := registry.acquire(context.Background(), broadcastKey{channelID: "one"}, "main", core.Decision{MaxHeight: 720}, create)
		if err != nil {
			t.Fatal(err)
		}
		if created != 1 || a.broadcast != b.broadcast || len(runner.encoders) != 1 {
			t.Fatal("duplicate shared output")
		}
		a.close()
		time.Sleep(61 * time.Second)
		synctest.Wait()
		if _, err := b.output(); err != nil {
			t.Fatal("remaining viewer lost output")
		}
		b.close()
		time.Sleep(time.Second)
		synctest.Wait()
		warm, err := registry.acquire(context.Background(), broadcastKey{channelID: "one"}, "main", core.Decision{MaxHeight: 720}, create)
		if err != nil {
			t.Fatal(err)
		}
		if created != 1 || warm.broadcast != b.broadcast || len(runner.encoders) != 1 {
			t.Fatal("warm reopen discarded the continuous output")
		}
		warm.close()
		time.Sleep(59 * time.Second)
		synctest.Wait()
		b.broadcast.mu.Lock()
		closed := b.broadcast.closed
		b.broadcast.mu.Unlock()
		if closed {
			t.Fatal("idle grace cut short")
		}
		time.Sleep(2 * time.Second)
		synctest.Wait()
		if _, err := b.output(); err == nil {
			t.Fatal("idle output remained live")
		}
		personal, err := registry.acquire(context.Background(), broadcastKey{channelID: "one", viewerID: 2}, "personal", core.Decision{}, create)
		if err != nil {
			t.Fatal(err)
		}
		defer personal.close()
		if created != 2 {
			t.Fatal("personal channel shared across viewers")
		}
	})
}

func TestFractionalTickPCMAndCrossfade(t *testing.T) {
	f := smallBroadcastFormat()
	f.FPSNum, f.FPSDen = 30000, 1001
	var sum int
	for n := int64(0); n < 30000; n++ {
		sum += tickSamples(f, n)
	}
	if sum != 48000*1001 {
		t.Fatal("fractional PCM accumulator drift", sum)
	}
	old := make([]byte, 1600*4)
	next := make([]byte, len(old))
	for i := 0; i < len(old); i += 2 {
		binary.LittleEndian.PutUint16(old[i:], 10000)
	}
	fade := pcmFade{}
	fade.start(old, f)
	fade.apply(next, 2)
	if int16(binary.LittleEndian.Uint16(next)) != 10000 || fade.at != 1600 {
		t.Fatal("crossfade first tick")
	}
	tail := make([]byte, len(old))
	fade.apply(tail, 2)
	if fade.at != 1920 || int16(binary.LittleEndian.Uint16(tail[400*4:])) != 0 {
		t.Fatal("crossfade not exactly 40 ms")
	}
}

type timedBroadcastPlanner struct{ first, second broadcastPlan }

func (p timedBroadcastPlanner) plans(now time.Time) (broadcastPlan, *broadcastPlan) {
	if now.Before(p.second.start) {
		return p.first, &p.second
	}
	return p.second, nil
}

type countedBroadcastDecoder struct {
	transcode.Decoder
	calls *int
}

func (d countedBroadcastDecoder) Next(ctx context.Context) (transcode.RawFrame, error) {
	*d.calls++
	return d.Decoder.Next(ctx)
}
func TestBroadcastPrewarmsTwentySecondsBeforeCut(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		format := smallBroadcastFormat()
		origin := time.Now()
		runner := &broadcastFakeRunner{}
		var secondOpened time.Time
		nextCalls := 0
		first := broadcastPlan{id: "one", start: origin, end: origin.Add(25 * time.Second), open: func(context.Context) (transcode.Decoder, func(), error) {
			return &broadcastFakeDecoder{origin: origin, format: format, closed: make(chan struct{})}, nil, nil
		}}
		second := broadcastPlan{id: "two", start: first.end, end: first.end.Add(time.Hour), open: func(context.Context) (transcode.Decoder, func(), error) {
			secondOpened = time.Now()
			return countedBroadcastDecoder{Decoder: &broadcastFakeDecoder{origin: origin, format: format, closed: make(chan struct{})}, calls: &nextCalls}, nil, nil
		}}
		b, err := newBroadcast(context.Background(), runner, timedBroadcastPlanner{first: first, second: second}, format)
		if err != nil {
			t.Fatal(err)
		}
		defer b.close()
		if _, err := b.addRendition(context.Background(), "main", core.Decision{}); err != nil {
			t.Fatal(err)
		}
		b.start()
		time.Sleep(22 * time.Second)
		synctest.Wait()
		if secondOpened.Sub(origin) != 5*time.Second {
			t.Fatal("prewarm timing", secondOpened.Sub(origin))
		}
		if nextCalls != 1 {
			t.Fatal("prewarm read beyond held first frame", nextCalls)
		}
		time.Sleep(20 * time.Second)
		synctest.Wait()
		if nextCalls < 2 {
			t.Fatal("next input did not activate at boundary")
		}
	})
}

type elementBroadcastPlanner []broadcastPlan

func (p elementBroadcastPlanner) plans(now time.Time) (broadcastPlan, *broadcastPlan) {
	for i, plan := range p {
		if !now.Before(plan.start) && now.Before(plan.end) {
			if i+1 < len(p) {
				return plan, &p[i+1]
			}
			return plan, nil
		}
	}
	return broadcastPlan{}, nil
}
func (p elementBroadcastPlanner) prewarmPlans(now time.Time) []broadcastPlan {
	var plans []broadcastPlan
	for _, plan := range p {
		if plan.start.After(now) && !plan.start.After(now.Add(broadcastPrewarm)) {
			plans = append(plans, plan)
		}
	}
	return plans
}

func TestBroadcastPrewarmsAcrossShortElements(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		format := smallBroadcastFormat()
		origin := time.Now()
		opened := map[string]time.Time{}
		var openedMu sync.Mutex
		plans := elementBroadcastPlanner{}
		for n, bounds := range [][2]int{{0, 10}, {10, 14}, {14, 18}, {18, 90}} {
			id := string(rune('a' + n))
			plans = append(plans, broadcastPlan{id: id, start: origin.Add(time.Duration(bounds[0]) * time.Second), end: origin.Add(time.Duration(bounds[1]) * time.Second), open: func(context.Context) (transcode.Decoder, func(), error) {
				openedMu.Lock()
				opened[id] = time.Now()
				openedMu.Unlock()
				return &broadcastFakeDecoder{format: format, closed: make(chan struct{})}, nil, nil
			}})
		}
		b, err := newBroadcast(context.Background(), &broadcastFakeRunner{}, plans, format)
		if err != nil {
			t.Fatal(err)
		}
		defer b.close()
		b.start()
		time.Sleep(time.Second)
		synctest.Wait()
		for _, id := range []string{"b", "c", "d"} {
			if !opened[id].Equal(origin) {
				t.Fatalf("element %s prepared at %s", id, opened[id].Sub(origin))
			}
		}
		time.Sleep(18 * time.Second)
		synctest.Wait()
		if b.current == nil || b.current.plan.id != "d" || b.current.lastFrameAt.IsZero() {
			t.Fatal("prewarmed programme did not air at its boundary")
		}
	})
}

func TestProviderBroadcastReservationIsReleasedWhileWaiting(t *testing.T) {
	s := testService(t, Options{})
	release, err := s.reserveBroadcastProvider("account", "one", "first", 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.reserveBroadcastProvider("account", "two", "second", 1); err != ErrProviderBusy {
		t.Fatal("provider account cap exceeded", err)
	}
	release()
	release()
	release, err = s.reserveBroadcastProvider("account", "two", "second", 1)
	if err != nil {
		t.Fatal("waiting output retained provider slot", err)
	}
	release()
}

type fakeBroadcastIndex struct{}

func (fakeBroadcastIndex) Latest() (transcode.Segment, bool) {
	return transcode.Segment{Seq: 5, Start: time.Unix(20, 0), Duration: 4 * time.Second}, true
}
func (fakeBroadcastIndex) Window(from, until time.Time) []transcode.Segment {
	var out []transcode.Segment
	for n := int64(0); n < 6; n++ {
		seg := transcode.Segment{Seq: n, Start: time.Unix(n*4, 0), Duration: 4 * time.Second}
		if seg.Start.Before(until) && seg.Start.Add(seg.Duration).After(from) {
			out = append(out, seg)
		}
	}
	return out
}
func (fakeBroadcastIndex) Since(int64, int) []transcode.Segment { return nil }
func (fakeBroadcastIndex) Wait(ctx context.Context, _ int64) (transcode.Segment, error) {
	<-ctx.Done()
	return transcode.Segment{}, ctx.Err()
}
func (fakeBroadcastIndex) ServeTS(http.ResponseWriter, *http.Request, int64)   {}
func (fakeBroadcastIndex) ServeInit(http.ResponseWriter, *http.Request)        {}
func (fakeBroadcastIndex) ServeFMP4(http.ResponseWriter, *http.Request, int64) {}
func (fakeBroadcastIndex) AttachTS(context.Context) (io.ReadCloser, error)     { return nil, io.EOF }
func (fakeBroadcastIndex) Format() (string, int, int, int) {
	return "avc1.640028,mp4a.40.2", 2, 2, 1000
}

func (f *fakeTranscoder) HWAccel() core.HWAccel { return core.HWAccel{Name: "none"} }
func (f *fakeTranscoder) StartEncoder(context.Context, transcode.EncoderRequest) (transcode.ContinuousEncoder, error) {
	return &broadcastFakeEncoder{}, nil
}
func (f *fakeTranscoder) StartDecoder(_ context.Context, r transcode.DecoderRequest) (transcode.Decoder, error) {
	return &broadcastFakeDecoder{format: r.Format, origin: time.Now(), closed: make(chan struct{})}, nil
}

// A splice starts at the last emitted sample even if the previous PCM block
// starts at the opposite phase. Replaying that block introduces a click.
func TestPCMFadeContinuesEmittedEndpoint(t *testing.T) {
	f := smallBroadcastFormat()
	old := make([]byte, 1600*4)
	for i := 0; i < len(old); i += 2 {
		binary.LittleEndian.PutUint16(old[i:], uint16(i))
	}
	next := make([]byte, len(old))
	fade := pcmFade{}
	fade.start(old, f)
	fade.apply(next, 2)
	for ch := 0; ch < 2; ch++ {
		got := binary.LittleEndian.Uint16(next[ch*2:])
		want := binary.LittleEndian.Uint16(old[len(old)-4+ch*2:])
		if got != want {
			t.Fatalf("splice channel %d: first sample %d, endpoint %d", ch, got, want)
		}
	}
}

// The edge may lead schedule time only by media actually produced. A fast
// source fills that lead without a cold-playlist hold; a slow source has a
// bounded burst attempt and returns to its ordinary continuous clock.
type burstTestPlanner struct {
	broadcastFakePlanner
	cutAt   time.Time
	cutPlan broadcastPlan
}

func (p burstTestPlanner) plans(now time.Time) (broadcastPlan, *broadcastPlan) {
	if !p.cutAt.IsZero() && !now.Before(p.cutAt) {
		return p.cutPlan, nil
	}
	return p.broadcastFakePlanner.plans(now)
}

func (burstTestPlanner) startupLead() time.Duration { return 12 * time.Second }

func TestBroadcastBoundedStartupLead(t *testing.T) {
	for _, source := range []string{"fast", "slow", "short-file"} {
		t.Run(source, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				slow := source == "slow"
				format := smallBroadcastFormat()
				planner := burstTestPlanner{broadcastFakePlanner: broadcastFakePlanner{plan: broadcastPlan{id: "programme", open: func(context.Context) (transcode.Decoder, func(), error) {
					d := &broadcastFakeDecoder{format: format, closed: make(chan struct{})}
					if slow {
						return &burstSlowDecoder{broadcastFakeDecoder: d}, nil, nil
					}
					if source == "short-file" {
						return &burstFiniteDecoder{broadcastFakeDecoder: d, remaining: 239}, nil, nil
					}
					return d, nil, nil
				}}}}
				if source == "short-file" {
					planner.cutAt = time.Now().Add(8 * time.Second)
					planner.cutPlan = broadcastPlan{id: "next", start: planner.cutAt, open: func(context.Context) (transcode.Decoder, func(), error) {
						return &broadcastFakeDecoder{format: format, closed: make(chan struct{}), value: 90}, nil, nil
					}}
					planner.next = &planner.cutPlan
				}
				runner := &broadcastFakeRunner{}
				b, err := newBroadcast(context.Background(), runner, planner, format)
				if err != nil {
					t.Fatal(err)
				}
				defer b.close()
				if _, err = b.addRendition(context.Background(), "main", core.Decision{MaxHeight: 720}); err != nil {
					t.Fatal(err)
				}
				runner.encoders[0].index = burstSampleIndex{encoder: runner.encoders[0]}
				start := time.Now()
				b.start()
				time.Sleep(22 * time.Second)
				synctest.Wait()
				b.close()
				if !b.startupUntil.IsZero() {
					t.Fatal("startup remained active beyond deadline")
				}
				if b.lead < 0 || b.lead > 16*time.Second {
					t.Fatalf("unbounded lead: %s", b.lead)
				}
				if !slow && b.lead < 11*time.Second {
					t.Fatalf("fast source did not fill lead: %s", b.lead)
				}
				if slow && b.lead != 0 {
					t.Fatalf("slow source invented media lead: %s", b.lead)
				}
				frames := runner.encoders[0].snapshot()
				if len(frames) == 0 {
					t.Fatal("no continuous output")
				}
				for n, f := range frames {
					want := start.Add(time.Duration(n) * time.Second / 30)
					if f.OutputTime != want {
						t.Fatalf("frame %d schedule time %s, want %s", n, f.OutputTime, want)
					}
					if len(f.PCM) != 6400 {
						t.Fatalf("frame %d audio samples lost", n)
					}
					if source == "short-file" && n >= 240 && f.Video[0] != 90 {
						t.Fatalf("frame %d missed the scheduled element cut", n)
					}
				}
			})
		})
	}
}

type burstSlowDecoder struct{ *broadcastFakeDecoder }

// Container duration rounding can leave a file one frame short of its slot.
// Its final picture must bridge to the cut without stopping the burst clock.
type burstFiniteDecoder struct {
	*broadcastFakeDecoder
	remaining int
}

func (d *burstFiniteDecoder) Next(ctx context.Context) (transcode.RawFrame, error) {
	if d.remaining == 0 {
		return transcode.RawFrame{}, io.EOF
	}
	d.remaining--
	return d.broadcastFakeDecoder.Next(ctx)
}

func (d *burstSlowDecoder) Next(ctx context.Context) (transcode.RawFrame, error) {
	select {
	case <-ctx.Done():
		return transcode.RawFrame{}, ctx.Err()
	case <-time.After(50 * time.Millisecond):
	}
	return d.broadcastFakeDecoder.Next(ctx)
}

type burstSampleIndex struct {
	fakeBroadcastIndex
	encoder *broadcastFakeEncoder
}

func (s burstSampleIndex) Latest() (transcode.Segment, bool) {
	frames := s.encoder.snapshot()
	ticks := len(frames) / 120 * 120
	if ticks == 0 {
		return transcode.Segment{}, false
	}
	return transcode.Segment{Start: frames[0].OutputTime.Add(time.Duration(ticks-120) * time.Second / 30), Duration: 4 * time.Second}, true
}
