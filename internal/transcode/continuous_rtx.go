package transcode

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
)

type continuousRTXAttemptKey struct {
	cfg       config.RTXVideo
	path      string
	available bool
}

type continuousRTXPlan struct {
	width         int
	height        int
	quality       int
	level         string
	videoKbps     int
	bufsizeKbps   int
	audioKbps     int
	maxConcurrent int
}

type continuousPicture struct {
	video []byte
	tick  int64
}

type continuousChild struct {
	rtx         bool
	hardware    bool
	plan        continuousRTXPlan
	attemptKey  continuousRTXAttemptKey
	startSeq    int64
	startTick   int64
	video       chan continuousPicture
	audio       chan []byte
	draining    chan struct{}
	drainOnce   sync.Once
	proc        *continuousProcess
	rtxCtx      context.Context
	rtxCancel   context.CancelFunc
	rtxProc     Process
	rtxStderr   *tailBuffer
	rtxDone     chan struct{}
	rtxCloseOne sync.Once
	rtxErr      error
	ioWG        sync.WaitGroup
}

func (m *Manager) continuousRTXPlanFor(r EncoderRequest, holdingPermit bool) (continuousRTXPlan, continuousRTXAttemptKey, bool) {
	if m == nil {
		return continuousRTXPlan{}, continuousRTXAttemptKey{}, false
	}
	m.mu.Lock()
	cfg := m.cfg
	m.mu.Unlock()

	m.rtx.mu.Lock()
	rtxPath := m.rtx.path
	configured := m.rtx.configuredPath
	support := m.rtx.support
	active := m.rtx.active
	m.rtx.mu.Unlock()

	key := continuousRTXAttemptKey{
		cfg:       cfg.RTXVideo,
		path:      rtxPath,
		available: support.Available,
	}

	c, invalid := normalizedRTX(cfg.RTXVideo)
	if invalid != "" || c.Mode == "off" {
		return continuousRTXPlan{}, key, false
	}
	if c.ExecutablePath != configured || !support.Available || rtxPath == "" {
		return continuousRTXPlan{}, key, false
	}
	activeOthers := active
	if holdingPermit && activeOthers > 0 {
		activeOthers--
	}
	if activeOthers >= c.MaxConcurrent {
		return continuousRTXPlan{}, key, false
	}

	codec := r.Decision.VideoCodec
	if codec == "" {
		codec = "h264"
	}
	if codec != "h264" || r.ClientCaps.Download {
		return continuousRTXPlan{}, key, false
	}
	if c.Mode == "auto" && r.ClientCaps.Remote {
		return continuousRTXPlan{}, key, false
	}
	if len(r.ClientCaps.VideoCodecs) > 0 && !r.ClientCaps.SupportsVideo("h264") {
		return continuousRTXPlan{}, key, false
	}
	if profiles := r.ClientCaps.VideoProfiles["h264"]; len(profiles) > 0 && !contains(profiles, "high") {
		return continuousRTXPlan{}, key, false
	}

	f := r.Source
	if f.Height <= 0 || f.Height > c.SourceMaxHeight {
		return continuousRTXPlan{}, key, false
	}
	if r.Decision.MaxHeight > 0 && r.Decision.MaxHeight < f.Height {
		return continuousRTXPlan{}, key, false
	}

	height := even(minPositive(c.TargetMaxHeight, r.ClientCaps.MaxHeight, r.ClientCaps.VideoMaxHeights["h264"]))
	if height <= f.Height || height > 2160 {
		return continuousRTXPlan{}, key, false
	}
	width := even(int(math.Round(float64(f.Width) * float64(height) / float64(f.Height))))
	if width < 2 || width > 3840 {
		return continuousRTXPlan{}, key, false
	}

	fps := float64(f.FPSNum) / float64(f.FPSDen)
	if math.Ceil(float64(width)/16)*math.Ceil(float64(height)/16)*fps > 983040 {
		return continuousRTXPlan{}, key, false
	}
	levelNum := 41
	level := "4.1"
	if width*height > 1920*1088 || fps > 30 {
		levelNum = 51
		level = "5.1"
	}
	if maxLevel := r.ClientCaps.VideoLevels["h264"]; maxLevel > 0 && maxLevel < levelNum {
		return continuousRTXPlan{}, key, false
	}

	audioKbps := max(r.Decision.AudioBitrateKbps, 128)
	remote := 0
	if r.ClientCaps.Remote {
		remote = cfg.RemoteMaxBitrateKbps
	}
	capKbps := minPositive(r.ClientCaps.MaxBitrateKbps, remote)
	minVideo := minRTXVideoKbps(height)
	if capKbps > 0 && capKbps-audioKbps-64 < minVideo {
		return continuousRTXPlan{}, key, false
	}
	if capKbps == 0 {
		capKbps = bitrateCeiling(height, "h264") + audioKbps + 64
	}
	videoKbps := max(minVideo, capKbps-audioKbps-64)
	segSec := max(1, int(r.SegmentDuration/time.Second))
	bufsizeKbps := videoKbps * 2
	if r.LiveSource {
		bufsizeKbps = videoKbps * max(2, segSec)
	}

	return continuousRTXPlan{
		width:         width,
		height:        height,
		quality:       c.Quality,
		level:         level,
		videoKbps:     videoKbps,
		bufsizeKbps:   bufsizeKbps,
		audioKbps:     audioKbps,
		maxConcurrent: c.MaxConcurrent,
	}, key, true
}

func buildContinuousRTXArgs(r EncoderRequest, plan continuousRTXPlan, videoURL, keyfilePath string) []string {
	f := r.Source
	g, _, _ := continuousLadder(f, r.SegmentDuration, r.LiveSource)
	args := []string{
		"--avsw",
		"--device", "0",
		"--input-format", "matroska",
		"--input-option", "protocol_whitelist:tcp",
		"--input-analyze", "0.2",
		"--input-probesize", "32768",
		"-i", videoURL,
		"--fps", fmt.Sprintf("%d/%d", f.FPSNum, f.FPSDen),
		"--vpp-resize", fmt.Sprintf("algo=ngx-vsr,vsr-quality=%d", plan.quality),
		"--output-res", fmt.Sprintf("%dx%d", plan.width, plan.height),
		"-c", "h264",
		"--profile", "high",
		"--level", plan.level,
		"--output-depth", "8",
		"--output-csp", "yuv420",
		"--vbr", "0",
		"--vbr-quality", "20",
		"--max-bitrate", strconv.Itoa(plan.videoKbps),
		"--vbv-bufsize", strconv.Itoa(plan.bufsizeKbps),
		"--gop-len", strconv.FormatInt(g, 10),
		"--strict-gop",
		"--lookahead", "0",
		"--no-i-adapt",
		"--no-b-adapt",
		"--bframes", "0",
		"--repeat-headers",
		"--colormatrix", "bt709",
		"--colorprim", "bt709",
		"--transfer", "bt709",
		"--metadata", "clear",
	}
	if keyfilePath != "" {
		args = append(args, "--keyfile", filepath.ToSlash(keyfilePath))
	}
	args = append(args, "-m", "flush_packets:1", "-f", "mpegts", "-o", "-")
	return args
}

func continuousRTXMuxArgs(r EncoderRequest, plan continuousRTXPlan, h264Sock, audioSock *rawSocket, base string, startSeq, tsOffsetTick int64) []string {
	// Open raw PCM first (32-byte probe) so audioSock is accepted immediately
	// without waiting for NVEncC's first H.264 IDR packet.
	args := []string{
		"-hide_banner", "-loglevel", "warning", "-stats_period", "1", "-progress", "pipe:2", "-nostdin", "-y",
		"-thread_queue_size", "32", "-probesize", "32", "-analyzeduration", "0", "-f", "s16le", "-ar", "48000", "-ac", "2", "-i", audioSock.url(),
		"-thread_queue_size", "32", "-probesize", "8192", "-analyzeduration", "0", "-f", "mpegts", "-i", h264Sock.url(),
		"-map", "1:v:0", "-map", "0:a:0",
		"-c:v", "copy", "-bsf:v", "dump_extra", "-flags", "+global_header+cgop",
		"-c:a", "aac", "-b:a", fmt.Sprintf("%dk", plan.audioKbps), "-ar", "48000", "-ac", "2",
	}
	return append(args, continuousTeeArgs(r, base, startSeq, tsOffsetTick)...)
}

func continuousTeeArgs(r EncoderRequest, base string, startSeq, tsOffsetTick int64) []string {
	f := r.Source
	g, startup, _ := continuousLadder(f, r.SegmentDuration, r.LiveSource)
	duration := float64(g*int64(f.FPSDen)) / float64(f.FPSNum)
	ds := strconv.FormatFloat(duration, 'f', 6, 64)
	window := continuousWindow(r.DVRWindow, time.Second*time.Duration(g*int64(f.FPSDen))/time.Duration(f.FPSNum)) + 3
	opt := strings.ReplaceAll(base, ":", `\\\:`)
	put := "method=PUT:http_persistent=1"

	var args []string
	if tsOffsetTick > 0 {
		offsetSec := continuousFrameTime(f, tsOffsetTick).Seconds()
		args = append(args, "-output_ts_offset", strconv.FormatFloat(offsetSec, 'f', 6, 64))
	}

	hlsOpts := fmt.Sprintf("hls_time=%s", ds)
	if startSeq < int64(len(startup)) {
		initial := strconv.FormatFloat(duration/4, 'f', 6, 64)
		hlsOpts += ":hls_init_time=" + initial
	}
	if startSeq > 0 {
		hlsOpts += fmt.Sprintf(":start_number=%d", startSeq)
	}

	outputs := []string{
		fmt.Sprintf("[f=hls:%s:hls_list_size=%d:hls_flags=independent_segments:%s:hls_segment_filename=%sseg-%%09d.ts]%sindex.m3u8", hlsOpts, window, put, opt, base),
		fmt.Sprintf("[f=hls:%s:hls_list_size=%d:hls_segment_type=fmp4:hls_fmp4_init_filename=init.mp4:hls_flags=independent_segments:%s:hls_segment_filename=%sfragment-%%09d.m4s]%sfragments.m3u8", hlsOpts, window, put, opt, base),
		"[f=mpegts:mpegts_flags=resend_headers]pipe:1",
	}
	return append(args, "-f", "tee", strings.Join(outputs, "|"))
}

func (c *continuousChild) markDraining() {
	c.drainOnce.Do(func() {
		close(c.draining)
	})
}

func (c *continuousChild) isDraining() bool {
	select {
	case <-c.draining:
		return true
	default:
		return false
	}
}

func (c *continuousChild) startInputWriters(source RawFormat, videoSock, audioSock *rawSocket, onCancel func()) {
	c.ioWG.Add(2)
	go func() {
		defer c.ioWG.Done()
		conn, err := videoSock.accept()
		if err != nil {
			return
		}
		if _, err = conn.Write(rawVideoHeader(source)); err != nil {
			_ = conn.Close()
			onCancel()
			return
		}
		for {
			select {
			case <-c.proc.ctx.Done():
				return
			case frame, ok := <-c.video:
				if !ok {
					if c.isDraining() {
						_ = conn.Close()
					}
					return
				}
				if err = writeRawVideo(conn, frame.video, frame.tick, source); err != nil {
					_ = conn.Close()
					onCancel()
					return
				}
			}
		}
	}()
	go func() {
		defer c.ioWG.Done()
		conn, err := audioSock.accept()
		if err != nil {
			return
		}
		for {
			select {
			case <-c.proc.ctx.Done():
				return
			case pcm, ok := <-c.audio:
				if !ok {
					if c.isDraining() {
						_ = conn.Close()
					}
					return
				}
				if _, err = io.Copy(conn, bytes.NewReader(pcm)); err != nil {
					_ = conn.Close()
					onCancel()
					return
				}
			}
		}
	}()
}

func (c *continuousChild) forceClose() {
	if c == nil {
		return
	}
	if c.rtxCancel != nil {
		c.rtxCloseOne.Do(c.rtxCancel)
	}
	if c.proc != nil {
		_ = c.proc.close()
	}
	if c.rtxDone != nil {
		<-c.rtxDone
	}
	c.ioWG.Wait()
}

func (c *continuousChild) drainInputsAndWait(e *continuousEncoder, targetSeq int64) {
	if c == nil {
		return
	}
	c.markDraining()
	close(c.video)
	close(c.audio)
	c.ioWG.Wait()

	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(15 * time.Millisecond)
	defer ticker.Stop()
	for e.index.nextSeq() < targetSeq {
		select {
		case <-e.ctx.Done():
			c.forceClose()
			return
		case <-c.proc.done:
			e.index.refresh()
			c.forceClose()
			return
		case <-deadline.C:
			c.forceClose()
			return
		case <-ticker.C:
			e.index.refresh()
		}
	}
	c.forceClose()
}

func (c *continuousChild) failure() error {
	if c == nil {
		return io.EOF
	}
	if c.rtx && c.rtxDone != nil {
		select {
		case <-c.rtxDone:
			if c.rtxErr != nil {
				tail := ""
				if c.rtxStderr != nil {
					tail = strings.TrimSpace(c.rtxStderr.String())
				}
				if tail != "" {
					return fmt.Errorf("continuous rtx: %w: %s", c.rtxErr, tail)
				}
				return fmt.Errorf("continuous rtx: %w", c.rtxErr)
			}
		default:
		}
	}
	if c.proc != nil {
		return c.proc.result()
	}
	return io.EOF
}

func (e *continuousEncoder) ordinaryBandwidthBps() int {
	d := e.req.Decision
	if d.VideoCodec == "" {
		d.VideoCodec = "h264"
	}
	kbps := d.BitrateKbps
	if kbps <= 0 {
		kbps = videoBitrate(core.MediaInfo{}, d) + max(d.AudioBitrateKbps, 128)
	}
	return kbps * 1000
}

func (e *continuousEncoder) speedObserver(line string) {
	if value, ok := strings.CutPrefix(line, "speed="); ok {
		if speed, err := strconv.ParseFloat(strings.TrimSuffix(strings.TrimSpace(value), "x"), 64); err == nil && speed > 0 {
			e.speed.Store(math.Float64bits(speed))
		}
	}
}

func (e *continuousEncoder) rtxBlocked(key continuousRTXAttemptKey) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.hasRTXFailure && e.rtxFailed == key
}

func (e *continuousEncoder) recordRTXFailure(key continuousRTXAttemptKey) {
	e.mu.Lock()
	e.hasRTXFailure = true
	e.rtxFailed = key
	e.mu.Unlock()
}

func (e *continuousEncoder) clearRTXFailureIfOff(key continuousRTXAttemptKey) {
	c, _ := normalizedRTX(key.cfg)
	if c.Mode == "off" {
		e.mu.Lock()
		e.hasRTXFailure = false
		e.mu.Unlock()
	}
}

func (e *continuousEncoder) wantBoundarySwap(cur *continuousChild) bool {
	if e.forceCPU {
		return false
	}
	plan, key, wantRTX := e.m.continuousRTXPlanFor(e.req, cur != nil && cur.rtx)
	e.clearRTXFailureIfOff(key)
	if wantRTX && e.rtxBlocked(key) {
		wantRTX = false
	}
	if cur == nil {
		return true
	}
	if cur.rtx != wantRTX {
		return true
	}
	if cur.rtx && wantRTX && cur.plan != plan {
		return true
	}
	return false
}

func (e *continuousEncoder) startGeneration(startSeq, startTick, tsOffsetTick int64) (*continuousChild, error) {
	plan, key, wantRTX := e.m.continuousRTXPlanFor(e.req, false)
	e.clearRTXFailureIfOff(key)
	if wantRTX && !e.forceCPU && !e.rtxBlocked(key) {
		child, err := e.startRTXChild(startSeq, startTick, tsOffsetTick, plan, key)
		if err == nil {
			e.setChild(child)
			e.index.setBandwidth((plan.videoKbps + plan.audioKbps) * 1000)
			if e.m != nil && e.m.log != nil {
				e.m.log.Info("continuous RTX Video Super Resolution active",
					"session", e.req.SessionID,
					"seq", startSeq,
					"width", plan.width,
					"height", plan.height,
					"quality", plan.quality)
			}
			return child, nil
		}
		e.recordRTXFailure(key)
		if e.m != nil && e.m.log != nil {
			e.m.log.Warn("continuous RTX Video fallback to ordinary encoder",
				"session", e.req.SessionID,
				"seq", startSeq,
				"err", err)
		}
	}
	child, err := e.startOrdinaryChild(startSeq, startTick, tsOffsetTick)
	if err != nil {
		return nil, err
	}
	e.setChild(child)
	e.index.setBandwidth(e.ordinaryBandwidthBps())
	return child, nil
}

func (e *continuousEncoder) startOrdinaryChild(startSeq, startTick, tsOffsetTick int64) (*continuousChild, error) {
	sockets := make([]*rawSocket, 0, 2)
	for range 2 {
		s, err := newRawSocket()
		if err != nil {
			for _, open := range sockets {
				open.close()
			}
			return nil, err
		}
		sockets = append(sockets, s)
	}
	accel := e.m.hw
	if e.forceCPU {
		accel = core.HWAccel{Name: AccelNone}
	}
	_, hardware := videoEncoder(e.req.Decision.VideoCodec, accel, e.m.feat)
	args := e.m.continuousEncodeArgsAt(e.req, sockets, e.store.url(), startSeq, tsOffsetTick, accel)
	p, err := e.m.startContinuous(e.ctx, args, e.dir, false, sockets, e.stream, nil, e.speedObserver)
	if err != nil {
		for _, s := range sockets {
			s.close()
		}
		if hardware && !e.forceCPU && e.ctx.Err() == nil {
			e.forceCPU = true
			e.m.log.Warn("continuous hardware encoder failed to start; falling back to software", "session", e.req.SessionID, "err", err)
			return e.startOrdinaryChild(startSeq, startTick, tsOffsetTick)
		}
		return nil, err
	}
	rtxDone := make(chan struct{})
	close(rtxDone)
	child := &continuousChild{
		rtx:       false,
		hardware:  hardware,
		startSeq:  startSeq,
		startTick: startTick,
		video:     make(chan continuousPicture, 8),
		audio:     make(chan []byte, 8),
		draining:  make(chan struct{}),
		proc:      p,
		rtxDone:   rtxDone,
	}
	child.startInputWriters(e.req.Source, sockets[0], sockets[1], p.cancel)
	return child, nil
}

func (e *continuousEncoder) startRTXChild(startSeq, startTick, tsOffsetTick int64, plan continuousRTXPlan, key continuousRTXAttemptKey) (*continuousChild, error) {
	b := &e.m.rtx
	b.mu.Lock()
	if !b.support.Available || b.path == "" {
		b.mu.Unlock()
		return nil, errors.New("RTX Video backend unavailable")
	}
	if b.active >= plan.maxConcurrent {
		b.mu.Unlock()
		return nil, errors.New("RTX Video enhancement concurrency cap reached")
	}
	b.active++
	rtxPath := b.path
	b.mu.Unlock()
	releasePermit := sync.OnceFunc(func() {
		b.mu.Lock()
		b.active--
		b.mu.Unlock()
	})

	e.m.mu.Lock()
	if e.m.closed {
		e.m.mu.Unlock()
		releasePermit()
		return nil, errors.New("transcode: manager closed")
	}
	e.m.continuousWG.Add(1)
	e.m.mu.Unlock()

	sockets := make([]*rawSocket, 0, 3)
	for range 3 {
		s, err := newRawSocket()
		if err != nil {
			for _, open := range sockets {
				open.close()
			}
			e.m.continuousWG.Done()
			releasePermit()
			return nil, err
		}
		sockets = append(sockets, s)
	}
	videoSock, h264Sock, audioSock := sockets[0], sockets[1], sockets[2]

	keyfilePath := ""
	if startSeq == 0 {
		_, startup, steady := continuousLadder(e.req.Source, e.req.SegmentDuration, e.req.LiveSource)
		var kb strings.Builder
		for _, n := range startup[1:] {
			if n > 0 {
				fmt.Fprintln(&kb, n)
			}
		}
		if steady > 0 {
			fmt.Fprintln(&kb, steady)
		}
		keyfilePath = filepath.Join(e.dir, "rtx-continuous-keys.txt")
		if err := os.WriteFile(keyfilePath, []byte(kb.String()), 0o600); err != nil {
			for _, s := range sockets {
				s.close()
			}
			e.m.continuousWG.Done()
			releasePermit()
			return nil, err
		}
	}

	rtxArgs := buildContinuousRTXArgs(e.req, plan, videoSock.url(), keyfilePath)
	rtxStderr := newTailBuffer(24)
	rPipe, wPipe := io.Pipe()

	startFn := e.m.runner.Start
	if owner, ok := e.m.runner.(RTXRunner); ok {
		startFn = owner.StartRTX
	}
	rtxCtx, rtxCancel := context.WithTimeout(e.ctx, maxProcessLifetime)
	rtxProc, err := startFn(Command{
		Path:   rtxPath,
		Args:   rtxArgs,
		Dir:    e.dir,
		Stdout: wPipe,
		Stderr: &lineWriter{fn: func(line string) {
			rtxStderr.add(redactRTXLine(line, videoSock.url(), nil))
		}},
	})
	if rtxProc == nil {
		rtxCancel()
		_ = wPipe.Close()
		_ = rPipe.Close()
		for _, s := range sockets {
			s.close()
		}
		e.m.continuousWG.Done()
		releasePermit()
		if err != nil {
			return nil, err
		}
		return nil, errors.New("RTX Video executable failed to start")
	}

	child := &continuousChild{
		rtx:        true,
		plan:       plan,
		attemptKey: key,
		startSeq:   startSeq,
		startTick:  startTick,
		video:      make(chan continuousPicture, 16),
		audio:      make(chan []byte, 128),
		draining:   make(chan struct{}),
		rtxCtx:     rtxCtx,
		rtxCancel:  rtxCancel,
		rtxProc:    rtxProc,
		rtxStderr:  rtxStderr,
		rtxDone:    make(chan struct{}),
	}

	go func() {
		defer e.m.continuousWG.Done()
		select {
		case <-rtxCtx.Done():
		case <-e.m.stop:
		case <-rtxProc.Done():
			videoSock.close()
			_ = wPipe.Close()
			child.rtxErr = rtxProc.Err()
			releasePermit()
			close(child.rtxDone)
			return
		}
		rtxCancel()
		videoSock.close()
		_ = wPipe.Close()
		_ = rPipe.Close()
		rtxProc.Stop()
		child.rtxErr = rtxProc.Err()
		releasePermit()
		close(child.rtxDone)
	}()

	if err != nil {
		rtxCancel()
		for _, s := range sockets {
			s.close()
		}
		<-child.rtxDone
		return nil, err
	}

	muxArgs := continuousRTXMuxArgs(e.req, plan, h264Sock, audioSock, e.store.url(), startSeq, tsOffsetTick)
	muxProc, muxErr := e.m.startContinuous(e.ctx, muxArgs, e.dir, false, []*rawSocket{h264Sock, audioSock}, e.stream, nil, e.speedObserver)
	if muxErr != nil {
		rtxCancel()
		for _, s := range sockets {
			s.close()
		}
		<-child.rtxDone
		return nil, muxErr
	}
	child.proc = muxProc

	child.ioWG.Add(1)
	go func() {
		defer child.ioWG.Done()
		conn, acceptErr := h264Sock.accept()
		if acceptErr != nil {
			_ = rPipe.CloseWithError(acceptErr)
			return
		}
		_, copyErr := io.Copy(conn, rPipe)
		_ = conn.Close()
		if copyErr != nil {
			_ = rPipe.CloseWithError(copyErr)
		} else {
			_ = rPipe.Close()
		}
	}()

	cancelBoth := func() {
		rtxCancel()
		muxProc.cancel()
	}
	child.startInputWriters(e.req.Source, videoSock, audioSock, cancelBoth)
	return child, nil
}
