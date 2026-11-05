package transcode

import (
	"bytes"
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
)

var _ ContinuousRunner = (*Manager)(nil)

type continuousEncoder struct {
	speed atomic.Uint64
	*continuousProcess
	req     EncoderRequest
	dir     string
	frames  chan RawFrame
	writeMu chan struct{}
	tick    int64
	workers sync.WaitGroup
	index   *continuousSegmentIndex
	stream  *continuousFanout
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
	if r.SegmentDuration != 4*time.Second || r.DVRWindow < 0 {
		return nil, errors.New("transcode: invalid segment or DVR duration")
	}
	if r.Decision.VideoCodec != "" && r.Decision.VideoCodec != "h264" && r.Decision.VideoCodec != "hevc" {
		return nil, errors.New("transcode: unsupported continuous codec")
	}
	if r.Decision.AudioCodec != "" && r.Decision.AudioCodec != "aac" {
		return nil, errors.New("transcode: continuous output requires AAC")
	}
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
	sockets := make([]*rawSocket, 0, 2)
	for range 2 {
		s, e := newRawSocket()
		if e != nil {
			for _, s := range sockets {
				s.close()
			}
			return nil, e
		}
		sockets = append(sockets, s)
	}
	defer func() {
		if !ok {
			for _, s := range sockets {
				s.close()
			}
		}
	}()
	if r.SessionID == "" {
		r.SessionID = randomHex(12)
	}
	enc := &continuousEncoder{req: r, dir: dir, frames: make(chan RawFrame, 8), writeMu: make(chan struct{}, 1), stream: newContinuousFanout()}
	args := m.continuousEncodeArgs(r, sockets, dir)
	p, e := m.startContinuous(ctx, args, dir, false, sockets, enc.stream, func() { release(); enc.stream.close(); _ = removeAllRetry(dir) }, func(line string) {
		if value, ok := strings.CutPrefix(line, "speed="); ok {
			if speed, err := strconv.ParseFloat(strings.TrimSuffix(strings.TrimSpace(value), "x"), 64); err == nil && speed > 0 {
				enc.speed.Store(math.Float64bits(speed))
			}
		}
	})
	if e != nil {
		return nil, e
	}
	enc.continuousProcess = p
	enc.index = newContinuousSegmentIndex(enc)
	go enc.index.watch()
	// Each stream drains independently; probing or mux scheduling cannot deadlock
	// the other input. Sparse pictures carry output-clock timestamps, PCM is complete.
	type picture struct {
		video []byte
		tick  int64
	}
	video := make(chan picture, 8)
	audio := make(chan []byte, 8)
	enc.workers.Add(2)
	go func() {
		defer enc.workers.Done()
		c, err := sockets[0].accept()
		if err != nil {
			return
		}
		if _, err = c.Write(rawVideoHeader(r.Source)); err != nil {
			p.cancel()
			return
		}
		for {
			select {
			case <-p.ctx.Done():
				return
			case frame, ok := <-video:
				if !ok {
					return
				}
				if err = writeRawVideo(c, frame.video, frame.tick, r.Source); err != nil {
					p.cancel()
					return
				}
			}
		}
	}()
	go func() {
		defer enc.workers.Done()
		c, err := sockets[1].accept()
		if err != nil {
			return
		}
		for {
			select {
			case <-p.ctx.Done():
				return
			case pcm, ok := <-audio:
				if !ok {
					return
				}
				if _, err = io.Copy(c, bytes.NewReader(pcm)); err != nil {
					p.cancel()
					return
				}
			}
		}
	}()
	enc.workers.Add(1)
	go func() {
		defer enc.workers.Done()
		defer close(video)
		defer close(audio)
		var tick int64
		for {
			select {
			case <-p.ctx.Done():
				return
			case f := <-enc.frames:
				vch, ach := video, audio
				// Ten input pictures/s under pressure; fps repeats after scale/format.
				// Still send a timestamp at least every three ticks so mux progress continues.
				if f.RepeatVideo && tick%3 != 0 {
					vch = nil
				}
				for vch != nil || ach != nil {
					select {
					case vch <- picture{f.Video, tick}:
						vch = nil
					case ach <- f.PCM:
						ach = nil
					case <-p.ctx.Done():
						return
					}
				}
				tick++
			}
		}
	}()
	ok = true
	clean = false
	return enc, nil
}
func (m *Manager) continuousEncodeArgs(r EncoderRequest, s []*rawSocket, dir string) []string {
	f := r.Source
	rate := fmt.Sprintf("%d/%d", f.FPSNum, f.FPSDen)
	args := []string{"-hide_banner", "-loglevel", "warning", "-stats_period", "1", "-progress", "pipe:2", "-nostdin", "-y", "-filter_threads", "1"}
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
	enc, _ := videoEncoder(codec, m.hw, m.feat)
	w, h := outputSize(f.Width, f.Height, r.Decision.MaxHeight)
	vf := fmt.Sprintf("scale=%d:%d,setsar=1,format=yuv420p", w, h)
	if strings.HasSuffix(enc, "_videotoolbox") || strings.HasSuffix(enc, "_qsv") {
		vf = fmt.Sprintf("scale=%d:%d,setsar=1,format=nv12", w, h)
	}
	vf += ",fps=" + rate
	if strings.HasSuffix(enc, "_vaapi") {
		args = append(args, "-vaapi_device", m.hw.Device)
		vf += ",format=nv12,hwupload"
	}
	args = append(args, "-map", "0:v:0", "-map", "1:a:0", "-vf", vf)
	spec := runSpec{Decision: r.Decision, Accel: m.hw, Features: m.feat}
	spec.Decision.VideoCodec = codec
	ceiling := r.Decision.BitrateKbps
	if ceiling <= 0 {
		ceiling = bitrateCeiling(r.Decision.MaxHeight, codec)
	}
	// Reserve AAC and container overhead, and bound a one-second VBV burst:
	// maxrate + buffer <= remaining budget. Keep CRF/mbtree/lookahead intact.
	budget := max(200, ceiling-max(r.Decision.AudioBitrateKbps, 128)-64)
	spec.Rate = rateControl{TargetKbps: budget * 9 / 10, MaxrateKbps: budget * 9 / 10, BufsizeKbps: budget / 10}

	args = append(args, planEncodeArgs(spec, picturePlan{w: w, h: h, outRange: rangeSDR}, enc, codec, false)...)
	if strings.HasSuffix(enc, "_videotoolbox") {
		args = append(args, "-realtime", "1")
	}
	if enc == "libx264" {
		args = append(args, "-x264-params", "vbv-init=0.1:rc-lookahead=20:sync-lookahead=0:aq-mode=2:aq-strength=0.8:psy-rd=1.0,0.15")
	}
	g := int((int64(r.SegmentDuration)*int64(f.FPSNum) + int64(time.Second*time.Duration(f.FPSDen))/2) / int64(time.Second*time.Duration(f.FPSDen)))
	pix := "yuv420p"
	if strings.HasSuffix(enc, "_vaapi") {
		pix = "vaapi"
	}
	if strings.HasSuffix(enc, "_qsv") || strings.HasSuffix(enc, "_videotoolbox") {
		pix = "nv12"
	}
	args = append(args, "-pix_fmt", pix, "-bf", "0", "-g", strconv.Itoa(g), "-keyint_min", strconv.Itoa(g/4), "-sc_threshold", "0", "-force_key_frames", fmt.Sprintf("expr:if(lt(n,%d),eq(n,0)+eq(n,%d)+eq(n,%d),gte(n,(n_forced-2)*%d))", g, g/4, g/2, g), "-flags", "+global_header+cgop", "-c:a", "aac", "-b:a", fmt.Sprintf("%dk", max(r.Decision.AudioBitrateKbps, 128)), "-ar", "48000", "-ac", "2")
	// Tee encodes once and muxes the identical packets into all delivery forms.
	duration := float64(g*f.FPSDen) / float64(f.FPSNum)
	ds := strconv.FormatFloat(duration, 'f', 6, 64)
	window := continuousWindow(r.DVRWindow, time.Second*time.Duration(g*f.FPSDen)/time.Duration(f.FPSNum)) + 3
	initial := strconv.FormatFloat(duration/4, 'f', 6, 64)
	outputs := []string{fmt.Sprintf("[f=hls:hls_time=%s:hls_init_time=%s:hls_list_size=%d:hls_flags=delete_segments+independent_segments+temp_file:hls_segment_filename=%s]%s", ds, initial, window, "seg-%09d.ts", "index.m3u8"), fmt.Sprintf("[f=hls:hls_time=%s:hls_init_time=%s:hls_list_size=%d:hls_segment_type=fmp4:hls_fmp4_init_filename=init.mp4:hls_flags=delete_segments+independent_segments+temp_file:hls_segment_filename=fragment-%%09d.m4s]fragments.m3u8", ds, initial, window)}
	outputs = append(outputs, "[f=mpegts:mpegts_flags=resend_headers]pipe:1")
	return append(args, "-f", "tee", strings.Join(outputs, "|"))
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
func (e *continuousEncoder) RealtimeSpeed() float64 { return math.Float64frombits(e.speed.Load()) }
func (e *continuousEncoder) Diagnostics() string {
	progress := map[string]string{}
	for line := range strings.SplitSeq(e.stderr.String(), "\n") {
		key, value, ok := strings.Cut(line, "=")
		if ok {
			progress[strings.TrimSpace(key)] = strings.TrimSpace(value)
		}
	}
	return fmt.Sprintf("queued=%d frame=%s fps=%s speed=%s outTimeUS=%s", len(e.frames), progress["frame"], progress["fps"], progress["speed"], progress["out_time_us"])
}
func (e *continuousEncoder) Close() error {
	e.continuousProcess.close()
	e.workers.Wait()
	e.stream.close()
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
