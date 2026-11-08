package livetv

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/transcode"
)

const broadcastPrewarm = 20 * time.Second
const broadcastStall = 1500 * time.Millisecond

// Resolving/seek/probe may take several round trips before the first picture.
// Startup waits on the screen; the 1.5 s deadline applies after input is flowing.
const broadcastStartup = 45 * time.Second
const broadcastIdle = 60 * time.Second
const broadcastDVR = 30 * time.Minute

// broadcastPlan is immutable. Open performs blocking resolution away from the
// frame clock. Its release closes the playback/provider reservation exactly once.
type broadcastPlan struct {
	mediaKey               core.RatingKey
	sourceOffset           time.Duration
	interstitial, fallback bool
	element                string
	policy                 *broadcastPolicyTag
	fill                   bool
	programme              bool
	id                     string
	start, end             time.Time
	open                   func(context.Context) (transcode.Decoder, func(), error)
	failed                 func(error) time.Time
}
type broadcastPlanner interface {
	plans(time.Time) (broadcastPlan, *broadcastPlan)
}
type broadcastRendition struct {
	origin     time.Time
	originErr  error
	originDone chan struct{}
	height     int
	id         string
	encoder    transcode.ContinuousEncoder
	queue      *broadcastFrameQueue
	cancel     context.CancelFunc
	viewers    int
}
type broadcastMomentInput struct {
	fallback bool
	end      time.Time
}
type broadcastInput struct {
	policy                  *broadcastPolicyTag
	logger                  *slog.Logger
	openedAt, outputOrigin  time.Time
	wallStart               time.Time
	parkedDuration          atomic.Int64
	fpsNum, fpsDen          int
	overlays                *programmeOverlays
	started                 time.Time
	lastFrameAt             time.Time
	plan                    broadcastPlan
	decoder                 transcode.Decoder
	release                 func()
	ctx                     context.Context
	cancel                  context.CancelFunc
	frames                  chan transcode.RawFrame
	errs                    chan error
	activate                chan struct{}
	activateOnce, closeOnce sync.Once
}

func (i *broadcastInput) close() {
	if i == nil {
		return
	}
	i.closeOnce.Do(func() {
		i.cancel()
		i.decoder.Close()
		if i.release != nil {
			i.release()
		}
	})
}
func (i *broadcastInput) run() {
	defer close(i.frames)
	frameNo := int64(0)
	ready := false
	for {
		frame, err := i.decoder.Next(i.ctx)
		if err != nil {
			select {
			case i.errs <- err:
			default:
			}
			return
		}
		if !i.wallStart.IsZero() {
			at := i.wallStart.Add(time.Duration(i.parkedDuration.Load())).Add(time.Duration(frameNo) * time.Second * time.Duration(i.fpsDen) / time.Duration(i.fpsNum))
			frameNo++
			if at.Add(time.Second * time.Duration(i.fpsDen) / time.Duration(i.fpsNum)).Before(time.Now()) {
				continue
			}
		}
		if !ready {
			ready = true
			if i.logger != nil {
				i.logger.Debug("broadcast decoder ready", "input", i.plan.id, "programme", i.plan.programme, "prepareLatency", time.Since(i.openedAt), "sinceOutputStart", time.Since(i.outputOrigin))
			}
		}
		select {
		case i.frames <- frame:
		case <-i.ctx.Done():
			return
		}
		select {
		case <-i.activate:
		case <-i.ctx.Done():
			return
		}
	}
}
func (i *broadcastInput) activateNow() {
	if i != nil {
		i.activateOnce.Do(func() { close(i.activate) })
	}
}

type broadcastPrepared struct {
	input *broadcastInput
	err   error
	plan  broadcastPlan
}
type broadcast struct {
	startupUntil         time.Time
	startupTarget, lead  time.Duration
	bursting, burstReady bool
	introShownAt         time.Time
	introInput           *broadcastInput
	continuation         *broadcastInput
	continuationPlan     broadcastPlan
	continuationFrame    transcode.RawFrame
	continuationUntil    time.Time
	lastPodPlan          broadcastPlan
	lastPodFrame         transcode.RawFrame
	introFallback        bool
	introFade            *broadcastInput
	introFadeFrame       transcode.RawFrame
	introFadeStart       time.Time
	logger               *slog.Logger
	intro                broadcastPlan
	introUntil           time.Time
	dvr                  time.Duration
	black                []byte
	viewers              int
	startMu              sync.Mutex
	ctx                  context.Context
	cancel               context.CancelFunc
	format               transcode.RawFormat
	planner              broadcastPlanner
	runner               transcode.ContinuousRunner
	origin               time.Time
	mu                   sync.Mutex
	renditions           map[string]*broadcastRendition
	lastViewer           time.Time
	closed               bool
	wg                   sync.WaitGroup
	prepared             chan broadcastPrepared
	current, next        *broadcastInput
	later                map[string]*broadcastInput
	future               map[string]broadcastPlan
	opening              map[string]bool
	retry                map[string]time.Time
	lastFrame            transcode.RawFrame
	freshAt              time.Time
	fade                 pcmFade
	fallback             *broadcastInput
	momentCard           *broadcastInput
	momentCardPlan       broadcastPlan
	momentCardFrame      []byte
	screenPlan           broadcastPlan
	retryRequests        chan struct{}
	mode                 string
	plannedID            string
	momentInputs         map[string]broadcastMomentInput
	tick                 int64
	// yieldIdle asks the registry to close outputs nobody is watching when
	// this output hits the stream limit (see broadcastRegistry.yieldIdle).
	yieldIdle func(except *broadcast) bool
}

func newBroadcast(ctx context.Context, runner transcode.ContinuousRunner, planner broadcastPlanner, format transcode.RawFormat) (*broadcast, error) {
	if err := validateRawFormat(format); err != nil {
		return nil, err
	}
	if runner == nil || planner == nil {
		return nil, fmt.Errorf("broadcast requires runners and a planner")
	}
	ctx, cancel := context.WithCancel(ctx)
	b := &broadcast{ctx: ctx, cancel: cancel, runner: runner, planner: planner, format: format, origin: time.Now(), black: blackFrame(format, 0).Video, dvr: broadcastDVR, renditions: map[string]*broadcastRendition{}, opening: map[string]bool{}, retry: map[string]time.Time{}, prepared: make(chan broadcastPrepared, 4), retryRequests: make(chan struct{}, 1), lastViewer: time.Now()}
	b.logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	b.later = map[string]*broadcastInput{}
	b.future = map[string]broadcastPlan{}
	if source, ok := planner.(interface{ startupLead() time.Duration }); ok {
		b.startupTarget = source.startupLead()
	}
	if source, ok := planner.(interface{ broadcastLogger() *slog.Logger }); ok {
		b.logger = source.broadcastLogger()
	}
	if lifecycle, ok := planner.(interface{ startPlanner(context.Context) }); ok {
		lifecycle.startPlanner(ctx)
	}
	if planner, ok := planner.(interface {
		introPlan(time.Time) (broadcastPlan, time.Duration)
	}); ok {
		intro, d := planner.introPlan(b.origin)
		if intro.id != "" {
			intro.id = "cold-" + intro.id
		}
		b.intro = intro
		b.introUntil = b.origin.Add(d)
	}
	return b, nil
}

// Decoder shutdown waits for ffmpeg and playback reservations. It must never
// block the frame clock at a scheduled cut. close still joins these workers.
func (b *broadcast) closeInput(input *broadcastInput) {
	if input == nil {
		return
	}
	input.cancel()
	b.wg.Add(1)
	go func() { defer b.wg.Done(); input.close() }()
}

func (b *broadcast) start() {
	if b.startupTarget > 0 {
		b.startupUntil = time.Now().Add(20 * time.Second)
	}
	b.wg.Add(1)
	go func() { defer b.wg.Done(); b.run() }()
}
func (b *broadcast) close() {
	b.startMu.Lock()
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		b.startMu.Unlock()
		b.wg.Wait()
		return
	}
	b.closed = true
	b.cancel()
	b.mu.Unlock()
	b.startMu.Unlock()
	b.wg.Wait()
}
func (b *broadcast) addRendition(ctx context.Context, id string, decision core.Decision) (transcode.SegmentIndex, error) {
	b.startMu.Lock()
	defer b.startMu.Unlock()
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return nil, core.ErrNotFound
	}
	if r := b.renditions[id]; r != nil {
		r.viewers++
		b.viewers++
		b.lastViewer = time.Now()
		b.mu.Unlock()
		return r.encoder.Segments(), nil
	}
	if len(b.renditions) >= 2 {
		b.mu.Unlock()
		return nil, fmt.Errorf("channel rendition limit reached")
	}
	b.mu.Unlock()
	lifetime, cancel := context.WithCancel(b.ctx)
	stop := context.AfterFunc(ctx, cancel)
	encoder, err := b.runner.StartEncoder(lifetime, transcode.EncoderRequest{SessionID: id, Source: b.format, Decision: decision, SegmentDuration: 4 * time.Second, DVRWindow: b.dvr})
	stop()
	if err != nil {
		cancel()
		return nil, err
	}
	if ctx.Err() != nil || b.ctx.Err() != nil {
		cancel()
		encoder.Close()
		return nil, context.Canceled
	}
	r := &broadcastRendition{originDone: make(chan struct{}), height: decision.MaxHeight, id: id, encoder: encoder, queue: newBroadcastFrameQueue((b.format.FPSNum + b.format.FPSDen - 1) / b.format.FPSDen), cancel: cancel, viewers: 1}
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		cancel()
		encoder.Close()
		return nil, core.ErrNotFound
	}
	b.renditions[id] = r
	b.viewers++
	b.lastViewer = time.Now()
	b.wg.Add(2)
	b.mu.Unlock()
	go func() {
		defer b.wg.Done()
		defer encoder.Close()
		write := encoder.Write
		if shared, ok := encoder.(interface {
			WriteShared(context.Context, transcode.RawFrame) error
		}); ok {
			write = shared.WriteShared
		}
		for {
			frame, ok := r.queue.pop(lifetime)
			if !ok {
				return
			}
			if err := write(lifetime, frame); err != nil {
				b.logger.Warn("broadcast rendition write stopped", "rendition", id, "reason", err)
				b.dropRendition(id, r)
				return
			}
		}
	}()
	go func() {
		defer b.wg.Done()
		index := encoder.Segments()
		var first transcode.Segment
		var err error
		if segments := index.Since(-1, 1); len(segments) > 0 {
			first = segments[0]
		} else {
			first, err = index.Wait(lifetime, -1)
		}
		if err == nil && first.Seq != 0 {
			err = fmt.Errorf("continuous encoder did not expose its first segment")
		}
		b.mu.Lock()
		r.origin = first.Start
		r.originErr = err
		close(r.originDone)
		b.mu.Unlock()
	}()
	return encoder.Segments(), nil
}
func (b *broadcast) dropRendition(id string, r *broadcastRendition) {
	b.mu.Lock()
	if b.renditions[id] == r {
		delete(b.renditions, id)
		r.cancel()
		b.lastViewer = time.Now()
	}
	b.mu.Unlock()
}
func (b *broadcast) releaseRendition(id string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.viewers > 0 {
		b.viewers--
	}
	b.lastViewer = time.Now()
	if r := b.renditions[id]; r != nil {
		r.viewers--
		b.lastViewer = time.Now()
		if r.viewers <= 0 && len(b.renditions) > 1 {
			delete(b.renditions, id)
			r.cancel()
		}
	}
}
func (b *broadcast) idle(now time.Time) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.viewers == 0 && now.Sub(b.lastViewer) >= broadcastIdle
}
func (b *broadcast) open(plan broadcastPlan, now time.Time) {
	if b.current != nil && !b.current.lastFrameAt.IsZero() && resumesBroadcastInput(b.current.plan, plan) {
		return
	}
	if plan.id == b.intro.id && (b.introFallback || b.introInput != nil) {
		return
	}
	if b.momentInputs[plan.id].fallback {
		return
	}
	if plan.id == "" || plan.open == nil || b.opening[plan.id] || now.Before(b.retry[plan.id]) {
		return
	}
	b.opening[plan.id] = true
	age := time.Duration(0)
	if !plan.start.IsZero() {
		age = now.Sub(plan.start)
	}
	b.logger.Debug("broadcast decoder prepare started", "input", plan.id, "offsetAge", age)
	b.wg.Add(1)
	go func() {
		defer b.wg.Done()
		began := time.Now()
		d, release, err := plan.open(b.ctx)
		result := broadcastPrepared{plan: plan, err: err}
		defer func() {
			if b.ctx.Err() != nil && result.input != nil {
				result.input.close()
			}
		}()
		if err == nil {
			if d == nil {
				err = fmt.Errorf("decoder opener returned no decoder")
				result.err = err
			} else {
				result.input = newBroadcastInput(b.ctx, plan, d, release, b.format)
				result.input.logger = b.logger
				result.input.openedAt = began
				result.input.outputOrigin = b.origin
			}
		}
		if err != nil {
			if d != nil {
				d.Close()
			}
			if release != nil {
				release()
			}
		}
		select {
		case b.prepared <- result:
		case <-b.ctx.Done():
			if result.input != nil {
				result.input.close()
			}
		}
	}()
}
func (b *broadcast) accept(now time.Time, current broadcastPlan, next *broadcastPlan) {
	for {
		select {
		case result := <-b.prepared:
			delete(b.opening, result.plan.id)
			if result.err != nil {
				b.noteFailure(result.plan, result.err, now)
				if result.plan.id == b.intro.id {
					b.introUntil = now
				}
				continue
			}
			input := result.input
			if b.momentInputs[result.plan.id].fallback {
				b.closeInput(input)
				continue
			}
			if result.plan.id == b.continuationPlan.id {
				b.closeInput(b.continuation)
				b.continuation = input
				input.activateNow()
			} else if result.plan.id == b.intro.id {
				if b.introFallback {
					b.closeInput(input)
					continue
				}
				b.closeInput(b.introInput)
				b.introInput = input
				input.started = now
				input.activateNow()
			} else if result.plan.id == current.id {
				b.closeInput(b.current)
				b.current = input
				input.started = now
				input.activateNow()
			} else if result.plan.id == b.momentCardPlan.id {
				b.closeInput(b.momentCard)
				b.momentCard = input
				input.activateNow()
				input.started = now
			} else if result.plan.id == b.screenPlan.id {
				b.closeInput(b.fallback)
				b.fallback = input
				input.activateNow()
				input.started = now
			} else if next != nil && result.plan.id == next.id {
				b.closeInput(b.next)
				b.next = input
			} else if _, ok := b.future[result.plan.id]; ok {
				b.closeInput(b.later[result.plan.id])
				b.later[result.plan.id] = input
			} else {
				b.closeInput(input)
				continue
			}
			b.wg.Add(1)
			go func() { defer b.wg.Done(); input.run() }()
		default:
			return
		}
	}
}
func (b *broadcast) noteFailure(p broadcastPlan, err error, now time.Time) {
	details := ""
	if b.current != nil && b.current.plan.id == p.id {
		d := b.current.decoder
		if wrapped, ok := d.(*overlayDecoder); ok {
			d = wrapped.Decoder
		}
		if source, ok := d.(interface{ Diagnostics() string }); ok {
			details = source.Diagnostics()
		}
	}
	b.logger.Debug("broadcast input abandoned", "input", p.id, "reason", err, "context", context.Cause(b.ctx), "decoder", details)
	retry := now.Add(15 * time.Second)
	if p.start.After(now) {
		// A prewarm cache miss is not a failed airing. Keep preparing until
		// the boundary; rendering may still be running in the background.
		retry = now.Add(time.Second)
	} else if p.failed != nil {
		retry = p.failed(err)
	}
	// Channels left in their idle grace still hold stream slots. A viewer who
	// switches channels must not wait out that grace on a limited server.
	if errors.Is(err, core.ErrStreamLimit) && b.yieldIdle != nil && b.yieldIdle(b) {
		if soon := now.Add(time.Second); soon.Before(retry) {
			retry = soon
		}
	}
	b.retry[p.id] = retry
}
func (b *broadcast) unwatched(now time.Time) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	// A brand-new output has no viewer until acquire attaches its rendition.
	return b.viewers == 0 && !b.closed && now.Sub(b.origin) >= 2*time.Second
}
func (b *broadcast) run() {
	defer func() {
		b.logger.Debug("broadcast stopped", "reason", context.Cause(b.ctx))
		b.cancel()
		if lifecycle, ok := b.planner.(interface{ closePlanner() }); ok {
			lifecycle.closePlanner()
		}
		b.mu.Lock()
		b.closed = true
		b.mu.Unlock()
		b.closeInput(b.current)
		b.closeInput(b.next)
		for _, input := range b.later {
			b.closeInput(input)
		}
		b.closeInput(b.fallback)
		b.closeInput(b.momentCard)
		b.closeInput(b.introInput)
		b.closeInput(b.continuation)
		b.closeInput(b.introFade)
		b.mu.Lock()
		for _, r := range b.renditions {
			r.cancel()
		}
		b.mu.Unlock()
		for {
			select {
			case result := <-b.prepared:
				if result.input != nil {
					result.input.close()
				}
			default:
				return
			}
		}
	}()
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-b.ctx.Done():
			return
		case <-timer.C:
		}
		now := time.Now()
		if b.idle(now) {
			return
		}
		scheduled := b.origin.Add(time.Duration(b.tick) * time.Second * time.Duration(b.format.FPSDen) / time.Duration(b.format.FPSNum))
		b.bursting = !b.startupUntil.IsZero() && now.Before(b.startupUntil) && !b.startupOutputReady(now)
		if !b.startupUntil.IsZero() && !b.bursting {
			b.lead = max(0, min(b.startupTarget+4*time.Second, scheduled.Sub(now)))
			b.startupUntil = time.Time{}
			b.logger.Info("broadcast startup burst finished", "lead", b.lead, "elapsed", now.Sub(b.origin))
		}
		// Feed a bounded amount ahead of the encoder. Startup must not turn a
		// healthy fast encoder into a video-degradation event through queue growth.
		// Due ticks still enter the continuous PCM queue: pacing those behind a
		// slow encoder would prevent the queue's picture degradation from helping.
		if b.bursting && scheduled.After(now) && (scheduled.After(now.Add(b.startupTarget+4*time.Second)) || b.startupQueueFull()) {
			timer.Reset(2 * time.Millisecond)
			continue
		}
		if !b.step(scheduled) {
			timer.Reset(2 * time.Millisecond)
			continue
		}
		b.tick++
		next := b.origin.Add(time.Duration(b.tick) * time.Second * time.Duration(b.format.FPSDen) / time.Duration(b.format.FPSNum)).Add(-b.lead)
		delay := time.Until(next)
		if b.bursting {
			delay = 0
			if !b.burstReady {
				delay = time.Until(next.Add(-time.Second))
			}
		}
		if delay < 0 {
			delay = 0
		}
		timer.Reset(delay)
	}
}
func (b *broadcast) step(now time.Time) bool {
	b.burstReady = false
	fresh := false
	previousPCM := b.lastFrame.PCM
	current, next := b.planner.plans(now)
	if b.plannedID != current.id {
		if current.programme && b.lastPodPlan.id != "" && b.plannedID != "" {
			b.closeInput(b.continuation)
			b.continuation = nil
			b.continuationPlan = b.lastPodPlan
			b.continuationPlan.id = "pod-fill-" + b.lastPodPlan.id
			b.continuationPlan.failed = nil
			b.continuationUntil = now.Add(broadcastStartup)
			b.continuationFrame = b.lastPodFrame
			b.lastPodPlan = broadcastPlan{}
			b.lastPodFrame = transcode.RawFrame{}
		}
		b.plannedID = current.id
		b.freshAt = time.Time{}
	}
	clear(b.future)
	if planner, ok := b.planner.(interface {
		prewarmPlans(time.Time) []broadcastPlan
	}); ok {
		for _, plan := range planner.prewarmPlans(now) {
			b.future[plan.id] = plan
		}
	}
	if planner, ok := b.planner.(interface{ screen(time.Time) broadcastPlan }); ok {
		b.screenPlan = planner.screen(now)
	}
	if planner, ok := b.planner.(interface{ momentScreen() broadcastPlan }); ok {
		b.momentCardPlan = planner.momentScreen()
	}
	if !now.Before(b.introUntil) && b.introInput != nil {
		b.finishIntro(now)
	}
	introActive := b.intro.id != "" && now.Before(b.introUntil)
	if introActive && !b.introFallback && b.introInput == nil {
		b.open(b.intro, now)
	}

	select {
	case <-b.retryRequests:
		if b.current != nil {
			b.closeInput(b.current)
			b.current = nil
		}
		delete(b.retry, current.id)
	default:
	}
	if b.fallback != nil && b.fallback.plan.id != b.screenPlan.id {
		b.closeInput(b.fallback)
		b.fallback = nil
	}
	b.accept(now, current, next)
	// Keep one bounded screen input prewarmed while programmes play. A stall
	// then cuts to its first frame at the 1.5 s deadline, without raster/decoder
	// initialization on the frame clock or an intervening black frame.
	if b.fallback == nil {
		b.open(b.screenPlan, now)
	}
	if b.momentCard == nil {
		b.open(b.momentCardPlan, now)
	}
	if b.current != nil && b.current.plan.id != current.id {
		retained := false
		for id, plan := range b.future {
			if !b.current.lastFrameAt.IsZero() && resumesBroadcastInput(b.current.plan, plan) && (b.current.policy == nil || b.current.policy.programme.Item.Key == plan.mediaKey) {
				b.closeInput(b.later[id])
				if b.next != nil && b.next.plan.id == id {
					b.closeInput(b.next)
					b.next = nil
				}
				b.current.parkedDuration.Add(int64(plan.start.Sub(b.current.plan.end)))
				b.current.plan = plan
				b.current.policy = plan.policy
				b.later[id] = b.current
				b.logger.Debug("broadcast programme parked for pod", "resume", id, "boundary", plan.start)
				retained = true
				break
			}
		}
		if !retained && b.current.plan.interstitial && current.programme && !b.current.lastFrameAt.IsZero() {
			b.closeInput(b.continuation)
			b.continuation = b.current
			b.continuationPlan = b.current.plan
			b.continuationPlan.id = "pod-fill-" + b.current.plan.id
			b.continuationPlan.failed = nil
			b.continuationUntil = now.Add(broadcastStartup)
			b.continuationFrame = b.lastFrame
			retained = true
		}
		if !retained {
			b.closeInput(b.current)
		}
		b.current = nil
		b.freshAt = time.Time{}
	}
	if b.next != nil && b.next.plan.id == current.id {
		b.current = b.next
		b.next = nil
		b.current.started = now
		b.current.activateNow()
	}
	if input := b.later[current.id]; input != nil {
		b.closeInput(b.current)
		b.current = input
		delete(b.later, current.id)
		input.started = now
		input.activateNow()
	}
	if next != nil && b.next == nil {
		if input := b.later[next.id]; input != nil {
			b.next = input
			delete(b.later, next.id)
		}
	}
	for id, input := range b.later {
		if _, ok := b.future[id]; !ok {
			b.closeInput(input)
			delete(b.later, id)
		}
	}
	if b.next != nil && (next == nil || b.next.plan.id != next.id) {
		b.closeInput(b.next)
		b.next = nil
	}
	if b.current == nil {
		b.open(current, now)
	}
	if next != nil && next.start.Sub(now) <= broadcastPrewarm && b.next == nil {
		b.open(*next, now)
	}
	for id, plan := range b.future {
		if (b.next == nil || b.next.plan.id != id) && b.later[id] == nil && !resumesBroadcastInput(current, plan) {
			b.open(plan, now)
		}
	}
	// The last tick before a moment selects its input once. Decoders that
	// become ready after that deadline must not replace a live fallback in
	// the middle of the interval.
	frameDuration := time.Second * time.Duration(b.format.FPSDen) / time.Duration(b.format.FPSNum)
	if next != nil && !next.start.After(now.Add(frameDuration)) {
		b.latchMomentInput(*next, now)
	}
	if !introActive {
		b.latchMomentInput(current, now)
	}
	momentFallback := b.momentInputs[current.id].fallback
	samples := tickSamples(b.format, b.tick)
	frame := transcode.RawFrame{}
	mode := "screen"
	if b.current != nil && !momentFallback && (!introActive || b.intro.fill) {
		select {
		case f, ok := <-b.current.frames:
			if ok && validRawFrame(b.format, f) {
				b.burstReady = true
				frame = f
				if b.current.policy != nil {
					frame.PolicyTags = []any{b.current.policy}
				}
				if introActive && b.intro.fill {
					if b.introFallback || (!b.introShownAt.IsZero() && now.Sub(b.introShownAt) >= time.Second) {
						b.finishIntro(now)
						introActive = false
					}
				}
				if introActive {
					frame = transcode.RawFrame{}
					break
				}
				if source, ok := b.current.decoder.(interface{ Aired(transcode.RawFrame) }); ok {
					source.Aired(frame)
				}
				if b.current.lastFrameAt.IsZero() && current.programme {
					b.logger.Debug("broadcast programme first aired", "input", current.id, "prepareLatency", now.Sub(b.current.started), "sinceOutputStart", now.Sub(b.origin))
				}
				if current.interstitial && current.element != "offair" {
					b.lastPodPlan = current
					b.lastPodFrame = frame
				}
				b.freshAt = now
				b.current.lastFrameAt = now
				fresh = true
				mode = current.id
			}
		default:
		}
		if frame.Video == nil {
			select {
			case err := <-b.current.errs:
				if errors.Is(err, io.EOF) && current.interstitial && !current.end.After(now.Add(broadcastStall)) {
					// A finite clip that runs out within the stall bridge of its
					// rounded slot ended normally: its last picture holds to the
					// boundary rather than latching the live fallback.
					b.retry[current.id] = current.end
				} else {
					b.noteFailure(current, err, now)
				}
				b.closeInput(b.current)
				b.current = nil
			default:
			}
		}
	}
	startupExpired := b.current != nil && b.current.lastFrameAt.IsZero() && now.Sub(b.current.started) >= broadcastStartup
	stalled := b.current != nil && !b.current.lastFrameAt.IsZero() && now.Sub(b.current.lastFrameAt) >= broadcastStall
	if frame.Video == nil && !introActive && b.current != nil && (startupExpired || stalled) {
		b.noteFailure(current, fmt.Errorf("decoder stalled"), now)
		b.closeInput(b.current)
		b.current = nil
	}
	// Ahead-of-clock production may wait for its input. A due tick must keep
	// the live fallback flowing even when resolution has not produced a frame;
	// otherwise a missing source can prevent an opening segment from finishing.
	// A cold decoder with no first picture must not halt the startup cushion
	// after the ident ends: keep encoding the ready live fallback while it warms.
	// An exhausted file's final picture still bridges its rounded slot duration.
	coldWaiting := b.current != nil && b.current.lastFrameAt.IsZero() && !b.current.openedAt.IsZero() && time.Since(b.current.openedAt) >= 250*time.Millisecond
	if b.bursting && now.After(time.Now()) && frame.Video == nil && !introActive && b.current != nil && !coldWaiting && !momentFallback {
		return false
	}
	if frame.Video == nil && !introActive && !b.freshAt.IsZero() && now.Sub(b.freshAt) < broadcastStall {
		frame = transcode.RawFrame{Video: b.lastFrame.Video, PolicyTags: b.lastFrame.PolicyTags}
		mode = "hold"
	}
	if fresh || !current.programme || !now.Before(b.continuationUntil) {
		b.closeInput(b.continuation)
		b.continuation = nil
		b.continuationPlan = broadcastPlan{}
		b.continuationFrame = transcode.RawFrame{}
	} else if frame.Video == nil && b.continuationPlan.id != "" {
		if b.continuation != nil {
			select {
			case f, ok := <-b.continuation.frames:
				if ok && validRawFrame(b.format, f) {
					b.continuationFrame = f
					frame = f
				} else {
					b.closeInput(b.continuation)
					b.continuation = nil
				}
			default:
			}
		}
		if b.continuation == nil {
			b.open(b.continuationPlan, now)
		}
		if frame.Video == nil {
			frame = transcode.RawFrame{Video: b.continuationFrame.Video}
		}
		mode = "pod-extension"
	}
	if frame.Video == nil && introActive && !b.introFallback && b.introInput != nil {
		select {
		case f, ok := <-b.introInput.frames:
			if ok && validRawFrame(b.format, f) {
				frame = f
				mode = "intro"
				if b.introShownAt.IsZero() {
					b.introShownAt = now
				}
			}
		default:
		}
	}
	if momentFallback && !introActive && b.momentCardPlan.id != "" {
		// A cold tune may land inside a missing moment. Wait for its own
		// prewarmed plate before the first picture instead of briefly airing
		// a faster generic source-error screen.
		frame = transcode.RawFrame{Video: b.momentCardFrame}
		if b.momentCard != nil {
			select {
			case f, ok := <-b.momentCard.frames:
				if ok && validRawFrame(b.format, f) {
					frame = f
					b.momentCardFrame = f.Video
				}
			default:
			}
		}
		if frame.Video == nil {
			if b.tick == 0 && time.Since(b.origin) < time.Second {
				return false
			}
			frame.Video = b.black
		}
		b.burstReady = true
	}
	if frame.Video == nil {
		// Give an already cached welcome a bounded chance to decode its first
		// picture. Its preparation never replaces the normal live signal input.
		if b.tick == 0 && introActive && !b.introFallback && time.Since(b.origin) < 200*time.Millisecond {
			return false
		}
		if b.fallback == nil {
			b.open(b.screenPlan, now)
		} else {
			select {
			case f, ok := <-b.fallback.frames:
				if ok && validRawFrame(b.format, f) {
					frame = f
					if introActive {
						b.introFallback = true
						b.closeInput(b.introInput)
						b.introInput = nil
					}
				}
			default:
			}
			select {
			case err := <-b.fallback.errs:
				b.noteFailure(b.screenPlan, err, now)
				b.closeInput(b.fallback)
				b.fallback = nil
			default:
			}
		}
		if b.bursting && now.After(time.Now()) && frame.Video == nil {
			return false
		}
		if frame.Video == nil {
			if b.tick == 0 && time.Since(b.origin) < time.Second {
				return false
			}
			frame = transcode.RawFrame{Video: b.lastFrame.Video, PolicyTags: b.lastFrame.PolicyTags}
			if frame.Video == nil {
				frame = transcode.RawFrame{Video: b.black}
			}
		}
	}
	if momentFallback && !introActive {
		mode = "fallback/" + current.id
	}
	// PCM follows the output tick, including fractional frame rates. Never let a
	// malformed decoder chunk shift every subsequent audio timestamp.
	pcm := make([]byte, samples*b.format.Channels*2)
	copy(pcm, frame.PCM)
	frame.PCM = pcm
	cut := mode != b.mode
	if cut {
		element := current.element
		if mode == "pod-extension" {
			element = b.continuationPlan.element
		} else if mode == "intro" {
			element = b.intro.element
			if element == "" {
				element = "ident"
			}
		} else if mode == "screen" {
			element = b.screenPlan.element
			if element == "" {
				element = "screen"
			}
		} else if mode == "hold" {
			element = "hold"
		} else if element == "" && current.programme {
			element = "programme"
		}
		transition := "cut"
		if b.introFade != nil {
			transition = "crossfade"
		}
		b.logger.Debug("broadcast output switched", "element", element, "fallback", momentFallback, "transition", transition, "from", b.mode, "input", mode, "tick", b.tick, "scheduledAt", now, "clockLag", time.Since(now))
		if b.introFade == nil {
			b.fade.start(previousPCM, b.format)
		}
		b.mode = mode
	}
	b.fade.apply(frame.PCM, b.format.Channels)
	// Next transfers an exclusively owned video buffer. Composite a fresh frame
	// once in place, then publish it immutably to all renditions. A stalled input
	// holds that exact completed picture; it must not accumulate another blend.
	if fresh && b.current != nil && b.current.overlays != nil {
		b.current.overlays.blend(frame.Video, b.format, now, current.start, current.end)
	}
	frame.PolicyTags = mergeBroadcastTags(frame.PolicyTags, []any{current.policy})
	b.blendIntro(now, &frame, fresh)
	// Start rendering the first screen immediately while resolution is pending,
	// while a quick decoder gets its first picture. If that cold preparation
	// takes longer, the ready live screen must fill the playable cushion too;
	// otherwise the ident can leave an already-playing client at a bare edge.
	b.burstReady = b.burstReady || (introActive && !b.intro.fill) || (mode == "screen" && frame.Video != nil && time.Since(b.origin) >= 250*time.Millisecond)
	frame.OutputTime = now
	b.lastFrame = frame
	b.mu.Lock()
	for id, r := range b.renditions {
		slow := false
		if encoder, ok := r.encoder.(interface{ RealtimeSpeed() float64 }); ok {
			speed := encoder.RealtimeSpeed()
			slow = speed > 0 && speed < 0.98
		}
		changed, repeating, pending := r.queue.push(frame, cut, slow)
		if changed {
			b.logger.Info("broadcast encoder video cadence changed", "rendition", id, "repeating", repeating, "pendingTicks", pending, "tick", b.tick)
		}
		if b.tick%300 == 0 || (b.bursting && b.tick%30 == 0) {
			progress := ""
			if source, ok := r.encoder.(interface{ Diagnostics() string }); ok {
				progress = source.Diagnostics()
			}
			b.logger.Debug("broadcast encoder progress", "rendition", id, "inputTick", b.tick, "pendingTicks", pending, "repeating", repeating, "encoder", progress)
		}
	}
	b.mu.Unlock()
	return true
}

func newBroadcastInput(parent context.Context, plan broadcastPlan, d transcode.Decoder, release func(), format transcode.RawFormat) *broadcastInput {
	ctx, cancel := context.WithCancel(parent)
	capacity := 8
	if plan.programme {
		capacity = max(capacity, 3*format.FPSNum/format.FPSDen)
	}
	input := &broadcastInput{ctx: ctx, cancel: cancel, decoder: d, release: release, plan: plan, frames: make(chan transcode.RawFrame, capacity), errs: make(chan error, 1), activate: make(chan struct{})}
	if decoder, ok := d.(*overlayDecoder); ok {
		input.policy = decoder.policy
		input.overlays = decoder.overlays
		input.wallStart = decoder.wallStart
		input.fpsNum = decoder.format.FPSNum
		input.fpsDen = decoder.format.FPSDen
	}
	return input
}

func (b *broadcast) requestRetry() {
	select {
	case b.retryRequests <- struct{}{}:
	default:
	}
}

func (b *broadcast) latchMomentInput(plan broadcastPlan, now time.Time) {
	if !plan.interstitial {
		return
	}
	if _, decided := b.momentInputs[plan.id]; decided {
		return
	}
	input := b.current
	if input == nil || input.plan.id != plan.id {
		input = b.next
	}
	if input == nil || input.plan.id != plan.id {
		input = b.later[plan.id]
	}
	ready := input != nil && len(input.frames) > 0 && !plan.fallback
	if b.momentInputs == nil {
		b.momentInputs = map[string]broadcastMomentInput{}
	}
	b.momentInputs[plan.id] = broadcastMomentInput{fallback: !ready, end: plan.end}
	for id, decision := range b.momentInputs {
		if decision.end.Before(now) {
			delete(b.momentInputs, id)
		}
	}
	if !ready {
		b.closeInput(input)
		b.logger.Debug("broadcast moment live fallback selected", "input", plan.id, "element", plan.element, "boundary", plan.start, "decisionAt", now)
	}
}

func (b *broadcast) startupQueueFull() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, r := range b.renditions {
		r.queue.mu.Lock()
		pending := len(r.queue.frames) - r.queue.head
		r.queue.mu.Unlock()
		fps := max(1, b.format.FPSNum/b.format.FPSDen)
		limit := max(1, fps/2)
		// A startup speed sample includes encoder priming and is not a
		// sustainable throughput measurement. Pace the burst before queue
		// pressure can reduce pictures; hosts without burst margin simply
		// retain the smaller lead at the bounded startup deadline.
		if pending >= limit {
			return true
		}
	}
	return false
}

// The startup cushion is measured at the published edge, not at the producer
// queue. Encoder priming and queued raw ticks are not playable client media.
func (b *broadcast) startupOutputReady(now time.Time) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.renditions) == 0 {
		return false
	}
	for _, r := range b.renditions {
		last, ok := r.encoder.Segments().Latest()
		if !ok || last.Start.Add(last.Duration).Before(now.Add(b.startupTarget)) {
			return false
		}
	}
	return true
}

// Only consecutive slices of the same media can share a decoder. Repeated
// episodes beginning at offset zero still receive a fresh input.
func resumesBroadcastInput(previous, next broadcastPlan) bool {
	return previous.programme && next.programme && previous.mediaKey != 0 && previous.mediaKey == next.mediaKey && next.sourceOffset > 0 && !next.start.Before(previous.end) && next.sourceOffset == previous.sourceOffset+previous.end.Sub(previous.start)
}
