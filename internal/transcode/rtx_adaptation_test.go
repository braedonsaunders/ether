package transcode

import (
	"net/http/httptest"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

func TestRTXOrdinaryLadderAndRecovery(t *testing.T) {
	for _, proto := range []string{core.ProtocolHLS, "hls-fmp4", core.ProtocolDASH} {
		t.Run(proto, func(t *testing.T) {
			m, rr, req := newRTXSessionManager(t)
			rr.onOrdinaryStart = func() {
				if m.EnhancementStatus().Active != 0 {
					t.Error("ordinary encoder started before native permit release")
				}
			}
			m.cfg.ABRLadder, m.cfg.ABRLadderUnderCopy, m.cfg.FastStart = true, true, true
			m.opts.rungDwell = 0 // exercise lifecycle independently of the shared dwell policy
			req.Protocol = proto
			if proto == "hls-fmp4" {
				req.Protocol = core.ProtocolHLS
				req.Decision.Container = ContainerFMP4
			}
			req.SegmentDuration = 4 * time.Second
			for n := 0; n < 300; n++ {
				req.Info.Keyframes = append(req.Info.Keyframes, core.KeyframePoint{Time: time.Duration(n) * 3 * time.Second})
			}
			s := startSession(t, m, req)
			if s.adapt.FastStart || len(s.rungs) < 3 || s.rungs[0].height != 1080 {
				t.Fatalf("adaptive plan %+v / %+v", s.adapt, s.rungs)
			}
			for i, r := range s.rungs {
				if i > 0 && (r.decision.Enhancement.Backend != "" || r.height > 720 || r.width > 1280) {
					t.Fatalf("ordinary rung %+v", r)
				}
			}
			serve := func(id string, n int) {
				t.Helper()
				rec := httptest.NewRecorder()
				s.ServeRungSegment(rec, httptest.NewRequest("GET", "/segment", nil), id, n)
				if rec.Code != 200 {
					t.Fatalf("%s/%d: %d %s", id, n, rec.Code, rec.Body.String())
				}
			}
			serve("base", 0)
			waitFor(t, "genuine base running", func() bool { return s.Decision().Enhancement.State == "running" })
			native := rr.fake.lastProc()
			serve("480p1500k", 4)
			if !native.wasKilled() || m.EnhancementStatus().Active != 0 || s.Decision().Enhancement.State != "fallback" {
				t.Fatalf("ordinary takeover did not reap VSR: %+v", s.Decision())
			}
			if rr.count() != 1 || s.cur.rtx || s.cur.spec.Decision.MaxHeight != 480 || s.cur.spec.Decision.Enhancement.Backend != "" {
				t.Fatalf("lower run %+v", s.cur.spec.Decision)
			}
			if proto == core.ProtocolHLS {
				mustContain(t, rr.fake.lastStart(), "-muxdelay 0 -muxpreload 0")
			}
			serve("360p800k", 8)
			if s.cur.spec.Decision.MaxHeight != 360 || rr.count() != 1 {
				t.Fatal("360p started enhancement")
			}
			serve("base", 12)
			waitFor(t, "recovered native base", func() bool { return s.Decision().Enhancement.State == "running" })
			if rr.count() != 2 || s.Decision().Enhancement.Width != 1920 || s.cur.spec.Start != 48*time.Second {
				t.Fatalf("bad recovery: %+v", s.Decision())
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			if m.EnhancementStatus().Active != 0 {
				t.Fatal("permit leaked")
			}
		})
	}
}

func TestRTXOrdinaryClockArgsAreScoped(t *testing.T) {
	for _, proto := range []string{core.ProtocolHLS, core.ProtocolDASH} {
		for _, container := range []string{ContainerMPEGTS, ContainerFMP4} {
			for _, origin := range []bool{false, true} {
				spec := rtxSpec(t)
				spec.Decision.Enhancement = core.VideoEnhancement{}
				spec.Decision.Container, spec.Protocol, spec.RTXTimeline = container, proto, origin
				a := buildArgs(spec)
				if proto == core.ProtocolHLS && container == ContainerFMP4 {
					want := spec.InitName
					if runtime.GOOS == "windows" {
						want = filepath.Join(spec.Dir, spec.InitName)
					}
					if got := argValue(a, "-hls_fmp4_init_filename"); got != want {
						t.Fatalf("init path=%q want=%q", got, want)
					}
				}
				if want := origin && proto == core.ProtocolHLS && container == ContainerMPEGTS; strings.Contains(strings.Join(a, " "), "-muxdelay 0 -muxpreload 0") != want {
					t.Fatalf("clock hook scope proto=%s container=%s origin=%v: %v", proto, container, origin, a)
				}
				if proto == core.ProtocolHLS && container == ContainerMPEGTS {
					want := ""
					if origin {
						want = "mpegts_copyts=1:avoid_negative_ts=disabled"
					}
					if got := argValue(a, "-hls_segment_options"); got != want {
						t.Fatalf("child TS clock options=%q want=%q", got, want)
					}
				}
			}
		}
	}
}

func TestRTXManifestCodecsMatchNativeLevel(t *testing.T) {
	for _, fps := range []float64{24, 60} {
		info, caps, opts := rtxFixture()
		info.Streams[0].FrameRate = fps
		d := rtxManager().Decide(info, caps, opts)
		rungs := buildRungs(info, d, adaptiveSettings{Ladder: true})
		want := "avc1.640029"
		if fps > 30 {
			want = "avc1.640033"
		}
		if !strings.HasPrefix(rungs[0].codecs, want+",") {
			t.Fatalf("enhanced codec %s want %s", rungs[0].codecs, want)
		}
		if got := dashRepsLadder(info, d, rungs)[0].Codecs; got != want {
			t.Fatalf("DASH codec %s want %s", got, want)
		}
		for _, r := range rungs[1:] {
			if enhancementSelected(r.decision) || r.height > 720 {
				t.Fatalf("bad ordinary rung %+v", r)
			}
		}
	}
}

func TestRTXFallbackKeepsPerRungMetadataAndTimeline(t *testing.T) {
	m, rr, req := newRTXSessionManager(t)
	m.cfg.ABRLadder, m.cfg.ABRLadderUnderCopy, m.cfg.FastStart = true, true, true
	req.Protocol = core.ProtocolDASH
	rr.fail = true
	s := startSession(t, m, req)
	rec := httptest.NewRecorder()
	s.ServeMedia(rec, httptest.NewRequest("GET", "/segment", nil), 0, 0)
	if rec.Code != 200 {
		t.Fatal(rec.Code)
	}
	if s.Decision().Enhancement.State != "fallback" {
		t.Fatal(s.Decision())
	}
	for _, rep := range s.dash.reps {
		if rep.Kind == "video" {
			r := s.rungs[rungForDashTrack(rep.ID)]
			if rep.Width != r.width || rep.Height != r.height || rep.Codecs == "" {
				t.Fatalf("stale rep %+v vs %+v", rep, r)
			}
		}
	}
	if s.adapt.FastStart || s.segStart(2) != 2*s.segDur {
		t.Fatal("fallback changed advertised clock")
	}
	if rr.count() != 1 || m.EnhancementStatus().Active != 0 {
		t.Fatal("fallback leaked/retried")
	}
}

func TestRTXBandwidthRecoveryStaysOrdinary(t *testing.T) {
	m, rr, req := newRTXSessionManager(t)
	m.cfg.ABRLadder, m.cfg.ABRLadderUnderCopy = true, true
	m.opts.rungDwell = 0
	s := startSession(t, m, req)
	if rec := get(t, s, 0); rec.Code != 200 {
		t.Fatal(rec.Code)
	}
	s.goodput.add(100000, time.Second) // 800 kbps cannot support the enhanced base
	s.mu.Lock()
	s.wantTrack = 0
	err := s.startRunLocked(20, false)
	s.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if d := s.Decision(); d.Enhancement.State != "fallback" || !strings.Contains(d.Enhancement.Reason, "bandwidth") {
		t.Fatal(d)
	}
	if rr.count() != 1 || m.EnhancementStatus().Active != 0 || s.cur.rtx {
		t.Fatal("low bandwidth restarted enhancement")
	}
	s.mu.Lock()
	stale := s.done[0][0]
	s.mu.Unlock()
	if stale {
		t.Fatal("enhanced base cache survived ordinary resolution change")
	}
	if rec := get(t, s, 0); rec.Code != 200 {
		t.Fatal(rec.Code)
	}
	if s.cur.rtx || s.cur.spec.Decision.MaxHeight != 720 || rr.count() != 1 {
		t.Fatal("base revisit did not regenerate ordinary source-sized output")
	}
}
