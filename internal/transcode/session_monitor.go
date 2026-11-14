package transcode

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

var missingFilterRE = regexp.MustCompile(`(?i)(?:no such filter:\s*['"]?([a-z0-9_]+)|filter ['"]([a-z0-9_]+)['"] not found)`)

// watch polls a run's output for finished segments until ffmpeg exits.
func (s *Session) watch(r *run) {
	defer func() {
		s.mu.Lock()
		unusedStop := r.stopWork
		r.stopWork = nil
		s.mu.Unlock()
		unusedStop.finish()
		r.watchWork.finish()
	}()
	defer s.watchers.Done()
	t := time.NewTicker(s.m.opts.pollInterval)
	defer t.Stop()
	playlist := filepath.Join(r.spec.Dir, r.spec.Playlist)
	var watchdog runWatchdog
	collect := func(exited bool) {
		s.collectTextSubtitles(r)
		switch {
		case r.dash != nil:
			s.collectDASH(r, exited)
		case r.chunks != nil:
			s.collectChunks(r, exited)
		default:
			s.poll(r, playlist, exited)
		}
	}
	for {
		select {
		case <-r.proc.Done():
			collect(true)
			s.finish(r)
			s.mu.Lock()
			obsolete := s.cur != r
			s.mu.Unlock()
			if obsolete {
				for _, path := range r.spec.TextSubtitlePaths {
					s.removals.remove(path)
				}
			}
			if obsolete && r.spec.SoftSubtitlePath != "" {
				s.removals.remove(r.spec.SoftSubtitlePath)
			}
			if r.dash != nil {
				_ = os.RemoveAll(r.dash.dir)
			}
			if r.chunks != nil {
				_ = os.RemoveAll(r.chunks.dir)
			}
			return
		case <-t.C:
			collect(false)
			watchdog.check(s, r)
			if s.traceOn() {
				s.mu.Lock()
				s.traceBufferLocked(r)
				s.mu.Unlock()
			}
			s.removals.retry()
		}
	}
}

func (s *Session) poll(r *run, playlist string, exited bool) {
	p, err := readFFmpegPlaylist(playlist, exited)
	if err != nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	if r.stopped && s.cur != r {
		return // superseded; its segments may already be discarded
	}
	if r.edits != nil && r.edits.failed {
		return
	}
	changed := false
	t := r.track
	if t < 0 || t >= len(s.done) {
		t = 0
	}
	for i, n := range p.segments {
		if n < r.startSeg || (n <= r.maxDone && r.produced > 0) {
			continue
		}
		if s.total > 0 && n >= s.total+2 {
			continue // past any plausible trailing sliver
		}
		if r.planEnd > 0 && n >= r.planEnd {
			// Produced past this run's mode boundary before we could stop
			// it: drop the segment, the next run redoes it in its mode.
			_ = os.Remove(s.trackSegPath(t, n))
			continue
		}
		// ffmpeg can advertise a zero-byte HLS segment after seeking beyond
		// a truncated input, yet exit successfully. Never publish it as media:
		// treat the run as failed so playback can retry or select another source.
		path := s.trackSegPath(t, n)
		if st, err := os.Stat(path); err == nil && st.Size() == 0 {
			_ = os.Remove(path)
			s.failRunOutputLocked(r, fmt.Errorf("transcode: segment %d is empty", n))
			break
		}
		if s.heldToClock(r) {
			path := s.trackSegPath(t, n)
			err := s.moveOntoRetainedLocked(r, t, path)
			if err == nil {
				err = s.noteInitLocked(r, t)
			}
			if err != nil {
				_ = os.Remove(path)
				s.failRunOutputLocked(r, fmt.Errorf("transcode: segment %d on the retained clock: %w", n, err))
				break
			}
		} else {
			_ = s.noteInitLocked(r, t)
		}
		if r.produced == 0 {
			s.log.Info("first segment ready", "run", r.seq, "segment", n, "after", time.Since(r.started))
		}
		r.produced++
		r.maxDone = n
		s.done[t][n] = true
		if s.liveInput && t == 0 && i < len(p.entries) {
			s.noteLiveDurationLocked(n, p.entries[i].dur)
		}
		if t == 0 {
			s.noteSegSizeLocked(n, s.trackSegPath(t, n))
		}
		changed = true
	}
	if changed {
		s.progressedLocked(r)
	}
}

// noteInitLocked publishes run r's init segment (fMP4 init or Matroska
// header) as track t's once ffmpeg has written it.
// An fMP4 init off the track's retained clock is an error: neither it nor
// the run's media may be published.
func (s *Session) noteInitLocked(r *run, t int) error {
	if !hasInitSegment(s.container) {
		return nil
	}
	if _, err := os.Stat(filepath.Join(r.spec.Dir, r.spec.InitName)); err != nil {
		return nil
	}
	rel, err := filepath.Rel(s.dir, filepath.Join(r.spec.Dir, r.spec.InitName))
	if err != nil {
		return nil
	}
	if err := s.holdInitEditsLocked(r, t); err != nil {
		return err
	}
	if r.edits != nil && (len(r.edits.absent) > 0 || r.edits.keepInit) {
		return nil // partial track init cannot replace the retained init
	}
	for len(s.initFiles) <= t {
		s.initFiles = append(s.initFiles, "")
	}
	s.initFiles[t] = rel
	if t == 0 {
		s.initFile = rel
		if s.clockInput == "" {
			s.clockInput = r.spec.Input
		}
	}
	if t == 0 && s.container == ContainerFMP4 && !s.fmp4TrackKnown {
		if b, err := os.ReadFile(filepath.Join(s.dir, rel)); err == nil {
			if tracks, err := parseInitTracks(b); err == nil {
				v := trackInit{}
				for _, tr := range tracks {
					if tr.Handler == "vide" {
						v = tr
						break
					}
					if v.TrackID == 0 {
						v = tr
					}
				}
				if v.TrackID != 0 && v.Timescale != 0 {
					s.fmp4VideoTrack = emptyFMP4Tracks{TrackID: v.TrackID, Timescale: v.Timescale}
					s.fmp4TrackKnown = true
				}
			}
		}
	}
	return nil
}

// noteSegSizeLocked records a primary-track segment's size (governor).
func (s *Session) noteSegSizeLocked(n int, path string) {
	if s.segSizes == nil {
		s.segSizes = map[int]int64{}
	}
	if st, err := os.Stat(path); err == nil {
		s.segSizes[n] = st.Size()
	}
}

// progressedLocked reacts to run r finishing segments: failure counters,
// the governor, chaining at a plan mode boundary, throttling, waiters.
func (s *Session) progressedLocked(r *run) {
	if r.produced > 0 && s.cur == r {
		if r.rtx && r.rtxConfirmed.Load() {
			s.decision.Enhancement.State = "running"
			s.decision.Enhancement.Reason = "NVEncC NGX VSR produced enhanced segments"
		}
		s.failures = 0
		s.failErr = nil
	}
	s.maybeGovernLocked()
	s.chainOrHoldLocked(r)
	s.throttleLocked()

	s.broadcastLocked()
}

// chainOrHoldLocked starts the next plan run when this one has reached its
// mode boundary and the client is close enough to need it. A forced-key
// cap (maxForcedKeySegments) is the same kind of boundary: reaching it
// while the player is still minutes behind must not start that far run or
// the encoder head becomes the only window left on disk.
// traceBufferLocked also runs while no segment completes, exposing stalls and pauses.
func (s *Session) traceBufferLocked(r *run) {
	if s.traceOn() && s.cur == r && time.Since(r.traceBufferAt) >= time.Second {
		r.traceBufferAt = time.Now()
		r.progressMu.Lock()
		p := r.progress
		r.progressMu.Unlock()
		readyLead := time.Duration(0)
		if r.produced > 0 {
			readyLead = max(0, s.segStart(r.maxDone+1)-s.segStart(s.lastReq))
		}
		s.log.Debug("encoder buffer trace", "run", r.seq, "track", r.track, "playheadSegment", s.lastReq,
			"maxDone", r.maxDone, "readyLeadSeconds", readyLead.Seconds(), "paused", r.paused,
			"wallSeconds", time.Since(r.started).Seconds(), "runStartSeconds", r.spec.Start.Seconds(),
			"outTimeSeconds", p.OutTime.Seconds(), "frame", p.Frame, "speed", p.Speed, "fps", p.FPS)
	}
}

func (s *Session) chainOrHoldLocked(r *run) {
	if !s.boundaryReadyLocked(r) {
		return
	}
	next := r.planEnd
	if !s.clientNearLocked(next) {
		s.log.Info("transcode run reached its segment cap ahead of the player", "segment", next, "playhead", s.lastReq)
		s.stopRunForLocked("segment cap ahead of player")
		s.resumeAt = next
		return
	}
	s.log.Debug("plan mode boundary; chaining run", "segment", next, "fromMode", r.mode, "playhead", s.lastReq)
	if err := s.startRunForLocked(next, false, "plan boundary"); err != nil {
		s.log.Warn("chain run at mode boundary", "segment", next, "err", err)
	}
}

// boundaryReadyLocked reports that run r filled up to its plan boundary
// and another run is what produces the following segment.
// emptyRestartCap is how many consecutive ffmpeg starts that publish
// nothing are allowed before the session stops looping. The first is
// the GOP-boundary hole (seek landed on the previous keyframe); the
// second is a real short file or a dead source window.
const emptyRestartCap = 2

func (s *Session) handleEmptyRestartLocked(r *run) {
	log := s.log.With("run", r.seq)
	s.emptyRuns++
	s.emptyAt = r.startSeg
	next := r.startSeg + 1
	if s.emptyRuns >= emptyRestartCap {
		if s.lastReal >= 0 {
			s.endSeg = s.lastReal
		} else {
			s.endSeg = r.startSeg - 1
		}
		log.Info("ffmpeg finished", "lastSegment", s.endSeg, "emptyRestarts", s.emptyRuns, "reason", "empty restart cap: run produced no new segment")
		return
	}
	aliased := s.publishBoundaryAliasLocked(r.startSeg)
	if aliased {
		log.Info("transcode restart produced no new segment; serving previous GOP and advancing", "segment", r.startSeg, "next", next, "emptyRestarts", s.emptyRuns)
	} else {
		log.Info("transcode restart produced no new segment; advancing", "segment", r.startSeg, "next", next, "emptyRestarts", s.emptyRuns)
	}
	if s.total > 0 && next >= s.total {
		if s.lastReal >= 0 {
			s.endSeg = s.lastReal
		} else {
			s.endSeg = r.startSeg - 1
		}
		log.Info("ffmpeg finished", "lastSegment", s.endSeg)
		return
	}
	if s.clientNearLocked(next) {
		if err := s.startRunForLocked(next, false, "empty restart advance"); err != nil {
			s.failErr = err
		}
		return
	}
	s.resumeAt = next
}

func (s *Session) publishBoundaryAliasLocked(n int) bool {
	t := 0
	if t >= len(s.done) || n <= 0 || !s.done[t][n-1] {
		return false
	}
	if s.done[t][n] {
		return true
	}
	dst := s.trackSegPath(t, n)
	if s.container == ContainerMatroska {
		if os.WriteFile(dst, s.emptyFragmentLocked(n), 0o644) != nil {
			return false
		}
	} else if s.container == ContainerFMP4 && !s.isDASH() {
		if os.WriteFile(dst, s.emptyFragmentLocked(n), 0o644) != nil {
			return false
		}
	} else if linkOrCopy(s.trackSegPath(t, n-1), dst) != nil {
		return false
	}
	s.done[t][n] = true
	return true
}

func (s *Session) boundaryReadyLocked(r *run) bool {
	return s.cur == r && r.planEnd > 0 && r.maxDone >= r.planEnd-1 && (s.total == 0 || r.planEnd < s.total) && s.endSeg < 0
}

// clientNearLocked reports that the player is close enough to seg that the
// next run has to be producing it. With throttling disabled the encoder is
// allowed to run the whole title, and the boundary chains as before.
func (s *Session) clientNearLocked(seg int) bool {
	if s.m == nil || s.m.opts.maxAhead <= 0 || s.segDur <= 0 {
		return true
	}
	return seg-s.lastReq <= s.aheadLimit()
}

// finish records a run's exit and reacts to failures: a hardware encoder
// that fails is retried on CPU at the first unpublished segment, any other
// start failure is retried once with a larger probe, and other mid-stream
// failures are reported to the failure handler.
func (s *Session) finish(r *run) {
	err := redactScopedCommandError(r.proc.Err(), r.spec)
	s.mu.Lock()
	defer s.mu.Unlock()
	r.finished = true
	r.err = err
	r.paused = false
	defer s.broadcastLocked()
	if s.closed || r.stopped {
		return
	}
	if s.traceOn() {
		why := "eof"
		if err != nil {
			why = "error"
		}
		s.log.Info("ffmpeg run", "event", "stop", "reason", why, "run", r.seq, "segment", r.startSeg, "maxDone", r.maxDone, "planEnd", r.planEnd, "playhead", s.lastReq, "err", err)
	}
	if err != nil && s.cur == r && s.retryQSVLocked(r) {
		return
	}
	log := s.log.With("run", r.seq)
	if s.liveInput && s.cur == r {
		if err != nil && s.retryMissingLiveAudioLocked(r) {
			return
		}
		if err != nil && r.hw && !s.forceCPU && looksLikeEncoderFailure(r.stderr.String(), r.spec) {
			s.forceCPU = true
			log.Warn("live hardware encoder failed; falling back to CPU", "hwaccel", r.spec.Accel.Name, "segment", r.next())
		}
		s.recoverLiveLocked(r, err)
		return
	}
	if err == nil {
		if r.rtx && s.cur == r {
			s.decision.Enhancement.State = "stopped"
			s.decision.Enhancement.Reason = "enhancement run completed"
		}
		// A run bounded by a plan mode boundary that produced up to it
		// has not hit the end of the input: chain the next mode's run
		// when the player is in (or near) this run's range.
		if s.boundaryReadyLocked(r) && r.produced > 0 {
			if s.clientNearLocked(r.planEnd) {
				// Chain even when the player has not fetched this run yet.
				// A governor clamp (or any mode boundary) starts the run
				// ahead of the playhead; declaring EOF there drops the rest
				// of the title.
				log.Debug("ffmpeg finished at plan boundary; chaining", "segment", r.planEnd, "playhead", s.lastReq)
				if err := s.startRunForLocked(r.planEnd, false, "plan boundary exit"); err != nil {
					s.failErr = err
				}
				return
			}
			// The process exited at the cap. This is not the end of the
			// input; resume there once the player arrives.
			s.resumeAt = r.planEnd
			log.Info("transcode run reached its segment cap ahead of the player", "segment", r.planEnd, "playhead", s.lastReq)
			return
		}
		// ffmpeg reached the end of this run's input. That is not the
		// end of the title when the playlist still lists later
		// segments: a remux of a cached debrid source often EOFs after
		// one buffered window (lab: 620–692 of 730). Declaring endSeg
		// there 404s the next sequential fetch and the player sits on
		// "seeking". Hold a resume, or chain if the playhead is already
		// there. A run that produced nothing at n is a GOP-boundary
		// hole or a real short file: alias n and advance once, then
		// cap (lab: 143 empty restarts at 695 → 502).
		if r.produced > 0 {
			s.emptyRuns = 0
			s.emptyAt = -1
			s.lastReal = r.maxDone
			if s.total > 0 && r.maxDone < s.total-1 {
				next := r.maxDone + 1
				if s.clientNearLocked(next) {
					log.Info("transcode run ended before the title; continuing", "segment", next, "playhead", s.lastReq, "total", s.total)
					if err := s.startRunForLocked(next, false, "title continues"); err != nil {
						s.failErr = err
					}
					return
				}
				s.resumeAt = next
				log.Info("transcode run ended before the title", "lastSegment", r.maxDone, "resumeAt", next, "playhead", s.lastReq, "total", s.total)
				return
			}
			s.endSeg = r.maxDone
		} else if s.total > 0 && r.startSeg > 0 && r.startSeg < s.total {
			s.handleEmptyRestartLocked(r)
			return
		} else {
			s.endSeg = r.startSeg - 1
		}
		log.Info("ffmpeg finished", "lastSegment", s.endSeg)
		return
	}
	stderr := r.stderr.String()
	log.Warn("ffmpeg failed", "err", err, "segments", r.produced, "stderr", tail(stderr, 1500))
	if s.cur != r {
		return
	}
	// Normalization changes the output clock. Retry only before an init has
	// been published in this scope; an established player clock must not move.
	if errors.Is(err, errNegativeFMP4DecodeTime) && r.produced == 0 && !r.spec.NormalizeNegativeDTS {
		_, retained := s.clockLocked(r, runTrack(s, r))
		if retained == nil {
			s.normalizeNegativeDTS = true
			if retryErr := s.startRunForLocked(r.startSeg, r.spec.BigProbe, "negative fMP4 decode time"); retryErr != nil {
				s.failErr = retryErr
			}
			return
		}
	}
	if r.stalled && !s.inputFailed && s.takeStallRetry(time.Now()) {
		if startErr := s.startRunForLocked(r.next(), false, "stalled encoder recovery"); startErr == nil {
			return
		} else {
			err = startErr
		}
	}
	if r.rtx && r.produced == 0 && !s.inputFailed {
		s.fallbackRTXLocked("native VSR run failed before producing segments", r.startSeg)
		if err := s.startRunForLocked(r.startSeg, r.spec.BigProbe, "rtx fallback"); err != nil {
			s.failErr = err
		}
		return
	}
	if r.rtx {
		s.decision.Enhancement.State = "fallback"
		s.decision.Enhancement.Reason = "native VSR input/process failed"
	}
	// A DASH hardware graph can fail before writing its first fragment with
	// driver-specific text, or a missing hardware filter. Try software once
	// before filter classification can terminate the session. Preserve the
	// segment number; forceCPU also keeps seeks and later runs on software.
	if s.isDASH() && r.hw && r.produced == 0 && !s.forceCPU && !r.stalled && !s.inputFailed {
		s.forceCPU = true
		log.Warn("hardware encoder failed; falling back to CPU", "hwaccel", r.spec.Accel.Name, "segment", r.startSeg)
		if retryErr := s.startRunForLocked(r.startSeg, r.spec.BigProbe, "hardware startup fallback"); retryErr != nil {
			s.failErr = retryErr
		}
		return
	}
	if r.produced == 0 {
		if name := missingFilter(stderr); name != "" {
			if s.tryNextGraphLocked(r, name) {
				return
			}
			_ = s.reportGraphLocked(&GraphError{Message: filterUnavailableMessage(name)})
			return
		}
		if namelessFilterMiss(stderr) {
			_ = s.reportGraphLocked(&GraphError{Message: "FFmpeg rejected a filter in this graph. The same graph will not be retried."})
			return
		}
	}
	if !r.stalled && r.hw && !s.forceCPU && looksLikeEncoderFailure(stderr, r.spec) {
		s.forceCPU = true
		next := r.next()
		log.Warn("hardware encoder failed; falling back to CPU", "hwaccel", r.spec.Accel.Name, "segment", next)
		if err := s.startRunForLocked(next, r.spec.BigProbe, "hardware fallback"); err != nil {
			s.failErr = err
		}
		return
	}
	if s.recoverCopyLocked(r, stderr) {
		return
	}
	if r.produced == 0 && !r.stalled {
		s.failures++
		s.failErr = fmt.Errorf("ffmpeg: %w: %s", err, tail(stderr, 300))
		if s.failures == 1 {
			if err := s.startRunForLocked(r.startSeg, true, "start retry"); err != nil {
				s.failErr = err
			}
			return
		}
	} else {
		s.failErr = fmt.Errorf("ffmpeg: %w: %s", err, tail(stderr, 300))
	}
	if s.inputFailed {
		return
	}
	s.inputFailed = true
	if h := s.m.opts.onInputFailure; h != nil {
		at, input, ferr := s.segStart(r.next()), s.input, s.failErr
		go h(s.id, input, at, ferr)
	} else if h := s.m.opts.onFailure; h != nil {
		at := s.segStart(r.next())
		ferr := s.failErr
		go h(s.id, at, ferr)
	}
}

// recoverCopyLocked handles a fatal packet conversion in the MP4-to-Annex-B
// copy path. A decoder can conceal a damaged NAL that the copy bitstream
// filter rejects. TS carries replacement parameter sets in-band; static
// headers and HDR/DV contracts cannot safely make this transition here.
func (s *Session) recoverCopyLocked(r *run, stderr string) bool {
	v := s.info.Video()
	if s.copyFallback || s.inputFailed || s.liveInput || s.isDASH() ||
		s.container != ContainerMPEGTS || !r.spec.Decision.VideoCopy ||
		v == nil || v.Codec != "h264" || encodedRange(v) != rangeSDR ||
		!strings.Contains(strings.ToLower(stderr), "error applying bitstream filters") {
		return false
	}
	next := r.next()
	s.copyFallback = true
	d := s.decision
	d.Kind, d.VideoCopy, d.VideoCodec = core.DecisionTranscode, false, "h264"
	if d.BitrateKbps <= 0 {
		d.BitrateKbps = s.info.BitrateKbps
	}
	d.Reason += "; copied video packet conversion failed; encoding video"
	s.decision = d
	if len(s.rungs) > 0 {
		s.rungs[0].decision = d
	}
	s.copyDiscontinuities = map[int]bool{next: true}
	s.playlist = nil
	s.log.Warn("copy packet conversion failed; encoding video", "segment", next)
	if err := s.startRunForLocked(next, r.spec.BigProbe, "copy packet recovery"); err != nil {
		s.failErr = err
	}
	return true
}

// restartBudget bounds attempts in a sliding window even if every attempt
// produces some output. It can also be used for other encoder continuations.
type restartBudget []time.Time

func (b *restartBudget) take(now time.Time, limit int, window time.Duration) bool {
	kept := (*b)[:0]
	for _, at := range *b {
		if now.Sub(at) < window {
			kept = append(kept, at)
		}
	}
	*b = kept
	if len(kept) >= limit {
		return false
	}
	*b = append(kept, now)
	return true
}

// continueAfterExitLocked resumes at the first segment the run did not
// publish. Callers own the timeline and decide whether a discontinuity is
// required before invoking this shared encoder transition.
func (s *Session) continueAfterExitLocked(r *run, reason string) error {
	return s.startRunForLocked(r.next(), false, reason)
}

// recoverLiveLocked reconnects an active live HLS session at the next
// unpublished segment. Delivered media replenishes the consecutive-failure
// allowance; a separate sliding ceiling still bounds rapid reconnects.
func (s *Session) recoverLiveLocked(r *run, err error) {
	now := time.Now()
	next := r.next()
	reason := "EOF"
	if err != nil {
		reason = "encoder exit: " + err.Error()
	}
	stderr := strings.ToLower(r.stderr.String())
	if strings.Contains(stderr, "corrupt") || strings.Contains(stderr, "pes packet size mismatch") {
		reason += " (corrupt input observed)"
	}
	idle := s.m.opts.idleTimeout > 0 && now.Sub(s.LastAccess()) >= s.m.opts.idleTimeout
	delay, allowed := s.liveRecoveryDelay(now, r)
	if idle || !allowed {
		endReason := "restart limit reached"
		if idle {
			endReason = "session idle"
		}
		s.endSeg = next - 1
		s.inputFailed = true
		s.failErr = fmt.Errorf("live source ended: %s: %s", endReason, reason)
		s.log.Warn("live encoder recovery ended", "reason", endReason, "exit", reason, "restarts", len(s.liveRestarts), "lastSegment", s.endSeg)
		return
	}
	s.liveRecovering = true
	timer := time.NewTimer(delay)
	cancel := s.encoder.cancel
	s.mu.Unlock()
	select {
	case <-timer.C:
	case <-cancel:
	}
	timer.Stop()
	s.mu.Lock()
	if s.cur != r {
		return
	}
	s.liveRecovering = false
	if s.closed {
		return
	}
	if next > 0 {
		if s.liveDiscontinuities == nil {
			s.liveDiscontinuities = make(map[int]bool)
		}
		s.liveDiscontinuities[next] = true
	}
	s.log.Warn("restarting live encoder", "reason", reason, "segment", next, "restart", len(s.liveRestarts))
	if startErr := s.continueAfterExitLocked(r, "live input recovery"); startErr != nil {
		s.endSeg = next - 1
		s.inputFailed = true
		s.failErr = startErr
		s.log.Warn("live encoder recovery failed", "err", startErr)
	}
}

// looksLikeEncoderFailure tells a hardware encoder/filter failure apart
// from an input failure, so only the former switches the session to CPU.
func looksLikeEncoderFailure(stderr string, spec runSpec) bool {
	if strings.TrimSpace(stderr) == "" {
		return true
	}
	l := strings.ToLower(stderr)
	enc, _ := videoEncoder(spec.Decision.VideoCodec, spec.Accel, spec.Features)
	for _, k := range []string{strings.ToLower(enc), "encoder", "hwaccel", "hwupload", "hw_frames", "hardware",
		"device", "vaapi", "cuda", "nvenc", "qsv", "videotoolbox", "scale_vt", "tonemap_vaapi", "impossible to convert",
		"function not implemented", "filter", "pixel format"} {
		if strings.Contains(l, k) {
			return true
		}
	}
	return false
}

func missingFilter(stderr string) string {
	m := missingFilterRE.FindStringSubmatch(stderr)
	if len(m) == 0 {
		return ""
	}
	if m[1] != "" {
		return m[1]
	}
	if len(m) > 2 {
		return m[2]
	}
	return ""
}

func namelessFilterMiss(stderr string) bool {
	l := strings.ToLower(stderr)
	return strings.Contains(l, "filter not found") || strings.Contains(l, "no such filter")
}

// tryNextGraphLocked drops a filter ffmpeg says is missing and starts the
// next graph when that graph is different. It reports a terminal graph
// error itself when nothing else can play the video.
func (s *Session) tryNextGraphLocked(r *run, name string) bool {
	if s.deniedFilters == nil {
		s.deniedFilters = map[string]bool{}
	}
	if s.deniedFilters[name] || s.filterFallbacks >= 4 {
		return false
	}
	s.deniedFilters[name] = true
	s.filterFallbacks++
	next := r.spec
	next.Features = s.featuresLocked()
	if err := graphFailure(next); err != nil {
		_ = s.reportGraphLocked(err)
		return true
	}
	if filterGraph(buildArgs(next)) == filterGraph(buildArgs(r.spec)) {
		return false
	}
	s.log.Warn("ffmpeg filter missing; trying the next graph", "filter", name, "run", r.seq)
	if err := s.startRunForLocked(r.startSeg, false, "filter fallback"); err != nil {
		s.failErr = err
	}
	return true
}

func (s *Session) reportGraphLocked(err error) error {
	if err == nil {
		return nil
	}
	s.failErr = err
	s.inputFailed = true
	s.failures = 2
	if s.m != nil {
		s.m.noteGraphError(err)
	}
	if s.graphReported || s.closed {
		return err
	}
	s.graphReported = true
	if s.log != nil {
		s.log.Warn("transcode graph rejected", "err", err)
	}
	if s.m == nil {
		return err
	}
	at := s.segStart(0)
	if s.cur != nil {
		at = s.segStart(s.cur.next())
	}
	id, input, ferr := s.id, s.input, err
	if h := s.m.opts.onInputFailure; h != nil {
		go h(id, input, at, ferr)
		return err
	}
	if h := s.m.opts.onFailure; h != nil {
		go h(id, at, ferr)
	}
	return err
}

// Two delivered segments covering at least four seconds are useful playback,
// rather than an HTTP 200 or a few corrupt packets. Keep the burst ceiling
// independent of this replenishment so progress cannot create a retry storm.
func (s *Session) liveRecoveryDelay(now time.Time, r *run) (time.Duration, bool) {
	healthy := r.produced >= 2 && s.deliveredSeg >= r.startSeg+1 && s.deliveredCount >= 2 && time.Duration(s.deliveredCount)*s.segDur >= 4*time.Second && now.Sub(r.started) >= 2*time.Second
	if healthy {
		s.liveFailures = 0
	}
	if s.liveFailures >= 7 || !s.liveRestarts.take(now, 28, 30*time.Second) {
		return 0, false
	}
	s.liveFailures++
	return 250 * time.Millisecond << min(s.liveFailures-1, 4), true
}
