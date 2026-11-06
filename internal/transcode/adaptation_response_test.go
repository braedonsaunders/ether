package transcode

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

// pacedDeliveryWriter models delivery time without sleeping or opening a
// socket. Its clock is explicit; this is a link/player MODEL, not a browser.
type pacedDeliveryWriter struct {
	*httptest.ResponseRecorder
	now    *time.Time
	kbps   int
	pause  time.Duration
	fail   bool
	cancel context.CancelFunc
}

func (w *pacedDeliveryWriter) Write(b []byte) (int, error) {
	*w.now = w.now.Add(time.Duration(float64(len(b)*8) / float64(w.kbps*1000) * float64(time.Second)))
	if w.pause > 0 {
		*w.now = w.now.Add(w.pause)
		w.pause = 0
	}
	if w.cancel != nil {
		w.cancel()
		w.cancel = nil
	}
	if w.fail {
		return len(b) / 2, errors.New("link interrupted")
	}
	return w.ResponseRecorder.Write(b)
}

func adaptationModel(t *testing.T, proto string) (*Session, *time.Time) {
	t.Helper()
	now := time.Unix(1000, 0)
	s := &Session{
		proto:  proto,
		segDur: 2 * time.Second,
		m:      &Manager{opts: options{rungDwell: 8 * time.Second}},
		sessionInput: sessionInput{
			info: ladderInfo(1080, 8000),
		},
		sessionEncoding: sessionEncoding{
			cur: &run{track: 0, started: now, produced: 100},
		},
		sessionAdaptation: sessionAdaptation{
			rungs: []rung{
				{decision: core.Decision{BitrateKbps: 8000, VideoCodec: "h264"}},
				{decision: core.Decision{BitrateKbps: 800, VideoCodec: "h264"}},
			},
		},
	}
	s.goodput.clock = func() time.Time { return now }
	return s, &now
}
func modelDelivery(s *Session, now *time.Time, track, n, kbps, rate int, pause time.Duration, fail bool) time.Duration {
	w := &pacedDeliveryWriter{ResponseRecorder: httptest.NewRecorder(), now: now, kbps: kbps, pause: pause, fail: fail}
	req := httptest.NewRequest(http.MethodGet, "/segment.ts", nil)
	req.RemoteAddr = "198.51.100.7:4321"
	start := *now
	s.serveCounted(w, req, "segment.ts", time.Time{}, bytes.NewReader(make([]byte, rate*1000*2/8)), track, n)
	return now.Sub(start)
}

func TestAdaptiveImpairedLinkModel(t *testing.T) {
	for _, proto := range []string{core.ProtocolHLS, core.ProtocolDASH} {
		t.Run(proto, func(t *testing.T) {
			s, now := adaptationModel(t, proto)
			lowTrack := 1
			if proto == core.ProtocolDASH {
				lowTrack = 2
			}
			// Two actually delivered segments establish progress; encoder-produced
			// segments deliberately stay far ahead and never count as buffer.
			modelDelivery(s, now, 0, 0, 8000, 4000, 0, false)
			modelDelivery(s, now, 0, 1, 8000, 4000, 0, false)
			buffer := 8 * time.Second // explicit modeled player state, never server telemetry
			stalls := time.Duration(0)
			consume := func(d time.Duration, complete bool) {
				if d > buffer {
					stalls += d - buffer
					buffer = 0
				} else {
					buffer -= d
				}
				if complete {
					buffer += 2 * time.Second
				}
			}
			dropStart := *now
			consume(modelDelivery(s, now, 0, 2, 1000, 4000, 0, false), true)
			if s.GoodputKbps() != 1000 {
				t.Fatalf("drop estimate=%d", s.GoodputKbps())
			}
			s.wantTrack = lowTrack
			if left := s.rungDwellLocked(true, 3); left != 0 {
				t.Fatalf("downshift delayed %v", left)
			}
			decisionLatency := now.Sub(dropStart)
			if decisionLatency != 8*time.Second {
				t.Fatalf("latency=%v", decisionLatency)
			}
			// Once selected, the sole encoder moves to the aligned lower slot.
			s.cur = &run{track: lowTrack, started: *now, produced: 2}
			s.deliveredCount = 0
			for i, link := range []int{900, 1200, 1000, 850, 1100} {
				consume(modelDelivery(s, now, lowTrack, 3+i, link, 800, 0, false), true)
			}
			// A failed transfer consumes wall time and buffer but contributes no
			// sample or complete media duration. Retry includes a brief interruption.
			before := s.goodput.Samples()
			consume(modelDelivery(s, now, lowTrack, 8, 1000, 800, 250*time.Millisecond, true), false)
			if s.goodput.Samples() != before {
				t.Fatal("failed transfer changed estimate")
			}
			consume(modelDelivery(s, now, lowTrack, 8, 1000, 800, 500*time.Millisecond, false), true)
			low := s.GoodputKbps()
			for n := 9; n < 11; n++ {
				consume(modelDelivery(s, now, lowTrack, n, 8000, 800, 0, false), true)
			}
			if s.GoodputKbps() != low {
				t.Fatal("two recovery samples raised ceiling")
			}
			consume(modelDelivery(s, now, lowTrack, 11, 8000, 800, 0, false), true)
			if s.GoodputKbps() <= low || s.GoodputKbps() >= 8000 {
				t.Fatalf("unsafe recovery=%d", s.GoodputKbps())
			}
			if stalls != 0 {
				t.Fatalf("modeled stalls=%v", stalls)
			}
			t.Logf("MODEL: 8->1Mbps decision=%v (one completed 4Mbps/2s segment); server dwell=0; jitter+brief interruption stalls=%v final buffer=%v recovery=%dkbps", decisionLatency, stalls, buffer, s.GoodputKbps())
			// An outage longer than available media necessarily stalls; account for
			// it explicitly instead of claiming an adaptive encoder prevents it.
			consume(buffer+time.Second, false)
			if stalls != time.Second {
				t.Fatalf("outage stall accounting=%v", stalls)
			}
		})
	}
}

func TestDeliveryEvidenceRejectsProbesFailuresAndLocalSocketTiming(t *testing.T) {
	for _, kind := range []string{"head", "init", "range", "conditional", "failed", "canceled", "loopback", "fast"} {
		t.Run(kind, func(t *testing.T) {
			s, now := adaptationModel(t, core.ProtocolHLS)
			req := httptest.NewRequest(http.MethodGet, "/segment.ts", nil)
			req.RemoteAddr = "198.51.100.7:4321"
			n := 0
			w := &pacedDeliveryWriter{ResponseRecorder: httptest.NewRecorder(), now: now, kbps: 1000}
			switch kind {
			case "head":
				req.Method = http.MethodHead
			case "init":
				n = InitSegment
			case "range":
				req.Header.Set("Range", "bytes=0-65535")
			case "conditional":
				req.Header.Set("If-Modified-Since", time.Unix(50, 0).UTC().Format(http.TimeFormat))
			case "failed":
				w.fail = true
			case "canceled":
				ctx, cancel := context.WithCancel(req.Context())
				defer cancel()
				req = req.WithContext(ctx)
				w.cancel = cancel
			case "loopback":
				req.RemoteAddr = "127.0.0.1:1234"
			case "fast":
				w.kbps = 1_000_000
			}
			s.serveCounted(w, req, "segment.ts", time.Unix(10, 0), bytes.NewReader(make([]byte, 128<<10)), 0, n)
			wantDelivery := kind == "range" || kind == "loopback" || kind == "fast"
			if s.PlaybackDelivered() != wantDelivery {
				t.Fatalf("%s playback delivery = %v, want %v", kind, s.PlaybackDelivered(), wantDelivery)
			}
			if s.GoodputKbps() != 0 || s.goodput.Samples() != 0 {
				t.Fatalf("%s fed estimator", kind)
			}
			if kind != "loopback" && kind != "fast" && s.deliveredCount != 0 {
				t.Fatalf("%s manufactured media progress", kind)
			}
		})
	}
}

func TestAdaptiveDwellClassifiesMediaAndRecovery(t *testing.T) {
	for _, proto := range []string{core.ProtocolHLS, core.ProtocolDASH} {
		t.Run(proto, func(t *testing.T) {
			s, now := adaptationModel(t, proto)
			low := 1
			if proto == core.ProtocolDASH {
				low = 2
			}
			s.wantTrack = low
			s.deliveredTrack, s.deliveredSeg, s.deliveredCount = 0, 1, 2
			for _, q := range []struct {
				media bool
				n     int
				rapid bool
			}{
				{false, 2, true}, {true, 0, false}, {true, 1, false}, {true, 20, false}, {true, 2, true},
			} {
				if got := s.rungDwellLocked(q.media, q.n) == 0; got != q.rapid {
					t.Fatalf("request %+v rapid=%v", q, got)
				}
			}
			s.deliveredCount = 1
			if s.rungDwellLocked(true, 2) != 0 {
				t.Fatal("next media after startup delivery was held")
			}
			s.goodput.add(125000, time.Second)
			if s.rungDwellLocked(true, 2) != 0 {
				t.Fatal("unsafe delivery did not expedite lower media")
			}
			s.cur.track = low
			s.wantTrack = 0
			s.deliveredTrack = low
			s.deliveredCount = 5
			if s.rungDwellLocked(true, 2) != 0 {
				t.Fatal("sequential recovery media was held")
			}
			s.cur.track = 0 // init switched, but no media delivered on new track
			s.wantTrack = low
			if s.rungDwellLocked(false, 2) <= 0 {
				t.Fatal("repeated init probe reused old-track delivery evidence")
			}
			*now = now.Add(9 * time.Second)
			if s.rungDwellLocked(true, 2) != 0 {
				t.Fatal("recovery held beyond dwell")
			}
		})
	}
}

func TestGoodputExpiresAndReadFromCountsAcceptedBytes(t *testing.T) {
	s, now := adaptationModel(t, core.ProtocolHLS)
	s.goodput.add(1000000, time.Second)
	*now = now.Add(goodputMaxAge + time.Second)
	if s.GoodputKbps() != 0 {
		t.Fatal("stale delivery evidence survived")
	}
	w := &pacedDeliveryWriter{ResponseRecorder: httptest.NewRecorder(), now: now, kbps: 1000, fail: true}
	cw := &countingWriter{ResponseWriter: w}
	n, err := cw.ReadFrom(io.LimitReader(bytes.NewReader(make([]byte, 65536)), 65536))
	if err == nil || cw.err == nil || cw.bytes != n || n >= 65536 {
		t.Fatalf("n=%d counted=%d err=%v", n, cw.bytes, err)
	}
}

func TestTimelineSeparatesAdjacentClampRates(t *testing.T) {
	p := buildTimeline(time.Minute, 2*time.Second, timelineOptions{steadyMode: ModeCopy})
	p.SetClamp(2, 800)
	p.SetClamp(3, 600)
	if p.NextChange(2) != 3 || p.Start(3) != 6*time.Second || p.Dur(3) != 2*time.Second {
		t.Fatal("clamp rate boundary or aligned timeline lost")
	}
}

func TestCanceledRenditionDemandCannotSteerEncoder(t *testing.T) {
	for _, init := range []bool{false, true} {
		t.Run(fmt.Sprint("init=", init), func(t *testing.T) {
			s, now := adaptationModel(t, core.ProtocolHLS)
			s.container = ContainerFMP4
			s.done = []map[int]bool{{}, {}}
			s.changed = make(chan struct{})
			s.m.opts.segmentTimeout = time.Second
			s.total = 30
			s.endSeg = -1
			entered := make(chan struct{})
			var once sync.Once
			s.goodput.clock = func() time.Time { once.Do(func() { close(entered) }); return *now }
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			req := httptest.NewRequest(http.MethodGet, "/", nil).WithContext(ctx)
			ended := make(chan struct{})
			go func() {
				defer close(ended)
				if init {
					s.serveInit(httptest.NewRecorder(), req, 1)
				} else {
					s.serveSegment(httptest.NewRecorder(), req, 1, 1)
				}
			}()
			select {
			case <-entered:
			case <-time.After(time.Second):
				t.Fatal("request did not reach dwell")
			}
			s.mu.Lock()
			want := s.wantTrack
			s.mu.Unlock()
			if want != 0 {
				t.Errorf("pending probe changed encoder demand to %d", want)
			}
			cancel()
			select {
			case <-ended:
			case <-time.After(time.Second):
				t.Fatal("canceled request leaked")
			}
			s.mu.Lock()
			want = s.wantTrack
			s.mu.Unlock()
			if want != 0 {
				t.Fatalf("canceled request left track%d selected", want)
			}
		})
	}
}

func TestFirstRenditionChoiceDoesNotWaitForEagerBase(t *testing.T) {
	for _, media := range []bool{false, true} {
		s, _ := adaptationModel(t, core.ProtocolHLS)
		s.wantTrack, s.runSeq = 1, 1
		if s.rungDwellLocked(media, 0) != 0 {
			t.Fatal("initial player choice waited for unused base")
		}
		s.runSeq = 2
		if s.rungDwellLocked(media, 0) <= 0 {
			t.Fatal("repeated startup probe bypassed protection")
		}
	}
}
