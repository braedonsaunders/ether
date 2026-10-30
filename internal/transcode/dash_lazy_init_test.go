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
	rec := httptest.NewRecorder()
	s.ServeInit(rec, httptest.NewRequest(http.MethodGet, "/2/header", nil), 2)
	if rec.Code != http.StatusOK || rec.Body.Len() == 0 {
		t.Fatalf("lazy init: HTTP %d %s", rec.Code, rec.Body.String())
	}
	if fr.startCount() != 2 {
		t.Fatalf("init did not start selected rung: %d starts", fr.startCount())
	}
	rec = httptest.NewRecorder()
	s.ServeInit(rec, httptest.NewRequest(http.MethodGet, "/1/header", nil), 1)
	if rec.Code != http.StatusOK || rec.Body.Len() == 0 || fr.startCount() != 2 {
		t.Fatalf("audio init switched the video rung: HTTP %d, starts %d", rec.Code, fr.startCount())
	}
}
