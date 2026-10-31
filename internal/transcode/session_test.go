package transcode

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
)

func remuxRequest(t *testing.T, id string, dur time.Duration) core.HLSRequest {
	info := mustParse(t, "mkv_truehd_atmos.json")
	info.Duration = dur
	return core.HLSRequest{
		SessionID: id,
		InputURL:  "http://127.0.0.1:1/source/a",
		Info:      info,
		Decision:  Decide(info, appleTV, allow(), Policy{}),
	}
}

func transcodeRequest(t *testing.T, id string) core.HLSRequest {
	info := mustParse(t, "mkv_hevc_hdr10.json")
	info.Duration = 10 * time.Minute
	return core.HLSRequest{
		SessionID: id,
		InputURL:  "http://127.0.0.1:1/source/t",
		Info:      info,
		Decision:  Decide(info, plexWeb, allow(), Policy{}),
	}
}

func get(t *testing.T, s core.HLSSession, n int) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/seg/%d", n), nil)
	s.ServeSegment(rec, req, n)
	return rec
}

func startSession(t *testing.T, m *Manager, req core.HLSRequest) *Session {
	t.Helper()
	hs, err := m.StartHLS(context.Background(), req)
	if err != nil {
		t.Fatalf("StartHLS: %v", err)
	}
	return hs.(*Session)
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	// Generous: real ffmpeg work on a busy, race-instrumented CI runner can
	// take several seconds; a condition that holds returns at once.
	deadline := time.Now().Add(30 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestSessionServesPlaylistAndSegments(t *testing.T) {
	fr := newFakeRunner()
	m := newTestManager(t, fr, config.Transcode{})
	s := startSession(t, m, remuxRequest(t, "abc", 10*time.Minute))

	rec := httptest.NewRecorder()
	s.ServePlaylist(rec, httptest.NewRequest(http.MethodGet, "/index.m3u8", nil))
	body := rec.Body.String()
	if rec.Header().Get("Content-Type") != "application/vnd.apple.mpegurl" {
		t.Errorf("content type %q", rec.Header().Get("Content-Type"))
	}
	if n := strings.Count(body, "#EXTINF"); n != 150 {
		t.Errorf("segments in playlist = %d, want 150", n)
	}
	if !strings.Contains(body, "00149.ts") || !strings.Contains(body, "#EXT-X-PLAYLIST-TYPE:VOD") {
		t.Errorf("playlist:\n%s", body[:200])
	}

	r := get(t, s, 0)
	if r.Code != 200 || !strings.HasPrefix(segText(r.Body.Bytes()), "segment 0") {
		t.Fatalf("segment 0: %d %q", r.Code, r.Body.String())
	}
	if ct := r.Header().Get("Content-Type"); ct != "video/mp2t" {
		t.Errorf("segment content type %q", ct)
	}
	if r := get(t, s, 2); r.Code != 200 || !strings.HasPrefix(segText(r.Body.Bytes()), "segment 2") {
		t.Fatalf("segment 2: %d %q", r.Code, r.Body.String())
	}
	if fr.startCount() != 1 {
		t.Errorf("ffmpeg starts = %d, want 1", fr.startCount())
	}
	first := fr.lastStart()
	if argValue(first, "-start_number") != "0" || hasArg(first, "-ss") {
		t.Errorf("first run args: %v", first)
	}
	if r := get(t, s, 150); r.Code != 404 {
		t.Errorf("past end: %d", r.Code)
	}
	if r := get(t, s, -5); r.Code != 404 {
		t.Errorf("negative: %d", r.Code)
	}
	if _, ok := m.Session("abc"); !ok {
		t.Error("Session lookup failed")
	}
	if p := s.Progress(); p.Speed != 4 {
		t.Errorf("progress %+v", p)
	}
}

func TestSpikeGovernorReplansUpcomingCopySegment(t *testing.T) {
	segDur := 4 * time.Second
	plan := buildTimeline(time.Minute, segDur, timelineOptions{steadyMode: ModeCopy})
	s := &Session{
		m:         &Manager{opts: options{maxAhead: 20 * time.Second}},
		id:        "governor-test",
		dir:       t.TempDir(),
		segDur:    segDur,
		container: ContainerMPEGTS,
		log:       slog.Default(),
		sessionEncoding: sessionEncoding{
			cur: &run{track: 0, mode: ModeCopy, planEnd: -1, maxDone: 1},
		},
		sessionDelivery: sessionDelivery{
			lastReq: 0,
			done:    []map[int]bool{{1: true}},
		},
		sessionAdaptation: sessionAdaptation{
			plan:       plan,
			adapt:      adaptiveSettings{Governor: true, SpikeMargin: 1.25},
			governorOn: true,
			segSizes:   map[int]int64{1: 2_000_000},
		},
	}
	s.goodput.add(250_000, time.Second)
	s.goodput.add(250_000, time.Second)
	s.maybeGovernLocked()
	if got := plan.ClampKbps(1); got != 1600 {
		t.Fatalf("clamp = %d kbps, want 1600", got)
	}
	if s.cur.planEnd != 1 {
		t.Fatalf("copy run boundary = %d, want 1", s.cur.planEnd)
	}
	if s.done[0][1] {
		t.Fatal("stale copy segment remained eligible after clamp planning")
	}
}

func TestSessionSeekRestarts(t *testing.T) {
	fr := newFakeRunner()
	fr.segDelay = 30 * time.Millisecond
	m := newTestManager(t, fr, config.Transcode{})
	s := startSession(t, m, remuxRequest(t, "seek", time.Hour))
	firstProc := fr.lastProc()

	// Far ahead: restart at 100 with -ss 400.
	if r := get(t, s, 100); r.Code != 200 || !strings.Contains(r.Body.String(), "segment 100 args-ss=400.000") {
		t.Fatalf("segment 100: %d %q", r.Code, r.Body.String())
	}
	if fr.startCount() != 2 {
		t.Fatalf("starts = %d, want 2", fr.startCount())
	}
	if !firstProc.wasKilled() {
		t.Error("old ffmpeg not killed on seek")
	}
	last := fr.lastStart()
	if argValue(last, "-ss") != "400.000" {
		t.Errorf("restart args: %v", last)
	}
	// Just ahead of the encoder: wait, no restart.
	if r := get(t, s, 103); r.Code != 200 {
		t.Fatalf("segment 103: %d", r.Code)
	}
	if fr.startCount() != 2 {
		t.Errorf("near-ahead request restarted ffmpeg (%d starts)", fr.startCount())
	}
	// Behind the run's start and not on disk: restart there.
	if r := get(t, s, 40); r.Code != 200 || !strings.Contains(r.Body.String(), "segment 40 args-ss=160.000") {
		t.Fatalf("segment 40: %d %q", r.Code, r.Body.String())
	}
	if fr.startCount() != 3 {
		t.Errorf("starts = %d, want 3", fr.startCount())
	}
	// Segments produced by an earlier run are served from disk.
	if r := get(t, s, 101); r.Code != 200 || !strings.Contains(r.Body.String(), "segment 101") {
		t.Fatalf("segment 101: %d", r.Code)
	}
	if fr.startCount() != 3 {
		t.Errorf("finished segment caused a restart (%d starts)", fr.startCount())
	}
}

func TestSessionStartOffset(t *testing.T) {
	fr := newFakeRunner()
	m := newTestManager(t, fr, config.Transcode{})
	req := remuxRequest(t, "off", 10*time.Minute)
	req.StartOffset = 5 * time.Minute
	req.SegmentDuration = 6 * time.Second
	s := startSession(t, m, req)
	if r := get(t, s, 0); r.Code != 200 || !strings.Contains(r.Body.String(), "args-ss=300.000") {
		t.Fatalf("segment 0: %d %q", r.Code, r.Body.String())
	}
	rec := httptest.NewRecorder()
	s.ServePlaylist(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if n := strings.Count(rec.Body.String(), "#EXTINF:6.000000"); n != 50 {
		t.Errorf("segments = %d, want 50", n)
	}
	// A copy without a keyframe index is cut at every keyframe and
	// regrouped onto the playlist.
	if argValue(fr.lastStart(), "-hls_time") != "0.500" || argValue(fr.lastStart(), "-start_number") != "0" {
		t.Errorf("chunked copy args %v", fr.lastStart())
	}
}

func TestSessionHardwareFallsBackToCPU(t *testing.T) {
	fr := newFakeRunner()
	fr.failStart = func(args []string) (string, bool) {
		if argValue(args, "-c:v") == "h264_videotoolbox" {
			return "[h264_videotoolbox @ 0x1] Error: cannot create compression session: -12902", true
		}
		return "", false
	}
	m := newTestManager(t, fr, config.Transcode{HWAccel: "auto"})
	if m.HWAccel().Name != AccelVideoToolbox {
		t.Fatalf("hw = %+v", m.HWAccel())
	}
	s := startSession(t, m, transcodeRequest(t, "hw"))
	if r := get(t, s, 0); r.Code != 200 {
		t.Fatalf("segment 0: %d %s", r.Code, r.Body.String())
	}
	if fr.startCount() != 2 {
		fr.mu.Lock()
		for i, args := range fr.starts {
			t.Logf("start %d c:v=%s graph=%s", i, argValue(args, "-c:v"), argValue(args, "-filter_complex"))
		}
		fr.mu.Unlock()
		t.Errorf("starts = %d, want 2 (hw then cpu)", fr.startCount())
	}
	if enc := argValue(fr.lastStart(), "-c:v"); enc != "libx264" {
		t.Errorf("fallback encoder %q", enc)
	}
	if s.HWAccelerated() {
		t.Error("session still reports hardware")
	}
	// Later restarts stay on CPU.
	get(t, s, 100)
	if enc := argValue(fr.lastStart(), "-c:v"); enc != "libx264" {
		t.Errorf("after seek encoder %q", enc)
	}
}

func TestSessionMissingFilterTriesNextGraph(t *testing.T) {
	fr := newFakeRunner()
	fr.failStart = func(args []string) (string, bool) {
		if strings.Contains(argValue(args, "-filter_complex"), "zscale") {
			return "No such filter: 'zscale'", true
		}
		return "", false
	}
	failures := make(chan error, 2)
	m := newTestManager(t, fr, config.Transcode{HWAccel: "none"}, WithFailureHandler(func(_ string, _ time.Duration, err error) {
		failures <- err
	}))
	info := core.MediaInfo{Duration: time.Minute, Container: "mkv", Streams: []core.StreamInfo{
		{Index: 0, Type: "video", Codec: "h264", Width: 720, Height: 480, BitDepth: 8},
		{Index: 1, Type: "audio", Codec: "aac", Channels: 2},
	}}
	caps := appleTV
	caps.HLSContainers = []string{"mkv"}
	opts := allow()
	opts.DirectPlay, opts.DirectStream = false, false
	d := Decide(info, caps, opts, Policy{})
	s := startSession(t, m, core.HLSRequest{SessionID: "bt601", InputURL: "http://127.0.0.1:1/dvd", Info: info, Decision: d})
	if r := get(t, s, 0); r.Code != 200 {
		t.Fatalf("segment 0: %d %s", r.Code, r.Body.String())
	}
	if fr.startCount() != 2 {
		t.Fatalf("starts = %d, want zscale then colorspace", fr.startCount())
	}
	graphs := []string{}
	fr.mu.Lock()
	for _, args := range fr.starts {
		graphs = append(graphs, argValue(args, "-filter_complex"))
	}
	fr.mu.Unlock()
	if !strings.Contains(graphs[0], "zscale") || strings.Contains(graphs[1], "zscale") || !strings.Contains(graphs[1], "colorspace=") {
		t.Fatalf("graphs %q", graphs)
	}
}

func TestSessionMissingToneMapFilterDoesNotRetrySameGraph(t *testing.T) {
	fr := newFakeRunner()
	fr.failStart = func(args []string) (string, bool) {
		return "No such filter: 'zscale'", true
	}
	failures := make(chan error, 2)
	m := newTestManager(t, fr, config.Transcode{HWAccel: "none"}, WithFailureHandler(func(_ string, _ time.Duration, err error) {
		failures <- err
	}))
	// Strip every tone-map alternative so the retry has no second graph.
	m.feat.filters = map[string]bool{}
	for name := range testFeatures().filters {
		m.feat.filters[name] = true
	}
	for _, name := range []string{"libplacebo", "tonemap_opencl", "scale_vt", "tonemap_vaapi", "tonemap_cuda", "tonemap_videotoolbox", "colorspace"} {
		delete(m.feat.filters, name)
	}
	s := startSession(t, m, transcodeRequest(t, "hdr-noz"))
	r := get(t, s, 0)
	if r.Code != http.StatusBadGateway && r.Code != http.StatusInternalServerError {
		t.Fatalf("code %d %s", r.Code, r.Body.String())
	}
	if fr.startCount() != 1 {
		t.Fatalf("starts = %d, want one zscale attempt and no identical retry", fr.startCount())
	}
	select {
	case err := <-failures:
		if err == nil || !strings.Contains(err.Error(), "tone-map") || strings.Contains(err.Error(), "No such filter") {
			t.Fatalf("handler err %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("failure handler not called")
	}
	if !strings.Contains(m.Capability().PlaybackError, "tone-map") {
		t.Fatalf("capability %+v", m.Capability())
	}
}

func TestSessionInputFailure(t *testing.T) {
	fr := newFakeRunner()
	fr.failStart = func([]string) (string, bool) { return "http://x: Server returned 404 Not Found", true }
	failures := make(chan time.Duration, 4)
	m := newTestManager(t, fr, config.Transcode{}, WithFailureHandler(func(id string, at time.Duration, err error) {
		failures <- at
	}))
	s := startSession(t, m, remuxRequest(t, "bad", time.Hour))
	r := get(t, s, 0)
	if r.Code != http.StatusBadGateway {
		t.Fatalf("code %d", r.Code)
	}
	// The retry used a larger probe.
	sawBig := false
	fr.mu.Lock()
	for _, a := range fr.starts {
		if argValue(a, "-probesize") == "50000000" {
			sawBig = true
		}
	}
	fr.mu.Unlock()
	if !sawBig {
		t.Error("no retry with a larger probesize")
	}
	select {
	case <-failures:
	case <-time.After(2 * time.Second):
		t.Error("failure handler not called")
	}
}

func TestSessionMidStreamFailureReported(t *testing.T) {
	fr := newFakeRunner()
	fr.failAfter = 3
	got := make(chan time.Duration, 4)
	m := newTestManager(t, fr, config.Transcode{}, WithFailureHandler(func(id string, at time.Duration, err error) {
		if id == "mid" && err != nil {
			got <- at
		}
	}))
	startSession(t, m, remuxRequest(t, "mid", time.Hour))
	select {
	case at := <-got:
		if at != 12*time.Second {
			t.Errorf("failure at %v, want 12s", at)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("failure handler not called")
	}
}

func TestSessionSwapInput(t *testing.T) {
	fr := newFakeRunner()
	fr.segDelay = 10 * time.Millisecond
	m := newTestManager(t, fr, config.Transcode{})
	req := remuxRequest(t, "swap", time.Hour)
	s := startSession(t, m, req)
	if r := get(t, s, 0); r.Code != 200 {
		t.Fatal(r.Code)
	}
	// The new release has DTS instead of TrueHD+AC3, same language.
	newInfo := mustParse(t, "mkv_truehd_atmos.json")
	newInfo.Duration = time.Hour
	newInfo.Streams = []core.StreamInfo{newInfo.Streams[0], newInfo.Streams[3]}
	newInfo.Streams[1].Language = "eng"
	if err := s.SwapInput(context.Background(), "http://127.0.0.1:1/source/b", newInfo, 41*time.Second); err != nil {
		t.Fatal(err)
	}
	last := fr.lastStart()
	if argValue(last, "-i") != "http://127.0.0.1:1/source/b" || argValue(last, "-ss") != "40.000" {
		t.Errorf("swap args: %v", last)
	}
	r := get(t, s, 10)
	if r.Code != 200 || !strings.Contains(r.Body.String(), "input=http://127.0.0.1:1/source/b") {
		t.Fatalf("segment 10 after swap: %d %q", r.Code, r.Body.String())
	}
	d := s.Decision()
	if d.AudioStream != 3 || d.AudioCopy || d.AudioCodec != "eac3" || !d.VideoCopy {
		t.Errorf("adapted decision %+v", d)
	}
	if err := s.SwapInput(context.Background(), "", newInfo, 0); err == nil {
		t.Error("empty URL accepted")
	}
}

func TestAdaptDecisionReencodesChangedVideo(t *testing.T) {
	old := mustParse(t, "mkv_truehd_atmos.json") // h264
	hevc := mustParse(t, "mkv_hevc_hdr10.json")  // hevc HDR10
	d := Decide(old, appleTV, allow(), Policy{})
	nd := adaptDecision(d, old, hevc, ContainerMPEGTS)
	if nd.VideoCopy || nd.Kind != core.DecisionTranscode || nd.VideoCodec != "h264" || nd.MaxHeight != 1080 || !nd.ToneMap {
		t.Errorf("adapted %+v", nd)
	}
	if nd.Container != ContainerMPEGTS || !strings.Contains(nd.Reason, "failover") {
		t.Errorf("adapted %+v", nd)
	}
}

func TestSessionEndOfInput(t *testing.T) {
	fr := newFakeRunner()
	fr.lastSeg = 3 // the file is shorter than its reported duration
	m := newTestManager(t, fr, config.Transcode{})
	s := startSession(t, m, remuxRequest(t, "eof", time.Minute))
	if r := get(t, s, 3); r.Code != 200 {
		t.Fatalf("segment 3: %d", r.Code)
	}
	waitFor(t, "run to finish", func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.endSeg == 3
	})
	if r := get(t, s, 5); r.Code != 404 {
		t.Errorf("past EOF: %d", r.Code)
	}
}

func TestSessionFMP4Init(t *testing.T) {
	fr := newFakeRunner()
	m := newTestManager(t, fr, config.Transcode{})
	info := mustParse(t, "mkv_hevc_hdr10.json")
	info.Duration = time.Minute
	s := startSession(t, m, core.HLSRequest{SessionID: "f", InputURL: "http://h/x", Info: info, Decision: Decide(info, iOS, allow(), Policy{})})
	if s.Container() != ContainerFMP4 {
		t.Fatalf("container %s", s.Container())
	}
	r := get(t, s, InitSegment)
	if r.Code != 200 || r.Body.String() != string(testVideoInit(1280, 720)) || r.Header().Get("Content-Type") != "video/mp4" {
		t.Fatalf("init: %d %q", r.Code, r.Body.String())
	}
	if r := get(t, s, 1); r.Code != 200 {
		t.Fatalf("seg: %d", r.Code)
	}
	rec := httptest.NewRecorder()
	s.ServePlaylist(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if !strings.Contains(rec.Body.String(), `#EXT-X-MAP:URI="init.mp4"`) || !strings.Contains(rec.Body.String(), "00001.m4s") {
		t.Errorf("playlist %s", rec.Body.String())
	}
}

func TestSessionUnknownDurationEventPlaylist(t *testing.T) {
	fr := newFakeRunner()
	fr.lastSeg = 2
	m := newTestManager(t, fr, config.Transcode{})
	s := startSession(t, m, remuxRequest(t, "live", 0))
	waitFor(t, "eof", func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.endSeg == 2
	})
	rec := httptest.NewRecorder()
	s.ServePlaylist(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	b := rec.Body.String()
	if !strings.Contains(b, "EVENT") || strings.Count(b, "#EXTINF") != 3 || !strings.Contains(b, "ENDLIST") {
		t.Errorf("event playlist:\n%s", b)
	}
}

func TestMaxTranscodes(t *testing.T) {
	fr := newFakeRunner()
	m := newTestManager(t, fr, config.Transcode{MaxTranscodes: 1})
	startSession(t, m, transcodeRequest(t, "t1"))
	if _, err := m.StartHLS(context.Background(), transcodeRequest(t, "t2")); !errors.Is(err, ErrTooManyTranscodes) {
		t.Fatalf("err = %v", err)
	}
	// Remuxes are not limited, and the same ID may restart.
	startSession(t, m, remuxRequest(t, "r1", time.Hour))
	startSession(t, m, transcodeRequest(t, "t1"))
	if len(m.Sessions()) != 2 {
		t.Errorf("sessions = %d", len(m.Sessions()))
	}
	m.UpdateConfig(config.Transcode{MaxTranscodes: 0})
	startSession(t, m, transcodeRequest(t, "t2"))
}

func TestSessionReplaceSameID(t *testing.T) {
	fr := newFakeRunner()
	m := newTestManager(t, fr, config.Transcode{})
	a := startSession(t, m, remuxRequest(t, "same", time.Hour))
	pa := fr.lastProc()
	b := startSession(t, m, remuxRequest(t, "same", time.Hour))
	if a == b {
		t.Fatal("same session returned")
	}
	if !pa.wasKilled() {
		t.Error("replaced session's ffmpeg still running")
	}
	if _, err := os.Stat(a.dir); !os.IsNotExist(err) {
		t.Error("replaced session dir not removed")
	}
	if got, _ := m.Session("same"); got != core.HLSSession(b) {
		t.Error("lookup returns the old session")
	}
	if r := get(t, a, 0); r.Code != http.StatusGone {
		t.Errorf("closed session served %d", r.Code)
	}
}

func TestIdleReaping(t *testing.T) {
	fr := newFakeRunner()
	m := newTestManager(t, fr, config.Transcode{}, WithIdleTimeout(150*time.Millisecond))
	s := startSession(t, m, remuxRequest(t, "idle", time.Hour))
	p := fr.lastProc()
	waitFor(t, "reaping", func() bool { _, ok := m.Session("idle"); return !ok })
	if !p.wasKilled() {
		t.Error("ffmpeg not killed")
	}
	if _, err := os.Stat(s.dir); !os.IsNotExist(err) {
		t.Error("dir not removed")
	}
}

func TestTouchKeepsSessionAlive(t *testing.T) {
	fr := newFakeRunner()
	m := newTestManager(t, fr, config.Transcode{}, WithIdleTimeout(200*time.Millisecond))
	s := startSession(t, m, remuxRequest(t, "touch", time.Hour))
	for i := 0; i < 8; i++ {
		time.Sleep(50 * time.Millisecond)
		s.Touch()
	}
	if _, ok := m.Session("touch"); !ok {
		t.Error("touched session reaped")
	}
}

func TestPlaylistPollingCannotPreserveAbandonedEncoder(t *testing.T) {
	fr := newFakeRunner()
	m := newTestManager(t, fr, config.Transcode{})
	s := startSession(t, m, remuxRequest(t, "manifest-only", time.Hour))
	p := fr.lastProc()
	last := time.Now().Add(-3 * time.Minute)
	s.lastAccess.Store(last.UnixNano())
	rec := httptest.NewRecorder()
	s.ServePlaylist(rec, httptest.NewRequest("GET", "/index.m3u8", nil))
	if rec.Code != 200 {
		t.Fatal(rec.Code)
	}
	m.reapIdle(time.Now())
	if _, ok := m.Session(s.ID()); ok || !p.wasKilled() {
		t.Fatal("manifest polling preserved abandoned encoder")
	}
}

func TestStopAndClose(t *testing.T) {
	fr := newFakeRunner()
	m := newTestManager(t, fr, config.Transcode{})
	s := startSession(t, m, remuxRequest(t, "stop", time.Hour))
	p := fr.lastProc()
	m.Stop("stop")
	if !p.wasKilled() {
		t.Error("not killed")
	}
	if _, ok := m.Session("stop"); ok {
		t.Error("still registered")
	}
	if err := s.Close(); err != nil {
		t.Errorf("second close: %v", err)
	}
	s2 := startSession(t, m, remuxRequest(t, "x", time.Hour))
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	if r := get(t, s2, 0); r.Code != http.StatusGone {
		t.Errorf("after manager close: %d", r.Code)
	}
	if _, err := m.StartHLS(context.Background(), remuxRequest(t, "y", time.Hour)); err == nil {
		t.Error("StartHLS after Close succeeded")
	}
}

func TestThrottlePausesAndResumes(t *testing.T) {
	fr := newFakeRunner()
	fr.segDelay = time.Millisecond
	m := newTestManager(t, fr, config.Transcode{}, WithBuffering(24*time.Second, 16*time.Second, 0))
	s := startSession(t, m, remuxRequest(t, "thr", time.Hour))
	p := fr.lastProc()
	waitFor(t, "pause", p.isPaused)
	s.mu.Lock()
	ahead := s.cur.maxDone
	s.mu.Unlock()
	// The upper bound depends on how far the poller lagged ffmpeg.
	if ahead < 4 {
		t.Errorf("paused at segment %d", ahead)
	}
	// Player caught up with the paused encoder. Under load the poller can
	// see ffmpeg pause well past the limit (and one more segment may land
	// after the pause), so request the paused frontier itself rather than
	// a fixed segment that may still be more than limit/2 behind it.
	if r := get(t, s, ahead); r.Code != 200 {
		t.Fatal(r.Code)
	}
	// ffmpeg resumed and produces more.
	waitFor(t, "progress past pause", func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.cur != nil && s.cur.maxDone > ahead
	})
}

func TestThrottleStopsWhenPauseUnsupported(t *testing.T) {
	fr := newFakeRunner()
	fr.segDelay = time.Millisecond
	fr.noPause = true
	m := newTestManager(t, fr, config.Transcode{}, WithBuffering(24*time.Second, 16*time.Second, 0))
	s := startSession(t, m, remuxRequest(t, "nopause", time.Hour))
	first := fr.lastProc()
	waitFor(t, "throttle stop", first.wasKilled)
	s.mu.Lock()
	resumeAt := s.resumeAt
	s.mu.Unlock()
	if resumeAt < 5 { // no upper bound: it depends on how far the poller lagged
		t.Fatalf("resumeAt = %d", resumeAt)
	}
	if fr.startCount() != 1 {
		t.Fatalf("starts = %d", fr.startCount())
	}
	// The player catches up: ffmpeg restarts at the next missing segment
	// before the player needs it.
	if r := get(t, s, resumeAt-2); r.Code != 200 {
		t.Fatal(r.Code)
	}
	waitFor(t, "restart", func() bool { return fr.startCount() == 2 })
	if got, want := argValue(fr.lastStart(), "-ss"), secs(time.Duration(resumeAt)*4*time.Second); got != want {
		t.Errorf("restarted at %s, want segment %d at %s", got, resumeAt, want)
	}
	if r := get(t, s, resumeAt+1); r.Code != 200 {
		t.Fatal(r.Code)
	}
}

func TestPruneDeletesOldSegments(t *testing.T) {
	fr := newFakeRunner()
	fr.segDelay = time.Millisecond
	m := newTestManager(t, fr, config.Transcode{}, WithBuffering(24*time.Second, 0, 20*time.Second))
	s := startSession(t, m, remuxRequest(t, "prune", time.Hour))
	waitFor(t, "encoder to pass 31", func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.cur != nil && s.cur.maxDone > 31
	})
	if r := get(t, s, 30); r.Code != 200 {
		t.Fatal(r.Code)
	}
	if r := get(t, s, 31); r.Code != 200 {
		t.Fatal(r.Code)
	}
	if _, err := os.Stat(s.segPath(2)); !os.IsNotExist(err) {
		t.Error("old segment 2 not pruned")
	}
	if _, err := os.Stat(s.segPath(28)); err != nil {
		t.Error("recent segment 28 pruned")
	}
	// A pruned segment is produced again on demand.
	if r := get(t, s, 2); r.Code != 200 || !strings.Contains(r.Body.String(), "segment 2") {
		t.Fatalf("re-produce: %d", r.Code)
	}
}

func TestConcurrentSegmentRequests(t *testing.T) {
	fr := newFakeRunner()
	fr.segDelay = 2 * time.Millisecond
	m := newTestManager(t, fr, config.Transcode{})
	s := startSession(t, m, remuxRequest(t, "conc", time.Hour))
	var wg sync.WaitGroup
	errs := make(chan string, 64)
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			base := g * 37
			for i := 0; i < 5; i++ {
				n := base + i
				// Requests overtaken by a newer far request are told to retry.
				if r := get(t, s, n); r.Code != 200 && r.Code != http.StatusBadGateway && r.Code != http.StatusServiceUnavailable {
					errs <- fmt.Sprintf("segment %d: %d", n, r.Code)
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

func TestStartHLSValidation(t *testing.T) {
	fr := newFakeRunner()
	m := newTestManager(t, fr, config.Transcode{})
	ctx := context.Background()
	req := remuxRequest(t, "", time.Hour)
	req.InputURL = ""
	if _, err := m.StartHLS(ctx, req); err == nil {
		t.Error("empty input accepted")
	}
	req = remuxRequest(t, "", time.Hour)
	req.Decision = core.Decision{}
	if _, err := m.StartHLS(ctx, req); err == nil {
		t.Error("empty decision accepted")
	}
	// Direct play decisions are remuxed; empty IDs are generated.
	req = remuxRequest(t, "", time.Hour)
	req.Decision = Decide(req.Info, appleTV, allow(), Policy{})
	req.Decision.Kind = core.DecisionDirectPlay
	s := startSession(t, m, req)
	if s.ID() == "" || s.Decision().Kind != core.DecisionRemux {
		t.Errorf("id %q decision %+v", s.ID(), s.Decision())
	}
	fr.startError = errors.New("exec: no such file")
	if _, err := m.StartHLS(ctx, remuxRequest(t, "z", time.Hour)); err == nil {
		t.Error("start error not returned")
	}
	if _, ok := m.Session("z"); ok {
		t.Error("failed session registered")
	}
}

func TestManagerBasics(t *testing.T) {
	fr := newFakeRunner()
	m := newTestManager(t, fr, config.Transcode{HWAccel: "none", RemoteMaxBitrateKbps: 3000})
	if m.Version() != "7.1-fake" {
		t.Errorf("version %q", m.Version())
	}
	if hw := m.HWAccel(); hw.Name != AccelNone || !contains(hw.Encoders, "libx264") {
		t.Errorf("hw %+v", hw)
	}
	pol := m.Policy()
	if pol.RemoteMaxBitrateKbps != 3000 || pol.HEVCEncoder || pol.HEVCHardware {
		t.Errorf("policy %+v", pol)
	}
	mp4 := mustParse(t, "mp4_h264_aac.json")
	if d := m.Decide(mp4, withRemote(appleTV), allow()); d.Kind != core.DecisionTranscode {
		t.Errorf("remote cap not applied: %+v", d)
	}
	fr.probeJSON = []string{string(loadGolden(t, "mp4_h264_aac.json"))}
	info, err := m.Probe(context.Background(), "http://h/x")
	if err != nil || info.Container != "mp4" {
		t.Fatalf("probe %v %+v", err, info)
	}
	if m.FFmpegPath() == "" || m.FFprobePath() == "" {
		t.Error("paths empty")
	}
	var out strings.Builder
	if err := m.ExtractSubtitle(context.Background(), "http://h/x", 2, "pgs", &out); !errors.Is(err, ErrImageSubtitle) {
		t.Errorf("image subtitle err %v", err)
	}
}

func TestNewStaleCleanupAndNoFFmpeg(t *testing.T) {
	dir := t.TempDir()
	stale := dir + "/s-old-1234"
	if err := os.MkdirAll(stale, 0o755); err != nil {
		t.Fatal(err)
	}
	keep := dir + "/other"
	_ = os.MkdirAll(keep, 0o755)
	fr := newFakeRunner()
	newTestManager(t, fr, config.Transcode{TempDir: dir})
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Error("stale session dir kept")
	}
	if _, err := os.Stat(keep); err != nil {
		t.Error("unrelated dir removed")
	}
	_, err := New(config.Transcode{FFmpegPath: dir + "/missing"}, nil, WithRunner(fr))
	if !errors.Is(err, ErrFFmpegNotFound) {
		t.Errorf("err = %v", err)
	}
}

type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// Wiring: a real session (fake runner) whose runs fall back HEVC->libx264
// must warn at most once per session, however many runs startRunLocked
// starts (StartHLS's first run plus a seek restart here). Removing the
// call site in startRunLocked makes the count 0 and fails this test.
func TestSessionHEVCEncoderFallbackWarnsOncePerSession(t *testing.T) {
	fr := newFakeRunner()
	m := newTestManager(t, fr, config.Transcode{})
	var buf lockedBuffer
	m.log = slog.New(slog.NewTextHandler(&buf, nil))
	// No HW HEVC and no libx265: HEVC encodes fall back to libx264.
	// Filters stay probed so the HDR tone map is a real graph; an unread
	// filter list is not treated as zscale.
	m.hw = core.HWAccel{Name: AccelNone}
	m.feat = features{
		encoders:      map[string]bool{"libx264": true},
		filters:       map[string]bool{"zscale": true, "tonemap": true, "scale": true},
		filtersProbed: true,
	}

	req := transcodeRequest(t, "hevc-fallback-once")
	req.Decision.VideoCodec = "hevc"
	req.Decision.VideoCopy = false
	s := startSession(t, m, req) // first run: warns

	s.mu.Lock()
	err := s.startRunLocked(5, false) // seek restart: must not warn again
	s.mu.Unlock()
	if err != nil {
		t.Fatalf("second startRunLocked: %v", err)
	}
	if n := fr.startCount(); n != 2 {
		t.Fatalf("ffmpeg starts = %d, want 2 (two runs went through startRunLocked)", n)
	}
	if n := strings.Count(buf.String(), "falling back to libx264"); n != 1 {
		t.Errorf("HEVC fallback warnings = %d, want exactly 1 per session; log:\n%s", n, buf.String())
	}
}

func sessionStartedCopy(fr *fakeRunner, sessionID string) bool {
	fr.mu.Lock()
	defer fr.mu.Unlock()
	needle := "/s-" + sessionID + "-"
	for _, args := range fr.starts {
		if strings.Contains(strings.Join(args, " "), needle) && argValue(args, "-c:v") == "copy" {
			return true
		}
	}
	return false
}

// The transcode cap counts encoders, not decisions: a copy session whose
// run encodes (a fast-start ramp or a lower rung) holds a slot, and a copy
// session started at the cap plays plainly rather than encoding past it.
func TestMaxTranscodesCountsEncodingCopySessions(t *testing.T) {
	fr := newFakeRunner()
	cfg := fastStartConfig()
	cfg.MaxTranscodes, cfg.ABRLadder, cfg.ABRLadderUnderCopy = 1, true, true
	m := newTestManager(t, fr, cfg)
	startSession(t, m, transcodeRequest(t, "t1"))
	// At the cap the copy session still starts, without the encoded ramp
	// or lower rungs.
	s := startSession(t, m, remuxRequest(t, "r1", 10*time.Minute))
	// t1 can chain another encode after r1 starts. The copy is r1's run,
	// not whichever ffmpeg happened to start last.
	if !sessionStartedCopy(fr, "r1") {
		t.Fatalf("capped copy session did not start with -c:v copy; last %q", fr.lastStart())
	}
	if n := len(s.Rungs()); n != 1 {
		t.Errorf("rungs at the cap = %d, want the primary only", n)
	}

	fr2 := newFakeRunner()
	m2 := newTestManager(t, fr2, config.Transcode{MaxTranscodes: 1, ABRLadder: true, ABRLadderUnderCopy: true})
	s2 := startSession(t, m2, remuxRequest(t, "r2", 10*time.Minute))
	if len(s2.Rungs()) < 2 {
		t.Fatalf("under-copy ladder missing: %+v", s2.Rungs())
	}
	if _, err := m2.StartHLS(context.Background(), transcodeRequest(t, "t2")); err != nil {
		t.Fatalf("copy session blocked a transcode: %v", err)
	}
	m2.Stop("t2")
	rec := httptest.NewRecorder()
	s2.ServeRungSegment(rec, httptest.NewRequest(http.MethodGet, "/720p4m/00001.ts", nil), "720p4m", 1)
	if rec.Code != 200 {
		t.Fatalf("rung segment: %d", rec.Code)
	}
	if _, err := m2.StartHLS(context.Background(), transcodeRequest(t, "t3")); !errors.Is(err, ErrTooManyTranscodes) {
		t.Fatalf("a copy session encoding a lower rung did not count: err = %v", err)
	}
}

func TestPausedSessionSurvivesPhoneCallAndStillExpires(t *testing.T) {
	for _, resume := range []bool{false, true} {
		t.Run(fmt.Sprint(resume), func(t *testing.T) {
			fr := newFakeRunner()
			m := newTestManager(t, fr, config.Transcode{})
			s := startSession(t, m, remuxRequest(t, "phone-call", time.Hour))
			s.PlaybackState("paused")
			m.reapIdle(time.Now().Add(5 * time.Minute))
			if _, ok := m.Session(s.ID()); !ok {
				t.Fatal("paused session reaped during phone call")
			}
			if resume {
				s.PlaybackState("playing")
				s.lastAccess.Store(time.Now().Add(-3 * time.Minute).UnixNano())
				m.reapIdle(time.Now())
			} else {
				m.reapIdle(time.Now().Add(core.PausedSessionRetention + time.Minute))
			}
			if _, ok := m.Session(s.ID()); ok {
				t.Fatal("abandoned session retained indefinitely")
			}
		})
	}
}
