package transcode

import (
	"bytes"
	"context"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
)

// Keyframe-planned copy sessions: every segment the playlist advertises
// must be exactly the media ffmpeg produced for it, on the first run and
// after a mid-session restart. Fixtures have sparse (10s) and uneven
// (2.5s against 4s segments) keyframes, where ffmpeg's own segmenter cuts
// elsewhere (15 segments of 5,5,2.5,... or 7 segments for 60s).

// keyframeIndex reads a file's video keyframes the way the container index
// would list them.
func (e *realEnv) keyframeIndex(t *testing.T, name string) []core.KeyframePoint {
	t.Helper()
	out, err := exec.Command(e.ffprobe, "-v", "error", "-select_streams", "v:0", "-show_entries",
		"packet=pts_time,pos,flags", "-of", "csv=p=0", filepath.Join(e.dir, name)).Output()
	if err != nil {
		t.Fatalf("ffprobe keyframes: %v", err)
	}
	var kfs []core.KeyframePoint
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		f := strings.Split(line, ",")
		if len(f) < 3 || !strings.HasPrefix(f[2], "K") {
			continue
		}
		pts, err1 := strconv.ParseFloat(f[0], 64)
		pos, err2 := strconv.ParseInt(f[1], 10, 64)
		if err1 != nil || err2 != nil {
			continue
		}
		kfs = append(kfs, core.KeyframePoint{Offset: pos, Time: time.Duration(pts * float64(time.Second))})
	}
	if len(kfs) < 2 {
		t.Fatalf("keyframe index of %s: %v", name, kfs)
	}
	return kfs
}

// planStarts is the media start of every playlist segment (and the end of
// the last one), from the served playlist's #EXTINF durations.
func planStarts(t *testing.T, s core.HLSSession, base time.Duration) []float64 {
	t.Helper()
	rec := httptest.NewRecorder()
	s.ServePlaylist(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	at := base.Seconds()
	starts := []float64{at}
	for _, line := range strings.Split(rec.Body.String(), "\n") {
		if v, ok := strings.CutPrefix(line, "#EXTINF:"); ok {
			d, err := strconv.ParseFloat(strings.TrimSuffix(v, ","), 64)
			if err != nil {
				t.Fatalf("playlist line %q", line)
			}
			at += d
			starts = append(starts, at)
		}
	}
	return starts
}

// videoSpan is the media time of a served segment's first video frame and
// the end of its last one.
func (e *realEnv) videoSpan(t *testing.T, container string, init, seg []byte, name string) (float64, float64) {
	t.Helper()
	first, end, _ := e.videoSpanKey(t, container, init, seg, name)
	return first, end
}

// videoSpanKey is videoSpan plus whether the segment's first video packet
// is a keyframe.
func (e *realEnv) videoSpanKey(t *testing.T, container string, init, seg []byte, name string) (float64, float64, bool) {
	t.Helper()
	file := e.writeTemp(t, name+"."+segmentExt(container), append(append([]byte(nil), init...), seg...))
	out, err := exec.Command(e.ffprobe, "-v", "error", "-select_streams", "v:0", "-show_entries",
		"packet=pts_time,duration_time,flags", "-of", "csv=p=0", file).Output()
	if err != nil {
		t.Fatalf("ffprobe %s: %v", name, err)
	}
	first, last := math.Inf(1), math.Inf(-1)
	key := false
	for i, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		f := strings.Split(line, ",")
		if i == 0 && len(f) > 2 {
			key = strings.HasPrefix(f[2], "K")
		}
		pts, err := strconv.ParseFloat(f[0], 64)
		if err != nil {
			continue
		}
		dur := 1.0 / 24
		if len(f) > 1 {
			if d, err := strconv.ParseFloat(f[1], 64); err == nil && d > 0 {
				dur = d
			}
		}
		first = math.Min(first, pts)
		last = math.Max(last, pts+dur)
	}
	if math.IsInf(first, 0) {
		t.Fatalf("%s has no video", name)
	}
	shift := 0.0
	switch container {
	case ContainerMPEGTS:
		shift = tsMuxDelay.Seconds()
	case ContainerMatroska:
		shift = matroskaOrigin.Seconds()
	}
	return first - shift, last - shift, key
}

// joinedMonotonic decodes a joined Matroska stream and requires its video
// frames to never go backwards.
func (e *realEnv) joinedMonotonic(t *testing.T, file string) {
	t.Helper()
	cmd := exec.Command(e.ffprobe, "-v", "error", "-select_streams", "v:0", "-show_entries", "frame=best_effort_timestamp_time", "-of", "csv=p=0", file)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil || stderr.Len() > 0 {
		t.Fatalf("joined stream: %v %s", err, stderr.String())
	}
	last := -1.0
	for _, line := range strings.Fields(string(out)) {
		at, err := strconv.ParseFloat(strings.TrimSuffix(line, ","), 64)
		if err != nil {
			continue
		}
		if at < last {
			t.Fatalf("joined stream runs backwards: %.3f -> %.3f", last, at)
		}
		last = at
	}
}

// decodesAlone requires a segment (with its init segment) to start on a
// keyframe and decode without errors, as a player's fresh decoder must
// after a seek.
func (e *realEnv) decodesAlone(t *testing.T, container string, init, seg []byte, name string) {
	t.Helper()
	if _, _, key := e.videoSpanKey(t, container, init, seg, name+"-key"); !key {
		t.Errorf("%s does not start on a keyframe", name)
	}
	// ffprobe decodes every frame for -show_frames and reports decode
	// errors (e.g. "non-existing PPS referenced" mid-GOP) on stderr.
	file := e.writeTemp(t, name+"-decode."+segmentExt(container), append(append([]byte(nil), init...), seg...))
	cmd := exec.Command(e.ffprobe, "-v", "error", "-select_streams", "v:0", "-show_entries", "frame=key_frame", "-of", "csv=p=0", file)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	frames := strings.Fields(string(out))
	if err != nil || stderr.Len() > 0 || len(frames) == 0 || strings.TrimSuffix(frames[0], ",") != "1" {
		t.Errorf("%s does not decode on its own from a keyframe: %v %d frames (first %q) %s", name, err, len(frames), firstOf(frames), stderr.String())
	}
}

func firstOf(s []string) string {
	if len(s) == 0 {
		return ""
	}
	return s[0]
}

// checkSegments fetches segments from..to-1 and requires each to hold
// exactly its playlist span.
func (e *realEnv) checkSegments(t *testing.T, hs core.HLSSession, container string, starts []float64, from, to int, label string) {
	t.Helper()
	e.checkSegmentsWith(t, hs, container, nil, starts, from, to, label)
}

// checkSegmentsWith checks segments against a fixed init segment (the one
// a player fetched first) when init is set.
func (e *realEnv) checkSegmentsWith(t *testing.T, hs core.HLSSession, container string, init0 []byte, starts []float64, from, to int, label string) {
	t.Helper()
	const tol = 0.1 // start_at_zero shifts by the audio priming (21ms)
	for n := from; n < to; n++ {
		seg := fetch(t, hs, n)
		init := init0
		if init == nil && hasInitSegment(container) {
			init = fetch(t, hs, InitSegment)
		}
		first, end := e.videoSpan(t, container, init, seg, fmt.Sprintf("%s-%d", label, n))
		if n == from {
			// The first segment a player fetches after a seek or restart.
			e.decodesAlone(t, container, init, seg, fmt.Sprintf("%s-%d", label, n))
		}
		wantEnd := starts[n+1]
		if n == len(starts)-2 {
			wantEnd = math.Min(wantEnd, end) // the source may end a frame early
		}
		if n > 0 && (math.Abs(first-starts[n]) > tol || math.Abs(end-wantEnd) > tol) {
			t.Errorf("%s segment %d holds video %.3f-%.3f, playlist says %.3f-%.3f", label, n, first, end, starts[n], starts[n+1])
		}
		if n == 0 && (first > starts[0]+tol || math.Abs(end-wantEnd) > tol) {
			t.Errorf("%s segment 0 holds video %.3f-%.3f, playlist says %.3f-%.3f", label, first, end, starts[0], starts[1])
		}
	}
}

func TestIntegrationKeyframePlannedCopySegmentsMatchPlaylist(t *testing.T) {
	e := newRealEnv(t, WithoutHWDetection(), WithBuffering(4*time.Second, 0, 0))
	for _, fx := range []struct {
		name string
		gop  int // frames at 24 fps
	}{{"sparse-10s.mkv", 240}, {"uneven-2500ms.mkv", 60}} {
		g := strconv.Itoa(fx.gop)
		e.gen(t, fx.name, 60, "-c:v", "libx264", "-preset", "ultrafast", "-g", g, "-keyint_min", g, "-sc_threshold", "0", "-c:a", "aac")
	}
	for _, fx := range []string{"sparse-10s.mkv", "uneven-2500ms.mkv"} {
		info, err := e.m.Probe(t.Context(), e.url(fx))
		if err != nil {
			t.Fatal(err)
		}
		info.Keyframes = e.keyframeIndex(t, fx)
		for _, container := range []string{ContainerMPEGTS, ContainerFMP4, ContainerMatroska} {
			label := strings.TrimSuffix(fx, ".mkv") + "-" + container
			t.Run(label, func(t *testing.T) {
				d := core.Decision{Kind: core.DecisionRemux, Container: container, VideoCopy: true, AudioCopy: true, AudioStream: 1}
				hs, err := e.m.StartHLS(context.Background(), core.HLSRequest{SessionID: label, InputURL: e.url(fx), Info: *info, Decision: d})
				if err != nil {
					t.Fatal(err)
				}
				defer hs.Close()
				s := hs.(*Session)
				if !s.plan.Keyframed() {
					t.Fatal("plan not built from the keyframe index")
				}
				starts := planStarts(t, hs, 0)
				total := len(starts) - 1
				// Players fetch the init segment once (one EXT-X-MAP): every
				// later run's segments must decode at their media time
				// against it.
				var init0 []byte
				if hasInitSegment(container) {
					init0 = fetch(t, hs, InitSegment)
				}
				for n, at := range starts[1 : len(starts)-1] {
					onKey := false
					for _, k := range info.Keyframes {
						onKey = onKey || math.Abs(k.Time.Seconds()-at) < 0.002
					}
					if !onKey {
						t.Fatalf("boundary %d at %.3f is not a keyframe", n+1, at)
					}
				}
				t.Logf("plan: %d segments %v", total, starts)
				// First run, in order.
				e.checkSegments(t, hs, container, starts, 0, total, label+"-first")
				s.mu.Lock()
				for n := range s.done[0] {
					if n >= total {
						t.Errorf("segment %d produced past the playlist's %d", n, total)
					}
				}
				s.mu.Unlock()

				// A mid-session restart (seek, failover): discard from the
				// middle and restart ffmpeg there, as serveSegment does for a
				// request behind or far ahead of the encoder.
				mid := total / 2
				s.mu.Lock()
				for n := range s.done[0] {
					if n >= mid-1 {
						delete(s.done[0], n)
					}
				}
				err = s.startRunLocked(mid, false)
				s.mu.Unlock()
				if err != nil {
					t.Fatal(err)
				}
				e.checkSegmentsWith(t, hs, container, init0, starts, mid, total, label+"-restart")
				// Behind the restart point: another restart.
				e.checkSegmentsWith(t, hs, container, init0, starts, mid-1, mid, label+"-behind")

				// Failover to the same release from inside a segment.
				at := time.Duration((starts[1] + 0.5) * float64(time.Second))
				if err := hs.SwapInput(context.Background(), e.url(fx), *info, at); err != nil {
					t.Fatal(err)
				}
				e.checkSegmentsWith(t, hs, container, init0, starts, 1, total, label+"-swap")
			})
		}
	}
}

func TestIntegrationKeyframePlannedCopyAtStartOffset(t *testing.T) {
	e := newRealEnv(t, WithoutHWDetection(), WithBuffering(4*time.Second, 0, 0))
	e.gen(t, "offset.mkv", 40, "-c:v", "libx264", "-preset", "ultrafast", "-g", "60", "-keyint_min", "60", "-sc_threshold", "0", "-c:a", "aac")
	info, err := e.m.Probe(t.Context(), e.url("offset.mkv"))
	if err != nil {
		t.Fatal(err)
	}
	info.Keyframes = e.keyframeIndex(t, "offset.mkv")
	d := core.Decision{Kind: core.DecisionRemux, Container: ContainerMPEGTS, VideoCopy: true, AudioCopy: true, AudioStream: 1}
	base := 11 * time.Second // not a keyframe: the copy starts at 10s
	hs, err := e.m.StartHLS(context.Background(), core.HLSRequest{SessionID: "offset", InputURL: e.url("offset.mkv"),
		Info: *info, Decision: d, StartOffset: base})
	if err != nil {
		t.Fatal(err)
	}
	defer hs.Close()
	starts := planStarts(t, hs, base)
	e.checkSegments(t, hs, ContainerMPEGTS, starts, 0, len(starts)-1, "offset")
	seg0 := fetch(t, hs, 0)
	if !bytes.HasPrefix(seg0, []byte{0x47}) {
		t.Fatal("segment 0 is not MPEG-TS")
	}
}

// Encoded runs on a keyframe plan (the fast-start ramp of a TS copy, a
// governor clamp, a ladder rung, a failover that must re-encode) force
// their keyframes at the plan's boundaries and are regrouped the same way.
func TestIntegrationKeyframePlannedEncodedRuns(t *testing.T) {
	e := newRealEnv(t, WithoutHWDetection(), WithBuffering(4*time.Second, 0, 0))
	e.gen(t, "kf-360.mkv", 40, "-c:v", "libx264", "-preset", "ultrafast", "-g", "60", "-keyint_min", "60", "-sc_threshold", "0", "-c:a", "aac")
	e.gen(t, "kf-480.mkv", 40, "-vf", "scale=854:480", "-c:v", "libx264", "-preset", "ultrafast", "-g", "240", "-keyint_min", "240", "-sc_threshold", "0", "-c:a", "aac")
	probe := func(name string) core.MediaInfo {
		info, err := e.m.Probe(t.Context(), e.url(name))
		if err != nil {
			t.Fatal(err)
		}
		info.Keyframes = e.keyframeIndex(t, name)
		return *info
	}
	small, big := probe("kf-360.mkv"), probe("kf-480.mkv")
	copyTS := core.Decision{Kind: core.DecisionRemux, Container: ContainerMPEGTS, VideoCopy: true, AudioCopy: true, AudioStream: 1}
	start := func(t *testing.T, id, name string, info core.MediaInfo) (*Session, []float64) {
		t.Helper()
		hs, err := e.m.StartHLS(context.Background(), core.HLSRequest{SessionID: id, InputURL: e.url(name), Info: info, Decision: copyTS})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = hs.Close() })
		return hs.(*Session), planStarts(t, hs, 0)
	}

	t.Run("fast-start ramp", func(t *testing.T) {
		e.m.UpdateConfig(fastStartConfig())
		defer e.m.UpdateConfig(config.Transcode{})
		for _, fx := range []struct {
			name string
			info core.MediaInfo
		}{{"kf-360.mkv", small}, {"kf-480.mkv", big}} {
			s, starts := start(t, "ramp-"+fx.name, fx.name, fx.info)
			if sw := s.plan.SwitchSeg; sw < 2 || s.plan.ModeAt(sw-1) != ModeFastStart || s.plan.ModeAt(sw) != ModeCopy {
				t.Fatalf("plan: switch %d modes %v %v", s.plan.SwitchSeg, s.plan.ModeAt(0), s.plan.ModeAt(2))
			}
			e.checkSegments(t, s, ContainerMPEGTS, starts, 0, len(starts)-1, "ramp-"+fx.name)
		}
	})

	t.Run("governor clamp", func(t *testing.T) {
		s, starts := start(t, "clamp", "kf-360.mkv", small)
		_ = fetch(t, s, 0)
		s.mu.Lock()
		s.plan.SetClamp(2, 600)
		s.playlist = nil
		for n := range s.done[0] {
			if n >= 1 {
				delete(s.done[0], n)
			}
		}
		err := s.startRunLocked(1, false)
		s.mu.Unlock()
		if err != nil {
			t.Fatal(err)
		}
		e.checkSegments(t, s, ContainerMPEGTS, starts, 1, len(starts)-1, "clamp")
	})

	t.Run("ladder rung", func(t *testing.T) {
		cfg := config.Transcode{ABRLadder: true, ABRLadderUnderCopy: true}
		e.m.UpdateConfig(cfg)
		defer e.m.UpdateConfig(config.Transcode{})
		s, starts := start(t, "rung", "kf-480.mkv", big)
		if len(s.rungs) < 2 {
			t.Skipf("no ladder rung below the 480p fixture: %d", len(s.rungs))
		}
		id := s.rungs[len(s.rungs)-1].id
		for n := 0; n < len(starts)-1; n++ {
			rec := httptest.NewRecorder()
			s.ServeRungSegment(rec, httptest.NewRequest(http.MethodGet, "/", nil), id, n)
			if rec.Code != 200 {
				t.Fatalf("rung segment %d: %d %s", n, rec.Code, rec.Body.String())
			}
			first, end := e.videoSpan(t, ContainerMPEGTS, nil, rec.Body.Bytes(), fmt.Sprintf("rung-%d", n))
			if (n > 0 && math.Abs(first-starts[n]) > 0.1) || math.Abs(end-math.Min(starts[n+1], end)) > 0.1 || end < starts[n+1]-0.1 {
				t.Errorf("rung segment %d holds video %.3f-%.3f, playlist says %.3f-%.3f", n, first, end, starts[n], starts[n+1])
			}
		}
	})

	t.Run("failover re-encodes", func(t *testing.T) {
		s, starts := start(t, "swap-encode", "kf-360.mkv", small)
		_ = fetch(t, s, 0)
		// A taller release cannot be copied into this stream: the rest of
		// the session is a transcode on the 360p source's keyframe plan.
		at := time.Duration(starts[2]*float64(time.Second)) + time.Second
		if err := s.SwapInput(context.Background(), e.url("kf-480.mkv"), big, at); err != nil {
			t.Fatal(err)
		}
		if d := s.Decision(); d.VideoCopy {
			t.Fatalf("decision after swap still copies: %+v", d)
		}
		e.checkSegments(t, s, ContainerMPEGTS, starts, 2, len(starts)-1, "swap-encode")
	})
}

// Without a keyframe index a restarted fMP4 run's fragments must still
// carry media decode times: the player keeps the first run's init segment
// (one EXT-X-MAP), so run-relative times would place them at the start.
func TestIntegrationFMP4RestartKeepsMediaTime(t *testing.T) {
	e := newRealEnv(t, WithoutHWDetection(), WithBuffering(4*time.Second, 0, 0))
	e.gen(t, "gop1s.mkv", 40, "-c:v", "libx264", "-preset", "ultrafast", "-g", "24", "-keyint_min", "24", "-sc_threshold", "0", "-c:a", "aac")
	info, err := e.m.Probe(t.Context(), e.url("gop1s.mkv"))
	if err != nil {
		t.Fatal(err)
	}
	d := core.Decision{Kind: core.DecisionRemux, Container: ContainerFMP4, VideoCopy: true, AudioCopy: true, AudioStream: 1}
	hs, err := e.m.StartHLS(context.Background(), core.HLSRequest{SessionID: "fmp4-restart", InputURL: e.url("gop1s.mkv"), Info: *info, Decision: d})
	if err != nil {
		t.Fatal(err)
	}
	defer hs.Close()
	s := hs.(*Session)
	init0 := fetch(t, hs, InitSegment)
	_ = fetch(t, hs, 1)
	s.mu.Lock()
	for n := range s.done[0] {
		if n >= 5 {
			delete(s.done[0], n)
		}
	}
	err = s.startRunLocked(5, false)
	s.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	first, _ := e.videoSpan(t, ContainerFMP4, init0, fetch(t, hs, 6), "fmp4-restart-6")
	if math.Abs(first-24) > 1.1 {
		t.Errorf("segment 6 of a restarted run decodes at %.3f against the first init, want ~24", first)
	}
}

// An index that stops early (callers cap stored indexes) leaves nominal
// boundaries past its end: segments there follow the source's real
// keyframes, so they may differ from #EXTINF by up to a GOP, but every
// advertised segment exists and they join without gaps or overlaps.
func TestIntegrationKeyframePlannedTruncatedIndex(t *testing.T) {
	e := newRealEnv(t, WithoutHWDetection(), WithBuffering(4*time.Second, 0, 0))
	e.gen(t, "trunc.mkv", 50, "-c:v", "libx264", "-preset", "ultrafast", "-g", "60", "-keyint_min", "60", "-sc_threshold", "0", "-c:a", "aac")
	info, err := e.m.Probe(t.Context(), e.url("trunc.mkv"))
	if err != nil {
		t.Fatal(err)
	}
	kfs := e.keyframeIndex(t, "trunc.mkv")
	info.Keyframes = kfs[:len(kfs)/3] // up to ~15s
	d := core.Decision{Kind: core.DecisionRemux, Container: ContainerMPEGTS, VideoCopy: true, AudioCopy: true, AudioStream: 1}
	hs, err := e.m.StartHLS(context.Background(), core.HLSRequest{SessionID: "trunc", InputURL: e.url("trunc.mkv"), Info: *info, Decision: d})
	if err != nil {
		t.Fatal(err)
	}
	defer hs.Close()
	starts := planStarts(t, hs, 0)
	prevEnd := 0.0
	for n := 0; n < len(starts)-1; n++ {
		first, end := e.videoSpan(t, ContainerMPEGTS, nil, fetch(t, hs, n), fmt.Sprintf("trunc-%d", n))
		if n > 0 && math.Abs(first-prevEnd) > 0.1 {
			t.Errorf("segment %d starts at %.3f, previous ended %.3f", n, first, prevEnd)
		}
		if math.Abs(first-starts[n]) > 2.6 {
			t.Errorf("segment %d starts at %.3f, playlist %.3f", n, first, starts[n])
		}
		prevEnd = end
	}
	if math.Abs(prevEnd-starts[len(starts)-1]) > 0.1 {
		t.Errorf("last segment ends %.3f, title %.3f", prevEnd, starts[len(starts)-1])
	}
}

// Without a keyframe index the plan is nominal. A copy whose GOPs are
// longer than a segment must still produce every advertised segment, each
// starting on a keyframe at or before its playlist position (inside a long
// GOP, the GOP covering it), on the first run and after restarts.
func TestIntegrationCopyWithoutKeyframeIndex(t *testing.T) {
	e := newRealEnv(t, WithoutHWDetection(), WithBuffering(4*time.Second, 0, 0))
	e.gen(t, "noindex-10s.mkv", 60, "-c:v", "libx264", "-preset", "ultrafast", "-g", "240", "-keyint_min", "240", "-sc_threshold", "0", "-c:a", "aac")
	info, err := e.m.Probe(t.Context(), e.url("noindex-10s.mkv"))
	if err != nil {
		t.Fatal(err)
	}
	if len(info.Keyframes) != 0 {
		t.Fatal("probe produced a keyframe index; this test needs none")
	}
	for _, container := range []string{ContainerMPEGTS, ContainerFMP4, ContainerMatroska} {
		t.Run(container, func(t *testing.T) {
			d := core.Decision{Kind: core.DecisionRemux, Container: container, VideoCopy: true, AudioCopy: true, AudioStream: 1}
			hs, err := e.m.StartHLS(context.Background(), core.HLSRequest{SessionID: "noindex-" + container, InputURL: e.url("noindex-10s.mkv"), Info: *info, Decision: d})
			if err != nil {
				t.Fatal(err)
			}
			defer hs.Close()
			s := hs.(*Session)
			starts := planStarts(t, hs, 0)
			total := len(starts) - 1
			if total != 15 {
				t.Fatalf("nominal plan has %d segments, want 15", total)
			}
			var init0 []byte
			if hasInitSegment(container) {
				init0 = fetch(t, hs, InitSegment)
			}
			// Each segment holds the GOPs starting in its range; one inside
			// a GOP longer than a segment repeats that GOP (TS, fMP4) or is
			// an empty Cluster (Matroska). Every segment with video starts on
			// a keyframe and decodes alone, and together they cover the
			// title from the first segment fetched without gaps.
			check := func(from, to int, label string) {
				t.Helper()
				covered := -1.0
				for n := from; n < to; n++ {
					seg := fetch(t, hs, n)
					name := fmt.Sprintf("%s-%s-%d", label, container, n)
					if container == ContainerMatroska && len(seg) < 64 {
						continue // an empty Cluster inside a long GOP
					}
					first, end := e.videoSpan(t, container, init0, seg, name)
					// A restart that continues playback starts its first segment
					// at the next GOP when the one before is already published.
					if first < starts[n]-10.1 || first > starts[n+1]+10.1 {
						t.Errorf("%s holds video %.3f-%.3f, playlist says %.3f-%.3f", name, first, end, starts[n], starts[n+1])
					}
					if covered >= 0 && first > covered+0.1 {
						t.Errorf("%s starts at %.3f: gap after %.3f", name, first, covered)
					}
					e.decodesAlone(t, container, init0, seg, name)
					covered = math.Max(covered, end)
				}
				if covered < starts[total]-0.1 {
					t.Errorf("%s segments cover up to %.3f of %.3f", label, covered, starts[total])
				}
			}
			check(0, total, "first")
			if container == ContainerMatroska {
				// Plex joins the header and successive fragments into one
				// stream: in order, it must decode and never run backwards.
				joined := append([]byte(nil), init0...)
				for n := 0; n < total; n++ {
					joined = append(joined, fetch(t, hs, n)...)
				}
				e.joinedMonotonic(t, e.writeTemp(t, "noindex-joined.mkv", joined))
			}
			// Seeks with every segment cached: a fresh client asks for a
			// segment inside a GOP (an alias) or starting one; what it gets
			// must decode on its own from a keyframe.
			for _, n := range []int{8, 3, 9, 12, 1, 14, 6} {
				seg := fetch(t, hs, n)
				name := fmt.Sprintf("seek-%s-%d", container, n)
				e.decodesAlone(t, container, init0, seg, name)
				if retry := fetch(t, hs, n); !bytes.Equal(retry, seg) {
					t.Fatalf("%s retry changed bytes: %d -> %d", name, len(seg), len(retry))
				}
				// A connection interrupted halfway through the seek response
				// resumes the same GOP, even for a cached Matroska alias.
				offset := len(seg) / 2
				req := httptest.NewRequest(http.MethodGet, "/segment", nil)
				req.Header.Set("Range", fmt.Sprintf("bytes=%d-", offset))
				rec := httptest.NewRecorder()
				hs.ServeSegment(rec, req, n)
				if rec.Code != http.StatusPartialContent || !bytes.Equal(rec.Body.Bytes(), seg[offset:]) {
					t.Fatalf("%s range retry: status=%d bytes=%d", name, rec.Code, rec.Body.Len())
				}
				if first, end := e.videoSpan(t, container, init0, seg, name); first < starts[n]-10.1 || first > starts[n+1]+0.1 || end <= starts[n] {
					t.Errorf("%s holds video %.3f-%.3f, playlist says %.3f-%.3f", name, first, end, starts[n], starts[n+1])
				}
			}
			if container == ContainerMatroska {
				_ = fetch(t, hs, 14)
				head := httptest.NewRecorder()
				hs.ServeSegment(head, httptest.NewRequest(http.MethodHead, "/0.mkv", nil), 0)
				if head.Code != http.StatusOK || head.Body.Len() != 0 {
					t.Fatalf("HEAD segment: %d bytes=%d", head.Code, head.Body.Len())
				}
				e.decodesAlone(t, container, init0, fetch(t, hs, 1), "after-head")
				_ = fetch(t, hs, 0)
				init1 := fetch(t, hs, InitSegment)
				e.decodesAlone(t, container, init1, fetch(t, hs, 1), "after-new-init")
			}
			s.mu.Lock()
			for n := range s.done[0] {
				if n >= total {
					t.Errorf("segment %d produced past the playlist's %d", n, total)
				}
			}
			mid := 8 // inside the 40-50s GOP: the restart seeks back to 40s
			for n := range s.done[0] {
				if n >= mid {
					delete(s.done[0], n)
				}
			}
			err = s.startRunLocked(mid, false)
			s.mu.Unlock()
			if err != nil {
				t.Fatal(err)
			}
			check(mid, total, "restart")
			// A failover inside a GOP whose start is already published: the
			// new run must not repeat that GOP (Plex's joined Matroska
			// stream would run backwards).
			if err := hs.SwapInput(context.Background(), e.url("noindex-10s.mkv"), *info, 25*time.Second); err != nil {
				t.Fatal(err)
			}
			check(6, total, "swap")
			if container == ContainerMatroska {
				joined := append([]byte(nil), init0...)
				for n := 0; n < total; n++ {
					joined = append(joined, fetch(t, hs, n)...)
				}
				e.joinedMonotonic(t, e.writeTemp(t, "noindex-swap-joined.mkv", joined))
			}
		})
	}
}
