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
			if err := s.startRunLocked(at, false); err != nil {
				s.log.Warn("throttle: restart failed", "err", err)
			}
		}
		return
	}
	ahead := r.maxDone - s.lastReq
	if r.produced == 0 {
		ahead = 0
	}
	switch {
	case !r.paused && ahead > limit:
		if !s.noPause {
			err := r.proc.Pause()
			if err == nil {
				r.paused = true
				s.log.Debug("throttle: paused ffmpeg", "ahead", ahead)
				return
			}
			if !errors.Is(err, ErrPauseUnsupported) {
				return
			}
			s.noPause = true
		}
		resume := r.maxDone + 1
		s.stopRunLocked()
		s.resumeAt = resume
		s.log.Debug("throttle: stopped ffmpeg", "ahead", ahead, "resumeAt", resume)
	case r.paused && ahead <= limit/2:
		if err := r.proc.Resume(); err == nil {
			r.paused = false
			s.log.Debug("throttle: resumed ffmpeg", "ahead", ahead)
		}
	}
}

// aheadLimit is how many segments ffmpeg may run ahead of the player.
func (s *Session) aheadLimit() int {
	limit := int(s.m.opts.maxAhead / s.segDur)
	if limit < 4 {
		limit = 4
	}
	return limit
}

// pruneLocked deletes finished segments well behind the play position (and,
// when throttling bounds the encoder, stale ones far ahead of it) to bound
// disk use.
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
func (s *Session) startRunLocked(seg int, bigProbe bool) error {
	if s.closed {
		return errors.New("transcode: session closed")
	}
	if s.inputFailed {
		return errInputExhausted
	}
	s.stopRunLocked()
	// Reserve this transition before dropping the lock. A later seek or swap
	// supersedes it; only the newest transition may start another encoder.
	s.runSeq++
	sequence := s.runSeq
	for s.reaping != nil {
		reaped := s.reaping
		s.mu.Unlock()
		<-reaped
		s.mu.Lock()
		if s.closed {
			return errors.New("transcode: session closed")
		}
		if sequence != s.runSeq {
			return nil
		}
		if s.reaping == reaped {
			s.reaping = nil
		}
	}
	s.resumeAt = -1
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
		Features:     s.m.feat,
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
	if err := os.MkdirAll(spec.Dir, 0o755); err != nil {
		return fmt.Errorf("transcode: run dir: %w", err)
	}
	var dr *dashRun
	if s.isDASH() {
		spec.Dir = filepath.Join(s.dir, fmt.Sprintf("run-%d", s.runSeq))
		spec.Playlist = "manifest.mpd"
		spec.ChunkDur = dashChunkDuration
		if spec.ChunkDur > s.segDur/2 {
			spec.ChunkDur = s.segDur / 2
		}
		if err := os.MkdirAll(spec.Dir, 0o755); err != nil {
			return fmt.Errorf("transcode: run dir: %w", err)
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
		if err := os.MkdirAll(spec.ChunkDir, 0o755); err != nil {
			return fmt.Errorf("transcode: chunk dir: %w", err)
		}
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
	if hint := s.m.opts.onRunStart; hint != nil {
		// The first segment needs its whole span, and a GOP either side.
		go hint(spec.Input, spec.Start, spec.Start+2*s.segDur)
	}
	if !spec.Decision.VideoCopy && !s.hevcFallbackWarned {
		// Caller holds s.mu (startRunLocked), guarding hevcFallbackWarned.
		s.hevcFallbackWarned = warnVideoEncoderFallback(s.log, spec.Decision.VideoCodec, spec.Accel, spec.Features)
	}
	args := buildArgs(spec)
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
		l = redactRTXLine(l, spec.Input, spec.InputHeaders)
		if strings.Contains(strings.ToLower(l), "resize: ngx-vsr ") {
			r.rtxConfirmed.Store(true)
		}
		r.stderr.add(l)
		runLog.Debug("ffmpeg", "line", l)
	}}
	var proc Process
	var err error
	if enhancementSelected(spec.Decision) {
		proc, args, err = s.startRTXRunLocked(spec, stdout, stderr)
		if err != nil {
			s.fallbackRTXLocked(err.Error(), seg)
			spec.Decision = s.trackDecisionLocked(s.wantTrack)
			r.spec = spec
			args = buildArgs(spec)
		} else {
			r.rtx = true
			r.hw = true
		}
	}
	if proc == nil {
		proc, err = s.m.runner.Start(Command{Path: s.m.ffmpeg, Args: args, Dir: s.dir, Stdout: stdout, Stderr: stderr})
	}
	if err != nil {
		if dr != nil {
			_ = os.RemoveAll(dr.dir)
		}
		if cr != nil {
			_ = os.RemoveAll(cr.dir)
		}
		return fmt.Errorf("transcode: start ffmpeg: %w", err)
	}
	r.proc = proc
	s.cur = r
	if s.isDASH() {
		// A prior seek may have ended on an empty trailing audio/video
		// fragment. Its EOF is not a bound on another rendition: the new
		// run must finish and publish its own rounded-up final alias.
		s.endSeg = -1
	}
	s.deliveredCount = 0
	runLog.Info("ffmpeg started", "segment", seg, "at", spec.Start, "kind", s.decision.Kind,
		"videoCopy", s.decision.VideoCopy, "audioCopy", s.decision.AudioCopy, "hw", r.hw)
	runLog.Debug("ffmpeg args", "args", strings.Join(redactedRTXArgs(args), " "))
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
	r := s.cur
	if r == nil {
		return
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
		s.watchers.Add(1)
		go func() {
			defer s.watchers.Done()
			defer close(reaped)
			r.proc.Stop()
			if previous != nil {
				<-previous
			}
		}()
	}
}
