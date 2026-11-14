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

// This is the playback contract, not a CPU benchmark: on-demand startup, exact
// shared startup slots, and a bounded overlap that preserves the old output
// until the requested replacement segment has actually been published.
func TestABRRungHandoff(t *testing.T) {
	for _, protocol := range []string{ContainerMPEGTS, ContainerFMP4, core.ProtocolDASH} {
		t.Run(protocol, func(t *testing.T) {
			fr := newFakeRunner()
			fr.segDelay = 200 * time.Millisecond
			m := newTestManager(t, fr, config.Transcode{ABRLadder: true, FastStart: true, FastStartSegments: 2, FastStartSegment: config.Duration(2 * time.Second)})
			req := ladderEncodeRequest(t, "handoff-"+protocol, 2*time.Minute)
			if protocol == core.ProtocolDASH {
				req.Protocol = core.ProtocolDASH
			} else {
				req.Decision.Container = protocol
			}
			s := startSession(t, m, req)
			if fr.startCount() != 0 {
				t.Fatal("eager base encoder started before rendition selection")
			}
			s.mu.Lock()
			if !s.isDASH() {
				s.wantTrack = 1
				first := s.runPlanLocked(0)
				steady := s.runPlanLocked(2)
				if first.hlsDur != 2*time.Second || first.planEnd != 2 || steady.hlsDur != 4*time.Second {
					t.Fatalf("rung clock first=%+v steady=%+v", first, steady)
				}
				s.wantTrack = 0
			}
			s.mu.Unlock()
			fetch := func(track, n int) *httptest.ResponseRecorder {
				w := httptest.NewRecorder()
				r := httptest.NewRequest(http.MethodGet, "/segment", nil)
				if s.isDASH() {
					s.ServeMedia(w, r, track, n)
				} else {
					rungs := s.Rungs()
					s.ServeRungSegment(w, r, rungs[track].ID, n)
				}
				return w
			}
			if got := fetch(0, 0); got.Code != 200 {
				t.Fatalf("initial segment %d", got.Code)
			}
			old := fr.lastProc()
			before := fr.startCount()
			target := 1
			if s.isDASH() {
				target = 2
			}
			started := time.Now()
			done := make(chan *httptest.ResponseRecorder, 1)
			go func() { done <- fetch(target, 1) }()
			deadline := time.Now().Add(time.Second)
			for fr.startCount() == before && time.Now().Before(deadline) {
				time.Sleep(time.Millisecond)
			}
			if fr.startCount() != before+1 {
				t.Fatalf("switch start count %d -> %d", before, fr.startCount())
			}
			if old.wasKilled() {
				t.Fatal("previous rendition killed before replacement produced requested segment")
			}
			w := <-done
			elapsed := time.Since(started)
			t.Logf("requested segment 1 ready after %s", elapsed)
			if w.Code != 200 || elapsed > 2*time.Second {
				t.Fatalf("handoff status=%d time=%s", w.Code, elapsed)
			}
			fr.mu.Lock()
			args := append([]string(nil), fr.starts[before]...)
			fr.mu.Unlock()
			if s.isDASH() {
				if !strings.Contains(strings.Join(args, " "), "-ss ") {
					t.Fatal("DASH switch did not seek to requested slot")
				}
			} else {
				mustContain(t, args, "-start_number 1", "-ss 2.000")
			}
			deadline = time.Now().Add(time.Second)
			for !old.wasKilled() && time.Now().Before(deadline) {
				time.Sleep(time.Millisecond)
			}
			if !old.wasKilled() {
				t.Fatal("previous encoder retained after replacement publication")
			}
		})
	}
}
