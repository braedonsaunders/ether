package transcode

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

var _ ContinuousRunner = (*Manager)(nil)

type continuousEncoder struct {
	speed atomic.Uint64
	*continuousProcess
	closeOnce     sync.Once
	mu            sync.Mutex
	child         *continuousChild
	fatalErr      error
	forceCPU      bool // latched for this output after a hardware failure
	hasRTXFailure bool
	rtxFailed     continuousRTXAttemptKey
	req           EncoderRequest
	dir           string
	frames        chan RawFrame
	writeMu       chan struct{}
	tick          int64
	workers       sync.WaitGroup
	index         *continuousSegmentIndex
	stream        *continuousFanout
	store         *continuousStore
	m             *Manager
}

func (e *continuousEncoder) setChild(c *continuousChild) {
	e.mu.Lock()
	e.child = c
	if c != nil && c.proc != nil && e.continuousProcess != nil {
		e.continuousProcess.proc = c.proc.proc
		e.continuousProcess.sockets = c.proc.sockets
		e.continuousProcess.done = c.proc.done
		e.continuousProcess.stderr = c.proc.stderr
	}
	e.mu.Unlock()
}

func (e *continuousEncoder) currentChild() *continuousChild {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.child
}

func (e *continuousEncoder) fail(err error) {
	e.mu.Lock()
	if e.fatalErr == nil {
		e.fatalErr = err
	}
	e.mu.Unlock()
	e.cancel()
}

func (e *continuousEncoder) writeError() error {
	e.mu.Lock()
	err := e.fatalErr
	c := e.child
	e.mu.Unlock()
	if err != nil {
		return err
	}
	if c != nil && c.proc != nil {
		return c.proc.writeError()
	}
	return io.ErrClosedPipe
}

func (e *continuousEncoder) recoverFromEncoderFailure(child **continuousChild, tick int64) error {
	old := *child
	err := old.failure()
	if !old.rtx && (!old.hardware || e.forceCPU) {
		return err
	}
	message := "continuous hardware encoder exited; falling back to software"
	if old.rtx {
		e.recordRTXFailure(old.attemptKey)
		message = "continuous RTX Video exited; falling back to ordinary encoder"
	} else {
		e.forceCPU = true
	}
	if e.m != nil && e.m.log != nil {
		e.m.log.Warn(message,
			"session", e.req.SessionID,
			"tick", tick,
			"err", err)
	}
	old.forceClose()
	// Include every fully published pair before choosing the replacement sequence.
	e.index.refresh()

	resumeSeq := e.index.nextSeq()
	tsOffsetTick, _ := continuousSegmentFrames(e.req.Source, e.req.SegmentDuration, e.req.LiveSource, resumeSeq)
	replacement, startErr := e.startOrdinaryChild(resumeSeq, tick, tsOffsetTick)
	if startErr != nil {
		return startErr
	}
	e.setChild(replacement)
	e.index.setBandwidth(e.ordinaryBandwidthBps())
	*child = replacement
	return nil
}

// StartEncoder starts a fixed rendition. Scaling stays in ffmpeg (one scale per
// rendition); callers send the same largest working frame to every rendition.
func (m *Manager) StartEncoder(ctx context.Context, r EncoderRequest) (ContinuousEncoder, error) {
	if e := validateRaw(r.Source); e != nil {
		return nil, e
	}
	if r.SegmentDuration == 0 {
		r.SegmentDuration = 4 * time.Second
	}
	if (r.SegmentDuration != 4*time.Second && r.SegmentDuration != 2*time.Second) || r.DVRWindow < 0 {
		return nil, errors.New("transcode: invalid segment or DVR duration")
	}
	if r.Decision.VideoCodec != "" && r.Decision.VideoCodec != "h264" && r.Decision.VideoCodec != "hevc" {
		return nil, errors.New("transcode: unsupported continuous codec")
	}
	if r.Decision.AudioCodec != "" && r.Decision.AudioCodec != "aac" {
		return nil, errors.New("transcode: continuous output requires AAC")
	}
	startSchedulerWatch(m.log)
	ctx, release, e := m.AcquireTranscodeSlot(ctx, SlotViewer)
	if e != nil {
		return nil, e
	}
	ok := false
	defer func() {
		if !ok {
			release()
		}
	}()
	dir, e := os.MkdirTemp(m.tempDir, "continuous-")
	if e != nil {
		return nil, e
	}
	clean := true
	defer func() {
		if clean {
			os.RemoveAll(dir)
		}
	}()
	if r.SessionID == "" {
		r.SessionID = randomHex(12)
	}
	encCtx, encCancel := context.WithCancel(ctx)
	enc := &continuousEncoder{
		continuousProcess: &continuousProcess{ctx: encCtx, cancel: encCancel, done: make(chan struct{}), stderr: newTailBuffer(24)},
		req:               r,
		dir:               dir,
		frames:            make(chan RawFrame, 8),
		writeMu:           make(chan struct{}, 1),
		stream:            newContinuousFanout(),
		m:                 m,
	}
	enc.index = newContinuousSegmentIndex(enc)
	// Keep about 30 s of each container in memory: the live window clients
	// read, served without touching the disk.
	keep := int((30*time.Second + r.SegmentDuration - 1) / r.SegmentDuration)
	enc.store, e = newContinuousStore(dir, keep, enc.index.kick)
	if e != nil {
		encCancel()
		return nil, e
	}
	defer func() {
		if !ok {
			encCancel()
			enc.store.close()
		}
	}()

	child, e := enc.startGeneration(0, 0, 0)
	if e != nil {
		return nil, e
	}

	cleanup := sync.OnceFunc(func() {
		release()
		enc.stream.close()
		enc.store.close()
		_ = removeAllRetry(dir)
	})

	go enc.index.watch()

	g, startup, steady := continuousLadder(r.Source, r.SegmentDuration, r.LiveSource)
	enc.workers.Add(1)
	go func() {
		defer enc.workers.Done()
		defer cleanup()
		defer encCancel()
		defer func() {
			if child != nil {
				child.forceClose()
			}
		}()

		watchdog := time.NewTicker(time.Second)
		defer watchdog.Stop()
		var watched *continuousChild
		var published int64
		progressAt := time.Now()
		stalled := func(feeding bool) bool {
			next := enc.index.nextSeq()
			if watched != child || published != next || !feeding {
				watched, published, progressAt = child, next, time.Now()
				return false
			}
			return child.hardware && time.Since(progressAt) >= max(10*time.Second, 3*r.SegmentDuration)
		}
		var tick int64
		for {
			var f RawFrame
			gotFrame := false
			for !gotFrame {
				var rtxDied <-chan struct{}
				if child.rtx {
					rtxDied = child.rtxDone
				}
				select {
				case <-enc.ctx.Done():
					return
				case <-m.stop:
					return
				case <-watchdog.C:
					if stalled(len(enc.frames) > 0) {
						child.forceClose()
						if err := enc.recoverFromEncoderFailure(&child, tick); err != nil {
							enc.fail(err)
							return
						}
					}
				case <-child.proc.done:
					if enc.ctx.Err() != nil {
						return
					}
					if err := enc.recoverFromEncoderFailure(&child, tick); err != nil {
						enc.fail(err)
						return
					}
				case <-rtxDied:
					if enc.ctx.Err() != nil {
						return
					}
					if err := enc.recoverFromEncoderFailure(&child, tick); err != nil {
						enc.fail(err)
						return
					}
				case f = <-enc.frames:
					gotFrame = true
				}
			}

			if tick >= steady && (tick-steady)%g == 0 && enc.wantBoundarySwap(child) {
				seq := int64(len(startup)) + (tick-steady)/g
				child.drainInputsAndWait(enc, seq)
				next, swapErr := enc.startGeneration(seq, tick, tick)
				if swapErr != nil {
					child = nil
					enc.fail(swapErr)
					return
				}
				child = next
			}

			for {
				localTick := tick - child.startTick
				vch, ach := child.video, child.audio
				// Ten input pictures/s under pressure; fps repeats after scale/format.
				// Still send a timestamp at least every three ticks so mux progress continues.
				if !child.rtx && f.RepeatVideo && localTick%3 != 0 {
					vch = nil
				}
				recovered := false
				for vch != nil || ach != nil {
					var rtxDied <-chan struct{}
					if child.rtx {
						rtxDied = child.rtxDone
					}
					select {
					case vch <- continuousPicture{f.Video, localTick}:
						vch = nil
					case ach <- f.PCM:
						ach = nil
					case <-enc.ctx.Done():
						return
					case <-m.stop:
						return
					case <-watchdog.C:
						if stalled(true) {
							child.forceClose()
							if err := enc.recoverFromEncoderFailure(&child, tick); err != nil {
								enc.fail(err)
								return
							}
							recovered = true
							vch, ach = nil, nil
						}
					case <-child.proc.done:
						if enc.ctx.Err() != nil {
							return
						}
						if err := enc.recoverFromEncoderFailure(&child, tick); err != nil {
							enc.fail(err)
							return
						}
						recovered = true
						vch, ach = nil, nil
					case <-rtxDied:
						if enc.ctx.Err() != nil {
							return
						}
						if err := enc.recoverFromEncoderFailure(&child, tick); err != nil {
							enc.fail(err)
							return
						}
						recovered = true
						vch, ach = nil, nil
					}
				}
				if !recovered {
					break
				}
			}
			tick++
		}
	}()
	ok = true
	clean = false
	return enc, nil
}

func (m *Manager) continuousEncodeArgsAt(r EncoderRequest, s []*rawSocket, base string, startSeq, tsOffsetTick int64, accel core.HWAccel) []string {
	f := r.Source
	rate := fmt.Sprintf("%d/%d", f.FPSNum, f.FPSDen)
	args := []string{"-hide_banner", "-loglevel", "warning", "-stats_period", "1", "-progress", "pipe:2", "-nostdin", "-y", "-filter_threads", "1"}
	enc, _ := videoEncoder(r.Decision.VideoCodec, accel, m.feat)
	if strings.HasSuffix(enc, "_qsv") && accel.Device != "" {
		args = append(args, "-qsv_device", accel.Device)
	}
	for i := range 2 {
		args = append(args, "-thread_queue_size", "8", "-probesize", "32", "-analyzeduration", "0")
		if i == 0 {
			args = append(args, "-f", "matroska")
		} else {
			args = append(args, "-f", "s16le", "-ar", "48000", "-ac", "2")
		}
		args = append(args, "-i", s[i].url())
	}
	codec := r.Decision.VideoCodec
	if codec == "" {
		codec = "h264"
	}
	w, h := outputSize(f.Width, f.Height, r.Decision.MaxHeight)
	vf := fmt.Sprintf("scale=%d:%d,setsar=1,format=yuv420p", w, h)
	if strings.HasSuffix(enc, "_videotoolbox") || strings.HasSuffix(enc, "_qsv") {
		vf = fmt.Sprintf("scale=%d:%d,setsar=1,format=nv12", w, h)
	}
	vf += ",fps=" + rate
	if strings.HasSuffix(enc, "_vaapi") {
		args = append(args, "-vaapi_device", accel.Device)
		vf += ",format=nv12,hwupload"
	}
	args = append(args, "-map", "0:v:0", "-map", "1:a:0", "-vf", vf)
	spec := runSpec{Decision: r.Decision, Accel: accel, Features: m.feat}
	spec.Decision.VideoCodec = codec
	ceiling := r.Decision.BitrateKbps
	if ceiling <= 0 {
		ceiling = bitrateCeiling(r.Decision.MaxHeight, codec)
	}
	// Reserve AAC and container overhead, and bound a one-second VBV burst:
	// maxrate + buffer <= remaining budget. Keep CRF/mbtree/lookahead intact.
	budget := max(200, ceiling-max(r.Decision.AudioBitrateKbps, 128)-64)
	spec.Rate = rateControl{TargetKbps: budget * 9 / 10, MaxrateKbps: budget * 9 / 10, BufsizeKbps: budget / 10}
	if r.LiveSource {
		// A segment's average stays under the ceiling; within it, an I-frame
		// may borrow from the pictures after it.
		segment := int(r.SegmentDuration / time.Second)
		spec.Rate = rateControl{TargetKbps: budget, MaxrateKbps: budget, BufsizeKbps: budget * max(1, segment)}
	}

	args = append(args, planEncodeArgs(spec, picturePlan{w: w, h: h, outRange: rangeSDR}, enc, codec, false)...)
	if strings.HasSuffix(enc, "_videotoolbox") {
		args = append(args, "-realtime", "1")
	}
	if enc == "libx264" {
		// A live provider output starts its client one short cushion behind
		// the first picture; ten frames of lookahead keep mbtree while taking
		// a third of a second off every segment's publish lag.
		lookahead := 20
		if r.LiveSource {
			lookahead = 10
		}
		args = append(args, "-x264-params", fmt.Sprintf("vbv-init=0.1:rc-lookahead=%d:sync-lookahead=0:aq-mode=2:aq-strength=0.8:psy-rd=1.0,0.15", lookahead))
	}
	g := int((int64(r.SegmentDuration)*int64(f.FPSNum) + int64(time.Second*time.Duration(f.FPSDen))/2) / int64(time.Second*time.Duration(f.FPSDen)))
	pix := "yuv420p"
	if strings.HasSuffix(enc, "_vaapi") {
		pix = "vaapi"
	}
	if strings.HasSuffix(enc, "_qsv") || strings.HasSuffix(enc, "_videotoolbox") {
		pix = "nv12"
	}
	keyExpr := continuousKeyframeExpr(f, r.SegmentDuration, r.LiveSource)
	if startSeq > 0 {
		keyExpr = continuousKeyframeExprAt(f, r.SegmentDuration, r.LiveSource, startSeq)
	}
	args = append(args, "-pix_fmt", pix, "-bf", "0", "-g", strconv.Itoa(g), "-keyint_min", strconv.Itoa(g/4), "-sc_threshold", "0", "-force_key_frames", keyExpr, "-flags", "+global_header+cgop", "-c:a", "aac", "-b:a", fmt.Sprintf("%dk", max(r.Decision.AudioBitrateKbps, 128)), "-ar", "48000", "-ac", "2")
	return append(args, continuousTeeArgs(r, base, startSeq, tsOffsetTick)...)
}

func (e *continuousEncoder) Write(ctx context.Context, f RawFrame) error {
	f.Video = append([]byte(nil), f.Video...)
	f.PCM = append([]byte(nil), f.PCM...)
	return e.WriteShared(ctx, f)
}

// WriteShared transfers immutable buffers to the encoder. The continuous
// switcher owns each completed picture/PCM tick and never mutates it again.
// A scheduling dip blocks this consumer, never the independent output clock.
func (e *continuousEncoder) WriteShared(ctx context.Context, f RawFrame) error {
	select {
	case e.writeMu <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	case <-e.ctx.Done():
		return e.writeError()
	}
	defer func() { <-e.writeMu }()
	if len(f.Video) != e.req.Source.Width*e.req.Source.Height*3/2 || len(f.PCM) != tickPCM(e.req.Source, e.tick) {
		return errors.New("transcode: raw tick size mismatch")
	}
	e.index.recordPolicy(e.tick, f.PolicyTags)
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-e.ctx.Done():
		return e.writeError()
	case e.frames <- f:
		if e.tick == 0 {
			at := f.OutputTime
			if at.IsZero() {
				at = time.Now()
			}
			e.index.setStart(at)
		}
		e.tick++
		return nil
	}
}
func (e *continuousEncoder) Segments() SegmentIndex { return e.index }

// RecentPublishLag is the worst publication lag within about the last minute.
func (e *continuousEncoder) RecentPublishLag() time.Duration { return e.index.RecentLag() }
func (e *continuousEncoder) RealtimeSpeed() float64          { return math.Float64frombits(e.speed.Load()) }
func (e *continuousEncoder) Diagnostics() string {
	progress := map[string]string{}
	if c := e.currentChild(); c != nil && c.proc != nil && c.proc.stderr != nil {
		for line := range strings.SplitSeq(c.proc.stderr.String(), "\n") {
			key, value, ok := strings.Cut(line, "=")
			if ok {
				progress[strings.TrimSpace(key)] = strings.TrimSpace(value)
			}
		}
	}
	lag, worst := e.index.PublishLag()
	return fmt.Sprintf("queued=%d frame=%s fps=%s speed=%s outTimeUS=%s publishLag=%s worstLag=%s droppedHistory=%d", len(e.frames), progress["frame"], progress["fps"], progress["speed"], progress["out_time_us"], lag.Round(time.Millisecond), worst.Round(time.Millisecond), e.store.droppedWrites())
}
func (e *continuousEncoder) Close() error {
	e.closeOnce.Do(func() {
		e.cancel()
		if c := e.currentChild(); c != nil {
			c.forceClose()
		}
	})
	e.workers.Wait()
	return nil
}

// StartRenditions starts encoders sharing one switcher working format. Each
// encoder performs its own native scale; no Go image scaling or extra decoder
// is needed. Failure closes every encoder already started by this call.
func (m *Manager) StartRenditions(ctx context.Context, requests []EncoderRequest) ([]ContinuousEncoder, error) {
	var out []ContinuousEncoder
	for i, r := range requests {
		if i > 0 && r.Source != requests[0].Source {
			for _, e := range out {
				e.Close()
			}
			return nil, errors.New("transcode: renditions must share a working format")
		}
		e, err := m.StartEncoder(ctx, r)
		if err != nil {
			for _, e := range out {
				e.Close()
			}
			return nil, err
		}
		out = append(out, e)
	}
	return out, nil
}
