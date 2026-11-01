package transcode

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
)

// fakeRunner stands in for ffmpeg/ffprobe. Run answers the startup
// queries; Start simulates an HLS run writing segments into its directory.
type fakeRunner struct {
	mu sync.Mutex

	encoders string // -encoders output
	hwaccels string
	filters  string
	// failEncode makes the 1-frame test encode fail for an encoder name.
	failEncode map[string]bool
	probeJSON  []string // successive ffprobe outputs (last repeats)
	probeCalls [][]string
	runCalls   [][]string

	// HLS simulation.
	segDelay   time.Duration // time to produce each segment
	lastSeg    int           // input ends after this segment; -1 = never
	failStart  func(args []string) (stderr string, fail bool)
	failAfter  int // fail after producing this many segments (0 = never)
	eofAfter   int // clean EOF after this many segments of this run (0 = never)
	starts     [][]string
	procs      []*fakeProc
	runFunc    func(ctx context.Context, cmd Command) error
	startError error
	noPause    bool // processes cannot be suspended (Windows)

	// DASH simulation: the input ends at this media time (0 = never).
	dashEnd  time.Duration
	dashHold bool // keep the bounded producer alive until Stop, to test cleanup before exit

	// Chunked HLS simulation (keyframe-planned sessions): source keyframe
	// interval (default 2.5s) and end of input (0 = never).
	chunkGOP time.Duration
	chunkEnd time.Duration
}

func newFakeRunner() *fakeRunner {
	return &fakeRunner{
		encoders: sampleEncoders,
		hwaccels: "Hardware acceleration methods:\nvideotoolbox\n\n",
		filters:  sampleFilters,
		segDelay: 5 * time.Millisecond,
		lastSeg:  -1,
	}
}

const sampleEncoders = `Encoders:
 V..... = Video
 A..... = Audio
 S..... = Subtitle
 ------
 V....D libx264              libx264 H.264 / AVC / MPEG-4 AVC / MPEG-4 part 10 (codec h264)
 V....D libx265              libx265 H.265 / HEVC (codec hevc)
 V....D h264_videotoolbox    VideoToolbox H.264 Encoder (codec h264)
 V....D hevc_videotoolbox    VideoToolbox H.265 Encoder (codec hevc)
 V....D h264_nvenc           NVIDIA NVENC H.264 encoder (codec h264)
 V....D hevc_nvenc           NVIDIA NVENC hevc encoder (codec hevc)
 V..... h264_qsv             H.264 / AVC / MPEG-4 AVC / MPEG-4 part 10 (Intel Quick Sync Video acceleration) (codec h264)
 V....D h264_vaapi           H.264/AVC (VAAPI) (codec h264)
 A....D aac                  AAC (Advanced Audio Coding)
 A....D ac3                  ATSC A/52A (AC-3)
 A....D eac3                 ATSC A/52 E-AC-3
 S..... webvtt               WebVTT subtitle
`

const sampleFilters = `Filters:
  T.. = Timeline support
  .S. = Slice threading
  A = Audio input/output
  | = Source or sink filter
 TS overlay           VV->V      Overlay a video source on top of the input.
 .. scale             V->V       Scale the input video size and/or convert the image format.
 .. scale_vt          V->V       Scale Videotoolbox frames
 .. zscale            V->V       Apply resizing, colorspace and bit depth conversion.
 .. colorspace        V->V       Convert between colorspaces.
 .S tonemap           V->V       Conversion to/from different dynamic ranges.
 .. tonemap_vaapi     V->V       VAAPI VPP for tone-mapping
 .. subtitles         V->V       Render text subtitles onto input video using the libass library.
 .. buffer            |->V       Buffer video frames, and make them accessible to the filterchain.
`

func argValue(args []string, flag string) string {
	for i := 0; i < len(args)-1; i++ {
		if args[i] == flag {
			return args[i+1]
		}
	}
	return ""
}

func hasArg(args []string, flag string) bool {
	for _, a := range args {
		if a == flag {
			return true
		}
	}
	return false
}

func (f *fakeRunner) Run(ctx context.Context, cmd Command) error {
	f.mu.Lock()
	f.runCalls = append(f.runCalls, cmd.Args)
	rf := f.runFunc
	f.mu.Unlock()
	if rf != nil {
		if err := rf(ctx, cmd); !errors.Is(err, errUnhandled) {
			return err
		}
	}
	w := func(dst io.Writer, s string) {
		if dst != nil {
			_, _ = io.WriteString(dst, s)
		}
	}
	args := cmd.Args
	switch {
	case strings.Contains(filepath.Base(cmd.Path), "ffprobe"):
		f.mu.Lock()
		f.probeCalls = append(f.probeCalls, args)
		n := len(f.probeCalls)
		var out string
		if len(f.probeJSON) > 0 {
			i := n - 1
			if i >= len(f.probeJSON) {
				i = len(f.probeJSON) - 1
			}
			out = f.probeJSON[i]
		}
		f.mu.Unlock()
		if out == "" {
			w(cmd.Stderr, "http://x: Server returned 404 Not Found")
			return errors.New("exit status 1")
		}
		w(cmd.Stdout, out)
		return nil
	case hasArg(args, "-version"):
		w(cmd.Stdout, "ffmpeg version 7.1-fake Copyright (c) 2000-2024 the FFmpeg developers\n")
		return nil
	case hasArg(args, "-encoders"):
		w(cmd.Stdout, f.encoders)
		return nil
	case hasArg(args, "-hwaccels"):
		w(cmd.Stdout, f.hwaccels)
		return nil
	case hasArg(args, "-filters"):
		w(cmd.Stdout, f.filters)
		return nil
	case argValue(args, "-h") == "filter=scale_vt":
		w(cmd.Stdout, "  format <pix_fmt> Output pixel format (default none)\n")
		return nil
	case argValue(args, "-f") == "lavfi":
		enc := argValue(args, "-c:v")
		rc := argValue(args, "-rc_mode")
		vf := argValue(args, "-vf")
		f.mu.Lock()
		fail := f.failEncode[enc] ||
			(rc != "" && (f.failEncode["rc:"+rc] || f.failEncode[enc+":"+rc])) ||
			(f.failEncode["scale_vaapi"] && strings.Contains(vf, "scale_vaapi")) ||
			(f.failEncode["tonemap_vaapi"] && strings.Contains(vf, "tonemap_vaapi"))
		f.mu.Unlock()
		if fail {
			w(cmd.Stderr, "Error while opening encoder - maybe incorrect parameters")
			return errors.New("exit status 1")
		}
		return nil
	}
	return fmt.Errorf("fakeRunner: unexpected Run %v", args)
}

var errUnhandled = errors.New("unhandled")

func (f *fakeRunner) Start(cmd Command) (Process, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.starts = append(f.starts, cmd.Args)
	if f.startError != nil {
		return nil, f.startError
	}
	p := &fakeProc{
		done:    make(chan struct{}),
		kill:    make(chan struct{}),
		resumed: make(chan struct{}),
		args:    cmd.Args,
		noPause: f.noPause,
	}
	close(p.resumed)
	stderr := ""
	fail := false
	if f.failStart != nil {
		stderr, fail = f.failStart(cmd.Args)
	}
	if argValue(cmd.Args, "-f") == "dash" {
		go p.simulateDASH(cmd, f.segDelay, f.dashEnd, f.dashHold, f.failAfter, fail, stderr)
	} else if fakeChunked(cmd.Args) {
		go p.simulateChunks(cmd, f.segDelay, f.chunkGOP, f.chunkEnd, f.lastSeg, f.failAfter, f.eofAfter, fail, stderr)
	} else {
		go p.simulate(cmd, f.segDelay, f.lastSeg, f.failAfter, f.eofAfter, fail, stderr)
	}
	f.procs = append(f.procs, p)
	return p, nil
}

func (f *fakeRunner) startCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.starts)
}

func (f *fakeRunner) lastStart() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.starts) == 0 {
		return nil
	}
	return f.starts[len(f.starts)-1]
}

func (f *fakeRunner) lastProc() *fakeProc {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.procs) == 0 {
		return nil
	}
	return f.procs[len(f.procs)-1]
}

// fakeProc writes segments like ffmpeg's HLS muxer: each segment is
// written to .tmp, renamed, then listed in the run playlist.
type fakeProc struct {
	args     []string
	done     chan struct{}
	kill     chan struct{}
	killOnce sync.Once
	err      error

	mu      sync.Mutex
	paused  bool
	resumed chan struct{}
	pauses  int
	killed  bool
	noPause bool
}

func (p *fakeProc) Done() <-chan struct{} { return p.done }
func (p *fakeProc) Err() error            { <-p.done; return p.err }
func (p *fakeProc) Stop() {
	p.killOnce.Do(func() {
		p.mu.Lock()
		p.killed = true
		p.mu.Unlock()
		close(p.kill)
	})
	<-p.done
}

func (p *fakeProc) Pause() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.noPause {
		return ErrPauseUnsupported
	}
	if !p.paused {
		p.paused = true
		p.pauses++
		p.resumed = make(chan struct{})
	}
	return nil
}

func (p *fakeProc) Resume() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.paused {
		p.paused = false
		close(p.resumed)
	}
	return nil
}

func (p *fakeProc) isPaused() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.paused
}

func (p *fakeProc) wasKilled() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.killed
}

// publish models the muxer's completed-file handoff. Windows may briefly
// refuse replacement while the polling reader holds the previous playlist;
// never silently discard that update and pretend all segments were published.
func (p *fakeProc) publish(path string, data []byte) bool {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		p.err = fmt.Errorf("fake muxer write: %w", err)
		return false
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		err := os.Rename(tmp, path)
		if err == nil {
			return true
		}
		if runtime.GOOS != "windows" || time.Now().After(deadline) {
			p.err = fmt.Errorf("fake muxer publish: %w", err)
			return false
		}
		select {
		case <-p.kill:
			p.err = errors.New("signal: killed")
			return false
		case <-time.After(time.Millisecond):
		}
	}
}

func (p *fakeProc) simulate(cmd Command, delay time.Duration, lastSeg, failAfter, eofAfter int, failStart bool, stderr string) {
	defer close(p.done)
	args := cmd.Args
	playlist := args[len(args)-1]
	pattern := argValue(args, "-hls_segment_filename")
	start, _ := strconv.Atoi(argValue(args, "-start_number"))
	init := argValue(args, "-hls_fmp4_init_filename")
	if argValue(args, "-segment_format") == "matroska" {
		pattern, playlist = playlist, argValue(args, "-segment_list")
		start, _ = strconv.Atoi(argValue(args, "-segment_start_number"))
		init = filepath.Base(argValue(args, "-segment_header_filename"))
	}
	if failStart {
		if cmd.Stderr != nil {
			_, _ = io.WriteString(cmd.Stderr, stderr+"\n")
		}
		p.err = errors.New("exit status 1")
		return
	}
	if init != "" {
		initPath := init
		if !filepath.IsAbs(initPath) {
			initPath = filepath.Join(filepath.Dir(playlist), initPath)
		}
		b := []byte("init")
		if argValue(args, "-hls_segment_type") == "fmp4" {
			b = testVideoInit(1280, 720) // read for the session's retained clock
		}
		_ = os.WriteFile(initPath, b, 0o644)
	}
	var list strings.Builder
	list.WriteString("#EXTM3U\n#EXT-X-PLAYLIST-TYPE:EVENT\n")
	for n, produced := start, 0; ; n, produced = n+1, produced+1 {
		if lastSeg >= 0 && n > lastSeg {
			list.WriteString("#EXT-X-ENDLIST\n")
			if !p.publish(playlist, []byte(list.String())) {
				return
			}
			return
		}
		if eofAfter > 0 && produced >= eofAfter {
			list.WriteString("#EXT-X-ENDLIST\n")
			if !p.publish(playlist, []byte(list.String())) {
				return
			}
			return
		}
		if failAfter > 0 && produced >= failAfter {
			if cmd.Stderr != nil {
				_, _ = io.WriteString(cmd.Stderr, "http: Stream ends prematurely\n")
			}
			p.err = errors.New("exit status 1")
			return
		}
		p.mu.Lock()
		resumed := p.resumed
		p.mu.Unlock()
		select {
		case <-p.kill:
			p.err = errors.New("signal: killed")
			return
		case <-resumed:
		}
		select {
		case <-p.kill:
			p.err = errors.New("signal: killed")
			return
		case <-time.After(delay):
		}
		name := fmt.Sprintf(pattern, n)
		if !p.publish(name, []byte(fmt.Sprintf("segment %d args-ss=%s input=%s", n, argValue(args, "-ss"), argValue(args, "-i")))) {
			return
		}
		fmt.Fprintf(&list, "#EXTINF:4.000000,\n%s\n", filepath.Base(name))
		if !p.publish(playlist, []byte(list.String())) {
			return
		}
		if cmd.Stdout != nil {
			fmt.Fprintf(cmd.Stdout, "frame=%d\nout_time_us=%d\nspeed=4.0x\nprogress=continue\n", n*96, n*4000000)
		}
	}
}

// fakeChunked reports whether a run's args write chunks (chunks.go).
func fakeChunked(args []string) bool {
	pat := argValue(args, "-hls_segment_filename")
	if argValue(args, "-segment_format") == "matroska" {
		pat = args[len(args)-1]
	}
	return strings.HasPrefix(filepath.Base(filepath.Dir(pat)), "c-")
}

// fakeChunkSeg is the fake source's default keyframe interval: one 4s
// segment, so legacy tests see the segment numbers they expect.
const fakeChunkSeg = 4 * time.Second

// simulateChunks writes a chunked HLS run like ffmpeg's segmenters with a
// tiny segment length (chunks.go): each chunk is a GOP of the given length
// (default fakeChunkSeg) from the keyframe at or before -ss (a remux
// restart reads the keyframe interval before its seek point). The input ends
// at end (or after legacy segment lastSeg); failAfter fails the run after
// that many chunks. Chunks carry real timestamps (MPEG-TS PTS, fMP4 tfdt,
// Matroska Cluster) and a text marker "segment N args-ss=... input=...".
func (p *fakeProc) simulateChunks(cmd Command, delay, gop, end time.Duration, lastSeg, failAfter, eofAfter int, failStart bool, stderr string) {
	defer close(p.done)
	args := cmd.Args
	playlist := args[len(args)-1]
	pattern := argValue(args, "-hls_segment_filename")
	init := ""
	if v := argValue(args, "-hls_fmp4_init_filename"); v != "" {
		init = v
		if !filepath.IsAbs(init) {
			init = filepath.Join(filepath.Dir(playlist), init)
		}
	}
	mkv := argValue(args, "-segment_format") == "matroska"
	if mkv {
		pattern, playlist = playlist, argValue(args, "-segment_list")
		init = argValue(args, "-segment_header_filename")
	}
	if failStart {
		if cmd.Stderr != nil {
			_, _ = io.WriteString(cmd.Stderr, stderr+"\n")
		}
		p.err = errors.New("exit status 1")
		return
	}
	if init != "" {
		_ = os.WriteFile(init, testVideoInit(1280, 720), 0o644)
	}
	ssf, _ := strconv.ParseFloat(argValue(args, "-ss"), 64)
	ss := time.Duration(math.Round(ssf*1000)) * time.Millisecond
	if gop <= 0 {
		gop = fakeChunkSeg
	}
	t := ss / gop * gop
	if t == ss && t > 0 {
		t -= gop // ffmpeg's seek lands on the keyframe before
	}
	if end <= 0 && lastSeg >= 0 {
		end = time.Duration(lastSeg+1) * fakeChunkSeg
	}
	var list strings.Builder
	list.WriteString("#EXTM3U\n#EXT-X-PLAYLIST-TYPE:EVENT\n")
	for n := 0; ; n++ {
		if end > 0 && t >= end {
			list.WriteString("#EXT-X-ENDLIST\n")
			if !p.publish(playlist, []byte(list.String())) {
				return
			}
			return
		}
		if eofAfter > 0 && n >= eofAfter {
			list.WriteString("#EXT-X-ENDLIST\n")
			if !p.publish(playlist, []byte(list.String())) {
				return
			}
			return
		}
		if failAfter > 0 && n >= failAfter {
			if cmd.Stderr != nil {
				_, _ = io.WriteString(cmd.Stderr, "http: Stream ends prematurely\n")
			}
			p.err = errors.New("exit status 1")
			return
		}
		p.mu.Lock()
		resumed := p.resumed
		p.mu.Unlock()
		select {
		case <-p.kill:
			p.err = errors.New("signal: killed")
			return
		case <-resumed:
		}
		select {
		case <-p.kill:
			p.err = errors.New("signal: killed")
			return
		case <-time.After(delay):
		}
		d := gop
		if end > 0 && t+d > end {
			d = end - t
		}
		name := fmt.Sprintf(pattern, n)
		marker := fmt.Sprintf("segment %d args-ss=%s input=%s t=%.3f", int(t/fakeChunkSeg), argValue(args, "-ss"), argValue(args, "-i"), t.Seconds())
		var b []byte
		switch {
		case mkv:
			b = testMKVChunk(t, marker)
		case strings.HasSuffix(name, ".m4s"):
			b = testChunk(uint32(n+1), int64(t*90000/time.Second), 3750, int(d*24/time.Second), []byte(marker))
		default:
			b = testTSChunk(t, marker)
		}
		if !p.publish(name, b) {
			return
		}
		fmt.Fprintf(&list, "#EXTINF:%.6f,\n%s\n", d.Seconds(), filepath.Base(name))
		if !p.publish(playlist, []byte(list.String())) {
			return
		}
		if cmd.Stdout != nil {
			fmt.Fprintf(cmd.Stdout, "frame=%d\nout_time_us=%d\nspeed=4.0x\nprogress=continue\n", n*96, t.Microseconds())
		}
		t += d
	}
}

// testMKVChunk is a Matroska Cluster with the Timestamp the segmented
// Matroska output gives media time t, and a text marker.
func testMKVChunk(t time.Duration, marker string) []byte {
	ms := uint64((t + matroskaOrigin) / time.Millisecond)
	ts := []byte{0xE7, 0x88}
	for i := 7; i >= 0; i-- {
		ts = append(ts, byte(ms>>(8*i)))
	}
	block := append([]byte{0xA3, 0x40 | byte(len(marker)>>8), byte(len(marker))}, marker...)
	body := append(ts, block...)
	return append([]byte{0x1F, 0x43, 0xB6, 0x75, 0x40 | byte(len(body)>>8), byte(len(body))}, body...)
}

// testTSChunk is one MPEG-TS packet: a video PES with the PTS ffmpeg's
// muxer would write for media time t, and a text marker as its payload.
func testTSChunk(t time.Duration, marker string) []byte {
	pkt := make([]byte, 188)
	pkt[0], pkt[1], pkt[2], pkt[3] = 0x47, 0x41, 0x00, 0x10 // PUSI, PID 0x100, payload only
	pts := uint64((t + tsMuxDelay) * 90000 / time.Second)
	pes := []byte{0, 0, 1, 0xE0, 0, 0, 0x80, 0x80, 5,
		byte(0x21 | (pts>>29)&0x0E), byte(pts >> 22), byte((pts>>14)&0xFE | 1), byte(pts >> 7), byte((pts<<1)&0xFE | 1)}
	n := copy(pkt[4:], pes)
	copy(pkt[4+n:], marker)
	return pkt
}

// segText is a fake segment's text: its chunks' markers joined by "|"
// (chunked runs), or the legacy whole-segment payload.
func segText(b []byte) string {
	if len(b) >= 188 && len(b)%188 == 0 && b[0] == 0x47 {
		return strings.Join(tsChunkMarkers(b), "|")
	}
	return string(b)
}

// tsChunkMarkers lists the markers of a fake chunked segment's packets.
func tsChunkMarkers(b []byte) []string {
	var out []string
	for off := 0; off+188 <= len(b); off += 188 {
		m := string(bytes.TrimRight(b[off+18:off+188], "\x00"))
		out = append(out, m)
	}
	return out
}

// simulateDASH writes chunks like ffmpeg's dash muxer: init-<rep>.m4s
// first, then chunk-<rep>-<n>.m4s (from 1, via .tmp and rename), each one
// -seg_duration long with absolute decode times from -ss. Video uses a
// 90 kHz timescale at 24 fps, audio 48 kHz in 20 ms frames.
func (p *fakeProc) simulateDASH(cmd Command, delay, end time.Duration, hold bool, failAfter int, failStart bool, stderr string) {
	defer close(p.done)
	args := cmd.Args
	dir := filepath.Dir(args[len(args)-1])
	if failStart {
		if cmd.Stderr != nil {
			_, _ = io.WriteString(cmd.Stderr, stderr+"\n")
		}
		p.err = errors.New("exit status 1")
		return
	}
	ss, _ := strconv.ParseFloat(argValue(args, "-ss"), 64)
	chunk, _ := strconv.ParseFloat(argValue(args, "-seg_duration"), 64)
	if chunk <= 0 {
		chunk = 1
	}
	var kinds []string
	for _, set := range strings.Fields(argValue(args, "-adaptation_sets")) {
		if strings.HasSuffix(set, "streams=a") {
			kinds = append(kinds, "audio")
		} else {
			kinds = append(kinds, "video")
		}
	}
	for rep, k := range kinds {
		init := testVideoInit(1280, 720)
		if k == "audio" {
			init = testAudioInit(2, 48000)
		}
		_ = os.WriteFile(filepath.Join(dir, dashInitFile(rep)), init, 0o644)
	}
	for n := 1; ; n++ {
		t := ss + float64(n-1)*chunk
		if end > 0 && t >= end.Seconds()-1e-9 {
			if hold {
				<-p.kill
			}
			return // end of input
		}
		if failAfter > 0 && n > failAfter {
			if cmd.Stderr != nil {
				_, _ = io.WriteString(cmd.Stderr, "http: Stream ends prematurely\n")
			}
			p.err = errors.New("exit status 1")
			return
		}
		p.mu.Lock()
		resumed := p.resumed
		p.mu.Unlock()
		select {
		case <-p.kill:
			p.err = errors.New("signal: killed")
			return
		case <-resumed:
		}
		select {
		case <-p.kill:
			p.err = errors.New("signal: killed")
			return
		case <-time.After(delay):
		}
		for rep, k := range kinds {
			payload := []byte(fmt.Sprintf("[rep=%d chunk=%d t=%.3f ss=%s input=%s]", rep, n, t, argValue(args, "-ss"), argValue(args, "-i")))
			var b []byte
			if k == "audio" {
				b = testChunk(uint32(n), int64(math.Round(t*48000)), 960, int(math.Round(chunk*50)), payload)
			} else {
				b = testChunk(uint32(n), int64(math.Round(t*90000)), 3750, int(math.Round(chunk*24)), payload)
			}
			name := filepath.Join(dir, dashChunkFile(rep, n))
			if !p.publish(name, b) {
				return
			}
		}
		if cmd.Stdout != nil {
			fmt.Fprintf(cmd.Stdout, "frame=%d\nout_time_us=%d\nspeed=4.0x\nprogress=continue\n", n*24, int64(t*1e6))
		}
	}
}

// newTestManager builds a Manager on the fake runner with dummy tool files.
func newTestManager(t *testing.T, fr *fakeRunner, cfg config.Transcode, opts ...Option) *Manager {
	t.Helper()
	dir := t.TempDir()
	ff := filepath.Join(dir, "ffmpeg")
	fp := filepath.Join(dir, "ffprobe")
	for _, p := range []string{ff, fp} {
		if err := os.WriteFile(p, []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	cfg.FFmpegPath, cfg.FFprobePath = ff, fp
	if cfg.TempDir == "" {
		cfg.TempDir = filepath.Join(dir, "tmp")
	}
	all := append([]Option{
		WithRunner(fr),
		withFinder(finder{goos: "darwin", isExec: isExecutable}),
		WithBuffering(24*time.Second, 0, 0),
		withPollInterval(5 * time.Millisecond),
		// Generous: a race-instrumented run on a busy CI runner can take
		// several seconds per segment. Tests that need a timeout set their own.
		WithSegmentTimeout(30 * time.Second),
		withRungDwell(0), // rung switches are immediate unless a test opts in
	}, opts...)
	m, err := New(cfg, testLogger(t), all...)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = m.Close() })
	return m
}

func withPollInterval(d time.Duration) Option { return func(o *options) { o.pollInterval = d } }

type testWriter struct{ t *testing.T }

func (w testWriter) Write(b []byte) (int, error) {
	w.t.Helper()
	w.t.Log(strings.TrimRight(string(b), "\n"))
	return len(b), nil
}

func testLogger(t *testing.T) *slog.Logger {
	if testing.Verbose() {
		return slog.New(slog.NewTextHandler(testWriter{t}, &slog.HandlerOptions{Level: slog.LevelDebug}))
	}
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
