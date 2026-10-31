package transcode

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

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
	collect := func(exited bool) {
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
			if r.dash != nil {
				_ = os.RemoveAll(r.dash.dir)
			}
			if r.chunks != nil {
				_ = os.RemoveAll(r.chunks.dir)
			}
			return
		case <-t.C:
			collect(false)
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
	for _, n := range p.segments {
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
	for len(s.initFiles) <= t {
		s.initFiles = append(s.initFiles, "")
	}
	s.initFiles[t] = rel
	if t == 0 {
		s.initFile = rel
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
	// A run that reached its plan's mode boundary (fast-start ramp ->
	// steady, copy -> clamp, ...) stops there and the next mode's run
	// starts immediately, so the switch segment has no startup gap.
	if s.cur == r && r.planEnd > 0 && r.maxDone >= r.planEnd-1 && (s.total == 0 || r.planEnd < s.total) && s.endSeg < 0 {
		next := r.planEnd
		s.log.Debug("plan mode boundary; chaining run", "segment", next, "fromMode", r.mode)
		if err := s.startRunLocked(next, false); err != nil {
			s.log.Warn("chain run at mode boundary", "segment", next, "err", err)
		}
	}
	s.throttleLocked()
	s.broadcastLocked()
}

// finish records a run's exit and reacts to failures: a hardware encoder
// that dies before its first segment is retried on CPU, any other start
// failure is retried once with a larger probe, and a mid-stream failure is
// reported to the failure handler (playback fails over to another release).
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
	log := s.log.With("run", r.seq)
	if err == nil {
		if r.rtx && s.cur == r {
			s.decision.Enhancement.State = "stopped"
			s.decision.Enhancement.Reason = "enhancement run completed"
		}
		// A run bounded by a plan mode boundary that produced up to it
		// has not hit the end of the input: chain the next mode's run
		// when the player is in (or near) this run's range.
		if r.planEnd > 0 && r.produced > 0 && r.maxDone >= r.planEnd-1 && (s.total == 0 || r.planEnd < s.total) && s.cur == r {
			if s.lastReq >= r.startSeg {
				log.Debug("ffmpeg finished at plan boundary; chaining", "segment", r.planEnd)
				if err := s.startRunLocked(r.planEnd, false); err != nil {
					s.failErr = err
				}
				return
			}
		}
		// ffmpeg reached the end of the input.
		if r.produced > 0 {
			s.endSeg = r.maxDone
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
	if r.rtx && r.produced == 0 && !s.inputFailed {
		s.fallbackRTXLocked("native VSR run failed before producing segments", r.startSeg)
		if err := s.startRunLocked(r.startSeg, r.spec.BigProbe); err != nil {
			s.failErr = err
		}
		return
	}
	if r.rtx {
		s.decision.Enhancement.State = "fallback"
		s.decision.Enhancement.Reason = "native VSR input/process failed"
	}
	if r.produced == 0 {
		if r.hw && !s.forceCPU && looksLikeEncoderFailure(stderr, r.spec) {
			s.forceCPU = true
			log.Warn("hardware encoder failed; falling back to CPU", "hwaccel", r.spec.Accel.Name)
			if err := s.startRunLocked(r.startSeg, r.spec.BigProbe); err != nil {
				s.failErr = err
			}
			return
		}
		s.failures++
		s.failErr = fmt.Errorf("ffmpeg: %w: %s", err, tail(stderr, 300))
		if s.failures == 1 {
			if err := s.startRunLocked(r.startSeg, true); err != nil {
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
