package livetv

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/transcode"
)

// The relay (ffmpeg copy or conversion) also publishes onto an owned
// timeline. The ffmpeg session is only a producer: when its source stalls,
// when the session gives up, or when copy mode cannot cut a segment because
// keyframes stopped arriving, Ether replaces the producer (another source,
// or the same input re-encoded) and splices the result in with a
// discontinuity while slate keeps the client's playlist moving.

// localRelayOrigin reads a local relay session's playlist and segments.
type localRelayOrigin struct {
	hls  core.HLSSession
	name string
}

func (o *localRelayOrigin) label() string { return o.name }

func (o *localRelayOrigin) media(ctx context.Context) ([]byte, *url.URL, error) {
	rec := httptest.NewRecorder()
	o.hls.ServePlaylist(rec, httptest.NewRequest(http.MethodGet, "/index.m3u8", nil).WithContext(ctx))
	if rec.Code != http.StatusOK {
		return nil, nil, fmt.Errorf("livetv: relay playlist status %d: %w", rec.Code, errOriginEnded)
	}
	return rec.Body.Bytes(), nil, nil
}

func (o *localRelayOrigin) get(ctx context.Context, uri string, _, _ int64) ([]byte, error) {
	name := uri
	if u, err := url.Parse(uri); err == nil {
		name = u.Path
	}
	n, ok := transcode.ParseSegmentName(name)
	if !ok {
		return nil, fmt.Errorf("livetv: unknown relay resource %q", name)
	}
	rec := httptest.NewRecorder()
	o.hls.ServeSegment(rec, httptest.NewRequest(http.MethodGet, "/"+strings.TrimPrefix(name, "/"), nil).WithContext(ctx), n)
	if rec.Code != http.StatusOK && rec.Code != http.StatusPartialContent {
		return nil, fmt.Errorf("livetv: relay segment %d status %d", n, rec.Code)
	}
	return rec.Body.Bytes(), nil
}

type relayProducer struct {
	s    *Service
	tl   *ownedTimeline
	log  *slog.Logger
	sess atomic.Pointer[session]
	caps core.ClientCaps // the relay's output capabilities

	mu        sync.Mutex
	hls       core.HLSSession
	cancel    context.CancelFunc
	info      core.MediaInfo // the current source's probe
	dec       core.Decision
	cut       time.Duration
	gen       int
	ingest    *hlsIngest
	escalated bool
	starved   bool
	// hedge is a re-encode started beside a copy producer that is slow to
	// cut its first segment; whichever cuts first becomes the producer.
	hedge *relayHedge
	// overlap is a producer started on the input's new format the moment
	// the tap saw its program change, before the old producer could cut a
	// mislabeled segment; formatGen is the program generation in use.
	overlap   *relayHedge
	formatGen uint64
	// steady is a Steady hand-off feed that replaced the relay; steadyReady
	// one waiting for the run loop to splice it in.
	steady        SteadyFeed
	steadyReady   SteadyFeed
	steadyClass   string
	steadyBusy    bool
	steadyRetryAt time.Time
	// churnAt and slateRuns are the run loop's own (no lock).
	churnAt   []time.Time
	slateRuns int
	// starving is set while a running copy producer is past its next
	// segment with input flowing; producing while input flows and nothing
	// is starving. The timeline reads both without locks.
	starving  atomic.Bool
	producing atomic.Bool
	// inputStalled mirrors the input's health: stalled, failed, or a TS
	// tap with no byte for a second.
	inputStalled atomic.Bool
	// damaged is set when a copy segment failed its decode check: the
	// source is delivering damage copy passes straight to the player.
	damaged atomic.Bool
	longAt  time.Time // the last overlong copy segment
	// reformat holds the source's new format when copy output stopped
	// matching its own PMT; the producer reopens on the new format.
	reformat  *mediaFormat
	reopened  time.Time
	startedAt time.Time
	progress  time.Time
	closed    bool
}

// bind points the producer at the published session after a staged tune.
func (p *relayProducer) bind(ss *session) { p.sess.Store(ss) }

// current is the relay session producing now (tests and diagnostics).
func (p *relayProducer) current() core.HLSSession {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.hls
}

// relayTargetFloor is the cut itself: an Apple player reloads a changed
// live playlist only once per target duration, so a target longer than the
// segments delivers each one late. Copy cuts at the first source keyframe
// after the cut, and the first segments' real lengths raise the target for a
// longer GOP.
func relayTargetFloor(cut time.Duration) time.Duration {
	return cut
}

// startOwnedRelay starts the first relay producer and its timeline.
func (s *Service) startOwnedRelay(ctx context.Context, ss *session, info core.MediaInfo, caps core.ClientCaps, dec core.Decision, cut time.Duration) (*ownedLease, error) {
	p := s.newRelayProducer(ss, info, caps, dec, cut)
	if err := p.start(ctx, info, dec, cut, false); err != nil {
		return nil, err
	}
	return s.runRelay(ss, p), nil
}

func (s *Service) newRelayProducer(ss *session, info core.MediaInfo, caps core.ClientCaps, dec core.Decision, cut time.Duration) *relayProducer {
	c := ss.info.Channel
	log := s.sessionLog(ss).With("owned", "relay")
	// A relay delivers in bursts (an HLS input arrives a provider segment
	// at a time; ffmpeg encodes it at many times real time) and starts
	// from the input's backlog. Publishing at real time from a reserve of
	// at least two segments turns bursts into a steady playlist, and the
	// backlog primes the channel's stability reserve.
	tl := newOwnedTimeline(ss.info.ID, log, nil, max(s.stabilityReserve(c), 2*cut), s.slateFor(c))
	output := liveRelayInfo(info, dec)
	container := "ts"
	if dec.Container == transcode.ContainerFMP4 {
		container = "fmp4"
	}
	tl.setHint(slateProfileFromInfo(c.ID, &output, container))
	tl.expectStart(2, relayTargetFloor(cut))
	tl.startMedia = 2 * cut
	p := &relayProducer{s: s, tl: tl, log: log, caps: caps, info: info, dec: dec, cut: cut}
	p.bind(ss)
	// The timeline reads only these flags under its lock; input health is
	// sampled by the producer loop, so a busy tap never delays publication.
	tl.producing = p.producing.Load
	tl.stalled = func() bool { return p.starving.Load() || p.inputStalled.Load() }
	p.ingest = newHLSIngest(tl, nil, log, 0, 2)
	p.ingest.live = true
	p.ingest.accept = p.accept
	p.ingest.split = p.split
	return p
}

// runRelay starts publishing; the lease's release stops the producer.
func (s *Service) runRelay(ss *session, p *relayProducer) *ownedLease {
	base := ss.ctx
	if base == nil {
		base = s.serviceContext()
	}
	runCtx, stop := context.WithCancel(base)
	go p.tl.run(runCtx)
	go p.run(runCtx)
	return &ownedLease{tl: p.tl, relay: p, release: func() {
		stop()
		p.close()
	}}
}

// start launches a relay session for the session's current input and makes
// it the producer, retiring the previous one. newFormat joins the input at
// the oldest buffered keyframe of its current format (the old format must
// not be read again after a codec change, and the new one is in hand at
// once).
func (p *relayProducer) start(ctx context.Context, info core.MediaInfo, dec core.Decision, cut time.Duration, newFormat bool) error {
	id, hls, cancel, err := p.launch(ctx, info, dec, cut, newFormat)
	if err != nil {
		return err
	}
	return p.adopt(id, hls, cancel, info, dec, cut)
}

// launch starts a relay session on the session's current input without
// making it the producer.
func (p *relayProducer) launch(ctx context.Context, info core.MediaInfo, dec core.Decision, cut time.Duration, newFormat bool) (string, core.HLSSession, context.CancelFunc, error) {
	ss := p.sess.Load()
	input, err := p.s.relayInput(ss)
	if newFormat {
		input, err = p.s.relayInputFormat(ss)
	}
	if err != nil {
		return "", nil, nil, err
	}
	p.mu.Lock()
	p.gen++
	id := "livetv-" + ss.info.ID
	if p.gen > 1 {
		id = fmt.Sprintf("livetv-%s-%d", ss.info.ID, p.gen)
	}
	p.mu.Unlock()
	// An HLS input on a reserve-trimmed grant starts at the reserve depth:
	// the relay races through that backlog and the timeline holds it.
	backlog := info.Container == "hls" && !newFormat && p.s.stabilityReserve(ss.info.Channel) > 0
	hls, cancel, err := p.s.startOwnedHLS(ctx, ss, core.HLSRequest{SessionID: id, InputURL: input, LiveInput: true, LiveFromBacklog: backlog, Info: info, Decision: dec, SegmentDuration: cut})
	return id, hls, cancel, err
}

// adopt makes a started relay session the producer. Its first segment is
// spliced in with a discontinuity unless it is the first producer.
func (p *relayProducer) adopt(id string, hls core.HLSSession, cancel context.CancelFunc, info core.MediaInfo, dec core.Decision, cut time.Duration) error {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		cancel()
		hls.Close()
		return core.ErrNotFound
	}
	old, oldCancel := p.hls, p.cancel
	steady := p.steady
	p.steady = nil
	p.hls, p.cancel, p.info, p.dec, p.cut = hls, cancel, info, dec, cut
	p.starved, p.reformat = false, nil
	p.startedAt = time.Now()
	origin := &localRelayOrigin{hls: hls, name: id}
	if old == nil && steady == nil {
		p.ingest.origin = origin
	} else {
		p.ingest.rebase(origin)
		p.tl.splice(true)
	}
	p.mu.Unlock()
	if old != nil {
		oldCancel()
		old.Close()
	}
	if steady != nil {
		steady.Close()
	}
	p.log.Info("owned relay producer started", "producer", id, "video_copy", dec.VideoCopy, "audio_copy", dec.AudioCopy, "container", dec.Container, "video_codec", dec.VideoCodec)
	return nil
}

func (p *relayProducer) close() {
	p.noteRelayEnd()
	p.mu.Lock()
	p.closed = true
	hls, cancel := p.hls, p.cancel
	p.hls, p.cancel = nil, nil
	hedge, overlap := p.hedge, p.overlap
	p.hedge, p.overlap = nil, nil
	steady, ready := p.steady, p.steadyReady
	p.steady, p.steadyReady = nil, nil
	p.mu.Unlock()
	hedge.close()
	overlap.close()
	for _, f := range []SteadyFeed{steady, ready} {
		if f != nil {
			f.Close()
		}
	}
	if cancel != nil {
		cancel()
	}
	if hls != nil {
		hls.Close()
	}
}

// split cuts a segment that would round above the playlist's fixed target
// at its inner keyframes. A reconnect splice leaves irregular keyframe
// spacing, so a copy cut can land up to a GOP late.
func (p *relayProducer) split(seg *ownedSegment) []*ownedSegment {
	limit := p.tl.targetDuration()
	if limit <= 0 {
		limit = relayTargetFloor(p.cut)
	}
	if seg.ext != "ts" || seg.dur < limit+500*time.Millisecond {
		return []*ownedSegment{seg}
	}
	parts := splitTS(seg.data, limit)
	if len(parts) < 2 {
		return []*ownedSegment{seg}
	}
	out := make([]*ownedSegment, 0, len(parts))
	for i, data := range parts {
		part := *seg
		part.data = data
		part.disc = seg.disc && i == 0
		part.dur = seg.dur / time.Duration(len(parts))
		if span, ok := tsVideoSpan(data); ok {
			part.dur = span
		}
		out = append(out, &part)
	}
	p.log.Info("owned relay split an overlong segment at its keyframes", "duration", seg.dur, "parts", len(out), "target", limit)
	return out
}

// accept vets a relay segment before it airs. A copy segment past the
// target means a keyframe went missing: one modestly long segment (up to
// twice the target) airs, as a transient loss heals at the next keyframe; a
// grossly long one, which an Apple player cannot take, is dropped, and so is
// a second long one within a minute. Either switches the producer to a
// re-encode, which cuts on its own keyframes.
func (p *relayProducer) accept(seg *ownedSegment) bool {
	if seg.format.mislabeled {
		// A copy remux declares the codec it started with; after a source
		// codec change its PMT lies about the payload. Drop it and reopen.
		p.mu.Lock()
		f := seg.format
		p.reformat = &f
		p.mu.Unlock()
		p.log.Warn("owned relay output no longer matches its declared codecs", "payload", seg.format.String())
		return false
	}
	p.mu.Lock()
	copyMode := p.dec.VideoCopy && !p.escalated
	p.mu.Unlock()
	if copyMode && seg.ext == "ts" && !p.damaged.Load() && damageCheckable(seg.format) && p.s.segmentDamaged(seg.data) {
		// Copy hands damaged pictures straight to the player, which drops
		// them (seconds of frozen picture). Keep this one off the
		// playlist; the producer switches to a re-encode that conceals.
		p.damaged.Store(true)
		p.log.Warn("owned relay copy segment carries input damage", "duration", seg.dur)
		return false
	}
	td := p.tl.targetDuration()
	if td <= 0 || seg.dur <= td+td/4 {
		return true
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.dec.VideoCopy {
		return true
	}
	now := time.Now()
	repeat := now.Sub(p.longAt) < time.Minute
	p.longAt = now
	drop := seg.dur > 2*td || repeat
	if drop {
		p.starved = true
	}
	p.log.Warn("owned relay segment exceeds the target duration", "duration", seg.dur, "target", td, "dropped", drop)
	return !drop
}

func (p *relayProducer) run(ctx context.Context) {
	warmed := false
	failures := 0
	var flowing time.Time // since when provider data has arrived without a pause
	for sleepCtx(ctx, 100*time.Millisecond) {
		p.mu.Lock()
		in := p.ingest
		startedAt := p.startedAt
		p.mu.Unlock()
		progress, err := in.step(ctx)
		if ctx.Err() != nil {
			return
		}
		now := time.Now()
		if progress {
			failures = 0
			p.mu.Lock()
			p.progress = now
			p.mu.Unlock()
			if !warmed && p.tl.started() {
				warmed = true
				p.tl.warmSlate()
			}
		}
		if err != nil {
			failures++
		}
		if p.takeSteady() {
			failures = 0
			continue
		}
		p.mu.Lock()
		onSteady := p.steady != nil
		p.mu.Unlock()
		if onSteady {
			// Steady owns recovery now (its ladder and reconnect screen);
			// the deadline and slate still guard this timeline. Only a
			// feed that ended outright brings the relay back.
			if errors.Is(err, errOriginEnded) || failures >= 15 {
				p.recover(ctx, "steady feed ended")
			}
			continue
		}
		if runs := p.tl.slateRunCount(); runs > p.slateRuns {
			p.slateRuns = runs
			if p.churn(now) {
				p.wantSteady(ctx, "stall")
			}
		}
		h, healthy := p.s.inputHealth(p.sess.Load())
		p.inputStalled.Store(healthy && (h.Stalled || h.Failed || h.Kind == "ts" && h.DataAge > time.Second))
		if healthy && h.FormatGeneration != 0 {
			switch {
			case p.formatGen == 0:
				p.formatGen = h.FormatGeneration
			case h.FormatGeneration != p.formatGen:
				p.formatGen = h.FormatGeneration
				// Off the loop: this producer keeps ingesting meanwhile.
				go p.startOverlap(ctx)
			}
		}
		p.expireOverlap()
		if healthy && !h.Stalled && !h.Failed && h.DataAge < 2*time.Second {
			if flowing.IsZero() {
				flowing = now
			}
		} else {
			flowing = time.Time{}
		}
		p.mu.Lock()
		last, starved, escalated, copyMode := p.progress, p.starved, p.escalated, p.dec.VideoCopy
		reformat, reopened := p.reformat, p.reopened
		p.mu.Unlock()
		td := p.tl.targetDuration()
		if td <= 0 {
			td = relayTargetFloor(p.cut)
		}
		// A new producer gets ffmpeg's start-up time before the deadline
		// applies to it; afterwards the deadline is one target duration.
		limit, from, cold := td, last, last.Before(startedAt)
		if cold {
			from, limit = startedAt, 12*time.Second
		}
		since := now.Sub(from)
		// Keyframe starvation: input has kept arriving for this long
		// without a copy segment being cut. A cold start gets the tune
		// budget; a running stream a second past the target (sized from
		// the longest segment seen, so a healthy GOP never reaches it,
		// and measured from when input resumed, so a source returning
		// from a stall first gets to its next keyframe).
		starvedFor := time.Duration(0)
		if !flowing.IsZero() {
			starvedFor = now.Sub(later(from, flowing))
		}
		// Before the first publication no cadence is known: the target
		// stands in for the last segment.
		lastDur := p.tl.lastSegmentDuration()
		if lastDur <= 0 {
			lastDur = td
		}
		starveLimit := lastDur + time.Second
		if cold {
			// The hedge normally settles a slow cold start first.
			starveLimit = 6 * time.Second
		}
		starving := copyMode && !cold && !escalated && starvedFor > lastDur+500*time.Millisecond
		p.starving.Store(starving)
		p.producing.Store(!cold && !flowing.IsZero() && !starving)
		if h := p.hedgeState(); h != nil {
			switch {
			case !cold || escalated:
				p.dropHedge()
			case hedgeCut(ctx, h):
				p.adoptHedge(h)
				continue
			}
		} else if copyMode && cold && !escalated && !flowing.IsZero() && now.Sub(later(startedAt, flowing)) > 2*time.Second {
			p.startHedge(ctx)
		}
		ended := errors.Is(err, errOriginEnded) || failures >= 15
		switch {
		case reformat != nil && now.Sub(reopened) > 10*time.Second:
			p.reopen(ctx, *reformat)
			if p.churn(now) {
				p.wantSteady(ctx, "format")
			}
		case ended:
			p.recover(ctx, "relay ended")
		case healthy && h.Failed && !cold:
			// The tap gave up on this source: fail over now, not after
			// the deadline (a healthy backup should show within ~3 s).
			p.recover(ctx, "input failed")
		case healthy && h.Stalled && !cold && since > p.tl.lastSegmentDuration() && p.tl.depth() < td && p.hasBackup():
			// The tap's same-source reconnects already run; with a backup
			// at hand the next rung starts now, while the client still
			// plays its buffer, not after the deadline.
			p.recover(ctx, "input stalled, backup available")
		case copyMode && !escalated && p.damaged.Load():
			// Re-encode at once (it conceals); Steady takes over when it
			// has capacity, sharing one encode across viewers.
			p.noteRelayEscalation("damage", 0)
			p.wantSteady(ctx, "damage")
			p.escalate(ctx, "input damage: re-encoding conceals it")
		case copyMode && !escalated && (starved || starvedFor > starveLimit):
			// Copy cannot cut where the source has no keyframe (lost,
			// or a GOP far longer than the target). Re-encode, which
			// cuts on its own keyframes.
			p.noteRelayEscalation("keyframe", max(starvedFor, lastDur))
			p.wantSteady(ctx, "keyframe")
			p.escalate(ctx, "copy cannot cut segments from this source")
		case since > limit:
			switch {
			case healthy && h.Failed:
				p.recover(ctx, "input failed")
			case p.tl.depth() >= td:
				// The reserve still covers the client; ffmpeg's own input
				// retries (and the tap's reconnects) get that long.
			case healthy && h.Kind == "ts" && !h.Failed:
				// The tap is reconnecting the same source underneath this
				// ffmpeg and splices it back in (no EOF, continuous
				// timestamps). Re-granting would close the tap mid-reconnect
				// and throw its reserve away; slate covers the gap, and only
				// InputFailed (or a backup above) moves on.
			case since > limit+2*td:
				// The tap reconnects the same source while slate covers;
				// fall through when that takes too long.
				p.recover(ctx, "no segment")
			}
		}
	}
}

// reencodeDecision is the current source converted: video re-encoded (cut
// on its own keyframes), in the producer's container.
func (p *relayProducer) reencodeDecision() (core.Decision, bool) {
	p.mu.Lock()
	info, dec := p.info, p.dec
	p.mu.Unlock()
	next := p.s.transcoder().Decide(info, p.caps, core.DecideOptions{DirectStream: false, AudioStream: -1, SubtitleStream: -1})
	if next.VideoCopy {
		return next, false
	}
	if next.Container != transcode.ContainerFMP4 && next.Container != transcode.ContainerMPEGTS {
		next.Container = dec.Container
	}
	return next, true
}

type relayHedge struct {
	id     string
	hls    core.HLSSession
	cancel context.CancelFunc
	dec    core.Decision
	info   core.MediaInfo
	at     time.Time
}

func (h *relayHedge) close() {
	if h != nil {
		h.cancel()
		h.hls.Close()
	}
}

// startHedge starts a re-encode beside a copy producer that has not cut
// its first segment: a source GOP longer than the tune budget would
// otherwise hold the first picture back until its second keyframe.
func (p *relayProducer) startHedge(ctx context.Context) {
	next, ok := p.reencodeDecision()
	if !ok {
		return
	}
	p.mu.Lock()
	info := p.info
	p.mu.Unlock()
	sctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	id, hls, hcancel, err := p.launch(sctx, info, next, relaySegment, false)
	if err != nil {
		p.log.Warn("owned relay could not hedge a slow copy start", Diagnose("relay_hedge", err).LogAttrs()...)
		return
	}
	p.mu.Lock()
	if p.closed || p.hedge != nil {
		p.mu.Unlock()
		hcancel()
		hls.Close()
		return
	}
	p.hedge = &relayHedge{id: id, hls: hls, cancel: hcancel, dec: next}
	p.mu.Unlock()
	p.log.Info("owned relay hedging a slow copy start with a re-encode", "producer", id)
}

func (p *relayProducer) hedgeState() *relayHedge {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.hedge
}

func (p *relayProducer) dropHedge() {
	p.mu.Lock()
	h := p.hedge
	p.hedge = nil
	p.mu.Unlock()
	h.close()
}

// adoptHedge makes a hedge that cut first the producer.
func (p *relayProducer) adoptHedge(h *relayHedge) {
	p.mu.Lock()
	if p.hedge != h {
		p.mu.Unlock()
		return
	}
	p.hedge = nil
	p.escalated = true
	info := p.info
	p.mu.Unlock()
	p.noteRelayEscalation("keyframe", 0)
	p.log.Warn("owned relay switching producer to re-encode", "reason", "the re-encode cut first")
	if err := p.adopt(h.id, h.hls, h.cancel, info, h.dec, relaySegment); err != nil {
		h.close()
	}
}

func hedgeCut(ctx context.Context, h *relayHedge) bool {
	body, _, err := (&localRelayOrigin{hls: h.hls}).media(ctx)
	if err != nil {
		return false
	}
	m, ok := parseMediaPlaylist(body, nil)
	return ok && len(m.segments) > 0
}

// reopen restarts the producer on the same input, from its new format,
// after the source changed codecs, deciding again for the new audio.
func (p *relayProducer) reopen(ctx context.Context, f mediaFormat) {
	p.mu.Lock()
	p.reopened = time.Now()
	cut := p.cut
	overlap := p.overlap
	p.overlap = nil
	p.mu.Unlock()
	info, dec := p.reopenPlan(f)
	if overlap == nil {
		// No program change at the tap: a codec switch only this relay
		// saw, which the source's history must still count.
		ss := p.sess.Load()
		ss.mu.Lock()
		c, src := ss.info.Channel, ss.inputSource
		ss.mu.Unlock()
		p.s.noteSourceEvidence(c.ID, src, SourceEvidence{FormatChanges: 1})
	}
	match := overlap != nil && overlap.dec.VideoCopy == dec.VideoCopy && overlap.dec.AudioCopy == dec.AudioCopy && (dec.AudioCopy || overlap.dec.AudioCodec == dec.AudioCodec)
	p.log.Warn("owned relay reopening for the source's new format", "format", f.String(), "audio_copy", dec.AudioCopy, "audio_codec", dec.AudioCodec, "overlapped", match)
	if overlap != nil {
		if match {
			// Started when the program changed: its first segment is
			// already cut, ready at the splice.
			if err := p.adopt(overlap.id, overlap.hls, overlap.cancel, info, overlap.dec, cut); err != nil {
				overlap.close()
			}
			return
		}
		overlap.close()
	}
	sctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if err := p.start(sctx, info, dec, cut, true); err != nil {
		p.log.Warn("owned relay reopen failed", Diagnose("relay_reopen", err).LogAttrs()...)
		p.recover(ctx, "reopen failed")
	}
}

// reopenPlan is the producer's input and decision for the source's new
// format.
func (p *relayProducer) reopenPlan(f mediaFormat) (core.MediaInfo, core.Decision) {
	p.mu.Lock()
	info, prev := p.info, p.dec
	prevAudio := prev.AudioCodec
	if prev.AudioCopy {
		prevAudio = ""
		for _, a := range info.Streams {
			if a.Type == "audio" {
				prevAudio = a.Codec
				break
			}
		}
	}
	p.mu.Unlock()
	info.Streams = append([]core.StreamInfo(nil), info.Streams...)
	if len(f.audio) > 0 {
		for i := range info.Streams {
			if info.Streams[i].Type == "audio" {
				a := f.audio[0]
				info.Streams[i].Codec, info.Streams[i].CodecString, info.Streams[i].Profile = a.codec, "", ""
				info.Streams[i].Channels, info.Streams[i].SampleRate, info.Streams[i].ChannelLayout = a.channels, a.rate, ""
				break
			}
		}
	}
	dec, _, _ := p.s.relayPlan(p.sess.Load(), info)
	stereo := len(f.audio) > 0 && f.audio[0].channels > 0 && f.audio[0].channels <= 2
	surround := p.caps.SupportsAudio("ac3") || p.caps.SupportsAudio("eac3")
	if !prev.VideoCopy && prevAudio == "aac" && dec.AudioCopy && len(f.audio) > 0 && f.audio[0].codec != "aac" && (stereo || !surround) {
		// A converting relay already re-encodes its picture: keep its
		// sound AAC too, so the client meets a discontinuity but no
		// codec change (an Apple player's AAC->AC-3 renderer switch alone
		// freezes about a second). Only when AAC keeps every channel:
		// transcode's AAC is stereo, so a surround source the client can
		// play stays surround through the ordinary decision.
		caps := p.caps
		caps.AudioCodecs = []string{"aac"}
		next := p.s.transcoder().Decide(info, caps, core.DecideOptions{DirectStream: false, AudioStream: -1, SubtitleStream: -1})
		if !next.AudioCopy && !next.VideoCopy {
			next.Container = prev.Container
			dec = next
		}
	}
	return info, dec
}

// startOverlap starts a producer on the input's new program as soon as the
// tap sees it change. It probes the new program first and decides as a
// reopen would (a stereo source on a converting relay keeps AAC; copy and
// a surround source the client plays keep the source audio), so when the old
// producer's first mislabeled segment arrives, the replacement's first
// segment is already cut.
func (p *relayProducer) startOverlap(ctx context.Context) {
	p.mu.Lock()
	cut, busy, began := p.cut, p.overlap != nil || p.closed, time.Now()
	p.mu.Unlock()
	if busy {
		return
	}
	var f mediaFormat
	if input, err := p.s.relayInputFormat(p.sess.Load()); err == nil {
		f = sniffInputFormat(ctx, input)
	}
	info, dec := p.reopenPlan(f)
	sctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	id, hls, hcancel, err := p.launch(sctx, info, dec, cut, true)
	if err != nil {
		p.log.Warn("owned relay could not overlap a program change", Diagnose("relay_overlap", err).LogAttrs()...)
		return
	}
	p.mu.Lock()
	if p.closed || p.overlap != nil || p.reopened.After(began) {
		// Closed, raced, or the reopen already happened without it.
		p.mu.Unlock()
		hcancel()
		hls.Close()
		return
	}
	p.overlap = &relayHedge{id: id, hls: hls, cancel: hcancel, dec: dec, info: info, at: time.Now()}
	p.mu.Unlock()
	p.log.Info("owned relay started a producer on the input's new program", "producer", id, "format", f.String(), "audio_copy", dec.AudioCopy, "audio_codec", dec.AudioCodec)
}

// sniffInputFormat reads the start of the input's newest program (the tap
// holds it from its first keyframe, so it arrives at once) and describes its
// streams; an unknown format when that takes longer than a second.
func sniffInputFormat(ctx context.Context, input string) mediaFormat {
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, input, nil)
	if err != nil {
		return mediaFormat{}
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return mediaFormat{}
	}
	defer res.Body.Close()
	buf := make([]byte, 0, 512<<10)
	chunk := make([]byte, 64<<10)
	for len(buf) < cap(buf) {
		n, err := res.Body.Read(chunk)
		buf = append(buf, chunk[:n]...)
		if f := inspectTS(buf[:len(buf)/tsPacket*tsPacket]); f.known && len(f.audio) > 0 && f.audio[0].channels > 0 {
			return f
		}
		if err != nil {
			break
		}
	}
	return inspectTS(buf[:len(buf)/tsPacket*tsPacket])
}

// expireOverlap retires an overlap that no codec change followed (a program
// change the old producer copes with, such as new PIDs).
func (p *relayProducer) expireOverlap() {
	p.mu.Lock()
	o := p.overlap
	if o == nil || time.Since(o.at) < 10*time.Second || p.reformat != nil {
		p.mu.Unlock()
		return
	}
	p.overlap = nil
	p.mu.Unlock()
	o.close()
}

// damageCheckable limits the per-segment decode check to pictures it can
// decode well inside a segment's time on one thread.
func damageCheckable(f mediaFormat) bool {
	switch f.video {
	case "h264", "mpeg2video":
		return f.width <= 1920 && f.height <= 1088
	case "hevc":
		return f.width > 0 && f.width <= 1920 && f.height <= 1088
	}
	return false
}

// segmentDamaged decodes one segment (one thread, errors only, bounded) and
// reports decoder errors: corrupt or missing transport packets that a copy
// relay would otherwise pass to the player. A check that cannot finish in
// time reports no damage.
func (s *Service) segmentDamaged(data []byte) bool {
	ffmpeg := s.ffmpegPath()
	if ffmpeg == "" {
		return false
	}
	ctx, cancel := context.WithTimeout(s.serviceContext(), 700*time.Millisecond)
	defer cancel()
	cmd := exec.CommandContext(ctx, ffmpeg, "-hide_banner", "-v", "error", "-threads", "1", "-f", "mpegts", "-i", "pipe:0", "-f", "null", "-")
	cmd.Stdin = bytes.NewReader(data)
	var errs lineCounter
	cmd.Stderr = &errs
	if err := cmd.Run(); err != nil && ctx.Err() != nil {
		return false
	}
	return errs.n >= 2
}

type lineCounter struct{ n int }

func (c *lineCounter) Write(p []byte) (int, error) {
	c.n += bytes.Count(p, []byte{'\n'})
	return len(p), nil
}

func (p *relayProducer) hasBackup() bool {
	ss := p.sess.Load()
	ss.mu.Lock()
	c, cur := ss.info.Channel, ss.inputSource.ID
	ss.mu.Unlock()
	return len(p.s.orderedSourceCandidates(c, cur)) > 0
}

// escalate replaces a copy producer with a re-encode of the same input.
func (p *relayProducer) escalate(ctx context.Context, why string) {
	p.mu.Lock()
	if p.escalated {
		p.mu.Unlock()
		return
	}
	p.escalated = true
	info := p.info
	p.mu.Unlock()
	p.dropHedge()
	next, ok := p.reencodeDecision()
	if !ok {
		return
	}
	p.log.Warn("owned relay switching producer to re-encode", "reason", why)
	sctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if err := p.start(sctx, info, next, relaySegment, false); err != nil {
		p.log.Warn("owned relay re-encode producer failed", Diagnose("relay_escalate", err).LogAttrs()...)
		p.recover(ctx, "re-encode start failed")
	}
}

// recover restarts the relay on the next source in the channel's health
// order, or on the same one when it is the only source.
func (p *relayProducer) recover(ctx context.Context, why string) {
	ss := p.sess.Load()
	ss.mu.Lock()
	c, cur := ss.info.Channel, ss.inputSource
	ss.mu.Unlock()
	candidates := append(p.s.orderedSourceCandidates(c, cur.ID), cur)
	p.log.Warn("owned relay recovering", "reason", why, "input", cur.ID, "candidates", len(candidates))
	for _, src := range candidates {
		if ctx.Err() != nil {
			return
		}
		if err := p.switchSource(ctx, ss, c, src, src.ID != cur.ID); err != nil {
			if !errors.Is(err, context.Canceled) {
				p.s.recordSourceHealth(c.ID, src, err, false)
			}
			p.log.Warn("owned relay source unavailable", append(Diagnose("relay_failover", err).LogAttrs(), "input", src.ID)...)
			continue
		}
		if src.ID != cur.ID {
			p.s.recordSourceHealth(c.ID, cur, errOriginStalled, false)
			p.log.Warn("owned relay switched source", "from", cur.ID, "to", src.ID)
		}
		return
	}
	// Nothing opened: slate continues; the next deadline tries again.
	p.mu.Lock()
	p.startedAt = time.Now()
	p.mu.Unlock()
}

func (p *relayProducer) switchSource(ctx context.Context, ss *session, c Channel, src StreamSource, probe bool) error {
	sctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	raw, headers, err := p.s.ResolveStreamSource(sctx, c, src)
	if err != nil {
		return err
	}
	grantSrc := src
	grantSrc.URL = raw
	if err := p.s.grantDirectInput(ss, grantSrc, headers); err != nil {
		return err
	}
	ss.mu.Lock()
	ss.inputURL, ss.inputSource, ss.inputHeaders = raw, src, headers.Clone()
	ss.info.ActiveSourceID = src.ID
	ss.mu.Unlock()
	p.mu.Lock()
	info, dec, cut := p.info, p.dec, p.cut
	p.mu.Unlock()
	if probe {
		probed, err := p.s.probeSessionInput(sctx, ss)
		if err != nil {
			return err
		}
		if probed == nil || probed.Video() == nil {
			return &TuneError{Code: "no_video", Stage: "probe", cause: core.ErrNotAvailable}
		}
		info = *probed
		info.Duration = 0
		dec, cut, _ = p.s.relayPlan(ss, info)
	}
	return p.start(sctx, info, dec, cut, false)
}

// grantDirectInput grants a Direct session's input. The tune's sniff and
// probe and any relay started later all read through this one grant, and it
// is reserve-aware from the start: a relay joins its input as deep as the
// channel's stability reserve allows (an HLS provider's trimmed backlog, or
// what a TS tap holds, such as a connect burst) without a second provider
// connection. The lead covers the first owned playlist's window, so the
// reserve is still whole once that window is out. With no reserve (Fastest)
// it is the plain live-edge grant.
func (s *Service) grantDirectInput(ss *session, src StreamSource, headers http.Header) error {
	return s.grantSteadyInputLead(ss, src, headers, 2*relaySegment)
}
