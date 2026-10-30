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

func TestMatroskaNativeRenditionsCannotReplaceActiveEncoderDuringProbes(t *testing.T) {
	fr := newFakeRunner()
	m := newTestManager(t, fr, config.Transcode{ABRLadder: true})
	req := remuxRequest(t, "native-single-rendition", time.Hour)
	req.Decision.Container = ContainerMatroska
	// Encode primaries still get a ladder; copy/Direct Stream sessions do not.
	enc := req.Decision
	enc.VideoCopy, enc.Kind = false, core.DecisionTranscode
	enc.VideoCodec, enc.BitrateKbps = "h264", 20000
	if len(buildRungs(req.Info, enc, adaptiveSettings{Ladder: true})) < 2 {
		t.Fatal("fixture must otherwise produce multiple qualities")
	}
	s := startSession(t, m, req)
	if rungs := s.Rungs(); len(rungs) != 1 || rungs[0].ID != "base" {
		t.Fatalf("native player can probe speculative encoders: %+v", rungs)
	}
	for _, rung := range []string{"1080p8m", "720p4m", "480p1500k", "360p800k"} {
		rec := httptest.NewRecorder()
		s.ServeRungSegment(rec, httptest.NewRequest("GET", "/header", nil), rung, InitSegment)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("unadvertised rung %s = %d", rung, rec.Code)
		}
	}
	if fr.startCount() != 1 {
		t.Fatalf("header probes restarted encoder %d times", fr.startCount())
	}
	if rec := get(t, s, 0); rec.Code != 200 {
		t.Fatalf("base stream interrupted: %d", rec.Code)
	}
}

func ladderInfo(h, bitrate int) core.MediaInfo {
	return core.MediaInfo{
		Container: "mkv", Duration: 10 * time.Minute, BitrateKbps: bitrate,
		Streams: []core.StreamInfo{
			{Index: 0, Type: "video", Codec: "h264", Width: h * 16 / 9, Height: h, FrameRate: 23.976},
			{Index: 1, Type: "audio", Codec: "aac", Channels: 2, SampleRate: 48000, BitrateKbps: 192},
		},
	}
}

func TestBuildRungsBelowSourceOnly(t *testing.T) {
	adapt := adaptiveSettings{Ladder: true}
	info := ladderInfo(2160, 40000)
	// Video copy without LadderUnderCopy: primary only.
	copyPrimary := core.Decision{Kind: core.DecisionRemux, VideoCopy: true, AudioCopy: true, AudioStream: 1, Container: ContainerMPEGTS}
	if got := buildRungs(info, copyPrimary, adapt); len(got) != 1 || got[0].id != "base" || got[0].height != 2160 {
		t.Fatalf("copy ladder = %+v", got)
	}
	// With LadderUnderCopy: full soft range (legacy abrLadder under copy).
	under := adaptiveSettings{Ladder: true, LadderUnderCopy: true}
	got := buildRungs(info, copyPrimary, under)
	var underIDs []string
	for _, r := range got {
		underIDs = append(underIDs, r.id)
	}
	if strings.Join(underIDs, ",") != "base,1080p8m,720p4m,480p1500k,360p800k" {
		t.Fatalf("under-copy ladder = %v", underIDs)
	}
	for _, r := range got[1:] {
		if r.decision.VideoCopy || r.decision.VideoCodec != "h264" {
			t.Errorf("under-copy soft rung %+v / %+v", r, r.decision)
		}
	}
	// Transcode primary: cheaper rungs below the source.
	primary := core.Decision{Kind: core.DecisionTranscode, VideoCodec: "h264", MaxHeight: 2160, BitrateKbps: 40000, AudioStream: 1, AudioCodec: "aac", Container: ContainerMPEGTS}
	rungs := buildRungs(info, primary, adapt)
	var ids []string
	for _, r := range rungs {
		ids = append(ids, r.id)
	}
	want := []string{"base", "1080p8m", "720p4m", "480p1500k", "360p800k"}
	if strings.Join(ids, ",") != strings.Join(want, ",") {
		t.Fatalf("rungs = %v", ids)
	}
	if rungs[0].height != 2160 || rungs[1].height != 1080 || rungs[1].decision.BitrateKbps != 8000 || rungs[1].decision.VideoCopy {
		t.Errorf("rungs: %+v / %+v", rungs[0], rungs[1].decision)
	}
	if !strings.HasPrefix(rungs[1].codecs, "avc1.") || !strings.Contains(rungs[1].codecs, "mp4a") {
		t.Errorf("codecs %q", rungs[1].codecs)
	}
	// 720p source: no 1080/720 rungs.
	rungs = buildRungs(ladderInfo(720, 5000), primary, adapt)
	if len(rungs) != 3 || rungs[1].id != "480p1500k" {
		t.Errorf("720p rungs = %+v", rungs)
	}
	// Ladder off: just the primary.
	if got := buildRungs(info, primary, adaptiveSettings{}); len(got) != 1 {
		t.Errorf("ladder off = %d rungs", len(got))
	}
}

// ladderEncodeRequest is a high-bitrate encode primary so ABR can attach
// cheaper rungs (video-copy sessions no longer get a ladder).
func ladderEncodeRequest(t *testing.T, id string, dur time.Duration) core.HLSRequest {
	t.Helper()
	req := remuxRequest(t, id, dur)
	v := req.Info.Video()
	h, br := 1080, 12000
	if v != nil {
		if v.Height > 0 {
			h = v.Height
		}
		if v.BitrateKbps > 0 {
			br = max(v.BitrateKbps, 12000)
		}
	}
	req.Decision = core.Decision{
		Kind: core.DecisionTranscode, VideoCodec: "h264", MaxHeight: h, BitrateKbps: br,
		AudioStream: 1, AudioCodec: "aac", Container: ContainerMPEGTS,
	}
	return req
}

func ladderEncodeDASHRequest(t *testing.T, id string, dur time.Duration) core.HLSRequest {
	t.Helper()
	req := ladderEncodeRequest(t, id, dur)
	req.Protocol = core.ProtocolDASH
	req.Decision.Container = ContainerFMP4
	return req
}

func TestBuildRungsRespectCap(t *testing.T) {
	adapt := adaptiveSettings{Ladder: true}
	// Primary already transcoded down to a 4000 kbps / 720p cap: rungs at
	// or above it must be omitted.
	info := ladderInfo(2160, 40000)
	primary := core.Decision{Kind: core.DecisionTranscode, VideoCodec: "h264", MaxHeight: 720, BitrateKbps: 4000, AudioStream: 1, AudioCodec: "aac", Container: ContainerMPEGTS}
	rungs := buildRungs(info, primary, adapt)
	if len(rungs) != 3 {
		t.Fatalf("rungs = %+v", rungs)
	}
	if rungs[1].id != "480p1500k" || rungs[2].id != "360p800k" {
		t.Errorf("capped rungs = %v, %v", rungs[1].id, rungs[2].id)
	}
	if rungs[0].height != 720 || rungs[0].avgKbps != 4000 {
		t.Errorf("base = %+v", rungs[0])
	}
}

func TestLadderSessionLazyRungStart(t *testing.T) {
	fr := newFakeRunner()
	m := newTestManager(t, fr, config.Transcode{ABRLadder: true})
	s := startSession(t, m, ladderEncodeRequest(t, "lad", 10*time.Minute))
	rungs := s.Rungs()
	if len(rungs) < 3 || rungs[0].ID != "base" {
		t.Fatalf("rungs = %+v", rungs)
	}
	var low core.HLSRung
	for _, r := range rungs {
		if r.ID == "720p4m" {
			low = r
		}
	}
	if low.ID == "" || low.Height != 720 || low.Bandwidth <= 0 || low.Codecs == "" {
		t.Fatalf("720 rung = %+v", low)
	}
	// Primary only until a lower rung is requested.
	if r := get(t, s, 3); r.Code != 200 {
		t.Fatalf("base segment 3: %d", r.Code)
	}
	if fr.startCount() != 1 {
		t.Fatalf("starts = %d, want 1", fr.startCount())
	}
	primaryProc := fr.lastProc()
	// Rung playlist shares the timeline numbering.
	rec := httptest.NewRecorder()
	s.ServeRungPlaylist(rec, httptest.NewRequest(http.MethodGet, "/720p4m/index.m3u8", nil), "720p4m")
	if n := strings.Count(rec.Body.String(), "#EXTINF"); n != 150 {
		t.Errorf("rung playlist segments = %d", n)
	}
	// A segment on the lower rung lazily switches the one encoder there.
	rec = httptest.NewRecorder()
	s.ServeRungSegment(rec, httptest.NewRequest(http.MethodGet, "/720p4m/00010.ts", nil), "720p4m", 10)
	if rec.Code != 200 || !strings.HasPrefix(rec.Body.String(), "segment 10") {
		t.Fatalf("rung segment 10: %d %q", rec.Code, rec.Body.String())
	}
	if !primaryProc.wasKilled() {
		t.Error("primary encoder not stopped when the rung took over")
	}
	last := fr.lastStart()
	mustContain(t, last, "-start_number 10", "scale_vt=w=1280:h=720")
	mustNotContain(t, last, "-c:v copy")
	if !strings.Contains(argValue(last, "-hls_segment_filename"), "rung-720p4m") {
		t.Errorf("rung segment dir: %v", last)
	}
	// Unknown rung 404s.
	rec = httptest.NewRecorder()
	s.ServeRungSegment(rec, httptest.NewRequest(http.MethodGet, "/x", nil), "nope", 0)
	if rec.Code != http.StatusNotFound {
		t.Errorf("unknown rung: %d", rec.Code)
	}
	// Primary segments already on disk are still served without a restart.
	starts := fr.startCount()
	if r := get(t, s, 1); r.Code != 200 {
		t.Fatalf("base segment 1: %d", r.Code)
	}
	if fr.startCount() != starts {
		t.Error("serving a finished base segment restarted the encoder")
	}
}

func TestDASHLadderMPDAndLazyRep(t *testing.T) {
	fr := newFakeRunner()
	m := newTestManager(t, fr, config.Transcode{ABRLadder: true})
	s := startDASHSession(t, m, ladderEncodeDASHRequest(t, "dlad", 10*time.Minute))
	rec := httptest.NewRecorder()
	s.ServeManifest(rec, httptest.NewRequest(http.MethodGet, "/start.mpd", nil))
	body := rec.Body.String()
	if rec.Code != 200 {
		t.Fatalf("mpd: %d %s", rec.Code, body)
	}
	videoReps := strings.Count(body, "mimeType=\"video/mp4\"")
	if videoReps < 2 {
		t.Fatalf("video representations = %d\n%s", videoReps, body)
	}
	if !strings.Contains(body, "contentType=\"video\"") {
		t.Errorf("video reps are not one AdaptationSet:\n%s", body)
	}
	// Every representation is $Number$-aligned on the session timeline.
	if !strings.Contains(body, "startNumber=\"0\"") {
		t.Errorf("no aligned startNumber:\n%s", body)
	}
	// The extra representation (track 2) serves segments, switching the
	// encoder to that rung's decision.
	rec = httptest.NewRecorder()
	s.ServeMedia(rec, httptest.NewRequest(http.MethodGet, "/2/0.m4s", nil), 2, 0)
	if rec.Code != 200 || rec.Body.Len() == 0 {
		t.Fatalf("rep 2 segment 0: %d", rec.Code)
	}
}

// A request on another rung inside the running rung's dwell waits it out
// instead of restarting the shared encoder: rendition probes cannot keep
// stopping the active run, and the real switch still happens.
func TestRungSwitchWaitsOutDwell(t *testing.T) {
	fr := newFakeRunner()
	m := newTestManager(t, fr, config.Transcode{ABRLadder: true}, withRungDwell(400*time.Millisecond))
	s := startSession(t, m, ladderEncodeRequest(t, "dwell", 10*time.Minute))
	if r := get(t, s, 0); r.Code != 200 {
		t.Fatalf("base segment 0: %d", r.Code)
	}
	primary := fr.lastProc()
	started := time.Now()
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		rec := httptest.NewRecorder()
		s.ServeRungSegment(rec, httptest.NewRequest(http.MethodGet, "/720p4m/00000.ts", nil), "720p4m", 0)
		done <- rec
	}()
	time.Sleep(150 * time.Millisecond)
	if fr.startCount() != 1 || primary.wasKilled() {
		t.Fatalf("a rung probe replaced the encoder inside the dwell: starts=%d killed=%v", fr.startCount(), primary.wasKilled())
	}
	rec := <-done
	if rec.Code != 200 || !strings.HasPrefix(rec.Body.String(), "segment 0") {
		t.Fatalf("rung segment after dwell: %d %q", rec.Code, rec.Body.String())
	}
	if waited := time.Since(started); waited < 350*time.Millisecond {
		t.Errorf("switched after %v, before the dwell passed", waited)
	}
	if fr.startCount() != 2 || !primary.wasKilled() {
		t.Errorf("encoder not switched to the rung after the dwell: starts=%d", fr.startCount())
	}
	mustContain(t, fr.lastStart(), "scale_vt=w=1280:h=720", "-start_number 0")
}
