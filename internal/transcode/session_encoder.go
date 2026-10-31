package transcode

import (
	"errors"
	"fmt"
	"github.com/braedonsaunders/ether/internal/core"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// throttleLocked keeps ffmpeg from running far ahead of the player, so a
// remux does not pull a whole title from Usenet up front: it suspends
// ffmpeg and resumes it as the player catches up. Where processes cannot be
// suspended (Windows) it stops ffmpeg instead and restarts it at the next
// missing segment in time.
func (s *Session) throttleLocked() {
	if s.m.opts.maxAhead <= 0 || s.info.Duration <= 0 {
		return
	}
	limit := s.aheadLimit()
	r := s.cur
	if r == nil || r.finished {
		if s.resumeAt >= 0 && s.endSeg < 0 && s.resumeAt-s.lastReq <= limit/2 && !s.closed {
			at := s.resumeAt
			s.log.Debug("throttle: restarting ffmpeg", "segment", at)
			if err := s.startRunForLocked(at, false, "throttle resume"); err != nil {
				s.log.Warn("throttle: restart failed", "err", err)
			}
		}
		return
	}
	ahead := r.maxDone - s.lastReq
	if r.produced == 0 {
		ahead = 0
	}
	// SIGSTOP on the ffmpeg pid does not always stop a VideoToolbox encode.
	// One extra segment can land after the signal; a run that keeps
	// publishing has to be stopped or it reaches the eviction window.
	if r.paused && r.maxDone > r.pausedDone+2 {
		s.stopForLeadLocked(r, ahead)
		return
	}
	switch {
	case !r.paused && ahead > limit:
		if !s.noPause {
			err := r.proc.Pause()
			if err == nil {
				r.paused = true
				r.pausedDone = r.maxDone
				s.log.Debug("throttle: paused ffmpeg", "ahead", ahead, "playhead", s.lastReq)
				return
			}
			if !errors.Is(err, ErrPauseUnsupported) {
				return
			}
			s.noPause = true
		}
		resume := r.maxDone + 1
		s.stopRunForLocked("pause unsupported")
		s.resumeAt = resume
		s.log.Debug("throttle: stopped ffmpeg", "ahead", ahead, "resumeAt", resume)
	case r.paused && ahead <= limit/2:
		if err := r.proc.Resume(); err == nil {
			r.paused = false
			s.log.Debug("throttle: resumed ffmpeg", "ahead", ahead)
		}
	}
}

// stopForLeadLocked stops a run that got ahead of the playhead and resumes
// inside the throttle window, not at the encoder head.
func (s *Session) stopForLeadLocked(r *run, ahead int) {
	resume := s.lastReq + s.aheadLimit()
	if r != nil && r.maxDone+1 < resume {
		resume = r.maxDone + 1
	}
	if resume < 0 {
		resume = 0
	}
	s.stopRunForLocked("lead cap")
	s.resumeAt = resume
	s.log.Info("transcode lead capped", "ahead", ahead, "playhead", s.lastReq, "resumeAt", resume)
}

// leadOverLocked reports that this run has published past the throttle window.
func (s *Session) leadOverLocked(r *run) bool {
	if s.m == nil || s.m.opts.maxAhead <= 0 || s.segDur <= 0 || r == nil || r.produced == 0 {
		return false
	}
	return r.maxDone-s.lastReq > s.aheadLimit()
}

// aheadLimit is how many segments ffmpeg may run ahead of the player.
func (s *Session) aheadLimit() int {
	limit := int(s.m.opts.maxAhead / s.segDur)
	if limit < 4 {
		limit = 4
	}
	return limit
}

// pruneLocked deletes finished segments well behind the playhead (and, when
// throttling bounds the encoder, stale ones far ahead of it) to bound disk
// use. The playhead is the client's oldest outstanding fetch inside the
// restart window, advanced as those segments are delivered. A prefetch of
// the encoder's newest segment does not move it, so the segment the client
// is still waiting on is not evicted to make room for the encoder head.
func (s *Session) pruneLocked() {
	if s.m.opts.keepBehind <= 0 {
		return
	}
	behind := int(s.m.opts.keepBehind / s.segDur)
	farAhead := -1
	if s.m.opts.maxAhead > 0 {
		farAhead = 2*s.aheadLimit() + 8
	}
	for t, done := range s.done {
		if s.total == 0 && !s.isDASH() {
			s.listedLocked(t) // keep pruned segments in the EVENT playlist
		}
		for n := range done {
			if n < s.lastReq-behind || (farAhead >= 0 && n > s.lastReq+farAhead) {
				if err := os.Remove(s.trackSegPath(t, n)); err == nil || errors.Is(err, os.ErrNotExist) {
					delete(done, n)
				}
			}
		}
	}
}

// startRunLocked stops the current run and starts ffmpeg at segment seg.
func (s *Session) traceOn() bool {
	return s != nil && s.log != nil && s.m != nil && s.m.opts.tracePlayback
}

// startRunForLocked starts a run and, when playback tracing is on, records why.
func (s *Session) startRunForLocked(seg int, bigProbe bool, reason string) error {
	s.traceCause = reason
	return s.startRunLocked(seg, bigProbe)
}

// stopRunForLocked stops the current run and, when playback tracing is on, records why.
func (s *Session) stopRunForLocked(reason string) {
	s.traceStop = reason
	s.stopRunLocked()
}

func (s *Session) startRunLocked(seg int, bigProbe bool) error {
	cause := s.traceCause
	s.traceCause = ""
	if s.closed {
		return errors.New("transcode: session closed")
	}
	if s.inputFailed {
		return errInputExhausted
	}
	s.encoder.decisionEncoding(!s.decision.VideoCopy)
	s.stopRunLocked()
	// This start replaces any throttle resume. Clear it before waiting on
	// the old process: a collector can take the lock during that wait and
	// would otherwise start the old lead as a second run.
	s.resumeAt = -1
	// Reserve this transition before dropping the lock. A later seek or swap
	// supersedes it; only the newest transition may start another encoder.
	s.runSeq++
	sequence := s.runSeq
	for s.reaping != nil || s.starting != nil {
		barrier := s.reaping
		if s.starting != nil {
			barrier = s.starting
		}
		var canceled <-chan struct{}
		if s.encoder != nil {
			canceled = s.encoder.cancel
		}
		s.mu.Unlock()
		select {
		case <-barrier:
		case <-canceled:
		}
		s.mu.Lock()
		if s.closed || (s.encoder != nil && s.encoder.isSealed()) {
			return errors.New("transcode: session closed")
		}
		if sequence != s.runSeq {
			return nil
		}
		if s.reaping == barrier {
			s.reaping = nil
		}
		if s.starting == barrier {
			s.starting = nil
		}
	}
	if s.total > 0 && seg >= s.total {
		seg = s.total - 1
	}
	if seg < 0 {
		seg = 0
	}
	accel := s.m.hw
	if s.forceCPU {
		accel = core.HWAccel{Name: AccelNone}
	}
	rp := s.runPlanLocked(seg)
	s.prepareRTXRunLocked(&rp, seg)
	s.seekRampSeg = -1
	spec := runSpec{
		RTXTimeline:  s.rtxTimeline,
		AudioGainDB:  s.audioMatch.gain,
		LiveInput:    s.liveInput,
		InputHeaders: s.inputHeaders,
		Input:        s.input,
		Info:         s.info,
		Decision:     rp.decision,
		Start:        s.segStart(seg),
		SegDur:       rp.hlsDur,
		StartSeg:     seg,
		Dir:          s.runDirLocked(),
		Playlist:     fmt.Sprintf("run-%d.m3u8", s.runSeq),
		InitName:     fmt.Sprintf("init-%d.mp4", s.runSeq),
		Accel:        accel,
		Features:     s.featuresLocked(),
		BigProbe:     bigProbe,
		Protocol:     s.proto,
		FastStart:    rp.fastStart,
		Rate:         rp.rate,
	}
	if s.container == ContainerMatroska {
		spec.InitName = fmt.Sprintf("header-%d.mkv", s.runSeq)
	}
	if s.contract != nil && !rp.decision.VideoCopy {
		spec.Target = s.contract
		spec.PictureCrop = s.inputCrop
	}
	var dr *dashRun
	if s.isDASH() {
		spec.Dir = filepath.Join(s.dir, fmt.Sprintf("run-%d", s.runSeq))
		spec.Playlist = "manifest.mpd"
		spec.ChunkDur = dashChunkDuration
		if spec.ChunkDur > s.segDur/2 {
			spec.ChunkDur = s.segDur / 2
		}
		// The muxer produces video (the wanted rung) then audio; map
		// those to their session tracks.
		var mapping []int
		if s.info.Video() != nil {
			mapping = append(mapping, s.wantTrack)
		}
		if findStream(s.info, spec.Decision.AudioStream, "audio") != nil {
			at := s.audioTrackLocked()
			if at < 0 {
				at = len(mapping)
			}
			mapping = append(mapping, at)
		}
		dr = newDASHRun(spec.Dir, mapping, s.base, s.segDur, s.total, seg)
	}
	var cr *chunkRun
	if s.chunkedLocked(spec.Decision) {
		spec.ChunkDir = filepath.Join(spec.Dir, fmt.Sprintf("c-%d", s.runSeq))
		spec.ChunkCut = chunkCutFor(s.segDur)
		if !spec.Decision.VideoCopy {
			// Force keyframes at the plan's boundaries (through the one
			// the run stops at), in the run's output clock.
			end := rp.planEnd
			if end < 0 || end > s.total {
				end = s.total
			}
			if end-seg > maxForcedKeySegments {
				end = seg + maxForcedKeySegments
				rp.planEnd = end
			}
			origin := time.Duration(0)
			if len(inputTimestampArgs(spec)) == 0 {
				origin = spec.Start // output timestamps count from -ss
			}
			for k := seg + 1; k <= end && k < s.total; k++ {
				spec.KeyTimes = append(spec.KeyTimes, s.segStart(k)-origin)
			}
		}
		sl := newSlotter(s.base, s.segDur, s.total, seg)
		plan, base := s.plan, s.base // boundaries never change after StartHLS
		// Chunk times are title time. The Matroska muxer adds -ss plus the
		// 10s origin, and mkvChunkStart removes only the origin, so the
		// playlist index matches -ss and -segment_start_number. A run-relative
		// clock would pile every chunk onto the run's first segment.
		sl.index = func(t time.Duration) int { return plan.IndexAt(t - base) }
		cr = &chunkRun{dir: spec.ChunkDir, container: s.container,
			initPath: filepath.Join(spec.Dir, spec.InitName), startAt: spec.Start, sl: sl}
	}
	r := &run{
		chunks:   cr,
		seq:      s.runSeq,
		startSeg: seg,
		spec:     spec,
		hw:       spec.usesHW(),
		maxDone:  -1,
		stderr:   newTailBuffer(40),
		started:  time.Now(),
		dash:     dr,
		mode:     rp.mode,
		planEnd:  rp.planEnd,
		track:    s.wantTrack,
	}
	if err := graphFailure(spec); err != nil {
		return s.reportGraphLocked(err)
	}
	if !spec.Decision.VideoCopy && !s.hevcFallbackWarned {
		// Caller holds s.mu (startRunLocked), guarding hevcFallbackWarned.
		s.hevcFallbackWarned = warnVideoEncoderFallback(s.log, spec.Decision.VideoCodec, spec.Accel, spec.Features)
	}
	var args []string
	runLog := s.log.With("run", r.seq)
	var pp progressParser
	lastLog := time.Time{}
	stdout := &lineWriter{fn: func(l string) {
		p, ok := pp.line(l)
		if !ok {
			return
		}
		// Never take s.mu here: Stop holds it while waiting for ffmpeg to
		// exit, and exit waits for this pipe to drain.
		r.progressMu.Lock()
		r.progress = p
		r.progressMu.Unlock()
		if time.Since(lastLog) > 15*time.Second {
			lastLog = time.Now()
			runLog.Debug("ffmpeg progress", "outTime", p.OutTime, "speed", p.Speed, "fps", p.FPS)
		}
	}}
	stderr := &lineWriter{fn: func(l string) {
		l = redactRTXLine(l, spec.Input, spec.commandHeaders())
		if strings.Contains(strings.ToLower(l), "resize: ngx-vsr ") {
			r.rtxConfirmed.Store(true)
		}
		r.stderr.add(l)
		runLog.Debug("ffmpeg", "line", l)
	}}
	work, err := s.encoder.runWork(!s.decision.VideoCopy || rp.mode != ModeCopy || s.wantTrack != 0)
	if err != nil {
		return err
	}
	r.watchWork, r.stopWork = work[0], work[1]
	var hintWork *encoderWork
	if s.m.opts.onRunStart != nil {
		reserved, reserveErr := s.encoder.reserve(1)
		if reserveErr != nil {
			r.watchWork.finish()
			r.stopWork.finish()
			return reserveErr
		}
		hintWork = reserved[0]
	}
	starting := make(chan struct{})
	s.starting = starting
	rtxConfig, runDir, inputLease := s.rtxConfig, s.dir, s.inputLease
	hint := s.m.opts.onRunStart
	hintEnd := spec.Start + 2*s.segDur
	s.mu.Unlock()
	var proc Process
	spec.InputScopeHeaders, err = encoderScopeHeaders(inputLease, spec.Input)
	if err == nil {
		args = buildArgs(spec)
		r.spec = spec
		err = os.MkdirAll(spec.Dir, 0o755)
	}
	if err == nil && spec.ChunkDir != "" {
		err = os.MkdirAll(spec.ChunkDir, 0o755)
	}
	if err == nil && hint != nil {
		go func() {
			defer hintWork.finish()
			hint(spec.Input, spec.Start, hintEnd)
		}()
	} else {
		hintWork.finish()
	}
	if err == nil && enhancementSelected(spec.Decision) {
		proc, args, err = s.m.startRTXRun(spec, rtxConfig, runDir, stdout, stderr)
		if err != nil && proc == nil {
			s.mu.Lock()
			if !s.closed && sequence == s.runSeq && (s.encoder == nil || !s.encoder.isSealed()) {
				s.fallbackRTXLocked(err.Error(), seg)
				spec.Decision = s.trackDecisionLocked(s.wantTrack)
				r.spec = spec
				args = buildArgs(spec)
				err = nil
			}
			s.mu.Unlock()
		} else if err == nil {
			r.rtx, r.hw = true, true
		}
	}
	if err == nil && proc == nil {
		proc, err = s.m.runner.Start(Command{Path: s.m.ffmpeg, Args: args, Dir: runDir, Stdout: stdout, Stderr: stderr})
	}
	err = redactScopedCommandError(err, spec)
	s.mu.Lock()
	if s.starting == starting {
		s.starting = nil
	}
	close(starting)
	stale := s.closed || sequence != s.runSeq || (s.encoder != nil && s.encoder.isSealed())
	if proc != nil {
		s.encoder.markStarted()
	}
	if err != nil || stale || proc == nil {
		if proc != nil {
			s.reapUnpublishedLocked(r, proc)
		} else {
			r.watchWork.finish()
			r.stopWork.finish()
		}
		if err != nil {
			if dr != nil {
				_ = os.RemoveAll(dr.dir)
			}
			if cr != nil {
				_ = os.RemoveAll(cr.dir)
			}
			s.noteEncoder(false, args)
			return fmt.Errorf("transcode: start ffmpeg: %w", err)
		}
		// A newer seek or swap owns the session. Match the wait above: this
		// start is not a failure, and the caller keeps serving the new run.
		if sequence != s.runSeq && !s.closed && (s.encoder == nil || !s.encoder.isSealed()) {
			return nil
		}
		return errors.New("transcode: start superseded or returned no process")
	}
	if r.rtx {
		s.decision.Enhancement = spec.Decision.Enhancement
		s.decision.Enhancement.State = "starting"
		s.decision.Enhancement.Reason = "waiting for enhanced segments from NVEncC/NGX"
	}
	r.proc = proc
	s.cur = r
	// A previous run's EOF does not bound the replacement. The copy of a
	// short title can finish before a governor clamp restarts at an earlier
	// segment, and a DASH seek can end on an empty trailing fragment. The
	// new run publishes its own end.
	s.endSeg = -1
	s.deliveredCount = 0
	runLog.Info("ffmpeg started", "segment", seg, "at", spec.Start, "kind", s.decision.Kind,
		"videoCopy", s.decision.VideoCopy, "audioCopy", s.decision.AudioCopy, "hw", r.hw)
	if s.traceOn() {
		if cause == "" {
			cause = "unspecified"
		}
		runLog.Info("ffmpeg run", "event", "start", "reason", cause, "segment", seg, "playhead", s.lastReq, "oldest", s.oldestFetchLocked())
	}
	runLog.Debug("ffmpeg args", "args", strings.Join(redactedRTXArgs(args), " "))
	s.noteEncoder(true, args)
	s.watchers.Add(1)
	go s.watch(r)
	s.broadcastLocked()
	return nil
}

// chunkedLocked reports whether an HLS run producing decision d must cut
// at every keyframe into chunks the session regroups onto the plan
// (chunks.go), because ffmpeg's segmenters cannot be told where to cut:
//   - the plan's boundaries are source keyframes (encodes force keyframes
//     at them);
//   - a copy without a keyframe index: wherever a GOP is longer than a
//     segment, ffmpeg would make fewer segments than the playlist lists.
//     Regrouped, every segment starts on a keyframe (or, inside a GOP
//     longer than a segment, repeats the GOP covering it).
//
// Matroska encodes also use the collector: after a copy-to-encode swap,
// the previous published GOP may extend beyond the nominal boundary.
// The collector removes that already-held media before publishing.
// Other encodes on nominal plans cut at uniform boundaries directly.
func (s *Session) chunkedLocked(d core.Decision) bool {
	if s.isDASH() || s.plan == nil || s.info.Video() == nil {
		return false
	}
	return s.plan.Keyframed() || d.VideoCopy || s.container == ContainerMatroska
}

// stopRunLocked detaches the current run immediately. Process reaping happens
// outside the session lock and is included in Close's watcher barrier.
func (s *Session) stopRunLocked() {
	reason := s.traceStop
	s.traceStop = ""
	r := s.cur
	if r == nil {
		return
	}
	if s.traceOn() && !r.finished {
		if reason == "" {
			reason = "superseded"
		}
		s.log.Info("ffmpeg run", "event", "stop", "reason", reason, "run", r.seq, "segment", r.startSeg, "maxDone", r.maxDone, "playhead", s.lastReq, "oldest", s.oldestFetchLocked())
	}
	s.cur = nil
	if r.rtx && enhancementSelected(s.decision) {
		s.decision.Enhancement.State = "stopped"
		s.decision.Enhancement.Reason = "enhancement run stopped"
	}
	if !r.finished {
		r.stopped = true
		reaped := make(chan struct{})
		previous := s.reaping
		s.reaping = reaped
		stopWork := r.stopWork
		r.stopWork = nil
		s.watchers.Add(1)
		go func() {
			defer stopWork.finish()
			defer s.watchers.Done()
			defer close(reaped)
			r.proc.Stop()
			if stopWork != nil {
				<-r.proc.Done()
			}
			if previous != nil {
				<-previous
			}
		}()
	}
}

// A process returned together with an error, or after seal, still owns both
// pre-reserved tickets. It never publishes segments or starts a fallback.
func (s *Session) reapUnpublishedLocked(r *run, proc Process) {
	reaped := make(chan struct{})
	previous := s.reaping
	s.reaping = reaped
	s.watchers.Add(2)
	go func() {
		defer r.watchWork.finish()
		defer s.watchers.Done()
		<-proc.Done()
	}()
	go func() {
		defer r.stopWork.finish()
		defer s.watchers.Done()
		defer close(reaped)
		proc.Stop()
		<-proc.Done()
		if previous != nil {
			<-previous
		}
	}()
}

func (s *Session) featuresLocked() features {
	if s == nil || s.m == nil {
		return features{}
	}
	f := s.m.feat
	if len(s.deniedFilters) == 0 {
		return f
	}
	cloned := f
	cloned.filters = map[string]bool{}
	for name, ok := range f.filters {
		if ok && !s.deniedFilters[name] {
			cloned.filters[name] = true
		}
	}
	return cloned
}

func (s *Session) noteEncoder(started bool, args []string) {
	if s == nil || s.m == nil || s.m.opts.onEncoder == nil {
		return
	}
	s.m.opts.onEncoder(s.id, started, strings.Join(redactedRTXArgs(args), " "))
}
