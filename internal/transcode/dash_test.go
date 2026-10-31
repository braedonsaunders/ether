package transcode

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
)

var updateGolden = flag.Bool("update", false, "rewrite golden files")

func checkGolden(t *testing.T, name string, got []byte) {
	t.Helper()
	path := filepath.Join("testdata", "dash", name)
	if *updateGolden {
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%s: %v (run go test -update)", name, err)
	}
	if string(got) != string(want) {
		t.Errorf("%s mismatch\n--- got ---\n%s\n--- want ---\n%s", name, got, want)
	}
}

// The MPD matches, byte for byte, what a real PMS returned from start.mpd
// for a 3m37.866s 1080p H.264 episode with E-AC-3 audio converted to Opus.
func TestRenderMPDMatchesPMS(t *testing.T) {
	init, media := PlexDASHURLs("ethercapdash01")
	got := renderMPD(217866*time.Millisecond, 5*time.Second, init, media, []dashRep{
		{ID: 0, Kind: "video", Codecs: "avc1.640028", Bandwidth: 7184000, Width: 1920, Height: 1080},
		{ID: 1, Kind: "audio", Codecs: "opus", Bandwidth: 256000, SampleRate: 48000, Channels: 2},
	})
	want, err := os.ReadFile(filepath.Join("testdata", "dash", "pms-start.mpd"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Errorf("MPD differs from PMS capture\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

func TestRenderMPDGolden(t *testing.T) {
	// Audio-only, fractional segment length, hours-long, escaped URLs.
	got := renderMPD(2*time.Hour+3*time.Minute+4*time.Second, 2500*time.Millisecond,
		"s/a&b/$RepresentationID$/init", "s/a&b/$RepresentationID$/$Number$.m4s",
		[]dashRep{{ID: 0, Kind: "audio", Codecs: "mp4a.40.2", Bandwidth: 192000, SampleRate: 44100, Channels: 6}})
	checkGolden(t, "audio-only.mpd", got)

	// The plan for a remux of the TrueHD sample to Plex Web: video copied,
	// TrueHD converted.
	info := mustParse(t, "mkv_truehd_atmos.json")
	info.Duration = 2*time.Hour + 500*time.Millisecond
	d := dashDecision(info, Decide(info, plexWeb, allow(), Policy{}))
	init, media := PlexDASHURLs("abc")
	checkGolden(t, "truehd-remux.mpd", renderMPD(info.Duration, DefaultDASHSegmentDuration, init, media, dashReps(info, d)))
}

func TestMPDDuration(t *testing.T) {
	for d, want := range map[time.Duration]string{
		0:                         "PT0H0M0S",
		217866 * time.Millisecond: "PT0H3M37.866S",
		2*time.Hour + 5*time.Minute + 19*time.Second + 500*time.Millisecond: "PT2H5M19.500S",
		time.Hour: "PT1H0M0S",
	} {
		if got := mpdDuration(d); got != want {
			t.Errorf("%v: %q, want %q", d, got, want)
		}
	}
	if got := mpdSeconds(10 * time.Second); got != "PT10S" {
		t.Errorf("mpdSeconds %q", got)
	}
}

func TestDashRepsAndDecision(t *testing.T) {
	hdr := mustParse(t, "mkv_hevc_hdr10.json")
	hdr.Duration = time.Hour
	d := dashDecision(hdr, Decide(hdr, plexWeb, allow(), Policy{}))
	if d.Container != ContainerFMP4 || d.VideoCopy || d.VideoCodec != "h264" {
		t.Fatalf("decision %+v", d)
	}
	reps := dashReps(hdr, d)
	if len(reps) != 2 || reps[0].Kind != "video" || reps[1].Kind != "audio" || reps[1].ID != 1 {
		t.Fatalf("reps %+v", reps)
	}
	if reps[0].Height != d.MaxHeight || reps[0].Codecs != "avc1.640028" || reps[0].Bandwidth <= 0 {
		t.Errorf("video rep %+v (decision %+v)", reps[0], d)
	}

	// Copies a stream browsers cannot decode are converted.
	truehd := mustParse(t, "mkv_truehd_atmos.json")
	dd := Decide(truehd, appleTV, allow(), Policy{})
	dd.AudioCopy, dd.AudioCodec = true, ""
	dd = dashDecision(truehd, dd)
	if dd.AudioCopy || dd.AudioCodec != "aac" || !strings.Contains(dd.Reason, "DASH") {
		t.Errorf("truehd copy not converted: %+v", dd)
	}
	av := core.MediaInfo{Duration: time.Minute, Streams: []core.StreamInfo{{Index: 0, Type: "video", Codec: "mpeg2video", Height: 576}}}
	dv := dashDecision(av, core.Decision{Kind: core.DecisionRemux, VideoCopy: true, AudioStream: -1})
	if dv.VideoCopy || dv.Kind != core.DecisionTranscode || dv.VideoCodec != "h264" || dv.MaxHeight != 576 {
		t.Errorf("mpeg2 copy not converted: %+v", dv)
	}
	if reps := dashReps(av, dv); len(reps) != 1 || reps[0].Kind != "video" {
		t.Errorf("video-only reps %+v", reps)
	}
}

func chunkAt(start, end float64) fmp4Chunk {
	return fmp4Chunk{
		path:  fmt.Sprintf("%.3f", start),
		start: time.Duration(start * float64(time.Second)),
		end:   time.Duration(end * float64(time.Second)),
	}
}

// slotLog renders slotter output: "n:[chunk starts]" or "n=alias".
func slotLog(outs []slotOut) string {
	var parts []string
	for _, o := range outs {
		if o.alias >= 0 {
			parts = append(parts, fmt.Sprintf("%d=%d", o.n, o.alias))
			continue
		}
		var cs []string
		for _, c := range o.chunks {
			cs = append(cs, c.path)
		}
		parts = append(parts, fmt.Sprintf("%d:[%s]", o.n, strings.Join(cs, " ")))
	}
	return strings.Join(parts, " ")
}

func feed(sl *slotter, spans ...[2]float64) string {
	var outs []slotOut
	for _, s := range spans {
		outs = append(outs, sl.add(chunkAt(s[0], s[1]))...)
	}
	return slotLog(outs)
}

func TestSlotterAlignedChunks(t *testing.T) {
	sl := newSlotter(0, 5*time.Second, 10, 0)
	// 2s GOPs: chunks at 0,2,4,6,8,10; segment 0 is complete once a chunk
	// reaches past 5s.
	got := feed(sl, [2]float64{0, 2}, [2]float64{2, 4}, [2]float64{4, 6}, [2]float64{6, 8}, [2]float64{8, 10}, [2]float64{10, 12})
	if want := "0:[0.000 2.000 4.000] 1:[6.000 8.000]"; got != want {
		t.Errorf("got %s, want %s", got, want)
	}
	if sl.last != 1 || sl.count != 2 {
		t.Errorf("last %d count %d", sl.last, sl.count)
	}
	// Transcode chunks cut exactly at segment boundaries complete
	// immediately.
	sl = newSlotter(0, 5*time.Second, 10, 3)
	if got := feed(sl, [2]float64{15, 20}, [2]float64{20, 25}); got != "3:[15.000] 4:[20.000]" {
		t.Errorf("forced keyframes: %s", got)
	}
}

func TestSlotterLongGOPAliases(t *testing.T) {
	sl := newSlotter(0, 5*time.Second, 10, 0)
	// 10.4s GOPs: segment 1 lies inside the first GOP, 3 inside the second.
	got := feed(sl, [2]float64{0, 10.4}, [2]float64{10.4, 20.8}, [2]float64{20.8, 31.2})
	if want := "0:[0.000] 1=0 2:[10.400] 3=2 4:[20.800] 5=4"; got != want {
		t.Errorf("got %s, want %s", got, want)
	}
}

func TestSlotterFirstChunkOfRun(t *testing.T) {
	// A remux restarted at segment 30 begins at the keyframe before 150s;
	// that chunk still belongs to segment 30.
	sl := newSlotter(0, 5*time.Second, 100, 30)
	got := feed(sl, [2]float64{148.15, 150.2}, [2]float64{150.2, 152.3}, [2]float64{152.3, 154.95}, [2]float64{154.95, 157})
	if want := "30:[148.150 150.200 152.300]"; got != want {
		t.Errorf("got %s, want %s", got, want)
	}
	// 154.95 is within the rounding tolerance of 155: segment 31.
	if sl.slot != 31 {
		t.Errorf("pending slot %d", sl.slot)
	}
}

// A resume at 11:36 is playlist segment 175 (two 2s ramp segments, then 4s).
// Chunk timestamps are that title clock, which is what -ss plus the Matroska
// origin produces after the origin is removed. They must not be numbered as
// if the run's output clock started at zero.
func TestResumeChunkIndexMatchesSeek(t *testing.T) {
	tl := buildTimeline(90*time.Minute+28*time.Second, 4*time.Second, timelineOptions{
		fastStart: true, rampSegs: 2, rampDur: 2 * time.Second, steadyMode: ModeTranscode,
	})
	const seg = 175
	at := tl.Start(seg)
	if at != 11*time.Minute+36*time.Second {
		t.Fatalf("segment %d starts at %s, want 11m36s", seg, at)
	}
	if tl.IndexAt(at) != seg || tl.IndexAt(at+4*time.Second) != seg+1 {
		t.Fatalf("IndexAt(%s)=%d IndexAt(+4s)=%d", at, tl.IndexAt(at), tl.IndexAt(at+4*time.Second))
	}
	sl := newSlotter(0, 4*time.Second, tl.Total(), seg)
	sl.index = func(ts time.Duration) int { return tl.IndexAt(ts) }
	got := feed(sl, [2]float64{at.Seconds(), at.Seconds() + 4}, [2]float64{at.Seconds() + 4, at.Seconds() + 8})
	if got != "175:[696.000] 176:[700.000]" {
		t.Fatalf("slotted %s, want segments 175 and 176", got)
	}
}

func TestSlotterFlush(t *testing.T) {
	sl := newSlotter(0, 5*time.Second, 3, 0)
	got := feed(sl, [2]float64{0, 4}, [2]float64{4, 8}, [2]float64{8, 9.5})
	if got != "0:[0.000 4.000]" {
		t.Fatalf("got %s", got)
	}
	// End of input: the pending segment is complete and the MPD's last
	// (rounded up) segment repeats it.
	if got := slotLog(sl.flush(true)); got != "1:[8.000] 2=1" {
		t.Errorf("flush: %s", got)
	}
	// A stopped run drops its partial segment.
	sl = newSlotter(0, 5*time.Second, 10, 0)
	feed(sl, [2]float64{0, 2})
	if got := sl.flush(false); len(got) != 0 || sl.count != 0 {
		t.Errorf("stopped flush: %v", got)
	}
	// Chunks past the MPD's end fold into its last segment.
	sl = newSlotter(0, 5*time.Second, 2, 0)
	got = feed(sl, [2]float64{0, 5}, [2]float64{5, 10}, [2]float64{10, 10.5})
	if got != "0:[0.000]" || sl.slot != 1 || len(sl.pending) != 2 {
		t.Errorf("past end: %s slot %d pending %d", got, sl.slot, len(sl.pending))
	}
	if got := slotLog(sl.flush(true)); got != "1:[5.000 10.000]" {
		t.Errorf("past end flush: %s", got)
	}
}

// ---------------------------------------------------------------------------
// Sessions on the fake runner.

func dashRequest(t *testing.T, id string, dur time.Duration) core.HLSRequest {
	info := mustParse(t, "mp4_h264_aac.json")
	info.Duration = dur
	o := allow()
	o.DirectPlay = false
	return core.HLSRequest{
		SessionID: id,
		InputURL:  "http://127.0.0.1:1/source/a",
		Info:      info,
		Decision:  Decide(info, plexWeb, o, Policy{}),
		Protocol:  core.ProtocolDASH,
	}
}

func startDASHSession(t *testing.T, m *Manager, req core.HLSRequest) *Session {
	t.Helper()
	hs, err := m.StartHLS(context.Background(), req)
	if err != nil {
		t.Fatalf("StartHLS: %v", err)
	}
	s := hs.(*Session)
	if s.Protocol() != core.ProtocolDASH {
		t.Fatalf("protocol %q", s.Protocol())
	}
	return s
}

func media(t *testing.T, s *Session, rep, n int) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/%d/%d.m4s", rep, n), nil)
	if n == InitSegment {
		s.ServeInit(rec, req, rep)
	} else {
		s.ServeMedia(rec, req, rep, n)
	}
	return rec
}

// chunkTimes lists the fake chunk start times in a served segment.
func chunkTimes(body string) []string {
	var out []string
	for _, part := range strings.Split(body, "[rep=")[1:] {
		_, rest, _ := strings.Cut(part, " t=")
		ts, _, _ := strings.Cut(rest, " ")
		out = append(out, ts)
	}
	return out
}

func TestDASHSessionManifestAndSegments(t *testing.T) {
	fr := newFakeRunner()
	m := newTestManager(t, fr, config.Transcode{})
	s := startDASHSession(t, m, dashRequest(t, "d1", 10*time.Minute))

	rec := httptest.NewRecorder()
	s.ServePlaylist(rec, httptest.NewRequest(http.MethodGet, "/start.mpd", nil))
	body := rec.Body.String()
	if rec.Code != 200 || rec.Header().Get("Content-Type") != "application/dash+xml" {
		t.Fatalf("MPD %d %q", rec.Code, rec.Header().Get("Content-Type"))
	}
	for _, want := range []string{
		`mediaPresentationDuration="PT0H10M0S"`,
		`media="session/d1/$RepresentationID$/$Number$.m4s"`,
		`initialization="session/d1/$RepresentationID$/header"`,
		`<Representation id="0" mimeType="video/mp4" codecs="avc1.640028"`,
		`<Representation id="1" mimeType="audio/mp4" codecs="mp4a.40.2"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("MPD lacks %s:\n%s", want, body)
		}
	}
	args := fr.lastStart()
	if argValue(args, "-f") != "dash" || argValue(args, "-seg_duration") != "1.000" || hasArg(args, "-ss") ||
		argValue(args, "-adaptation_sets") != "id=0,streams=v id=1,streams=a" ||
		argValue(args, "-format_options") != "movflags=+frag_discont" {
		t.Errorf("ffmpeg args %v", args)
	}

	init := media(t, s, 0, InitSegment)
	if init.Code != 200 || init.Body.String() != string(testVideoInit(1280, 720)) ||
		init.Header().Get("Content-Type") != "application/octet-stream" {
		t.Fatalf("video init: %d %q", init.Code, init.Header().Get("Content-Type"))
	}
	for rep := 0; rep < 2; rep++ {
		for n := 0; n < 3; n++ {
			r := media(t, s, rep, n)
			if r.Code != 200 {
				t.Fatalf("rep %d segment %d: %d %s", rep, n, r.Code, r.Body.String())
			}
			ts := chunkTimes(r.Body.String())
			want := []string{}
			for k := 0; k < 5; k++ {
				want = append(want, fmt.Sprintf("%.3f", float64(5*n+k)))
			}
			if strings.Join(ts, ",") != strings.Join(want, ",") {
				t.Errorf("rep %d segment %d chunks %v, want %v", rep, n, ts, want)
			}
			wantInit := testVideoInit(1280, 720)
			if rep == 1 {
				wantInit = testAudioInit(2, 48000)
			}
			if !strings.HasPrefix(r.Body.String(), string(wantInit)) {
				t.Errorf("rep %d segment %d does not start with its init segment", rep, n)
			}
		}
	}
	if fr.startCount() != 1 {
		t.Errorf("ffmpeg starts = %d", fr.startCount())
	}
	if r := media(t, s, 0, 120); r.Code != 404 {
		t.Errorf("past end: %d", r.Code)
	}
	if r := media(t, s, 2, 0); r.Code != 404 {
		t.Errorf("unknown representation: %d", r.Code)
	}
	if hs, ok := m.Session("d1"); !ok {
		t.Error("session not registered")
	} else if _, ok := hs.(core.DASHSession); !ok {
		t.Error("registered session is not a DASHSession")
	}
	// HLSSession methods map onto the first representation.
	if r := get(t, s, 1); r.Code != 200 || !strings.Contains(r.Body.String(), "[rep=0 chunk=") {
		t.Errorf("ServeSegment: %d", r.Code)
	}
}

func TestDASHSegmentNumberingAcrossSeeks(t *testing.T) {
	fr := newFakeRunner()
	fr.segDelay = 3 * time.Millisecond
	m := newTestManager(t, fr, config.Transcode{})
	s := startDASHSession(t, m, dashRequest(t, "seek", time.Hour))
	check := func(rep, n int) {
		t.Helper()
		r := media(t, s, rep, n)
		if r.Code != 200 {
			t.Fatalf("rep %d segment %d: %d", rep, n, r.Code)
		}
		ts := chunkTimes(r.Body.String())
		if len(ts) == 0 || ts[0] != fmt.Sprintf("%.3f", float64(5*n)) {
			t.Errorf("rep %d segment %d starts with chunk %v, want %d.000", rep, n, ts, 5*n)
		}
	}
	check(0, 0)
	// Far ahead: ffmpeg restarts at segment 100 (-ss 500).
	check(0, 100)
	check(1, 100)
	if argValue(fr.lastStart(), "-ss") != "500.000" || fr.startCount() != 2 {
		t.Errorf("restart: %d starts, args %v", fr.startCount(), fr.lastStart())
	}
	// Just ahead: no restart.
	check(1, 102)
	check(0, 102)
	if fr.startCount() != 2 {
		t.Errorf("near-ahead request restarted ffmpeg (%d starts)", fr.startCount())
	}
	// Back behind the run: restart there; earlier segments stay on disk.
	check(0, 40)
	if argValue(fr.lastStart(), "-ss") != "200.000" || fr.startCount() != 3 {
		t.Errorf("seek back: %d starts, args %v", fr.startCount(), fr.lastStart())
	}
	check(0, 101)
	check(0, 0)
	if fr.startCount() != 3 {
		t.Errorf("finished segments caused a restart (%d starts)", fr.startCount())
	}
}

func TestDASHStartOffsetAndEOF(t *testing.T) {
	fr := newFakeRunner()
	fr.dashEnd = 62 * time.Second
	m := newTestManager(t, fr, config.Transcode{})
	req := dashRequest(t, "off", 62*time.Second)
	req.StartOffset = 47 * time.Second
	s := startDASHSession(t, m, req)
	// The MPD still spans the whole title from segment 0.
	rec := httptest.NewRecorder()
	s.ServeManifest(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if !strings.Contains(rec.Body.String(), `mediaPresentationDuration="PT0H1M2S"`) {
		t.Errorf("MPD %s", rec.Body.String())
	}
	if argValue(fr.lastStart(), "-ss") != "45.000" {
		t.Fatalf("first run args %v", fr.lastStart())
	}
	if r := media(t, s, 0, 9); r.Code != 200 || chunkTimes(r.Body.String())[0] != "45.000" {
		t.Fatalf("segment 9: %d %s", r.Code, r.Body.String())
	}
	// The input ends at 62s: segment 12 (60-62s) completes at EOF.
	r := media(t, s, 1, 12)
	if r.Code != 200 || strings.Join(chunkTimes(r.Body.String()), ",") != "60.000,61.000" {
		t.Fatalf("segment 12: %d %v", r.Code, chunkTimes(r.Body.String()))
	}
	waitFor(t, "EOF", func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.endSeg == 12
	})
	if fr.startCount() != 1 {
		t.Errorf("starts %d", fr.startCount())
	}
}

func TestDASHSwapInput(t *testing.T) {
	fr := newFakeRunner()
	m := newTestManager(t, fr, config.Transcode{})
	req := dashRequest(t, "swap", time.Hour)
	s := startDASHSession(t, m, req)
	if r := media(t, s, 0, 3); r.Code != 200 {
		t.Fatal(r.Code)
	}
	if err := s.SwapInput(context.Background(), "http://127.0.0.1:1/source/b", req.Info, 12*time.Second); err != nil {
		t.Fatal(err)
	}
	if argValue(fr.lastStart(), "-ss") != "10.000" || argValue(fr.lastStart(), "-i") != "http://127.0.0.1:1/source/b" {
		t.Errorf("swap args %v", fr.lastStart())
	}
	for rep := 0; rep < 2; rep++ {
		r := media(t, s, rep, 2)
		if r.Code != 200 || !strings.Contains(r.Body.String(), "input=http://127.0.0.1:1/source/b") {
			t.Errorf("rep %d segment 2 after swap: %d", rep, r.Code)
		}
	}
	// Segments before the swap point are kept.
	if r := media(t, s, 0, 1); r.Code != 200 || strings.Contains(r.Body.String(), "source/b") {
		t.Errorf("segment 1 after swap: %d", r.Code)
	}
}

func TestDASHHardwareFallsBackToCPU(t *testing.T) {
	fr := newFakeRunner()
	fr.failStart = func(args []string) (string, bool) {
		if argValue(args, "-c:v") == "h264_videotoolbox" {
			return "[h264_videotoolbox @ 0x1] Error: cannot create compression session", true
		}
		return "", false
	}
	m := newTestManager(t, fr, config.Transcode{HWAccel: "auto"})
	req := transcodeRequest(t, "dhw")
	req.Protocol = core.ProtocolDASH
	s := startDASHSession(t, m, req)
	if r := media(t, s, 0, 0); r.Code != 200 {
		t.Fatalf("segment 0: %d %s", r.Code, r.Body.String())
	}
	if fr.startCount() != 2 || argValue(fr.lastStart(), "-c:v") != "libx264" {
		t.Errorf("starts %d, last encoder %q", fr.startCount(), argValue(fr.lastStart(), "-c:v"))
	}
	if !strings.Contains(strings.Join(fr.lastStart(), " "), "-force_key_frames expr:gte(t,n_forced*2.000)") {
		t.Errorf("keyframes not forced at segment boundaries: %v", fr.lastStart())
	}
}

func TestDASHMidStreamFailureReported(t *testing.T) {
	fr := newFakeRunner()
	fr.failAfter = 12
	got := make(chan time.Duration, 4)
	m := newTestManager(t, fr, config.Transcode{}, WithFailureHandler(func(id string, at time.Duration, err error) {
		if id == "dfail" && err != nil {
			got <- at
		}
	}))
	startDASHSession(t, m, dashRequest(t, "dfail", time.Hour))
	select {
	case at := <-got:
		// Chunks 0-11s: segments 0 and 1 complete, playback resumes at 10s.
		if at != 10*time.Second {
			t.Errorf("failure at %v, want 10s", at)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("failure handler not called")
	}
}

func TestDASHManifestFallsBackToEstimates(t *testing.T) {
	fr := newFakeRunner()
	fr.failStart = func([]string) (string, bool) { return "http://x: Server returned 404 Not Found", true }
	m := newTestManager(t, fr, config.Transcode{}, WithManifestWait(100*time.Millisecond))
	s := startDASHSession(t, m, dashRequest(t, "est", time.Minute))
	rec := httptest.NewRecorder()
	s.ServeManifest(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `codecs="avc1.640028"`) {
		t.Errorf("estimated MPD: %d %s", rec.Code, rec.Body.String())
	}
	s.mu.Lock()
	cached := s.dash.mpd != nil
	s.mu.Unlock()
	if cached {
		t.Error("estimated MPD was cached")
	}
	if r := media(t, s, 0, 0); r.Code != http.StatusBadGateway {
		t.Errorf("segment with failing input: %d", r.Code)
	}
}

func TestDASHValidationAndLimits(t *testing.T) {
	fr := newFakeRunner()
	m := newTestManager(t, fr, config.Transcode{MaxTranscodes: 1})
	ctx := context.Background()
	req := dashRequest(t, "x", 0)
	if hs, err := m.StartHLS(ctx, req); err != nil {
		t.Fatalf("live DASH rejected: %v", err)
	} else {
		hs.Close()
	}
	req = dashRequest(t, "x", time.Minute)
	req.Protocol = "smooth"
	if _, err := m.StartHLS(ctx, req); err == nil {
		t.Error("unknown protocol accepted")
	}
	tx := transcodeRequest(t, "t1")
	tx.Protocol = core.ProtocolDASH
	startDASHSession(t, m, tx)
	tx.SessionID = "t2"
	if _, err := m.StartHLS(ctx, tx); !errors.Is(err, ErrTooManyTranscodes) {
		t.Errorf("transcode limit: %v", err)
	}
}

func TestDASHPruneAndClose(t *testing.T) {
	fr := newFakeRunner()
	fr.segDelay = time.Millisecond
	ahead := 24 * time.Second
	if runtime.GOOS == "windows" {
		// Windows cannot pause ffmpeg, so getting ahead stops it, and the
		// fake runner does not model the restart; keep the encoder below
		// that threshold until it has passed the segments checked here.
		ahead = 10 * time.Minute
	}
	m := newTestManager(t, fr, config.Transcode{}, WithBuffering(ahead, 0, 20*time.Second))
	s := startDASHSession(t, m, dashRequest(t, "prune", time.Hour))
	if r := media(t, s, 0, 0); r.Code != 200 {
		t.Fatal(r.Code)
	}
	waitFor(t, "encoder to pass 31", func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.cur != nil && s.cur.maxDone > 31
	})
	if r := media(t, s, 1, 30); r.Code != 200 {
		t.Fatal(r.Code)
	}
	if _, err := os.Stat(s.trackSegPath(0, 0)); !os.IsNotExist(err) {
		t.Error("old segment not pruned")
	}
	if _, err := os.Stat(s.trackSegPath(1, 28)); err != nil {
		t.Error("recent segment pruned")
	}
	// Consumed chunks do not pile up in the run directory.
	s.mu.Lock()
	runDir := s.cur.dash.dir
	s.mu.Unlock()
	// Chunk pruning keeps up because the encoder is paused when it gets
	// ahead (SIGSTOP). Windows can't pause a process, so Ether stops and
	// restarts ffmpeg instead, which the fake runner doesn't model; there
	// the fake encoder races ahead unconsumed. Deletion itself is covered
	// on every platform by the session-dir check after Stop below.
	if runtime.GOOS != "windows" {
		waitFor(t, "consumed chunks to be deleted", func() bool {
			entries, _ := os.ReadDir(runDir)
			return len(entries) <= 30
		})
	}
	p := fr.lastProc()
	m.Stop("prune")
	if !p.wasKilled() {
		t.Error("ffmpeg not killed")
	}
	if _, err := os.Stat(s.dir); !os.IsNotExist(err) {
		t.Error("session dir kept")
	}
	if r := media(t, s, 0, 31); r.Code != http.StatusGone {
		t.Errorf("closed session: %d", r.Code)
	}
}

func TestDASHConcurrentRequests(t *testing.T) {
	fr := newFakeRunner()
	fr.segDelay = time.Millisecond
	m := newTestManager(t, fr, config.Transcode{})
	s := startDASHSession(t, m, dashRequest(t, "conc", time.Hour))
	var wg sync.WaitGroup
	errs := make(chan string, 64)
	for g := 0; g < 6; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 4; i++ {
				n := (g/2)*30 + i
				// One player cannot want three places at once: the newest
				// request owns the encoder and older ones are told to retry.
				if r := media(t, s, g%2, n); r.Code != 200 && r.Code != http.StatusBadGateway && r.Code != http.StatusServiceUnavailable {
					errs <- fmt.Sprintf("rep %d segment %d: %d", g%2, n, r.Code)
				}
			}
		}(g)
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Error(e)
	}
}

// The ffmpeg DASH muxer finds the directory for its init and chunk
// files by scanning the manifest output path for '/'. The output path
// must therefore use forward slashes on every platform; with Windows
// backslashes the muxer wrote every chunk into its working directory
// and the session's run directory stayed empty.
func TestDASHMuxOutputPathUsesForwardSlashes(t *testing.T) {
	info := mustParse(t, "mp4_h264_aac.json")
	s := baseSpec(info, Decide(info, plexWeb, allow(), Policy{}))
	s.Protocol = core.ProtocolDASH
	s.Dir = filepath.Join("root", "session", "run-1")
	s.Playlist = "manifest.mpd"
	args := buildArgs(s)
	out := args[len(args)-1]
	if strings.Contains(out, "\\") || !strings.HasSuffix(out, "root/session/run-1/manifest.mpd") {
		t.Fatalf("DASH output path %q must use forward slashes", out)
	}
}

// A collect pass must publish what it has instead of chasing a fast
// producer until it catches up: with far more chunk files already on
// disk than one pass may consume, the init segment and the first
// segments are published after a single bounded pass, and the final
// pass after ffmpeg exits consumes the remainder. Without the bound
// nothing was published while the producer kept pace — on Windows,
// where each chunk file operation is slower, that starved every DASH
// request into a timeout.
func TestDASHCollectPublishesAfterBoundedPass(t *testing.T) {
	dir := t.TempDir()
	runDir := filepath.Join(dir, "run-1")
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(runDir, dashInitFile(0)), testVideoInit(1280, 720), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(runDir, dashInitFile(1)), testAudioInit(2, 48000), 0o644); err != nil {
		t.Fatal(err)
	}
	const chunks = 200 // 1s chunks, 5s segments: 40 segments per track
	for n := 1; n <= chunks; n++ {
		v := testChunk(uint32(n), int64(n-1)*90000, 3750, 24, []byte("v"))
		a := testChunk(uint32(n), int64(n-1)*48000, 960, 50, []byte("a"))
		if err := os.WriteFile(filepath.Join(runDir, dashChunkFile(0, n)), v, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(runDir, dashChunkFile(1, n)), a, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	s := &Session{
		proto:  core.ProtocolDASH,
		m:      &Manager{opts: options{}},
		dir:    dir,
		segDur: 5 * time.Second,
		total:  40,
		log:    testLogger(t),
		sessionDelivery: sessionDelivery{
			done:    []map[int]bool{{}, {}},
			changed: make(chan struct{}),
			dash:    newDASHState([]dashRep{{ID: 0, Kind: "video"}, {ID: 1, Kind: "audio"}}, nil, "bounded"),
		},
	}
	proc := &fakeProc{done: make(chan struct{})}
	close(proc.done) // Err() == nil: a clean EOF exit for the final pass
	r := &run{seq: 1, proc: proc, maxDone: -1, started: time.Now(),
		dash: newDASHRun(runDir, []int{0, 1}, 0, s.segDur, s.total, 0)}
	s.cur = r

	s.collectDASH(r, false)
	for tr, track := range r.dash.tracks {
		if want := 1 + dashCollectBatch; track.next != want {
			t.Fatalf("track %d consumed to chunk %d, want bounded pass to stop at %d", tr, track.next, want)
		}
		if _, err := os.Stat(filepath.Join(runDir, dashChunkFile(tr, track.next))); err != nil {
			t.Fatalf("track %d: unconsumed chunk should still exist: %v", tr, err)
		}
	}
	if s.dash.inits[0] == "" || s.dash.inits[1] == "" {
		t.Fatal("init segments not published by bounded pass")
	}
	// 32 one-second chunks complete segments 0-5; segment 6 is pending.
	for _, track := range []int{0, 1} {
		if !s.done[track][0] || !s.done[track][5] || s.done[track][6] {
			t.Fatalf("track %d published segments wrong after bounded pass: %v", track, s.done[track])
		}
		if _, err := os.Stat(s.trackSegPath(track, 0)); err != nil {
			t.Fatalf("track %d segment 0 file missing: %v", track, err)
		}
	}

	s.collectDASH(r, true)
	for tr, track := range r.dash.tracks {
		if track.next != chunks+1 {
			t.Fatalf("track %d final pass consumed to %d, want %d", tr, track.next, chunks+1)
		}
		if len(s.done[tr]) != 40 || !s.done[tr][39] {
			t.Fatalf("track %d final pass published %d segments, want all 40", tr, len(s.done[tr]))
		}
	}
}

func TestLiveMPDOnlyAdvertisesCompletedWindow(t *testing.T) {
	start := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	init, media := PlexDASHURLs("live")
	b := string(renderLiveMPD(start, start.Add(time.Hour), 4*time.Second, 100, init, media, []dashRep{{ID: 0, Kind: "video", Codecs: "avc1.640028", Bandwidth: 4000000}}))
	for _, want := range []string{`type="dynamic"`, `startNumber="71"`, `<S t="284000" d="4000" r="29"/>`, `minimumUpdatePeriod="PT2S"`} {
		if !strings.Contains(b, want) {
			t.Fatalf("missing %s in %s", want, b)
		}
	}
	if strings.Contains(b, "mediaPresentationDuration") {
		t.Fatal("live stream has an invented end")
	}
}

// A seek must not ping-pong the encoder: the request for the old position
// the player abandoned gives way instead of restarting ffmpeg there.
func TestDASHAbandonedRequestDoesNotPullEncoderBack(t *testing.T) {
	fr := newFakeRunner()
	fr.segDelay = 300 * time.Millisecond
	m := newTestManager(t, fr, config.Transcode{})
	s := startDASHSession(t, m, dashRequest(t, "seek", time.Hour))
	old := make(chan int, 1)
	go func() { old <- media(t, s, 0, 100).Code }()
	time.Sleep(50 * time.Millisecond) // the old request moved the encoder to 100
	before := fr.startCount()
	if r := media(t, s, 0, 500); r.Code != 200 {
		t.Fatalf("seek target: %d", r.Code)
	}
	select {
	case code := <-old:
		if code != http.StatusServiceUnavailable {
			t.Fatalf("abandoned request: %d, want 503", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("abandoned request still waiting")
	}
	if n := fr.startCount() - before; n != 1 {
		t.Fatalf("encoder started %d times for one seek", n)
	}
	// The same segment asked for again is a new request and is served.
	if r := media(t, s, 0, 100); r.Code != 200 {
		t.Fatalf("retried old segment: %d", r.Code)
	}
}

func TestRunStartHintNamesTheSpanBeforeFFmpegReads(t *testing.T) {
	fr := newFakeRunner()
	type hint struct {
		input    string
		from, to time.Duration
	}
	hints := make(chan hint, 8)
	m := newTestManager(t, fr, config.Transcode{}, WithRunStartHandler(func(input string, from, to time.Duration) {
		hints <- hint{input, from, to}
	}))
	s := startDASHSession(t, m, dashRequest(t, "hint", time.Hour))
	if r := media(t, s, 0, 300); r.Code != 200 {
		t.Fatalf("seek: %d", r.Code)
	}
	deadline := time.After(2 * time.Second)
	for {
		select {
		case h := <-hints:
			if h.from == 300*s.segDur {
				if h.to != h.from+2*s.segDur || h.input == "" {
					t.Fatalf("hint %+v", h)
				}
				return
			}
		case <-deadline:
			t.Fatal("no hint for the seek run")
		}
	}
}
