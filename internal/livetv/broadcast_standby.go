package livetv

import (
	"context"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/braedonsaunders/ether/internal/transcode"
)

// steadyStandby is a backup input opened at its live edge while the primary
// shows stress (a delayed hot-standby switch). Its decoder is held by
// backpressure once its first pictures are buffered, so adopting it on
// failure splices media without a cold open, continuing exactly where it
// paused: a playlist input keeps the rest of the segment it fetched instead
// of waiting for the provider's next one. It holds its own provider
// connection, so it only exists while the account's budget has room.
type steadyStandby struct {
	src      StreamSource
	link     *steadyLink
	decoder  transcode.Decoder
	frames   chan transcode.RawFrame
	err      error
	produced atomic.Bool
	failed   atomic.Bool
	done     chan struct{}
	cancel   context.CancelFunc // stops run, which may be parked on a full frames channel
	once     sync.Once
}

// startSteadyStandby starts buffering the decoder's first frames.
func startSteadyStandby(ctx context.Context, src StreamSource, link *steadyLink, decoder transcode.Decoder, buffer int) *steadyStandby {
	ctx, cancel := context.WithCancel(ctx)
	sb := &steadyStandby{src: src, link: link, decoder: decoder, frames: make(chan transcode.RawFrame, buffer), done: make(chan struct{}), cancel: cancel}
	go sb.run(ctx)
	return sb
}

func (sb *steadyStandby) ready() bool {
	return sb != nil && sb.produced.Load() && !sb.failed.Load()
}

func (sb *steadyStandby) run(ctx context.Context) {
	defer close(sb.done)
	defer close(sb.frames)
	for {
		frame, err := sb.decoder.Next(ctx)
		if err != nil {
			sb.err = err
			sb.failed.Store(true)
			return
		}
		sb.produced.Store(true)
		select {
		case sb.frames <- frame:
		case <-ctx.Done():
			sb.err = ctx.Err()
			return
		}
	}
}

// adopt hands the standby's decoder to the channel clock.
func (sb *steadyStandby) adopt() transcode.Decoder {
	return &standbyDecoder{sb: sb}
}

func (sb *steadyStandby) close(s *Service) {
	if sb == nil {
		return
	}
	sb.once.Do(func() {
		sb.cancel()
		sb.decoder.Close()
		<-sb.done
		sb.link.close(s)
	})
}

type standbyDecoder struct{ sb *steadyStandby }

func (d *standbyDecoder) Next(ctx context.Context) (transcode.RawFrame, error) {
	select {
	case <-ctx.Done():
		return transcode.RawFrame{}, ctx.Err()
	case frame, ok := <-d.sb.frames:
		if !ok {
			return transcode.RawFrame{}, d.sb.err
		}
		return frame, nil
	}
}

// Close stops the decoder; the adopted link now belongs to the planner.
func (d *standbyDecoder) Close() error {
	d.sb.once.Do(func() {
		d.sb.cancel()
		d.sb.decoder.Close()
		<-d.sb.done
	})
	return nil
}

func (d *standbyDecoder) StarvingErrors() (time.Duration, bool) {
	if source, ok := d.sb.decoder.(starving); ok {
		return source.StarvingErrors()
	}
	return 0, false
}

func (d *standbyDecoder) Diagnostics() string {
	if source, ok := d.sb.decoder.(interface{ Diagnostics() string }); ok {
		return "standby " + source.Diagnostics()
	}
	return "standby"
}

// watchStandby opens a backup when the primary input shows stress and
// closes it once the primary has stayed healthy for steadyStandbyLinger.
// Stress is upstream data older than a second when little reserve remains,
// or, without that signal, a decoder that produced no picture for a second.
func (p *providerBroadcastPlanner) watchStandby(ctx context.Context) {
	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()
	serial := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		now := time.Now()
		p.mu.Lock()
		a, sb := p.attempt, p.standby
		p.mu.Unlock()
		if sb != nil && sb.failed.Load() {
			p.mu.Lock()
			if p.standby == sb {
				p.standby = nil
			}
			p.mu.Unlock()
			sb.close(p.s)
			continue
		}
		if a == nil {
			// Between attempts: when the input's own transport has recovered
			// (the tap reconnected after the ladder backed off), open now
			// rather than at the backed-off retry time. Never trigger this
			// while a stalled current input is still open in a parallel race.
			p.mu.Lock()
			link, waiting, retry, stalled := p.link, p.waiting, p.retry, p.stalledAttempt
			p.mu.Unlock()
			if stalled == nil && waiting && link != nil && retry != nil {
				if h, ok := p.s.inputHealth(link.ss); ok && h.Connected && !h.Failed {
					p.mu.Lock()
					p.waiting = false
					p.mu.Unlock()
					p.logger().Debug("live input recovered while waiting", "input", link.src.ID)
					retry()
				}
			}
			continue
		}
		stressed := a.decoder.stressed(now)
		h, healthy := p.s.inputHealth(a.link.ss)
		stressAge := steadyStress
		if healthy && h.StallAfter > tapStallAfter {
			stressAge = h.StallAfter
		}
		if healthy && (!h.Connected || h.Stalled || h.Failed || (h.DataAge > stressAge && h.ReserveDepth < 2*time.Second)) {
			stressed = true
		}
		if healthy && h.LiveSocket && !h.Failed && h.StallAfter > tapStallAfter && h.DataAge < h.StallAfter && a.decoder.sinceDecoded(now) < h.StallAfter {
			stressed = false
		}
		// Switch on failure: once the primary stops delivering pictures past
		// its adapted threshold (or its tap gave up) a ready standby takes over
		// at once, before the clock's picture hold would expire.
		starved := a.decoder.starved(now)
		if starved && healthy && h.LiveSocket && !h.Failed && a.decoder.sinceDecoded(now) < h.StallAfter {
			starved = false
		}
		if sb.ready() && (starved || healthy && h.Failed) {
			p.mu.Lock()
			retry := p.retry
			if p.attempt == a && p.standby == sb {
				p.attempt = nil
				p.ladder.next, p.ladder.liveEdge = sb.src.ID, false
			} else {
				retry = nil
			}
			p.mu.Unlock()
			if retry != nil {
				p.s.recordSourceHealth(p.channel.ID, a.src, errSteadyStarved, false)
				p.logger().Info("live input lost", append([]any{"input", a.src.ID, "reason", errSteadyStarved, "next", sb.src.ID, "standby", true}, p.s.duplicateFailoverLogAttrs(p.channel, a.src, sb.src)...)...)
				retry()
			}
			continue
		}
		if stressed {
			p.mu.Lock()
			p.lastStress = now
			racing := p.stalledAttempt != nil
			p.mu.Unlock()
			if sb == nil && !racing {
				serial++
				p.openStandby(ctx, a.src, serial)
			}
			continue
		}
		p.mu.Lock()
		quiet := now.Sub(p.lastStress) >= steadyStandbyLinger
		if quiet && p.standby == sb && sb != nil {
			p.standby = nil
		} else {
			sb = nil
		}
		p.mu.Unlock()
		if sb != nil {
			p.logger().Debug("live standby closed", "input", sb.src.ID)
			sb.close(p.s)
		}
	}
}

func (p *providerBroadcastPlanner) openStandby(ctx context.Context, primary StreamSource, serial int) {
	c := p.channel
	for _, src := range p.s.orderedSourceCandidates(c, primary.ID) {
		p.mu.Lock()
		parked := p.ladder.sources[src.ID] != nil && p.ladder.sources[src.ID].parked.After(p.s.o.Now())
		p.mu.Unlock()
		if parked {
			continue
		}
		link, err := p.s.linkSteadySource(ctx, c, src, "broadcast-"+c.ID+steadyStandbyID+"-"+strconv.Itoa(serial), 0)
		if err != nil {
			// A busy account has no room for a second connection; never
			// overlap a capped account.
			continue
		}
		input, err := p.s.relayInputLive(link.ss)
		var decoder transcode.Decoder
		if err == nil {
			decoder, err = p.runner.StartDecoder(ctx, transcode.DecoderRequest{SessionID: link.ss.info.ID, InputURL: input, Format: p.format, Live: true, InputFormat: link.format})
		}
		if err != nil {
			link.close(p.s)
			continue
		}
		sb := startSteadyStandby(ctx, src, link, decoder, max(1, p.format.FPSNum/p.format.FPSDen/2))
		p.mu.Lock()
		if p.standby != nil || ctx.Err() != nil {
			p.mu.Unlock()
			sb.close(p.s)
			return
		}
		p.standby = sb
		p.mu.Unlock()
		p.logger().Info("live standby opened", append([]any{"input", src.ID, "primary", primary.ID}, p.s.duplicateFailoverLogAttrs(c, primary, src)...)...)
		return
	}
}
