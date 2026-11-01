package transcode

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
)

func TestTimelineUniformMatchesHistoric(t *testing.T) {
	for _, span := range []time.Duration{10 * time.Minute, 8*time.Second + 900*time.Millisecond, 9 * time.Second, time.Second, 61 * time.Second} {
		tl := buildTimeline(span, 4*time.Second, timelineOptions{steadyMode: ModeCopy})
		if got, want := tl.Total(), segmentCount(span, 4*time.Second); got != want {
			t.Errorf("span %v: total = %d, want %d", span, got, want)
		}
		for i := 0; i < tl.Total(); i++ {
			if got, want := tl.Dur(i), segmentLength(i, tl.Total(), span, 4*time.Second); got != want {
				t.Errorf("span %v seg %d: dur = %v, want %v", span, i, got, want)
			}
			if got, want := tl.Start(i), time.Duration(i)*4*time.Second; i < tl.Total()-1 && got != want {
				t.Errorf("span %v seg %d: start = %v, want %v", span, i, got, want)
			}
			if tl.ModeAt(i) != ModeCopy {
				t.Errorf("span %v seg %d: mode = %v", span, i, tl.ModeAt(i))
			}
		}
	}
}

func TestRemuxAudioTranscodeUsesShortOpeningSegments(t *testing.T) {
	adapt := adaptiveFromConfig(config.Transcode{FastStart: true, FastStartSegment: config.Duration(2 * time.Second)})
	v := &core.StreamInfo{Codec: "hevc", Height: 1080}
	d := core.Decision{Kind: core.DecisionRemux, VideoCopy: true, AudioCopy: false, Container: ContainerMatroska}
	if got := remuxOpenSegment(adapt, d, v); got != 2*time.Second {
		t.Fatalf("remux+audio transcode open = %v, want 2s", got)
	}
	d.AudioCopy = true
	if got := remuxOpenSegment(adapt, d, v); got != 0 {
		t.Fatalf("pure copy remux must keep 4s segments (long-GOP replay), got %v", got)
	}
	d.AudioCopy = false
	d.Container = ContainerMPEGTS
	if got := remuxOpenSegment(adapt, d, v); got != 0 {
		t.Fatalf("MPEG-TS remux keeps the encoded FastStart ramp, got %v", got)
	}
	adapt.FastStart = false
	d.Container = ContainerMatroska
	if got := remuxOpenSegment(adapt, d, v); got != 0 {
		t.Fatalf("FastStart off must not change remux segments, got %v", got)
	}
}

func TestTimelineFastStartRamp(t *testing.T) {
	tl := buildTimeline(10*time.Minute, 4*time.Second, timelineOptions{
		steadyMode: ModeCopy, fastStart: true, rampSegs: 2, rampDur: 2 * time.Second,
	})
	if tl.SwitchSeg != 2 {
		t.Fatalf("SwitchSeg = %d", tl.SwitchSeg)
	}
	if tl.Dur(0) != 2*time.Second || tl.Dur(1) != 2*time.Second || tl.Dur(2) != 4*time.Second {
		t.Errorf("durations %v %v %v", tl.Dur(0), tl.Dur(1), tl.Dur(2))
	}
	if tl.Start(2) != 4*time.Second || tl.Start(3) != 8*time.Second {
		t.Errorf("starts %v %v", tl.Start(2), tl.Start(3))
	}
	if tl.ModeAt(0) != ModeFastStart || tl.ModeAt(2) != ModeCopy {
		t.Errorf("modes %v %v", tl.ModeAt(0), tl.ModeAt(2))
	}
	if tl.TargetDuration() != 4*time.Second {
		t.Errorf("target = %v", tl.TargetDuration())
	}
	if got := tl.Total(); got != 151 { // 2 + 596/4
		t.Errorf("total = %d", got)
	}
	// Seeks land in the right segment on both sides of the ramp.
	for at, want := range map[time.Duration]int{time.Second: 0, 3 * time.Second: 1, 4 * time.Second: 2, 41 * time.Second: 11} {
		if got := tl.IndexAt(at); got != want {
			t.Errorf("IndexAt(%v) = %d, want %d", at, got, want)
		}
	}
	if got := tl.NextChange(0); got != 2 {
		t.Errorf("NextChange(0) = %d", got)
	}
	if got := tl.NextChange(2); got != -1 {
		t.Errorf("NextChange(2) = %d", got)
	}
}

func TestTimelineKeyframeSnap(t *testing.T) {
	var kfs []core.KeyframePoint
	for tm := time.Duration(0); tm < time.Minute; tm += 2500 * time.Millisecond {
		kfs = append(kfs, core.KeyframePoint{Offset: int64(tm), Time: tm})
	}
	tl := buildTimeline(time.Minute, 4*time.Second, timelineOptions{
		steadyMode: ModeCopy, snap: true, keyframes: kfs,
	})
	// Nominal boundary 4s snaps to the next keyframe at 5s.
	if got := tl.Start(1); got != 5*time.Second {
		t.Errorf("Start(1) = %v, want 5s (first keyframe >= 4s)", got)
	}
	for i := 1; i < tl.Total()-1; i++ {
		// Every boundary but the folded tail sits on a keyframe.
		if st := tl.Start(i); st%(2500*time.Millisecond) != 0 {
			t.Errorf("boundary %d at %v is not keyframe-aligned", i, st)
		}
	}
}

func TestTimelineOpenEnded(t *testing.T) {
	tl := buildTimeline(0, 4*time.Second, timelineOptions{
		steadyMode: ModeTranscode, fastStart: true, rampSegs: 2, rampDur: 1500 * time.Millisecond,
	})
	if tl.Total() != 0 {
		t.Fatal("open timeline has a total")
	}
	if tl.Dur(0) != 1500*time.Millisecond || tl.Dur(5) != 4*time.Second {
		t.Errorf("durs %v %v", tl.Dur(0), tl.Dur(5))
	}
	if tl.Start(3) != 3*time.Second+4*time.Second {
		t.Errorf("Start(3) = %v", tl.Start(3))
	}
	if tl.IndexAt(4*time.Second) != 2 || tl.ModeAt(1) != ModeFastStart || tl.ModeAt(9) != ModeTranscode {
		t.Errorf("open queries: %d %v %v", tl.IndexAt(4*time.Second), tl.ModeAt(1), tl.ModeAt(9))
	}
}

func fastStartConfig() config.Transcode {
	return config.Transcode{FastStart: true, FastStartSegments: 2, FastStartSegment: config.Duration(2 * time.Second), FastStartMaxHeight: 720}
}

func TestFastStartCopySessionSwitchesToCopy(t *testing.T) {
	fr := newFakeRunner()
	m := newTestManager(t, fr, fastStartConfig())
	s := startSession(t, m, remuxRequest(t, "fs", 10*time.Minute))
	if !s.Decision().VideoCopy {
		t.Fatalf("want a copy session, got %+v", s.Decision())
	}
	rec := httptest.NewRecorder()
	s.ServePlaylist(rec, httptest.NewRequest(http.MethodGet, "/index.m3u8", nil))
	body := rec.Body.String()
	if n := strings.Count(body, "#EXTINF:2.000000,"); n != 2 {
		t.Errorf("ramp EXTINFs = %d\n%s", n, body[:400])
	}
	if n := strings.Count(body, "#EXT-X-DISCONTINUITY"); n != 1 {
		t.Errorf("discontinuities = %d", n)
	}
	if !strings.Contains(body, "#EXT-X-TARGETDURATION:4") {
		t.Errorf("target duration wrong:\n%s", body[:200])
	}
	// First run: quick low-latency transcode of the two ramp segments.
	first := fr.starts[0]
	mustContain(t, first, "-hls_time 2.000", "-force_key_frames expr:gte(t,n_forced*2.000)", "scale_vt=w=1280:h=720")
	mustNotContain(t, first, "-c:v copy")
	if j := joined(first); !strings.Contains(j, "ultrafast") && !strings.Contains(j, "-realtime 1") {
		t.Errorf("first run is not a low-latency encode: %s", j)
	}
	if r := get(t, s, 0); r.Code != 200 {
		t.Fatalf("segment 0: %d", r.Code)
	}
	if r := get(t, s, 1); r.Code != 200 {
		t.Fatalf("segment 1: %d", r.Code)
	}
	// The chain at the mode boundary starts the copy run at segment 2.
	waitFor(t, "copy run chained", func() bool { return fr.startCount() >= 2 })
	second := fr.lastStart()
	mustContain(t, second, "-c:v copy", "-hls_time 0.500", "-start_number 0")
	if got := argValue(second, "-ss"); got != "4.000" {
		t.Errorf("copy run -ss = %q, want segment 2 start 4.000", got)
	}
	if r := get(t, s, 2); r.Code != 200 || !strings.Contains(segText(r.Body.Bytes()), "args-ss=4.000") {
		t.Fatalf("segment 2: %d %q", r.Code, r.Body.String())
	}
}

func TestFastStartTranscodeSessionNoDiscontinuity(t *testing.T) {
	fr := newFakeRunner()
	m := newTestManager(t, fr, fastStartConfig())
	s := startSession(t, m, transcodeRequest(t, "fst"))
	rec := httptest.NewRecorder()
	s.ServePlaylist(rec, httptest.NewRequest(http.MethodGet, "/index.m3u8", nil))
	body := rec.Body.String()
	if strings.Contains(body, "#EXT-X-DISCONTINUITY") {
		t.Error("transcode ramp must not produce a discontinuity")
	}
	if n := strings.Count(body, "#EXTINF:2.000000,"); n != 2 {
		t.Errorf("ramp EXTINFs = %d", n)
	}
	mustContain(t, fr.starts[0], "-hls_time 2.000")
	if j := joined(fr.starts[0]); !strings.Contains(j, "ultrafast") && !strings.Contains(j, "-realtime 1") {
		t.Errorf("ramp run is not a low-latency encode: %s", j)
	}
	if r := get(t, s, 0); r.Code != 200 {
		t.Fatalf("segment 0: %d", r.Code)
	}
	if r := get(t, s, 2); r.Code != 200 {
		t.Fatalf("segment 2 (steady): %d", r.Code)
	}
	second := fr.lastStart()
	mustContain(t, second, "-hls_time 4.000")
	mustNotContain(t, second, "ultrafast", "-realtime 1")
}

func TestFastStartSwapInputKeepsPlan(t *testing.T) {
	fr := newFakeRunner()
	m := newTestManager(t, fr, fastStartConfig())
	s := startSession(t, m, remuxRequest(t, "fsswap", time.Hour))
	if r := get(t, s, 0); r.Code != 200 {
		t.Fatal(r.Code)
	}
	newInfo := mustParse(t, "mkv_truehd_atmos.json")
	newInfo.Duration = time.Hour
	// 41s lands in segment 11 of the ramped timeline (2+2+9*4=40 < 41).
	if err := s.SwapInput(t.Context(), "http://127.0.0.1:1/source/b", newInfo, 41*time.Second); err != nil {
		t.Fatal(err)
	}
	last := fr.lastStart()
	if argValue(last, "-ss") != "40.000" {
		t.Errorf("swap args ss=%v start=%v", argValue(last, "-ss"), argValue(last, "-start_number"))
	}
	if r := get(t, s, 11); r.Code != 200 || !strings.Contains(r.Body.String(), "input=http://127.0.0.1:1/source/b") {
		t.Fatalf("segment 11 after swap: %d %q", r.Code, r.Body.String())
	}
}
