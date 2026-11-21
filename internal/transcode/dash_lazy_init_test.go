package transcode

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
)

// DASH clients fetch the chosen representation's init before requesting
// media. An idle rung must start on that request, or neither side proceeds.
func TestDASHLazyRepresentationStartsOnInit(t *testing.T) {
	fr := newFakeRunner()
	m := newTestManager(t, fr, config.Transcode{ABRLadder: true}, WithSegmentTimeout(500*time.Millisecond), withRungDwell(10*time.Second))
	// Encode primary. This manager config leaves abrLadderUnderCopy false, so a copy primary would not advertise soft rungs.
	s := startDASHSession(t, m, ladderEncodeDASHRequest(t, "init-first", 10*time.Minute))
	if fr.startCount() != 0 {
		t.Fatal("DASH metadata started an unrequested encoder")
	}
	rec := httptest.NewRecorder()
	s.ServeInit(rec, httptest.NewRequest(http.MethodGet, "/2/header", nil), 2)
	if rec.Code != http.StatusOK || rec.Body.Len() == 0 {
		t.Fatalf("lazy init: HTTP %d %s", rec.Code, rec.Body.String())
	}
	if fr.startCount() != 1 {
		t.Fatalf("init did not start selected rung: %d starts", fr.startCount())
	}
	rec = httptest.NewRecorder()
	s.ServeInit(rec, httptest.NewRequest(http.MethodGet, "/1/header", nil), 1)
	if rec.Code != http.StatusOK || rec.Body.Len() == 0 || fr.startCount() != 1 {
		t.Fatalf("audio init switched the video rung: HTTP %d, starts %d", rec.Code, fr.startCount())
	}
}

func TestDASHManifestMultiRungDoesNotStartEncoder(t *testing.T) {
	fr := newFakeRunner()
	m := newTestManager(t, fr, config.Transcode{ABRLadder: true, ABRLadderUnderCopy: true}, WithManifestWait(2*time.Second))
	s := startDASHSession(t, m, dashRequest(t, "multi-rung-mpd", 10*time.Minute))
	if len(s.rungs) < 2 {
		t.Fatalf("expected multi-rung ladder, got %d rungs", len(s.rungs))
	}
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.ServeManifest(rec, httptest.NewRequest(http.MethodGet, "/start.mpd", nil))
	}()
	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("ServeManifest blocked on multi-rung DASH session")
	}
	if rec.Code != http.StatusOK || rec.Body.Len() == 0 {
		t.Fatalf("ServeManifest HTTP %d: %s", rec.Code, rec.Body.String())
	}
	if fr.startCount() != 0 {
		t.Fatalf("ServeManifest started %d encoders, want 0", fr.startCount())
	}
	s.mu.Lock()
	cached := s.dash.mpd != nil
	s.mu.Unlock()
	if cached {
		t.Fatal("ServeManifest cached estimated MPD before exact codecs arrived")
	}
}

func TestDASHResumeHintStartsAtResumeSegment(t *testing.T) {
	fr := newFakeRunner()
	m := newTestManager(t, fr, config.Transcode{})
	req := dashRequest(t, "dash-resume", 10*time.Minute)
	req.FirstSegmentAt = 2 * time.Minute
	s := startDASHSession(t, m, req)
	wantSeg := int(req.FirstSegmentAt / s.segDur)
	if wantSeg <= 0 || s.lastReq != wantSeg {
		t.Fatalf("lastReq = %d, want %d", s.lastReq, wantSeg)
	}
}
